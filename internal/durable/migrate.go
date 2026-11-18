package durable

import (
	"context"
	"errors"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// op labels on a migrate's step spans and metrics. the seed and the delta
// are snapshots taken through the snapshot steps, under ops of their own so
// their host-agent calls are keyed apart.
const (
	opMigrate      = "migrate"
	opMigrateLive  = "migrate_live"
	opMigrateSeed  = "migrate_seed"
	opMigrateDelta = "migrate_delta"
	opMigrateLazy  = "migrate_lazy"
)

// migrateRefusedCode is the error code a migrate's own refusal without a
// sentinel (a vm not running, a cross-backend target) crosses restate as, so
// the client hands the api the fleet's message unchanged. migrateErrorBase+i
// carries orchestrator.MigrateRefusals[i]. clear of the 480s, which other
// handlers number from, and of create's 529-539.
const (
	migrateRefusedCode restate.Code = 540
	migrateErrorBase   restate.Code = 541
)

// the lazy-memory watcher's polls start at memoryPollFirst and back off to
// memoryPollMax, so a 30 minute watch journals tens of polls, not hundreds.
// vars so tests can shorten them.
var (
	memoryPollFirst = 2 * time.Second
	memoryPollMax   = 30 * time.Second
)

// Migrator is the part of the fleet the migrate handler drives, one method
// per step. *FleetManager satisfies it.
type Migrator interface {
	MigrateBegin(ctx context.Context, vmID string, req orchestrator.MigrateRequest) (orchestrator.MigratePlan, error)
	MigratePlaceholder(ctx context.Context, plan orchestrator.MigratePlan) error
	MigrateTransferPlan(ctx context.Context, plan orchestrator.MigratePlan, seed orchestrator.SnapshotRecord) (orchestrator.ChainMove, error)
	MigrateDeltaPlan(ctx context.Context, plan orchestrator.MigratePlan, child orchestrator.SnapshotRecord, base string) (orchestrator.ArtifactMoveJob, error)
	MigrateCreate(ctx context.Context, plan orchestrator.MigratePlan, seed orchestrator.SnapshotRecord, localID string) error
	MigrateIngress(ctx context.Context, plan orchestrator.MigratePlan) ([]byte, error)
	MigrateCredentials(ctx context.Context, plan orchestrator.MigratePlan) (orchestrator.RotatedCredentials, error)
	MigrateUpload(ctx context.Context, plan orchestrator.MigratePlan, started orchestrator.MigrateStarted) error
	MigrateStartAgent(ctx context.Context, plan orchestrator.MigratePlan, started orchestrator.MigrateStarted) error
	MigratePersist(ctx context.Context, plan orchestrator.MigratePlan, started orchestrator.MigrateStarted) error
	MigrateTask(ctx context.Context, plan orchestrator.MigratePlan) error
	MigrateCutover(ctx context.Context, plan orchestrator.MigratePlan, started orchestrator.MigrateStarted) error
	MigrateLineage(ctx context.Context, plan orchestrator.MigratePlan, seed orchestrator.SnapshotRecord, delta bool) error
	MigrateHandover(ctx context.Context, plan orchestrator.MigratePlan) error
	MigrateDrainSource(ctx context.Context, plan orchestrator.MigratePlan) error
	MigrateResumeSource(ctx context.Context, plan orchestrator.MigratePlan) error
	MigrateFrozen(ctx context.Context, plan orchestrator.MigratePlan, d time.Duration, outcome string) error
	MigrateDeleteSeed(ctx context.Context, seed orchestrator.SnapshotRecord) error
	MigrateMemoryPoll(ctx context.Context, vmID string) (orchestrator.MemoryPoll, error)
	MigrateMemorySettled(ctx context.Context, vmID, seedID string, poll orchestrator.MemoryPoll, timedOut bool) error
}

// migrateFleet is what the migrate handler drives: its own steps, the
// snapshot steps for the seed and the delta, and the destroy of the source
// and the checkpoint chain that goes with it.
type migrateFleet interface {
	Migrator
	Snapshots
	destroyFleet
}

// migratePermanent marks a step's err terminal when a retry cannot change it:
// the snapshot sentinels and agent 4xx the way snapshot steps mark them, and
// the migrate's own refusals under codes of their own.
func migratePermanent(err error) error {
	if err == nil {
		return nil
	}
	if i := sentinelIndex(err, orchestrator.MigrateRefusals); i >= 0 {
		return restate.ToTerminalError(err, restate.WithErrorCode(migrateErrorBase+restate.Code(i)))
	}
	return permanent(err)
}

// migrateRefusal marks every error of a step that only reads the fleet
// terminal: what it refuses, a retry refuses.
func migrateRefusal(err error) error {
	if err == nil {
		return nil
	}
	if perr := migratePermanent(err); restate.IsTerminalError(perr) {
		return perr
	}
	return restate.ToTerminalError(err, restate.WithErrorCode(migrateRefusedCode))
}

// migrateError turns a migrate's failure back into the fleet error its code
// names, so the api answers it with that error's own status and message.
func migrateError(err error) error {
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		return err
	}
	code := ierr.Code()
	if code == migrateRefusedCode {
		return errors.New(ierr.Message())
	}
	if i := int(code) - int(migrateErrorBase); i >= 0 && i < len(orchestrator.MigrateRefusals) {
		return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.MigrateRefusals[i]}
	}
	if i := int(code) - int(fleetErrorBase); i >= 0 && i < len(orchestrator.SnapshotFleetErrors) {
		return &sentinelError{msg: ierr.Message(), sentinel: orchestrator.SnapshotFleetErrors[i]}
	}
	return err
}

