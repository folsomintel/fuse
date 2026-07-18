package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func host(id string, cpus, ramMB, storageGB, vmCount int, state HostState) *Host {
	return &Host{
		ID:       id,
		URL:      "http://" + id + ".test",
		Region:   "us-east",
		Capacity: HostCapacity{CPUs: cpus, RamMB: ramMB, StorageGB: storageGB, VMCount: vmCount},
		State:    state,
		LastSeen: time.Now(),
	}
}

func hostWithAlloc(id string, capCPU, capRAM, allocCPU, allocRAM int) *Host {
	return &Host{
		ID:        id,
		URL:       "http://" + id + ".test",
		Region:    "us-east",
		Capacity:  HostCapacity{CPUs: capCPU, RamMB: capRAM, StorageGB: 100, VMCount: 10},
		Allocated: HostCapacity{CPUs: allocCPU, RamMB: allocRAM},
		State:     HostActive,
		LastSeen:  time.Now(),
	}
}

func spec(cpus, ramMB, storageGB int) Spec {
	return Spec{CPUs: cpus, RamMB: ramMB, StorageGB: storageGB}
}

func TestSchedule_emptyHostsReturnsErrNoHosts(t *testing.T) {
	_, _, err := Schedule(spec(1, 256, 10), nil, PlacementSpread)
	if !errors.Is(err, ErrNoHosts) {
		t.Errorf("err = %v, want ErrNoHosts", err)
	}
}

func TestSchedule_allCordonedReturnsErrNoCapacity(t *testing.T) {
	hosts := []*Host{
		host("h1", 8, 4096, 100, 10, HostCordoned),
		host("h2", 8, 4096, 100, 10, HostDraining),
	}
	_, _, err := Schedule(spec(1, 256, 10), hosts, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_noFitReturnsErrNoCapacity(t *testing.T) {
	hosts := []*Host{
		host("h1", 2, 1024, 50, 10, HostActive),
	}
	// Request more CPUs than the host has.
	_, _, err := Schedule(spec(4, 256, 10), hosts, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_regionFilterExcludesMismatch(t *testing.T) {
	h := host("h1", 8, 4096, 100, 10, HostActive)
	h.Region = "eu-west"

	s := spec(1, 256, 10)
	s.Region = "us-east" // doesn't match h1's region

	_, _, err := Schedule(s, []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (region mismatch)", err)
	}
}

func TestSchedule_binpackPicksMostPacked(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 6, 3072), // 2 free CPUs, 1024 free RAM
		hostWithAlloc("h2", 8, 4096, 2, 1024), // 6 free CPUs, 3072 free RAM
	}
	picked, dec, err := Schedule(spec(1, 256, 10), hosts, PlacementBinpack)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h1" {
		t.Errorf("binpack picked %s, want h1 (most packed)", picked.ID)
	}
	if dec.Candidates != 2 {
		t.Errorf("candidates = %d, want 2", dec.Candidates)
	}
	if dec.Policy != PlacementBinpack {
		t.Errorf("policy = %s", dec.Policy)
	}
}

func TestSchedule_spreadPicksLeastPacked(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 6, 3072), // 2 free CPUs
		hostWithAlloc("h2", 8, 4096, 2, 1024), // 6 free CPUs
	}
	picked, _, err := Schedule(spec(1, 256, 10), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h2" {
		t.Errorf("spread picked %s, want h2 (least packed)", picked.ID)
	}
}

func TestSchedule_tieBreaksByID(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("b-host", 8, 4096, 4, 2048),
		hostWithAlloc("a-host", 8, 4096, 4, 2048), // same allocation, lower ID
	}
	picked, _, err := Schedule(spec(1, 256, 10), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	// Same headroom → tiebreak by ID (alphabetical).
	if picked.ID != "a-host" {
		t.Errorf("tiebreak picked %s, want a-host", picked.ID)
	}
}

func TestSchedule_vmCountLimitEnforced(t *testing.T) {
	h := host("h1", 64, 65536, 1000, 2, HostActive)
	h.Allocated = HostCapacity{VMCount: 2} // at capacity for VM count
	_, _, err := Schedule(spec(1, 256, 10), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (VM count full)", err)
	}
}

