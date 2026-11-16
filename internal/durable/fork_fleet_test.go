package durable

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/firecracker"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// forkHost is a stub firecracker host. a vm forked from a checkpoint holds
// its uploads until open is closed, and with refuseAgent its guest agent
// refuses to start for good.
type forkHost struct {
	*firecracker.Provider
	open        chan struct{}
	entered     chan struct{}
	refuseAgent bool
}

func (h *forkHost) CreateFromCheckpoint(ctx context.Context, spec orchestrator.Spec, srcVMID, checkpointID string) (orchestrator.Environment, error) {
	env, err := h.Provider.CreateFromCheckpoint(ctx, spec, srcVMID, checkpointID)
	if err != nil {
		return nil, err
	}
	return &gatedEnv{Environment: env, p: &gatedProvider{open: h.open, entered: h.entered}}, nil
}

func (h *forkHost) Get(ctx context.Context, name string) (orchestrator.Environment, error) {
	env, err := h.Provider.Get(ctx, name)
	if err != nil || !h.refuseAgent || !strings.Contains(name, "fork-") {
		return env, err
	}
	return refusingEnv{env}, nil
}

type refusingEnv struct{ orchestrator.Environment }

func (refusingEnv) StartAgent(context.Context, orchestrator.AgentSpec) error {
	return &orchestrator.HTTPStatusError{Code: 400, Body: "agent refused"}
}

// startForkFleet runs a source vm on a real fleet over host and forks it
// until the fork's first upload, then stops the orchestrator there, as kill -9
// would. it returns the source's id and the restate endpoint's address.
func startForkFleet(t *testing.T, ctx context.Context, store orchestrator.StateStore, host *forkHost) (string, string) {
	t.Helper()
	first, addr, stopFirst := durableFleet(t, store, host, "")
	task := uniqueVM("src")[len("fuse-"):]
	src, err := first.ProvisionAndAssign(ctx, task, orchestrator.Spec{CPUs: 1, RamMB: 256}, []byte(`{"version":"1"}`), nil, orchestrator.BootOptions{})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	go func() { _, _ = first.ForkEnvironment(ctx, src.ID, orchestrator.ForkOptions{Comment: "seed"}) }()
	select {
	case <-host.entered:
	case <-ctx.Done():
		t.Fatal("fork never reached the upload")
	}
	stopFirst()
	return src.ID, addr
}

// forkOf is the one vm the fleet tracks other than src.
func forkOf(fm *orchestrator.FleetManager, src string) (orchestrator.VMInfo, bool) {
	for _, v := range fm.ListFleet() {
		if v.ID != src {
			return v, true
		}
	}
	return orchestrator.VMInfo{}, false
}

// TestForkFleetSurvivesRestart: the orchestrator dies after the fork exists
// on its host and before its credentials reach it. the restarted
// orchestrator keeps the fork provisioning, restate resumes it at the upload,
// and it ends up running with its own token and its lineage.
func TestForkFleetSurvivesRestart(t *testing.T) {
	store := orchestrator.NewMemoryStateStore()
	host := &forkHost{Provider: firecracker.New(firecracker.Config{UseStub: true}), open: make(chan struct{}), entered: make(chan struct{}, 1)}
	defer close(host.open)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, addr := startForkFleet(t, ctx, store, host)

	second, _, stopSecond := durableFleet(t, store, host.Provider, addr)
	defer stopSecond()
	v, ok := forkOf(second, src)
	if !ok || v.State != orchestrator.VMStateProvisioning {
		t.Fatalf("recovered fork %+v, want provisioning", v)
	}
	for v.State != orchestrator.VMStateRunning {
		if ctx.Err() != nil {
			t.Fatalf("fork never reached running after the restart: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
		v, _ = second.GetVM(v.ID)
	}

	env, err := host.Provider.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	srcEnv, err := host.Provider.Get(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if env.Token() == "" || env.Token() == srcEnv.Token() {
		t.Fatal("fork does not answer to a token of its own")
	}
	lineage, err := second.ListSnapshots(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	seeds, _ := second.ListSnapshots(ctx, src)
	if len(lineage) != 1 || len(seeds) != 1 || lineage[0].ParentSnapshotID != seeds[0].SnapshotID {
		t.Fatalf("fork lineage %+v, source snapshots %+v, want one seed and its child", lineage, seeds)
	}
}

// TestForkFleetRollsBackAfterRestart: the fork resumed after a restart fails
// for good, and nothing of it is left tracked: no placeholder, no row, no
// seed. on a fleet with no hosts the rollback's destroy names no host, so the
// vm itself goes with reconcile's orphan sweep; TestForkRollsBackThroughRestate
// covers the destroy on a placed fork.
func TestForkFleetRollsBackAfterRestart(t *testing.T) {
	store := orchestrator.NewMemoryStateStore()
	host := &forkHost{Provider: firecracker.New(firecracker.Config{UseStub: true}), open: make(chan struct{}), entered: make(chan struct{}, 1)}
	defer close(host.open)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, addr := startForkFleet(t, ctx, store, host)

	host.refuseAgent = true
	second, _, stopSecond := durableFleet(t, store, host, addr)
	defer stopSecond()
	for {
		_, tracked := forkOf(second, src)
		seeds, _ := second.ListSnapshots(ctx, src)
		if !tracked && len(seeds) == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("fork never rolled back: tracked %v, seeds %+v", tracked, seeds)
		}
		time.Sleep(50 * time.Millisecond)
	}
	rows, err := store.ListVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != src {
		t.Fatalf("vm rows %+v, want the source's only", rows)
	}
}
