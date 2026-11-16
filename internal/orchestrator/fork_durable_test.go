package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

var forkTestKey = bytes.Repeat([]byte{7}, 32)

// stepForker runs a durable fork's steps in process, in the handler's order,
// each one twice when twice is set, the way a retry after a lost journal
// entry runs a step again.
type stepForker struct {
	fm    *FleetManager
	twice bool
	// stopAfter ends the fork after the named step, as a crash would.
	stopAfter string
	plan      ForkPlan
}

var errStopped = errors.New("stopped")

func (s *stepForker) Fork(ctx context.Context, vmID string, req ForkRequest) error {
	fm := s.fm
	plan, err := fm.ForkBegin(ctx, vmID, req)
	if err != nil {
		return err
	}
	s.plan = plan
	var (
		seed    = plan.Seed
		hostID  string
		localID string
		started MigrateStarted
	)
	steps := []struct {
		name string
		fn   func() error
	}{
		{"placeholder", func() error { return fm.ForkPlaceholder(ctx, plan) }},
		{"seed", func() error {
			if plan.Seed.SnapshotID != "" {
				return nil
			}
			seed, err = fm.snapshotInProcess(ctx, plan.SourceVMID, SnapshotOptions{Comment: plan.Comment})
			return err
		}},
		{"place", func() error { hostID, err = fm.ForkPlace(ctx, plan, seed); return err }},
		{"artifact", func() error {
			if hostID == plan.SourceHostID {
				return nil
			}
			localID, err = fm.ForkArtifact(ctx, plan, seed, hostID)
			return err
		}},
		{"create", func() error { return fm.ForkCreate(ctx, plan, seed, hostID, localID) }},
		{"ingress", func() error { started.Ingress, err = fm.ForkIngress(ctx, plan); return err }},
		{"credentials", func() error { started.Credentials, err = fm.ForkCredentials(ctx, plan); return err }},
		{"upload", func() error { return fm.ForkUpload(ctx, plan, started) }},
		{"start_agent", func() error { return fm.ForkStartAgent(ctx, plan, started) }},
		{"running", func() error { return fm.ForkRunning(ctx, plan, started) }},
		{"task", func() error { return fm.ForkTask(ctx, plan) }},
		{"lineage", func() error { return fm.ForkLineage(ctx, plan, seed) }},
	}
	for _, st := range steps {
		runs := 1
		// the seed and the create are never run twice: a durable retry of
		// either gets the host agent's first answer back by its key, which
		// these test hosts do not keep.
		if s.twice && st.name != "seed" && st.name != "create" {
			runs = 2
		}
		for range runs {
			if err := st.fn(); err != nil {
				return err
			}
		}
		if st.name == s.stopAfter {
			return errStopped
		}
	}
	return nil
}

func newDurableForkFleet(t *testing.T, store StateStore) (*FleetManager, *forkTestProvider, string) {
	t.Helper()
	provider := newForkTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, StateStore: store, Prefix: "fuse-", TokenEncryptionKey: forkTestKey})
	return fm, provider, provisionSnapshotTestVM(t, fm, "task-1")
}

