package orchestrator

import (
	"context"
	"errors"
	"fmt"
)

// placeFork picks the host a fork lands on, and the provider that owns it.
//
// It is a preference ladder that ends in a refusal: a fork that fits nowhere
// fails with ErrNoCapacity (503) instead of being charged to a host that
// cannot take it, so a burst of forks waits and retries rather than
// overcommitting the source's host.
//
//  1. the source's host, when it can still take a vm of this shape. this is the
//     cheapest by far: CreateFromCheckpoint copies the rootfs within one
//     filesystem rather than across the network.
//  2. otherwise, if the seed artifact can be copied, whichever host the
//     scheduler picks -- preferring hosts that already hold those bytes, and
//     restricted to the source's backend, since a rootfs prepared for one
//     backend is not a thing another backend can boot.
//  3. otherwise refuse: ErrSeedUnplaceable (which wraps ErrNoCapacity) when the
//     seed cannot leave the source's host, ErrNoCapacity when no other host
//     has room.
//
// a live fork skips the first rung (see liveForkOffSource) and is refused
// with errLiveForkNoHost, a 409, when no other host can take it.
func (fm *FleetManager) placeFork(ctx context.Context, spec Spec, srcHostID string, srcProvider Provider, seed SnapshotRecord, live bool) (string, Provider, error) {
	// single-provider mode: there is no scheduler and no host registry to
	// consult, so there is nothing to choose between.
	if srcHostID == "" {
		if live {
			return "", nil, errLiveForkNoHost("", "there is no other host to resume on")
		}
		return "", srcProvider, nil
	}

	fm.mu.RLock()
	hosts := fm.activeHostsLocked()
	srcBackend := HostBackend("")
	if h, ok := fm.hosts[srcHostID]; ok {
		srcBackend = h.Backend
	}
	fm.mu.RUnlock()

	why := "is not active"
	if src := hostByID(hosts, srcHostID); src != nil {
		why = hostRejection(src, spec)
		if live {
			why = "is the source's own host"
		} else if why == "" {
			return srcHostID, srcProvider, nil
		}
	}

	plan := fm.planSeedPlacement(ctx, Spec{SeedSnapshotID: seed.SnapshotID})
	if !plan.movable {
		if live {
			return "", nil, errLiveForkNoHost(srcHostID, "the seed cannot be copied off it ("+plan.whyImmovable()+")")
		}
		return "", nil, fmt.Errorf("%w: source host %s %s, and the seed cannot be copied (%s)",
			ErrSeedUnplaceable, srcHostID, why, plan.whyImmovable())
	}

	eligible := make([]*Host, 0, len(hosts))
	for _, h := range hosts {
		// a rootfs prepared under one backend is not portable to another, and
		// the backends' snapshot stores are not interchangeable either, so a
		// fork never crosses that line however much room the other side has.
		if h.Backend == srcBackend && !liveForkOffSource(live, h.ID, srcHostID) {
			eligible = append(eligible, h)
		}
	}
	selected, _, err := SchedulePreferring(spec, eligible, fm.placementPolicy,
		PlacementHints{ArtifactHosts: plan.hostPreference()})
	if err != nil && live {
		return "", nil, errLiveForkNoHost(srcHostID, fmt.Sprintf("no other host can take it: %v", err))
	}
	if err != nil {
		// a label or pin miss is still "nowhere to put it" for a fork, so every
		// scheduling refusal reaches the caller as ErrNoCapacity.
		if errors.Is(err, ErrNoCapacity) {
			return "", nil, fmt.Errorf("fork: source host %s %s: %w", srcHostID, why, err)
		}
		return "", nil, fmt.Errorf("%w: source host %s %s: %v", ErrNoCapacity, srcHostID, why, err)
	}

	fm.mu.RLock()
	targetProvider, ok := fm.providerForHost(selected.ID)
	fm.mu.RUnlock()
	if !ok {
		// the host left between scheduling and here: retry-later, not a 500.
		return "", nil, fmt.Errorf("%w: host %s was removed while the fork was placed", ErrNoCapacity, selected.ID)
	}
	return selected.ID, targetProvider, nil
}

// liveForkOffSource reports whether hostID is ruled out for a live fork: it
// is the source's own host. pending the fc-agent same-host live resume:
// resume_plan refuses a live snapshot whose recorded drive path is a running
// vm's dir (409), and resume_firecracker would rmtree that dir, which here is
// the source's. until the agent can resume next to a running source, a live
// child always lands on another host.
func liveForkOffSource(live bool, hostID, srcHostID string) bool {
	return live && hostID == srcHostID
}

// errLiveForkNoHost is the refusal of a live fork with nowhere but its
// source's host to resume on (see liveForkOffSource): a 409 naming why,
// rather than the 503 of a full fleet.
func errLiveForkNoHost(srcHostID, why string) error {
	if srcHostID == "" {
		srcHostID = "the only provider"
	}
	return fmt.Errorf("%w: a live fork cannot resume on its source's host (%s) yet, and %s; fork without live, or add a host",
		ErrSnapshotNotSeedable, srcHostID, why)
}

