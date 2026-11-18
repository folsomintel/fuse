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
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", Metrics: metrics})
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

// TestReconcileSingleProviderListFailureRunsOtherPasses: with no hosts, a
// failed list skips only the passes that read it.
func TestReconcileSingleProviderListFailureRunsOtherPasses(t *testing.T) {
	p := &listFailProvider{mockProvider: newMockProvider()}
	metrics := &captureMetrics{}
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-", Metrics: metrics, TaskStuckTimeout: time.Minute})
	old := time.Now().Add(-time.Hour)
	fm.mu.Lock()
	fm.vms["fuse-a"] = &vm{id: "fuse-a", state: VMStateRunning, taskID: "t1", createdAt: old, updatedAt: old}
	fm.mu.Unlock()

	fm.reconcile(context.Background())

	got := metrics.last()
	if got.HostsUnreachable != 1 || got.VMsMissingProvider != 0 {
		t.Errorf("summary = %+v, want 1 unreachable and nothing missing", got)
	}
	// the stuck-task detector does not read the list, so it still strikes.
	if got.StuckTasksSuspected != 1 {
		t.Errorf("StuckTasksSuspected = %d, want 1", got.StuckTasksSuspected)
	}
}
