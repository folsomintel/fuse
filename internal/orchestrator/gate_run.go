package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// lifecycleRunTimeout bounds an approved lifecycle action. a live migrate
// is the slowest of them.
const lifecycleRunTimeout = 30 * time.Minute

// claimRun marks an approved action as running in this process. it must be
// called with hitlMu held, in the same critical section that stored the
// approval, so the orphan pass can never see an approval nobody claimed. it
// reports false when the action is already claimed.
func (fm *FleetManager) claimRun(in hitl.Intervention) bool {
	if !in.Runs() || in.Decision == nil || in.Decision.Outcome != hitl.OutcomeApprove || in.Result != nil {
		return false
	}
	if fm.hitlRuns == nil {
		fm.hitlRuns = make(map[string]bool)
	}
	if fm.hitlRuns[in.ID] {
		return false
	}
	fm.hitlRuns[in.ID] = true
	return true
}

// launchRun performs a claimed approved action in the background and stores
// what happened on the intervention.
func (fm *FleetManager) launchRun(in hitl.Intervention) {
	go func() {
		defer func() {
			fm.hitlMu.Lock()
			delete(fm.hitlRuns, in.ID)
			fm.hitlMu.Unlock()
		}()
		timeout := lifecycleRunTimeout
		if in.Exec != nil {
			timeout = execTimeout(in.Exec.TimeoutMS) + time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res := fm.runApproved(ctx, in)
		res.FinishedAt = time.Now()
		if err := fm.storeRunResult(context.Background(), in.ID, res); err != nil {
			fm.logger.Error("store approved action result failed", "intervention", in.ID, "err", err)
		}
	}()
}

func execTimeout(ms int64) time.Duration {
	d := time.Duration(ms) * time.Millisecond
	if d <= 0 {
		return DefaultExecTimeout
	}
	return min(d, MaxExecTimeout)
}

// runApproved performs the action exactly as approved: the edited argv when
// there is one, else what the caller sent. it calls the fleet directly, so
// the action is not gated a second time.
func (fm *FleetManager) runApproved(ctx context.Context, in hitl.Intervention) hitl.Result {
	fail := func(err error) hitl.Result { return hitl.Result{Error: err.Error()} }
	if in.Exec != nil {
		r, err := fm.Exec(ctx, in.EnvID, in.Argv(), ExecOptions{Timeout: execTimeout(in.Exec.TimeoutMS)})
		if err != nil {
			return fail(err)
		}
		return hitl.Result{Exec: &hitl.ExecResult{ExitCode: r.ExitCode, Stdout: string(r.Stdout), Stderr: string(r.Stderr)}}
	}
	if in.Lifecycle == nil {
		return fail(fmt.Errorf("intervention %s has no action to run", in.ID))
	}
	params := in.Lifecycle.Params
	decode := func(v any) error {
		if len(params) == 0 {
			return nil
		}
		return json.Unmarshal(params, v)
	}
	switch in.Lifecycle.Action {
	case hitl.ActionDestroy:
		if err := fm.DestroyVM(ctx, in.EnvID); err != nil {
			return fail(err)
		}
	case hitl.ActionDrain:
		if err := fm.Drain(ctx, in.EnvID); err != nil {
			return fail(err)
		}
	case hitl.ActionRotateToken:
		if err := fm.RotateToken(ctx, in.EnvID); err != nil {
			return fail(err)
		}
	case hitl.ActionFork:
		var opts ForkOptions
		if err := decode(&opts); err != nil {
			return fail(err)
		}
		id, err := fm.ForkEnvironment(ctx, in.EnvID, opts)
		if err != nil {
			return fail(err)
		}
		return hitl.Result{EnvID: id}
	case hitl.ActionMigrate:
		var opts MigrateOptions
		if err := decode(&opts); err != nil {
			return fail(err)
		}
		id, err := fm.MigrateVM(ctx, in.EnvID, opts)
		if err != nil {
			return fail(err)
		}
		return hitl.Result{EnvID: id}
	case hitl.ActionSnapshot:
		var opts SnapshotOptions
		if err := decode(&opts); err != nil {
			return fail(err)
		}
		rec, err := fm.CreateSnapshot(ctx, in.EnvID, opts)
		if err != nil {
			return fail(err)
		}
		return hitl.Result{SnapshotID: rec.SnapshotID}
	default:
		return fail(fmt.Errorf("unknown lifecycle action %q", in.Lifecycle.Action))
	}
	return hitl.Result{}
}

// storeRunResult records an approved action's result and wakes waiters.
func (fm *FleetManager) storeRunResult(ctx context.Context, id string, res hitl.Result) error {
	fm.hitlMu.Lock()
	rec, err := fm.store.GetIntervention(ctx, id)
	if err != nil {
		fm.hitlMu.Unlock()
		return err
	}
	in, err := fm.interventionFromRecord(rec)
	if err != nil {
		fm.hitlMu.Unlock()
		return err
	}
	in.Result = &res
	in.UpdatedAt = time.Now()
	out, err := fm.interventionToRecord(in)
	if err == nil {
		err = fm.store.UpsertIntervention(ctx, out)
	}
	if err != nil {
		fm.hitlMu.Unlock()
		return err
	}
	if ch, ok := fm.hitlWaiters[id]; ok {
		close(ch)
		delete(fm.hitlWaiters, id)
	}
	fm.hitlMu.Unlock()

	fm.appendEvent(ctx, "intervention", id, "intervention.ran", map[string]any{
		"env_id": in.EnvID, "error": res.Error != "",
	})
	fm.publishInterventionEvent(in)
	return nil
}

// recoverOrphanRuns settles approved actions whose run died with a previous
// process: nothing in this process is running them, so a waiter would wait
// forever. whether the action happened is unknown, and the result says so.
// every approval this process makes is claimed before it is visible, so an
// unclaimed one is always an orphan; this runs once, on the first reconcile.
func (fm *FleetManager) recoverOrphanRuns(ctx context.Context) {
	now := time.Now()
	for _, state := range []hitl.State{hitl.StateApproved, hitl.StateExpired} {
		recs, err := fm.store.ListInterventions(ctx, InterventionFilter{State: string(state)})
		if err != nil {
			fm.logger.Warn("list approved interventions failed", "err", err)
			return
		}
		for _, rec := range recs {
			if rec.Kind != string(hitl.KindExec) && rec.Kind != string(hitl.KindLifecycle) {
				continue
			}
			if rec.Outcome != string(hitl.OutcomeApprove) || len(rec.Result) > 0 {
				continue
			}
			in, err := fm.interventionFromRecord(rec)
			if err != nil || in.Settled() {
				continue
			}
			fm.hitlMu.Lock()
			running := fm.hitlRuns[in.ID]
			fm.hitlMu.Unlock()
			if running {
				continue
			}
			res := hitl.Result{
				Error:      "the orchestrator restarted before the approved action finished; it may or may not have run",
				FinishedAt: now,
			}
			if err := fm.storeRunResult(ctx, in.ID, res); err != nil {
				fm.logger.Warn("settle orphaned run failed", "intervention", in.ID, "err", err)
			}
		}
	}
}
