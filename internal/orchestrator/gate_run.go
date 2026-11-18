package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// lifecycleRunTimeout bounds an approved lifecycle action. a live migrate
// is the slowest of them.
const lifecycleRunTimeout = 30 * time.Minute

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
	if in.Computer != nil {
		return fm.runApprovedComputer(ctx, in)
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

	if m := fm.hitlMetrics(); m != nil && in.Decision != nil {
		m.HitlRunFinished(runAction(in), res.FinishedAt.Sub(in.Decision.DecidedAt), res.Error != "")
	}
	fm.appendEvent(ctx, "intervention", id, "intervention.ran", map[string]any{
		"env_id": in.EnvID, "error": res.Error != "",
	})
	fm.publishInterventionEvent(in)
	return nil
}

// runApprovedComputer performs an approved computer action: the body the
// agent sent, with the text a human edited. it skips the agent lock like
// an approved exec does; the approval is the permission.
func (fm *FleetManager) runApprovedComputer(ctx context.Context, in hitl.Intervention) hitl.Result {
	body, err := in.ComputerInput()
	if err != nil {
		return hitl.Result{Error: err.Error()}
	}
	fm.noteAgentCall(in.EnvID)
	res, err := fm.guestComputer(ctx, in.EnvID, http.MethodPost, "/v1/computer/action", body)
	if err != nil {
		return hitl.Result{Error: err.Error()}
	}
	if res.Status < 200 || res.Status > 299 {
		return hitl.Result{Error: fmt.Sprintf("guest answered %d: %s", res.Status, guestStreamError(res.Body))}
	}
	var out hitl.ComputerResult
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return hitl.Result{Error: "malformed guest answer: " + err.Error()}
	}
	return hitl.Result{Computer: &out}
}
