package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ArtifactMoveJob is one planned host-to-host artifact move, and the input of
// the durable Artifacts/{local_id}.Move handler. it names hosts by id only:
// their agent tokens are looked up inside the pull step, so no secret reaches
// the restate journal.
type ArtifactMoveJob struct {
	// SnapshotID is the record being moved, Digest its rootfs digest.
	SnapshotID string `json:"snapshot_id"`
	Digest     string `json:"digest"`
	// SourceHostID serves the bytes, stored there as SourceSnapshotID.
	SourceHostID     string `json:"source_host_id"`
	SourceSnapshotID string `json:"source_snapshot_id"`
	// HostID receives them and commits them as LocalID.
	HostID  string `json:"host_id"`
	LocalID string `json:"local_id"`

	Files map[string]string `json:"files,omitempty"`
	Lazy  bool              `json:"lazy,omitempty"`
	// Base makes this a delta move merged onto Base on HostID, see
	// moveDeltaToHost.
	Base     string `json:"base,omitempty"`
	DropBase bool   `json:"drop_base,omitempty"`

	// Notify names the vm whose checkpoint chain waits on this move. its end
	// is sent to Environment/{Notify}.ChainMoved instead of being awaited,
	// so the chain does not hold the vm's object for the whole copy.
	Notify string `json:"notify,omitempty"`

	// what the copy's record takes from the moved record.
	TenantID  string       `json:"tenant_id"`
	LayerKey  string       `json:"layer_key,omitempty"`
	Arch      string       `json:"arch,omitempty"`
	Kind      SnapshotKind `json:"kind,omitempty"`
	SizeBytes int64        `json:"size_bytes,omitempty"`
}

