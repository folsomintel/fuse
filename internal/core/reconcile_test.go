package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// failingDestroyProvider wraps mockProvider so Destroy always fails for a
// chosen VM name, producing an orphan that the reconcile loop cannot clean
// up. Used to exercise the DLQ path for orphan destroy retries.
type failingDestroyProvider struct {
	*mockProvider
	failName  string
	destroyMu sync.Mutex
	calls     int
}

func newFailingDestroyProvider(failName string) *failingDestroyProvider {
	return &failingDestroyProvider{
		mockProvider: newMockProvider(),
		failName:     failName,
	}
}

func (p *failingDestroyProvider) Destroy(ctx context.Context, name string) error {
	p.destroyMu.Lock()
	defer p.destroyMu.Unlock()
	if name == p.failName {
		p.calls++
		return fmt.Errorf("injected destroy failure for %s", name)
	}
	return p.mockProvider.Destroy(ctx, name)
}

func (p *failingDestroyProvider) destroyCalls() int {
	p.destroyMu.Lock()
	defer p.destroyMu.Unlock()
	return p.calls
}

// captureMetrics records every ReconcileSummary for assertions.
type captureMetrics struct {
	mu        sync.Mutex
	summaries []ReconcileSummary
}

func (c *captureMetrics) ReconcileCompleted(s ReconcileSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.summaries = append(c.summaries, s)
}

func (c *captureMetrics) last() ReconcileSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.summaries) == 0 {
		return ReconcileSummary{}
	}
	return c.summaries[len(c.summaries)-1]
}

func (c *captureMetrics) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.summaries)
}

func TestReconcileOrphan_successClearsRetries(t *testing.T) {
	p := newMockProvider()
	metrics := &captureMetrics{}
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
		Metrics:  metrics,
	})

	// Inject an orphan — not tracked in the fleet.
	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan", url: "http://orphan.test"}
	p.mu.Unlock()

	fm.reconcile(context.Background())

	if p.count() != 0 {
		t.Errorf("orphan not destroyed: %d envs remain", p.count())
	}
	if metrics.count() != 1 {
		t.Fatalf("expected 1 metric emission, got %d", metrics.count())
	}
	got := metrics.last()
	if got.OrphansDestroyed != 1 {
		t.Errorf("OrphansDestroyed = %d, want 1", got.OrphansDestroyed)
	}
	if got.OrphansDeadLettered != 0 {
		t.Errorf("OrphansDeadLettered = %d, want 0", got.OrphansDeadLettered)
	}

	fm.mu.RLock()
	if retries := fm.orphanRetries["fuse-orphan"]; retries != 0 {
		t.Errorf("retry counter not cleared: %d", retries)
	}
	fm.mu.RUnlock()
}

func TestReconcileOrphan_deadLettersAfterMaxRetries(t *testing.T) {
	p := newFailingDestroyProvider("fuse-orphan")
	store := NewMemoryStateStore()
	metrics := &captureMetrics{}

	fm := NewFleetManager(FleetConfig{
		Provider:                p,
		StateStore:              store,
		Prefix:                  "fuse-",
		OrphanDestroyMaxRetries: 3,
		Metrics:                 metrics,
	})

	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan", url: "http://orphan.test"}
	p.mu.Unlock()

	// Run reconcile repeatedly. Each cycle should fail once and bump the
	// retry counter. After 3 failing cycles the entry is dead-lettered and
	// destroy is no longer attempted.
	for i := 0; i < 5; i++ {
		fm.reconcile(context.Background())
	}

	if got := p.destroyCalls(); got != 3 {
		t.Errorf("expected 3 destroy attempts, got %d (should stop after dead-letter)", got)
	}

	entries, err := store.ListDeadLetters(context.Background())
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 dead letter, got %d: %+v", len(entries), entries)
	}
	if entries[0].Kind != DeadLetterOrphanDestroy {
		t.Errorf("Kind = %q, want %q", entries[0].Kind, DeadLetterOrphanDestroy)
	}
	if entries[0].EntityID != "fuse-orphan" {
		t.Errorf("EntityID = %q", entries[0].EntityID)
	}
	if entries[0].RetryCount < 3 {
		t.Errorf("RetryCount = %d, want >= 3", entries[0].RetryCount)
	}

	// Orphans-dead-lettered should be reported on the cycle that crossed
	// the threshold (the 3rd reconcile call), not on subsequent no-op cycles.
	var deadLetteredEvents int
	for _, s := range metrics.summaries {
		deadLetteredEvents += s.OrphansDeadLettered
	}
	if deadLetteredEvents != 1 {
		t.Errorf("expected DLQ report on exactly one cycle, got %d", deadLetteredEvents)
	}
}

