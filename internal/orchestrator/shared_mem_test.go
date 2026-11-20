package orchestrator

import (
	"context"
	"testing"
)

func memFleetHost(id string, ramMB int) Host {
	return Host{
		ID:      id,
		URL:     "http://" + id + ".test",
		Backend: BackendFirecracker,
		Capacity: HostCapacity{
			CPUs:      64,
			RamMB:     ramMB,
			StorageGB: 1000,
			VMCount:   100,
		},
	}
}

// clone is a vm resumed from seed's memory image.
func clone(seed string, ramMB int) Spec {
	return Spec{CPUs: 1, RamMB: ramMB, StorageGB: 4, SeedSnapshotID: seed, ResumeSeed: true}
}

func memFleet(t *testing.T, ramMB int) *FleetManager {
	t.Helper()
	stub := newStubProvider()
	fm := newInlineFleet(FleetConfig{Provider: stub, Prefix: "mem-"})
	if err := fm.RegisterHost(context.Background(), memFleetHost("h1", ramMB), stub); err != nil {
		t.Fatal(err)
	}
	return fm
}

// place schedules spec onto the fleet's hosts and charges it, as placeVM
// does, and reports whether it fit.
func place(fm *FleetManager, spec Spec) (*vm, bool) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	h, _, err := Schedule(spec, fm.activeHostsLocked(), PlacementSpread)
	if err != nil {
		return nil, false
	}
	v := &vm{spec: spec, hostID: h.ID}
	fm.allocateOnHost(h.ID, v)
	return v, true
}

func TestSharedSeed_chargesTheImageOncePerHost(t *testing.T) {
	fm := memFleet(t, 8192)
	const n = 8
	var vms []*vm
	for range n {
		v, ok := place(fm, clone("snap-a", 1024))
		if !ok {
			t.Fatal("a clone did not fit")
		}
		if v.spec.SharedMemSeed != "snap-a" {
			t.Fatalf("SharedMemSeed = %q, want snap-a", v.spec.SharedMemSeed)
		}
		vms = append(vms, v)
	}
	h := findHost(t, fm, "h1")
	// one image plus n private shares, not n images.
	if want := 1024 + n*seedPrivateMB(1024); h.Allocated.RamMB != want {
		t.Errorf("allocated ram = %d, want %d", h.Allocated.RamMB, want)
	}
	if h.Allocated.SharedSeeds["snap-a"] != n {
		t.Errorf("shared seeds = %v, want snap-a: %d", h.Allocated.SharedSeeds, n)
	}

	// destroy in any order: the image stays charged until the last one goes.
	fm.mu.Lock()
	fm.deallocateOnHost("h1", vms[0].spec)
	fm.mu.Unlock()
	if want := 1024 + (n-1)*seedPrivateMB(1024); findHost(t, fm, "h1").Allocated.RamMB != want {
		t.Errorf("after one destroy allocated ram = %d, want %d", findHost(t, fm, "h1").Allocated.RamMB, want)
	}
	fm.mu.Lock()
	for _, v := range vms[1:] {
		fm.deallocateOnHost("h1", v.spec)
	}
	fm.mu.Unlock()
	h = findHost(t, fm, "h1")
	if h.Allocated.RamMB != 0 || h.Allocated.SharedSeeds != nil {
		t.Errorf("after all destroys allocated = ram %d seeds %v, want 0 and none", h.Allocated.RamMB, h.Allocated.SharedSeeds)
	}
}

func TestSharedSeed_createsAfterAFanOutStillPlace(t *testing.T) {
	// 4 GiB holds four 1 GiB vms charged in full. a fan-out of eight clones
	// from one seed leaves room for a plain create.
	fm := memFleet(t, 4096)
	for i := range 8 {
		if _, ok := place(fm, clone("snap-a", 1024)); !ok {
			t.Fatalf("clone %d did not fit", i)
		}
	}
	if _, ok := place(fm, Spec{CPUs: 1, RamMB: 1024, StorageGB: 4}); !ok {
		t.Fatalf("a plain create after the fan-out did not place (allocated %+v)", findHost(t, fm, "h1").Allocated)
	}
	// and the host still refuses once the private shares run out.
	for {
		if _, ok := place(fm, clone("snap-a", 1024)); !ok {
			break
		}
	}
	h := findHost(t, fm, "h1")
	if free := h.Capacity.RamMB - h.Allocated.RamMB; free >= seedPrivateMB(1024) {
		t.Errorf("clones stopped with %d MB free, want less than one private share", free)
	}
}

