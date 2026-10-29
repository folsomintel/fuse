package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/secrets"
)

// ErrHitlSecretMissing is a hitl.notify webhook whose signing secret is not
// in the create request's secrets.
var ErrHitlSecretMissing = errors.New("hitl webhook secret missing")

// EnvKeyStore is the part of the api key store the fleet needs: scoped keys
// die with their environment and follow it across a migrate. it is an
// interface so the orchestrator does not import the key store.
type EnvKeyStore interface {
	RevokeEnv(ctx context.Context, envID string, now time.Time) error
	RebindEnv(ctx context.Context, fromID, toID string) error
}

// SetEnvKeyStore wires the api key store in. nil (the default) means there
// are no keys to revoke, as with an in-memory state store.
func (fm *FleetManager) SetEnvKeyStore(s EnvKeyStore) {
	fm.mu.Lock()
	fm.envKeys = s
	fm.mu.Unlock()
}

// SetPublicURL sets the base url webhook notifications link to.
func (fm *FleetManager) SetPublicURL(u string) {
	fm.mu.Lock()
	fm.publicURL = u
	fm.mu.Unlock()
}

// revokeEnvKeys revokes the scoped keys of a destroyed environment.
func (fm *FleetManager) revokeEnvKeys(ctx context.Context, envID string) {
	fm.mu.RLock()
	ks := fm.envKeys
	fm.mu.RUnlock()
	if ks == nil {
		return
	}
	if err := ks.RevokeEnv(ctx, envID, time.Now().UTC()); err != nil {
		fm.logger.Warn("revoke scoped api keys of destroyed env failed", "vm", envID, "err", err)
	}
}

// rebindEnvKeys moves scoped keys from a migrate source to its target. it
// runs before the source is destroyed, which would otherwise revoke them.
func (fm *FleetManager) rebindEnvKeys(ctx context.Context, fromID, toID string) {
	fm.mu.RLock()
	ks := fm.envKeys
	fm.mu.RUnlock()
	if ks == nil {
		return
	}
	if err := ks.RebindEnv(ctx, fromID, toID); err != nil {
		fm.logger.Warn("rebind scoped api keys across migrate failed", "from", fromID, "to", toID, "err", err)
	}
}

// EnvCovered reports whether a key scoped to keyEnv may reach envID: the
// environment itself or any environment forked from it, never a parent or
// a sibling. a destroyed environment is only covered by its own id.
func (fm *FleetManager) EnvCovered(keyEnv, envID string) bool {
	if keyEnv == envID {
		return true
	}
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[envID]
	return ok && slices.Contains(v.spec.Ancestors, keyEnv)
}

// EnvsCoveredBy returns keyEnv and every live environment forked from it.
func (fm *FleetManager) EnvsCoveredBy(keyEnv string) []string {
	out := []string{keyEnv}
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	for id, v := range fm.vms {
		if id != keyEnv && slices.Contains(v.spec.Ancestors, keyEnv) {
			out = append(out, id)
		}
	}
	slices.Sort(out[1:])
	return out
}

// hitlKeysFor picks the hitl.notify signing secrets out of a create
// request's secrets and seals them for the vm row. without an encryption
// key they are kept in memory only, so deliveries stop being sent after a
// restart rather than having their key stored in the clear.
func (fm *FleetManager) hitlKeysFor(spec *hitl.Spec, secretMap map[string]string) (map[string]string, []byte, error) {
	if spec == nil || len(spec.Notify) == 0 {
		return nil, nil, nil
	}
	keys := make(map[string]string, len(spec.Notify))
	for i, w := range spec.Notify {
		val, ok := secretMap[w.Secret]
		if !ok || val == "" {
			return nil, nil, fmt.Errorf("%w: hitl.notify[%d].secret names %q, which is not in secrets", ErrHitlSecretMissing, i, w.Secret)
		}
		keys[w.Secret] = val
	}
	if len(fm.tokenEncryptionKey) != 32 {
		fm.logger.Warn("hitl webhook secrets kept in memory only; set TOKEN_ENCRYPTION_KEY to keep them across a restart")
		return keys, nil, nil
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return nil, nil, err
	}
	ct, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		return nil, nil, fmt.Errorf("seal hitl webhook secrets: %w", err)
	}
	return keys, ct, nil
}

// openHitlKeys unseals a recovered vm's webhook secrets. a failure is
// logged and leaves the vm without them: its deliveries are skipped, which
// is better than failing recovery.
func (fm *FleetManager) openHitlKeys(vmID string, ct []byte) map[string]string {
	if len(ct) == 0 {
		return nil
	}
	if len(fm.tokenEncryptionKey) != 32 {
		fm.logger.Warn("hitl webhook secrets are sealed but no TOKEN_ENCRYPTION_KEY is configured", "vm", vmID)
		return nil
	}
	raw, err := secrets.DecryptBytes(ct, fm.tokenEncryptionKey)
	if err != nil {
		fm.logger.Warn("unseal hitl webhook secrets failed", "vm", vmID, "err", err)
		return nil
	}
	var keys map[string]string
	if err := json.Unmarshal(raw, &keys); err != nil {
		fm.logger.Warn("decode hitl webhook secrets failed", "vm", vmID, "err", err)
		return nil
	}
	return keys
}
