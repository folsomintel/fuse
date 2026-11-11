package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

var deltaTestFiles = map[string]string{
	"vmstate": "d-vmstate", "mem.delta": "d-mem-delta", "rootfs.delta": "d-rootfs-delta", "live.json": "d-manifest",
}

// deltaMigrateEnv takes diff checkpoints and can be resumed, the way a
// firecracker env backed by a delta-capable host agent can.
type deltaMigrateEnv struct {
	liveMigrateEnv
	p *deltaMigrateProvider
}

func (e *deltaMigrateEnv) CheckpointDiff(ctx context.Context, comment, parent string, keepPaused bool) (Checkpoint, error) {
	e.p.mu.Lock()
	refuse := e.p.refuseDiff
	e.p.diffs = append(e.p.diffs, fmt.Sprintf("%s keep=%v", parent, keepPaused))
	e.p.mu.Unlock()
	if refuse {
		return Checkpoint{}, &HTTPStatusError{Code: http.StatusConflict, Body: "not the last live snapshot"}
	}
	cp, err := e.CheckpointWithDigest(ctx, comment)
	cp.Kind = SnapshotKindLive
	cp.Files = deltaTestFiles
	cp.Parent = parent
	return cp, err
}

func (e *deltaMigrateEnv) Resume(context.Context) error {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	e.p.resumes++
	return nil
}

type deltaMigrateProvider struct {
	*liveMigrateProvider
	refuseDiff bool
	diffs      []string
	resumes    int
}

func newDeltaMigrateProvider() *deltaMigrateProvider {
	return &deltaMigrateProvider{liveMigrateProvider: newLiveMigrateProvider()}
}

func (p *deltaMigrateProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, spec)
	env := &deltaMigrateEnv{
		liveMigrateEnv: liveMigrateEnv{
			digestForkEnv: digestForkEnv{snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}},
			files:         p.files,
		},
		p: p,
	}
	p.envs[spec.Name] = &env.digestForkEnv
	return env, nil
}

// deltaMover records every move and can fail the delta ones. mergedFiles is
// what its agent reports for a merged copy; nil plays one that reports no mem
// digest.
type deltaMover struct {
	recordingMover
	mu          sync.Mutex
	failDelta   bool
	mergedFiles map[string]string
}

func (m *deltaMover) MoveArtifact(ctx context.Context, move ArtifactMove) (ArtifactMoved, error) {
	m.mu.Lock()
	fail := m.failDelta && move.Base != ""
	m.mu.Unlock()
	if fail {
		return ArtifactMoved{}, errors.New("peer went away mid-delta")
	}
	moved, err := m.recordingMover.MoveArtifact(ctx, move)
	if err == nil && move.Base != "" {
		moved.Files = m.mergedFiles
	}
	return moved, err
}

func newDeltaFleet(t *testing.T, provider *deltaMigrateProvider, mover ArtifactMover) *FleetManager {
	t.Helper()
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", ArtifactMover: mover, CheckpointInterval: time.Hour})
	for _, id := range []string{"h-a", "h-b"} {
		if err := fm.RegisterHost(ctx, artifactHost(id, 8), provider); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src",
		Spec{CPUs: 1, RamMB: 512, HostID: "h-a"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision source: %v", err)
	}
	return fm
}

func TestMigrateVM_deltaCopiesTheBulkThenOnlyTheDiff(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{}
	fm := newDeltaFleet(t, provider, mover)

	newID, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true})
	if err != nil {
		t.Fatalf("delta migrate: %v", err)
	}

	moves := mover.calls()
	if len(moves) != 2 {
		t.Fatalf("%d moves, want the full seed then the delta: %+v", len(moves), moves)
	}
	full, delta := moves[0], moves[1]
	if full.Base != "" || !reflect.DeepEqual(full.Files, liveTestFiles) {
		t.Errorf("first move = %+v, want a full live copy", full)
	}
	if delta.Base != full.SnapshotID || !delta.DropBase || !reflect.DeepEqual(delta.Files, deltaTestFiles) {
		t.Errorf("second move = %+v, want the delta merged onto %s and the base dropped", delta, full.SnapshotID)
	}
	if delta.SourceSnapshotID == "" || full.SourceSnapshotID == "" {
		t.Error("a move did not pin the source snapshot it reads from")
	}
	if len(provider.diffs) != 1 || provider.diffs[0] != full.SourceSnapshotID+" keep=true" {
		t.Errorf("diffs = %v, want one against the seed with the source kept paused", provider.diffs)
	}
	info, _ := fm.GetVM(newID)
	if !info.Spec.ResumeSeed || info.Spec.SeedSnapshotID != delta.SnapshotID {
		t.Errorf("migrated vm resumes from %q (resume %v), want the merged copy %q", info.Spec.SeedSnapshotID, info.Spec.ResumeSeed, delta.SnapshotID)
	}
	if provider.resumes != 0 {
		t.Error("a successful migrate thawed the source it was about to destroy")
	}
	if _, ok := fm.GetVM("fuse-task-src"); ok {
		t.Error("source vm was not destroyed after the migration")
	}
	// this mover's agent reports no digests for the merged copy (a version 1
	// base, or an agent that predates them), so it records none and must not
	// offer itself as the source of a full move.
	merged, err := fm.GetSnapshotByID(context.Background(), delta.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveFiles(merged)) != 0 {
		t.Errorf("merged copy records live files %v", liveFiles(merged))
	}
	if err := fm.RegisterHost(context.Background(), artifactHost("h-c", 8), provider); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.ensureArtifactOnHost(context.Background(), merged, "h-c", false); !errors.Is(err, ErrArtifactImmovable) {
		t.Errorf("moving a merged copy with no digests = %v, want ErrArtifactImmovable", err)
	}
}