// a durable fork whose every step runs twice still ends with one running vm,
// a token of its own, one seed, one lineage record and its task running.
func TestDurableFork_stepsAreRetrySafe(t *testing.T) {
	store := NewMemoryStateStore()
	fm, provider, srcID := newDurableForkFleet(t, store)
	fm.SetDurableForker(&stepForker{fm: fm, twice: true})

	ctx := context.Background()
	newID, err := fm.ForkEnvironment(ctx, srcID, ForkOptions{Comment: "forked"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !strings.HasPrefix(newID, "fuse-"+forkTaskPrefix) {
		t.Fatalf("fork id %q, want a fork task id", newID)
	}
	info, ok := fm.GetVM(newID)
	if !ok || info.State != VMStateRunning {
		t.Fatalf("fork %+v, want running", info)
	}
	if _, err := provider.Get(ctx, newID); err != nil {
		t.Fatalf("host has no fork: %v", err)
	}
	rows, _ := store.ListVMs(ctx)
	var row VMRecord
	for _, r := range rows {
		if r.ID == newID {
			row = r
		}
	}
	if row.State != VMStateRunning || len(row.AuthTokenEncrypted) == 0 {
		t.Fatalf("fork row %+v, want running with a token of its own", row)
	}
	for _, r := range rows {
		if r.ID == srcID && bytes.Equal(r.AuthTokenEncrypted, row.AuthTokenEncrypted) {
			t.Fatal("fork shares the source's token")
		}
	}
	env := provider.envs[newID]
	env.mu.Lock()
	token := string(env.files[fuseAuthTokenPath])
	env.mu.Unlock()
	if token == "" {
		t.Fatal("fork guest never got its own token")
	}
	seeds, _ := fm.ListSnapshots(ctx, srcID)
	lineage, _ := fm.ListSnapshots(ctx, newID)
	if len(seeds) != 1 || len(lineage) != 1 || lineage[0].ParentSnapshotID != seeds[0].SnapshotID {
		t.Fatalf("seeds %+v lineage %+v, want one seed and one child", seeds, lineage)
	}
	tasks, _ := store.ListTasks(ctx)
	var running int
	for _, task := range tasks {
		if task.VMID == newID && task.RunStatus == TaskRunRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("tasks %+v, want the fork's running", tasks)
	}
	spec := fm.vms[newID].spec
	if len(spec.Ancestors) != 1 || spec.Ancestors[0] != srcID {
		t.Fatalf("fork ancestors %v, want the source", spec.Ancestors)
	}
}

// the placeholder is persisted provisioning before anything exists on the
// host, and a restart with restate on keeps it provisioning for the fork to
// finish rather than promoting or tearing it down.
func TestDurableFork_placeholderSurvivesRecovery(t *testing.T) {
	store := NewMemoryStateStore()
	fm, provider, srcID := newDurableForkFleet(t, store)
	forker := &stepForker{fm: fm, stopAfter: "create"}
	fm.SetDurableForker(forker)
	if _, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{}); !errors.Is(err, errStopped) {
		t.Fatalf("fork: %v, want the stop", err)
	}
	newID := forker.plan.NewVMID

	fm2 := NewFleetManager(FleetConfig{Provider: provider, StateStore: store, Prefix: "fuse-", TokenEncryptionKey: forkTestKey})
	fm2.SetDurableForker(&stepForker{fm: fm2})
	if err := fm2.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, ok := fm2.GetVM(newID)
	if !ok || info.State != VMStateProvisioning {
		t.Fatalf("recovered fork %+v, want provisioning", info)
	}
}

// the rollback drops the placeholder and its row, and deletes the seed the
// fork took.
func TestDurableFork_abandonLeavesNothingTracked(t *testing.T) {
	store := NewMemoryStateStore()
	fm, _, srcID := newDurableForkFleet(t, store)
	forker := &stepForker{fm: fm, stopAfter: "create"}
	fm.SetDurableForker(forker)
	_, _ = fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	plan := forker.plan

	ctx := context.Background()
	seeds, _ := fm.ListSnapshots(ctx, srcID)
	if len(seeds) != 1 {
		t.Fatalf("seeds %+v, want the one the fork took", seeds)
	}
	if err := fm.ForkUnpublishIngress(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := fm.ForkAbandon(ctx, plan, "agent refused"); err != nil {
		t.Fatal(err)
	}
	if err := fm.ForkDeleteSeed(ctx, seeds[0]); err != nil {
		t.Fatal(err)
	}
	if _, ok := fm.GetVM(plan.NewVMID); ok {
		t.Fatal("abandoned fork still tracked")
	}
	rows, _ := store.ListVMs(ctx)
	for _, r := range rows {
		if r.ID == plan.NewVMID {
			t.Fatalf("abandoned fork row %+v left behind", r)
		}
	}
	if seeds, _ := fm.ListSnapshots(ctx, srcID); len(seeds) != 0 {
		t.Fatalf("seeds %+v left behind", seeds)
	}
	// a replayed abandon finds nothing and succeeds.
	if err := fm.ForkAbandon(ctx, plan, "agent refused"); err != nil {
		t.Fatal(err)
	}
}

// begin refuses what the in-process fork refuses, before writing anything.
func TestDurableFork_beginRefusesGPU(t *testing.T) {
	store := NewMemoryStateStore()
	fm, _, srcID := newDurableForkFleet(t, store)
	fm.mu.Lock()
	fm.vms[srcID].spec.GPUs = 1
	fm.mu.Unlock()
	fm.SetDurableForker(&stepForker{fm: fm})
	if _, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{}); !errors.Is(err, ErrGPUUnsupported) {
		t.Fatalf("fork: %v, want ErrGPUUnsupported", err)
	}
	if got := len(fm.ListFleet()); got != 1 {
		t.Fatalf("fleet has %d vms, want the source only", got)
	}
}

// a retry carrying the same idempotency key names the same fork, so restate
// attaches it to the first request's invocation.
func TestForkTaskID_followsIdempotencyKey(t *testing.T) {
	a, b := forkTaskID("fuse-src", "key-1"), forkTaskID("fuse-src", "key-1")
	if a != b {
		t.Fatalf("same key named %s and %s", a, b)
	}
	if forkTaskID("fuse-src", "key-2") == a || forkTaskID("fuse-other", "key-1") == a {
		t.Fatal("a different key or source named the same fork")
	}
	if c, d := forkTaskID("fuse-src", ""), forkTaskID("fuse-src", ""); c == d {
		t.Fatal("forks without a key share a name")
	}
}
