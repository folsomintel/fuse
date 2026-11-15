package durable

import (
	"context"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opBuild is the op label on a build's step spans and metrics.
const opBuild = "build"

// buildAbortTimeout is how long restate lets one build attempt run without
// journal progress: a setup exec runs up to 600s, a builder boot and a
// snapshot can take minutes.
const buildAbortTimeout = 30 * time.Minute

// Builds is the part of the fleet the build workflow drives, one method per
// step. *FleetManager satisfies it.
type Builds interface {
	BuildResolveCache(ctx context.Context, in orchestrator.BuildInput) (orchestrator.BuildPlan, error)
	BuildCreateBuilder(ctx context.Context, in orchestrator.BuildInput, plan orchestrator.BuildPlan) (string, error)
	BuildExec(ctx context.Context, in orchestrator.BuildInput, vmID string, ex orchestrator.BuildExec) error
	BuildSnapshotLayer(ctx context.Context, in orchestrator.BuildInput, vmID string, layer int) (string, error)
	BuildSnapshot(ctx context.Context, in orchestrator.BuildInput, vmID string) (string, error)
	BuildDestroyBuilder(ctx context.Context, in orchestrator.BuildInput) error
	BuildFinish(ctx context.Context, in orchestrator.BuildInput) error
	BuildFailed(ctx context.Context, in orchestrator.BuildInput, cause string) error
}

// build is Build/{build_id}.Run: resolve the layer cache, create the builder,
// run each setup exec and snapshot each cacheable layer, take the artifact,
// destroy the builder. every step is journaled, so a crash resumes at the
// first unfinished one: a layer that was running runs again on the same
// builder. a step that fails for good destroys the builder before the build
// is recorded failed, so no failure leaves one holding host capacity.
func build(ctx restate.WorkflowContext, fleet Builds, policy RetryPolicy, in orchestrator.BuildInput) error {
	retry := retryUnder(policy)
	plan, err := buildStep(ctx, in, orchestrator.BuildStepResolve, func(c context.Context) (orchestrator.BuildPlan, error) {
		return fleet.BuildResolveCache(c, in)
	}, retry)
	var vmID string
	if err == nil {
		vmID, err = buildStep(ctx, in, orchestrator.BuildStepCreate, func(c context.Context) (string, error) {
			return fleet.BuildCreateBuilder(c, in, plan)
		}, retry)
	}
	for _, ex := range plan.Execs {
		if err != nil {
			break
		}
		_, err = buildStep(ctx, in, ex.Step, func(c context.Context) (restate.Void, error) {
			return restate.Void{}, fleet.BuildExec(c, in, vmID, ex)
		}, retry)
		if err == nil && ex.Layer >= 0 {
			_, err = buildStep(ctx, in, "snapshot_"+ex.Step, func(c context.Context) (string, error) {
				return fleet.BuildSnapshotLayer(c, in, vmID, ex.Layer)
			}, retry)
		}
	}
	if err == nil {
		_, err = buildStep(ctx, in, orchestrator.BuildStepSnapshot, func(c context.Context) (string, error) {
			return fleet.BuildSnapshot(c, in, vmID)
		}, retry)
	}
	if err != nil && !restate.IsTerminalError(err) {
		// suspension or cancellation: restate resumes the build here.
		return err
	}

	// the builder goes on both paths. a destroy that runs out of retries is
	// already dead-lettered by the destroy handler, so it does not change
	// what the build produced.
	if _, derr := buildStep(ctx, in, orchestrator.BuildStepDestroy, func(c context.Context) (restate.Void, error) {
		return restate.Void{}, fleet.BuildDestroyBuilder(c, in)
	}, retry); derr != nil && !restate.IsTerminalError(derr) {
		return derr
	}

	if err != nil {
		cause := err.Error()
		if _, ferr := buildStep(ctx, in, "failed", func(c context.Context) (restate.Void, error) {
			return restate.Void{}, fleet.BuildFailed(c, in, cause)
		}, retry); ferr != nil && !restate.IsTerminalError(ferr) {
			return ferr
		}
		return err
	}
	_, err = buildStep(ctx, in, "finish", func(c context.Context) (restate.Void, error) {
		return restate.Void{}, fleet.BuildFinish(c, in)
	}, retry)
	return err
}

// buildStep is Run for one build step. its ctx carries an idempotency key
// unique to the build and step, so a snapshot or destroy the step hands to
// restate attaches to the invocation an earlier attempt started instead of
// starting a second. a failure a retry cannot change is terminal at once.
func buildStep[T any](ctx restate.Context, in orchestrator.BuildInput, step string, fn func(context.Context) (T, error), retry []restate.RunOption) (T, error) {
	return Run(ctx, opBuild, step, func(c context.Context) (T, error) {
		c = orchestrator.WithIdempotencyKey(c, "build:"+in.ID+":"+step)
		v, err := fn(c)
		if orchestrator.PermanentBuildError(err) {
			return v, restate.ToTerminalError(err)
		}
		return v, err
	}, retry...)
}

// SubmitBuild starts Build/{in.ID}.Run without waiting for it. restate runs a
// workflow id once, so a second submit attaches to the first.
func (c *Client) SubmitBuild(ctx context.Context, in orchestrator.BuildInput) error {
	_, err := ingress.WorkflowSend[orchestrator.BuildInput](c.ingress, BuildWorkflow, in.ID, "Run").
		Send(ctx, in)
	return err
}

var _ orchestrator.DurableBuilds = (*Client)(nil)
