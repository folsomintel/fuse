package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// fakeDurableMigrator stands in for the restate client: it runs nothing and
// records what it was asked to do.
type fakeDurableMigrator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeDurableMigrator) Migrate(_ context.Context, vmID string, req MigrateRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s target=%s live=%v lazy=%v", vmID, req.TargetHostID, req.Live, req.Lazy))
	return "fuse-migrate-new", nil
}

// TestMigrateVM_durable: with restate configured a migrate runs on the
// source's Environment object, and an unknown vm is refused before restate
// is asked.
func TestMigrateVM_durable(t *testing.T) {
	fm := newDeltaFleet(t, newDeltaMigrateProvider(), &deltaMover{})
	d := &fakeDurableMigrator{}
	fm.SetDurableMigrator(d)
	ctx := context.Background()

	id, err := fm.MigrateVM(ctx, "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true})
	if err != nil || id != "fuse-migrate-new" {
		t.Fatalf("migrate = %q, %v", id, err)
	}
	if _, err := fm.MigrateVM(ctx, "fuse-missing", MigrateOptions{}); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("unknown vm err = %v, want ErrVMNotFound", err)
	}
	if got := fmt.Sprint(d.calls); got != "[fuse-task-src target=h-b live=true lazy=false]" {
		t.Fatalf("restate calls %s, want the one migrate", got)
	}
}

// fakeMigrateMetrics records the frozen time of each delta migrate.
type fakeMigrateMetrics struct {
	mu       sync.Mutex
	outcomes []string
}

func (m *fakeMigrateMetrics) ReconcileCompleted(ReconcileSummary) {}
func (m *fakeMigrateMetrics) MigrateFrozen(outcome string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d >= 0 {
		m.outcomes = append(m.outcomes, outcome)
	}
}

// TestMigrateVM_deltaRecordsFrozenTime: a delta migrate records how long its
// source was paused, as a cutover when the target took over and rolled back
// when the source had to be resumed.
func TestMigrateVM_deltaRecordsFrozenTime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failDelta bool
		want      string
	}{
		{"cutover", false, "[cutover]"},
		{"rolled back", true, "[rolled_back]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm := newDeltaFleet(t, newDeltaMigrateProvider(), &deltaMover{failDelta: tc.failDelta})
			m := &fakeMigrateMetrics{}
			fm.metrics = m
			_, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true})
			if (err != nil) != tc.failDelta {
				t.Fatalf("migrate err = %v", err)
			}
			if got := fmt.Sprint(m.outcomes); got != tc.want {
				t.Fatalf("frozen outcomes %s, want %s", got, tc.want)
			}
		})
	}
}

// TestMigratePlaceholder_persistsOnce: the placeholder step tracks and
// persists the new vm as provisioning on the target, and a retry neither
// tracks a second copy nor allocates it twice.
func TestMigratePlaceholder_persistsOnce(t *testing.T) {
	store := NewMemoryStateStore()
	provider := newDeltaMigrateProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", StateStore: store, CheckpointInterval: time.Hour})
	ctx := context.Background()
	for _, id := range []string{"h-a", "h-b"} {
		if err := fm.RegisterHost(ctx, artifactHost(id, 8), provider); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src", Spec{CPUs: 1, RamMB: 512, HostID: "h-a"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	plan, err := fm.MigrateBegin(ctx, "fuse-task-src", MigrateRequest{TargetHostID: "h-b"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := fm.MigratePlaceholder(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.ListVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rec VMRecord
	for _, r := range rows {
		if r.ID == plan.NewVMID {
			rec = r
		}
	}
	if rec.State != VMStateProvisioning || rec.HostID != "h-b" || rec.TaskID != plan.MigrationID {
		t.Fatalf("placeholder row %+v, want provisioning on h-b under the migrate's task", rec)
	}
	if h := findHost(t, fm, "h-b"); h.Allocated.VMCount != 1 {
		t.Fatalf("h-b vm count %d after a retried placeholder, want 1", h.Allocated.VMCount)
	}
}

// TestRecover_keepsADurableMigratePlaceholder: a migrate target that was not
// created before a restart stays provisioning when restate runs migrates, so
// the migrate resumes onto it; without restate it is torn down as before.
func TestRecover_keepsADurableMigratePlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		durable bool
		want    VMState
	}{
		{"durable", true, VMStateProvisioning},
		{"in process", false, VMStateDestroying},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStateStore()
			if err := store.UpsertVM(context.Background(), VMRecord{
				ID: "gpu-migrate-1", TaskID: "migrate-1", State: VMStateProvisioning, Spec: Spec{Name: "gpu-migrate-1"},
			}); err != nil {
				t.Fatal(err)
			}
			fm := recoverFleet(store, newStubProvider())
			if tc.durable {
				fm.SetDurableMigrator(&fakeDurableMigrator{})
			}
			if err := fm.recoverState(context.Background()); err != nil {
				t.Fatal(err)
			}
			fm.mu.RLock()
			v, ok := fm.vms["gpu-migrate-1"]
			fm.mu.RUnlock()
			if !ok || v.state != tc.want {
				t.Fatalf("recovered %+v, want state %s", v, tc.want)
			}
		})
	}
}

