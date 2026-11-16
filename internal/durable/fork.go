package durable

import (
	"context"
	"errors"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opFork is the op label on a fork's step spans and metrics.
const opFork = "fork"

// forkRefusedCode is the error code a fork's own refusal without a sentinel
// (a source not running, a provider that cannot fork) crosses restate as, and
// forkFailedCode that of any other failure that rolled the fork back, so the
// client hands the api the fleet's message unchanged. forkErrorBase+i carries
// orchestrator.ForkRefusals[i]. clear of the 480s, create's 529-539 and
// migrate's 540s.
const (
	forkFailedCode  restate.Code = 550
	forkRefusedCode restate.Code = 551
	forkErrorBase   restate.Code = 552
)

// forkSeedPinTTL bounds the pin a fork holds on its seed, in case the fork's
// invocation is killed and never releases it. far longer than any fork.
const forkSeedPinTTL = 24 * time.Hour

// forkSeedHolder is the pin holder a fork's new vm id makes.
func forkSeedHolder(vmID string) string { return "fork:" + vmID }

// Forker is the part of the fleet the fork handler drives, one method per
// step and per compensation step. *FleetManager satisfies it.
type Forker interface {
	ForkBegin(ctx context.Context, vmID string, req orchestrator.ForkRequest) (orchestrator.ForkPlan, error)
	ForkPlaceholder(ctx context.Context, plan orchestrator.ForkPlan) error
	ForkPlace(ctx context.Context, plan orchestrator.ForkPlan, seed orchestrator.SnapshotRecord) (string, error)
	ForkArtifact(ctx context.Context, plan orchestrator.ForkPlan, seed orchestrator.SnapshotRecord, hostID string) (string, error)
	ForkCreate(ctx context.Context, plan orchestrator.ForkPlan, seed orchestrator.SnapshotRecord, hostID, localID string) error
	ForkIngress(ctx context.Context, plan orchestrator.ForkPlan) ([]byte, error)
	ForkCredentials(ctx context.Context, plan orchestrator.ForkPlan) (orchestrator.RotatedCredentials, error)
	ForkUpload(ctx context.Context, plan orchestrator.ForkPlan, started orchestrator.MigrateStarted) error
	ForkStartAgent(ctx context.Context, plan orchestrator.ForkPlan, started orchestrator.MigrateStarted) error
	ForkRunning(ctx context.Context, plan orchestrator.ForkPlan, started orchestrator.MigrateStarted) error
	ForkTask(ctx context.Context, plan orchestrator.ForkPlan) error
	ForkLineage(ctx context.Context, plan orchestrator.ForkPlan, seed orchestrator.SnapshotRecord) error

	ForkUnpublishIngress(ctx context.Context, plan orchestrator.ForkPlan) error
	ForkAbandon(ctx context.Context, plan orchestrator.ForkPlan, cause string) error
	ForkDeleteSeed(ctx context.Context, seed orchestrator.SnapshotRecord) error
}

// forkMarked is fork with the in-flight mark create uses: reconcile asks
// CreateInFlight about a vm recovered provisioning, and a fork's placeholder
// is one until the fork has ended.
func forkMarked(ctx restate.ObjectContext, fleet Forker, policy RetryPolicy, req orchestrator.ForkRequest) error {
	restate.Set(ctx, createKey, true)
	err := fork(ctx, fleet, policy, req)
	if err == nil || restate.IsTerminalError(err) {
		restate.Clear(ctx, createKey)
	}
	return err
}

// forkPermanent marks a step's err terminal when a retry cannot change it:
// the fork's refusals under codes of their own, and the snapshot sentinels
// and agent 4xx the way snapshot steps mark them.
func forkPermanent(err error) error {
	if err == nil {
		return nil
	}
	for i, s := range orchestrator.ForkRefusals {
		if errors.Is(err, s) {
			return restate.ToTerminalError(err, restate.WithErrorCode(forkErrorBase+restate.Code(i)))
		}
	}
	return permanent(err)
}

// forkRefusal marks every error of begin terminal: it only reads, so what it
// refuses, a retry refuses.
func forkRefusal(err error) error {
	if err == nil {
		return nil
	}
	if perr := forkPermanent(err); restate.IsTerminalError(perr) {
		return perr
	}
	return restate.ToTerminalError(err, restate.WithErrorCode(forkRefusedCode))
}

// forking is one run of the fork handler, and what its compensation has to
// undo when it fails for good.
type forking struct {
	ctx    restate.ObjectContext
	fleet  Forker
	policy RetryPolicy
	plan   orchestrator.ForkPlan

	// booted: a vm may exist on hostID, and is destroyed on failure.
	booted bool
	hostID string
	// seed is the snapshot the fork took and deletes on failure. empty for a
	// reused one, which belongs to the caller.
	seed orchestrator.SnapshotRecord
	// pinned is the seed the fork holds against gc until it ends, taken or
	// reused.
	pinned string
}

func (f *forking) step(name string, fn func(context.Context) error) error {
	return runStep(f.ctx, opFork, name, func(c context.Context) error {
		return forkPermanent(fn(c))
	}, retryUnder(f.policy)...)
}

// fork is Environment/{vm_id}.Fork, keyed by the fork's new vm id, so the new
// vm's other ops queue behind it. begin, placeholder, the seed snapshot,
// place, artifact, create, ingress, credentials, upload, start agent,
// running, task, lineage. every step is journaled, so after a crash the
// handler resumes at the first unfinished one. the seed is taken by a call to
// the source's own Environment.Snapshot, which serializes it with the
// source's other ops and only it. a step that fails for good before running
// rolls the fork back: ingress unpublished, the placeholder dropped, the new
// vm destroyed on its host and the seed snapshot the fork took deleted.
func fork(ctx restate.ObjectContext, fleet Forker, policy RetryPolicy, req orchestrator.ForkRequest) error {
	vmID := restate.Key(ctx)
	plan, err := Run(ctx, opFork, "begin", func(c context.Context) (orchestrator.ForkPlan, error) {
		plan, err := fleet.ForkBegin(c, vmID, req)
		return plan, forkRefusal(err)
	})
	if err != nil {
		return err
	}
	f := &forking{ctx: ctx, fleet: fleet, policy: policy, plan: plan}
	seed, err := f.run()
	if err != nil {
		if !restate.IsTerminalError(err) {
			// suspension: restate resumes the fork here.
			return err
		}
		if cerr := f.compensate(err); cerr != nil {
			return cerr
		}
		return forkFailure(err)
	}
	return f.finish(seed)
}

// forkFailure is the terminal err a rolled back fork returns: as it is when
// its code names a fleet sentinel, otherwise under forkFailedCode so the
// client gets the fleet's message rather than restate's.
func forkFailure(err error) error {
	terr := restate.AsTerminalError(err)
	if terr == nil {
		return err
	}
	code := terr.Code()
	if i := int(code) - int(fleetErrorBase); i >= 0 && i < len(orchestrator.SnapshotFleetErrors) {
		return err
	}
	if i := int(code) - int(forkErrorBase); i >= 0 && i < len(orchestrator.ForkRefusals) {
		return err
	}
	return restate.ToTerminalError(errors.New(terr.Message()), restate.WithErrorCode(forkFailedCode))
}

// run carries the fork up to running, the point after which nothing undoes
// it. it returns the seed.
func (f *forking) run() (orchestrator.SnapshotRecord, error) {
	ctx, fleet, plan := f.ctx, f.fleet, f.plan

	if err := f.step("placeholder", func(c context.Context) error {
		return fleet.ForkPlaceholder(c, plan)
	}); err != nil {
		return orchestrator.SnapshotRecord{}, err
	}

	seed := plan.Seed
	if seed.SnapshotID == "" {
		var err error
		seed, err = restate.Object[orchestrator.SnapshotRecord](ctx, EnvironmentObject, plan.SourceVMID, "Snapshot").
			Request(orchestrator.SnapshotOptions{Comment: plan.Comment})
		if err != nil {
			return seed, err
		}
		f.seed = seed
	}
	// until the lineage row lands the seed has no child, so retention gc
	// could collect it while the fork waits between steps.
	if _, err := restate.Object[restate.Void](ctx, ArtifactPinObject, seed.SnapshotID, "Pin").
		Request(PinRequest{Holder: forkSeedHolder(plan.NewVMID), TTL: forkSeedPinTTL}); err != nil {
		return seed, err
	}
	f.pinned = seed.SnapshotID

	hostID, err := Run(ctx, opFork, "place", func(c context.Context) (string, error) {
		hostID, err := fleet.ForkPlace(c, plan, seed)
		return hostID, forkPermanent(err)
	}, retryUnder(f.policy)...)
	if err != nil {
		return seed, err
	}

	localID := ""
	if hostID != plan.SourceHostID {
		if localID, err = Run(ctx, opFork, "artifact", func(c context.Context) (string, error) {
			localID, err := fleet.ForkArtifact(c, plan, seed, hostID)
			return localID, forkPermanent(err)
		}, retryUnder(f.policy)...); err != nil {
			return seed, err
		}
	}

	// from here on a vm may exist on the host.
	f.booted, f.hostID = true, hostID
	if err := f.step("create", func(c context.Context) error {
		return fleet.ForkCreate(c, plan, seed, hostID, localID)
	}); err != nil {
		return seed, err
	}

	var started orchestrator.MigrateStarted
	if started.Ingress, err = Run(ctx, opFork, "ingress", func(c context.Context) ([]byte, error) {
		sealed, err := fleet.ForkIngress(c, plan)
		return sealed, forkPermanent(err)
	}, retryUnder(f.policy)...); err != nil {
		return seed, err
	}
	// made once and journaled sealed, so a retried upload sends the same
	// files under the same keys.
	if started.Credentials, err = Run(ctx, opFork, "credentials", func(c context.Context) (orchestrator.RotatedCredentials, error) {
		creds, err := fleet.ForkCredentials(c, plan)
		return creds, forkPermanent(err)
	}, retryUnder(f.policy)...); err != nil {
		return seed, err
	}
	for _, s := range []struct {
		name string
		fn   func(context.Context, orchestrator.ForkPlan, orchestrator.MigrateStarted) error
	}{
		{"upload", fleet.ForkUpload},
		{"start_agent", fleet.ForkStartAgent},
		{"running", fleet.ForkRunning},
	} {
		if err := f.step(s.name, func(c context.Context) error {
			return s.fn(c, plan, started)
		}); err != nil {
			return seed, err
		}
	}
	return seed, nil
}

// finish runs once the fork is running: its task and its lineage. neither
// fails for good, and nothing undoes the fork.
func (f *forking) finish(seed orchestrator.SnapshotRecord) error {
	if err := f.step("task", func(c context.Context) error {
		return f.fleet.ForkTask(c, f.plan)
	}); err != nil {
		return err
	}
	if err := f.step("lineage", func(c context.Context) error {
		return f.fleet.ForkLineage(c, f.plan, seed)
	}); err != nil {
		return err
	}
	// the lineage row is the seed's child now, which keeps gc off it.
	f.release()
	return nil
}

// release drops the fork's pin on its seed.
func (f *forking) release() {
	if f.pinned != "" {
		restate.ObjectSend(f.ctx, ArtifactPinObject, f.pinned, "Release").Send(forkSeedHolder(f.plan.NewVMID))
	}
}

// compensate rolls back a fork that failed for good with cause. the unpublish
// and the seed deletion are best effort; the abandon retries without an
// attempt bound, so a state store that stays down holds the rollback until it
// is back instead of leaving the placeholder provisioning.
func (f *forking) compensate(cause error) error {
	ctx, fleet, plan := f.ctx, f.fleet, f.plan
	if err := f.step("unpublish_ingress", func(c context.Context) error {
		return fleet.ForkUnpublishIngress(c, plan)
	}); err != nil && !restate.IsTerminalError(err) {
		return err
	}
	msg := cause.Error()
	if err := runStep(ctx, opFork, "abandon", func(c context.Context) error {
		return fleet.ForkAbandon(c, plan, msg)
	}, restate.WithInitialRetryInterval(f.policy.InitialInterval),
		restate.WithMaxRetryInterval(f.policy.MaxInterval)); err != nil {
		return err
	}
	if f.booted {
		// the vm is no longer tracked, so the destroy is told its host. it
		// runs on this object once the fork has ended.
		restate.ObjectSend(ctx, EnvironmentObject, plan.NewVMID, "Destroy").
			Send(orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonForkRollback, HostID: f.hostID})
	}
	f.release()
	if f.seed.SnapshotID != "" {
		if err := f.step("delete_seed", func(c context.Context) error {
			return fleet.ForkDeleteSeed(c, f.seed)
		}); err != nil && !restate.IsTerminalError(err) {
			return err
		}
	}
	return nil
}

// Fork runs Environment/{vmID}.Fork and waits for it. a fork snapshots its
// source and can copy an artifact, so it goes through the client with no
// request timeout of its own; ctx bounds the wait, and the fork keeps running
// if ctx ends first.
func (c *Client) Fork(ctx context.Context, vmID string, req orchestrator.ForkRequest) error {
	_, err := ingress.Object[orchestrator.ForkRequest, restate.Void](c.longIngress, EnvironmentObject, vmID, "Fork").
		Request(ctx, req, callerKey(ctx)...)
	return forkError(err)
}

// forkError turns a fork's failure back into the fleet error its code names,
// so the api answers it with the same status and message as in mode off.
func forkError(err error) error {
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		return err
	}
	code := ierr.Code()
	if code == forkRefusedCode || code == forkFailedCode {
		return errors.New(ierr.Message())
	}
	if i := int(code) - int(forkErrorBase); i >= 0 && i < len(orchestrator.ForkRefusals) {
		return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.ForkRefusals[i]}
	}
	if i := int(code) - int(fleetErrorBase); i >= 0 && i < len(orchestrator.SnapshotFleetErrors) {
		return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.SnapshotFleetErrors[i]}
	}
	return err
}

var _ orchestrator.DurableForker = (*Client)(nil)
