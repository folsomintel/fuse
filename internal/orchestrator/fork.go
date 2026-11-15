package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// discardFork drops the fork's provisioning placeholder and gives back the host
// capacity it reserved, for a fork that never became a live vm. the placeholder
// was never persisted, so the store needs no cleanup.
func (fm *FleetManager) discardFork(vmID, hostID string, spec Spec) {
	fm.mu.Lock()
	delete(fm.vms, vmID)
	if hostID != "" {
		fm.deallocateOnHost(hostID, spec)
	}
	fm.mu.Unlock()
	fm.unpublishIngress(context.Background(), vmID)
}

// abandonFork tears down a fork that was created on the provider but could not
// be finished, releasing the real microVM (with its tap and forwards), the
// placeholder record, and the host capacity reserved for it. failures are
// logged rather than returned: the caller is already reporting the error that
// got us here.
func (fm *FleetManager) abandonFork(ctx context.Context, provider Provider, hostID, vmID string, spec Spec) {
	if err := provider.Destroy(ctx, vmID); err != nil {
		fm.logger.Warn("destroy partially forked vm failed", "vm", vmID, "err", err)
	}
	fm.discardFork(vmID, hostID, spec)
}

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
	if d := fm.durableForker(); d != nil {
		id, err = fm.durableForkVM(ctx, d, srcVMID, opts)
	} else {
		id, err = fm.forkEnvironment(ctx, srcVMID, opts)
	}
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
// writes nothing, so the in-process and durable forks refuse the same
// requests the same way.
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

