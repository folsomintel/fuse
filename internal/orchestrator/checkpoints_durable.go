package orchestrator

import (
	"context"
	"time"
)

// chainHoldLease bounds how long a migrate keeps the checkpoint loop off its
// vm's chain on restate. a migrate that dies with the process leaves the hold
// to lapse instead of stopping the chain for good.
const chainHoldLease = time.Hour

// checkpointCallWait bounds how long a tick waits on one vm's checkpoint
// handler. the handler keeps running if the wait ends.
const checkpointCallWait = 10 * time.Minute

// ChainState is one vm's checkpoint chain as the restate Environment object
// keeps it, so a restart loses neither the chain nor what it holds on the
// standby. see checkpointChain for what each field is.
type ChainState struct {
	Standby string         `json:"standby,omitempty"`
	Head    SnapshotRecord `json:"head"`
	Base    string         `json:"base,omitempty"`
	Bytes   int64          `json:"bytes,omitempty"`
}

// Empty reports whether the chain holds nothing.
func (c ChainState) Empty() bool {
	return c.Head.SnapshotID == "" && c.Base == ""
}

// ChainPlan is what one checkpoint tick does, as ChainBegin settled it.
type ChainPlan struct {
	// Gone is a vm the fleet no longer tracks: its chain is retired.
	Gone bool `json:"gone,omitempty"`
	// Diff extends the chain: a diff against its head, merged onto its base
	// on Standby. otherwise any chain is retired, and a full checkpoint is
	// copied to Standby, or nothing is taken when Standby is "".
	Diff    bool            `json:"diff,omitempty"`
	Standby string          `json:"standby,omitempty"`
	Options SnapshotOptions `json:"options"`
}

// ChainMove is how a tick's checkpoint reaches the standby: a copy the
// standby already holds (Existing), or Job.
type ChainMove struct {
	Job      ArtifactMoveJob `json:"job"`
	Existing string          `json:"existing,omitempty"`
}

// ChainHold is what HoldChain hands a migrate.
type ChainHold struct {
	Busy bool `json:"busy,omitempty"`
	// Holder names the hold, for its release.
	Holder string     `json:"holder,omitempty"`
	Chain  ChainState `json:"chain"`
}

// ChainMoveResult is the end of a chain's move, sent by the Artifacts move
// to Environment/{vm_id}.ChainMoved: the committed id, or the move's error.
type ChainMoveResult struct {
	LocalID string `json:"local_id"`
	ID      string `json:"id,omitempty"`
	Err     string `json:"err,omitempty"`
}

// checkpointTickDurable is checkpointTick on restate: each candidate's tick
// runs as Environment/{vm_id}.Checkpoint, serialized with the vm's other
// lifecycle ops, and its chain lives in that object's state. vms still go one
// at a time: the next is not started until this one's copy to its standby
// has ended.
func (fm *FleetManager) checkpointTickDurable(ctx context.Context, d DurableSnapshots) {
	for _, vmID := range fm.checkpointCandidates() {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, checkpointCallWait)
		err := d.Checkpoint(cctx, vmID)
		cancel()
		if err != nil {
			fm.logger.Warn("durable checkpoint tick failed", "vm", vmID, "err", err)
			continue
		}
		fm.waitChainSettled(ctx, vmID)
	}

	// what the chain of a vm that is gone still holds is removed by its
	// object; a destroy on restate already did it, this catches the rest.
	fm.mu.RLock()
	fm.chainsMu.Lock()
	var gone []string
	for vmID := range fm.chains {
		if _, ok := fm.vms[vmID]; !ok {
			gone = append(gone, vmID)
		}
	}
	fm.chainsMu.Unlock()
	fm.mu.RUnlock()
	for _, vmID := range gone {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := d.SendRetireChain(sctx, vmID)
		cancel()
		if err != nil {
			fm.logger.Warn("send checkpoint chain retire failed, the next tick retries", "vm", vmID, "err", err)
			continue
		}
		fm.MirrorChain(vmID, ChainState{}, false)
	}
}

// waitChainSettled waits until vmID's chain has no copy in flight, ctx ends,
// or durableMoveWait passes.
func (fm *FleetManager) waitChainSettled(ctx context.Context, vmID string) {
	deadline := time.NewTimer(durableMoveWait)
	defer deadline.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for fm.chainPending(vmID) {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-poll.C:
		}
	}
}

func (fm *FleetManager) chainPending(vmID string) bool {
	fm.chainsMu.Lock()
	defer fm.chainsMu.Unlock()
	c, ok := fm.chains[vmID]
	return ok && c.pending
}

// MirrorChain copies a chain's durable state into fm.chains, which the
// standby picker and migrate target choice read. it is not journaled: the
// handlers call it on every replay, so it always ends at the object's state.
func (fm *FleetManager) MirrorChain(vmID string, chain ChainState, pending bool) {
	fm.chainsMu.Lock()
	defer fm.chainsMu.Unlock()
	if chain.Empty() && chain.Standby == "" && !pending {
		delete(fm.chains, vmID)
		return
	}
	c, ok := fm.chains[vmID]
	if !ok {
		c = &checkpointChain{}
		fm.chains[vmID] = c
	}
	c.standby, c.bytes, c.head, c.base, c.pending = chain.Standby, chain.Bytes, chain.Head, chain.Base, pending
}

// the methods below are the steps of the durable checkpoint chain. each runs
// inside a journaled restate step and must be safe to run again.

