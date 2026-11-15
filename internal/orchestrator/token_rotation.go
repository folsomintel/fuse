package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// the refusals a token rotation fails with when a retry cannot help.
// TokenRotationRefused reports them.
var (
	// ErrRotateNotRunning is a vm that is not running.
	ErrRotateNotRunning = errors.New("token rotation requires running")
	// errRotateNoKey is an orchestrator with no TOKEN_ENCRYPTION_KEY.
	errRotateNoKey = errors.New("token rotation requires a 32-byte encryption key (TOKEN_ENCRYPTION_KEY)")
	// errRotateSealed is rotated credentials that no longer decrypt, as
	// after TOKEN_ENCRYPTION_KEY changed under an unfinished rotation.
	errRotateSealed = errors.New("rotated credentials do not decrypt under TOKEN_ENCRYPTION_KEY")
)

// TokenRotationRefused reports whether a token rotation step failed with one
// of the rotation's own refusals (vm not running, no encryption key,
// credentials that do not decrypt). a retry cannot change any of them.
func TokenRotationRefused(err error) bool {
	return errors.Is(err, ErrRotateNotRunning) || errors.Is(err, errRotateNoKey) || errors.Is(err, errRotateSealed)
}

// RotatedCredentials is the output of RotateTokenGenerate and the input of
// the later steps. both fields are ciphertext under TOKEN_ENCRYPTION_KEY: the
// durable handler journals this value, so neither the plaintext token nor the
// tls key ever reaches restate.
type RotatedCredentials struct {
	// Sealed is the cert, key and token as json, encrypted. the upload step
	// decrypts it.
	Sealed []byte `json:"sealed"`
	// TokenEncrypted is the token as the vm row stores it.
	TokenEncrypted []byte `json:"token_encrypted"`
}

// sealedCredentials is the plaintext inside RotatedCredentials.Sealed.
type sealedCredentials struct {
	AuthToken string `json:"auth_token"`
	CertPEM   []byte `json:"cert_pem"`
	KeyPEM    []byte `json:"key_pem"`
}

// DurableTokenRotator runs a token rotation as the restate
// Environment/{vm_id}.RotateToken handler and waits for it. *durable.Client
// satisfies it.
type DurableTokenRotator interface {
	RotateToken(ctx context.Context, vmID string) error
}

// SetDurableTokenRotator routes token rotation through restate. nil (mode
// off, the default) keeps it in process. set it before Start.
func (fm *FleetManager) SetDurableTokenRotator(d DurableTokenRotator) {
	fm.mu.Lock()
	fm.durableTokens = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableTokenRotator() DurableTokenRotator {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableTokens
}

// RotateToken generates new TLS credentials and an auth token for a
// running VM, uploads them to the guest filesystem, and persists the
// new encrypted token. on restate it runs as the
// Environment/{vm_id}.RotateToken handler, so a crash between the upload and
// the persist resumes at the persist with the same token.
//
// KNOWN GAP: fused reads --auth-token-file exactly once at process start
// (fused/main.go) and has no credential poller, so a live fused keeps
// serving the OLD token until it is restarted. Rotation therefore updates the
// orchestrator's copy and the guest's files but does not yet take effect in the
// running guest; it needs a StartAgent call (which restarts fused) the way
// ForkEnvironment does.
//
// Rotation is a server-side operation: the orchestrator updates its
// own encrypted copy and the guest agent's credential files (paths owned
// by the agent profile). New inbound connections use the rotated
// credentials; existing connections retain the old cert until they reconnect.
func (fm *FleetManager) RotateToken(ctx context.Context, vmID string) error {
	ctx, end := fm.startOperation(ctx, "rotate_token", vmID)
	err := fm.rotateToken(ctx, vmID)
	end(err)
	return err
}

func (fm *FleetManager) rotateToken(ctx context.Context, vmID string) error {
	if d := fm.durableTokenRotator(); d != nil {
		ctx, cancel := durableWait(ctx)
		defer cancel()
		return d.RotateToken(ctx, vmID)
	}
	rotated, err := fm.RotateTokenGenerate(ctx, vmID)
	if err != nil {
		return err
	}
	if err := fm.RotateTokenUpload(ctx, vmID, rotated); err != nil {
		return err
	}
	if err := fm.storeRotatedToken(vmID, rotated); err != nil {
		return err
	}
	// mode off keeps the background persist it always had.
	fm.persistVMBackground(vmID)
	fm.appendEvent(ctx, "vm", vmID, "vm.token_rotated", nil)
	fm.logger.InfoContext(ctx, "vm token rotated", "vm", vmID)
	return nil
}

// the methods below are the steps of a token rotation, in order. the
// durable handler runs each as a journaled step; mode off runs them in turn.

// rotateTarget returns vmID's environment handle when the vm can take a
// rotation.
func (fm *FleetManager) rotateTarget(vmID string) (Environment, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	env := v.env
	state := v.state
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return nil, fmt.Errorf("vm %s in state %s: %w", vmID, state, ErrRotateNotRunning)
	}
	if env == nil {
		return nil, fmt.Errorf("%w: vm %s", errNoEnvironment, vmID)
	}
	if len(fm.tokenEncryptionKey) != 32 {
		return nil, errRotateNoKey
	}
	return env, nil
}

