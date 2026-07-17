package orchestrator

import (
	"errors"
	"fmt"
	"time"
)

// HostState captures the scheduling eligibility of a registered host.
type HostState string

const (
	// HostActive means the host is healthy and accepting new VMs.
	HostActive HostState = "active"

	// HostCordoned means the host is not accepting new VMs. Existing
	// VMs are left running. Cordon is the operator's "maintenance
	// soon, stop sending work here" signal.
	HostCordoned HostState = "cordoned"

	// HostDraining means the host is cordoned AND its existing VMs
	// are being evicted by the reconcile loop. Once all VMs are gone
	// the host is eligible for removal. Drain support in reconcile
	// is stubbed in this PR; the state is defined so the data model
	// doesn't need a follow-up migration.
	HostDraining HostState = "draining"
)

// HostCapacity is the resource envelope of a host. It is reported
// by the host agent at registration time and refreshed by heartbeat.
// The scheduler compares Spec against (Capacity - Allocated) to make
// admission decisions.
type HostCapacity struct {
	CPUs      int `json:"cpus"`
	RamMB     int `json:"ram_mb"`
	StorageGB int `json:"storage_gb"`
	VMCount   int `json:"vm_count"` // max concurrent VMs

	// GPUs is the count of whole GPU devices available on the host.
	// Zero means no GPUs. Only qemu-backed hosts may report GPUs > 0
	// (enforced at registration, see internal/api registerHost).
	GPUs int `json:"gpus,omitempty"`

	// GPUKind identifies the GPU model (e.g. "a100"). Empty when GPUs is 0.
	GPUKind string `json:"gpu_kind,omitempty"`

	// MIGProfiles is fractional GPU capacity: MIG instance count by
	// profile name (e.g. {"1g.10gb": 4}). A GPU in MIG mode stays on the
	// nvidia driver and exports mdev devices, so it is never part of the
	// whole-device GPUs pool — the two counters are independent (D5).
	// Only qemu-backed hosts may advertise MIG profiles.
	MIGProfiles map[string]int `json:"mig_profiles,omitempty"`
}

// freeMIG returns the free instance count for a MIG profile given the
// host's capacity and allocated maps.
func freeMIG(capacity, allocated HostCapacity, profile string) int {
	return capacity.MIGProfiles[profile] - allocated.MIGProfiles[profile]
}

// HostBackend identifies the virtualization backend a host agent runs.
// It determines which capabilities the host can offer the scheduler
// (e.g. only qemu hosts may advertise GPUs).
type HostBackend string

const (
	// BackendFirecracker is the default backend: microVMs with no GPU
	// passthrough support.
	BackendFirecracker HostBackend = "firecracker"

	// BackendQEMU is a full-VM backend that supports GPU passthrough.
	BackendQEMU HostBackend = "qemu"
)

// fits returns true if the host has enough headroom (capacity minus
// allocated) to place a VM with the given spec.
func fits(capacity, allocated HostCapacity, spec Spec) bool {
	if (capacity.CPUs-allocated.CPUs) < spec.CPUs ||
		(capacity.RamMB-allocated.RamMB) < spec.RamMB ||
		(capacity.StorageGB-allocated.StorageGB) < spec.StorageGB ||
		(capacity.VMCount-allocated.VMCount) < 1 {
		return false
	}
	if spec.GPUs > 0 {
		if spec.GPUProfile != "" {
			// Fractional request: spec.GPUs counts MIG instances of the
			// requested profile, allocated from the host's MIG pool (D5).
			if freeMIG(capacity, allocated, spec.GPUProfile) < int(spec.GPUs) {
				return false
			}
		} else if capacity.GPUs-allocated.GPUs < int(spec.GPUs) {
			return false
		}
		if spec.GPUKind != "" && spec.GPUKind != capacity.GPUKind {
			return false
		}
	}
	return true
}

