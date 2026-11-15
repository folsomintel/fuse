package orchestrator

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// fakeDurableHitl records what the fleet hands restate.
type fakeDurableHitl struct {
	mu      sync.Mutex
	runs    []string
	expires map[string]time.Duration
	err     error
}

func (d *fakeDurableHitl) SendHitlRun(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.runs = append(d.runs, id)
	return nil
}

func (d *fakeDurableHitl) SendHitlExpire(_ context.Context, id string, delay time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.expires == nil {
		d.expires = map[string]time.Duration{}
	}
	d.expires[id] = delay
	return nil
}

func (d *fakeDurableHitl) sent() ([]string, map[string]time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]time.Duration{}
	for k, v := range d.expires {
		out[k] = v
	}
	return append([]string(nil), d.runs...), out
}

// fakeHitlMetrics records the hitl metrics.
type fakeHitlMetrics struct {
	mu       sync.Mutex
	decided  []string
	deciders []string
	runs     []string
}

func (m *fakeHitlMetrics) ReconcileCompleted(ReconcileSummary) {}
func (m *fakeHitlMetrics) HitlDecided(kind, outcome string, _ time.Duration) {
	m.mu.Lock()
	m.decided = append(m.decided, kind+":"+outcome)
	m.mu.Unlock()
}
func (m *fakeHitlMetrics) HitlDeciderAsked(verdict string, _ time.Duration) {
	m.mu.Lock()
	m.deciders = append(m.deciders, verdict)
	m.mu.Unlock()
}
func (m *fakeHitlMetrics) HitlRunFinished(action string, _ time.Duration, failed bool) {
	m.mu.Lock()
	r := action + ":ok"
	if failed {
		r = action + ":error"
	}
	m.runs = append(m.runs, r)
	m.mu.Unlock()
}

func durableHitlFleet(t *testing.T) (*FleetManager, string, *fakeDurableHitl, *fakeHitlMetrics) {
	t.Helper()
	m := &fakeHitlMetrics{}
	fm, vmID := hitlFleet(t, FleetConfig{Metrics: m}, gateSpec())
	d := &fakeDurableHitl{}
	fm.SetDurableHitl(d)
	return fm, vmID, d, m
}

