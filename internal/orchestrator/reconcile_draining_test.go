package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// hangingDestroyProvider wraps mockProvider so Destroy for one name blocks
// until its context ends or the test releases it, the way a host agent's
// DELETE queues behind a runaway exec holding the vm lock.
type hangingDestroyProvider struct {
	*mockProvider
	hangName string
	release  chan struct{}

	mu      sync.Mutex
	calls   int
	lastErr error
}

func newHangingDestroyProvider(t *testing.T, hangName string) *hangingDestroyProvider {
	p := &hangingDestroyProvider{
		mockProvider: newMockProvider(),
		hangName:     hangName,
		release:      make(chan struct{}),
	}
	t.Cleanup(func() { close(p.release) })
	return p
}

func (p *hangingDestroyProvider) Destroy(ctx context.Context, name string) error {
	if name != p.hangName {
		return p.mockProvider.Destroy(ctx, name)
	}
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-p.release:
		err = errors.New("released by test cleanup")
	}
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
	return err
}

func (p *hangingDestroyProvider) destroyCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// drainingFleet builds a fleet with one Draining VM whose transition into
// draining is back-dated by drainingFor.
func drainingFleet(t *testing.T, cfg FleetConfig, taskID string, drainingFor time.Duration) (*FleetManager, string) {
	t.Helper()
	// keep the stuck-task detector out of the way; these tests are only
	// about the draining bound.
	if cfg.TaskStuckTimeout == 0 {
		cfg.TaskStuckTimeout = 24 * time.Hour
	}
	fm, vmID := fleetWithRunningVM(t, cfg, taskID, drainingFor)
	fm.mu.Lock()
	fm.vms[vmID].state = VMStateDraining
	fm.vms[vmID].updatedAt = time.Now().Add(-drainingFor)
	fm.mu.Unlock()
	return fm, vmID
}

func TestDrainingTimeoutDefaults(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, drainTimeout, drainTimeout / 2} {
		fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), DrainingTimeout: d})
		if fm.drainingTimeout != DefaultDrainingTimeout {
			t.Errorf("DrainingTimeout %s: got %s, want the default %s", d, fm.drainingTimeout, DefaultDrainingTimeout)
		}
	}
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), DrainingTimeout: time.Hour})
	if fm.drainingTimeout != time.Hour {
		t.Errorf("explicit DrainingTimeout not kept: %s", fm.drainingTimeout)
	}
}

func TestStuckDraining_underTimeoutIsKept(t *testing.T) {
	metrics := &captureMetrics{}
	p := newMockProvider()
	fm, vmID := drainingFleet(t, FleetConfig{
		Provider:        p,
		DrainingTimeout: 10 * time.Minute,
		Metrics:         metrics,
	}, "task-1", 5*time.Minute)

	fm.reconcile(context.Background())

	if got := metrics.last().DrainingVMsDestroyed; got != 0 {
		t.Errorf("DrainingVMsDestroyed = %d, want 0", got)
	}
	fm.mu.RLock()
	state := fm.vms[vmID].state
	fm.mu.RUnlock()
	if state != VMStateDraining {
		t.Errorf("state = %s, want draining", state)
	}
	if p.count() != 1 {
		t.Errorf("provider envs = %d, want 1", p.count())
	}
}

func TestStuckDraining_pastTimeoutIsDestroyed(t *testing.T) {
	metrics := &captureMetrics{}
	store := NewMemoryStateStore()
	p := newMockProvider()
	fm, vmID := drainingFleet(t, FleetConfig{
		Provider:        p,
		StateStore:      store,
		DrainingTimeout: 10 * time.Minute,
		Metrics:         metrics,
	}, "task-1", 30*time.Minute)

	fm.reconcile(context.Background())

	if got := metrics.last().DrainingVMsDestroyed; got != 1 {
		t.Fatalf("DrainingVMsDestroyed = %d, want 1", got)
	}

	// the destroy runs in the background; wait for the vm to leave both the
	// provider and the fleet.
	waitForEgress(t, "draining vm destroyed", func() bool {
		fm.mu.RLock()
		_, tracked := fm.vms[vmID]
		fm.mu.RUnlock()
		return !tracked && p.count() == 0
	})

	waitForEgress(t, "vm.drain_expired event", func() bool {
		for _, e := range storedEvents(store, vmID) {
			if e.EventType == "vm.drain_expired" {
				return true
			}
		}
		return false
	})

	entries, err := store.ListDeadLetters(context.Background())
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Kind == DeadLetterDrainTimeout && e.EntityID == vmID {
			found = true
			if e.TaskID != "task-1" {
				t.Errorf("dead letter TaskID = %q, want task-1", e.TaskID)
			}
		}
	}
	if !found {
		t.Errorf("no %s dead letter for %s: %+v", DeadLetterDrainTimeout, vmID, entries)
	}

	// a second cycle has nothing left to do.
	fm.reconcile(context.Background())
	if got := metrics.last().DrainingVMsDestroyed; got != 0 {
		t.Errorf("second cycle DrainingVMsDestroyed = %d, want 0", got)
	}
}

func TestStuckDraining_runningVMIsNotTouched(t *testing.T) {
	metrics := &captureMetrics{}
	fm, vmID := fleetWithRunningVM(t, FleetConfig{
		TaskStuckTimeout: 24 * time.Hour,
		DrainingTimeout:  10 * time.Minute,
		Metrics:          metrics,
	}, "task-1", time.Hour)

	fm.reconcile(context.Background())

	if got := metrics.last().DrainingVMsDestroyed; got != 0 {
		t.Errorf("DrainingVMsDestroyed = %d, want 0 for a running vm", got)
	}
	fm.mu.RLock()
	state := fm.vms[vmID].state
	fm.mu.RUnlock()
	if state != VMStateRunning {
		t.Errorf("state = %s, want running", state)
	}
}

// a draining vm whose destroy hangs must not hold the reconcile cycle: the
// teardown runs in the background under destroyAndRemove's own timeout.
func TestStuckDraining_hangingDestroyDoesNotBlockCycle(t *testing.T) {
	metrics := &captureMetrics{}
	p := newHangingDestroyProvider(t, "fuse-task-1")
	fm, vmID := drainingFleet(t, FleetConfig{
		Provider:        p,
		DrainingTimeout: 10 * time.Minute,
		Metrics:         metrics,
	}, "task-1", 30*time.Minute)
	p.mu.Lock()
	p.envs[vmID] = &mockEnv{name: vmID}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		fm.reconcile(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile blocked on a hanging draining-vm destroy")
	}
	if got := metrics.last().DrainingVMsDestroyed; got != 1 {
		t.Errorf("DrainingVMsDestroyed = %d, want 1", got)
	}
	waitForEgress(t, "destroy attempted", func() bool { return p.destroyCalls() == 1 })
	fm.mu.RLock()
	state := fm.vms[vmID].state
	fm.mu.RUnlock()
	if state != VMStateDestroying {
		t.Errorf("state = %s, want destroying while the host call hangs", state)
	}
}