func TestReconcileOrphan_retriesClearedWhenOrphanDisappears(t *testing.T) {
	p := newFailingDestroyProvider("fuse-orphan")
	fm := NewFleetManager(FleetConfig{
		Provider:                p,
		Prefix:                  "fuse-",
		OrphanDestroyMaxRetries: 5,
	})

	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan", url: "http://orphan.test"}
	p.mu.Unlock()

	fm.reconcile(context.Background())
	fm.reconcile(context.Background())

	fm.mu.RLock()
	beforeRetries := fm.orphanRetries["fuse-orphan"]
	fm.mu.RUnlock()
	if beforeRetries != 2 {
		t.Fatalf("expected 2 retries recorded, got %d", beforeRetries)
	}

	// Orphan disappears from the provider (some other actor cleaned it up).
	p.mu.Lock()
	delete(p.envs, "fuse-orphan")
	p.mu.Unlock()

	fm.reconcile(context.Background())

	fm.mu.RLock()
	_, stillTracked := fm.orphanRetries["fuse-orphan"]
	fm.mu.RUnlock()
	if stillTracked {
		t.Error("retry counter should be cleared when orphan disappears")
	}
}

// fleetWithRunningVM constructs a fleet manager with a single Running VM
// whose createdAt is back-dated to simulate age without waiting.
func fleetWithRunningVM(t *testing.T, cfg FleetConfig, taskID string, age time.Duration) (*FleetManager, string) {
	t.Helper()
	if cfg.Provider == nil {
		cfg.Provider = newMockProvider()
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "fuse-"
	}
	fm := NewFleetManager(cfg)

	vmID := "fuse-" + taskID
	now := time.Now()
	createdAt := now.Add(-age)
	fm.mu.Lock()
	fm.vms[vmID] = &vm{
		id:        vmID,
		state:     VMStateRunning,
		taskID:    taskID,
		env:       &mockEnv{name: vmID, url: "http://" + vmID + ".test"},
		spec:      Spec{Name: vmID},
		createdAt: createdAt,
		updatedAt: createdAt,
	}
	// Also make the provider aware of the VM so reconcile doesn't mark it
	// "missing from provider" and short-circuit the stuck check.
	if p, ok := cfg.Provider.(*mockProvider); ok {
		p.mu.Lock()
		p.envs[vmID] = &mockEnv{name: vmID, url: "http://" + vmID + ".test"}
		p.mu.Unlock()
	}
	fm.mu.Unlock()
	return fm, vmID
}

func TestStuckTask_underCeilingIsIgnored(t *testing.T) {
	metrics := &captureMetrics{}
	fm, _ := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 1 * time.Hour,
		Metrics:          metrics,
	}, "task-1", 10*time.Minute)

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.StuckTasksSuspected != 0 || got.StuckTasksFailed != 0 {
		t.Errorf("healthy VM flagged: %+v", got)
	}
}

func TestStuckTask_firstCycleIsSuspectedOnly(t *testing.T) {
	store := NewMemoryStateStore()
	metrics := &captureMetrics{}
	fm, vmID := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 5 * time.Minute,
		StateStore:       store,
		Metrics:          metrics,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.StuckTasksSuspected != 1 {
		t.Errorf("StuckTasksSuspected = %d, want 1", got.StuckTasksSuspected)
	}
	if got.StuckTasksFailed != 0 {
		t.Errorf("StuckTasksFailed = %d, want 0 on first cycle", got.StuckTasksFailed)
	}

	// VM should still be Running — strike count is 1, not failed yet.
	info, ok := fm.GetVM(vmID)
	if !ok {
		t.Fatal("vm removed prematurely")
	}
	if info.State != VMStateRunning {
		t.Errorf("VM state = %q, want running", info.State)
	}

	fm.mu.RLock()
	strikes := fm.stuckStrikes[vmID]
	fm.mu.RUnlock()
	if strikes != 1 {
		t.Errorf("strike count = %d, want 1", strikes)
	}
}

