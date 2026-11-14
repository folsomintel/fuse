package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// snapFleet scripts the snapshot, restore, gc and checkpoint chain steps.
// fail[step] errors are returned in turn before the step succeeds; block,
// when set for a step, holds it until closed and signals entered first.
type snapFleet struct {
	fakeFleet

	mu      sync.Mutex
	calls   []string
	fail    map[string][]error
	block   map[string]chan struct{}
	entered chan string
	seq     int
	aborted string
	failed  string
	retired []orchestrator.ChainState
	mirror  orchestrator.ChainState
	pending bool
	mirrors int
}

func newSnapFleet() *snapFleet {
	return &snapFleet{fail: map[string][]error{}, block: map[string]chan struct{}{}, entered: make(chan string, 16)}
}

// enter records step, waits if it is blocked, and returns its next scripted
// error.
func (f *snapFleet) enter(ctx context.Context, step string) error {
	f.mu.Lock()
	f.calls = append(f.calls, step)
	block := f.block[step]
	f.mu.Unlock()
	if block != nil {
		select {
		case f.entered <- step:
		default:
		}
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if errs := f.fail[step]; len(errs) > 0 {
		err := errs[0]
		if len(errs) > 1 {
			f.fail[step] = errs[1:]
		} else if errs[0] != errAlways {
			delete(f.fail, step)
		}
		return err
	}
	return nil
}

// errAlways, as the last scripted error of a step, makes it fail forever.
var errAlways = errors.New("store down")

func (f *snapFleet) steps() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *snapFleet) SnapshotBegin(ctx context.Context, vm string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotPlan, error) {
	return orchestrator.SnapshotPlan{Options: opts, HostID: "h1"}, f.enter(ctx, "begin")
}