func TestSchedule_decisionRecordsHeadroom(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 2, 1024),
	}
	_, dec, err := Schedule(spec(2, 512, 10), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	// After placement: 8-2-2=4 CPUs, 4096-1024-512=2560 RAM.
	if dec.HeadroomCPUs != 4 {
		t.Errorf("headroom CPUs = %d, want 4", dec.HeadroomCPUs)
	}
	if dec.HeadroomRam != 2560 {
		t.Errorf("headroom RAM = %d, want 2560", dec.HeadroomRam)
	}
}

func TestSchedule_emptyPolicyDefaultsToSpread(t *testing.T) {
	hosts := []*Host{
		hostWithAlloc("h1", 8, 4096, 6, 3072),
		hostWithAlloc("h2", 8, 4096, 2, 1024),
	}
	picked, _, err := Schedule(spec(1, 256, 10), hosts, "")
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h2" {
		t.Errorf("empty policy picked %s, want h2 (spread default)", picked.ID)
	}
}

func TestFits_variousCombinations(t *testing.T) {
	cap := HostCapacity{CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5}
	tests := []struct {
		name      string
		allocated HostCapacity
		spec      Spec
		want      bool
	}{
		{"empty alloc, small spec", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10}, true},
		{"exact fit", HostCapacity{CPUs: 7, RamMB: 3840, StorageGB: 90, VMCount: 4}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10}, true},
		{"CPU overcommit", HostCapacity{CPUs: 8}, Spec{CPUs: 1}, false},
		{"RAM overcommit", HostCapacity{RamMB: 4096}, Spec{RamMB: 1}, false},
		{"storage overcommit", HostCapacity{StorageGB: 100}, Spec{StorageGB: 1}, false},
		{"VM count full", HostCapacity{VMCount: 5}, Spec{CPUs: 1, RamMB: 1, StorageGB: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fits(cap, tc.allocated, tc.spec)
			if got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostCapacity_GPUFieldsRoundTripJSON(t *testing.T) {
	in := HostCapacity{CPUs: 4, RamMB: 1024, StorageGB: 50, VMCount: 2, GPUs: 2, GPUKind: "a100"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !containsJSONField(data, `"gpus":2`) || !containsJSONField(data, `"gpu_kind":"a100"`) {
		t.Fatalf("marshaled json missing gpu fields: %s", data)
	}

	var out HostCapacity
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestHostCapacity_GPUFieldsOmittedWhenZero(t *testing.T) {
	in := HostCapacity{CPUs: 4, RamMB: 1024, StorageGB: 50, VMCount: 2}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsJSONField(data, `"gpus"`) || containsJSONField(data, `"gpu_kind"`) {
		t.Fatalf("expected gpu fields to be omitted, got: %s", data)
	}

	var out HostCapacity
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GPUs != 0 || out.GPUKind != "" {
		t.Errorf("defaults = %+v, want zero values", out)
	}
}

func TestHostBackend_DefaultsToFirecracker(t *testing.T) {
	// A Host built without an explicit Backend (e.g. legacy record) must
	// behave as firecracker — this is asserted at the registration
	// boundary in internal/api, but the zero value itself must be the
	// empty string so callers can detect "unset" and apply the default.
	var h Host
	if h.Backend != "" {
		t.Errorf("zero-value Backend = %q, want empty string", h.Backend)
	}
}

func TestHostBackend_Constants(t *testing.T) {
	if BackendFirecracker != "firecracker" {
		t.Errorf("BackendFirecracker = %q, want firecracker", BackendFirecracker)
	}
	if BackendQEMU != "qemu" {
		t.Errorf("BackendQEMU = %q, want qemu", BackendQEMU)
	}
}

// containsJSONField is a small substring helper so the round-trip tests
// can assert on exact key:value presence without decoding into a map
// (which would hide omitempty behavior behind Go's zero-value defaults).
func containsJSONField(data []byte, field string) bool {
	return bytes.Contains(data, []byte(field))
}

func gpuHost(id string, backend HostBackend, gpus int, kind string) *Host {
	h := host(id, 8, 4096, 100, 10, HostActive)
	h.Backend = backend
	h.Capacity.GPUs = gpus
	h.Capacity.GPUKind = kind
	return h
}

func gpuSpec(gpus int32, kind string) Spec {
	s := spec(1, 256, 10)
	s.GPUs = gpus
	s.GPUKind = kind
	return s
}

func TestSchedule_gpuRequiresGPUHost(t *testing.T) {
	// host with GPUs:0 must be rejected for a GPUs:1 request, even if cpu/ram fit
	h := gpuHost("h1", BackendQEMU, 0, "")
	_, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestSchedule_gpuNeverLandsOnFirecrackerHost(t *testing.T) {
	// registration rejects gpus>0 on firecracker hosts, but the scheduler
	// stays defensive against a stale or hand-edited host record
	h := gpuHost("h1", BackendFirecracker, 2, "a100")
	_, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (firecracker host)", err)
	}
}

func TestSchedule_gpuPlacedOnQEMUHostWithFreeGPUs(t *testing.T) {
	hosts := []*Host{
		gpuHost("fc-1", BackendFirecracker, 0, ""),
		gpuHost("gpu-1", BackendQEMU, 2, "a100"),
	}
	picked, _, err := Schedule(gpuSpec(1, ""), hosts, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "gpu-1" {
		t.Errorf("picked %s, want gpu-1", picked.ID)
	}
}

func TestSchedule_gpuKindMustMatch(t *testing.T) {
	h100 := gpuHost("h100-1", BackendQEMU, 2, "h100")
	_, _, err := Schedule(gpuSpec(1, "a100"), []*Host{h100}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (kind mismatch)", err)
	}

	a100 := gpuHost("a100-1", BackendQEMU, 2, "a100")
	picked, _, err := Schedule(gpuSpec(1, "a100"), []*Host{h100, a100}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "a100-1" {
		t.Errorf("picked %s, want a100-1", picked.ID)
	}
}

func TestSchedule_gpuEmptyKindMatchesAnyKind(t *testing.T) {
	h := gpuHost("h1", BackendQEMU, 1, "h100")
	picked, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h1" {
		t.Errorf("picked %s, want h1", picked.ID)
	}
}

func TestFits_migProfileUsesMIGPool(t *testing.T) {
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5,
		GPUs: 0, GPUKind: "a100",
		MIGProfiles: map[string]int{"1g.10gb": 4},
	}
	tests := []struct {
		name      string
		allocated HostCapacity
		spec      Spec
		want      bool
	}{
		{
			name: "profile fits free pool",
			spec: Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2, GPUProfile: "1g.10gb"},
			want: true,
		},
		{
			name:      "profile exhausted",
			allocated: HostCapacity{MIGProfiles: map[string]int{"1g.10gb": 4}},
			spec:      Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUProfile: "1g.10gb"},
			want:      false,
		},
		{
			name: "unknown profile",
			spec: Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUProfile: "2g.20gb"},
			want: false,
		},
		{
			name: "whole-device request ignores mig pool",
			spec: Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1},
			want: false, // GPUs capacity is 0
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fits(cap, tc.allocated, tc.spec)
			if got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSchedule_migProfilePlacedOnMatchingHost(t *testing.T) {
	whole := gpuHost("whole-1", BackendQEMU, 2, "a100")
	mig := gpuHost("mig-1", BackendQEMU, 0, "a100")
	mig.Capacity.MIGProfiles = map[string]int{"1g.10gb": 4}

	spec := gpuSpec(1, "a100")
	spec.GPUProfile = "1g.10gb"

	picked, _, err := Schedule(spec, []*Host{whole, mig}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "mig-1" {
		t.Errorf("picked %s, want mig-1", picked.ID)
	}
}

func TestSchedule_gpuAllocatedDevicesAreNotReused(t *testing.T) {
	// single-gpu host with its device already allocated cannot take another
	h := gpuHost("h1", BackendQEMU, 1, "a100")
	h.Allocated.GPUs = 1
	_, _, err := Schedule(gpuSpec(1, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (gpu already allocated)", err)
	}
}

func TestSchedule_gpuRequestExceedingFreeInventoryFails(t *testing.T) {
	h := gpuHost("h1", BackendQEMU, 2, "a100")
	_, _, err := Schedule(gpuSpec(4, ""), []*Host{h}, PlacementSpread)
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (not enough free gpus)", err)
	}
}

func TestSchedule_zeroGPUSpecSchedulesOnFirecrackerUnchanged(t *testing.T) {
	// regression: non-gpu envs keep scheduling on firecracker hosts
	h := host("h1", 8, 4096, 100, 10, HostActive)
	picked, _, err := Schedule(spec(1, 256, 10), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "h1" {
		t.Errorf("picked %s, want h1", picked.ID)
	}
}

func TestSchedule_zeroGPUSpecMayUseQEMUGPUHost(t *testing.T) {
	// d3: non-gpu workloads may run on qemu gpu hosts
	h := gpuHost("gpu-1", BackendQEMU, 2, "a100")
	picked, _, err := Schedule(spec(1, 256, 10), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "gpu-1" {
		t.Errorf("picked %s, want gpu-1", picked.ID)
	}
}

func TestFits_gpuCombinations(t *testing.T) {
	cap := HostCapacity{CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5, GPUs: 2, GPUKind: "a100"}
	tests := []struct {
		name      string
		allocated HostCapacity
		spec      Spec
		want      bool
	}{
		{"gpu fits", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}, true},
		{"gpu exact fit", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2}, true},
		{"gpu overcommit", HostCapacity{GPUs: 2}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}, false},
		{"kind match", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100"}, true},
		{"kind mismatch", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "h100"}, false},
		{"kind ignored when no gpu requested", HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUKind: "h100"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fits(cap, tc.allocated, tc.spec)
			if got != tc.want {
				t.Errorf("fits = %v, want %v", got, tc.want)
			}
		})
	}
}

// deviceHost builds a per-device host (Capacity.GPUDevices populated). The
// scalar GPUs count and GPUKind are derived as back-compat aggregates.
func deviceHost(id string, backend HostBackend, devices ...GPUDevice) *Host {
	h := host(id, 8, 4096, 100, 10, HostActive)
	h.Backend = backend
	h.Capacity.GPUDevices = devices
	h.Capacity.GPUs = len(devices)
	if len(devices) > 0 {
		h.Capacity.GPUKind = devices[0].Model
	}
	return h
}

func TestFits_perDeviceCountsFreeMatchingDevices(t *testing.T) {
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5, GPUs: 2,
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
			{UUID: "gpu-b", Model: "NVIDIA A100-SXM4-40GB"},
		},
	}
	// one device already bound: only one free, so a 2-gpu request fails but a
	// 1-gpu request fits.
	alloc := HostCapacity{GPUDeviceUUIDs: []string{"gpu-a"}}
	if fits(cap, alloc, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2}) {
		t.Error("fits = true, want false (only one free device)")
	}
	if !fits(cap, alloc, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1}) {
		t.Error("fits = false, want true (one free device)")
	}
}

