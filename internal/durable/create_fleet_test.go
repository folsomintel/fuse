package durable

import (
	"context"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/firecracker"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// gatedProvider is a host whose guests hold every upload until open is
// closed, so a test can stop the orchestrator between the create on the host
// and the upload.
type gatedProvider struct {
	orchestrator.Provider
	open    chan struct{}
	entered chan struct{}
}

func (p *gatedProvider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	env, err := p.Provider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &gatedEnv{Environment: env, p: p}, nil
}

func (p *gatedProvider) Get(ctx context.Context, name string) (orchestrator.Environment, error) {
	env, err := p.Provider.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return &gatedEnv{Environment: env, p: p}, nil
}

type gatedEnv struct {
	orchestrator.Environment
	p *gatedProvider
}

func (e *gatedEnv) Upload(ctx context.Context, data []byte, path string) error {
	select {
	case e.p.entered <- struct{}{}:
	default:
	}
	select {
	case <-e.p.open:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.Environment.Upload(ctx, data, path)
}

// durableFleet is a real fleet on restate over store and p, served at addr
// ("" picks one).
func durableFleet(t *testing.T, store orchestrator.StateStore, p orchestrator.Provider, addr string) (*orchestrator.FleetManager, string, func()) {
	t.Helper()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{Provider: p, StateStore: store, Prefix: "fuse-", TokenEncryptionKey: tokenTestKey})
	client, addr, stop := serveFleetAt(t, fm, addr)
	fm.SetDurableDestroyer(client)
	fm.SetDurableCreator(client)
	fm.SetDurableForker(client)
	ctx, cancel := context.WithCancel(context.Background())
	fm.Start(ctx)
	return fm, addr, func() {
		stop()
		cancel()
		fm.Stop()
	}
}

// TestCreateFleetSurvivesRestart runs a real fleet's create through restate
// and stops the orchestrator after the vm exists on its host and before any
// file reaches it, as kill -9 would. the restarted orchestrator keeps the vm
// provisioning, restate resumes the create at the upload, and the vm ends up
// running with its files and agent.
func TestCreateFleetSurvivesRestart(t *testing.T) {
	store := orchestrator.NewMemoryStateStore()
	host := firecracker.New(firecracker.Config{UseStub: true})
	gated := &gatedProvider{Provider: host, open: make(chan struct{}), entered: make(chan struct{}, 1)}
	defer close(gated.open)
	first, addr, stopFirst := durableFleet(t, store, gated, "")
	task := uniqueVM("t")[len("fuse-"):]
	vmID := "fuse-" + task

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		_, _ = first.ProvisionAndAssign(ctx, task, orchestrator.Spec{CPUs: 1, RamMB: 256}, []byte(`{"version":"1"}`),
			map[string]string{"API_KEY": "s3cret"}, orchestrator.BootOptions{Expose: []orchestrator.ExposeSpec{{Port: 8080}}})
	}()
	select {
	case <-gated.entered:
	case <-ctx.Done():
		t.Fatal("create never reached the upload")
	}
	stopFirst()

	second, _, stopSecond := durableFleet(t, store, host, addr)
	defer stopSecond()
	if v, ok := second.GetVM(vmID); !ok || v.State != orchestrator.VMStateProvisioning {
		t.Fatalf("recovered vm %+v, want provisioning", v)
	}
	var info orchestrator.VMInfo
	for {
		v, ok := second.GetVM(vmID)
		if ok && v.State == orchestrator.VMStateRunning {
			info = v
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("vm never reached running after the restart: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(info.Endpoints) != 1 {
		t.Fatalf("running vm endpoints %+v, want the agent's expose", info.Endpoints)
	}
	env, err := host.Get(ctx, vmID)
	if err != nil {
		t.Fatal(err)
	}
	if env.Token() == "" {
		t.Fatal("restarted create never installed the vm's token")
	}
	tasks, _ := store.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].RunStatus != orchestrator.TaskRunRunning {
		t.Fatalf("tasks %+v, want running", tasks)
	}
}