func (f *snapFleet) SnapshotCheckpoint(ctx context.Context, vm string, plan orchestrator.SnapshotPlan) (orchestrator.Checkpoint, error) {
	if err := f.enter(ctx, "checkpoint"); err != nil {
		return orchestrator.Checkpoint{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return orchestrator.Checkpoint{ID: fmt.Sprintf("snap-%d", f.seq), SizeBytes: 10, Kind: orchestrator.SnapshotKindLive}, nil
}

func (f *snapFleet) SnapshotRecordCreating(ctx context.Context, vm string, plan orchestrator.SnapshotPlan, cp orchestrator.Checkpoint) (orchestrator.SnapshotRecord, error) {
	return orchestrator.SnapshotRecord{SnapshotID: cp.ID, VMID: vm, HostID: plan.HostID, SizeBytes: cp.SizeBytes,
		State: orchestrator.SnapshotStateCreating}, f.enter(ctx, "record")
}

func (f *snapFleet) SnapshotList(ctx context.Context, vm, id string) (orchestrator.Checkpoint, error) {
	return orchestrator.Checkpoint{ID: id}, f.enter(ctx, "list")
}

func (f *snapFleet) SnapshotReady(ctx context.Context, rec orchestrator.SnapshotRecord, _ orchestrator.Checkpoint) (orchestrator.SnapshotRecord, error) {
	rec.State = orchestrator.SnapshotStateReady
	return rec, f.enter(ctx, "ready")
}

func (f *snapFleet) SnapshotAbort(ctx context.Context, vm string, _ orchestrator.SnapshotPlan, cp orchestrator.Checkpoint, cause string) error {
	f.mu.Lock()
	f.aborted = cp.ID + ": " + cause
	f.mu.Unlock()
	return f.enter(ctx, "abort")
}

func (f *snapFleet) RestoreValidate(ctx context.Context, vm, id string) (orchestrator.SnapshotRecord, error) {
	return orchestrator.SnapshotRecord{SnapshotID: id, VMID: vm}, f.enter(ctx, "validate")
}

func (f *snapFleet) RestoreBegin(ctx context.Context, _ orchestrator.SnapshotRecord) error {
	return f.enter(ctx, "restoring")
}

func (f *snapFleet) RestoreOnHost(ctx context.Context, _, _ string) error {
	return f.enter(ctx, "restore")
}

func (f *snapFleet) RestoreFinish(ctx context.Context, _ orchestrator.SnapshotRecord) error {
	return f.enter(ctx, "finish")
}

func (f *snapFleet) RestoreFailed(ctx context.Context, _ orchestrator.SnapshotRecord, lastErr string) error {
	f.mu.Lock()
	f.failed = lastErr
	f.mu.Unlock()
	return f.enter(ctx, "failed")
}

func (f *snapFleet) SnapshotCollect(ctx context.Context, _ string) error {
	return f.enter(ctx, "collect")
}

func (f *snapFleet) SnapshotCollectFailed(ctx context.Context, _, lastErr string) error {
	f.mu.Lock()
	f.failed = lastErr
	f.mu.Unlock()
	return f.enter(ctx, "collect_failed")
}

func (f *snapFleet) ChainBegin(ctx context.Context, vm string, chain orchestrator.ChainState) (orchestrator.ChainPlan, error) {
	if err := f.enter(ctx, "plan"); err != nil {
		return orchestrator.ChainPlan{}, err
	}
	if chain.Base != "" {
		return orchestrator.ChainPlan{Diff: true, Standby: chain.Standby,
			Options: orchestrator.SnapshotOptions{Live: true, Diff: true, Parent: chain.Head.SnapshotID}}, nil
	}
	return orchestrator.ChainPlan{Standby: "h2", Options: orchestrator.SnapshotOptions{Live: true}}, nil
}

func (f *snapFleet) ChainMovePlan(ctx context.Context, chain orchestrator.ChainState, plan orchestrator.ChainPlan, child orchestrator.SnapshotRecord) (orchestrator.ChainMove, error) {
	job := orchestrator.ArtifactMoveJob{
		SnapshotID: child.SnapshotID, SourceSnapshotID: child.SnapshotID, SourceHostID: "h1",
		HostID: plan.Standby, LocalID: fmt.Sprintf("%s-on-%s-%d", child.SnapshotID, plan.Standby, time.Now().UnixNano()),
	}
	if plan.Diff {
		job.Base, job.DropBase = chain.Base, true
	}
	return orchestrator.ChainMove{Job: job}, f.enter(ctx, "plan_move")
}

func (f *snapFleet) ChainRetire(ctx context.Context, _ string, chain orchestrator.ChainState) error {
	f.mu.Lock()
	f.retired = append(f.retired, chain)
	f.mu.Unlock()
	return f.enter(ctx, "retire")
}

func (f *snapFleet) ChainReset(ctx context.Context, _, _ string) error {
	return f.enter(ctx, "reset")
}

func (f *snapFleet) ChainAdvanced(ctx context.Context, _ string, _ orchestrator.ChainState, _ bool) error {
	return f.enter(ctx, "advanced")
}

func (f *snapFleet) MirrorChain(_ string, chain orchestrator.ChainState, pending bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mirror, f.pending = chain, pending
	f.mirrors++
}

func (f *snapFleet) mirrored() (orchestrator.ChainState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mirror, f.pending
}

// waitSettled polls the mirror until the chain's copy has ended.
func (f *snapFleet) waitSettled(t *testing.T) orchestrator.ChainState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		chain, pending := f.mirrored()
		if !pending {
			return chain
		}
		if time.Now().After(deadline) {
			t.Fatal("chain copy never settled")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func uniqueVM(name string) string {
	return fmt.Sprintf("fuse-%s-%d", name, time.Now().UnixNano())
}

// TestSnapshotThroughRestate: a host checkpoint that fails twice is retried
// inside its step, and the record runs once against the checkpoint that
// succeeded.
func TestSnapshotThroughRestate(t *testing.T) {
	fleet := newSnapFleet()
	fleet.fail["checkpoint"] = []error{errors.New("agent 503"), errors.New("agent 503")}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec, err := client.Snapshot(ctx, uniqueVM("snap"), orchestrator.SnapshotOptions{Comment: "c"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if rec.SnapshotID != "snap-1" || rec.State != orchestrator.SnapshotStateReady {
		t.Fatalf("record %+v, want snap-1 ready", rec)
	}
	if got, want := fleet.steps(), "begin,checkpoint,checkpoint,checkpoint,record,list,ready"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

// TestSnapshotRefusalKeepsItsSentinel: a refusal crosses restate as its code
// and comes back as the fleet error the api maps to a status.
func TestSnapshotRefusalKeepsItsSentinel(t *testing.T) {
	fleet := newSnapFleet()
	fleet.fail["begin"] = []error{fmt.Errorf("%w: vm in state stopped", orchestrator.ErrVMNotRunning)}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := client.Snapshot(ctx, uniqueVM("refused"), orchestrator.SnapshotOptions{})
	if !errors.Is(err, orchestrator.ErrVMNotRunning) {
		t.Fatalf("snapshot error %v, want ErrVMNotRunning", err)
	}
	if got := fleet.steps(); got != "begin" {
		t.Fatalf("steps %s, want only begin", got)
	}
}

// TestSnapshotAbortsWhenTheRecordCannotFinish: a ready write that never
// succeeds runs the compensation, which deletes the host artifact and marks
// the row error.
func TestSnapshotAbortsWhenTheRecordCannotFinish(t *testing.T) {
	fleet := newSnapFleet()
	fleet.fail["ready"] = []error{errAlways}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.Snapshot(ctx, uniqueVM("abort"), orchestrator.SnapshotOptions{}); err == nil {
		t.Fatal("snapshot succeeded with a ready write that never does")
	}
	want := "begin,checkpoint,record,list," + strings.Repeat("ready,", fastPolicy.MaxAttempts) + "abort"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if !strings.HasPrefix(fleet.aborted, "snap-1: ") || !strings.Contains(fleet.aborted, "store down") {
		t.Fatalf("abort %q, want snap-1 with the ready error", fleet.aborted)
	}
}

// TestSnapshotSurvivesEndpointRestart stands in for kill -9 of the
// orchestrator between the host checkpoint and the row write: the endpoint
// drops while the record step runs, and the restarted one writes the row for
// the checkpoint already taken instead of taking a second one.
func TestSnapshotSurvivesEndpointRestart(t *testing.T) {
	first := newSnapFleet()
	first.block["record"] = make(chan struct{})
	defer close(first.block["record"])
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var rec orchestrator.SnapshotRecord
	go func() {
		var err error
		rec, err = client.Snapshot(ctx, uniqueVM("snap-restart"), orchestrator.SnapshotOptions{})
		done <- err
	}()
	<-first.entered
	stop()

	second := newSnapFleet()
	_, _, _ = serveFleetAt(t, second, addr)
	if err := <-done; err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if rec.SnapshotID != "snap-1" {
		t.Fatalf("record %s, want the first endpoint's checkpoint snap-1", rec.SnapshotID)
	}
	if got, want := second.steps(), "record,list,ready"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
}

// TestRestoreThroughRestate: a host restore that fails once is retried, and a
// host that refuses for good marks the row error and fails the restore.
func TestRestoreThroughRestate(t *testing.T) {
	fleet := newSnapFleet()
	fleet.fail["restore"] = []error{errors.New("agent 503")}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Restore(ctx, uniqueVM("restore"), "snap-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, want := fleet.steps(), "validate,restoring,restore,restore,finish"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}

	refused := newSnapFleet()
	refused.fail["restore"] = []error{&orchestrator.HTTPStatusError{Code: 409, Body: "vm busy"}}
	client = serveFleet(t, refused, fastPolicy)
	if err := client.Restore(ctx, uniqueVM("restore-refused"), "snap-1"); err == nil || !strings.Contains(err.Error(), "vm busy") {
		t.Fatalf("restore error %v, want the agent's refusal", err)
	}
	if got, want := refused.steps(), "validate,restoring,restore,failed"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if !strings.Contains(refused.failed, "vm busy") {
		t.Fatalf("row error %q, want the refusal", refused.failed)
	}
}

// TestRestoreSurvivesEndpointRestart stands in for kill -9 during a
// restore: the restarted endpoint runs the restore to the end instead of
// leaving the row in restoring.
func TestRestoreSurvivesEndpointRestart(t *testing.T) {
	first := newSnapFleet()
	first.block["restore"] = make(chan struct{})
	defer close(first.block["restore"])
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Restore(ctx, uniqueVM("restore-restart"), "snap-1") }()
	<-first.entered
	stop()

	second := newSnapFleet()
	_, _, _ = serveFleetAt(t, second, addr)
	if err := <-done; err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, want := second.steps(), "restore,finish"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
}

// TestCollectSnapshotRecordsFailureWhenRetriesRunOut: a gc deletion that
// keeps failing is retried under the policy, then the row is marked error.
func TestCollectSnapshotRecordsFailureWhenRetriesRunOut(t *testing.T) {
	fleet := newSnapFleet()
	fleet.fail["collect"] = []error{errAlways}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	key := fmt.Sprintf("collect:snap-1:%d", time.Now().UnixNano())
	if err := client.SendCollectSnapshot(ctx, uniqueVM("gc"), "snap-1", key); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(strings.Repeat("collect,", fastPolicy.MaxAttempts), ",") + ",collect_failed"
	deadline := time.Now().Add(10 * time.Second)
	for fleet.steps() != want {
		if time.Now().After(deadline) {
			t.Fatalf("steps %s, want %s", fleet.steps(), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(fleet.failed, "store down") {
		t.Fatalf("row error %q, want the last delete error", fleet.failed)
	}
}

// TestCheckpointChainThroughRestate: a tick on an empty chain takes a full
// checkpoint and sends its copy to Artifacts, which reports back to
// ChainMoved; the copy becomes the base, pinned on restate. the next tick
// merges a diff onto it, moving the pin, and a destroy retires the chain.
func TestCheckpointChainThroughRestate(t *testing.T) {
	fleet := newSnapFleet()
	client := serveFleet(t, fleet, fastPolicy)
	vm := uniqueVM("chain")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Checkpoint(ctx, vm); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	chain := fleet.waitSettled(t)
	if chain.Standby != "h2" || chain.Head.SnapshotID != "snap-1" || chain.Base == "" {
		t.Fatalf("chain after the first tick %+v, want snap-1 copied to h2", chain)
	}
	firstBase := chain.Base
	if p := pinState(t, ctx, client, []string{firstBase}); !p[0] {
		t.Fatalf("base %s not pinned", firstBase)
	}

	if err := client.Checkpoint(ctx, vm); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	chain = fleet.waitSettled(t)
	if chain.Head.SnapshotID != "snap-2" || chain.Base == firstBase || chain.Base == "" {
		t.Fatalf("chain after the diff %+v, want snap-2 merged onto a new base", chain)
	}
	waitUnpinned(t, ctx, client, []string{firstBase})
	if p := pinState(t, ctx, client, []string{chain.Base}); !p[0] {
		t.Fatalf("new base %s not pinned", chain.Base)
	}
	want := "plan,begin,checkpoint,record,list,ready,plan_move,advanced," +
		"plan,begin,checkpoint,record,list,ready,plan_move,retire,advanced"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}

	if err := client.Destroy(ctx, vm, orchestrator.DestroyRequest{Reason: "manual_destroy"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	waitUnpinned(t, ctx, client, []string{chain.Base})
	fleet.mu.Lock()
	last := fleet.retired[len(fleet.retired)-1]
	fleet.mu.Unlock()
	if last.Base != chain.Base || last.Head.SnapshotID != "snap-2" {
		t.Fatalf("destroy retired %+v, want the whole chain", last)
	}
	if c, _ := fleet.mirrored(); !c.Empty() {
		t.Fatalf("mirror %+v after the destroy, want empty", c)
	}
}

// TestHoldChainKeepsTheTickOff: while a migrate holds the chain a tick does
// nothing, and once released it runs again.
func TestHoldChainKeepsTheTickOff(t *testing.T) {
	fleet := newSnapFleet()
	client := serveFleet(t, fleet, fastPolicy)
	vm := uniqueVM("hold")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hold, err := client.HoldChain(ctx, vm, time.Hour)
	if err != nil || hold.Busy {
		t.Fatalf("hold = %+v, %v", hold, err)
	}
	if err := client.Checkpoint(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if got := fleet.steps(); got != "" {
		t.Fatalf("held tick ran steps %s", got)
	}
	if err := client.ReleaseChain(ctx, vm, false); err != nil {
		t.Fatal(err)
	}
	if err := client.Checkpoint(ctx, vm); err != nil {
		t.Fatal(err)
	}
	fleet.waitSettled(t)
	if got := fleet.steps(); !strings.HasPrefix(got, "plan,") {
		t.Fatalf("released tick ran %q, want it to plan", got)
	}
}

// TestCheckpointChainFailedCopyRetiresADiff: a delta merge that fails for
// good deletes the child and retires the chain, whose base it may have
// consumed; the pin on that base goes with it.
func TestCheckpointChainFailedCopyRetiresADiff(t *testing.T) {
	fleet := newSnapFleet()
	client := serveFleet(t, fleet, fastPolicy)
	vm := uniqueVM("chain-fail")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Checkpoint(ctx, vm); err != nil {
		t.Fatal(err)
	}
	base := fleet.waitSettled(t).Base

	failing := &failingPullFleet{snapFleet: fleet}
	client = serveFleet(t, failing, fastPolicy)
	if err := client.Checkpoint(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if chain := fleet.waitSettled(t); !chain.Empty() {
		t.Fatalf("chain %+v after a failed merge, want it retired", chain)
	}
	waitUnpinned(t, ctx, client, []string{base})
	fleet.mu.Lock()
	retired := fleet.retired
	fleet.mu.Unlock()
	if len(retired) != 2 || retired[0].Head.SnapshotID != "snap-2" || retired[1].Base != base {
		t.Fatalf("retired %+v, want the child then the chain on %s", retired, base)
	}
}

// failingPullFleet refuses every pull for good.
type failingPullFleet struct{ *snapFleet }

func (f *failingPullFleet) ArtifactPull(context.Context, orchestrator.ArtifactMoveJob) (orchestrator.ArtifactMoved, error) {
	return orchestrator.ArtifactMoved{}, &orchestrator.HTTPStatusError{Code: 409, Body: "digest mismatch"}
}
