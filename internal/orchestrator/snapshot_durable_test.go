package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDurableSnapshots stands in for the restate client: it runs nothing and
// records what it was asked to do.
type fakeDurableSnapshots struct {
	mu          sync.Mutex
	snapshots   []string
	restores    []string
	checkpoints []string
	retired     []string
	collects    []string
}

func (f *fakeDurableSnapshots) Snapshot(_ context.Context, vmID string, _ SnapshotOptions) (SnapshotRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots = append(f.snapshots, vmID)
	return SnapshotRecord{SnapshotID: "snap-durable", VMID: vmID, State: SnapshotStateReady}, nil
}

func (f *fakeDurableSnapshots) Restore(_ context.Context, vmID, snapshotID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, vmID+"/"+snapshotID)
	return nil
}

func (f *fakeDurableSnapshots) Checkpoint(_ context.Context, vmID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints = append(f.checkpoints, vmID)
	return nil
}

func (f *fakeDurableSnapshots) SendRetireChain(_ context.Context, vmID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retired = append(f.retired, vmID)
	return nil
}

func (f *fakeDurableSnapshots) SendCollectSnapshot(_ context.Context, vmID, snapshotID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collects = append(f.collects, vmID+"/"+snapshotID+"/"+key)
	return nil
}

// TestSnapshotAndRestore_durable: the api's snapshot and restore run on the
// Environment object, never on the host directly.
func TestSnapshotAndRestore_durable(t *testing.T) {
	ctx := context.Background()
	provider := newSnapshotTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-durable")
	d := &fakeDurableSnapshots{}
	fm.SetDurableSnapshots(d)

	rec, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{Comment: "x"})
	if err != nil || rec.SnapshotID != "snap-durable" {
		t.Fatalf("create = %+v, %v; want the durable record", rec, err)
	}
	if err := fm.RestoreSnapshot(ctx, vmID, "snap-durable"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := strings.Join(d.snapshots, ","); got != vmID {
		t.Fatalf("durable snapshots %q, want %q", got, vmID)
	}
	if got := strings.Join(d.restores, ","); got != vmID+"/snap-durable" {
		t.Fatalf("durable restores %q", got)
	}
	if n := len(provider.envs[vmID].checkpoints); n != 0 {
		t.Fatalf("%d in-process checkpoints, want 0", n)
	}

}

