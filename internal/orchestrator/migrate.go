package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (fm *FleetManager) discardProvisionedVM(vmID, hostID string, spec Spec) {
	fm.mu.Lock()
	delete(fm.vms, vmID)
	if hostID != "" {
		fm.deallocateOnHost(hostID, spec)
	}
	fm.mu.Unlock()
	fm.unpublishIngress(context.Background(), vmID)
}

func (fm *FleetManager) abandonProvisionedVM(ctx context.Context, provider Provider, hostID, vmID string, spec Spec) {
	if err := provider.Destroy(ctx, vmID); err != nil {
		fm.logger.Warn("destroy partially provisioned vm failed", "vm", vmID, "err", err)
	}
	fm.discardProvisionedVM(vmID, hostID, spec)
}

// ErrLiveMigrateRefused is returned when a live migrate cannot be done as
// asked: no distinct target host, or the target host agent declined to resume
// the guest (different cpu or firecracker build, or the guest's network slot is
// taken there). the source vm is untouched and still running, which is what
// makes this a conflict to report rather than a failure to clean up after.
var ErrLiveMigrateRefused = errors.New("live migrate refused")

// MigrateOptions tunes a MigrateVM call. all fields are optional.
type MigrateOptions struct {
	// TargetHostID is the host to migrate to. empty means the orchestrator
	// picks a host other than the source's, preferring the vm's checkpoint
	// standby. a vm with no host (single-provider mode) stays where it is.
	TargetHostID string

	// Live carries the guest's memory across and resumes it on the target, so
	// processes survive the move, instead of cold-booting a disk snapshot.
	//
	// it is opt-in and never falls back to a cold migration. the seed it takes
	// is a live snapshot, whose rootfs is not bootable without its memory, so
	// there is nothing to fall back onto; and a caller who asked for processes
	// to survive should hear that they would not, while the source is still
	// running, rather than find out afterwards. it needs a target other than
	// the source's host, the same cpu and firecracker build on both, and the
	// source's network slot free on the target (the frozen guest keeps its
	// addresses and nothing remaps them yet).
	Live bool

	// Lazy resumes the guest on the target before its memory has arrived and
	// pages it in from the source afterwards, so the move does not wait on a
	// full copy of the memory image. it needs Live and a vm booted with
	// HugePages. until the target reports every page resident the guest still
	// depends on the source's seed snapshot, which stays pinned until then;
	// if the source host is lost first, the guest is lost with it.
	Lazy bool
}

// pickMigrateTarget chooses a host other than the source for a migrate that
// named none. the vm's checkpoint standby comes first when it still qualifies,
// since it already holds the vm's base and a live migrate there skips the bulk
// copy. it never falls back to the source: a caller moving a vm off a failing
// host must not get a same-host migrate instead.
func (fm *FleetManager) pickMigrateTarget(vmID, srcHostID string, srcBackend HostBackend, srcSpec Spec) (string, error) {
	fm.mu.RLock()
	hosts := fm.activeHostsLocked()
	fm.mu.RUnlock()

	// a rootfs prepared under one backend is not portable to another.
	eligible := make([]*Host, 0, len(hosts))
	for _, h := range hosts {
		if h.ID != srcHostID && h.Backend == srcBackend {
			eligible = append(eligible, h)
		}
	}

	spec := srcSpec
	spec.HostID = ""

	c := fm.chainFor(vmID)
	c.mu.Lock()
	standby := c.standby
	c.mu.Unlock()
	if h := hostByID(eligible, standby); h != nil && hostRejection(h, spec) == "" {
		return standby, nil
	}

	h, _, err := SchedulePreferring(spec, eligible, fm.placementPolicy, PlacementHints{})
	if err != nil {
		return "", fmt.Errorf("no host other than %s can take vm %s: %w", srcHostID, vmID, err)
	}
	return h.ID, nil
}

