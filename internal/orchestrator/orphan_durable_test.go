package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeOrphans stands in for restate: each DestroyOrphan blocks until
// release is closed, then drives the fleet's steps the way the handler does.
type fakeOrphans struct {
	fm      *FleetManager
	release chan struct{}
	dead    bool

	mu    sync.Mutex
	calls int
}

func (f *fakeOrphans) DestroyOrphan(ctx context.Context, vmID string, req OrphanDestroyRequest) (OrphanDestroyResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	<-f.release
	if f.dead {
		return OrphanDestroyResult{DeadLettered: true}, f.fm.OrphanFailed(ctx, vmID, req.MaxRetries, "host down", true)
	}
	if err := f.fm.OrphanDestroyOnHost(ctx, vmID, req.HostID, req.Epoch); err != nil {
		return OrphanDestroyResult{}, err
	}
	return OrphanDestroyResult{}, f.fm.OrphanDestroyed(ctx, vmID)
}

func (f *fakeOrphans) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestReconcileSendsOrphanOnceWhileInFlight: a cycle that finds an orphan
// already being destroyed does not send it again, and the outcome is counted
// in the next cycle's summary.
func TestReconcileSendsOrphanOnceWhileInFlight(t *testing.T) {
	p := newMockProvider()
	metrics := &captureMetrics{}
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-", Metrics: metrics})
	d := &fakeOrphans{fm: fm, release: make(chan struct{})}
	fm.SetDurableOrphans(d)
	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan"}
	p.mu.Unlock()

	ctx := context.Background()
	fm.reconcile(ctx)
	waitFor(t, 5*time.Second, func() bool { return d.count() == 1 }, "orphan sent")
	fm.reconcile(ctx)
	close(d.release)
	fm.destroySends.Wait()
	if d.count() != 1 {
		t.Fatalf("DestroyOrphan calls = %d, want 1", d.count())
	}
	if p.count() != 0 {
		t.Fatal("orphan not destroyed")
	}

	fm.reconcile(ctx)
	if got := metrics.last().OrphansDestroyed; got != 1 {
		t.Fatalf("OrphansDestroyed = %d, want 1", got)
	}
}

// TestReconcileStopsSendingDeadLetteredOrphan: once the handler reports the
// orphan dead-lettered, reconcile skips it.
func TestReconcileStopsSendingDeadLetteredOrphan(t *testing.T) {
	p := newMockProvider()
	metrics := &captureMetrics{}
	store := NewMemoryStateStore()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-", Metrics: metrics, StateStore: store})
	release := make(chan struct{})
	close(release)
	d := &fakeOrphans{fm: fm, release: release, dead: true}
	fm.SetDurableOrphans(d)
	p.mu.Lock()
	p.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan"}
	p.mu.Unlock()

	ctx := context.Background()
	fm.reconcile(ctx)
	fm.destroySends.Wait()
	fm.reconcile(ctx)
	fm.destroySends.Wait()
	if d.count() != 1 {
		t.Fatalf("DestroyOrphan calls = %d, want 1", d.count())
	}
	if got := metrics.last().OrphansDeadLettered; got != 1 {
		t.Fatalf("OrphansDeadLettered = %d, want 1", got)
	}
	letters, err := store.ListDeadLetters(ctx)
	if err != nil || len(letters) != 1 || letters[0].Kind != DeadLetterOrphanDestroy {
		t.Fatalf("dead letters = %+v, %v", letters, err)
	}
}
