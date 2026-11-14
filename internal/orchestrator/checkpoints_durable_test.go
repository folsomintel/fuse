package orchestrator

import (
	"context"
	"strings"
	"testing"
)

// TestChainSteps_fullThenDiff drives the durable chain's steps by hand the
// way the Environment handler does: an empty chain plans a full checkpoint
// to the standby, the recorded copy becomes the base, and the next tick plans
// a diff against the head.
func TestChainSteps_fullThenDiff(t *testing.T) {
	provider := newDeltaMigrateProvider()
	fm := newDeltaFleet(t, provider, &deltaMover{})
	ctx := context.Background()
	vmID := "fuse-task-src"

	plan, err := fm.ChainBegin(ctx, vmID, ChainState{})
	if err != nil || plan.Diff || plan.Gone || plan.Standby != "h-b" {
		t.Fatalf("first plan = %+v, %v; want a full checkpoint to h-b", plan, err)
	}
	full, err := fm.snapshotInProcess(ctx, vmID, plan.Options)
	if err != nil {
		t.Fatal(err)
	}
	mv, err := fm.ChainMovePlan(ctx, ChainState{}, plan, full)
	if err != nil || mv.Existing != "" || mv.Job.HostID != "h-b" || mv.Job.Base != "" {
		t.Fatalf("full move = %+v, %v; want a whole copy to h-b", mv, err)
	}
	moved, err := fm.ArtifactPull(ctx, mv.Job)
	if err != nil {
		t.Fatal(err)
	}
	base, err := fm.ArtifactRecord(ctx, mv.Job, moved)
	if err != nil {
		t.Fatal(err)
	}
	chain := ChainState{Standby: "h-b", Head: full, Base: base, Bytes: full.SizeBytes}
	fm.MirrorChain(vmID, chain, false)
	if got := fm.standbyCopies()[vmID]; got.host != "h-b" {
		t.Fatalf("mirrored standby = %+v, want h-b", got)
	}

	plan, err = fm.ChainBegin(ctx, vmID, chain)
	if err != nil || !plan.Diff || plan.Options.Parent != full.SnapshotID || plan.Standby != "h-b" {
		t.Fatalf("second plan = %+v, %v; want a diff against %s", plan, err, full.SnapshotID)
	}
	child, err := fm.snapshotInProcess(ctx, vmID, plan.Options)
	if err != nil {
		t.Fatal(err)
	}
	mv, err = fm.ChainMovePlan(ctx, chain, plan, child)
	if err != nil || mv.Job.Base != base || !mv.Job.DropBase {
		t.Fatalf("delta move = %+v, %v; want a merge onto %s", mv.Job, err, base)
	}

	// retiring the chain removes the base's record and the head.
	if err := fm.ChainRetire(ctx, vmID, chain); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.GetSnapshotByID(ctx, base); err == nil {
		t.Errorf("retired base %s still recorded", base)
	}
	if _, err := fm.GetSnapshotByID(ctx, full.SnapshotID); err == nil {
		t.Errorf("retired head %s still recorded", full.SnapshotID)
	}
}

// TestChainBegin_goneVM: a vm the fleet dropped plans its chain's removal.
func TestChainBegin_goneVM(t *testing.T) {
	fm := newDeltaFleet(t, newDeltaMigrateProvider(), &deltaMover{})
	plan, err := fm.ChainBegin(context.Background(), "fuse-nope", ChainState{Base: "b"})
	if err != nil || !plan.Gone {
		t.Fatalf("plan = %+v, %v; want gone", plan, err)
	}
}

// TestCheckpointTickDurable: each candidate's tick goes to its object, and a
// mirrored chain whose vm is gone is sent to be retired.
func TestCheckpointTickDurable(t *testing.T) {
	fm := newDeltaFleet(t, newDeltaMigrateProvider(), &deltaMover{})
	d := &fakeDurableSnapshots{}
	fm.MirrorChain("fuse-gone", ChainState{Standby: "h-b", Base: "b"}, false)

	fm.checkpointTickDurable(context.Background(), d)
	if got := strings.Join(d.checkpoints, ","); got != "fuse-task-src" {
		t.Fatalf("checkpoint calls %q, want fuse-task-src", got)
	}
	if got := strings.Join(d.retired, ","); got != "fuse-gone" {
		t.Fatalf("retire sends %q, want fuse-gone", got)
	}
	if _, ok := fm.standbyCopies()["fuse-gone"]; ok {
		t.Fatal("gone chain still mirrored")
	}
}

// TestMigrateVM_durableHoldsTheChain: a delta migrate on restate takes the
// chain from the vm's object, moves only the diff onto the staged base, and
// releases the hold with a reset since the merge consumed the base.
func TestMigrateVM_durableHoldsTheChain(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{}
	fm := newDeltaFleet(t, provider, mover)
	ctx := context.Background()

	// build a real chain in process, then hand it out through the hold.
	fm.advanceChain(ctx, "fuse-task-src")
	c := fm.chainFor("fuse-task-src")
	held := ChainState{Standby: c.standby, Head: c.head, Base: c.base, Bytes: c.bytes}
	d := &fakeDurableSnapshots{hold: ChainHold{Chain: held}}
	fm.SetDurableSnapshots(d)

	if _, err := fm.MigrateVM(ctx, "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	moves := mover.calls()
	if len(moves) != 2 || moves[1].Base != held.Base {
		t.Fatalf("moves = %+v, want the chain's full copy then one delta onto %s", moves, held.Base)
	}
	if len(d.holds) != 1 || len(d.releases) != 1 || !d.releases[0] {
		t.Fatalf("holds %v releases %v, want one hold released with a reset", d.holds, d.releases)
	}
}

// TestMigrateVM_durableBusyChainTakesAFullSeed: a chain whose copy is still
// in flight is not held, and the migrate seeds from a fresh full snapshot.
func TestMigrateVM_durableBusyChainTakesAFullSeed(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{}
	fm := newDeltaFleet(t, provider, mover)
	d := &fakeDurableSnapshots{hold: ChainHold{Busy: true}}
	fm.SetDurableSnapshots(d)

	if _, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	moves := mover.calls()
	if len(moves) != 1 || moves[0].Base != "" {
		t.Fatalf("moves = %+v, want one full seed copy", moves)
	}
	if len(d.releases) != 0 {
		t.Fatalf("released a hold that was never taken: %v", d.releases)
	}
}
