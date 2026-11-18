package orchestrator

import (
	"context"
	"fmt"
)

// placeFork picks the host a fork lands on, and the provider that owns it.
//
// It is a preference ladder, never a gate, and it CANNOT fail. That is the
// whole contract: forking has always charged the fork to the source's host
// without consulting capacity, and turning that into a hard admission check
// would fail forks that work today for a reason the caller never asked about.
// So every rung falls back to the source's host.
//
//  1. the source's host, when it can still take a vm of this shape. this is the
//     cheapest by far: CreateFromCheckpoint copies the rootfs within one
//     filesystem rather than across the network.
//  2. otherwise, if the seed artifact can be copied, whichever host the
//     scheduler picks -- preferring hosts that already hold those bytes, and
//     restricted to the source's backend, since a rootfs prepared for one
//     backend is not a thing another backend can boot.
//  3. otherwise the source's host anyway, exactly as before.
func (fm *FleetManager) placeFork(ctx context.Context, spec Spec, srcHostID string, srcProvider Provider, seed SnapshotRecord) (string, Provider) {
	// single-provider mode: there is no scheduler and no host registry to
	// consult, so there is nothing to choose between.
	if srcHostID == "" {
		return "", srcProvider
	}

	fm.mu.RLock()
	hosts := fm.activeHostsLocked()
	srcBackend := HostBackend("")
	if h, ok := fm.hosts[srcHostID]; ok {
		srcBackend = h.Backend
	}
	fm.mu.RUnlock()

	if src := hostByID(hosts, srcHostID); src != nil && hostRejection(src, spec) == "" {
		return srcHostID, srcProvider
	}

	plan := fm.planSeedPlacement(ctx, Spec{SeedSnapshotID: seed.SnapshotID})
	if !plan.movable {
		fm.logger.Warn("fork stays on a host that cannot fit it: its seed artifact cannot be copied",
			"host", srcHostID, "snapshot", seed.SnapshotID, "reason", plan.whyImmovable())
		return srcHostID, srcProvider
	}

	eligible := make([]*Host, 0, len(hosts))
	for _, h := range hosts {
		// a rootfs prepared under one backend is not portable to another, and
		// the backends' snapshot stores are not interchangeable either, so a
		// fork never crosses that line however much room the other side has.
		if h.Backend == srcBackend {
			eligible = append(eligible, h)
		}
	}
	selected, _, err := SchedulePreferring(spec, eligible, fm.placementPolicy,
		PlacementHints{ArtifactHosts: plan.hostPreference()})
	if err != nil {
		fm.logger.Warn("fork stays on a host that cannot fit it: no other host can take it",
			"host", srcHostID, "err", err)
		return srcHostID, srcProvider
	}

	fm.mu.RLock()
	targetProvider, ok := fm.providerForHost(selected.ID)
	fm.mu.RUnlock()
	if !ok {
		fm.logger.Warn("fork stays on its source host: no provider for the scheduled host",
			"host", srcHostID, "scheduled", selected.ID)
		return srcHostID, srcProvider
	}
	return selected.ID, targetProvider
}

// ForkOptions tunes a ForkEnvironment call. All fields are optional.
type ForkOptions struct {
	// Comment is attached to the seed snapshot when ForkEnvironment
	// creates one (empty ReuseSnapshotID). ignored when reusing an
	// existing snapshot.
	Comment string

	// ReuseSnapshotID selects an existing ready snapshot of the source
	// vm to seed the fork from. empty means snapshot the source first.
	ReuseSnapshotID string
}

// ForkEnvironment creates a brand-new vm seeded from a checkpoint of an
// existing running vm. it obtains a seed snapshot (creating one when
// ReuseSnapshotID is empty), asks a SnapshotForkable provider to build a
// new environment from that checkpoint, registers the new vm as running,
// and records lineage so the fork references its seed snapshot.
//
// lineage is recorded by persisting a SnapshotRecord for the new vm whose
// ParentSnapshotID is the seed snapshot id (state ready). this reuses the
// same store path CreateSnapshot uses (upsertSnapshotRecord) and is
// directly assertable via ListSnapshots / GetSnapshotByID.
//
// the fork is charged to whichever host it lands on, which is the source's host
// unless that host can no longer fit it and the seed artifact can be copied
// elsewhere (see placeFork). it is given its OWN guest credentials either way,
// since it boots a copy of the source's disk and would otherwise answer to the
// source's token.
//
// providers that cannot fork do not implement SnapshotForkable, so this reports
// fork as unsupported for them. the firecracker provider implements it; the
// qemu provider deliberately does not (vfio gpu passthrough cannot be
// checkpointed).
func (fm *FleetManager) ForkEnvironment(ctx context.Context, srcVMID string, opts ForkOptions) (string, error) {
	ctx, end := fm.startOperation(ctx, "fork", srcVMID)
	var (
		id  string
		err error
	)
	id, err = fm.durableForkVM(ctx, fm.durableForker(), srcVMID, opts)
	end(err)
	return id, err
}