// TestSnapshotSteps_abortDeletesArtifactAndMarksError: the compensation of a
// snapshot whose record could not be finished leaves nothing on the host and
// the row in error with the cause.
func TestSnapshotSteps_abortDeletesArtifactAndMarksError(t *testing.T) {
	ctx := context.Background()
	provider := newSnapshotTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-abort")

	plan, err := fm.SnapshotBegin(ctx, vmID, SnapshotOptions{Comment: "c"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	created, err := fm.SnapshotCheckpoint(ctx, vmID, plan)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := fm.SnapshotRecordCreating(ctx, vmID, plan, created); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := fm.SnapshotAbort(ctx, vmID, plan, created, "ready write failed"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if n := len(provider.envs[vmID].checkpoints); n != 0 {
		t.Fatalf("%d checkpoints left on the host, want 0", n)
	}
	rec, err := fm.GetSnapshotByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.State != SnapshotStateError || rec.LastError != "ready write failed" {
		t.Fatalf("row %s %q, want error with the cause", rec.State, rec.LastError)
	}
}

// TestPermanentSnapshotError: refusals a retry cannot change are terminal,
// host failures and timeouts are retried.
func TestPermanentSnapshotError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("%w: fuse-x", ErrVMNotRunning), true},
		{fmt.Errorf("%w: q", ErrSnapshotQuotaExceeded), true},
		{fmt.Errorf("%w: vm", errSnapshotOptions), true},
		{fmt.Errorf("snapshot: %w", &HTTPStatusError{Code: 409, Body: "not the chain head"}), true},
		{&HTTPStatusError{Code: 408}, false},
		{&HTTPStatusError{Code: 503}, false},
		{errors.New("connection reset"), false},
		{nil, false},
	} {
		if got := PermanentSnapshotError(tc.err); got != tc.want {
			t.Errorf("PermanentSnapshotError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestRestoreSteps_validateRejectsNonReady: a restore validates the row
// before anything is marked.
func TestRestoreSteps_validateRejectsNonReady(t *testing.T) {
	ctx := context.Background()
	provider := newSnapshotTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-restore")
	rec, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fm.RestoreValidate(ctx, vmID, rec.SnapshotID)
	if err != nil || got.SnapshotID != rec.SnapshotID {
		t.Fatalf("validate = %+v, %v", got, err)
	}
	if err := fm.RestoreBegin(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.RestoreValidate(ctx, vmID, rec.SnapshotID); !errors.Is(err, ErrSnapshotInvalidState) {
		t.Fatalf("validate of a restoring row: %v, want ErrSnapshotInvalidState", err)
	}
	if err := fm.RestoreOnHost(ctx, vmID, rec.SnapshotID); err != nil {
		t.Fatalf("restore on host: %v", err)
	}
	if err := fm.RestoreFinish(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, _ := fm.GetSnapshotByID(ctx, rec.SnapshotID)
	if after.State != SnapshotStateReady {
		t.Fatalf("row %s after the restore, want ready", after.State)
	}
}

// TestReconcileSnapshots_durableSendsCollect: on restate an expired leaf is
// marked deleting and handed to the Environment object, keyed on the
// marking, instead of being deleted by reconcile itself.
func TestReconcileSnapshots_durableSendsCollect(t *testing.T) {
	ctx := context.Background()
	provider := newSnapshotTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-gc")
	first, _ := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{})
	// no lineage, or first would be kept as the second's parent.
	if _, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{NoLineage: true}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour)
	first.RetentionUntil = &expired
	if err := fm.upsertSnapshotRecord(ctx, first); err != nil {
		t.Fatal(err)
	}
	d := &fakeDurableSnapshots{}
	fm.SetDurableSnapshots(d)

	fm.reconcileSnapshots(ctx)
	fm.reconcileSnapshots(ctx)
	if len(d.collects) != 2 || d.collects[0] != d.collects[1] {
		t.Fatalf("collect sends %v, want two with the same key", d.collects)
	}
	if !strings.HasPrefix(d.collects[0], vmID+"/"+first.SnapshotID+"/collect:"+first.SnapshotID+":") {
		t.Fatalf("collect send %q", d.collects[0])
	}
	rec, _ := fm.GetSnapshotByID(ctx, first.SnapshotID)
	if rec.State != SnapshotStateDeleting {
		t.Fatalf("row %s, want deleting until the object collects it", rec.State)
	}
	if n := len(provider.envs[vmID].checkpoints); n != 2 {
		t.Fatalf("%d checkpoints, want both still there", n)
	}

	// a move that pinned it after the marking keeps it, back in ready.
	da := newFakeDurableArtifacts()
	fm.SetDurableArtifacts(da)
	if err := da.PinArtifacts(ctx, "move:x", 0, first.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := fm.SnapshotCollect(ctx, first.SnapshotID); err != nil {
		t.Fatalf("collect pinned: %v", err)
	}
	if rec, _ := fm.GetSnapshotByID(ctx, first.SnapshotID); rec.State != SnapshotStateReady {
		t.Fatalf("pinned row %s after collect, want ready", rec.State)
	}
	_ = da.ReleaseArtifacts(ctx, "move:x", first.SnapshotID)
	rec.State = SnapshotStateDeleting
	if err := fm.upsertSnapshotRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	if err := fm.SnapshotCollect(ctx, first.SnapshotID); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := fm.GetSnapshotByID(ctx, first.SnapshotID); err == nil {
		t.Fatal("collected row still there")
	}
	// a repeat after the row is gone does nothing.
	if err := fm.SnapshotCollect(ctx, first.SnapshotID); err != nil {
		t.Fatalf("collect again: %v", err)
	}
}

// TestReconcileSnapshots_skipsPinned: retention gc leaves a checkpoint that a
// move still holds a durable pin on.
func TestReconcileSnapshots_skipsPinned(t *testing.T) {
	ctx := context.Background()
	provider := newSnapshotTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-pin")
	first, _ := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{})
	// no lineage, or first would be kept as the second's parent.
	if _, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{NoLineage: true}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour)
	first.RetentionUntil = &expired
	if err := fm.upsertSnapshotRecord(ctx, first); err != nil {
		t.Fatal(err)
	}

	da := newFakeDurableArtifacts()
	fm.SetDurableArtifacts(da)
	if err := da.PinArtifacts(ctx, "move:x", 0, first.SnapshotID); err != nil {
		t.Fatal(err)
	}
	fm.reconcileSnapshots(ctx)
	inlineWait(fm)
	if _, err := fm.GetSnapshotByID(ctx, first.SnapshotID); err != nil {
		t.Fatalf("durably pinned checkpoint collected: %v", err)
	}
	_ = da.ReleaseArtifacts(ctx, "move:x", first.SnapshotID)
	fm.reconcileSnapshots(ctx)
	inlineWait(fm)
	if _, err := fm.GetSnapshotByID(ctx, first.SnapshotID); err == nil {
		t.Fatal("released checkpoint not collected")
	}
}
