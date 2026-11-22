package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// failingForker fails the children named in fail, settled the way restate
// records a handler's terminal failure unless unsettled is set, and hands
// every other child to the fleet's own forker.
type failingForker struct {
	inner     DurableForker
	fail      map[string]bool
	unsettled bool

	mu    sync.Mutex
	calls []ForkRequest
}

func (f *failingForker) Fork(ctx context.Context, vmID string, req ForkRequest) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.fail[vmID] || f.fail["*"] {
		if f.unsettled {
			return context.DeadlineExceeded
		}
		return Settled(errors.New("child failed"))
	}
	return f.inner.Fork(ctx, vmID, req)
}

func childIDs(fm *FleetManager, srcID, key string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fm.prefix + forkTaskID(srcID, key, i)
	}
	return out
}

// a fork of three makes three running children from one seed snapshot of the
// source, each with lineage pointing at that seed.
func TestForkEnvironment_countSharesOneSeed(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newForkTestProvider(), Prefix: "fuse-"})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	ctx := context.Background()

	ids, err := fm.ForkEnvironment(ctx, srcID, ForkOptions{Count: 3, Comment: "fan"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("%d children, want 3", len(ids))
	}
	seeds, _ := fm.ListSnapshots(ctx, srcID)
	if len(seeds) != 1 {
		t.Fatalf("%d snapshots of the source, want the one shared seed", len(seeds))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || id == srcID {
			t.Fatalf("child id %s repeats", id)
		}
		seen[id] = true
		if info, ok := fm.GetVM(id); !ok || info.State != VMStateRunning {
			t.Fatalf("child %s = %+v, want running", id, info)
		}
		lineage, _ := fm.ListSnapshots(ctx, id)
		if len(lineage) != 1 || lineage[0].ParentSnapshotID != seeds[0].SnapshotID {
			t.Fatalf("child %s lineage %+v, want one record on seed %s", id, lineage, seeds[0].SnapshotID)
		}
	}
}

// the same idempotency key and count names the same children, and child 0
// keeps the id a single fork under that key always had.
func TestForkEnvironment_countIDsFollowTheKey(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newForkTestProvider(), Prefix: "fuse-"})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	ctx := WithIdempotencyKey(context.Background(), "key-1")

	ids, err := fm.ForkEnvironment(ctx, srcID, ForkOptions{Count: 2})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	want := childIDs(fm, srcID, "key-1", 2)
	if ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("children %v, want %v", ids, want)
	}
	sum := forkTaskID(srcID, "key-1", 0)
	if sum != forkTaskID(srcID, "key-1", 0) || sum == forkTaskID(srcID, "key-1", 1) {
		t.Fatal("child ids are not stable per index")
	}

	again, err := fm.ForkEnvironment(ctx, srcID, ForkOptions{Count: 2})
	if err != nil {
		t.Fatalf("replayed fork: %v", err)
	}
	if again[0] != ids[0] || again[1] != ids[1] {
		t.Fatalf("replay children %v, want %v", again, ids)
	}
	if n := len(fm.ListFleet()); n != 3 {
		t.Fatalf("%d vms after a replay, want the source and its two children", n)
	}
	seeds, _ := fm.ListSnapshots(context.Background(), srcID)
	if len(seeds) != 1 {
		t.Fatalf("%d seeds after a replay, want one", len(seeds))
	}
}

// a failed child leaves its siblings running and the shared seed in place.
func TestForkEnvironment_failedChildKeepsSiblings(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newForkTestProvider(), Prefix: "fuse-"})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	ctx := WithIdempotencyKey(context.Background(), "key-1")
	ids := childIDs(fm, srcID, "key-1", 3)
	f := &failingForker{inner: fm.durableForker(), fail: map[string]bool{ids[1]: true}}
	fm.SetDurableForker(f)

	got, err := fm.ForkEnvironment(ctx, srcID, ForkOptions{Count: 3})
	if err == nil {
		t.Fatal("fork with a failed child succeeded")
	}
	// a settled answer would move the caller's retry to a new key, and so to a
	// new set of children; with survivors it has to stay on this one.
	if IsSettled(err) {
		t.Fatalf("err %v is settled, want a retry to replay these children", err)
	}
	if len(got) != 2 || got[0] != ids[0] || got[1] != ids[2] {
		t.Fatalf("forked %v, want the two siblings %v and %v", got, ids[0], ids[2])
	}
	for _, id := range got {
		if info, ok := fm.GetVM(id); !ok || info.State != VMStateRunning {
			t.Fatalf("sibling %s = %+v, want running", id, info)
		}
	}
	seeds, _ := fm.ListSnapshots(context.Background(), srcID)
	if len(seeds) != 1 {
		t.Fatalf("%d seeds, want the shared seed kept for the siblings", len(seeds))
	}
	for _, req := range f.calls {
		if req.ReuseSnapshotID != seeds[0].SnapshotID {
			t.Fatalf("child request %+v, want it to reuse seed %s", req, seeds[0].SnapshotID)
		}
	}
}

