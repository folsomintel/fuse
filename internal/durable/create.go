package durable

import (
	"context"
	"errors"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opCreate is the op label on create's step spans and metrics.
const opCreate = "create"

// createErrorBase is the first error code a create sentinel crosses restate
// as: orchestrator.CreateFleetErrors[i] is createErrorBase+i. it sits clear of
// the snapshot sentinels (480-486) and the rotation refusal (487).
const createErrorBase restate.Code = 530

// createFailedCode is the code of any other create failure that a retry
// cannot change, so the client can tell the create's own failure apart from
// restate's.
const createFailedCode restate.Code = 529

// Creator is the part of the fleet the create handler drives, one method per
// step and per compensation step. *FleetManager satisfies it.
type Creator interface {
	CreateBegin(ctx context.Context, vmID string, req orchestrator.CreateRequest) (orchestrator.CreateState, error)
	CreatePlace(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) (string, error)
	CreateArtifact(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) (string, error)
	CreatePublishIngress(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) (*orchestrator.CreateIngress, error)
	CreateRestoreOrCreate(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) (bool, error)
	CreateProvisionEgress(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) error
	CreateUpload(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) error
	CreateStartupScript(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) error
	CreateStartAgent(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) ([]orchestrator.Endpoint, error)
	CreateRunning(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState) error

	CreateReleaseEgress(ctx context.Context, vmID string) error
	CreateUnpublishIngress(ctx context.Context, vmID string) error
	CreateAbandon(ctx context.Context, vmID string, req orchestrator.CreateRequest, cause string) error
	CreateBeginFailed(ctx context.Context, vmID string, req orchestrator.CreateRequest, cause string) error
}

// createKey is the Environment object state that marks a create in flight,
// so reconcile can tell a provisioning vm restate will finish from one left
// by a create that ran before restate was on.
const createKey = "create"

// createMarked is create with its in-flight mark: set before the first step,
// cleared once the create has ended either way.
func createMarked(ctx restate.ObjectContext, fleet Creator, policy RetryPolicy, req orchestrator.CreateRequest) error {
	restate.Set(ctx, createKey, true)
	err := create(ctx, fleet, policy, req)
	if err == nil || restate.IsTerminalError(err) {
		restate.Clear(ctx, createKey)
	}
	return err
}

// createInFlight is Environment/{vm_id}.CreateInFlight.
func createInFlight(ctx restate.ObjectSharedContext) (bool, error) {
	marked, err := restate.Get[bool](ctx, createKey)
	return marked, err
}

// createPermanent marks err terminal when a retry cannot change it, carrying
// a create sentinel as its code. the code is journaled with the error, so a
// replay that returns the recorded failure still carries it.
func createPermanent(err error) error {
	if err == nil || !orchestrator.PermanentCreateError(err) {
		return err
	}
	if i := sentinelIndex(err, orchestrator.CreateFleetErrors); i >= 0 {
		return restate.ToTerminalError(err, restate.WithErrorCode(createErrorBase+restate.Code(i)))
	}
	return restate.ToTerminalError(err, restate.WithErrorCode(createFailedCode))
}

// create is Environment/{vm_id}.Create: begin, place, artifact, ingress,
// boot (restore or create), egress, upload, startup, agent, running. every
// step is journaled, so after a crash the handler resumes at the first
// unfinished one and the vm is finished, not promoted to running without its
// files and agent. a step that fails for good rolls the create back: egress
// released, ingress unpublished, the vm and its task failed, and, once the vm
// may exist on its host, a destroy sent to this object, which restate runs
// after this invocation ends.
func create(ctx restate.ObjectContext, fleet Creator, policy RetryPolicy, req orchestrator.CreateRequest) error {
	vmID := restate.Key(ctx)
	retry := retryUnder(policy)
	// begin fails for good only on a refusal it makes before writing
	// anything (a task already assigned, inputs that do not decrypt, a bad
	// hitl spec), so there is nothing to compensate for then. its writes are
	// retried without an attempt bound: a begin that gave up after an earlier
	// attempt's row landed would leave the vm provisioning with no create
	// left to finish it.
	st, err := Run(ctx, opCreate, "begin", func(c context.Context) (orchestrator.CreateState, error) {
		st, err := fleet.CreateBegin(c, vmID, req)
		return st, createPermanent(err)
	}, restate.WithInitialRetryInterval(policy.InitialInterval),
		restate.WithMaxRetryInterval(policy.MaxInterval))
	if err != nil {
		if restate.IsTerminalError(err) {
			msg := err.Error()
			if cerr := createStep(ctx, "begin_failed", []restate.RunOption{
				restate.WithInitialRetryInterval(policy.InitialInterval),
				restate.WithMaxRetryInterval(policy.MaxInterval),
			}, func(c context.Context) error {
				return fleet.CreateBeginFailed(c, vmID, req, msg)
			}); cerr != nil {
				return cerr
			}
		}
		return err
	}
	err = createSteps(ctx, fleet, retry, vmID, req, &st)
	if err == nil {
		// create reached running. this is the single point every durable
		// create passes on success: anything to schedule off a running vm
		// (a ttl's delayed destroy) is sent from here.
		return nil
	}
	if !restate.IsTerminalError(err) {
		// suspension or cancellation: restate resumes the create here.
		return err
	}
	return compensateCreate(ctx, fleet, policy, vmID, req, st, err)
}

// createSteps runs the create's steps after begin, filling st as they return.
func createSteps(ctx restate.ObjectContext, fleet Creator, retry []restate.RunOption, vmID string, req orchestrator.CreateRequest, st *orchestrator.CreateState) error {
	hostID, err := Run(ctx, opCreate, "place", func(c context.Context) (string, error) {
		hostID, err := fleet.CreatePlace(c, vmID, req, *st)
		return hostID, createPermanent(err)
	}, retry...)
	if err != nil {
		return err
	}
	st.HostID = hostID

	seedID, err := Run(ctx, opCreate, "artifact", func(c context.Context) (string, error) {
		seedID, err := fleet.CreateArtifact(c, vmID, req, *st)
		return seedID, createPermanent(err)
	}, retry...)
	if err != nil {
		return err
	}
	st.SeedSnapshotID = seedID

	published, err := Run(ctx, opCreate, "ingress", func(c context.Context) (*orchestrator.CreateIngress, error) {
		published, err := fleet.CreatePublishIngress(c, vmID, req, *st)
		return published, createPermanent(err)
	}, retry...)
	if err != nil {
		return err
	}
	st.Ingress = published

	// from here on a vm may exist on the host.
	st.Booted = true
	fromCache, err := Run(ctx, opCreate, "boot", func(c context.Context) (bool, error) {
		fromCache, err := fleet.CreateRestoreOrCreate(c, vmID, req, *st)
		return fromCache, createPermanent(err)
	}, retry...)
	if err != nil {
		return err
	}
	st.FromCache = fromCache

	for _, s := range []struct {
		name string
		fn   func(context.Context, string, orchestrator.CreateRequest, orchestrator.CreateState) error
	}{
		{"egress", fleet.CreateProvisionEgress},
		{"upload", fleet.CreateUpload},
		{"startup", fleet.CreateStartupScript},
	} {
		if err := createStep(ctx, s.name, retry, func(c context.Context) error {
			return s.fn(c, vmID, req, *st)
		}); err != nil {
			return err
		}
	}

	endpoints, err := Run(ctx, opCreate, "agent", func(c context.Context) ([]orchestrator.Endpoint, error) {
		endpoints, err := fleet.CreateStartAgent(c, vmID, req, *st)
		return endpoints, createPermanent(err)
	}, retry...)
	if err != nil {
		return err
	}
	st.Endpoints = endpoints

	return createStep(ctx, "running", retry, func(c context.Context) error {
		return fleet.CreateRunning(c, vmID, req, *st)
	})
}

// compensateCreate rolls back a create that failed for good with cause and
// returns cause. the release and unpublish are best effort; the abandon
// retries without an attempt bound, so a state store that stays down holds
// the rollback until it is back instead of leaving the vm provisioning.
func compensateCreate(ctx restate.ObjectContext, fleet Creator, policy RetryPolicy, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState, cause error) error {
	retry := retryUnder(policy)
	if err := createStep(ctx, "release_egress", retry, func(c context.Context) error {
		return fleet.CreateReleaseEgress(c, vmID)
	}); err != nil && !restate.IsTerminalError(err) {
		return err
	}
	if err := createStep(ctx, "unpublish_ingress", retry, func(c context.Context) error {
		return fleet.CreateUnpublishIngress(c, vmID)
	}); err != nil && !restate.IsTerminalError(err) {
		return err
	}
	msg := cause.Error()
	if err := createStep(ctx, "abandon", []restate.RunOption{
		restate.WithInitialRetryInterval(policy.InitialInterval),
		restate.WithMaxRetryInterval(policy.MaxInterval),
	}, func(c context.Context) error {
		return fleet.CreateAbandon(c, vmID, req, msg)
	}); err != nil {
		return err
	}
	if st.Booted {
		// the vm is no longer tracked, so the destroy is told its host.
		restate.ObjectSend(ctx, EnvironmentObject, vmID, "Destroy").
			Send(orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonCreateCleanup, HostID: st.HostID, Epoch: st.Epoch})
	}
	return cause
}