func TestStuckTask_secondCycleFailsAndDeadLetters(t *testing.T) {
	store := NewMemoryStateStore()
	metrics := &captureMetrics{}
	fm, vmID := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 5 * time.Minute,
		StateStore:       store,
		Metrics:          metrics,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background()) // strike 1
	fm.reconcile(context.Background()) // strike 2 → fail

	var failed int
	for _, s := range metrics.summaries {
		failed += s.StuckTasksFailed
	}
	if failed != 1 {
		t.Errorf("StuckTasksFailed total = %d, want 1", failed)
	}

	// The fail path persists the failed task and dead-letter entry via
	// background goroutines. Poll briefly for both to land.
	var foundFailed bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !foundFailed {
		tasks, err := store.ListTasks(context.Background())
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		for _, task := range tasks {
			if task.TaskID == "task-1" && task.RunStatus == TaskRunFailed {
				foundFailed = true
				if task.LastError == "" {
					t.Error("failed task missing LastError")
				}
			}
		}
		if !foundFailed {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !foundFailed {
		t.Error("task-1 not marked failed in state store within deadline")
	}

	entries, err := store.ListDeadLetters(context.Background())
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	var foundDLQ bool
	for _, e := range entries {
		if e.Kind == DeadLetterStuckTask && e.EntityID == vmID {
			foundDLQ = true
			if e.TaskID != "task-1" {
				t.Errorf("DLQ TaskID = %q", e.TaskID)
			}
		}
	}
	if !foundDLQ {
		t.Errorf("stuck task not dead-lettered: %+v", entries)
	}
}

func TestStuckTask_specMaxRuntimeOverridesDefault(t *testing.T) {
	metrics := &captureMetrics{}
	fm, _ := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 10 * time.Minute, // default would say "stuck"
		Metrics:          metrics,
	}, "task-1", 30*time.Minute)

	// Override: task is allowed to run 1h.
	fm.mu.Lock()
	fm.vms["fuse-task-1"].spec.MaxRuntime = 1 * time.Hour
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.StuckTasksSuspected != 0 || got.StuckTasksFailed != 0 {
		t.Errorf("per-spec override not honored: %+v", got)
	}
}

func TestStuckTask_strikeClearsOnStateChange(t *testing.T) {
	fm, vmID := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 5 * time.Minute,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background())

	fm.mu.RLock()
	if fm.stuckStrikes[vmID] != 1 {
		t.Fatal("strike 1 not recorded")
	}
	fm.mu.RUnlock()

	// Simulate progress — move to destroying so the VM is no longer
	// eligible for stuck detection.
	fm.mu.Lock()
	fm.vms[vmID].state = VMStateDestroying
	fm.vms[vmID].taskID = ""
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	fm.mu.RLock()
	_, present := fm.stuckStrikes[vmID]
	fm.mu.RUnlock()
	if present {
		t.Error("strike counter should be cleared when VM leaves Running")
	}
}

func TestMemoryDeadLetter_upsertAdvancesLastSeen(t *testing.T) {
	s := NewMemoryStateStore()
	ctx := context.Background()

	first := time.Now().Add(-1 * time.Hour)
	if err := s.UpsertDeadLetter(ctx, DeadLetterRecord{
		Kind:        DeadLetterOrphanDestroy,
		EntityID:    "fuse-x",
		RetryCount:  1,
		FirstSeenAt: first,
		LastSeenAt:  first,
	}); err != nil {
		t.Fatal(err)
	}

	second := time.Now()
	if err := s.UpsertDeadLetter(ctx, DeadLetterRecord{
		Kind:       DeadLetterOrphanDestroy,
		EntityID:   "fuse-x",
		RetryCount: 3,
		LastSeenAt: second,
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListDeadLetters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if !e.FirstSeenAt.Equal(first) {
		t.Errorf("FirstSeenAt changed: %v vs %v", e.FirstSeenAt, first)
	}
	if !e.LastSeenAt.Equal(second) {
		t.Errorf("LastSeenAt not advanced: %v", e.LastSeenAt)
	}
	if e.RetryCount != 3 {
		t.Errorf("RetryCount = %d, want 3", e.RetryCount)
	}
}

func TestReconcile_metricsAlwaysEmitted(t *testing.T) {
	metrics := &captureMetrics{}
	fm := NewFleetManager(FleetConfig{
		Provider: newMockProvider(),
		Prefix:   "fuse-",
		Metrics:  metrics,
	})

	for i := 0; i < 3; i++ {
		fm.reconcile(context.Background())
	}

	if metrics.count() != 3 {
		t.Errorf("expected 3 emissions, got %d", metrics.count())
	}
	for i, s := range metrics.summaries {
		if s.Duration <= 0 {
			t.Errorf("summary %d has zero duration", i)
		}
	}
}