func (fm *FleetManager) forkEnvironment(ctx context.Context, srcVMID string, opts ForkOptions) (string, error) {
	src, seed, err := fm.checkFork(ctx, srcVMID, opts.ReuseSnapshotID)
	if err != nil {
		return "", err
	}
	srcSpec, srcHostID, provider := src.spec, src.hostID, src.provider
	forkable := provider.(SnapshotForkable)

	// obtain the seed snapshot: snapshot the source when no existing
	// snapshot was requested.
	if opts.ReuseSnapshotID == "" {
		created, err := fm.snapshotInProcess(ctx, srcVMID, SnapshotOptions{Comment: opts.Comment})
		if err != nil {
			return "", err
		}
		seed = created
	}

	// mint a new vm id. vm ids are always fm.prefix + taskID, so
	// synthesise a unique fork task id.
	forkTaskID := forkTaskPrefix + NewEventID()
	newVMID := fm.prefix + forkTaskID
	spec := srcSpec
	spec.Name = newVMID
	// a fork inherits the source's shape, never its seed. carrying the source's
	// own SeedSnapshotID forward would name an artifact on the SOURCE's host,
	// which is meaningless once the fork can land elsewhere; the fork's rootfs
	// comes from the seed snapshot chosen above.
	spec.SeedSnapshotID = ""
	spec.PinnedHostID = ""
	// placement is cleared, not inherited: the fork's placement is decided below
	// from the source's host and the seed's location, so carrying the source's
	// placement.host/labels forward would record selectors nothing evaluates --
	// and they could name a different host than the one the fork lands on.
	spec.HostID = ""
	spec.Labels = nil
	// the fork descends from the source, so an invite for the source (or any
	// of its ancestors) covers it too.
	spec.Ancestors = append(slices.Clone(srcSpec.Ancestors), srcVMID)

	// a fork is a real vm consuming real resources on whichever host it lands
	// on, so it must be charged to that host.
	//
	// this is load-bearing beyond bookkeeping: DestroyVM deallocates by v.spec
	// unconditionally (fleet.go), so a fork that was never allocated would, on
	// destroy, credit back its (source-sized) cpu/ram to a host that never
	// charged for it. the counters clamp at zero, making the drift permanent
	// and leaving the scheduler to overcommit that host from then on.
	// allocateOnHost binds by *vm; forks are gpu-free (rejected above), so the
	// placeholder below charges cpu/ram/storage with no per-device gpu binding
	// to record.
	//
	// the placeholder also has to enter fm.vms BEFORE the microVM exists on the
	// host, the same way ProvisionAndAssign registers a provisioning vm before
	// it boots. everything from CreateFromCheckpoint through the credential
	// upload and the fused restart is a window where the vm is real on the host
	// but not yet running from the fleet's point of view, and reconcileOrphans
	// destroys any provider vm it does not find in fm.vms. the reverse
	// direction is safe: reconcile skips provisioning vms when it looks for vms
	// that vanished from the provider, so the placeholder is not torn down for
	// not existing yet. it is not persisted, so an orchestrator crash mid-fork
	// leaves nothing behind to recover.
	//
	// the source's host is where a fork belongs by default, because
	// CreateFromCheckpoint copies the rootfs within one filesystem. "belongs
	// there" must not mean "fails there" though: when that host cannot take
	// another vm of this shape, the seed artifact follows the fork to a host
	// that can, exactly as a seeded create does. placeFork never fails; the
	// worst case is the source's host, which is what this always did.
	targetHostID, targetProvider := fm.placeFork(ctx, spec, srcHostID, provider, seed)

	now := time.Now()
	v := &vm{
		id:                newVMID,
		state:             VMStateProvisioning,
		taskID:            forkTaskID,
		hostID:            targetHostID,
		spec:              spec,
		createdAt:         now,
		updatedAt:         now,
		lastActivityAt:    now,
		hitlKeys:          src.hitlKeys,
		hitlKeysEncrypted: src.hitlKeysEncrypted,
	}
	fm.mu.Lock()
	fm.vms[newVMID] = v
	if targetHostID != "" {
		fm.allocateOnHost(targetHostID, v)
	}
	fm.mu.Unlock()

	var newEnv Environment
	if targetHostID == srcHostID {
		newEnv, err = forkable.CreateFromCheckpoint(ctx, spec, srcVMID, seed.SnapshotID)
	} else {
		// off-host fork: the seed's bytes are copied to the target first, then
		// the vm is created from them like any other seeded environment. this is
		// the same rootfs CreateFromCheckpoint would have used, so the fork's
		// contents are identical; only the transport differs.
		var localID string
		localID, err = fm.ensureArtifactOnHost(ctx, seed, targetHostID, false)
		if err == nil {
			spec.SeedSnapshotID = localID
			spec.PinnedHostID = targetHostID
			// only these two fields, never the whole spec: allocateOnHost may
			// have bound resources onto v.spec above, and overwriting it would
			// silently drop that binding.
			fm.mu.Lock()
			v.spec.SeedSnapshotID = localID
			v.spec.PinnedHostID = targetHostID
			fm.mu.Unlock()
			fm.touchArtifact(localID, seed.SnapshotID)
			newEnv, err = targetProvider.Create(ctx, spec)
		}
	}
	if err != nil {
		fm.discardFork(newVMID, targetHostID, spec)
		return "", fmt.Errorf("fork vm %s from snapshot %s: %w", srcVMID, seed.SnapshotID, err)
	}

	// the fork booted a byte copy of the source's disk, so its guest already has
	// the SOURCE's credentials on it (/fuse/auth-token, /fuse/tls/*) and its
	// fused unit auto-starts from that disk holding the SOURCE's identity.
	// leaving it there would both leave this vm's record with no token at all
	// and let two live vms authenticate with one shared secret, so mint fresh
	// credentials for the fork.
	//
	// uploading the files is NOT enough on its own: fused reads --auth-token-file
	// exactly once at process start (fused/main.go) and has no reloader, so
	// the already-running process would keep serving the source's token. StartAgent
	// is what makes the host agent rewrite the unit's env with the fork's own
	// vm id and restart fused, which is the step that actually takes effect.
	//
	// only the credential files are uploaded, never a full FusedAgentSpec: the
	// manifest and secrets the fork should run with are the ones already on the
	// copied disk, and a nil manifest would clobber them.
	//
	// like Boot, this is a no-op without a 32-byte encryption key (dev mode):
	// the source then had no credentials either, so the fork inherits none and
	// stays consistent with it.
	// a fork is a new environment, so it gets ports of its own; the tunnel
	// identity is fresh for the same reason the credentials below are.
	var plan *ingressPlan
	if fm.ingressSupported(targetHostID, spec) {
		plan, err = fm.publishIngress(ctx, newVMID, nil, "")
	}
	if err != nil {
		fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
		return "", err
	}

	var encToken []byte
	drainCommand := DefaultFusedDrainCommand
	if len(fm.tokenEncryptionKey) == 32 {
		creds, credErr := secrets.GenerateVMCredentials(newVMID)
		if credErr != nil {
			fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("generate credentials for forked vm %s: %w", newVMID, credErr)
		}
		if upErr := uploadFiles(ctx, newEnv, plan.withTunnelFiles(fusedCredentialFiles(creds))); upErr != nil {
			fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("upload credentials to forked vm %s: %w", newVMID, upErr)
		}
		setTokenIfSupported(newEnv, creds)
		encToken, err = secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
		if err != nil {
			fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("encrypt token for forked vm %s: %w", newVMID, err)
		}
		if agentErr := newEnv.StartAgent(ctx, AgentSpec{
			AuthToken:        creds.AuthToken,
			DrainCommand:     drainCommand,
			TunnelConfigPath: plan.tunnelConfigPath(),
		}); agentErr != nil {
			fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("restart guest agent on forked vm %s with its own credentials: %w", newVMID, agentErr)
		}
	} else if tunnelErr := fm.retunnelWithoutCredentials(ctx, newEnv, plan, drainCommand); tunnelErr != nil {
		fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
		return "", fmt.Errorf("start tunnel on forked vm %s: %w", newVMID, tunnelErr)
	}

	// promote the placeholder to running and persist it, mirroring the
	// running-state bookkeeping in ProvisionAndAssign: env handle, url, state,
	// then persistVMByID, task upsert, and publishStateChange.
	fm.mu.Lock()
	v.state = VMStateRunning
	v.env = newEnv
	v.url = newEnv.URL()
	v.authTokenEncrypted = encToken
	v.drainCommand = drainCommand
	v.updatedAt = time.Now()
	if plan != nil {
		v.applyIngress(plan.grant)
	}
	fm.mu.Unlock()

	// persisting the running state is load-bearing: roll the in-memory
	// registration back on failure so the map stays consistent with the
	// store (same guard ProvisionAndAssign applies to persistVMByID). the
	// microVM is real by this point, so it has to be torn down too, or it
	// keeps running on the host with nothing tracking it.
	if err := fm.persistVMByID(ctx, newVMID); err != nil {
		fm.abandonFork(ctx, targetProvider, targetHostID, newVMID, spec)
		return "", fmt.Errorf("persist forked vm %s running state: %w", newVMID, err)
	}
	// the task record mirrors the running task upsert in ProvisionAndAssign;
	// the vm is already running, so a persist failure here is best-effort.
	if fm.store != nil {
		if err := fm.store.UpsertTask(ctx, TaskRecord{
			TaskID:     forkTaskID,
			VMID:       newVMID,
			RunStatus:  TaskRunRunning,
			AssignedAt: now,
			UpdatedAt:  now,
		}); err != nil {
			fm.logger.Warn("persist forked task running state failed", "vm", newVMID, "task", forkTaskID, "err", err)
		}
	}
	fm.publishStateChange(newVMID, "")

	// record lineage: a ready snapshot record for the new vm whose parent is
	// the seed snapshot. this is the assertable link back to the source's
	// checkpoint and reuses CreateSnapshot's persist path.
	lineageMeta, _ := marshalSnapshotMetadata("", nil)
	lineage := SnapshotRecord{
		SnapshotID:       "fork-seed-" + NewEventID(),
		VMID:             newVMID,
		TaskID:           forkTaskID,
		HostID:           targetHostID,
		TenantID:         snapshotTenantID(forkTaskID, newVMID),
		ParentSnapshotID: seed.SnapshotID,
		Mode:             SnapshotModeAuto,
		State:            SnapshotStateReady,
		Metadata:         lineageMeta,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := fm.upsertSnapshotRecord(ctx, lineage); err != nil {
		fm.logger.Warn("persist fork lineage record failed", "vm", newVMID, "parent", seed.SnapshotID, "err", err)
	}

	fm.appendEvent(ctx, "vm", newVMID, "vm.forked", map[string]any{
		"source_vm_id":     srcVMID,
		"seed_snapshot_id": seed.SnapshotID,
	})

	return newVMID, nil
}