// createStep is Run for a create step with no result, its failure marked
// terminal when a retry cannot change it.
func createStep(ctx restate.Context, name string, retry []restate.RunOption, fn func(context.Context) error) error {
	return runStep(ctx, opCreate, name, func(c context.Context) error {
		return createPermanent(fn(c))
	}, retry...)
}

// Create runs Environment/{vmID}.Create and waits for it. a create can run a
// startup script and copy an artifact, so it goes through the client with no
// request timeout of its own; ctx bounds the wait, and the create keeps
// running if ctx ends first.
func (c *Client) Create(ctx context.Context, vmID string, req orchestrator.CreateRequest) error {
	ctx, a := watchAnswer(ctx)
	_, err := ingress.Object[orchestrator.CreateRequest, restate.Void](c.longIngress, EnvironmentObject, vmID, "Create").
		Request(ctx, req, callerKey(ctx)...)
	return settled(a, err, createError(err))
}

// createError turns a create's failure back into the fleet sentinel its code
// names, or into its own message, so the api answers it by that sentinel.
func createError(err error) error {
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		return err
	}
	if ierr.Code() == createFailedCode {
		return errors.New(ierr.Message())
	}
	i := int(ierr.Code()) - int(createErrorBase)
	if i < 0 || i >= len(orchestrator.CreateFleetErrors) {
		return err
	}
	return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.CreateFleetErrors[i]}
}

// sentinelError is a fleet failure that crossed restate: its message as the
// fleet wrote it, which already names the sentinel, and the sentinel for
// errors.Is.
type sentinelError struct {
	msg      string
	sentinel error
}

func (e *sentinelError) Error() string { return e.msg }
func (e *sentinelError) Unwrap() error { return e.sentinel }

// CreateInFlight asks Environment/{vmID}.CreateInFlight whether a create of
// vmID is unfinished.
func (c *Client) CreateInFlight(ctx context.Context, vmID string) (bool, error) {
	return ingress.Object[restate.Void, bool](c.ingress, EnvironmentObject, vmID, "CreateInFlight").
		Request(ctx, restate.Void{})
}

var _ orchestrator.DurableCreator = (*Client)(nil)
