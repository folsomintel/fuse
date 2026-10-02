package orchestrator

import (
	"context"
	"fmt"
	"time"
)

// deleteFreeArtifact removes a free-standing copy (a pulled artifact or a
// checkpoint base) from hostID's snapshot store. a host whose provider cannot
// do it is not an error: the bytes stay, which is what always happened before.
func (fm *FleetManager) deleteFreeArtifact(ctx context.Context, hostID, snapshotID string) error {
	fm.mu.RLock()
	provider, ok := fm.providerForHost(hostID)
	fm.mu.RUnlock()
	if !ok {
		return nil
	}
	deleter, ok := provider.(ArtifactDeleter)
	if !ok {
		return nil
	}
	return deleter.DeleteArtifact(ctx, snapshotID)
}

// forgetSnapshotRecord drops a snapshot's metadata once its bytes are gone,
// for a copy its host agent already removed itself (a delta's dropped base).
func (fm *FleetManager) forgetSnapshotRecord(ctx context.Context, snapshotID string) {
	if fm.store == nil || snapshotID == "" {
		return
	}
	if err := fm.store.DeleteSnapshot(ctx, snapshotID); err != nil {
		fm.logger.Warn("forget snapshot record failed", "snapshot", snapshotID, "err", err)
	}
}

// moveDeltaToHost copies a diff snapshot's delta files to hostID and has its
// agent merge them onto base, a complete copy of the diff's parent that host
// already holds. the merged result is a complete live snapshot, recorded as a
// free-standing copy and returned by id. dropBase has the agent delete base
// once the merge has committed, and its record goes with it.
//
// the merged copy records no memory digest (the agent does not re-hash a
// merge, see pull_artifact_delta), so it can be the base of the next delta or
// a seed on that host, but not the source of a full move to a third host:
// ensureArtifactOnHost refuses it for want of live_files.
func (fm *FleetManager) moveDeltaToHost(ctx context.Context, child SnapshotRecord, base, hostID string, dropBase bool) (string, error) {
	if diffParent(child) == "" {
		return "", fmt.Errorf("%w: snapshot %s is not a diff", ErrArtifactImmovable, child.SnapshotID)
	}
	if fm.artifactMover == nil {
		return "", fmt.Errorf("%w: no artifact mover is configured", ErrArtifactImmovable)
	}
	files := liveFiles(child)
	if len(files) == 0 {
		return "", fmt.Errorf("%w: diff snapshot %s has no recorded digests for its delta files", ErrArtifactImmovable, child.SnapshotID)
	}
	holders := map[string]ArtifactHolder{child.HostID: {HostID: child.HostID, SnapshotID: child.SnapshotID}}
	source, target, err := fm.artifactEndpoints(child, holders, hostID)
	if err != nil {
		return "", err
	}

	localID := liveReplicaSnapshotID(child.SnapshotID, hostID)
	fm.beginArtifactPull(child.SnapshotID, base, localID)
	defer fm.endArtifactPull(child.SnapshotID, base, localID)

	pullCtx := ctx
	if fm.artifactPullTimeout > 0 {
		var cancel context.CancelFunc
		pullCtx, cancel = context.WithTimeout(ctx, fm.artifactPullTimeout)
		defer cancel()
	}
	moved, err := fm.artifactMover.MoveArtifact(pullCtx, ArtifactMove{
		Digest:           child.Digest,
		SnapshotID:       localID,
		From:             source.endpoint,
		To:               target,
		Files:            files,
		SourceSnapshotID: child.SnapshotID,
		Base:             base,
		DropBase:         dropBase,
	})
	if err == nil && moved.Kind != SnapshotKindLive {
		err = fmt.Errorf("host %s did not commit the merged delta as a live snapshot; its host agent cannot merge deltas", hostID)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("move delta %s to host %s: %w", child.SnapshotID, hostID, ctxErr)
		}
		return "", fmt.Errorf("%w: move delta %s from host %s onto %s on host %s: %w",
			ErrArtifactTransferFailed, child.SnapshotID, source.endpoint.HostID, base, hostID, err)
	}

	committedID := moved.SnapshotID
	if committedID == "" {
		committedID = localID
	}
	now := time.Now()
	metadata, _ := marshalSnapshotMetadata("", map[string]string{
		"source_snapshot_id": child.SnapshotID,
		"source_host_id":     source.endpoint.HostID,
		"merged_onto":        base,
	})
	replica := SnapshotRecord{
		SnapshotID: committedID,
		HostID:     hostID,
		TenantID:   child.TenantID,
		// build mode keeps retention gc away from it: the code that made it
		// (a migrate or the checkpoint loop) is the code that removes it.
		Mode:      SnapshotModeBuild,
		Arch:      child.Arch,
		Digest:    child.Digest,
		Kind:      SnapshotKindLive,
		State:     SnapshotStateReady,
		SizeBytes: moved.SizeBytes,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := fm.upsertSnapshotRecord(ctx, replica); err != nil {
		return "", fmt.Errorf("persist merged delta %s on host %s: %w", committedID, hostID, err)
	}
	if dropBase {
		fm.forgetSnapshotRecord(ctx, base)
	}
	return committedID, nil
}