// PinIDs is every snapshot id the move reads or writes, which gc must leave
// alone while it runs.
func (j ArtifactMoveJob) PinIDs() []string {
	ids := []string{j.SnapshotID}
	for _, id := range []string{j.SourceSnapshotID, j.LocalID, j.Base} {
		if id != "" && !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ErrArtifactMoveUnsettled is a durable move the caller stopped waiting on
// without a result: the wait timed out or lost its connection to restate. the
// move may still be running, so nothing it reads or writes may be cleaned up
// on the strength of this error.
var ErrArtifactMoveUnsettled = errors.New("artifact move still running")

// DurableArtifacts runs artifact moves as the restate Artifacts/{local_id}.Move
// handler and keeps pull pins in restate state, so neither is lost when the
// orchestrator restarts. *durable.Client satisfies it.
type DurableArtifacts interface {
	// MoveArtifact runs the move and waits for it, returning the snapshot id
	// committed on the destination. the move keeps running if ctx ends first;
	// an error that is not the move's own failure wraps
	// ErrArtifactMoveUnsettled.
	MoveArtifact(ctx context.Context, job ArtifactMoveJob) (string, error)
	// PinArtifacts holds ids against gc for holder until ReleaseArtifacts, or
	// until ttl passes.
	PinArtifacts(ctx context.Context, holder string, ttl time.Duration, ids ...string) error
	ReleaseArtifacts(ctx context.Context, holder string, ids ...string) error
	// ArtifactPinned reports whether anything holds a pin on id.
	ArtifactPinned(ctx context.Context, id string) (bool, error)
}

// ArtifactMoveMetrics is an optional extension of ReconcileMetrics, recording
// each attempt at a host-to-host artifact copy.
type ArtifactMoveMetrics interface {
	ArtifactMoveCompleted(sourceHostID, targetHostID string, bytes int64, d time.Duration, err error)
}

// SetDurableArtifacts routes artifact moves and pull pins through restate. nil
// (mode off, the default) keeps them in process. set it before Start.
func (fm *FleetManager) SetDurableArtifacts(d DurableArtifacts) {
	fm.mu.Lock()
	fm.durableArtifacts = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableArtifactsClient() DurableArtifacts {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableArtifacts
}

// durableMoveWait bounds how long a caller with no deadline of its own waits
// on a durable move.
const durableMoveWait = time.Hour

// runArtifactMove carries out a planned move: on restate when it is
// configured, otherwise in process holding in-memory pins. wrap turns a failed
// copy into the caller's error; a failed record write in process is returned
// as itself, as before.
func (fm *FleetManager) runArtifactMove(ctx context.Context, job ArtifactMoveJob, wrap func(error) error) (string, error) {
	if d := fm.durableArtifactsClient(); d != nil {
		waitCtx := ctx
		if _, ok := ctx.Deadline(); !ok {
			// a background caller (the checkpoint loop) would otherwise wait
			// forever on a restate that accepts and never answers. giving up
			// leaves the move running; its copy is found on the next try.
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, durableMoveWait)
			defer cancel()
		}
		id, err := d.MoveArtifact(waitCtx, job)
		if err != nil {
			return "", wrap(err)
		}
		// a fresh use keeps the copy out of the next sweep, as the in-process
		// release does.
		fm.touchArtifact(job.PinIDs()...)
		return id, nil
	}
	ids := job.PinIDs()
	fm.beginArtifactPull(ids...)
	defer fm.endArtifactPull(ids...)
	moved, err := fm.ArtifactPull(ctx, job)
	if err != nil {
		return "", wrap(err)
	}
	return fm.ArtifactRecord(ctx, job, moved)
}

// holdArtifact pins id against gc for holder and returns its release. on
// restate the pin outlives a restart and lapses after ttl if nothing releases
// it; a pin restate refuses falls back to an in-memory one.
func (fm *FleetManager) holdArtifact(ctx context.Context, holder string, ttl time.Duration, id string) (release func()) {
	if d := fm.durableArtifactsClient(); d != nil {
		if err := d.PinArtifacts(ctx, holder, ttl, id); err == nil {
			return func() {
				rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := d.ReleaseArtifacts(rctx, holder, id); err != nil {
					fm.logger.Warn("release durable artifact pin failed; it lapses on its timer", "holder", holder, "snapshot", id, "err", err)
				}
			}
		} else {
			fm.logger.Warn("durable artifact pin failed; holding it in memory", "holder", holder, "snapshot", id, "err", err)
		}
	}
	fm.beginArtifactPull(id)
	return func() { fm.endArtifactPull(id) }
}

// dropDurablyPinned removes from collect every artifact a durable pin holds.
// a pin that cannot be read counts as held: gc waits for the next sweep
// rather than deleting something a move may be writing.
func (fm *FleetManager) dropDurablyPinned(ctx context.Context, collect []SnapshotRecord) []SnapshotRecord {
	d := fm.durableArtifactsClient()
	if d == nil {
		return collect
	}
	out := collect[:0]
	for _, record := range collect {
		pinned, err := d.ArtifactPinned(ctx, record.SnapshotID)
		if err != nil {
			fm.logger.Warn("read durable artifact pin failed; not collecting", "snapshot", record.SnapshotID, "err", err)
			continue
		}
		if pinned {
			continue
		}
		out = append(out, record)
	}
	return out
}

// the methods below are the steps of a move, in order. the durable handler
// runs each inside a journaled restate step; the in-process path calls them
// directly. each must be safe to run again.

// ArtifactPullTimeout bounds one attempt at a pull, from
// ORCH_ARTIFACT_PULL_TIMEOUT_SECONDS. zero leaves the agent client's own
// request timeout as the bound.
func (fm *FleetManager) ArtifactPullTimeout() time.Duration {
	return fm.artifactPullTimeout
}

// ArtifactMoveCommitted returns the id a ready copy of the job is already
// recorded under on its destination, or "". a move that finds one stops
// there: a concurrent move to the same host got there first.
func (fm *FleetManager) ArtifactMoveCommitted(ctx context.Context, job ArtifactMoveJob) (string, error) {
	if job.Lazy {
		// a lazy copy is never recorded.
		return "", nil
	}
	record, found, err := fm.lookupSnapshotRecord(ctx, job.LocalID)
	if err != nil || !found {
		return "", err
	}
	if record.HostID == job.HostID && record.State == SnapshotStateReady {
		return record.SnapshotID, nil
	}
	return "", nil
}

// lookupSnapshotRecord reads a snapshot record, telling a missing row
// (found false, nil error) apart from a failed read (an error).
func (fm *FleetManager) lookupSnapshotRecord(ctx context.Context, snapshotID string) (SnapshotRecord, bool, error) {
	if fm.store == nil {
		return SnapshotRecord{}, false, nil
	}
	record, err := fm.store.GetSnapshot(ctx, snapshotID)
	if errors.Is(err, ErrSnapshotNotFound) {
		return SnapshotRecord{}, false, nil
	}
	if err != nil {
		return SnapshotRecord{}, false, fmt.Errorf("read snapshot %s: %w", snapshotID, err)
	}
	return record, true, nil
}

// errArtifactKindMismatch is a destination that committed something other
// than what was sent: an agent too old for live snapshots or delta merges.
// retrying cannot change it.
var errArtifactKindMismatch = errors.New("artifact committed as the wrong kind")

// ArtifactPull has the destination fetch the bytes from the source. both
// agents' endpoints are read from the fleet here, not carried on the job.
func (fm *FleetManager) ArtifactPull(ctx context.Context, job ArtifactMoveJob) (ArtifactMoved, error) {
	if fm.artifactMover == nil {
		return ArtifactMoved{}, fmt.Errorf("%w: no artifact mover is configured", ErrArtifactImmovable)
	}
	from, err := fm.artifactEndpoint(job.SourceHostID)
	if err != nil {
		return ArtifactMoved{}, err
	}
	to, err := fm.artifactEndpoint(job.HostID)
	if err != nil {
		return ArtifactMoved{}, err
	}
	if fm.artifactPullTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fm.artifactPullTimeout)
		defer cancel()
	}

	start := time.Now()
	moved, err := fm.artifactMover.MoveArtifact(ctx, ArtifactMove{
		Digest:     job.Digest,
		SnapshotID: job.LocalID,
		From:       from,
		To:         to,
		Files:      job.Files,
		Lazy:       job.Lazy,

		SourceSnapshotID: job.SourceSnapshotID,
		Base:             job.Base,
		DropBase:         job.DropBase,
	})
	if err == nil && job.Base != "" && moved.Kind != SnapshotKindLive {
		err = fmt.Errorf("%w: host %s did not commit the merged delta as a live snapshot; its host agent cannot merge deltas",
			errArtifactKindMismatch, job.HostID)
	}
	if err == nil && job.Base == "" && job.Kind == SnapshotKindLive && moved.Kind != SnapshotKindLive {
		// the target took the rootfs and dropped the rest. nothing is
		// recorded, so nothing can seed from the half it committed.
		err = fmt.Errorf("%w: host %s committed only the rootfs of a live snapshot; its host agent cannot receive memory images",
			errArtifactKindMismatch, job.HostID)
	}
	if m, ok := fm.metrics.(ArtifactMoveMetrics); ok {
		m.ArtifactMoveCompleted(job.SourceHostID, job.HostID, moved.SizeBytes, time.Since(start), err)
	}
	if err != nil {
		return ArtifactMoved{}, err
	}
	return moved, nil
}