func TestSharedSeed_aSecondSeedPaysForItsOwnImage(t *testing.T) {
	fm := memFleet(t, 8192)
	place(fm, clone("snap-a", 1024))
	place(fm, clone("snap-b", 2048))
	if want := 1024 + seedPrivateMB(1024) + 2048 + seedPrivateMB(2048); findHost(t, fm, "h1").Allocated.RamMB != want {
		t.Errorf("allocated ram = %d, want %d", findHost(t, fm, "h1").Allocated.RamMB, want)
	}
}

func TestSharedSeed_fitsNeedsTheImageOnlyWhereItIsNotHeld(t *testing.T) {
	capacity := HostCapacity{CPUs: 8, RamMB: 2048, StorageGB: 100, VMCount: 10}
	spec := clone("snap-a", 1024)
	full := HostCapacity{RamMB: 1536}
	if fits(capacity, full, spec) {
		t.Error("fit with 512 MB free and the image not held, want no")
	}
	held := HostCapacity{RamMB: 1536, SharedSeeds: map[string]int{"snap-a": 1}}
	if !fits(capacity, held, spec) {
		t.Error("did not fit with 512 MB free and the image held, want a private share to fit")
	}
}

func TestSharedSeed_chargesInFullWhenNothingIsShared(t *testing.T) {
	for name, spec := range map[string]Spec{
		// a 2M guest is filled by fc-uffd into its own hugepages.
		"huge pages": {CPUs: 1, RamMB: 1024, SeedSnapshotID: "snap-a", ResumeSeed: true, HugePages: true},
		// a cold boot from a seed's rootfs maps no image.
		"cold boot": {CPUs: 1, RamMB: 1024, SeedSnapshotID: "snap-a"},
		"plain":     {CPUs: 1, RamMB: 1024},
	} {
		t.Run(name, func(t *testing.T) {
			fm := memFleet(t, 8192)
			place(fm, spec)
			v, _ := place(fm, spec)
			h := findHost(t, fm, "h1")
			if h.Allocated.RamMB != 2048 || h.Allocated.SharedSeeds != nil || v.spec.SharedMemSeed != "" {
				t.Errorf("allocated = ram %d seeds %v, vm seed %q; want 2048 in full and nothing shared",
					h.Allocated.RamMB, h.Allocated.SharedSeeds, v.spec.SharedMemSeed)
			}
		})
	}
}

func TestSharedSeed_aSpecThatTurnsIntoAResumeReleasesWhatItWasCharged(t *testing.T) {
	// a live migrate places its target before it sets ResumeSeed and the
	// seed, so it is charged in full and must be released in full.
	fm := memFleet(t, 8192)
	v, _ := place(fm, Spec{CPUs: 1, RamMB: 1024})
	v.spec.SeedSnapshotID, v.spec.ResumeSeed = "snap-a", true
	fm.mu.Lock()
	fm.deallocateOnHost("h1", v.spec)
	fm.mu.Unlock()
	if got := findHost(t, fm, "h1").Allocated.RamMB; got != 0 {
		t.Errorf("allocated ram = %d, want 0", got)
	}
}

func TestSharedSeed_hostCopiesDoNotSeeLaterAllocations(t *testing.T) {
	fm := memFleet(t, 8192)
	place(fm, clone("snap-a", 1024))
	before := findHost(t, fm, "h1")
	fm.mu.Lock()
	copies := fm.activeHostsLocked()
	fm.mu.Unlock()
	place(fm, clone("snap-a", 1024))
	if before.Allocated.SharedSeeds["snap-a"] != 1 || copies[0].Allocated.SharedSeeds["snap-a"] != 1 {
		t.Errorf("copies taken before the second clone moved: %v %v",
			before.Allocated.SharedSeeds, copies[0].Allocated.SharedSeeds)
	}
}

func TestRecoverRederivesSharedSeedCharge(t *testing.T) {
	store := NewMemoryStateStore()
	stub := newStubProvider()
	seed := recoverFleet(store, stub)
	if err := seed.RegisterHost(context.Background(), memFleetHost("h1", 8192), stub); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"clone-1", "clone-2", "clone-3"} {
		spec := clone("snap-a", 1024)
		spec.SharedMemSeed = "snap-a"
		seedRunningVM(t, store, stub, id, "h1", spec)
	}
	seedRunningVM(t, store, stub, "plain", "h1", Spec{CPUs: 1, RamMB: 512})

	fm := recoverFleet(store, stub)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatalf("recoverState: %v", err)
	}
	h := findHost(t, fm, "h1")
	if want := 1024 + 3*seedPrivateMB(1024) + 512; h.Allocated.RamMB != want {
		t.Errorf("recovered allocated ram = %d, want %d", h.Allocated.RamMB, want)
	}
	if h.Allocated.SharedSeeds["snap-a"] != 3 {
		t.Errorf("recovered shared seeds = %v, want snap-a: 3", h.Allocated.SharedSeeds)
	}
}