// ChainBegin settles what this tick does with vmID's chain, by the rules
// advanceChain follows in process.
func (fm *FleetManager) ChainBegin(ctx context.Context, vmID string, chain ChainState) (ChainPlan, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	srcHostID := ""
	if ok {
		srcHostID = v.hostID
	}
	fm.mu.RUnlock()
	if !ok {
		return ChainPlan{Gone: true}, nil
	}
	retention := time.Now().Add(4 * fm.checkpointInterval)

	keep := chain.Base != "" && chain.Standby != "" && chain.Standby != srcHostID
	if keep {
		if reason := fm.standbyRejectionFor(vmID, chain.Standby); reason != "" {
			fm.logger.InfoContext(ctx, "checkpoint standby no longer fits; re-picking", "vm", vmID, "host", chain.Standby, "reason", reason)
			fm.appendEventBackground("vm", vmID, "vm.checkpoint_chain_reset", map[string]any{
				"error": "standby host " + chain.Standby + " " + reason, "standby_host_id": chain.Standby,
			})
			keep = false
		}
	}
	if keep {
		return ChainPlan{Diff: true, Standby: chain.Standby, Options: SnapshotOptions{
			Comment: "checkpoint", Mode: SnapshotModeAuto, RetentionUntil: &retention,
			Live: true, Diff: true, Parent: chain.Head.SnapshotID, NoLineage: true,
		}}, nil
	}
	return ChainPlan{Standby: fm.pickStandby(vmID, srcHostID), Options: SnapshotOptions{
		Comment: "checkpoint", Mode: SnapshotModeAuto, RetentionUntil: &retention,
		Live: true, NoLineage: true,
	}}, nil
}

// ChainMovePlan works out how the tick's checkpoint reaches the standby: a
// delta merged onto the chain's base, or a full copy.
func (fm *FleetManager) ChainMovePlan(ctx context.Context, chain ChainState, plan ChainPlan, child SnapshotRecord) (ChainMove, error) {
	if plan.Diff {
		job, err := fm.planDeltaMove(child, chain.Base, plan.Standby, true)
		return ChainMove{Job: job}, err
	}
	job, existing, err := fm.planArtifactMove(ctx, child, plan.Standby, false)
	return ChainMove{Job: job, Existing: existing}, err
}

// ChainRetire removes what a chain holds: its base on the standby, with its
// record, and its head on the source. failures are logged, never returned:
// what is left is retention's or artifact gc's.
func (fm *FleetManager) ChainRetire(ctx context.Context, vmID string, chain ChainState) error {
	if chain.Base != "" {
		if err := fm.deleteFreeArtifact(ctx, chain.Standby, chain.Base); err != nil && !IsNotFound(err) {
			fm.logger.WarnContext(ctx, "delete stale checkpoint base failed", "vm", vmID, "host", chain.Standby, "snapshot", chain.Base, "err", err)
		}
		fm.forgetSnapshotRecord(ctx, chain.Base)
	}
	fm.deleteCheckpoint(ctx, chain.Head)
	return nil
}

// ChainReset records why a chain was retired.
func (fm *FleetManager) ChainReset(ctx context.Context, vmID, reason string) error {
	fm.logger.InfoContext(ctx, "checkpoint chain reset", "vm", vmID, "reason", reason)
	fm.appendEvent(ctx, "vm", vmID, "vm.checkpoint_chain_reset", map[string]any{"error": reason})
	return nil
}

// ChainAdvanced records a chain that took a step: its head is the new
// checkpoint and its base the copy on the standby.
func (fm *FleetManager) ChainAdvanced(ctx context.Context, vmID string, chain ChainState, diff bool) error {
	fm.touchArtifact(chain.Head.SnapshotID, chain.Base)
	fm.appendEvent(ctx, "vm", vmID, "vm.checkpointed", map[string]any{
		"snapshot_id": chain.Head.SnapshotID, "standby_host_id": chain.Standby, "base": chain.Base, "diff": diff,
	})
	return nil
}

// holdDurableChain holds vmID's chain on restate for a delta migrate and
// returns a copy of it and the hold's id, or nil when it cannot be held:
// restate refused, a tick's copy is still running, or another migrate holds
// it. a migrate without the hold takes a full seed
// instead, which only costs the chain its next diff.
func (fm *FleetManager) holdDurableChain(ctx context.Context, d DurableSnapshots, vmID string) (*checkpointChain, string) {
	hold, err := d.HoldChain(ctx, vmID, chainHoldLease)
	if err != nil {
		fm.logger.WarnContext(ctx, "hold checkpoint chain failed; migrating without it", "vm", vmID, "err", err)
		return nil, ""
	}
	if hold.Busy {
		fm.logger.InfoContext(ctx, "checkpoint chain busy (a copy in flight or another migrate); migrating without it", "vm", vmID)
		return nil, ""
	}
	c := hold.Chain
	return &checkpointChain{standby: c.Standby, head: c.Head, base: c.Base, bytes: c.Bytes}, hold.Holder
}

// releaseDurableChain ends a migrate's hold. reset forgets the chain, whose
// base the migrate consumed or put out of step.
func (fm *FleetManager) releaseDurableChain(d DurableSnapshots, vmID, holder string, reset bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.ReleaseChain(ctx, vmID, holder, reset); err != nil {
		fm.logger.Warn("release checkpoint chain failed; the hold lapses on its lease", "vm", vmID, "err", err)
	}
}
