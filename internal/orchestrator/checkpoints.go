package orchestrator

import (
	"context"
	"sort"
	"sync"
	"time"
)

// checkpointChain is one vm's background checkpoints: the last live snapshot
// taken on the source (head) and a complete merged copy of it kept on another
// host (base on standby). each tick takes a diff against head, ships only
// that, and merges it onto base, so a later migrate to standby only has to
// move what changed since the last tick.
//
// mu serializes everything that takes a live snapshot of the vm through the
// chain: a tick, and a migrate. any other live snapshot in between resets the
// host's dirty bitmap, and the next diff is refused rather than wrong.
type checkpointChain struct {
	mu      sync.Mutex
	standby string
	head    SnapshotRecord
	base    string
}

func (fm *FleetManager) chainFor(vmID string) *checkpointChain {
	fm.chainsMu.Lock()
	defer fm.chainsMu.Unlock()
	c, ok := fm.chains[vmID]
	if !ok {
		c = &checkpointChain{}
		fm.chains[vmID] = c
	}
	return c
}

// dropChain forgets a vm's chain once the vm is gone. the caller holds c.mu.
func (fm *FleetManager) dropChain(vmID string, c *checkpointChain) {
	fm.resetChainLocked(c)
	fm.chainsMu.Lock()
	if fm.chains[vmID] == c {
		delete(fm.chains, vmID)
	}
	fm.chainsMu.Unlock()
}

// resetChainLocked empties a chain so the next tick starts a fresh one, and
// releases the pin on its base. the caller holds c.mu. it deletes nothing:
// a base that was consumed is already gone, and one that was not is removed
// by retireChain.
func (fm *FleetManager) resetChainLocked(c *checkpointChain) {
	if c.base != "" {
		fm.endArtifactPull(c.base)
	}
	c.standby, c.head, c.base = "", SnapshotRecord{}, ""
}

// retireChain removes what a broken chain still holds, both ends, then
// resets it. the caller holds c.mu.
func (fm *FleetManager) retireChain(ctx context.Context, vmID string, c *checkpointChain) {
	base, standby, head := c.base, c.standby, c.head
	fm.resetChainLocked(c)
	if base != "" {
		if err := fm.deleteFreeArtifact(ctx, standby, base); err != nil && !IsNotFound(err) {
			fm.logger.Warn("delete stale checkpoint base failed", "vm", vmID, "host", standby, "snapshot", base, "err", err)
		}
		fm.forgetSnapshotRecord(ctx, base)
	}
	fm.deleteCheckpoint(ctx, head)
}

// deleteCheckpoint removes one source-side checkpoint the chain no longer
// needs. failures are logged and left to retention: the next diff only needs
// the newest head, never an older one.
func (fm *FleetManager) deleteCheckpoint(ctx context.Context, record SnapshotRecord) {
	if record.SnapshotID == "" {
		return
	}
	if err := fm.deleteSnapshotRecord(ctx, record); err != nil && !IsNotFound(err) {
		fm.logger.Warn("delete superseded checkpoint failed", "vm", record.VMID, "snapshot", record.SnapshotID, "err", err)
	}
}

func (fm *FleetManager) checkpointLoop(ctx context.Context) {
	tick := time.NewTicker(fm.checkpointInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			fm.checkpointTick(ctx)
		}
	}
}

// checkpointTick advances every eligible vm's chain by one step. vms are
// visited one at a time: a live snapshot pauses its guest, and the transfer
// that follows competes with real traffic, so this stays a trickle.
func (fm *FleetManager) checkpointTick(ctx context.Context) {
	for _, vmID := range fm.checkpointCandidates() {
		if ctx.Err() != nil {
			return
		}
		fm.advanceChain(ctx, vmID)
	}

	// chains of vms that are gone keep pins and bytes; drop them.
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
		c := fm.chainFor(vmID)
		if c.mu.TryLock() {
			fm.retireChain(ctx, vmID, c)
			fm.dropChain(vmID, c)
			c.mu.Unlock()
		}
	}
}