// MigrateVM moves vmID to another host and returns the new vm's id.
func (fm *FleetManager) MigrateVM(ctx context.Context, vmID string, opts MigrateOptions) (string, error) {
	op := "migrate"
	if opts.Live {
		op = "migrate_live"
	}
	ctx, end := fm.startOperation(ctx, op, vmID)
	id, err := fm.migrateVM(ctx, vmID, opts)
	end(err)
	return id, err
}

func (fm *FleetManager) migrateVM(ctx context.Context, vmID string, opts MigrateOptions) (string, error) {
	if d := fm.durableMigrator(); d != nil {
		return fm.durableMigrateVM(ctx, d, vmID, opts)
	}
	plan, err := fm.MigrateBegin(ctx, vmID, MigrateRequest(opts))
	if err != nil {
		return "", err
	}
	delta := plan.Delta
	var chain *checkpointChain
	if delta {
		// held for the whole migrate so the checkpoint loop cannot take a live
		// snapshot of this vm in between, which would break the diff's parent.
		// on restate the migrate runs on the vm's Environment object instead,
		// which the checkpoint loop's ticks queue behind.
		chain = fm.chainFor(vmID)
		chain.mu.Lock()
		defer chain.mu.Unlock()
	}
	var frozenAt time.Time
	frozen, committed := false, false
	defer func() {
		if frozen && !committed {
			// a failed migrate must never leave the source stopped: until the
			// target is running it is the only copy of the guest.
			_ = fm.MigrateResumeSource(context.Background(), plan)
			_ = fm.MigrateFrozen(context.Background(), plan, time.Since(frozenAt), "rolled_back")
		}
	}()

	var seed SnapshotRecord
	var stagedBase string
	if delta && chain.standby == plan.TargetHostID && chain.base != "" {
		// the checkpoint loop already keeps a merged copy of this vm on the
		// target, so the bulk copy happened before this call did.
		seed, stagedBase = chain.head, chain.base
	} else {
		// a live seed is the bulk of what moves, so the guest gives back what
		// it is not using first.
		reportStep(ctx, "snapshot")
		seed, err = fm.snapshotInProcess(ctx, vmID, SnapshotOptions{Comment: "migration seed", Live: opts.Live, ShrinkMemory: opts.Live})
		if err != nil {
			return "", err
		}
		if opts.Live && seed.Kind != SnapshotKindLive {
			// an agent too old for live snapshots answers with a disk one.
			return "", fmt.Errorf("%w: host %s took a %s snapshot for a live migrate of vm %s", ErrSnapshotUnsupported, plan.SourceHostID, seed.Kind, vmID)
		}
	}

	v, err := fm.addMigratePlaceholder(plan)
	if err != nil {
		return "", err
	}
	discard := func() {
		fm.mu.RLock()
		spec := v.spec
		fm.mu.RUnlock()
		fm.discardProvisionedVM(plan.NewVMID, plan.TargetHostID, spec)
	}

	localID := stagedBase
	if !plan.SameHost() {
		if localID == "" {
			reportStep(ctx, "transfer")
			localID, err = fm.ensureArtifactOnHost(ctx, seed, plan.TargetHostID, opts.Lazy)
		}
		if err == nil && delta {
			reportStep(ctx, "delta")
			// the host pauses the guest before it writes the diff, so the
			// pause is timed from the request.
			diffAt := time.Now()
			var child SnapshotRecord
			child, err = fm.snapshotInProcess(ctx, vmID, SnapshotOptions{
				Comment: "migration delta", Live: true, Diff: true,
				Parent: seed.SnapshotID, KeepPaused: true, NoLineage: true,
			})
			if err == nil {
				frozen, frozenAt = true, diffAt
				localID, err = fm.moveDeltaToHost(ctx, child, localID, plan.TargetHostID, true)
			}
			// the chain's base on the target was consumed by the merge, or is
			// out of step with a source that just took another live snapshot.
			fm.resetChainLocked(chain)
		}
	}
	if err == nil {
		reportStep(ctx, "boot")
		err = fm.MigrateCreate(ctx, plan, seed, localID)
	}
	if err != nil {
		discard()
		return "", err
	}

	// published under ports of its own first. the source's ports are adopted
	// at the very end (MigrateCutover), once nothing can fail any more.
	reportStep(ctx, "cutover")
	var started MigrateStarted
	started.Ingress, err = fm.MigrateIngress(ctx, plan)
	if err == nil {
		started.Credentials, err = fm.MigrateCredentials(ctx, plan)
	}
	if err == nil {
		err = fm.MigrateUpload(ctx, plan, started)
	}
	if err == nil {
		err = fm.MigrateStartAgent(ctx, plan, started)
	}
	if err == nil {
		err = fm.MigratePersist(ctx, plan, started)
	}
	if err != nil {
		fm.abandonMigrateTarget(ctx, plan, v)
		return "", err
	}
	if frozen {
		_ = fm.MigrateFrozen(ctx, plan, time.Since(frozenAt), "cutover")
	}
	_ = fm.MigrateTask(ctx, plan)
	_ = fm.MigrateCutover(ctx, plan, started)
	_ = fm.MigrateLineage(ctx, plan, seed, delta)

	if opts.Lazy {
		// the seed on the source is now the only copy of every page the new
		// vm has not touched yet, so it is held until the target has them all.
		// taken before the source vm goes, and released by the watcher.
		release := fm.holdArtifact(ctx, LazyMemoryHolder(plan.NewVMID), LazyMemoryPinTTL, seed.SnapshotID)
		go fm.watchLazyMemory(plan.NewVMID, seed.SnapshotID, release)
	}

	// from here the target is the guest; the source is only torn down.
	committed = true
	_ = fm.MigrateHandover(ctx, plan)
	if !frozen {
		// drain runs a command inside the guest, which a frozen guest cannot
		// do. nothing needs flushing either: its state is already on the target.
		_ = fm.MigrateDrainSource(ctx, plan)
	}
	if destroyErr := fm.DestroyVM(ctx, vmID); destroyErr != nil {
		fm.logger.Warn("destroy source vm during migration failed", "vm", vmID, "err", destroyErr)
	}
	if chain != nil {
		fm.dropChain(vmID, chain)
	}

	return plan.NewVMID, nil
}

