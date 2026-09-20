package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// migrateTestProvider wraps forkTestProvider and additionally supports failing
// CreateFromCheckpoint or Create on demand, so tests can exercise cleanup paths.
type migrateTestProvider struct {
	*forkTestProvider
	failCreateFromCheckpoint bool
	failCreate               bool
}

func newMigrateTestProvider() *migrateTestProvider {
	return &migrateTestProvider{forkTestProvider: newForkTestProvider()}
}

func (p *migrateTestProvider) CreateFromCheckpoint(ctx context.Context, spec Spec, srcVMID, checkpointID string) (Environment, error) {
	if p.failCreateFromCheckpoint {
		return nil, fmt.Errorf("injected CreateFromCheckpoint failure")
	}
	return p.forkTestProvider.CreateFromCheckpoint(ctx, spec, srcVMID, checkpointID)
}

func (p *migrateTestProvider) Create(ctx context.Context, spec Spec) (Environment, error) {
	if p.failCreate {
		return nil, fmt.Errorf("injected Create failure")
	}
	return p.forkTestProvider.Create(ctx, spec)
}

func TestMigrateVM_happyPath(t *testing.T) {
	provider := newMigrateTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	newID, err := fm.MigrateVM(context.Background(), srcID, "")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if newID == srcID {
		t.Fatalf("new vm id %q must differ from source %q", newID, srcID)
	}

	if _, ok := fm.GetVM(srcID); ok {
		t.Fatal("source vm was not destroyed after migration")
	}

	info, ok := fm.GetVM(newID)
	if !ok {
		t.Fatalf("GetVM(%s) not found", newID)
	}
	if info.State != VMStateRunning {
		t.Fatalf("new vm state = %s, want running", info.State)
	}

	if _, err := provider.Get(context.Background(), newID); err != nil {
		t.Fatalf("provider missing migrated env: %v", err)
	}
	if _, err := provider.Get(context.Background(), srcID); err == nil {
		t.Fatal("source env still exists in provider after migration")
	}

	forkSnaps, err := fm.ListSnapshots(context.Background(), newID)
	if err != nil {
		t.Fatalf("list migrate snapshots: %v", err)
	}
	if len(forkSnaps) != 1 {
		t.Fatalf("migrate lineage records = %d, want 1", len(forkSnaps))
	}
	if forkSnaps[0].ParentSnapshotID == "" {
		t.Fatal("lineage parent is empty; want a seed snapshot id")
	}
}

func TestMigrateVM_sameHost(t *testing.T) {
	provider := newMigrateTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	newID, err := fm.MigrateVM(context.Background(), srcID, "")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if newID == srcID {
		t.Fatalf("new vm id %q must differ from source", newID)
	}
	if _, ok := fm.GetVM(srcID); ok {
		t.Fatal("source vm was not destroyed")
	}
	info, ok := fm.GetVM(newID)
	if !ok || info.State != VMStateRunning {
		t.Fatalf("migrated vm not running: ok=%v state=%s", ok, info.State)
	}
}

func TestMigrateVM_rejectsGPU(t *testing.T) {
	provider := newMigrateTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	manifest := `{"version":"1","services":{}}`
	_, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{GPUs: 1}, []byte(manifest), nil, BootOptions{})
	if err == nil {
		t.Fatal("expected gpu provision to fail in single-provider mode; need a registered gpu host")
	}

	// verify MigrateVM on a gpu vm (GPUs > 0) is rejected up front.
	// since we can't provision a GPU vm in single-provider mode, we
	// manually inject one to test the guardrail.
	provider2 := newMigrateTestProvider()
	fm2 := NewFleetManager(FleetConfig{
		Provider: provider2,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm2, "task-1")
	fm2.mu.Lock()
	fm2.vms[srcID].spec.GPUs = 1
	fm2.mu.Unlock()

	_, err = fm2.MigrateVM(context.Background(), srcID, "")
	if !errors.Is(err, ErrGPUUnsupported) {
		t.Fatalf("err = %v, want ErrGPUUnsupported", err)
	}

	if got := len(fm2.ListFleet()); got != 1 {
		t.Fatalf("vm count = %d, want 1; a rejected migrate must not leave an environment behind", got)
	}
	info, ok := fm2.GetVM(srcID)
	if !ok || info.State != VMStateRunning {
		t.Fatalf("source vm state after rejected migrate = %v (ok=%v), want running", info.State, ok)
	}
}

func TestMigrateVM_rejectsNotRunning(t *testing.T) {
	provider := newMigrateTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	fm.mu.Lock()
	fm.vms[srcID].state = VMStateDraining
	fm.mu.Unlock()

	_, err := fm.MigrateVM(context.Background(), srcID, "")
	if err == nil {
		t.Fatal("expected migrate of non-running vm to fail")
	}
}

func TestMigrateVM_notFound(t *testing.T) {
	provider := newMigrateTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})

	_, err := fm.MigrateVM(context.Background(), "fuse-nonexistent", "")
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("err = %v, want ErrVMNotFound", err)
	}
}

func TestMigrateVM_providerNotForkable(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	_, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	srcID := "fuse-task-1"

	_, err = fm.MigrateVM(context.Background(), srcID, "")
	if err == nil {
		t.Fatal("expected migrate to fail when provider does not implement SnapshotForkable")
	}
}

func TestMigrateVM_cleanupOnCreateFromCheckpointFailure(t *testing.T) {
	provider := newMigrateTestProvider()
	provider.failCreateFromCheckpoint = true
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	_, err := fm.MigrateVM(context.Background(), srcID, "")
	if err == nil {
		t.Fatal("expected migrate to fail when CreateFromCheckpoint fails")
	}

	info, ok := fm.GetVM(srcID)
	if !ok || info.State != VMStateRunning {
		t.Fatalf("source vm state after failed migrate = %v (ok=%v), want running", info.State, ok)
	}

	if got := len(fm.ListFleet()); got != 1 {
		t.Fatalf("vm count = %d, want 1; a failed migrate must not leak an environment", got)
	}
}