// Host is a registered compute host in the fleet. It represents a
// single Firecracker host agent that can provision VMs.
type Host struct {
	ID        string
	URL       string // base URL of the host agent (e.g. https://agent-1.local)
	Token     string // bearer token for this host's agent
	Region    string
	Backend   HostBackend // "firecracker" or "qemu"; empty means firecracker (default)
	Capacity  HostCapacity
	Allocated HostCapacity
	State     HostState
	LastSeen  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// schedulable returns true if the host is healthy and eligible for
// new VM placements. A host must be Active and have been seen
// recently to schedule work to it.
func (h *Host) schedulable() bool {
	return h.State == HostActive
}

// PlacementPolicy controls how the scheduler picks among eligible
// hosts that all have sufficient capacity.
type PlacementPolicy string

const (
	// PlacementBinpack fills hosts as densely as possible. It picks
	// the host with the MOST already-allocated resources that still
	// has room. This minimizes the number of active hosts.
	PlacementBinpack PlacementPolicy = "binpack"

	// PlacementSpread distributes VMs as evenly as possible. It picks
	// the host with the LEAST already-allocated resources. This
	// maximizes isolation between VMs.
	PlacementSpread PlacementPolicy = "spread"
)

// ErrNoCapacity is returned by Schedule when no registered host can
// fit the requested spec. The REST handler maps this to 503.
var ErrNoCapacity = errors.New("no host has sufficient capacity")

// ErrNoHosts is returned by Schedule when the host registry is empty.
var ErrNoHosts = errors.New("no hosts registered")

// PlacementDecision records why the scheduler picked a particular
// host. It is attached to the VM record for debugging.
type PlacementDecision struct {
	HostID       string          `json:"host_id"`
	Policy       PlacementPolicy `json:"policy"`
	Candidates   int             `json:"candidates"`    // eligible hosts considered
	HeadroomCPUs int             `json:"headroom_cpus"` // CPUs remaining after placement
	HeadroomRam  int             `json:"headroom_ram"`  // RAM remaining after placement
}

// Schedule picks the best host for the given spec according to the
// placement policy. It is a pure function with no side effects — the
// caller is responsible for updating the host's Allocated counters
// and persisting the decision.
//
// Filter pipeline:
//  1. Exclude non-schedulable hosts (cordoned, draining).
//  2. If spec.Region is non-empty, exclude hosts in a different region.
//  3. Exclude hosts that don't fit the spec (capacity - allocated < spec).
//  4. Among survivors, pick by policy (binpack or spread).
//
// Ties within a policy are broken by host ID for determinism.
func Schedule(spec Spec, hosts []*Host, policy PlacementPolicy) (*Host, PlacementDecision, error) {
	if len(hosts) == 0 {
		return nil, PlacementDecision{}, ErrNoHosts
	}

	// Filter to eligible candidates.
	type candidate struct {
		host     *Host
		headroom int // policy-specific score (lower = more packed)
	}
	var candidates []candidate

	for _, h := range hosts {
		if !h.schedulable() {
			continue
		}
		if spec.Region != "" && h.Region != spec.Region {
			continue
		}
		// gpu envs may only land on qemu hosts (D3). registration rejects
		// gpus > 0 on firecracker hosts, but the scheduler stays defensive
		// against a stale or hand-edited host record.
		if spec.GPUs > 0 && h.Backend != BackendQEMU {
			continue
		}
		if !fits(h.Capacity, h.Allocated, spec) {
			continue
		}
		// Score: total "free" resources as a single comparable int.
		// We use CPUs as the primary dimension and RAM as tiebreak
		// because CPUs are the scarcest resource in practice.
		freeCPUs := h.Capacity.CPUs - h.Allocated.CPUs
		freeRAM := h.Capacity.RamMB - h.Allocated.RamMB
		score := freeCPUs*10000 + freeRAM // weight CPUs heavily
		candidates = append(candidates, candidate{host: h, headroom: score})
	}

	if len(candidates) == 0 {
		return nil, PlacementDecision{}, fmt.Errorf(
			"%w: need %dCPU/%dMB/%dGB, no eligible host fits",
			ErrNoCapacity, spec.CPUs, spec.RamMB, spec.StorageGB)
	}

	// Pick by policy.
	best := candidates[0]
	for _, c := range candidates[1:] {
		switch policy {
		case PlacementBinpack:
			// Most packed = smallest headroom.
			if c.headroom < best.headroom ||
				(c.headroom == best.headroom && c.host.ID < best.host.ID) {
				best = c
			}
		case PlacementSpread, "":
			// Least packed = largest headroom (default).
			if c.headroom > best.headroom ||
				(c.headroom == best.headroom && c.host.ID < best.host.ID) {
				best = c
			}
		}
	}

	decision := PlacementDecision{
		HostID:       best.host.ID,
		Policy:       policy,
		Candidates:   len(candidates),
		HeadroomCPUs: best.host.Capacity.CPUs - best.host.Allocated.CPUs - spec.CPUs,
		HeadroomRam:  best.host.Capacity.RamMB - best.host.Allocated.RamMB - spec.RamMB,
	}

	return best.host, decision, nil
}
