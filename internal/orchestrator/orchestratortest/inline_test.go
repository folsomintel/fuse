package orchestratortest

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// testEnv is a sandbox that accepts every call.
type testEnv struct{ name string }

func (e *testEnv) Name() string  { return e.name }
func (e *testEnv) URL() string   { return "http://" + e.name + ".test" }
func (e *testEnv) Token() string { return "" }
func (e *testEnv) Exec(context.Context, []string, orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	return orchestrator.ExecResult{}, nil
}
func (e *testEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *testEnv) Upload(context.Context, []byte, string) error             { return nil }
func (e *testEnv) StartAgent(context.Context, orchestrator.AgentSpec) error { return nil }

// testProvider keeps its sandboxes in a map.
type testProvider struct {
	mu   sync.Mutex
	envs map[string]*testEnv
}

func (p *testProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &testEnv{name: spec.Name}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *testProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.envs[name]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("not found: %s", name)
}

func (p *testProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *testProvider) List(context.Context, string) ([]orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]orchestrator.Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (p *testProvider) Close() error { return nil }

func (p *testProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.envs)
}

// a vm created through the inline fleet runs, and its destroy removes it
// from the fleet and the host.
func TestInlineFleetCreateDestroy(t *testing.T) {
	p := &testProvider{envs: map[string]*testEnv{}}
	fm, d := NewFleet(orchestrator.FleetConfig{Provider: p, StateStore: orchestrator.NewMemoryStateStore(), Prefix: "fuse-"})
	ctx := context.Background()
	info, err := fm.ProvisionAndAssign(ctx, "task-1", orchestrator.Spec{}, []byte(`{}`), nil, orchestrator.BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got, ok := fm.GetVM(info.ID); !ok || got.State != orchestrator.VMStateRunning {
		t.Fatalf("vm %+v, want running", got)
	}
	if p.count() != 1 {
		t.Fatalf("provider has %d envs, want 1", p.count())
	}
	if inFlight, _ := d.CreateInFlight(ctx, info.ID); inFlight {
		t.Fatal("create still marked in flight after it finished")
	}
	if err := fm.DestroyVM(ctx, info.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	d.Wait()
	if p.count() != 0 || len(fm.ListFleet()) != 0 {
		t.Fatalf("provider has %d envs, fleet %d vms; want both empty", p.count(), len(fm.ListFleet()))
	}
}
