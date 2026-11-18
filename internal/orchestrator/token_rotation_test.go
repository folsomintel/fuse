package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/secrets"
)

// fakeTokenRotator stands in for the restate client and records the vms it
// was asked to rotate.
type fakeTokenRotator struct{ rotated []string }

func (f *fakeTokenRotator) RotateToken(_ context.Context, vmID string) error {
	f.rotated = append(f.rotated, vmID)
	return nil
}

func newRotateTestFleet(t *testing.T) (*FleetManager, *credForkProvider, *MemoryStateStore, string) {
	t.Helper()
	provider := newCredForkProvider()
	store := NewMemoryStateStore()
	fm := newInlineFleet(FleetConfig{
		Provider:           provider,
		StateStore:         store,
		Prefix:             "fuse-",
		TokenEncryptionKey: testEncryptionKey(),
	})
	return fm, provider, store, provisionSnapshotTestVM(t, fm, "task-1")
}

// storedToken decrypts the token the state store holds for vmID.
func storedToken(t *testing.T, store *MemoryStateStore, vmID string) string {
	t.Helper()
	records, err := store.ListVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.ID == vmID {
			plain, err := secrets.DecryptToken(r.AuthTokenEncrypted, testEncryptionKey())
			if err != nil {
				t.Fatalf("decrypt stored token: %v", err)
			}
			return plain
		}
	}
	t.Fatalf("no stored record for %s", vmID)
	return ""
}