// abandonMigrateTarget destroys a new vm a failed in-process migrate created.
func (fm *FleetManager) abandonMigrateTarget(ctx context.Context, plan MigratePlan, v *vm) {
	fm.mu.RLock()
	spec := v.spec
	fm.mu.RUnlock()
	fm.abandonProvisionedVM(ctx, fm.migrateProvider(plan.TargetHostID), plan.TargetHostID, plan.NewVMID, spec)
}

// LazyMemoryWatchTimeout matches hostwire.LazyArtifactGrantTTL: past it the
// target's grant is dead and no further chunk can arrive.
const LazyMemoryWatchTimeout = 30 * time.Minute

// LazyMemoryPinTTL is how long a durable lazy-memory pin lasts when no
// watcher releases it: a little past the grant.
const LazyMemoryPinTTL = LazyMemoryWatchTimeout + 5*time.Minute

// lazyMemoryPollInterval is how often the target is asked how far along it is.
var lazyMemoryPollInterval = 2 * time.Second

// watchLazyMemory polls a lazily resumed vm until its memory is all on the
// target, then releases the seed it was paging from. the outcome is an event
// either way (see MigrateMemorySettled). release drops the pin on the seed.
// on restate the watcher is Migrate/{migration_id}.WatchMemory instead.
func (fm *FleetManager) watchLazyMemory(vmID, seedID string, release func()) {
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), LazyMemoryWatchTimeout)
	defer cancel()
	t := time.NewTicker(lazyMemoryPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = fm.MigrateMemorySettled(context.Background(), vmID, seedID, MemoryPoll{}, true)
			return
		case <-t.C:
		}
		poll, _ := fm.MigrateMemoryPoll(ctx, vmID)
		if poll.Settled() {
			_ = fm.MigrateMemorySettled(context.Background(), vmID, seedID, poll, false)
			return
		}
	}
}
