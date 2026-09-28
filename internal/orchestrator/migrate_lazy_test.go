package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// lazyMigrateProvider is a live migrate provider whose target reports how far
// its memory has arrived, the way a host agent running fc-uffd does.
type lazyMigrateProvider struct {
	*liveMigrateProvider
	statusMu sync.Mutex
	status   MemoryStatus
}

func (p *lazyMigrateProvider) MemoryStatus(context.Context, string) (MemoryStatus, error) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	return p.status, nil
}

func (p *lazyMigrateProvider) setStatus(st MemoryStatus) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	p.status = st
}

func newLazyMigrateFleet(t *testing.T, provider *lazyMigrateProvider, mover *recordingMover, hugePages bool) *FleetManager {
	t.Helper()
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", ArtifactMover: mover})
	for _, id := range []string{"h-a", "h-b"} {
		if err := fm.RegisterHost(ctx, artifactHost(id, 8), provider); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src",
		Spec{CPUs: 1, RamMB: 512, HostID: "h-a", HugePages: hugePages}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision source: %v", err)
	}
	return fm
}

func pinnedArtifacts(fm *FleetManager) int {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return len(fm.artifactPulls)
}

func waitForNoPins(t *testing.T, fm *FleetManager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pinnedArtifacts(fm) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the seed is still pinned after the memory transfer ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func withFastLazyPolling(t *testing.T) {
	t.Helper()
	prev := lazyMemoryPollInterval
	lazyMemoryPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { lazyMemoryPollInterval = prev })
}

func TestMigrateVM_lazyLeavesMemoryOnTheSourceUntilResident(t *testing.T) {
	withFastLazyPolling(t)
	provider := &lazyMigrateProvider{liveMigrateProvider: newLiveMigrateProvider()}
	mover := &recordingMover{}
	fm := newLazyMigrateFleet(t, provider, mover, true)

	newID, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true, Lazy: true})
	if err != nil {
		t.Fatalf("lazy migrate: %v", err)
	}

	moves := mover.calls()
	if len(moves) != 1 || !moves[0].Lazy {
		t.Fatalf("moves = %+v, want one lazy copy", moves)
	}
	info, _ := fm.GetVM(newID)
	if !info.Spec.ResumeSeed || !info.Spec.HugePages {
		t.Errorf("migrated vm resume %v huge pages %v, want a 2M resume", info.Spec.ResumeSeed, info.Spec.HugePages)
	}
	// the target holds no memory image, so it must not look like a holder of
	// the artifact that the next move could copy from.
	if _, err := fm.GetSnapshotByID(context.Background(), moves[0].SnapshotID); err == nil {
		t.Errorf("a replica record %s was written for a target that has no memory image", moves[0].SnapshotID)
	}
	if _, ok := fm.GetVM("fuse-task-src"); ok {
		t.Error("source vm was not destroyed after the migration")
	}

	// the seed is the only copy of the pages the target has not touched yet.
	time.Sleep(4 * lazyMemoryPollInterval)
	if pinnedArtifacts(fm) == 0 {
		t.Fatal("the seed was released before the target had all of its memory")
	}
	provider.setStatus(MemoryStatus{ResidentChunks: 256, TotalChunks: 256, Done: true})
	waitForNoPins(t, fm)
}

func TestMigrateVM_lazyReleasesTheSeedWhenTheTransferFails(t *testing.T) {
	withFastLazyPolling(t)
	provider := &lazyMigrateProvider{liveMigrateProvider: newLiveMigrateProvider()}
	fm := newLazyMigrateFleet(t, provider, &recordingMover{}, true)

	if _, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true, Lazy: true}); err != nil {
		t.Fatalf("lazy migrate: %v", err)
	}
	provider.setStatus(MemoryStatus{ResidentChunks: 10, TotalChunks: 256, Error: "chunk 10: digest mismatch"})
	waitForNoPins(t, fm)
}

func TestMigrateVM_lazyRefusalsLeaveTheSourceRunning(t *testing.T) {
	cases := []struct {
		name      string
		opts      MigrateOptions
		hugePages bool
	}{
		{name: "lazy without live", opts: MigrateOptions{TargetHostID: "h-b", Lazy: true}, hugePages: true},
		{name: "source not booted with huge pages", opts: MigrateOptions{TargetHostID: "h-b", Live: true, Lazy: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &lazyMigrateProvider{liveMigrateProvider: newLiveMigrateProvider()}
			mover := &recordingMover{}
			fm := newLazyMigrateFleet(t, provider, mover, tc.hugePages)

			_, err := fm.MigrateVM(context.Background(), "fuse-task-src", tc.opts)
			if !errors.Is(err, ErrLiveMigrateRefused) {
				t.Fatalf("err = %v, want %v", err, ErrLiveMigrateRefused)
			}
			if info, ok := fm.GetVM("fuse-task-src"); !ok || info.State != VMStateRunning {
				t.Errorf("source vm = %+v (tracked %v), want it still running", info.State, ok)
			}
			if len(mover.calls()) != 0 {
				t.Error("a refused lazy migrate still copied the seed")
			}
		})
	}
}