// RotateTokenGenerate checks the vm can take a rotation and generates its
// new credentials, returned encrypted. on restate its result is journaled, so
// a replay reuses the same token instead of generating another.
func (fm *FleetManager) RotateTokenGenerate(_ context.Context, vmID string) (RotatedCredentials, error) {
	if _, err := fm.rotateTarget(vmID); err != nil {
		return RotatedCredentials{}, err
	}
	creds, err := secrets.GenerateVMCredentials(vmID)
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("generate credentials: %w", err)
	}
	raw, err := json.Marshal(sealedCredentials{AuthToken: creds.AuthToken, CertPEM: creds.CertPEM, KeyPEM: creds.KeyPEM})
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("encode credentials: %w", err)
	}
	sealed, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("encrypt credentials: %w", err)
	}
	token, err := secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
	if err != nil {
		return RotatedCredentials{}, fmt.Errorf("encrypt token: %w", err)
	}
	return RotatedCredentials{Sealed: sealed, TokenEncrypted: token}, nil
}

// RotateTokenUpload writes the credential files (cert, key, auth token) into
// the guest, at the paths the agent profile owns (fusedCredentialFiles). the
// same files are written in the same order on every attempt, so a retry is
// harmless.
func (fm *FleetManager) RotateTokenUpload(ctx context.Context, vmID string, rotated RotatedCredentials) error {
	env, err := fm.rotateTarget(vmID)
	if err != nil {
		return err
	}
	raw, err := secrets.DecryptBytes(rotated.Sealed, fm.tokenEncryptionKey)
	if err != nil {
		return errRotateSealed
	}
	var sealed sealedCredentials
	if err := json.Unmarshal(raw, &sealed); err != nil {
		return errRotateSealed
	}
	creds := &secrets.VMCredentials{AuthToken: sealed.AuthToken, CertPEM: sealed.CertPEM, KeyPEM: sealed.KeyPEM}
	// one at a time in path order, not concurrently like uploadFiles: on
	// restate each upload is keyed by its place in the step (key/1, key/2,
	// ...), and the agent replays a key's first answer whatever file it now
	// carries, so a retried step has to send the same file under each key.
	files := fusedCredentialFiles(creds)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := env.Upload(ctx, files[path], path); err != nil {
			return fmt.Errorf("upload credentials: upload %s: %w", path, err)
		}
	}
	setTokenIfSupported(env, creds)
	return nil
}

// RotateTokenPersist makes the new token the vm's own: in memory, on the
// environment handle (which a restart between the upload and this step left
// on the old one), and in the state store, written before it returns. the
// durable path's replacement for persistVMBackground.
func (fm *FleetManager) RotateTokenPersist(ctx context.Context, vmID string, rotated RotatedCredentials) error {
	if err := fm.storeRotatedToken(vmID, rotated); err != nil {
		return err
	}
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		return fmt.Errorf("persist rotated token: %w", err)
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.token_rotated", nil)
	fm.logger.InfoContext(ctx, "vm token rotated", "vm", vmID)
	return nil
}

// storeRotatedToken records the rotated token on vmID in memory and on its
// environment handle.
func (fm *FleetManager) storeRotatedToken(vmID string, rotated RotatedCredentials) error {
	token, err := secrets.DecryptToken(rotated.TokenEncrypted, fm.tokenEncryptionKey)
	if err != nil {
		return errRotateSealed
	}
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	v.authTokenEncrypted = rotated.TokenEncrypted
	v.updatedAt = time.Now()
	env := v.env
	fm.mu.Unlock()
	if ts, ok := env.(TokenSetter); ok {
		ts.SetToken(token)
	}
	return nil
}
