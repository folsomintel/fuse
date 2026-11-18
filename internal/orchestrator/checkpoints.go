package orchestrator

import (
	"context"
	"sort"
	"time"
)

// checkpointChain mirrors one vm's background checkpoint chain, whose state
// lives on its restate Environment object (see MirrorChain): the host its
// merged base sits on (standby) and how big that base is. the standby picker
// and the migrate target picker read it under fm.chainsMu.
type checkpointChain struct {
	standby string
	// bytes is what the base takes on the standby's disk: the size of the
	// full checkpoint it started as. a merge rewrites the base at the same
	// size, so it is not updated per tick (the merged record only carries
	// the delta that moved).
	bytes int64
	// pending is set while a tick's copy to the standby runs.
	pending bool
}

// standbyUsage is what one host holds as a checkpoint standby.
type standbyUsage struct {
	copies int
	bytes  int64
}

// standbyCopy is one chain's base: the host it sits on and its size.
type standbyCopy struct {
	host  string
	bytes int64
}

// standbyCopies lists every chain's standby copy by vm id.
func (fm *FleetManager) standbyCopies() map[string]standbyCopy {
	fm.chainsMu.Lock()
	defer fm.chainsMu.Unlock()
	out := make(map[string]standbyCopy)
	for id, c := range fm.chains {
		if c.standby != "" {
			out[id] = standbyCopy{host: c.standby, bytes: c.bytes}
		}
	}
	return out
}

// usageExcept sums copies by host, leaving out vmID's own: a vm's own copy
// becomes its disk when it migrates there, so it is already covered by the
// spec the host has to fit.
func usageExcept(copies map[string]standbyCopy, vmID string) map[string]standbyUsage {
	out := make(map[string]standbyUsage)
	for id, c := range copies {
		if id == vmID {
			continue
		}
		u := out[c.host]
		u.copies++
		u.bytes += c.bytes
		out[c.host] = u
	}
	return out
}

// standbyUsageExcept is usageExcept over the live chains.
func (fm *FleetManager) standbyUsageExcept(vmID string) map[string]standbyUsage {
	return usageExcept(fm.standbyCopies(), vmID)
}

// copyEstimate is how big a full live checkpoint of spec will be before one
// has been taken: its disk plus its memory image.
func copyEstimate(spec Spec) int64 {
	return int64(spec.StorageGB)<<30 + int64(spec.RamMB)<<20
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
			fm.checkpointTickDurable(ctx, fm.durableSnapshotsClient())
		}
	}
}

// checkpointCandidates are the running vms whose environment can take diff
// snapshots: firecracker, no gpu, placed on a host. a vm restate is migrating
// is left out: its tick would only queue behind the migrate on its object.
func (fm *FleetManager) checkpointCandidates() []string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	var out []string
	for id, v := range fm.vms {
		if v.state != VMStateRunning || v.hostID == "" || v.spec.GPUs > 0 || v.env == nil {
			continue
		}
		if fm.migrating[id] > 0 {
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

// pickStandby chooses where vmID's checkpoints are kept: another active host
// running the same backend that could place the vm if it migrated there, with
// the standby copies other vms keep on it counted against its storage. among
// those it prefers the host holding the fewest standby copies, so one host
// does not end up as the landing spot for every vm that has to move at once
// (a drained source sends all its vms to their standbys together). ties go
// to the lowest id, so the choice is stable from one tick to the next. ""
// when the fleet has no such host.
//
// a host is also passed over when the new copy would crowd out a vm whose
// standby is already there: that vm's next re-check would fail and reset its
// chain with nothing new scheduled on the host.
func (fm *FleetManager) pickStandby(vmID, srcHostID string) string {
	copies := fm.standbyCopies()
	usage := usageExcept(copies, vmID)
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	src, ok := fm.hosts[srcHostID]
	v, vok := fm.vms[vmID]
	if !ok || !vok {
		return ""
	}
	spec := v.spec
	spec.HostID = ""
	ids := make([]string, 0, len(fm.hosts))
	for id := range fm.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	best := ""
	for _, id := range ids {
		h := fm.hosts[id]
		if id == srcHostID || h.URL == "" || h.Token == "" {
			continue
		}
		if h.Backend != src.Backend || (hostArch(src) != "" && hostArch(h) != hostArch(src)) {
			continue
		}
		if standbyRejection(h, spec, usage[id]) != "" {
			continue
		}
		if fm.crowdsOutLocked(h, vmID, copyEstimate(spec), copies) {
			continue
		}
		if best == "" || usage[id].copies < usage[best].copies {
			best = id
		}
	}
	return best
}

// crowdsOutLocked reports whether adding vmID's copy of size bytes to h would
// make any other vm with a standby copy on h fail its re-check. the caller
// holds fm.mu.
func (fm *FleetManager) crowdsOutLocked(h *Host, vmID string, bytes int64, copies map[string]standbyCopy) bool {
	for otherID, c := range copies {
		if otherID == vmID || c.host != h.ID {
			continue
		}
		other, ok := fm.vms[otherID]
		if !ok {
			continue
		}
		held := usageExcept(copies, otherID)[h.ID]
		held.copies++
		held.bytes += bytes
		spec := other.spec
		spec.HostID = ""
		if standbyRejection(h, spec, held) != "" {
			return true
		}
	}
	return false
}

// standbyRejection is hostRejection with the standby copies a host already
// holds charged to its storage: a host whose disk is spoken for by other vms'
// checkpoints cannot take this one if it has to migrate. "" if it can.
func standbyRejection(h *Host, spec Spec, held standbyUsage) string {
	hc := *h
	hc.Allocated.StorageGB += bytesToGB(held.bytes)
	return hostRejection(&hc, spec)
}

// bytesToGB rounds up, so a partial gigabyte still counts as used.
func bytesToGB(n int64) int {
	if n <= 0 {
		return 0
	}
	const gb = 1 << 30
	return int((n + gb - 1) / gb)
}

// standbyRejectionFor says why hostID can no longer serve as vmID's standby,
// by the same rule pickStandby used to pick it, or "" if it still can.
func (fm *FleetManager) standbyRejectionFor(vmID, hostID string) string {
	usage := fm.standbyUsageExcept(vmID)
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	h, ok := fm.hosts[hostID]
	if !ok {
		return "is gone"
	}
	v, ok := fm.vms[vmID]
	if !ok {
		return ""
	}
	spec := v.spec
	spec.HostID = ""
	return standbyRejection(h, spec, usage[hostID])
}
