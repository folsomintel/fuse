package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// maxHitlSends bounds the one-way hitl sends in flight, so a restate that
// accepts connections and never answers holds at most this many.
const maxHitlSends = 8

// errRunInterrupted is the result an approved action gets when a previous
// attempt at it began and never reported back: it may or may not have run,
// and running it again could do it twice.
const errRunInterrupted = "the orchestrator restarted while the approved action was running; it may or may not have run"

// DurableHitl runs approved hitl actions and intervention deadlines on the
// restate Intervention/{id} object. *durable.Client satisfies it. both only
// enqueue.
type DurableHitl interface {
	// SendHitlRun enqueues Intervention/{id}.Run, keyed on id, so sending it
	// again while one is in flight or done starts nothing new.
	SendHitlRun(ctx context.Context, id string) error
	// SendHitlExpire enqueues Intervention/{id}.Expire to run after delay.
	SendHitlExpire(ctx context.Context, id string, delay time.Duration) error
}

// HitlMetrics is an optional extension of ReconcileMetrics for gated
// actions.
type HitlMetrics interface {
	// HitlDecided is a gated action decided by a human or its deadline,
	// d after the gate held it.
	HitlDecided(kind, outcome string, d time.Duration)
	// HitlDeciderAsked is one call to a gate's decider. verdict is the
	// decider's answer, or "error".
	HitlDeciderAsked(verdict string, d time.Duration)
	// HitlRunFinished is an approved action's result, d after the approval.
	// action is exec, computer or the lifecycle action.
	HitlRunFinished(action string, d time.Duration, failed bool)
}

// SetDurableHitl routes approved hitl actions and intervention deadlines
// through restate. set it before Start.
func (fm *FleetManager) SetDurableHitl(d DurableHitl) {
	fm.mu.Lock()
	fm.durableHitlRuns = d
	if fm.hitlSendSlots == nil {
		fm.hitlSendSlots = make(chan struct{}, maxHitlSends)
	}
	fm.mu.Unlock()
}

func (fm *FleetManager) durableHitl() DurableHitl {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableHitlRuns
}

// needsRun reports whether in is an approved gated action with no result yet.
func needsRun(in hitl.Intervention) bool {
	return in.Runs() && in.Decision != nil && in.Decision.Outcome == hitl.OutcomeApprove && in.Result == nil
}