// the seed a fan-out took is deleted once every child failed for good, and
// kept while any child may still be running on it.
func TestForkEnvironment_seedDeletedOnlyWhenEveryChildFailed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unsettled bool
		seeds     int
	}{
		{"settled", false, 0},
		{"unsettled", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm := newInlineFleet(FleetConfig{Provider: newForkTestProvider(), Prefix: "fuse-"})
			srcID := provisionSnapshotTestVM(t, fm, "task-1")
			fm.SetDurableForker(&failingForker{inner: fm.durableForker(), fail: map[string]bool{"*": true}, unsettled: tc.unsettled})

			got, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{Count: 2})
			if err == nil || len(got) != 0 {
				t.Fatalf("fork = (%v, %v), want every child failed", got, err)
			}
			if IsSettled(err) == tc.unsettled {
				t.Fatalf("settled = %v, want %v", IsSettled(err), !tc.unsettled)
			}
			seeds, _ := fm.ListSnapshots(context.Background(), srcID)
			if len(seeds) != tc.seeds {
				t.Fatalf("%d seeds left, want %d", len(seeds), tc.seeds)
			}
		})
	}
}

// a live fork resumes each child from the seed's memory image on a host other
// than its source's, which cannot resume next to its running source yet.
func TestForkEnvironment_liveResumesOffTheSourceHost(t *testing.T) {
	provider := newLiveMigrateProvider()
	fm := newLiveMigrateFleet(t, provider, &recordingMover{})
	ctx := context.Background()

	ids, err := fm.ForkEnvironment(ctx, "fuse-task-src", ForkOptions{Live: true, Count: 2})
	if err != nil {
		t.Fatalf("live fork: %v", err)
	}
	seeds, _ := fm.ListSnapshots(ctx, "fuse-task-src")
	if len(seeds) != 1 || seeds[0].Kind != SnapshotKindLive {
		t.Fatalf("source snapshots %+v, want one live seed", seeds)
	}
	for _, id := range ids {
		info, _ := fm.GetVM(id)
		if info.HostID != "h-b" || !info.Spec.ResumeSeed || info.Spec.SeedSnapshotID == "" {
			t.Fatalf("child %s = host %q resume %v seed %q, want a resume on h-b",
				id, info.HostID, info.Spec.ResumeSeed, info.Spec.SeedSnapshotID)
		}
	}
}

// a live fork with only its source's host to land on is a 409, before the
// seed snapshot pauses the source.
func TestForkEnvironment_liveRefusedOnTheSourceHostAlone(t *testing.T) {
	provider := newLiveMigrateProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-", ArtifactMover: &recordingMover{}})
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, artifactHost("h-a", 8), provider); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src",
		Spec{CPUs: 1, RamMB: 512, HostID: "h-a"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision source: %v", err)
	}

	for _, count := range []int{1, 3} {
		_, err := fm.ForkEnvironment(ctx, "fuse-task-src", ForkOptions{Live: true, Count: count})
		if !errors.Is(err, ErrSnapshotNotSeedable) {
			t.Fatalf("count %d: err = %v, want ErrSnapshotNotSeedable", count, err)
		}
	}
	if n := len(fm.ListFleet()); n != 1 {
		t.Fatalf("%d vms, want only the source", n)
	}
	if seeds, _ := fm.ListSnapshots(ctx, "fuse-task-src"); len(seeds) != 0 {
		t.Fatalf("%d snapshots, want none for a refused fork", len(seeds))
	}
}

// reusing a live snapshot is a live fork's to do: a cold fork still refuses
// it, and a live fork refuses a disk snapshot.
func TestForkEnvironment_liveReuseMatchesTheSeedKind(t *testing.T) {
	provider := newLiveMigrateProvider()
	fm := newLiveMigrateFleet(t, provider, &recordingMover{})
	ctx := context.Background()

	live, err := fm.CreateSnapshot(ctx, "fuse-task-src", SnapshotOptions{Live: true})
	if err != nil || live.Kind != SnapshotKindLive {
		t.Fatalf("live seed = (%+v, %v)", live, err)
	}
	disk, err := fm.CreateSnapshot(ctx, "fuse-task-src", SnapshotOptions{})
	if err != nil {
		t.Fatalf("disk seed: %v", err)
	}

	if _, err := forkOne(ctx, fm, "fuse-task-src", ForkOptions{ReuseSnapshotID: live.SnapshotID}); !errors.Is(err, ErrSnapshotNotSeedable) {
		t.Fatalf("cold fork of a live snapshot: err = %v, want ErrSnapshotNotSeedable", err)
	}
	if _, err := forkOne(ctx, fm, "fuse-task-src", ForkOptions{ReuseSnapshotID: disk.SnapshotID, Live: true}); !errors.Is(err, ErrSnapshotNotSeedable) {
		t.Fatalf("live fork of a disk snapshot: err = %v, want ErrSnapshotNotSeedable", err)
	}
	id, err := forkOne(ctx, fm, "fuse-task-src", ForkOptions{ReuseSnapshotID: live.SnapshotID, Live: true})
	if err != nil {
		t.Fatalf("live fork of a live snapshot: %v", err)
	}
	if info, _ := fm.GetVM(id); !info.Spec.ResumeSeed {
		t.Fatalf("child %+v, want resumed", info.Spec)
	}
}

// count past the bound is refused before anything runs.
func TestForkEnvironment_countBound(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newForkTestProvider(), Prefix: "fuse-"})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	if _, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{Count: MaxForkCount + 1}); err == nil {
		t.Fatal("fork over MaxForkCount succeeded")
	}
	if n := len(fm.ListFleet()); n != 1 {
		t.Fatalf("%d vms, want only the source", n)
	}
}
