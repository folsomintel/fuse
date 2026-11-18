package orchestrator

import (
	"context"
	"fmt"
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

// planDeltaMove is the move that merges child, a diff, onto the copy of
// base on hostID, dropping that copy when dropBase is set.
func (fm *FleetManager) planDeltaMove(child SnapshotRecord, base, hostID string, dropBase bool) (ArtifactMoveJob, error) {
	if diffParent(child) == "" {
		return ArtifactMoveJob{}, fmt.Errorf("%w: snapshot %s is not a diff", ErrArtifactImmovable, child.SnapshotID)
	}
	if fm.artifactMover == nil {
		return ArtifactMoveJob{}, fmt.Errorf("%w: no artifact mover is configured", ErrArtifactImmovable)
	}
	files := liveFiles(child)
	if len(files) == 0 {
		return ArtifactMoveJob{}, fmt.Errorf("%w: diff snapshot %s has no recorded digests for its delta files", ErrArtifactImmovable, child.SnapshotID)
	}
	holders := map[string]ArtifactHolder{child.HostID: {HostID: child.HostID, SnapshotID: child.SnapshotID}}
	source, _, err := fm.artifactEndpoints(child, holders, hostID)
	if err != nil {
		return ArtifactMoveJob{}, err
	}

	job := ArtifactMoveJob{
		SnapshotID:       child.SnapshotID,
		Digest:           child.Digest,
		SourceHostID:     source.endpoint.HostID,
		SourceSnapshotID: child.SnapshotID,
		HostID:           hostID,
		LocalID:          liveReplicaSnapshotID(child.SnapshotID, hostID),
		Files:            files,
		Base:             base,
		DropBase:         dropBase,
		TenantID:         child.TenantID,
		Arch:             child.Arch,
		Kind:             SnapshotKindLive,
	}
	return job, nil
}

// completeLiveFiles reports whether files names a digest for every file a
// full live move sends, and nothing else: what a merged copy needs to be the
// source of one.
func completeLiveFiles(files map[string]string) bool {
	for _, name := range []string{"vmstate", "mem", "live.json"} {
		if files[name] == "" {
			return false
		}
	}
	return len(files) == 3
}