// TestSealIngress: the ingress publish a migrate journals is ciphertext under
// the token encryption key, and opens back to the same publish.
func TestSealIngress(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: newStubProvider(), TokenEncryptionKey: bytes.Repeat([]byte{3}, 32)})
	p := &ingressPlan{
		tunnelConfig: []byte(`{"token":"tunnel-secret"}`),
		grant:        IngressGrant{ProxyAddr: "proxy:4433", Routes: []IngressRoute{{Name: ingressAgentRoute, URL: "edge:1"}}},
		request:      IngressRequest{Owner: "fuse-new", Token: "tunnel-secret"},
	}
	sealed, err := fm.sealIngress(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("tunnel-secret")) {
		t.Fatal("sealed ingress holds the tunnel token in plaintext")
	}
	opened, err := fm.openIngress(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.request.Token != "tunnel-secret" || string(opened.tunnelConfig) != string(p.tunnelConfig) || opened.grant.ProxyAddr != "proxy:4433" {
		t.Fatalf("opened %+v, want the sealed publish back", opened)
	}
	if none, err := fm.sealIngress(nil); err != nil || none != nil {
		t.Fatalf("seal of no publish = %v, %v; want nothing", none, err)
	}
}

// TestMigrateVM_credentialsUploadedInPathOrder: the new vm gets credentials
// of its own, uploaded one at a time in path order (what keeps a durable
// retry's keyed uploads in step), and its agent restarts on the token its
// row stores.
func TestMigrateVM_credentialsUploadedInPathOrder(t *testing.T) {
	provider := newCredForkProvider()
	key := bytes.Repeat([]byte{5}, 32)
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", TokenEncryptionKey: key})
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-src", Spec{CPUs: 1, RamMB: 512}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	newID, err := fm.MigrateVM(ctx, "fuse-task-src", MigrateOptions{})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	env := provider.envs[newID]
	if env == nil {
		t.Fatalf("no env for %s", newID)
	}
	if len(env.uploadedPaths) == 0 || !slices.IsSorted(env.uploadedPaths) {
		t.Fatalf("uploaded %v, want the credential files in path order", env.uploadedPaths)
	}
	if len(env.startAgentCalls) != 1 || env.startAgentCalls[0].AuthToken == "" || env.startAgentCalls[0].AuthToken != env.authToken {
		t.Fatalf("start agent %+v with env token %q, want one restart on the vm's own token", env.startAgentCalls, env.authToken)
	}
	fm.mu.RLock()
	stored := fm.vms[newID].authTokenEncrypted
	fm.mu.RUnlock()
	if plain, err := secrets.DecryptToken(stored, key); err != nil || plain != env.authToken {
		t.Fatalf("stored token %q (%v), want the guest's", plain, err)
	}
}

// blockingMigrator holds every migrate until release is closed.
type blockingMigrator struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingMigrator) Migrate(ctx context.Context, _ string, _ MigrateRequest) (string, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return "fuse-migrate-new", nil
}

// TestCheckpointCandidates_skipADurableMigrate: while restate migrates a vm
// the checkpoint loop leaves it out, rather than queue a tick behind the
// migrate on its object, and takes it back once the migrate returns.
func TestCheckpointCandidates_skipADurableMigrate(t *testing.T) {
	fm := newDeltaFleet(t, newDeltaMigrateProvider(), &deltaMover{})
	b := &blockingMigrator{entered: make(chan struct{}, 1), release: make(chan struct{})}
	fm.SetDurableMigrator(b)
	if got := fmt.Sprint(fm.checkpointCandidates()); got != "[fuse-task-src]" {
		t.Fatalf("candidates %s before the migrate", got)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{Live: true})
		done <- err
	}()
	<-b.entered
	if got := fm.checkpointCandidates(); len(got) != 0 {
		t.Fatalf("candidates %v during the migrate, want none", got)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(fm.checkpointCandidates()); got != "[fuse-task-src]" {
		t.Fatalf("candidates %s after the migrate", got)
	}
}
