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
