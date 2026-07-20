package api

import (
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestToAPIHost_MIGInstancesRoundTrip checks that per-instance MIG inventory
// on capacity and the bound-uuid set on allocated both flow through to the
// wire shape, so SDK clients see which MIG instances a host offers and which
// are currently bound.
func TestToAPIHost_MIGInstancesRoundTrip(t *testing.T) {
	h := orchestrator.Host{
		ID:      "h1",
		URL:     "http://h1.test",
		Backend: orchestrator.BackendQEMU,
		State:   orchestrator.HostActive,
		Capacity: orchestrator.HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10, GPUKind: "a100",
			MIGInstances: []orchestrator.MIGInstance{
				{UUID: "m1", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
				{UUID: "m2", Profile: "1g.10gb", Kind: "a100", ParentGPUUUID: "gpu-a"},
			},
		},
		Allocated: orchestrator.HostCapacity{
			MIGInstanceUUIDs: []string{"m1"},
			MIGProfiles:      map[string]int{"1g.10gb": 1},
		},
	}

	info := toAPIHost(h)
	if len(info.Capacity.MIGInstances) != 2 {
		t.Fatalf("capacity mig_instances = %v, want 2 entries", info.Capacity.MIGInstances)
	}
	got := info.Capacity.MIGInstances[0]
	if got.UUID != "m1" || got.Profile != "1g.10gb" || got.Kind != "a100" || got.ParentGPUUUID != "gpu-a" {
		t.Errorf("first mig_instance = %+v, want full field round-trip", got)
	}
	if len(info.Allocated.MIGInstanceUUIDs) != 1 || info.Allocated.MIGInstanceUUIDs[0] != "m1" {
		t.Errorf("allocated mig_instance_uuids = %v, want [m1]", info.Allocated.MIGInstanceUUIDs)
	}
	// the returned slices must not alias the orchestrator's backing arrays,
	// so a later mutation can't leak into a previously-serialized response.
	info.Capacity.MIGInstances[0].UUID = "mutated"
	if h.Capacity.MIGInstances[0].UUID == "mutated" {
		t.Error("capacity mig_instances aliases the orchestrator slice")
	}
	info.Allocated.MIGInstanceUUIDs[0] = "mutated"
	if h.Allocated.MIGInstanceUUIDs[0] == "mutated" {
		t.Error("allocated mig_instance_uuids aliases the orchestrator slice")
	}
}