// forkFitsSomewhere is the cheap half of placeFork's gate, run before the
// seed snapshot: ErrNoCapacity when no active host of the source's backend
// could take a vm of the source's shape (errLiveForkNoHost for a live fork,
// which never counts the source's host). a single-provider source has no
// host to check.
func (fm *FleetManager) forkFitsSomewhere(src forkSource, live bool) error {
	if src.hostID == "" {
		if live {
			return errLiveForkNoHost("", "there is no other host to resume on")
		}
		return nil
	}
	// the fork's shape, without the source's placement (see ForkPlaceholder).
	spec := src.spec
	spec.SeedSnapshotID, spec.PinnedHostID, spec.HostID, spec.Labels = "", "", "", nil
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	backend := HostBackend("")
	if h, ok := fm.hosts[src.hostID]; ok {
		backend = h.Backend
	}
	for _, h := range fm.activeHostsLocked() {
		if h.Backend == backend && !liveForkOffSource(live, h.ID, src.hostID) && hostRejection(h, spec) == "" {
			return nil
		}
	}
	if live {
		return errLiveForkNoHost(src.hostID, fmt.Sprintf("no other %s host has room for a %dCPU/%dMB/%dGB fork",
			backend, spec.CPUs, spec.RamMB, spec.StorageGB))
	}
	return fmt.Errorf("%w: no %s host has room for a %dCPU/%dMB/%dGB fork",
		ErrNoCapacity, backend, spec.CPUs, spec.RamMB, spec.StorageGB)
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

	// Live resumes every child from a live snapshot's memory image instead
	// of cold-booting its rootfs: a live seed is taken (or ReuseSnapshotID
	// must name a live snapshot). see liveForkOffSource for where it lands.
	Live bool

	// Count is how many children to fork from the one seed, which is taken
	// once. 0 means 1; it is at most MaxForkCount.
	Count int
}

// ForkEnvironment creates opts.Count brand-new vms (one by default) seeded
// from one checkpoint of an existing running vm, and returns their ids in
// child order. each child is a durable fork of its own (see forkChildren).
//
// the seed is ReuseSnapshotID, or a snapshot of the source taken once for all
// the children. each child is built from it by a SnapshotForkable provider
// (or, live, resumed from its memory image), registered as running, and
// given lineage that references the seed snapshot.
//
// lineage is recorded by persisting a SnapshotRecord for the new vm whose
// ParentSnapshotID is the seed snapshot id (state ready). this reuses the
// same store path CreateSnapshot uses (upsertSnapshotRecord) and is
// directly assertable via ListSnapshots / GetSnapshotByID.
//
// each child is charged to whichever host it lands on, which is the source's
// host (never, for a live fork) unless that host can no longer fit it and the seed artifact can be copied
// elsewhere (see placeFork). it is given its OWN guest credentials either way,
// since it boots a copy of the source's disk and would otherwise answer to the
// source's token.
//
// providers that cannot fork do not implement SnapshotForkable, so this reports
// fork as unsupported for them. the firecracker provider implements it; the
// qemu provider deliberately does not (vfio gpu passthrough cannot be
// checkpointed).
func (fm *FleetManager) ForkEnvironment(ctx context.Context, srcVMID string, opts ForkOptions) ([]string, error) {
	ctx, end := fm.startOperation(ctx, "fork", srcVMID)
	ids, err := fm.forkChildren(ctx, fm.durableForker(), srcVMID, opts)
	end(err)
	return ids, err
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
// when reuseSnapshotID is set, the seed snapshot. live is a live fork, which
// needs a live seed. it takes no snapshot and writes nothing, so a refused
// fork leaves no trace.
func (fm *FleetManager) checkFork(ctx context.Context, srcVMID, reuseSnapshotID string, live bool) (forkSource, SnapshotRecord, error) {
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
	// a cold fork boots the seed's rootfs on its own, and a live snapshot's
	// rootfs is only consistent alongside the memory image it was captured
	// with. reject it here rather than letting the fork provision a vm and
	// die on "agent start failed" once the guest cannot mount what it got. a
	// live fork resumes that memory image, so it is the one fork that may.
	if existing.Kind == SnapshotKindLive && !live {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: snapshot %s is a live snapshot, whose rootfs is only consistent with its memory image; fork from a disk snapshot instead, or fork with live", ErrSnapshotNotSeedable, reuseSnapshotID)
	}
	if live && existing.Kind != SnapshotKindLive {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: snapshot %s is a disk snapshot, with no memory image for a live fork to resume; reuse a live snapshot, or fork without live", ErrSnapshotNotSeedable, reuseSnapshotID)
	}
	if live && diffParent(existing) != "" {
		return forkSource{}, SnapshotRecord{}, fmt.Errorf("%w: snapshot %s is a diff against %s and cannot be resumed on its own", ErrSnapshotNotSeedable, reuseSnapshotID, diffParent(existing))
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
