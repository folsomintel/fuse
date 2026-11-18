package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrLiveMigrateRefused is returned when a live migrate cannot be done as
// asked: no distinct target host, or the target host agent declined to resume
// the guest (different cpu or firecracker build, or the guest's network slot is
// taken there). the source vm is untouched and still running, which is what
// makes this a conflict to report rather than a failure to clean up after.
var ErrLiveMigrateRefused = errors.New("live migrate refused")

// MigrateOptions tunes a MigrateVM call. all fields are optional.
type MigrateOptions struct {
	// TargetHostID is the host to migrate to. empty means the orchestrator
	// picks a host other than the source's, preferring the vm's checkpoint
	// standby. a vm with no host (single-provider mode) stays where it is.
	TargetHostID string

	// Live carries the guest's memory across and resumes it on the target, so
	// processes survive the move, instead of cold-booting a disk snapshot.
	//
	// it is opt-in and never falls back to a cold migration. the seed it takes
	// is a live snapshot, whose rootfs is not bootable without its memory, so
	// there is nothing to fall back onto; and a caller who asked for processes
	// to survive should hear that they would not, while the source is still
	// running, rather than find out afterwards. it needs a target other than
	// the source's host, the same cpu and firecracker build on both, and the
	// source's network slot free on the target (the frozen guest keeps its
	// addresses and nothing remaps them yet).
	Live bool

	// Lazy resumes the guest on the target before its memory has arrived and
	// pages it in from the source afterwards, so the move does not wait on a
	// full copy of the memory image. it needs Live and a vm booted with
	// HugePages. until the target reports every page resident the guest still
	// depends on the source's seed snapshot, which stays pinned until then;
	// if the source host is lost first, the guest is lost with it.
	Lazy bool
}

// pickMigrateTarget chooses a host other than the source for a migrate that
// named none. the vm's checkpoint standby comes first when it still qualifies,
// since it already holds the vm's base and a live migrate there skips the bulk
// copy. it never falls back to the source: a caller moving a vm off a failing
// host must not get a same-host migrate instead.
func (fm *FleetManager) pickMigrateTarget(vmID, srcHostID string, srcBackend HostBackend, srcSpec Spec) (string, error) {
	fm.mu.RLock()
	hosts := fm.activeHostsLocked()
	fm.mu.RUnlock()

	// a rootfs prepared under one backend is not portable to another.
	eligible := make([]*Host, 0, len(hosts))
	for _, h := range hosts {
		if h.ID != srcHostID && h.Backend == srcBackend {
			eligible = append(eligible, h)
		}
	}

	spec := srcSpec
	spec.HostID = ""

	var standby string
	fm.chainsMu.Lock()
	if c, ok := fm.chains[vmID]; ok {
		standby = c.standby
	}
	fm.chainsMu.Unlock()
	if h := hostByID(eligible, standby); h != nil && hostRejection(h, spec) == "" {
		return standby, nil
	}

	h, _, err := SchedulePreferring(spec, eligible, fm.placementPolicy, PlacementHints{})
	if err != nil {
		return "", fmt.Errorf("no host other than %s can take vm %s: %w", srcHostID, vmID, err)
	}
	return h.ID, nil
}

// MigrateVM moves vmID to another host and returns the new vm's id.
func (fm *FleetManager) MigrateVM(ctx context.Context, vmID string, opts MigrateOptions) (string, error) {
	op := "migrate"
	if opts.Live {
		op = "migrate_live"
	}
	ctx, end := fm.startOperation(ctx, op, vmID)
	id, err := fm.migrateVM(ctx, vmID, opts)
	end(err)
	return id, err
}

func (fm *FleetManager) migrateVM(ctx context.Context, vmID string, opts MigrateOptions) (string, error) {
	return fm.durableMigrateVM(ctx, fm.durableMigrator(), vmID, opts)
}

// LazyMemoryWatchTimeout matches hostwire.LazyArtifactGrantTTL: past it the
// target's grant is dead and no further chunk can arrive.
const LazyMemoryWatchTimeout = 30 * time.Minute

// LazyMemoryPinTTL is how long a durable lazy-memory pin lasts when no
// watcher releases it: a little past the grant.
const LazyMemoryPinTTL = LazyMemoryWatchTimeout + 5*time.Minute