// approved exec in mode self: the approval is handed to restate and nothing
// runs in process until the durable run's step does; the step runs it once,
// writes the result and wakes a waiter.
func TestDurableHitlApprovalGoesToRestate(t *testing.T) {
	fm, vmID, d, m := durableHitlFleet(t)
	env := mockEnvOf(fm, vmID)
	env.execResult = ExecResult{Stdout: []byte("pushed")}
	ctx := context.Background()

	held, err := gateExec(fm, vmID, "git", "push", "--force")
	if err != nil || held == nil {
		t.Fatalf("held=%v err=%v", held, err)
	}
	fm.hitlSends.Wait()
	_, expires := d.sent()
	if delay, ok := expires[held.ID]; !ok || delay <= 0 || delay > 600*time.Second {
		t.Fatalf("expire timer %v (armed %v), want the deadline", delay, ok)
	}

	waited := make(chan hitl.Intervention, 1)
	go func() {
		in, _ := fm.WaitIntervention(ctx, held.ID, 5*time.Second)
		waited <- in
	}()
	if _, err := fm.DecideIntervention(ctx, held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, By: approver}); err != nil {
		t.Fatal(err)
	}
	fm.hitlSends.Wait()
	if runs, _ := d.sent(); !slices.Equal(runs, []string{held.ID}) {
		t.Fatalf("runs sent %v, want %s", runs, held.ID)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(env.execArgv); n != 0 {
		t.Fatalf("the approval ran in process %d times", n)
	}

	plan, err := fm.HitlRunBegin(ctx, held.ID)
	if err != nil || plan.Route != HitlRouteInProcess || plan.EnvID != vmID {
		t.Fatalf("plan %+v err %v, want in process on %s", plan, err, vmID)
	}
	if err := fm.HitlRunInProcess(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	// a replayed step finds the result and does not run again.
	if err := fm.HitlRunInProcess(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	if plan, _ := fm.HitlRunBegin(ctx, held.ID); plan.Route != HitlRouteNone {
		t.Fatalf("plan after the run %+v, want nothing left", plan)
	}
	if n := len(env.execArgv); n != 1 {
		t.Fatalf("exec ran %d times, want once", n)
	}
	select {
	case in := <-waited:
		if in.Result == nil || in.Result.Exec == nil || in.Result.Exec.Stdout != "pushed" {
			t.Fatalf("waiter got %+v", in.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never woke")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Equal(m.decided, []string{"exec:approve"}) || !slices.Equal(m.runs, []string{"exec:ok"}) {
		t.Fatalf("metrics decided %v runs %v", m.decided, m.runs)
	}
}

// an action with no idempotency key runs at most once: an attempt that finds
// an earlier one's start mark settles it as possibly run.
func TestDurableHitlRunInProcessAtMostOnce(t *testing.T) {
	fm, vmID, _, _ := durableHitlFleet(t)
	ctx := context.Background()
	held, err := fm.GateLifecycle(ctx, vmID, hitl.ActionDrain, nil, GateCaller{By: testActor})
	if err != nil || held == nil {
		t.Fatalf("drain should be held: %v", err)
	}
	if _, err := fm.DecideIntervention(ctx, held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, By: approver}); err != nil {
		t.Fatal(err)
	}
	// an earlier attempt began and died.
	if started, err := fm.markRunStarted(ctx, held.ID); err != nil || started {
		t.Fatalf("first mark: started=%v err=%v", started, err)
	}
	if in, _ := fm.GetIntervention(ctx, held.ID); in.Settled() {
		t.Fatalf("the start mark must not settle the intervention: %+v", in.Result)
	}
	if err := fm.HitlRunInProcess(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := fm.GetIntervention(ctx, held.ID)
	if got.Result == nil || got.Result.Error != errRunInterrupted {
		t.Fatalf("result %+v, want the interrupted error", got.Result)
	}
	if info, _ := fm.GetVM(vmID); info.State != VMStateRunning {
		t.Fatalf("vm state %s: the drain ran a second time", info.State)
	}
}

func TestDurableHitlRunInProcessRunsDrain(t *testing.T) {
	fm, vmID, _, _ := durableHitlFleet(t)
	ctx := context.Background()
	held, _ := fm.GateLifecycle(ctx, vmID, hitl.ActionDrain, nil, GateCaller{By: testActor})
	if _, err := fm.DecideIntervention(ctx, held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, By: approver}); err != nil {
		t.Fatal(err)
	}
	if plan, _ := fm.HitlRunBegin(ctx, held.ID); plan.Route != HitlRouteInProcess {
		t.Fatalf("drain plan %+v, want in process", plan)
	}
	if err := fm.HitlRunInProcess(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := fm.GetIntervention(ctx, held.ID)
	if got.Result == nil || got.Result.Error != "" {
		t.Fatalf("drain result %+v", got.Result)
	}
	if info, _ := fm.GetVM(vmID); info.State != VMStateDraining {
		t.Fatalf("vm state %s, want draining", info.State)
	}
}

// destroy, snapshot and token rotation go to their environment handler.
func TestDurableHitlRunBeginRoutes(t *testing.T) {
	spec := &hitl.Spec{TimeoutSeconds: 600, Gates: []hitl.Gate{
		{Name: "life", On: hitl.KindLifecycle, Match: hitl.GateMatch{Action: []string{"destroy", "snapshot", "rotate-token"}}},
	}}
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	fm.SetDurableHitl(&fakeDurableHitl{})
	ctx := context.Background()
	approve := func(action hitl.LifecycleAction, params any) string {
		t.Helper()
		held, err := fm.GateLifecycle(ctx, vmID, action, params, GateCaller{By: testActor})
		if err != nil || held == nil {
			t.Fatalf("%s should be held: %v", action, err)
		}
		if _, err := fm.DecideIntervention(ctx, held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, By: approver}); err != nil {
			t.Fatal(err)
		}
		return held.ID
	}

	destroy := approve(hitl.ActionDestroy, nil)
	snap := approve(hitl.ActionSnapshot, SnapshotOptions{Comment: "before deploy"})
	rotate := approve(hitl.ActionRotateToken, nil)
	for id, want := range map[string]HitlRunRoute{destroy: HitlRouteDestroy, snap: HitlRouteSnapshot, rotate: HitlRouteRotateToken} {
		plan, err := fm.HitlRunBegin(ctx, id)
		if err != nil || plan.Route != want || plan.EnvID != vmID {
			t.Fatalf("plan %+v err %v, want %s on %s", plan, err, want, vmID)
		}
		if want == HitlRouteSnapshot && (plan.Snapshot == nil || plan.Snapshot.Comment != "before deploy") {
			t.Fatalf("snapshot plan %+v, want the approved options", plan.Snapshot)
		}
	}

	if err := fm.HitlRunFinish(ctx, snap, hitl.Result{SnapshotID: "snap-9"}); err != nil {
		t.Fatal(err)
	}
	// a replayed finish keeps the first result.
	if err := fm.HitlRunFinish(ctx, snap, hitl.Result{Error: "late"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := fm.GetIntervention(ctx, snap); got.Result == nil || got.Result.SnapshotID != "snap-9" {
		t.Fatalf("snapshot result %+v", got.Result)
	}

	// a destroy of an environment that is gone settles with not found.
	fm.mu.Lock()
	delete(fm.vms, vmID)
	fm.mu.Unlock()
	if plan, err := fm.HitlRunBegin(ctx, destroy); err != nil || plan.Route != HitlRouteNone {
		t.Fatalf("plan %+v err %v, want nothing to run", plan, err)
	}
	if got, _ := fm.loadIntervention(ctx, destroy); got.Result == nil || got.Result.Error == "" {
		t.Fatalf("gone destroy result %+v, want an error", got.Result)
	}
}

// an approval whose send never reached restate is sent again by the
// restart's first reconcile, instead of being settled as maybe-run.
func TestDurableHitlResumesApprovedRuns(t *testing.T) {
	fm, vmID, d, _ := durableHitlFleet(t)
	now := time.Now()
	in := hitl.Intervention{
		ID: hitl.NewID(now), EnvID: vmID, Kind: hitl.KindExec, Gate: "force-push", Mode: hitl.ModeAsk,
		State: hitl.StateApproved, Exec: &hitl.ExecPayload{Argv: []string{"git"}}, TriggeredBy: testActor,
		Deadline: now.Add(time.Minute), Decision: &hitl.Decision{Outcome: hitl.OutcomeApprove, DecidedBy: approver, DecidedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	rec, err := fm.interventionToRecord(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.store.UpsertIntervention(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	fm.reconcileInterventions(context.Background())
	fm.hitlSends.Wait()
	if runs, _ := d.sent(); !slices.Equal(runs, []string{in.ID}) {
		t.Fatalf("runs sent %v, want %s", runs, in.ID)
	}
	if got, _ := fm.GetIntervention(context.Background(), in.ID); got.Result != nil {
		t.Fatalf("result %+v, want the run left to restate", got.Result)
	}
}

// a send restate refused is sent again on the next reconcile.
func TestDurableHitlResendsAfterFailedSend(t *testing.T) {
	fm, vmID, d, _ := durableHitlFleet(t)
	ctx := context.Background()
	fm.reconcileInterventions(ctx) // the startup pass, with nothing owed.
	held, _ := gateExec(fm, vmID, "git", "push", "--force")
	d.mu.Lock()
	d.err = errors.New("restate down")
	d.mu.Unlock()
	if _, err := fm.DecideIntervention(ctx, held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, By: approver}); err != nil {
		t.Fatal(err)
	}
	fm.hitlSends.Wait()
	if !fm.hitlResend.Load() {
		t.Fatal("a failed send did not ask for a resend")
	}
	d.mu.Lock()
	d.err = nil
	d.mu.Unlock()
	fm.reconcileInterventions(ctx)
	fm.hitlSends.Wait()
	if runs, _ := d.sent(); !slices.Equal(runs, []string{held.ID}) {
		t.Fatalf("runs sent %v, want %s", runs, held.ID)
	}
	if fm.hitlResend.Load() {
		t.Fatal("the resend flag stayed set")
	}
}

// the deadline timer applies on_timeout; an approve on_timeout is run like
// a human's approval. a timer that fires early reports what is left.
func TestDurableHitlExpireAppliesOnTimeout(t *testing.T) {
	spec := gateSpec()
	spec.OnTimeout = hitl.OnTimeoutAllow
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	d := &fakeDurableHitl{}
	fm.SetDurableHitl(d)
	ctx := context.Background()
	held, _ := gateExec(fm, vmID, "git", "push", "--force")

	left, err := fm.HitlExpire(ctx, held.ID)
	if err != nil || left <= 0 {
		t.Fatalf("early timer: left=%v err=%v, want the rest of the deadline", left, err)
	}
	rec, _ := fm.store.GetIntervention(ctx, held.ID)
	rec.Deadline = time.Now().Add(-time.Second)
	if err := fm.store.UpsertIntervention(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if left, err := fm.HitlExpire(ctx, held.ID); err != nil || left != 0 {
		t.Fatalf("due timer: left=%v err=%v", left, err)
	}
	got, _ := fm.loadIntervention(ctx, held.ID)
	if got.State != hitl.StateExpired || got.Decision == nil || got.Decision.Outcome != hitl.OutcomeApprove {
		t.Fatalf("expired %+v, want on_timeout approve applied", got)
	}
	fm.hitlSends.Wait()
	if runs, _ := d.sent(); !slices.Equal(runs, []string{held.ID}) {
		t.Fatalf("runs sent %v, want the timed-out approval", runs)
	}
	if left, err := fm.HitlExpire(ctx, "hitl_missing"); err != nil || left != 0 {
		t.Fatalf("missing intervention: left=%v err=%v", left, err)
	}
}