// artifactEndpoint is a registered host's agent address and token.
func (fm *FleetManager) artifactEndpoint(hostID string) (ArtifactEndpoint, error) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	h, ok := fm.hosts[hostID]
	if !ok {
		return ArtifactEndpoint{}, fmt.Errorf("%w: host %s is not registered", ErrArtifactImmovable, hostID)
	}
	if h.URL == "" || h.Token == "" {
		return ArtifactEndpoint{}, fmt.Errorf("%w: host %s has no usable agent url or token", ErrArtifactImmovable, hostID)
	}
	return ArtifactEndpoint{HostID: h.ID, URL: h.URL, Token: h.Token}, nil
}

// PermanentArtifactError reports whether a failed pull cannot succeed on a
// retry: the artifact cannot move at all, the destination committed the wrong
// kind, or an agent refused the request with a 4xx other than a timeout or
// rate limit.
func PermanentArtifactError(err error) bool {
	if errors.Is(err, ErrArtifactImmovable) || errors.Is(err, errArtifactKindMismatch) {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		code := statusErr.Code
		return code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
	}
	return false
}

// ArtifactRecord records the copy the destination committed, and returns its
// id. a lazy copy is not recorded: its agent deletes it once the vm resumed.
func (fm *FleetManager) ArtifactRecord(ctx context.Context, job ArtifactMoveJob, moved ArtifactMoved) (string, error) {
	committedID := moved.SnapshotID
	if committedID == "" {
		committedID = job.LocalID
	}
	if job.Lazy {
		// the target holds no memory image, so it can never be the source of
		// the next move or a seed for anything else, and its agent deletes the
		// seed once the vm has resumed. recording it would say otherwise.
		return committedID, nil
	}
	if job.Base != "" {
		return fm.recordMergedDelta(ctx, job, moved, committedID)
	}
	size := moved.SizeBytes
	if size <= 0 {
		size = job.SizeBytes
	}

	now := time.Now()
	replicaMetadata := map[string]string{
		"source_snapshot_id": job.SnapshotID,
		"source_host_id":     job.SourceHostID,
	}
	if len(job.Files) > 0 {
		// the copy can be a source for the next move, so it carries the digests.
		replicaMetadata = withLiveFiles(replicaMetadata, job.Files)
	}
	metadata, _ := marshalSnapshotMetadata("", replicaMetadata)
	replica := SnapshotRecord{
		SnapshotID: committedID,
		// No VM: this copy was not checkpointed from anything running here, it
		// arrived over the wire. Leaving VMID empty is what marks it as a
		// free-standing artifact rather than a vm's checkpoint.
		VMID:     "",
		HostID:   job.HostID,
		TenantID: job.TenantID,
		// ParentSnapshotID is deliberately NOT the source. Lineage means "this
		// snapshot descends from that one", and deleting a snapshot with
		// children is refused; pointing at the source would make every origin
		// artifact undeletable the moment anything pulled a copy of it.
		Mode:      SnapshotModeBuild,
		LayerKey:  job.LayerKey,
		Arch:      job.Arch,
		Digest:    job.Digest,
		Kind:      job.Kind,
		State:     SnapshotStateReady,
		SizeBytes: size,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := fm.upsertSnapshotRecord(ctx, replica); err != nil {
		// The bytes are on the host but nothing records them. Failing is the
		// consistent choice: the create that triggered this would fail at its
		// own next persist anyway, and the id is derived from (digest, host) so
		// the next attempt re-commits over the same directory rather than
		// leaving one orphan per try.
		return "", fmt.Errorf("persist pulled artifact %s on host %s: %w", committedID, job.HostID, err)
	}

	fm.appendEventBackground("host", job.HostID, "artifact.pulled", map[string]any{
		"digest":             job.Digest,
		"snapshot_id":        committedID,
		"source_host_id":     job.SourceHostID,
		"source_snapshot_id": job.SnapshotID,
		"size_bytes":         size,
	})
	return committedID, nil
}

// recordMergedDelta records the complete live snapshot a delta merge left on
// the destination, and forgets the base the merge dropped.
func (fm *FleetManager) recordMergedDelta(ctx context.Context, job ArtifactMoveJob, moved ArtifactMoved, committedID string) (string, error) {
	now := time.Now()
	replicaMetadata := map[string]string{
		"source_snapshot_id": job.SnapshotID,
		"source_host_id":     job.SourceHostID,
		"merged_onto":        job.Base,
	}
	if completeLiveFiles(moved.Files) {
		replicaMetadata = withLiveFiles(replicaMetadata, moved.Files)
	}
	metadata, _ := marshalSnapshotMetadata("", replicaMetadata)
	replica := SnapshotRecord{
		SnapshotID: committedID,
		HostID:     job.HostID,
		TenantID:   job.TenantID,
		// build mode keeps retention gc away from it: the code that made it
		// (a migrate or the checkpoint loop) is the code that removes it.
		Mode:      SnapshotModeBuild,
		Arch:      job.Arch,
		Digest:    job.Digest,
		Kind:      SnapshotKindLive,
		State:     SnapshotStateReady,
		SizeBytes: moved.SizeBytes,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := fm.upsertSnapshotRecord(ctx, replica); err != nil {
		return "", fmt.Errorf("persist merged delta %s on host %s: %w", committedID, job.HostID, err)
	}
	if job.DropBase {
		fm.forgetSnapshotRecord(ctx, job.Base)
	}
	return committedID, nil
}

// ArtifactDiscard deletes what a failed move may have left on the
// destination. a copy that did get recorded is kept: the record step can
// commit on an attempt whose answer was lost. a record that cannot be read
// fails the step, so the bytes stay until the row is known to be absent.
func (fm *FleetManager) ArtifactDiscard(ctx context.Context, job ArtifactMoveJob) error {
	record, found, err := fm.lookupSnapshotRecord(ctx, job.LocalID)
	if err != nil {
		return err
	}
	if found && record.HostID == job.HostID {
		return nil
	}
	if err := fm.deleteFreeArtifact(ctx, job.HostID, job.LocalID); err != nil && !IsNotFound(err) {
		return fmt.Errorf("delete partial copy %s on host %s: %w", job.LocalID, job.HostID, err)
	}
	fm.logger.InfoContext(ctx, "deleted partial artifact copy after a failed move", "snapshot", job.LocalID, "host", job.HostID)
	return nil
}