// mergedTestFiles is what an agent reports for a delta merged onto a version 2
// base: the merged copy's own digests, not the delta's.
var mergedTestFiles = map[string]string{"vmstate": "d-vmstate", "mem": "m-mem-table", "live.json": "m-manifest"}

// TestMigrateVM_aMergedCopyMovesOnToAThirdHost moves a vm from h-a to h-b with
// only the diff crossing after the bulk, then moves the merged copy on to h-c
// as a full live snapshot under the digests h-b's agent reported for it.
func TestMigrateVM_aMergedCopyMovesOnToAThirdHost(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{mergedFiles: mergedTestFiles}
	fm := newDeltaFleet(t, provider, mover)
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, artifactHost("h-c", 8), provider); err != nil {
		t.Fatal(err)
	}

	if _, err := fm.MigrateVM(ctx, "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true}); err != nil {
		t.Fatalf("delta migrate: %v", err)
	}
	moves := mover.calls()
	if len(moves) != 2 || moves[1].Base == "" {
		t.Fatalf("moves = %+v, want the full seed then the delta", moves)
	}
	merged, err := fm.GetSnapshotByID(ctx, moves[1].SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(liveFiles(merged), mergedTestFiles) {
		t.Fatalf("merged copy records %v, want the digests its agent reported", liveFiles(merged))
	}

	localID, err := fm.ensureArtifactOnHost(ctx, merged, "h-c", false)
	if err != nil {
		t.Fatalf("move the merged copy on to h-c: %v", err)
	}
	moves = mover.calls()
	if len(moves) != 3 {
		t.Fatalf("%d moves, want one more: the merged copy in full", len(moves))
	}
	third := moves[2]
	if third.From.HostID != "h-b" || third.SourceSnapshotID != merged.SnapshotID || third.Base != "" {
		t.Errorf("third move = %+v, want a full copy read out of %s on h-b", third, merged.SnapshotID)
	}
	if !reflect.DeepEqual(third.Files, mergedTestFiles) || third.Digest != merged.Digest {
		t.Errorf("third move files %v digest %s, want the merged copy's", third.Files, third.Digest)
	}
	replica, err := fm.GetSnapshotByID(ctx, localID)
	if err != nil {
		t.Fatal(err)
	}
	if replica.HostID != "h-c" || replica.Kind != SnapshotKindLive || !reflect.DeepEqual(liveFiles(replica), mergedTestFiles) {
		t.Errorf("copy on h-c = host %s kind %s files %v, want a live record that can move again",
			replica.HostID, replica.Kind, liveFiles(replica))
	}
}

func TestCompleteLiveFiles(t *testing.T) {
	for _, tc := range []struct {
		files map[string]string
		want  bool
	}{
		{mergedTestFiles, true},
		{nil, false},
		{map[string]string{"vmstate": "a", "live.json": "b"}, false},
		{map[string]string{"vmstate": "a", "mem": "", "live.json": "b"}, false},
		{map[string]string{"vmstate": "a", "mem": "c", "live.json": "b", "mem.delta": "d"}, false},
	} {
		if got := completeLiveFiles(tc.files); got != tc.want {
			t.Errorf("completeLiveFiles(%v) = %v, want %v", tc.files, got, tc.want)
		}
	}
}

