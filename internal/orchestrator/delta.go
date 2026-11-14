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

// moveDeltaToHost copies a diff snapshot's delta files to hostID and has its
// agent merge them onto base, a complete copy of the diff's parent that host
// already holds. the merged result is a complete live snapshot, recorded as a
// free-standing copy and returned by id. dropBase has the agent delete base
// once the merge has committed, and its record goes with it.
//
// the merged copy records the digests the agent reports for it. merged onto
// a version 2 base, the agent works out a mem digest from the base's chunk
// table and the chunks the delta touched (see pull_artifact_delta), so the
// copy is a full live snapshot that can move on to a third host. merged onto
// a version 1 base, or by an older agent, it gets no mem digest and records
// none: it can be the base of the next delta or a seed on that host, and
// ensureArtifactOnHost refuses it as a source for want of live_files.
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
	source, _, err := fm.artifactEndpoints(child, holders, hostID)
	if err != nil {
		return "", err
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
	return fm.runArtifactMove(ctx, job, func(err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("move delta %s to host %s: %w", child.SnapshotID, hostID, ctxErr)
		}
		return fmt.Errorf("%w: move delta %s from host %s onto %s on host %s: %w",
			ErrArtifactTransferFailed, child.SnapshotID, source.endpoint.HostID, base, hostID, err)
	})
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