// TestRotateToken_rotatesGuestToken: a rotation writes a new token into the
// guest and makes it the vm's own, in memory and on the environment handle.
func TestRotateToken_rotatesGuestToken(t *testing.T) {
	fm, provider, _, vmID := newRotateTestFleet(t)
	env := provider.envs[vmID]
	before := string(env.files[fuseAuthTokenPath])

	if err := fm.RotateToken(context.Background(), vmID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	inGuest := string(env.files[fuseAuthTokenPath])
	if inGuest == "" || inGuest == before {
		t.Fatalf("guest token %q, want a new one (was %q)", inGuest, before)
	}
	if env.Token() != inGuest {
		t.Fatal("environment handle does not carry the rotated token")
	}
	fm.mu.RLock()
	enc := fm.vms[vmID].authTokenEncrypted
	fm.mu.RUnlock()
	if plain, err := secrets.DecryptToken(enc, testEncryptionKey()); err != nil || plain != inGuest {
		t.Fatalf("vm token %q (%v), want the guest's", plain, err)
	}
}

// TestRotateToken_durable: the rotation runs on the Environment object, and
// the api call itself never touches the guest.
func TestRotateToken_durable(t *testing.T) {
	fm, provider, _, vmID := newRotateTestFleet(t)
	d := &fakeTokenRotator{}
	fm.SetDurableTokenRotator(d)
	before := string(provider.envs[vmID].files[fuseAuthTokenPath])

	if err := fm.RotateToken(context.Background(), vmID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(d.rotated) != 1 || d.rotated[0] != vmID {
		t.Fatalf("durable rotations %v, want [%s]", d.rotated, vmID)
	}
	if got := string(provider.envs[vmID].files[fuseAuthTokenPath]); got != before {
		t.Fatal("durable rotation touched the guest in process")
	}
}

// TestRotateTokenSteps_restartBeforePersist runs the durable steps the way a
// replay after a crash between the upload and the persist does: the vm comes
// back with its old token, and the persist alone makes the guest's token the
// stored one. the generate step's output, which restate journals, never
// carries the plaintext token or tls key.
func TestRotateTokenSteps_restartBeforePersist(t *testing.T) {
	fm, provider, store, vmID := newRotateTestFleet(t)
	env := provider.envs[vmID]
	ctx := context.Background()
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		t.Fatal(err)
	}
	old := storedToken(t, store, vmID)

	rotated, err := fm.RotateTokenGenerate(ctx, vmID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := fm.RotateTokenUpload(ctx, vmID, rotated); err != nil {
		t.Fatalf("upload: %v", err)
	}
	inGuest := string(env.files[fuseAuthTokenPath])
	journaled, _ := json.Marshal(rotated)
	for _, secret := range []string{inGuest, string(env.files[fuseTLSKeyPath])} {
		if strings.Contains(string(journaled), secret) {
			t.Fatal("generate's output carries a plaintext credential")
		}
	}

	// the restart: the recovered handle and store still hold the old token.
	env.SetToken(old)
	if got := storedToken(t, store, vmID); got != old {
		t.Fatalf("store changed before the persist: %q", got)
	}

	if err := fm.RotateTokenPersist(ctx, vmID, rotated); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := storedToken(t, store, vmID); got != inGuest {
		t.Fatalf("stored token %q, want the guest's %q", got, inGuest)
	}
	if env.Token() != inGuest {
		t.Fatal("persist did not put the rotated token back on the environment handle")
	}
}

// TestRotateTokenSteps_refusals: what a retry cannot change is reported as a
// refusal, and a vm the fleet does not know keeps its sentinel.
func TestRotateTokenSteps_refusals(t *testing.T) {
	fm, _, _, vmID := newRotateTestFleet(t)
	ctx := context.Background()

	if _, err := fm.RotateTokenGenerate(ctx, "fuse-missing"); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("missing vm: %v, want ErrVMNotFound", err)
	}

	rotated, err := fm.RotateTokenGenerate(ctx, vmID)
	if err != nil {
		t.Fatal(err)
	}
	tampered := RotatedCredentials{Sealed: []byte("not ciphertext"), TokenEncrypted: rotated.TokenEncrypted}
	if err := fm.RotateTokenUpload(ctx, vmID, tampered); !TokenRotationRefused(err) {
		t.Fatalf("undecryptable credentials: %v, want a refusal", err)
	}

	fm.mu.Lock()
	fm.vms[vmID].state = VMStateDraining
	fm.mu.Unlock()
	_, err = fm.RotateTokenGenerate(ctx, vmID)
	if !TokenRotationRefused(err) || !strings.Contains(err.Error(), "token rotation requires running") {
		t.Fatalf("draining vm: %v, want the not-running refusal", err)
	}

	// startup refuses a fleet with no key; one that lost it anyway refuses
	// to rotate rather than store a plaintext token.
	noKey := newInlineFleet(FleetConfig{Provider: newCredForkProvider(), Prefix: "fuse-"})
	id := provisionSnapshotTestVM(t, noKey, "task-2")
	noKey.tokenEncryptionKey = nil
	if _, err := noKey.RotateTokenGenerate(ctx, id); !errors.Is(err, errRotateNoKey) {
		t.Fatalf("no key: %v, want errRotateNoKey", err)
	}
}

// TestRotateToken_metered: every rotation is counted as rotate_token, a
// refused one as an error.
func TestRotateToken_metered(t *testing.T) {
	metrics := &opMetrics{}
	fm := newInlineFleet(FleetConfig{
		Provider:           newCredForkProvider(),
		Prefix:             "fuse-",
		Metrics:            metrics,
		TokenEncryptionKey: testEncryptionKey(),
	})
	ctx := context.Background()
	vmID := provisionSnapshotTestVM(t, fm, "task-1")
	if err := fm.RotateToken(ctx, vmID); err != nil {
		t.Fatal(err)
	}
	if err := fm.RotateToken(ctx, "fuse-missing"); err == nil {
		t.Fatal("rotating an unknown vm succeeded")
	}
	want := []string{"create:ok", "rotate_token:ok", "rotate_token:error"}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if !slices.Equal(metrics.ops, want) {
		t.Fatalf("operations = %v, want %v", metrics.ops, want)
	}
}

// TestRotateTokenUpload_stableOrder: every attempt sends the credential files
// one at a time in the same order, so on restate a retried upload keys each
// file the way the first attempt did.
func TestRotateTokenUpload_stableOrder(t *testing.T) {
	fm, provider, _, vmID := newRotateTestFleet(t)
	env := provider.envs[vmID]
	ctx := context.Background()
	rotated, err := fm.RotateTokenGenerate(ctx, vmID)
	if err != nil {
		t.Fatal(err)
	}
	var attempts [][]string
	for range 3 {
		env.mu.Lock()
		env.uploadedPaths = nil
		env.mu.Unlock()
		if err := fm.RotateTokenUpload(ctx, vmID, rotated); err != nil {
			t.Fatal(err)
		}
		env.mu.Lock()
		attempts = append(attempts, append([]string(nil), env.uploadedPaths...))
		env.mu.Unlock()
	}
	for _, got := range attempts {
		if !slices.IsSorted(got) || len(got) != 3 || !slices.Equal(got, attempts[0]) {
			t.Fatalf("upload orders %v, want the same sorted three files each attempt", attempts)
		}
	}
}
