package orchestrator

import (
	"context"
	"fmt"
	"time"
)

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
// providers that cannot fork (the firecracker provider today) do not
// implement SnapshotForkable, so this reports fork as unsupported.
func (fm *FleetManager) ForkEnvironment(ctx context.Context, srcVMID string, opts ForkOptions) (string, error) {
	// validate the source vm exists and is running, and resolve the
	// provider under the lock (providerForHost requires fm.mu held),
	// mirroring snapshotEnvironment's resolution.
	fm.mu.RLock()
	src, ok := fm.vms[srcVMID]
	if !ok {
		fm.mu.RUnlock()
		return "", fmt.Errorf("%w: %s", ErrVMNotFound, srcVMID)
	}
	state := src.state
	srcSpec := src.spec
	srcHostID := src.hostID
	provider := fm.provider
	if srcHostID != "" {
		if hostProvider, ok := fm.providerForHost(srcHostID); ok {
			provider = hostProvider
		}
	}
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return "", fmt.Errorf("vm %s in state %s: fork requires running", srcVMID, state)
	}

	// obtain the seed snapshot: snapshot the source when no existing
	// snapshot was requested, otherwise validate the requested snapshot
	// belongs to the source and is ready.
	var seed SnapshotRecord
	if opts.ReuseSnapshotID == "" {
		created, err := fm.CreateSnapshot(ctx, srcVMID, SnapshotOptions{Comment: opts.Comment})
		if err != nil {
			return "", err
		}
		seed = created
	} else {
		existing, err := fm.GetSnapshotByID(ctx, opts.ReuseSnapshotID)
		if err != nil {
			return "", err
		}
		if existing.VMID != srcVMID {
			return "", fmt.Errorf("%w: %s", ErrSnapshotNotFound, opts.ReuseSnapshotID)
		}
		if existing.State != SnapshotStateReady {
			return "", fmt.Errorf("%w: snapshot %s is %s", ErrSnapshotInvalidState, opts.ReuseSnapshotID, existing.State)
		}
		seed = existing
	}

	// a true fork needs a provider that can seed a new environment from a
	// checkpoint. the firecracker provider does not implement this yet, so
	// fork is reported unsupported here.
	forkable, ok := provider.(SnapshotForkable)
	if !ok {
		return "", fmt.Errorf("provider does not support fork for vm %s", srcVMID)
	}

	// mint a new vm id. vm ids are always fm.prefix + taskID, so
	// synthesise a unique fork task id.
	forkTaskID := "fork-" + NewEventID()
	newVMID := fm.prefix + forkTaskID
	spec := srcSpec
	spec.Name = newVMID

	newEnv, err := forkable.CreateFromCheckpoint(ctx, spec, srcVMID, seed.SnapshotID)
	if err != nil {
		return "", fmt.Errorf("fork vm %s from snapshot %s: %w", srcVMID, seed.SnapshotID, err)
	}

	// register the new vm as running and persist it, mirroring the
	// running-state bookkeeping in ProvisionAndAssign (fleet.go 582-660):
	// env handle, url, state, spec, host, then persistVMByID, task upsert,
	// and publishStateChange.
	now := time.Now()
	v := &vm{
		id:        newVMID,
		state:     VMStateRunning,
		taskID:    forkTaskID,
		hostID:    srcHostID,
		env:       newEnv,
		url:       newEnv.URL(),
		spec:      spec,
		createdAt: now,
		updatedAt: now,
	}
	fm.mu.Lock()
	fm.vms[newVMID] = v
	fm.mu.Unlock()

	// persisting the running state is load-bearing: roll the in-memory
	// registration back on failure so the map stays consistent with the
	// store (same guard ProvisionAndAssign applies to persistVMByID).
	if err := fm.persistVMByID(ctx, newVMID); err != nil {
		fm.mu.Lock()
		delete(fm.vms, newVMID)
		fm.mu.Unlock()
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
		HostID:           srcHostID,
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
