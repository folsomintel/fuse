package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"
)

// listFailProvider is a mockProvider whose List always fails, as a host
// agent that is down does.
type listFailProvider struct {
	*mockProvider
}

func (p *listFailProvider) List(context.Context, string) ([]Environment, error) {
	return nil, errors.New("connection refused")
}

// TestReconcileIsolatesUnreachableHost: one host whose list fails leaves its
// own vms alone for the cycle, while the reachable host is still reconciled.
func TestReconcileIsolatesUnreachableHost(t *testing.T) {
	up := newMockProvider()
	down := &listFailProvider{mockProvider: newMockProvider()}
	metrics := &captureMetrics{}
	fm := newInlineFleet(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", Metrics: metrics})
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, gpuFleetHost("up", 1, "a100"), up); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterHost(ctx, gpuFleetHost("down", 1, "a100"), down); err != nil {
		t.Fatal(err)
	}

	up.mu.Lock()
	up.envs["fuse-orphan"] = &mockEnv{name: "fuse-orphan"}
	up.mu.Unlock()
	now := time.Now()
	fm.mu.Lock()
	fm.vms["fuse-running"] = &vm{id: "fuse-running", hostID: "down", state: VMStateRunning, createdAt: now, updatedAt: now}
	fm.vms["fuse-destroying"] = &vm{id: "fuse-destroying", hostID: "down", state: VMStateDestroying, createdAt: now, updatedAt: now}
	fm.vms["fuse-gone"] = &vm{id: "fuse-gone", hostID: "up", state: VMStateRunning, createdAt: now, updatedAt: now}
	fm.mu.Unlock()

	fm.reconcile(ctx)

	if up.count() != 0 {
		t.Errorf("orphan on the reachable host not destroyed")
	}
	if v, ok := fm.GetVM("fuse-running"); !ok || v.State != VMStateRunning {
		t.Errorf("vm on the unreachable host = %+v, %v; want it left running", v, ok)
	}
	if _, ok := fm.GetVM("fuse-destroying"); !ok {
		t.Errorf("destroying vm on the unreachable host was reaped")
	}
	// missing from a host that answered: marked destroying, then reaped.
	if _, ok := fm.GetVM("fuse-gone"); ok {
		t.Errorf("vm missing from the reachable host was not reaped")
	}
	got := metrics.last()
	if got.HostsUnreachable != 1 || got.OrphansDestroyed != 1 || got.VMsMissingProvider != 1 {
		t.Errorf("summary = %+v, want 1 unreachable, 1 orphan destroyed, 1 missing", got)
	}
}

// TestReconcileSingleProviderListFailure: with no hosts, a failed list counts
// the provider unreachable and marks none of its vms missing.
func TestReconcileSingleProviderListFailure(t *testing.T) {
	p := &listFailProvider{mockProvider: newMockProvider()}
	metrics := &captureMetrics{}
	fm := newInlineFleet(FleetConfig{Provider: p, Prefix: "fuse-", Metrics: metrics})
	old := time.Now().Add(-time.Hour)
	fm.mu.Lock()
	fm.vms["fuse-a"] = &vm{id: "fuse-a", state: VMStateRunning, taskID: "t1", createdAt: old, updatedAt: old}
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.HostsUnreachable != 1 || got.VMsMissingProvider != 0 {
		t.Errorf("summary = %+v, want 1 unreachable and nothing missing", got)
	}
}

// TestReconcileRefreshesHostLastSeen: a host whose list answers has last_seen
// moved to that cycle, in memory and in the store, so it survives a restart. a
// host whose list fails keeps its old last_seen.
func TestReconcileRefreshesHostLastSeen(t *testing.T) {
	store := NewMemoryStateStore()
	fm := newInlineFleet(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", StateStore: store})
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, gpuFleetHost("up", 1, "a100"), newMockProvider()); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterHost(ctx, gpuFleetHost("down", 1, "a100"), &listFailProvider{mockProvider: newMockProvider()}); err != nil {
		t.Fatal(err)
	}
	inlineWait(fm)
	stale := time.Now().Add(-time.Hour).Truncate(time.Second)
	fm.mu.Lock()
	fm.hosts["up"].LastSeen = stale
	fm.hosts["down"].LastSeen = stale
	fm.mu.Unlock()

	before := time.Now()
	fm.reconcile(ctx)
	fm.bg.Wait()

	if h, _ := fm.GetHost("up"); h.LastSeen.Before(before) {
		t.Errorf("reachable host last_seen = %v, want refreshed to this cycle", h.LastSeen)
	}
	if h, _ := fm.GetHost("down"); !h.LastSeen.Equal(stale) {
		t.Errorf("unreachable host last_seen = %v, want it left at %v", h.LastSeen, stale)
	}
	rec, err := store.GetHost(ctx, "up")
	if err != nil {
		t.Fatal(err)
	}
	if rec.LastSeen.Before(before) {
		t.Errorf("stored last_seen = %v, want the refreshed value persisted", rec.LastSeen)
	}
}

// TestMarkHostSeenIgnoresReplacedProvider: a list answered by a provider the
// host was re-registered away from does not refresh the new registration.
func TestMarkHostSeenIgnoresReplacedProvider(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})
	ctx := context.Background()
	old := newMockProvider()
	if err := fm.RegisterHost(ctx, gpuFleetHost("h", 1, "a100"), old); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterHost(ctx, gpuFleetHost("h", 1, "a100"), newMockProvider()); err != nil {
		t.Fatal(err)
	}
	inlineWait(fm)
	stale := time.Now().Add(-time.Hour)
	fm.mu.Lock()
	fm.hosts["h"].LastSeen = stale
	fm.mu.Unlock()

	fm.markHostSeen("h", old, time.Now())

	if h, _ := fm.GetHost("h"); !h.LastSeen.Equal(stale) {
		t.Errorf("last_seen = %v, want it left at %v after an answer from the replaced provider", h.LastSeen, stale)
	}
}