// migration is one run of the migrate handler, and what its compensation has
// to undo when it fails for good.
type migration struct {
	ctx    restate.ObjectContext
	fleet  migrateFleet
	policy RetryPolicy
	op     string
	plan   orchestrator.MigratePlan

	// placed: the target placeholder may exist, and is destroyed on failure.
	placed bool
	// frozenAt: the diff snapshot may have paused the source, unix ms. 0 when
	// it never ran.
	frozenAt int64
	// seed is the snapshot the migrate took and deletes on failure. empty
	// for a seed the checkpoint chain owns.
	seed orchestrator.SnapshotRecord
}

func (m *migration) step(name string, fn func(context.Context) error) error {
	return runStep(m.ctx, m.op, name, fn, retryUnder(m.policy)...)
}

// migrate is Environment/{vm_id}.Migrate. running on the source's object
// serializes it with the checkpoint loop and the vm's other lifecycle ops,
// and the checkpoint chain it may seed from is this object's state. each step
// is journaled, so after a crash it resumes at the first unfinished one, and
// a failure that a retry cannot fix runs the compensation: destroy the
// target, resume a frozen source, delete the seed. from the target's persist
// on the target is the guest: nothing is undone after it, and the source is
// destroyed through the destroy steps. it returns the new vm's id.
func migrate(ctx restate.ObjectContext, fleet migrateFleet, policy RetryPolicy, req orchestrator.MigrateRequest) (string, error) {
	vmID := restate.Key(ctx)
	op := opMigrate
	if req.Live {
		op = opMigrateLive
	}
	plan, err := Run(ctx, op, "plan", func(c context.Context) (orchestrator.MigratePlan, error) {
		plan, err := fleet.MigrateBegin(c, vmID, req)
		return plan, migrateRefusal(err)
	})
	if err != nil {
		return "", err
	}
	m := &migration{ctx: ctx, fleet: fleet, policy: policy, op: op, plan: plan}
	seed, started, delta, err := m.run()
	if err != nil {
		if !restate.IsTerminalError(err) {
			// suspension: restate resumes the migrate here.
			return "", err
		}
		if cerr := m.compensate(); cerr != nil {
			return "", cerr
		}
		return "", err
	}
	return plan.NewVMID, m.finish(seed, started, delta)
}