func TestMigrateVM_aFailedDeltaThawsTheSource(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{failDelta: true}
	fm := newDeltaFleet(t, provider, mover)

	_, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true})
	if !errors.Is(err, ErrArtifactTransferFailed) {
		t.Fatalf("err = %v, want a transfer failure", err)
	}
	if provider.resumes != 1 {
		t.Errorf("source resumed %d times, want once: a frozen source must never be left frozen", provider.resumes)
	}
	if info, ok := fm.GetVM("fuse-task-src"); !ok || info.State != VMStateRunning {
		t.Errorf("source vm = %+v (tracked %v), want it still running", info.State, ok)
	}
	if vms := fm.ListFleet(); len(vms) != 1 {
		t.Errorf("%d vms tracked, want only the source", len(vms))
	}
}

func TestMigrateVM_aRefusedDiffLeavesTheSourceRunning(t *testing.T) {
	provider := newDeltaMigrateProvider()
	provider.refuseDiff = true
	fm := newDeltaFleet(t, provider, &deltaMover{})

	if _, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true}); err == nil {
		t.Fatal("migrate succeeded with the diff refused")
	}
	// the host refused before pausing anything, so there is nothing to thaw.
	if provider.resumes != 0 {
		t.Errorf("source resumed %d times, want none", provider.resumes)
	}
	if info, ok := fm.GetVM("fuse-task-src"); !ok || info.State != VMStateRunning {
		t.Errorf("source vm = %+v (tracked %v), want it still running", info.State, ok)
	}
}

func TestCheckpointChain_advancesAndAMigrateToTheStandbyMovesOnlyTheDiff(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{}
	fm := newDeltaFleet(t, provider, mover)
	ctx := context.Background()

	fm.advanceChain(ctx, "fuse-task-src")
	c := fm.chainFor("fuse-task-src")
	if c.standby != "h-b" || c.base == "" {
		t.Fatalf("after the first tick chain = standby %q base %q, want a full copy on h-b", c.standby, c.base)
	}
	firstHead, firstBase := c.head.SnapshotID, c.base

	fm.advanceChain(ctx, "fuse-task-src")
	moves := mover.calls()
	if len(moves) != 2 || moves[1].Base != firstBase || !moves[1].DropBase {
		t.Fatalf("moves = %+v, want a full copy then a delta onto %s", moves, firstBase)
	}
	if c.head.SnapshotID == firstHead || c.base != moves[1].SnapshotID {
		t.Errorf("chain did not advance: head %s base %s", c.head.SnapshotID, c.base)
	}
	if _, err := fm.GetSnapshotByID(ctx, firstHead); err == nil {
		t.Errorf("superseded checkpoint %s is still recorded", firstHead)
	}

	stagedBase, stagedHead := c.base, c.head.SnapshotID
	if _, err := fm.MigrateVM(ctx, "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true}); err != nil {
		t.Fatalf("migrate to the standby: %v", err)
	}
	moves = mover.calls()
	if len(moves) != 3 {
		t.Fatalf("%d moves, want exactly one more: the final delta", len(moves))
	}
	if last := moves[2]; last.Base != stagedBase || !last.DropBase {
		t.Errorf("final move = %+v, want a delta onto the staged base %s", last, stagedBase)
	}
	if got := provider.diffs[len(provider.diffs)-1]; got != stagedHead+" keep=true" {
		t.Errorf("final diff = %q, want one against the chain head %s with the source kept paused", got, stagedHead)
	}
	fm.chainsMu.Lock()
	_, kept := fm.chains["fuse-task-src"]
	fm.chainsMu.Unlock()
	if kept {
		t.Error("the migrated vm's chain outlived it")
	}
	if pinnedArtifacts(fm) != 0 {
		t.Errorf("%d artifacts still pinned after the migrate", pinnedArtifacts(fm))
	}
}

func TestCheckpointChain_aRefusedDiffStartsOver(t *testing.T) {
	provider := newDeltaMigrateProvider()
	mover := &deltaMover{}
	fm := newDeltaFleet(t, provider, mover)
	ctx := context.Background()

	fm.advanceChain(ctx, "fuse-task-src")
	provider.mu.Lock()
	provider.refuseDiff = true
	provider.mu.Unlock()
	fm.advanceChain(ctx, "fuse-task-src")

	c := fm.chainFor("fuse-task-src")
	if c.base != "" || c.head.SnapshotID != "" {
		t.Errorf("chain = head %s base %s after a refused diff, want it empty", c.head.SnapshotID, c.base)
	}
	if pinnedArtifacts(fm) != 0 {
		t.Errorf("%d artifacts still pinned after the chain reset", pinnedArtifacts(fm))
	}

	provider.mu.Lock()
	provider.refuseDiff = false
	provider.mu.Unlock()
	fm.advanceChain(ctx, "fuse-task-src")
	if moves := mover.calls(); len(moves) != 2 || moves[1].Base != "" {
		t.Errorf("moves = %+v, want the restart to copy a fresh full checkpoint", moves)
	}
}