// checkpointCandidates are the running vms whose environment can take diff
// snapshots: firecracker, no gpu, placed on a host.
func (fm *FleetManager) checkpointCandidates() []string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	var out []string
	for id, v := range fm.vms {
		if v.state != VMStateRunning || v.hostID == "" || v.spec.GPUs > 0 || v.env == nil {
			continue
		}
		if _, ok := v.env.(DeltaSnapshotCapable); !ok {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// pickStandby chooses where a vm's checkpoints are kept: the first other
// active host running the same backend, by id, so the choice is stable from
// one tick to the next. "" when the fleet has no such host.
func (fm *FleetManager) pickStandby(srcHostID string) string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	src, ok := fm.hosts[srcHostID]
	if !ok {
		return ""
	}
	ids := make([]string, 0, len(fm.hosts))
	for id := range fm.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h := fm.hosts[id]
		if id == srcHostID || !h.schedulable() || h.URL == "" || h.Token == "" {
			continue
		}
		if h.Backend != src.Backend || (hostArch(src) != "" && hostArch(h) != hostArch(src)) {
			continue
		}
		return id
	}
	return ""
}

// advanceChain takes one step for vmID: a full checkpoint copied whole to a
// standby host if the chain is empty, otherwise a diff against head merged
// onto the standby's base. any failure retires the chain, and the next tick
// starts over with a full one; a chain is only ever extended from a state
// both ends agree on.
func (fm *FleetManager) advanceChain(ctx context.Context, vmID string) {
	c := fm.chainFor(vmID)
	if !c.mu.TryLock() {
		// a migrate of this vm is in progress.
		return
	}
	defer c.mu.Unlock()

	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	srcHostID := ""
	if ok {
		srcHostID = v.hostID
	}
	fm.mu.RUnlock()
	if !ok {
		return
	}
	retention := time.Now().Add(4 * fm.checkpointInterval)

	if c.base != "" && c.standby != "" && c.standby != srcHostID && fm.hostSchedulable(c.standby) {
		child, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{
			Comment: "checkpoint", Mode: SnapshotModeAuto, RetentionUntil: &retention,
			Live: true, Diff: true, Parent: c.head.SnapshotID, NoLineage: true,
		})
		if err != nil {
			fm.logger.Info("checkpoint chain reset", "vm", vmID, "reason", err)
			fm.appendEventBackground("vm", vmID, "vm.checkpoint_chain_reset", map[string]any{"error": err.Error()})
			fm.retireChain(ctx, vmID, c)
			return
		}
		merged, err := fm.moveDeltaToHost(ctx, child, c.base, c.standby, true)
		if err != nil {
			fm.logger.Warn("move checkpoint delta failed; chain reset", "vm", vmID, "host", c.standby, "err", err)
			fm.appendEventBackground("vm", vmID, "vm.checkpoint_chain_reset", map[string]any{"error": err.Error()})
			// the base may or may not have been merged and dropped; either way
			// the chain no longer has an agreed state.
			fm.deleteCheckpoint(ctx, child)
			fm.retireChain(ctx, vmID, c)
			return
		}
		fm.endArtifactPull(c.base)
		fm.beginArtifactPull(merged)
		old := c.head
		c.head, c.base = child, merged
		fm.deleteCheckpoint(ctx, old)
		fm.appendEventBackground("vm", vmID, "vm.checkpointed", map[string]any{
			"snapshot_id": child.SnapshotID, "standby_host_id": c.standby, "base": merged, "diff": true,
		})
		return
	}

	if c.head.SnapshotID != "" || c.base != "" {
		fm.retireChain(ctx, vmID, c)
	}
	standby := fm.pickStandby(srcHostID)
	if standby == "" {
		return
	}
	full, err := fm.CreateSnapshot(ctx, vmID, SnapshotOptions{
		Comment: "checkpoint", Mode: SnapshotModeAuto, RetentionUntil: &retention,
		Live: true, NoLineage: true,
	})
	if err != nil {
		fm.logger.Warn("full checkpoint failed", "vm", vmID, "err", err)
		return
	}
	base, err := fm.ensureArtifactOnHost(ctx, full, standby, false)
	if err != nil {
		fm.logger.Warn("copy full checkpoint to standby failed", "vm", vmID, "host", standby, "err", err)
		fm.deleteCheckpoint(ctx, full)
		return
	}
	fm.beginArtifactPull(base)
	c.standby, c.head, c.base = standby, full, base
	fm.appendEventBackground("vm", vmID, "vm.checkpointed", map[string]any{
		"snapshot_id": full.SnapshotID, "standby_host_id": standby, "base": base, "diff": false,
	})
}

func (fm *FleetManager) hostSchedulable(hostID string) bool {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	h, ok := fm.hosts[hostID]
	return ok && h.schedulable()
}
