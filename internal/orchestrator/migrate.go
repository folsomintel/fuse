package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

func (fm *FleetManager) discardProvisionedVM(vmID, hostID string, spec Spec) {
	fm.mu.Lock()
	delete(fm.vms, vmID)
	if hostID != "" {
		fm.deallocateOnHost(hostID, spec)
	}
	fm.mu.Unlock()
}

func (fm *FleetManager) abandonProvisionedVM(ctx context.Context, provider Provider, hostID, vmID string, spec Spec) {
	if err := provider.Destroy(ctx, vmID); err != nil {
		fm.logger.Warn("destroy partially provisioned vm failed", "vm", vmID, "err", err)
	}
	fm.discardProvisionedVM(vmID, hostID, spec)
}

func (fm *FleetManager) MigrateVM(ctx context.Context, vmID string, targetHostID string) (string, error) {
	fm.mu.RLock()
	src, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return "", fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
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

	if targetHostID != "" {
		srcBackend := HostBackend("")
		if h, ok := fm.hosts[srcHostID]; ok {
			srcBackend = h.Backend
		}
		targetBackend := HostBackend("")
		if h, ok := fm.hosts[targetHostID]; ok {
			targetBackend = h.Backend
		}
		if targetBackend == "" {
			fm.mu.RUnlock()
			return "", fmt.Errorf("%w: target host %s is not registered", ErrArtifactImmovable, targetHostID)
		}
		if srcBackend != "" && targetBackend != srcBackend {
			fm.mu.RUnlock()
			return "", fmt.Errorf("source host %s runs %s backend, target host %s runs %s backend: cross-backend migration is not supported", srcHostID, srcBackend, targetHostID, targetBackend)
		}
	}
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return "", fmt.Errorf("vm %s in state %s: migrate requires running", vmID, state)
	}

	if srcSpec.GPUs > 0 {
		return "", fmt.Errorf("%w: vm %s has a gpu passthrough device: migrate is not supported for gpu environments", ErrGPUUnsupported, vmID)
	}

	forkable, ok := provider.(SnapshotForkable)
	if !ok {
		return "", fmt.Errorf("provider does not support migrate for vm %s", vmID)
	}

	seed, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{Comment: "migration seed"})
	if err != nil {
		return "", err
	}

	migrateTaskID := "migrate-" + NewEventID()
	newVMID := fm.prefix + migrateTaskID
	spec := srcSpec
	spec.Name = newVMID
	spec.SeedSnapshotID = ""
	spec.PinnedHostID = ""
	spec.HostID = ""
	spec.Labels = nil

	if targetHostID == "" {
		targetHostID = srcHostID
	}

	targetProvider := provider
	if targetHostID != srcHostID && targetHostID != "" {
		fm.mu.RLock()
		if hostProvider, ok := fm.providerForHost(targetHostID); ok {
			targetProvider = hostProvider
		}
		fm.mu.RUnlock()
	}

	now := time.Now()
	v := &vm{
		id:             newVMID,
		state:          VMStateProvisioning,
		taskID:         migrateTaskID,
		hostID:         targetHostID,
		spec:           spec,
		createdAt:      now,
		updatedAt:      now,
		lastActivityAt: now,
	}

	fm.mu.Lock()
	fm.vms[newVMID] = v
	if targetHostID != "" {
		fm.allocateOnHost(targetHostID, v)
	}
	fm.mu.Unlock()

	var newEnv Environment
	if targetHostID == srcHostID {
		newEnv, err = forkable.CreateFromCheckpoint(ctx, spec, vmID, seed.SnapshotID)
	} else {
		var localID string
		localID, err = fm.ensureArtifactOnHost(ctx, seed, targetHostID)
		if err == nil {
			spec.SeedSnapshotID = localID
			spec.PinnedHostID = targetHostID
			fm.mu.Lock()
			v.spec.SeedSnapshotID = localID
			v.spec.PinnedHostID = targetHostID
			fm.mu.Unlock()
			fm.touchArtifact(localID, seed.SnapshotID)
			newEnv, err = targetProvider.Create(ctx, spec)
		}
	}

	if err != nil {
		fm.discardProvisionedVM(newVMID, targetHostID, spec)
		return "", fmt.Errorf("migrate vm %s from snapshot %s: %w", vmID, seed.SnapshotID, err)
	}

	var encToken []byte
	drainCommand := DefaultFusedDrainCommand
	if len(fm.tokenEncryptionKey) == 32 {
		creds, credErr := secrets.GenerateVMCredentials(newVMID)
		if credErr != nil {
			fm.abandonProvisionedVM(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("generate credentials for migrated vm %s: %w", newVMID, credErr)
		}
		if upErr := uploadFiles(ctx, newEnv, fusedCredentialFiles(creds)); upErr != nil {
			fm.abandonProvisionedVM(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("upload credentials to migrated vm %s: %w", newVMID, upErr)
		}
		setTokenIfSupported(newEnv, creds)
		encToken, err = secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
		if err != nil {
			fm.abandonProvisionedVM(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("encrypt token for migrated vm %s: %w", newVMID, err)
		}
		if agentErr := newEnv.StartAgent(ctx, AgentSpec{
			AuthToken:    creds.AuthToken,
			DrainCommand: drainCommand,
		}); agentErr != nil {
			fm.abandonProvisionedVM(ctx, targetProvider, targetHostID, newVMID, spec)
			return "", fmt.Errorf("restart guest agent on migrated vm %s with its own credentials: %w", newVMID, agentErr)
		}
	}

	fm.mu.Lock()
	v.state = VMStateRunning
	v.env = newEnv
	v.url = newEnv.URL()
	v.authTokenEncrypted = encToken
	v.drainCommand = drainCommand
	v.updatedAt = time.Now()
	fm.mu.Unlock()

	if err := fm.persistVMByID(ctx, newVMID); err != nil {
		fm.abandonProvisionedVM(ctx, targetProvider, targetHostID, newVMID, spec)
		return "", fmt.Errorf("persist migrated vm %s running state: %w", newVMID, err)
	}
	if fm.store != nil {
		if err := fm.store.UpsertTask(ctx, TaskRecord{
			TaskID:     migrateTaskID,
			VMID:       newVMID,
			RunStatus:  TaskRunRunning,
			AssignedAt: now,
			UpdatedAt:  now,
		}); err != nil {
			fm.logger.Warn("persist migrated task running state failed", "vm", newVMID, "task", migrateTaskID, "err", err)
		}
	}
	fm.publishStateChange(newVMID, "")

	lineageMeta, _ := marshalSnapshotMetadata("", nil)
	lineage := SnapshotRecord{
		SnapshotID:       "migrate-seed-" + NewEventID(),
		VMID:             newVMID,
		TaskID:           migrateTaskID,
		HostID:           targetHostID,
		TenantID:         snapshotTenantID(migrateTaskID, newVMID),
		ParentSnapshotID: seed.SnapshotID,
		Mode:             SnapshotModeAuto,
		State:            SnapshotStateReady,
		Metadata:         lineageMeta,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := fm.upsertSnapshotRecord(ctx, lineage); err != nil {
		fm.logger.Warn("persist migrate lineage record failed", "vm", newVMID, "parent", seed.SnapshotID, "err", err)
	}

	fm.appendEvent(ctx, "vm", newVMID, "vm.migrated", map[string]any{
		"source_vm_id":     vmID,
		"source_host_id":   srcHostID,
		"target_host_id":   targetHostID,
		"seed_snapshot_id": seed.SnapshotID,
	})

	if drainErr := fm.Drain(ctx, vmID); drainErr != nil {
		fm.logger.Warn("drain source vm during migration failed; destroying anyway", "vm", vmID, "err", drainErr)
	}
	if destroyErr := fm.DestroyVM(ctx, vmID); destroyErr != nil {
		fm.logger.Warn("destroy source vm during migration failed", "vm", vmID, "err", destroyErr)
	}

	return newVMID, nil
}