// run carries the migrate up to the target's persist, the point after which
// the target is the guest. it returns the seed, what the start steps left
// for the cutover, and whether it moved a delta.
func (m *migration) run() (seed orchestrator.SnapshotRecord, started orchestrator.MigrateStarted, delta bool, err error) {
	ctx, fleet, plan := m.ctx, m.fleet, m.plan
	vmID := plan.SourceVMID

	// no tick runs while this handler holds the object, so the chain read
	// here stays the chain until the migrate changes it.
	st, err := loadChain(ctx)
	if err != nil {
		return seed, started, false, err
	}
	delta = plan.Delta && st.Pending == nil && !st.Dropped
	staged := delta && st.Chain.Standby == plan.TargetHostID && st.Chain.Base != ""

	m.placed = true
	if err := m.step("placeholder", func(c context.Context) error {
		return migratePermanent(fleet.MigratePlaceholder(c, plan))
	}); err != nil {
		return seed, started, delta, err
	}

	localID := ""
	if staged {
		// the checkpoint loop already keeps a merged copy of this vm on the
		// target, so the bulk copy happened before this migrate did.
		seed, localID = st.Chain.Head, st.Chain.Base
	} else {
		// a live seed is the bulk of what moves, so the guest gives back what
		// it is not using first.
		seed, err = snapshotSteps(ctx, fleet, m.policy, opMigrateSeed, vmID, orchestrator.SnapshotOptions{
			Comment: "migration seed", Live: plan.Live, ShrinkMemory: plan.Live,
		})
		if err != nil {
			return seed, started, delta, err
		}
		m.seed = seed
		if plan.Live && seed.Kind != orchestrator.SnapshotKindLive {
			// an agent too old for live snapshots answers with a disk one.
			return seed, started, delta, permanent(fmt.Errorf("%w: host %s took a %s snapshot for a live migrate of vm %s",
				orchestrator.ErrSnapshotUnsupported, plan.SourceHostID, seed.Kind, vmID))
		}
	}

	if !plan.SameHost() {
		if localID == "" {
			mv, err := Run(ctx, m.op, "plan_transfer", func(c context.Context) (orchestrator.ChainMove, error) {
				mv, err := fleet.MigrateTransferPlan(c, plan, seed)
				return mv, artifactPermanent(err)
			}, retryUnder(m.policy)...)
			if err != nil {
				return seed, started, delta, err
			}
			localID = mv.Existing
			if localID == "" {
				if localID, err = migrateMove(ctx, mv.Job); err != nil {
					return seed, started, delta, err
				}
			}
		}
		if delta {
			if localID, err = m.moveDelta(st, seed, localID); err != nil {
				return seed, started, delta, err
			}
		}
	}

	if err := m.step("create", func(c context.Context) error {
		return migratePermanent(fleet.MigrateCreate(c, plan, seed, localID))
	}); err != nil {
		return seed, started, delta, err
	}
	// published under ports of its own first. the source's ports are adopted
	// at the cutover, once nothing can fail any more.
	if started.Ingress, err = Run(ctx, m.op, "ingress", func(c context.Context) ([]byte, error) {
		return fleet.MigrateIngress(c, plan)
	}, retryUnder(m.policy)...); err != nil {
		return seed, started, delta, err
	}
	// made once and journaled sealed, so a retried upload sends the same
	// files under the same keys.
	if started.Credentials, err = Run(ctx, m.op, "credentials", func(c context.Context) (orchestrator.RotatedCredentials, error) {
		creds, err := fleet.MigrateCredentials(c, plan)
		return creds, migrateRefusal(err)
	}); err != nil {
		return seed, started, delta, err
	}
	if err := m.step("upload", func(c context.Context) error {
		return migratePermanent(fleet.MigrateUpload(c, plan, started))
	}); err != nil {
		return seed, started, delta, err
	}
	if err := m.step("start_agent", func(c context.Context) error {
		return migratePermanent(fleet.MigrateStartAgent(c, plan, started))
	}); err != nil {
		return seed, started, delta, err
	}
	err = m.step("persist", func(c context.Context) error {
		return fleet.MigratePersist(c, plan, started)
	})
	return seed, started, delta, err
}

// artifactPermanent marks a move planner's err terminal when a retry cannot
// change it.
func artifactPermanent(err error) error {
	if err != nil && orchestrator.PermanentArtifactError(err) {
		return restate.ToTerminalError(err)
	}
	return err
}

