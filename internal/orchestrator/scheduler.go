package orchestrator

import (
	"errors"
	"fmt"
	"strings"
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

	// GPUDevices is the per-device detail probed from the host agent
	// (one entry per whole GPU). It rides the capacity wire alongside the
	// scalar GPUs/GPUKind counters. In-memory + wire only in this PR; the
	// durable per-device schema is issue #37.
	GPUDevices []GPUDevice `json:"gpu_devices,omitempty"`

	// GPUDeviceUUIDs is the set of device uuids currently bound to VMs. It
	// is populated only on a host's Allocated struct (never on Capacity),
	// and only for hosts that report per-device inventory. fits() derives
	// the free-device set as Capacity.GPUDevices minus this set. It is
	// in-memory only: allocation/deallocation maintain it, and recovery
	// recomputes it from live VM bindings, so it is never persisted as its
	// own column (the durable source of truth is the per-VM gpu_uuids).
	GPUDeviceUUIDs []string `json:"gpu_device_uuids,omitempty"`
}

// freeMIG returns the free instance count for a MIG profile given the
// host's capacity and allocated maps.
func freeMIG(capacity, allocated HostCapacity, profile string) int {
	return capacity.MIGProfiles[profile] - allocated.MIGProfiles[profile]
}

// GPUDevice is the per-device detail the host agent probes for a single
// whole GPU. Field names mirror the agent's gpu_devices payload. All fields
// are best-effort: the agent omits any it cannot determine.
type GPUDevice struct {
	UUID          string `json:"uuid,omitempty"`
	Model         string `json:"model,omitempty"`
	PCIBusID      string `json:"pci_bus_id,omitempty"`
	MemoryMB      int    `json:"memory_mb,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	CUDAVersion   string `json:"cuda_version,omitempty"`
	ComputeCap    string `json:"compute_cap,omitempty"`
	MIGCapable    bool   `json:"mig_capable,omitempty"`
	MIGMode       string `json:"mig_mode,omitempty"`
	IOMMUGroup    string `json:"iommu_group,omitempty"`
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
			// This pool is independent of the whole-device GPUs pool.
			if freeMIG(capacity, allocated, spec.GPUProfile) < int(spec.GPUs) {
				return false
			}
			return hostServesMIG(capacity, spec.GPUKind)
		}
		if len(capacity.GPUDevices) > 0 {
			// per-device host: count free devices whose kind matches the
			// request. free = all capacity devices minus the allocated-uuid
			// set. this makes heterogeneous hosts (mixed models) schedule
			// correctly where scalar subtraction cannot.
			return len(freeMatchingDevices(capacity, allocated, spec.GPUKind)) >= int(spec.GPUs)
		}
		// legacy homogeneous host: scalar count + exact gpu_kind string.
		if capacity.GPUs-allocated.GPUs < int(spec.GPUs) {
			return false
		}
		if spec.GPUKind != "" && spec.GPUKind != capacity.GPUKind {
			return false
		}
	}
	return true
}

// hostServesMIG reports whether a host can carve MIG instances of the
// requested kind. MIG instances come off the host's own GPUs, so when the host
// reports per-device inventory the request must be satisfiable by a SINGLE
// device that is both MIGCapable and a kind match (gpuKindMatches semantics,
// same as the whole-device branch). Both conjuncts must hold of the same card,
// which is why gpuKindMatches must not consult the host scalar kind for a
// device that reported its own Model.
//
// Devices are read from Capacity and not from the free set: a GPU in MIG mode
// stays on the nvidia driver and is never part of the whole-device pool (D5),
// so whole-device allocations say nothing about MIG availability.
//
// Absence of device data is not evidence of incapability. A host that reports
// no per-device inventory (legacy scalar hosts, and MIG pools declared by hand
// with --mig-profile at registration) falls back to the host's scalar GPUKind,
// and a host that reports no kind either is left to the qemu agent to accept
// or reject. Only positive evidence filters a host out here: devices that are
// all non-MIG-capable, or no MIG-capable device of the requested kind.
func hostServesMIG(capacity HostCapacity, gpuKind string) bool {
	if len(capacity.GPUDevices) == 0 {
		// no inventory: ask the same matcher about a device that reports
		// nothing, which is exactly the host-scalar fallback.
		return gpuKindMatches(gpuKind, GPUDevice{}, capacity.GPUKind)
	}
	for _, d := range capacity.GPUDevices {
		if d.MIGCapable && gpuKindMatches(gpuKind, d, capacity.GPUKind) {
			return true
		}
	}
	return false
}

// freeMatchingDevices returns the uuids of capacity devices that are not in
// the allocated-uuid set and whose kind matches gpuKind. An empty gpuKind
// matches any device. Matching is case-insensitive against both the device
// Model and the derived host GPUKind so requests like "a100" match a device
// whose Model is "NVIDIA A100-SXM4-40GB".
func freeMatchingDevices(capacity, allocated HostCapacity, gpuKind string) []string {
	used := make(map[string]struct{}, len(allocated.GPUDeviceUUIDs))
	for _, u := range allocated.GPUDeviceUUIDs {
		used[u] = struct{}{}
	}
	var free []string
	for _, d := range capacity.GPUDevices {
		if d.UUID == "" {
			continue
		}
		if _, taken := used[d.UUID]; taken {
			continue
		}
		if !gpuKindMatches(gpuKind, d, capacity.GPUKind) {
			continue
		}
		free = append(free, d.UUID)
	}
	return free
}

// gpuKindMatches reports whether a requested gpu kind matches a device. An
// empty request matches anything. Otherwise the request must be a
// case-insensitive substring of the device Model, so both "a100" and a full
// model string resolve.
//
// The host's derived GPUKind is only a fallback for a device that reports no
// Model of its own (the qemu agent degrades model to "" when the vfio metadata
// blob omits it). It must never override a Model the device did report: the
// host kind is a fleet-level scalar, so consulting it for a device that
// disagrees makes the match device-independent, and a caller asking two
// questions about one device (this branch and MIGCapable in hostServesMIG)
// could have them answered by two different cards.
func gpuKindMatches(gpuKind string, d GPUDevice, hostKind string) bool {
	if gpuKind == "" {
		return true
	}
	if d.Model != "" {
		return strings.Contains(strings.ToLower(d.Model), strings.ToLower(gpuKind))
	}
	// no device-level evidence: the host scalar is all we know about this card.
	return hostKind == "" || strings.EqualFold(gpuKind, hostKind)
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