// forkSource is what a fork reads off its source vm.
type forkSource struct {
	spec              Spec
	hostID            string
	provider          Provider
	hitlKeys          map[string]string
	hitlKeysEncrypted []byte
}

// checkFork validates that srcVMID can be forked and returns its source and,
// when reuseSnapshotID is set, the seed snapshot. it takes no snapshot and
// writes nothing, so a refused fork leaves no trace.
func (fm *FleetManager) checkFork(ctx context.Context, srcVMID, reuseSnapshotID string) (forkSource, SnapshotRecord, error) {
	// validate the source vm exists and is running, and resolve the
	// provider under the lock (providerForHost requires fm.mu held),
	// mirroring snapshotEnvironment's resolution.
	fm.mu.RLock()
	src, ok := fm.vms[srcVMID]
	if !ok {
		fm.mu.RUnlock()
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: %s", ErrVMNotFound, srcVMID)
	}
	state := src.state
	fs := forkSource{
		spec:              src.spec,
		hostID:            src.hostID,
		provider:          fm.provider,
		hitlKeys:          src.hitlKeys,
		hitlKeysEncrypted: src.hitlKeysEncrypted,
	}
	srcEnv := src.env
	if fs.hostID != "" {
		if hostProvider, ok := fm.providerForHost(fs.hostID); ok {
			fs.provider = hostProvider
		}
	}
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("vm %s in state %s: fork requires running", srcVMID, state)
	}

	// gpu environments cannot be forked: a vfio passthrough device cannot be
	// checkpointed (d4). this is checked up front, before the provider is
	// consulted, because it is a property of the ENVIRONMENT, not of the
	// provider's capabilities. it previously lived inside the not-forkable
	// branch below, which only held while no provider implemented
	// SnapshotForkable; the firecracker provider now does, so keying the
	// guardrail off the type assertion would silently stop protecting gpu vms.
	if fs.spec.GPUs > 0 {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: vm %s has a gpu passthrough device: fork is not supported for gpu environments", ErrGPUUnsupported, srcVMID)
	}

	// a true fork needs a provider that can seed a new environment from a
	// checkpoint. the firecracker provider implements this; the qemu provider
	// deliberately does not (its gpu envs are already rejected above).
	if _, ok := fs.provider.(SnapshotForkable); !ok {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("provider does not support fork for vm %s", srcVMID)
	}

	// an existing snapshot must belong to the source and be ready.
	if reuseSnapshotID == "" {
		return fs, SnapshotRecord{}, nil
	}
	existing, err := fm.GetSnapshotByID(ctx, reuseSnapshotID)
	if err != nil {
		return forkSource{}, SnapshotRecord{}, err
	}
	if existing.VMID != srcVMID {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: %s", ErrSnapshotNotFound, reuseSnapshotID)
	}
	if existing.State != SnapshotStateReady {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: snapshot %s is %s", ErrSnapshotInvalidState, reuseSnapshotID, existing.State)
	}
	// a fork boots the seed's rootfs on its own, and a live snapshot's
	// rootfs is only consistent alongside the memory image it was captured
	// with. reject it here rather than letting the fork provision a vm and
	// die on "agent start failed" once the guest cannot mount what it got.
	if existing.Kind == SnapshotKindLive {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: snapshot %s is a live snapshot, whose rootfs is only consistent with its memory image; fork from a disk snapshot instead", ErrSnapshotNotSeedable, reuseSnapshotID)
	}
	// a ready record is not proof of an artifact on the host. fork itself
	// writes lineage records that are ready but back no checkpoint, and a
	// snapshot can also be deleted out from under its record. confirm the
	// provider really holds it, the same guard RestoreSnapshot applies, so a
	// doomed fork fails here instead of 404ing deep in the host agent.
	if srcEnv == nil {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("vm %s has no active environment handle", srcVMID)
	}
	if _, found := lookupCheckpoint(ctx, srcEnv, reuseSnapshotID); !found {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: %s is not present on the host", ErrSnapshotNotFound, reuseSnapshotID)
	}
	return fs, existing, nil
}