// hitlSend runs send off the caller's goroutine, at most maxHitlSends at a
// time.
func (fm *FleetManager) hitlSend(what, id string, send func(context.Context) error) {
	fm.hitlSends.Add(1)
	go func() {
		defer fm.hitlSends.Done()
		fm.hitlSendSlots <- struct{}{}
		defer func() { <-fm.hitlSendSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := send(ctx); err != nil {
			fm.logger.Error("send durable hitl "+what+" failed", "intervention", id, "err", err)
			if what == "run" {
				// the next reconcile sends every approved action still
				// owed a result again.
				fm.hitlResend.Store(true)
			}
		}
	}()
}

// sendHitlRun hands an approved action to restate. an intervention whose
// send is already queued or in flight is not queued again, so a restate
// outage holds at most one waiting send per approval owed a result.
func (fm *FleetManager) sendHitlRun(d DurableHitl, id string) {
	fm.hitlMu.Lock()
	if fm.hitlQueued[id] {
		fm.hitlMu.Unlock()
		return
	}
	if fm.hitlQueued == nil {
		fm.hitlQueued = make(map[string]bool)
	}
	fm.hitlQueued[id] = true
	fm.hitlMu.Unlock()
	fm.hitlSend("run", id, func(ctx context.Context) error {
		defer func() {
			fm.hitlMu.Lock()
			delete(fm.hitlQueued, id)
			fm.hitlMu.Unlock()
		}()
		return d.SendHitlRun(ctx, id)
	})
}

// scheduleExpiry arms the durable timer that applies a pending
// intervention's on_timeout at its deadline. a delayed send, not a sleeping
// invocation, so a day-long deadline pins nothing to a deployment. a send
// that fails is covered by the reconcile expiry sweep.
func (fm *FleetManager) scheduleExpiry(in hitl.Intervention) {
	d := fm.durableHitl()
	if in.State != hitl.StatePending {
		return
	}
	// the delay is taken when the send goes out, so time spent waiting for
	// a slot does not push the timer past the deadline.
	fm.hitlSend("expire", in.ID, func(ctx context.Context) error {
		return d.SendHitlExpire(ctx, in.ID, max(time.Until(in.Deadline), 0))
	})
}

// resumeApprovedRuns sends every approved action still owed a result to
// restate again: one whose send was lost, or whose approval landed just
// before a crash. the send is keyed on the intervention, so one already in
// flight is not started twice. restate resumes the run, so the result never
// has to say it may or may not have happened.
func (fm *FleetManager) resumeApprovedRuns(ctx context.Context, d DurableHitl) {
	fm.hitlResend.Store(false)
	for _, state := range []hitl.State{hitl.StateApproved, hitl.StateExpired} {
		recs, err := fm.store.ListInterventions(ctx, InterventionFilter{State: string(state)})
		if err != nil {
			fm.logger.Warn("list approved interventions failed", "err", err)
			fm.hitlResend.Store(true)
			return
		}
		for _, rec := range recs {
			if rec.Outcome != string(hitl.OutcomeApprove) {
				continue
			}
			in, err := fm.interventionFromRecord(rec)
			if err != nil || !needsRun(in) {
				continue
			}
			fm.sendHitlRun(d, in.ID)
		}
	}
}

// HitlRunRoute is how the durable run carries out an approved action.
type HitlRunRoute string

const (
	// HitlRouteNone: nothing is left to run.
	HitlRouteNone HitlRunRoute = ""
	// HitlRouteInProcess runs the action in this process, in one step:
	// exec, computer, drain, fork and migrate.
	HitlRouteInProcess HitlRunRoute = "in_process"
	// the rest are durable already, so the run calls their handler on the
	// environment's object instead of running their steps itself.
	HitlRouteDestroy     HitlRunRoute = "destroy"
	HitlRouteSnapshot    HitlRunRoute = "snapshot"
	HitlRouteRotateToken HitlRunRoute = "rotate_token"
)

// HitlRunPlan is what HitlRunBegin settled. restate journals it, so it holds
// no argv, text or output: only the route, the environment and, for a
// snapshot, its options.
//
// a route other than in process is also the lifecycle operation it runs,
// and StartedAt when the run began, so HitlRunFinish can record it with the
// other operations.
type HitlRunPlan struct {
	Route     HitlRunRoute     `json:"route"`
	EnvID     string           `json:"env_id,omitempty"`
	Snapshot  *SnapshotOptions `json:"snapshot,omitempty"`
	StartedAt time.Time        `json:"started_at,omitempty"`
}

// the methods below are the steps of the durable Intervention/{id}.Run and
// Expire handlers. each runs inside a journaled restate step and must be
// safe to run again.

// HitlRunBegin reads the approved action and picks its route. an action
// with a result already, or one not approved, has nothing left to run. a
// destroy of an environment the fleet no longer tracks, or snapshot params
// that do not decode, get their error result here.
func (fm *FleetManager) HitlRunBegin(ctx context.Context, id string) (HitlRunPlan, error) {
	in, err := fm.loadIntervention(ctx, id)
	if err != nil {
		return HitlRunPlan{}, err
	}
	if !needsRun(in) {
		return HitlRunPlan{}, nil
	}
	if in.Lifecycle == nil {
		return HitlRunPlan{Route: HitlRouteInProcess, EnvID: in.EnvID}, nil
	}
	plan := HitlRunPlan{EnvID: in.EnvID, StartedAt: time.Now()}
	switch in.Lifecycle.Action {
	case hitl.ActionDestroy:
		if _, ok := fm.GetVM(in.EnvID); !ok {
			return HitlRunPlan{}, fm.storeRunResult(ctx, id, hitl.Result{Error: fmt.Sprintf("%v: %s", ErrVMNotFound, in.EnvID), FinishedAt: time.Now()})
		}
		plan.Route = HitlRouteDestroy
	case hitl.ActionSnapshot:
		var opts SnapshotOptions
		if len(in.Lifecycle.Params) > 0 {
			if err := json.Unmarshal(in.Lifecycle.Params, &opts); err != nil {
				return HitlRunPlan{}, fm.storeRunResult(ctx, id, hitl.Result{Error: err.Error(), FinishedAt: time.Now()})
			}
		}
		plan.Route, plan.Snapshot = HitlRouteSnapshot, &opts
	case hitl.ActionRotateToken:
		plan.Route = HitlRouteRotateToken
	default:
		plan.Route = HitlRouteInProcess
	}
	return plan, nil
}

// HitlRunInProcess runs an approved exec, computer, drain, fork or migrate
// and writes the result to the intervention. the result never leaves this
// step, so exec output and screenshots stay out of the journal.
//
// the step marks the row before it starts. a retried exec within
// execReplayWindow of the mark carries the step's idempotency key to the
// host agent, which hands back the first run's output instead of running it
// again. the others have no such key, so they run at most once: an attempt
// that finds the mark settles the action as possibly run rather than doing
// it a second time, and so does an exec retried after the window.
func (fm *FleetManager) HitlRunInProcess(ctx context.Context, id string) error {
	in, err := fm.loadIntervention(ctx, id)
	if err != nil {
		return err
	}
	if !needsRun(in) {
		return nil
	}
	if res, ok := fm.takeUnstored(id); ok {
		return fm.storeDurableResult(ctx, id, res)
	}
	started, err := fm.markRunStarted(ctx, id)
	if err != nil {
		return err
	}
	// an exec retried well inside the agent's key ttl is replayed by the
	// agent. past that the agent may have forgotten the key and would run
	// the command again, so it is settled like the others.
	if !started.IsZero() && (in.Exec == nil || time.Since(started) > execReplayWindow) {
		return fm.storeDurableResult(ctx, id, hitl.Result{Error: errRunInterrupted, FinishedAt: time.Now()})
	}
	actx, cancel := context.WithTimeout(ctx, runTimeout(in))
	res := fm.runApproved(actx, in)
	cancel()
	res.FinishedAt = time.Now()
	return fm.storeDurableResult(ctx, id, res)
}

// HitlRunFinish writes the result of an action the run handed to its
// environment's handler, and records it as the lifecycle operation it was,
// as DestroyVM, CreateSnapshot and RotateToken do. a result already there is
// kept and not recorded again.
func (fm *FleetManager) HitlRunFinish(ctx context.Context, id string, plan HitlRunPlan, res hitl.Result) error {
	in, err := fm.loadIntervention(ctx, id)
	if err != nil {
		return err
	}
	if !needsRun(in) {
		return nil
	}
	res.FinishedAt = time.Now()
	if err := fm.storeRunResult(ctx, id, res); err != nil {
		return err
	}
	if om, ok := fm.metrics.(OperationMetrics); ok && !plan.StartedAt.IsZero() {
		var opErr error
		if res.Error != "" {
			opErr = errors.New(res.Error)
		}
		om.OperationCompleted(string(plan.Route), res.FinishedAt.Sub(plan.StartedAt), opErr)
	}
	return nil
}

// HitlExpire applies an intervention's on_timeout once its deadline has
// passed, through the same path a reader takes. it returns how long is
// left when the timer fired early by this process's clock; zero otherwise.
// an approval the expiry makes is handed to restate like a human's.
func (fm *FleetManager) HitlExpire(ctx context.Context, id string) (time.Duration, error) {
	in, err := fm.GetIntervention(ctx, id)
	if errors.Is(err, ErrInterventionNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if in.State == hitl.StatePending {
		return max(time.Until(in.Deadline), time.Millisecond), nil
	}
	return 0, nil
}

// loadIntervention reads one intervention as stored, with no expiry applied.
func (fm *FleetManager) loadIntervention(ctx context.Context, id string) (hitl.Intervention, error) {
	rec, err := fm.store.GetIntervention(ctx, id)
	if err != nil {
		return hitl.Intervention{}, err
	}
	return fm.interventionFromRecord(rec)
}

// storeDurableResult stores res, and keeps it in memory when the store
// fails, so the step's retry in this process stores what ran instead of
// finding its own start mark.
func (fm *FleetManager) storeDurableResult(ctx context.Context, id string, res hitl.Result) error {
	if err := fm.storeRunResult(ctx, id, res); err != nil {
		fm.hitlMu.Lock()
		if fm.hitlUnstored == nil {
			fm.hitlUnstored = make(map[string]hitl.Result)
		}
		fm.hitlUnstored[id] = res
		fm.hitlMu.Unlock()
		return err
	}
	return nil
}

func (fm *FleetManager) takeUnstored(id string) (hitl.Result, bool) {
	fm.hitlMu.Lock()
	defer fm.hitlMu.Unlock()
	res, ok := fm.hitlUnstored[id]
	delete(fm.hitlUnstored, id)
	return res, ok
}

// execReplayWindow is how long after its first attempt an approved exec is
// retried on the host agent's idempotency key. well inside the agents' 24h
// default key ttl, which an operator can shorten.
const execReplayWindow = time.Hour

// markRunStarted records on the row that an attempt at the approved action
// is about to begin, and returns when an earlier one began, zero when none
// had. the mark lives in the sealed result column next to the answer, so it
// is not part of the intervention anyone reads; the result replaces it.
func (fm *FleetManager) markRunStarted(ctx context.Context, id string) (time.Time, error) {
	fm.hitlMu.Lock()
	defer fm.hitlMu.Unlock()
	rec, err := fm.store.GetIntervention(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	raw, err := fm.unseal(rec.Result, rec.Sealed)
	if err != nil {
		return time.Time{}, fmt.Errorf("intervention %s: result: %w", id, err)
	}
	var r interventionResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return time.Time{}, fmt.Errorf("intervention %s: result: %w", id, err)
		}
	}
	if r.Started != nil {
		return *r.Started, nil
	}
	now := time.Now().UTC()
	r.Started = &now
	if raw, err = json.Marshal(r); err != nil {
		return time.Time{}, err
	}
	if rec.Result, _, err = fm.seal(raw); err != nil {
		return time.Time{}, fmt.Errorf("seal intervention result: %w", err)
	}
	return time.Time{}, fm.store.UpsertIntervention(ctx, rec)
}

// runTimeout bounds one approved action.
func runTimeout(in hitl.Intervention) time.Duration {
	switch {
	case in.Exec != nil:
		return execTimeout(in.Exec.TimeoutMS) + time.Minute
	case in.Computer != nil:
		return guestComputerTimeout + time.Minute
	}
	return lifecycleRunTimeout
}

// HitlRunTimeout is the longest an approved action runs, the durable run's
// abort timeout.
const HitlRunTimeout = lifecycleRunTimeout

// runAction names an approved action for metrics.
func runAction(in hitl.Intervention) string {
	switch {
	case in.Exec != nil:
		return "exec"
	case in.Computer != nil:
		return "computer"
	case in.Lifecycle != nil:
		return string(in.Lifecycle.Action)
	}
	return "unknown"
}

func (fm *FleetManager) hitlMetrics() HitlMetrics {
	m, _ := fm.metrics.(HitlMetrics)
	return m
}