func TestFits_heterogeneousHostMatchesByModel(t *testing.T) {
	// mixed-model host: an a100 request must only count the a100 device, even
	// though the scalar free count is 2.
	cap := HostCapacity{
		CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 5, GPUs: 2,
		GPUDevices: []GPUDevice{
			{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
			{UUID: "gpu-h", Model: "NVIDIA H100-SXM5-80GB"},
		},
	}
	if !fits(cap, HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100"}) {
		t.Error("fits = false, want true (one a100 free)")
	}
	if fits(cap, HostCapacity{}, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 2, GPUKind: "a100"}) {
		t.Error("fits = true, want false (only one a100 on a mixed host)")
	}
	// once the a100 is bound, no a100 is free.
	alloc := HostCapacity{GPUDeviceUUIDs: []string{"gpu-a"}}
	if fits(cap, alloc, Spec{CPUs: 1, RamMB: 256, StorageGB: 10, GPUs: 1, GPUKind: "a100"}) {
		t.Error("fits = true, want false (the only a100 is allocated)")
	}
}

func TestSchedule_heterogeneousHostPlacesByKind(t *testing.T) {
	h := deviceHost("gpu-1", BackendQEMU,
		GPUDevice{UUID: "gpu-a", Model: "NVIDIA A100-SXM4-40GB"},
		GPUDevice{UUID: "gpu-h", Model: "NVIDIA H100-SXM5-80GB"},
	)
	picked, _, err := Schedule(gpuSpec(1, "h100"), []*Host{h}, PlacementSpread)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "gpu-1" {
		t.Errorf("picked %s, want gpu-1", picked.ID)
	}
	// a kind the host does not carry must not fit.
	if _, _, err := Schedule(gpuSpec(1, "v100"), []*Host{h}, PlacementSpread); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity (no v100 device)", err)
	}
}