// moveDelta pauses the source for a diff against the seed and merges it onto
// the seed's copy on the target, whose new id it returns. the resume of the
// source is registered before the diff is taken. the chain's base on the
// target is consumed by the merge, or out of step with a source that took
// another live snapshot, so the chain is forgotten either way, deleting
// nothing.
func (m *migration) moveDelta(st chainObject, seed orchestrator.SnapshotRecord, base string) (string, error) {
	ctx, fleet, plan := m.ctx, m.fleet, m.plan
	frozenAt, err := nowMillis(ctx)
	if err != nil {
		return "", err
	}
	m.frozenAt = frozenAt
	child, err := snapshotSteps(ctx, fleet, m.policy, opMigrateDelta, plan.SourceVMID, orchestrator.SnapshotOptions{
		Comment: "migration delta", Live: true, Diff: true,
		Parent: seed.SnapshotID, KeepPaused: true, NoLineage: true,
	})
	localID := ""
	if err == nil {
		var job orchestrator.ArtifactMoveJob
		job, err = Run(ctx, m.op, "plan_delta", func(c context.Context) (orchestrator.ArtifactMoveJob, error) {
			job, err := fleet.MigrateDeltaPlan(c, plan, child, base)
			return job, artifactPermanent(err)
		}, retryUnder(m.policy)...)
		if err == nil {
			localID, err = migrateMove(ctx, job)
		}
	}
	if err != nil && !restate.IsTerminalError(err) {
		return "", err
	}
	if !st.Chain.Empty() {
		if st.Chain.Base != "" {
			restate.ObjectSend(ctx, ArtifactPinObject, st.Chain.Base, "Release").Send(chainHolder(plan.SourceVMID))
		}
		st.Chain = orchestrator.ChainState{}
		saveChain(ctx, fleet, st)
	}
	return localID, err
}

// frozen records how long the diff snapshot kept the source paused, once.
func (m *migration) frozen(outcome string) error {
	if m.frozenAt == 0 {
		return nil
	}
	now, err := nowMillis(m.ctx)
	if err != nil {
		return err
	}
	d := time.Duration(now-m.frozenAt) * time.Millisecond
	m.frozenAt = 0
	return m.step("frozen", func(c context.Context) error {
		return m.fleet.MigrateFrozen(c, m.plan, d, outcome)
	})
}

// compensate undoes a migrate that failed for good, target first so the
// guest never runs twice: the target is destroyed through its own
// Environment object's destroy, the source is resumed if the diff may have
// paused it, and a seed the migrate took is deleted. the resume retries
// without an attempt bound: until it succeeds the source is the only copy of
// the guest, and it is stopped.
func (m *migration) compensate() error {
	ctx, fleet, plan := m.ctx, m.fleet, m.plan
	if m.placed {
		_, err := restate.Object[restate.Void](ctx, EnvironmentObject, plan.NewVMID, "Destroy").
			Request(orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonMigrateRollback, HostID: plan.TargetHostID})
		if err != nil && !restate.IsTerminalError(err) {
			return err
		}
		// a destroy that ran out of retries left the target destroy_failed
		// with a dead letter, for an operator.
	}
	if m.frozenAt != 0 {
		if err := runStep(ctx, m.op, "resume_source", func(c context.Context) error {
			return fleet.MigrateResumeSource(c, plan)
		}, restate.WithInitialRetryInterval(m.policy.InitialInterval),
			restate.WithMaxRetryInterval(m.policy.MaxInterval)); err != nil {
			return err
		}
		if err := m.frozen("rolled_back"); err != nil && !restate.IsTerminalError(err) {
			return err
		}
	}
	if m.seed.SnapshotID != "" {
		if err := m.step("delete_seed", func(c context.Context) error {
			return fleet.MigrateDeleteSeed(c, m.seed)
		}); err != nil && !restate.IsTerminalError(err) {
			return err
		}
	}
	return nil
}

