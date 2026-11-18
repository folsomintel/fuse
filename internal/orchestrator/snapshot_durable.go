package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

var (
	// errSnapshotOptions is a snapshot asked for with options that do not go
	// together, such as diff without live.
	errSnapshotOptions = errors.New("invalid snapshot options")
	// errNoEnvironment is a vm the fleet tracks with no environment handle.
	errNoEnvironment = errors.New("vm has no active environment handle")
)

// SnapshotFleetErrors are the sentinel errors a snapshot or restore step
// fails with when a retry cannot change the outcome. the durable handlers
// carry each one across restate as its own error code, so the api still maps
// it to the same status.
var SnapshotFleetErrors = []error{
	ErrVMNotFound,
	ErrSnapshotNotFound,
	ErrVMNotRunning,
	ErrSnapshotInvalidState,
	ErrSnapshotQuotaExceeded,
	ErrGPUUnsupported,
	ErrSnapshotUnsupported,
}

// PermanentSnapshotError reports whether a snapshot or restore step failed in
// a way a retry cannot change: one of SnapshotFleetErrors, options that do not
// go together, a vm with no environment, or an agent that refused the request
// with a 4xx other than a timeout or rate limit.
func PermanentSnapshotError(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range SnapshotFleetErrors {
		if errors.Is(err, s) {
			return true
		}
	}
	if errors.Is(err, errSnapshotOptions) || errors.Is(err, errNoEnvironment) {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		code := statusErr.Code
		return code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
	}
	return false
}

// DurableSnapshots runs snapshots, restores, the checkpoint chain and
// snapshot gc on the restate Environment/{vm_id} object, so each one is
// serialized with the vm's other lifecycle ops and resumes after a restart.
// *durable.Client satisfies it.
type DurableSnapshots interface {
	// Snapshot runs Environment/{vmID}.Snapshot and waits for it. the
	// snapshot keeps running if ctx ends first.
	Snapshot(ctx context.Context, vmID string, opts SnapshotOptions) (SnapshotRecord, error)
	// Restore runs Environment/{vmID}.Restore and waits for it.
	Restore(ctx context.Context, vmID, snapshotID string) error
	// Checkpoint advances vmID's checkpoint chain by one tick and waits
	// until the tick has taken its checkpoint; the copy to the standby
	// runs on after it returns.
	Checkpoint(ctx context.Context, vmID string) error
	// SendRetireChain enqueues the removal of what a gone vm's chain holds.
	SendRetireChain(ctx context.Context, vmID string) error
	// SendCollectSnapshot enqueues the deletion of a snapshot retention gc
	// marked deleting. key dedupes repeat sends for the same marking.
	SendCollectSnapshot(ctx context.Context, vmID, snapshotID, key string) error
}

// SetDurableSnapshots routes snapshots, restores, the checkpoint chain and
// retention gc through restate. set it before Start.
func (fm *FleetManager) SetDurableSnapshots(d DurableSnapshots) {
	fm.mu.Lock()
	fm.durableSnapshots = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableSnapshotsClient() DurableSnapshots {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableSnapshots
}

// durableWait bounds a wait on a durable op for a caller with no deadline of
// its own (an approved hitl run), so a restate that accepts and never answers
// does not hold it forever. the op keeps running if the wait ends.
func durableWait(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, durableMoveWait)
}

// snapshotPinned reports whether a durable pin (a move, a checkpoint chain's
// base) holds id against gc. a pin that cannot be read counts as held.
func (fm *FleetManager) snapshotPinned(ctx context.Context, id string) bool {
	pinned, err := fm.durableArtifactsClient().ArtifactPinned(ctx, id)
	if err != nil {
		fm.logger.Warn("read durable artifact pin failed; not collecting", "snapshot", id, "err", err)
		return true
	}
	return pinned
}

// sendCollectSnapshot hands a snapshot retention gc marked deleting to
// restate, which retries its deletion under the Environment policy. the key
// is the marking's own time, so every sweep that finds the same marking sends
// the same key and restate runs it once.
func (fm *FleetManager) sendCollectSnapshot(ctx context.Context, d DurableSnapshots, snapshot SnapshotRecord) {
	key := "collect:" + snapshot.SnapshotID + ":" + strconv.FormatInt(snapshot.UpdatedAt.UnixNano(), 10)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.SendCollectSnapshot(sctx, snapshot.VMID, snapshot.SnapshotID, key); err != nil {
		fm.logger.Warn("send snapshot gc failed, the next sweep retries", "snapshot", snapshot.SnapshotID, "err", err)
	}
}

// the methods below are the steps of durable snapshot gc.

// SnapshotCollect deletes a snapshot retention gc marked deleting: its host
// artifact, then its row. a row that is gone or no longer deleting was dealt
// with already, and a pinned one is handed back. a host that no longer has
// the artifact is not an error.
func (fm *FleetManager) SnapshotCollect(ctx context.Context, snapshotID string) error {
	record, found, err := fm.lookupSnapshotRecord(ctx, snapshotID)
	if err != nil {
		return err
	}
	if !found || record.State != SnapshotStateDeleting {
		return nil
	}
	// the collection may have queued behind other ops on the vm while a move
	// pinned the snapshot. a pinned one goes back to ready, and a later sweep
	// marks it again once the pin is released.
	if fm.snapshotPinned(ctx, snapshotID) {
		record.State = SnapshotStateReady
		record.UpdatedAt = time.Now()
		if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
			return fmt.Errorf("unmark pinned snapshot %s: %w", snapshotID, err)
		}
		fm.logger.InfoContext(ctx, "snapshot gc skipped a pinned snapshot", "snapshot", snapshotID)
		return nil
	}
	if err := fm.deleteSnapshotArtifact(ctx, record); err != nil && !IsNotFound(err) {
		return err
	}
	if fm.store != nil {
		if err := fm.store.DeleteSnapshot(ctx, snapshotID); err != nil {
			return fmt.Errorf("delete snapshot %s metadata: %w", snapshotID, err)
		}
	}
	fm.appendEvent(ctx, "vm", record.VMID, "snapshot.gc_deleted", map[string]any{"snapshot_id": snapshotID})
	return nil
}

// SnapshotCollectFailed records a gc deletion that ran out of retries: the
// row moves to error with the last error, and a later sweep marks it again.
func (fm *FleetManager) SnapshotCollectFailed(ctx context.Context, snapshotID, lastErr string) error {
	record, found, err := fm.lookupSnapshotRecord(ctx, snapshotID)
	if err != nil {
		return err
	}
	if !found || record.State != SnapshotStateDeleting {
		return nil
	}
	record.State = SnapshotStateError
	record.LastError = lastErr
	record.UpdatedAt = time.Now()
	if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
		return fmt.Errorf("mark snapshot %s error after a failed gc: %w", snapshotID, err)
	}
	fm.appendEvent(ctx, "vm", record.VMID, "snapshot.gc_failed", map[string]any{
		"snapshot_id": snapshotID,
		"error":       lastErr,
	})
	return nil
}
