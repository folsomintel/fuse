package orchestrator

import (
	"errors"
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
