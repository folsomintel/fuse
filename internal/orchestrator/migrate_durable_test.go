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

// TestMigrateVM_durable: a migrate runs on the
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
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-", StateStore: store, CheckpointInterval: time.Hour})
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
// created before a restart stays provisioning, so the migrate resumes onto it.
func TestRecover_keepsADurableMigratePlaceholder(t *testing.T) {
	store := NewMemoryStateStore()
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID: "gpu-migrate-1", TaskID: "migrate-1", State: VMStateProvisioning, Spec: Spec{Name: "gpu-migrate-1"},
	}); err != nil {
		t.Fatal(err)
	}
	fm := recoverFleet(store, newStubProvider())
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
	fm.mu.RLock()
	v, ok := fm.vms["gpu-migrate-1"]
	fm.mu.RUnlock()
	if !ok || v.state != VMStateProvisioning {
		t.Fatalf("recovered %+v, want state %s", v, VMStateProvisioning)
	}
}

// TestStaleMigratePlaceholderIsAbandoned: a recovered migrate target is
// kept while its migrate may still run, and failed once it has been
// provisioning past staleMigrateAfter: nothing will finish it.
func TestStaleMigratePlaceholderIsAbandoned(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStateStore()
	now := time.Now()
	for id, created := range map[string]time.Time{
		"fuse-migrate-young": now.Add(-time.Hour),
		"fuse-migrate-old":   now.Add(-staleMigrateAfter - time.Hour),
	} {
		if err := store.UpsertVM(ctx, VMRecord{
			ID: id, TaskID: "migrate-" + id, State: VMStateProvisioning, Spec: Spec{Name: id},
			CreatedAt: created, UpdatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fm := recoverFleet(store, newStubProvider())
	if err := fm.recoverState(ctx); err != nil {
		t.Fatal(err)
	}
	fm.reconcileStaleCreates(ctx)

	if info, ok := fm.GetVM("fuse-migrate-young"); !ok || info.State != VMStateProvisioning {
		t.Fatalf("young placeholder %+v (tracked %v), want it kept provisioning", info.State, ok)
	}
	if _, ok := fm.GetVM("fuse-migrate-old"); ok {
		t.Fatal("a migrate target provisioning past staleMigrateAfter was kept")
	}
}

// TestSealIngress: the ingress publish a migrate journals is ciphertext under
// the token encryption key, and opens back to the same publish.
func TestSealIngress(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newStubProvider(), TokenEncryptionKey: bytes.Repeat([]byte{3}, 32)})
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
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-", TokenEncryptionKey: key})
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

// resumeEnv answers Resume with err.
type resumeEnv struct {
	stubLoadEnv
	err error
}

func (e *resumeEnv) CheckpointDiff(context.Context, string, string, bool) (Checkpoint, error) {
	return Checkpoint{}, nil
}
func (e *resumeEnv) Resume(context.Context) error { return e.err }

// TestMigrateResumeSource_retriesAllButGoneOrRunning: a rollback's resume
// takes a source the host no longer has (404) or will not resume because it
// is not paused (409) as done, and returns every other failure, a rejected
// token among them, so the step retries it.
func TestMigrateResumeSource_retriesAllButGoneOrRunning(t *testing.T) {
	for _, tc := range []struct {
		err  error
		done bool
	}{
		{nil, true},
		{&HTTPStatusError{Code: 404}, true},
		{&HTTPStatusError{Code: 409}, true},
		{&HTTPStatusError{Code: 401}, false},
		{&HTTPStatusError{Code: 503}, false},
		{errors.New("connection refused"), false},
	} {
		fm := newInlineFleet(FleetConfig{Provider: newStubProvider()})
		fm.vms["fuse-src"] = &vm{id: "fuse-src", env: &resumeEnv{err: tc.err}}
		err := fm.MigrateResumeSource(context.Background(), MigratePlan{SourceVMID: "fuse-src"})
		if (err == nil) != tc.done {
			t.Errorf("resume answering %v: err = %v, want done %v", tc.err, err, tc.done)
		}
	}
}

// drainRecorder is a DurableExpiry that records the drains sent to restate.
type drainRecorder struct {
	mu     sync.Mutex
	drains []string
}

func (d *drainRecorder) Drain(_ context.Context, vmID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.drains = append(d.drains, vmID)
	return nil
}
func (d *drainRecorder) SendArmExpiry(context.Context, string) error { return nil }

// TestMigrateDrainSource_staysInProcess: with restate running drains, a
// migrate still drains its source in process. the durable migrate holds the
// source's Environment object, so a drain sent there would wait on it
// forever.
func TestMigrateDrainSource_staysInProcess(t *testing.T) {
	fm := newDeltaFleet(t, newDeltaMigrateProvider(), &deltaMover{})
	d := &drainRecorder{}
	fm.SetDurableExpiry(d)
	if err := fm.MigrateDrainSource(context.Background(), MigratePlan{SourceVMID: "fuse-task-src"}); err != nil {
		t.Fatal(err)
	}
	if len(d.drains) != 0 {
		t.Fatalf("drains sent to restate %v, want none", d.drains)
	}
	info, _ := fm.GetVM("fuse-task-src")
	if info.State != VMStateDraining {
		t.Fatalf("source state %s, want draining in process", info.State)
	}
}
