package orchestrator

import (
	"context"
	"testing"
)

// recoverFleet builds a fresh FleetManager wired to the given store and a
// host provider factory that hands back the supplied stub, mirroring how a
// real orchestrator reattaches per-host providers on restart. Every
// recovered host resolves to the same stub, which is fine because the tests
// register a single host.
func recoverFleet(store StateStore, stub Provider) *FleetManager {
	return NewFleetManager(FleetConfig{
		Provider:   stub,
		Prefix:     "gpu-",
		StateStore: store,
		HostProviderFactory: func(_ string, _ string, _ HostBackend) Provider {
			return stub
		},
	})
}

// seedRunningVM persists a Running VM assigned to hostID and pre-seeds the
// stub provider with its env so recovery keeps it live (a Running VM whose
// env is missing from the provider is demoted to Destroying).
func seedRunningVM(t *testing.T, store StateStore, stub *stubLoadProvider, id, hostID string, spec Spec) {
	t.Helper()
	stub.mu.Lock()
	stub.envs[id] = &stubLoadEnv{name: id, url: "http://" + id + ".test"}
	stub.mu.Unlock()
	spec.Name = id
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID:     id,
		HostID: hostID,
		State:  VMStateRunning,
		URL:    "http://" + id + ".test",
		Spec:   spec,
	}); err != nil {
		t.Fatalf("seed vm %s: %v", id, err)
	}
}

// TestRecoverRederivesGPUAllocationFromLiveVMs checks that a restart derives
// a per-device host's Allocated (uuid set + scalar count) purely from the
// live VM bindings.
func TestRecoverRederivesGPUAllocationFromLiveVMs(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	// first boot: register a two-device host and persist a live VM bound to
	// one of its devices.
	seed := recoverFleet(store, stub)
	host := deviceFleetHost("h1",
		GPUDevice{UUID: "gpu-a", Model: "a100"},
		GPUDevice{UUID: "gpu-b", Model: "a100"},
	)
	if err := seed.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "gpu-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100",
		GPUUUIDs: []string{"gpu-a"},
	})

	// restart: a fresh manager over the same store recovers state.
	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.CPUs != 2 || h.Allocated.RamMB != 512 || h.Allocated.StorageGB != 10 || h.Allocated.VMCount != 1 {
		t.Errorf("scalar allocated = %+v, want cpus=2 ram=512 storage=10 vmcount=1", h.Allocated)
	}
	if h.Allocated.GPUs != 1 {
		t.Errorf("allocated GPUs = %d, want 1", h.Allocated.GPUs)
	}
	if len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != "gpu-a" {
		t.Errorf("allocated uuids = %v, want [gpu-a]", h.Allocated.GPUDeviceUUIDs)
	}
}

// TestRecoverHealsInjectedGPUDrift persists a host whose Allocated counter is
// deliberately wrong (drift from a crash mid write-behind) alongside a single
// live VM, and asserts recovery re-derives the correct values from the VM.
func TestRecoverHealsInjectedGPUDrift(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	// persist a host record with a bogus Allocated: it claims both devices
	// are bound and 4 cpus in use, but only one live VM (2 cpus, gpu-a)
	// actually exists.
	drifted := HostRecord{
		ID:      "h1",
		URL:     "http://h1.test",
		Backend: BackendQEMU,
		State:   HostActive,
		Capacity: HostCapacity{
			CPUs: 8, RamMB: 4096, StorageGB: 100, VMCount: 10,
			GPUs:    2,
			GPUKind: "a100",
			GPUDevices: []GPUDevice{
				{UUID: "gpu-a", Model: "a100"},
				{UUID: "gpu-b", Model: "a100"},
			},
		},
		Allocated: HostCapacity{
			CPUs: 4, RamMB: 2048, StorageGB: 40, VMCount: 2,
			GPUs:           2,
			GPUDeviceUUIDs: []string{"gpu-a", "gpu-b"},
		},
	}
	if err := store.UpsertHost(context.Background(), drifted); err != nil {
		t.Fatal(err)
	}
	seedRunningVM(t, store, stub, "gpu-vm-1", "h1", Spec{
		CPUs: 2, RamMB: 512, StorageGB: 10, GPUs: 1, GPUKind: "a100",
		GPUUUIDs: []string{"gpu-a"},
	})

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.CPUs != 2 || h.Allocated.RamMB != 512 || h.Allocated.StorageGB != 10 || h.Allocated.VMCount != 1 {
		t.Errorf("healed scalar allocated = %+v, want cpus=2 ram=512 storage=10 vmcount=1 (drift not healed)", h.Allocated)
	}
	if h.Allocated.GPUs != 1 {
		t.Errorf("healed GPUs = %d, want 1 (drift not healed)", h.Allocated.GPUs)
	}
	if len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != "gpu-a" {
		t.Errorf("healed uuids = %v, want [gpu-a] (stale gpu-b not released)", h.Allocated.GPUDeviceUUIDs)
	}
}

// TestRecoverExcludesDestroyingVMFromRecompute checks that a VM demoted to
// Destroying during recovery does not contribute to the recomputed
// allocation, so its GPU device is treated as free again. The Destroying
// demotion is triggered by leaving its env out of the provider.
func TestRecoverExcludesDestroyingVMFromRecompute(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()

	seed := recoverFleet(store, stub)
	host := deviceFleetHost("h1",
		GPUDevice{UUID: "gpu-a", Model: "a100"},
		GPUDevice{UUID: "gpu-b", Model: "a100"},
	)
	if err := seed.RegisterHost(context.Background(), host, stub); err != nil {
		t.Fatal(err)
	}

	// live VM: env present, stays Running and counts toward the recompute.
	seedRunningVM(t, store, stub, "gpu-live", "h1", Spec{
		CPUs: 2, RamMB: 512, GPUs: 1, GPUKind: "a100",
		GPUUUIDs: []string{"gpu-a"},
	})

	// doomed VM: persisted as Running and bound to gpu-b, but its env is
	// absent from the provider, so recovery demotes it to Destroying. It
	// must be excluded from the sums.
	if err := store.UpsertVM(context.Background(), VMRecord{
		ID:     "gpu-doomed",
		HostID: "h1",
		State:  VMStateRunning,
		Spec: Spec{
			Name: "gpu-doomed", CPUs: 2, RamMB: 512, GPUs: 1, GPUKind: "a100",
			GPUUUIDs: []string{"gpu-b"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	// sanity: the doomed VM was actually demoted.
	if info, ok := fm.GetVM("gpu-doomed"); !ok || info.State != VMStateDestroying {
		t.Fatalf("gpu-doomed state = %v ok=%v, want destroying", info.State, ok)
	}

	h := findHost(t, fm, "h1")
	if h.Allocated.CPUs != 2 || h.Allocated.VMCount != 1 {
		t.Errorf("allocated = %+v, want only the live vm counted (cpus=2 vmcount=1)", h.Allocated)
	}
	if h.Allocated.GPUs != 1 {
		t.Errorf("allocated GPUs = %d, want 1 (destroying vm's device excluded)", h.Allocated.GPUs)
	}
	if len(h.Allocated.GPUDeviceUUIDs) != 1 || h.Allocated.GPUDeviceUUIDs[0] != "gpu-a" {
		t.Errorf("allocated uuids = %v, want [gpu-a] (gpu-b freed with the destroying vm)", h.Allocated.GPUDeviceUUIDs)
	}
}