// finish runs once the target is the guest: the frozen time, the task, the
// cutover, the lineage, the handover of what belongs to the logical
// environment, the lazy-memory watcher, and the drain and destroy of the
// source. no step here fails for good, and nothing undoes the migrate; a
// source destroy that runs out of retries is left destroy_failed with a dead
// letter, as any destroy is.
func (m *migration) finish(seed orchestrator.SnapshotRecord, started orchestrator.MigrateStarted, delta bool) error {
	ctx, fleet, plan := m.ctx, m.fleet, m.plan
	if err := m.frozen("cutover"); err != nil {
		return err
	}
	if err := m.step("task", func(c context.Context) error {
		return fleet.MigrateTask(c, plan)
	}); err != nil {
		return err
	}
	// the cutover: from here a client of the source's url reaches the new vm.
	if err := m.step("cutover", func(c context.Context) error {
		return fleet.MigrateCutover(c, plan, started)
	}); err != nil {
		return err
	}
	if err := m.step("lineage", func(c context.Context) error {
		return fleet.MigrateLineage(c, plan, seed, delta)
	}); err != nil {
		return err
	}
	// a pending decision belongs to the logical environment, which now lives
	// at the new id. moved before the source is destroyed, which would
	// otherwise cancel it.
	if err := m.step("handover", func(c context.Context) error {
		return fleet.MigrateHandover(c, plan)
	}); err != nil {
		return err
	}
	if plan.Lazy {
		// the seed on the source is now the only copy of every page the new
		// vm has not touched yet, so it is pinned until the target has them
		// all, taken before the source goes. the pin lapses once no chunk
		// could still arrive, in case the watcher is killed.
		if _, err := restate.Object[restate.Void](ctx, ArtifactPinObject, seed.SnapshotID, "Pin").
			Request(PinRequest{Holder: orchestrator.LazyMemoryHolder(plan.NewVMID), TTL: orchestrator.LazyMemoryPinTTL}); err != nil {
			return err
		}
		restate.ObjectSend(ctx, MigrateObject, plan.MigrationID, "WatchMemory").
			Send(orchestrator.MemoryWatch{VMID: plan.NewVMID, SeedID: seed.SnapshotID})
	}
	if !delta {
		// drain runs a command inside the guest, which a frozen guest cannot
		// do. nothing needs flushing either: its state is already on the
		// target.
		if err := m.step("drain_source", func(c context.Context) error {
			return fleet.MigrateDrainSource(c, plan)
		}); err != nil {
			return err
		}
	}
	// this handler holds the source's object, so the destroy runs here, as
	// its steps, rather than as a call to Destroy on the same object.
	err := destroy(ctx, fleet, m.policy, orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonMigrated})
	if err != nil && !restate.IsTerminalError(err) {
		return err
	}
	return nil
}

// migrateMove runs Artifacts/{job.LocalID}.Move and waits for it. a move that
// fails for good comes back terminal, as a transfer failure.
func migrateMove(ctx restate.ObjectContext, job orchestrator.ArtifactMoveJob) (string, error) {
	id, err := restate.Object[string](ctx, ArtifactsObject, job.LocalID, "Move").Request(job)
	if err != nil && restate.IsTerminalError(err) {
		return "", restate.ToTerminalError(fmt.Errorf("%w: copy %s from host %s to host %s: %w",
			orchestrator.ErrArtifactTransferFailed, job.SnapshotID, job.SourceHostID, job.HostID, err))
	}
	return id, err
}

// watchMemory is Migrate/{migration_id}.WatchMemory: it polls a lazily
// resumed vm until its memory is all on the target, the transfer fails, or
// orchestrator.LazyMemoryWatchTimeout passes, then records the outcome and
// releases the seed's pin. the waits are durable sleeps, so a restart
// resumes the watch where it was.
func watchMemory(ctx restate.ObjectContext, fleet Migrator, w orchestrator.MemoryWatch) error {
	start, err := nowMillis(ctx)
	if err != nil {
		return err
	}
	deadline := start + orchestrator.LazyMemoryWatchTimeout.Milliseconds()
	wait := memoryPollFirst
	for {
		if err := restate.Sleep(ctx, wait); err != nil {
			return err
		}
		poll, err := Run(ctx, opMigrateLazy, "memory_status", func(c context.Context) (orchestrator.MemoryPoll, error) {
			return fleet.MigrateMemoryPoll(c, w.VMID)
		})
		if err != nil {
			return err
		}
		now, err := nowMillis(ctx)
		if err != nil {
			return err
		}
		settled := poll.Settled()
		if settled || now >= deadline {
			if err := runStep(ctx, opMigrateLazy, "memory_settled", func(c context.Context) error {
				return fleet.MigrateMemorySettled(c, w.VMID, w.SeedID, poll, !settled)
			}); err != nil {
				return err
			}
			restate.ObjectSend(ctx, ArtifactPinObject, w.SeedID, "Release").Send(orchestrator.LazyMemoryHolder(w.VMID))
			return nil
		}
		wait = min(wait*2, memoryPollMax)
	}
}

// Migrate runs Environment/{vmID}.Migrate and waits for it, returning the
// new vm's id. a migrate moves whole memory images, so it goes through the
// client with no request timeout of its own; ctx bounds the wait, and the
// migrate keeps running if ctx ends first.
func (c *Client) Migrate(ctx context.Context, vmID string, req orchestrator.MigrateRequest) (string, error) {
	id, err := ingress.Object[orchestrator.MigrateRequest, string](c.longIngress, EnvironmentObject, vmID, "Migrate").
		Request(ctx, req, callerKey(ctx)...)
	return id, migrateError(err)
}

var _ orchestrator.DurableMigrator = (*Client)(nil)
