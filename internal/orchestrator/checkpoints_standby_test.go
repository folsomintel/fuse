package orchestrator

import (
	"context"
	"testing"
)

// newStandbyFleet is newDeltaFleet with a third host, so a standby that stops
// fitting has somewhere else to go.
func newStandbyFleet(t *testing.T, mover ArtifactMover) *FleetManager {
	t.Helper()
	provider := newDeltaMigrateProvider()
	fm := newDeltaFleet(t, provider, mover)
	if err := fm.RegisterHost(context.Background(), artifactHost("h-c", 8), provider); err != nil {
		t.Fatal(err)
	}
	return fm
}

func chainStandby(fm *FleetManager, vmID string) string {
	c := fm.chainFor(vmID)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.standby
}

func TestCheckpointChain_aCordonedStandbyIsReplaced(t *testing.T) {
	mover := &deltaMover{}
	fm := newStandbyFleet(t, mover)
	ctx := context.Background()

	fm.advanceChain(ctx, "fuse-task-src")
	if got := chainStandby(fm, "fuse-task-src"); got != "h-b" {
		t.Fatalf("first standby = %q, want h-b", got)
	}
	if err := fm.CordonHost(ctx, "h-b"); err != nil {
		t.Fatal(err)
	}

	fm.advanceChain(ctx, "fuse-task-src")
	if got := chainStandby(fm, "fuse-task-src"); got != "h-c" {
		t.Fatalf("standby after cordon = %q, want h-c", got)
	}
	moves := mover.calls()
	if len(moves) != 2 || moves[1].Base != "" || moves[1].To.HostID != "h-c" {
		t.Fatalf("moves = %+v, want a fresh full copy to h-c rather than a delta onto the cordoned host", moves)
	}
	if _, err := fm.GetSnapshotByID(ctx, moves[0].SnapshotID); err == nil {
		t.Errorf("the old chain's base %s on the cordoned host is still recorded", moves[0].SnapshotID)
	}
	if pinnedArtifacts(fm) != 1 {
		t.Errorf("%d artifacts pinned, want only the new base", pinnedArtifacts(fm))
	}
}

func TestCheckpointChain_aFullStandbyIsReplaced(t *testing.T) {
	mover := &deltaMover{}
	fm := newStandbyFleet(t, mover)
	ctx := context.Background()

	fm.advanceChain(ctx, "fuse-task-src")
	if got := chainStandby(fm, "fuse-task-src"); got != "h-b" {
		t.Fatalf("first standby = %q, want h-b", got)
	}
	// other work fills h-b's disk.
	if _, err := fm.ProvisionAndAssign(ctx, "task-big",
		Spec{CPUs: 1, RamMB: 512, StorageGB: 100, HostID: "h-b"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision filler: %v", err)
	}
	fm.mu.Lock()
	fm.vms["fuse-task-src"].spec.StorageGB = 1
	fm.mu.Unlock()

	fm.advanceChain(ctx, "fuse-task-src")
	if got := chainStandby(fm, "fuse-task-src"); got != "h-c" {
		t.Fatalf("standby after h-b filled = %q, want h-c", got)
	}
	moves := mover.calls()
	if len(moves) != 2 || moves[1].Base != "" || moves[1].To.HostID != "h-c" {
		t.Fatalf("moves = %+v, want a fresh full copy to h-c", moves)
	}
	if _, err := fm.GetSnapshotByID(ctx, moves[0].SnapshotID); err == nil {
		t.Errorf("the old chain's base %s on the full host is still recorded", moves[0].SnapshotID)
	}
}

func TestPickStandby_countsOtherStandbyBytesAgainstStorage(t *testing.T) {
	fm := newStandbyFleet(t, &deltaMover{})

	// another vm's standby copy takes 96 of h-b's 100 GB.
	fm.chainsMu.Lock()
	fm.chains["fuse-other"] = &checkpointChain{standby: "h-b", bytes: 96 << 30}
	fm.chainsMu.Unlock()
	fm.mu.Lock()
	fm.vms["fuse-task-src"].spec.StorageGB = 10
	fm.mu.Unlock()

	if got := fm.pickStandby("fuse-task-src", "h-a"); got != "h-c" {
		t.Errorf("standby = %q, want h-c: h-b has 4 GB left once the other copy is counted", got)
	}
}

func TestPickStandby_prefersTheHostHoldingFewerCopies(t *testing.T) {
	fm := newStandbyFleet(t, &deltaMover{})

	fm.chainsMu.Lock()
	fm.chains["fuse-other"] = &checkpointChain{standby: "h-b", bytes: 1 << 30}
	fm.chainsMu.Unlock()
	if got := fm.pickStandby("fuse-task-src", "h-a"); got != "h-c" {
		t.Errorf("standby = %q, want h-c: h-b already holds a copy", got)
	}

	// the vm's own copy is not held against the host it is on: counted, it
	// would tie h-b and h-c and the tie would go to h-b.
	fm.chainsMu.Lock()
	fm.chains["fuse-task-src"] = &checkpointChain{standby: "h-c", bytes: 1 << 30}
	fm.chainsMu.Unlock()
	if got := fm.pickStandby("fuse-task-src", "h-a"); got != "h-c" {
		t.Errorf("standby = %q, want h-c still", got)
	}
}

func TestPickStandby_noneWhenNoOtherHostFits(t *testing.T) {
	fm := newStandbyFleet(t, &deltaMover{})
	fm.mu.Lock()
	fm.vms["fuse-task-src"].spec.StorageGB = 101
	fm.mu.Unlock()

	if got := fm.pickStandby("fuse-task-src", "h-a"); got != "" {
		t.Errorf("standby = %q, want none: no host has 101 GB", got)
	}
}
