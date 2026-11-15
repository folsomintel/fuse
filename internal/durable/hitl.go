package durable

import (
	"context"
	"errors"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opHitlRun and opHitlExpire are the op labels on the Intervention object's
// step spans and metrics.
const (
	opHitlRun    = "hitl_run"
	opHitlExpire = "hitl_expire"
)

// hitlRunAbortTimeout is how long restate lets one attempt at an approved
// action run without journal progress before it aborts and retries it: the
// longest action, a live migrate, plus a margin.
const hitlRunAbortTimeout = orchestrator.HitlRunTimeout + 5*time.Minute

// Hitl is the part of the fleet the Intervention object drives, one method
// per step. *FleetManager satisfies it.
type Hitl interface {
	HitlRunBegin(ctx context.Context, id string) (orchestrator.HitlRunPlan, error)
	HitlRunInProcess(ctx context.Context, id string) error
	HitlRunFinish(ctx context.Context, id string, plan orchestrator.HitlRunPlan, res hitl.Result) error
	HitlExpire(ctx context.Context, id string) (time.Duration, error)
}

// hitlPermanent makes an intervention that no longer exists terminal: no
// retry brings it back.
func hitlPermanent(err error) error {
	if errors.Is(err, orchestrator.ErrInterventionNotFound) {
		return restate.ToTerminalError(err)
	}
	return err
}

// unbounded retries a step until it succeeds, under policy's backoff. the
// run's steps write the intervention row, and a state store that is down
// holds the run there until it is back rather than leaving it unsettled.
func unbounded(policy RetryPolicy) []restate.RunOption {
	return []restate.RunOption{
		restate.WithInitialRetryInterval(policy.InitialInterval),
		restate.WithMaxRetryInterval(policy.MaxInterval),
	}
}

// hitlRun is Intervention/{id}.Run: the approved action, once. the
// decision itself lives on the intervention row, where DecideIntervention
// or the deadline writes it; the orchestrator sends Run once it is an
// approval, keyed on the id, so there is no invocation waiting on a human.
//
// begin picks the route. an exec, computer, drain, fork or migrate runs in
// one in-process step that writes its own result. destroy, snapshot and
// token rotation are durable handlers on Environment/{env}, so the run calls
// them there and writes what they returned; their steps are not run again
// here.
func hitlRun(ctx restate.ObjectContext, fleet Hitl, policy RetryPolicy) error {
	id := restate.Key(ctx)
	retry := unbounded(policy)
	plan, err := Run(ctx, opHitlRun, "begin", func(c context.Context) (orchestrator.HitlRunPlan, error) {
		plan, err := fleet.HitlRunBegin(c, id)
		return plan, hitlPermanent(err)
	}, retry...)
	if err != nil {
		return err
	}

	var res hitl.Result
	switch plan.Route {
	case orchestrator.HitlRouteNone:
		return nil
	case orchestrator.HitlRouteInProcess:
		return runStep(ctx, opHitlRun, "run", func(c context.Context) error {
			return hitlPermanent(fleet.HitlRunInProcess(c, id))
		}, retry...)
	case orchestrator.HitlRouteDestroy:
		_, err = restate.Object[restate.Void](ctx, EnvironmentObject, plan.EnvID, "Destroy").
			Request(orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonManual})
	case orchestrator.HitlRouteSnapshot:
		var rec orchestrator.SnapshotRecord
		rec, err = restate.Object[orchestrator.SnapshotRecord](ctx, EnvironmentObject, plan.EnvID, "Snapshot").
			Request(*plan.Snapshot)
		res.SnapshotID = rec.SnapshotID
	case orchestrator.HitlRouteRotateToken:
		_, err = restate.Object[restate.Void](ctx, EnvironmentObject, plan.EnvID, "RotateToken").
			Request(restate.Void{})
	default:
		err = restate.TerminalErrorf("unknown hitl run route %q", plan.Route)
	}
	if err != nil {
		if !restate.IsTerminalError(err) {
			// suspension or cancellation: let restate handle it.
			return err
		}
		res = hitl.Result{Error: err.Error()}
	}
	return runStep(ctx, opHitlRun, "finish", func(c context.Context) error {
		return hitlPermanent(fleet.HitlRunFinish(c, id, plan, res))
	}, retry...)
}

// hitlExpire is Intervention/{id}.Expire, the durable timer of a pending
// intervention's deadline: the orchestrator sends it with the deadline as
// its delay when the intervention is raised. it applies on_timeout, and an
// approval that makes is run like a human's. a timer that fires early by
// the orchestrator's clock sends itself again for the rest; one that finds
// the intervention already decided does nothing.
func hitlExpire(ctx restate.ObjectContext, fleet Hitl, policy RetryPolicy) error {
	id := restate.Key(ctx)
	left, err := Run(ctx, opHitlExpire, "expire", func(c context.Context) (time.Duration, error) {
		return fleet.HitlExpire(c, id)
	}, unbounded(policy)...)
	if err != nil {
		return err
	}
	if left > 0 {
		restate.ObjectSend(ctx, InterventionObject, id, "Expire").Send(restate.Void{}, restate.WithDelay(left))
	}
	return nil
}

// SendHitlRun enqueues Intervention/{id}.Run. the idempotency key is the id,
// so a second send while one is in flight, or within the retention window
// after it finished, attaches to it instead of starting another; one after
// that finds the result on the row and does nothing.
func (c *Client) SendHitlRun(ctx context.Context, id string) error {
	_, err := ingress.ObjectSend[restate.Void](c.ingress, InterventionObject, id, "Run").
		Send(ctx, restate.Void{}, restate.WithIdempotencyKey("hitl-run:"+id))
	return err
}

// SendHitlExpire enqueues Intervention/{id}.Expire to run after delay.
func (c *Client) SendHitlExpire(ctx context.Context, id string, delay time.Duration) error {
	_, err := ingress.ObjectSend[restate.Void](c.ingress, InterventionObject, id, "Expire").
		Send(ctx, restate.Void{}, restate.WithDelay(delay))
	return err
}

var _ orchestrator.DurableHitl = (*Client)(nil)
