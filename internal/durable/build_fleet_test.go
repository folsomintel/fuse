package durable

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// guestEnv is a builder guest for a real FleetManager: it records each exec
// and can hold the exec whose script contains holdOn until its ctx ends, the
// way an exec in flight dies with the orchestrator.
type guestEnv struct {
	name   string
	holdOn string
	held   chan struct{}

	mu          sync.Mutex
	scripts     []string
	checkpoints []orchestrator.Checkpoint
}

func (e *guestEnv) Name() string  { return e.name }
func (e *guestEnv) URL() string   { return "http://" + e.name + ".test" }
func (e *guestEnv) Token() string { return "" }

func (e *guestEnv) Exec(ctx context.Context, cmd []string, _ orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	script := cmd[len(cmd)-1]
	e.mu.Lock()
	e.scripts = append(e.scripts, script)
	hold := e.holdOn != "" && strings.Contains(script, e.holdOn)
	if hold {
		// only the first attempt is held.
		e.holdOn = ""
	}
	e.mu.Unlock()
	if hold {
		close(e.held)
		<-ctx.Done()
		return orchestrator.ExecResult{}, ctx.Err()
	}
	return orchestrator.ExecResult{}, nil
}
func (e *guestEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *guestEnv) Upload(context.Context, []byte, string) error             { return nil }
func (e *guestEnv) StartAgent(context.Context, orchestrator.AgentSpec) error { return nil }

func (e *guestEnv) Checkpoint(_ context.Context, comment string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := orchestrator.Checkpoint{ID: fmt.Sprintf("%s-cp-%d", e.name, len(e.checkpoints)), Comment: comment, SizeBytes: 4096}
	e.checkpoints = append(e.checkpoints, cp)
	return cp.ID, nil
}
func (e *guestEnv) Restore(context.Context, string) error { return nil }
func (e *guestEnv) ListCheckpoints(context.Context) ([]orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]orchestrator.Checkpoint(nil), e.checkpoints...), nil
}

// guestProvider hands out guestEnvs and counts creates and destroys.
type guestProvider struct {
	holdOn string
	held   chan struct{}

	mu        sync.Mutex
	envs      map[string]*guestEnv
	creates   int
	destroyed []string
}

func (p *guestProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	e := &guestEnv{name: spec.Name, holdOn: p.holdOn, held: p.held}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *guestProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		return nil, orchestrator.ErrVMNotFound
	}
	return e, nil
}

func (p *guestProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, name)
	return nil
}

func (p *guestProvider) List(context.Context, string) ([]orchestrator.Environment, error) {
	return nil, nil
}
func (*guestProvider) Close() error { return nil }

type persistentStore struct{ *orchestrator.MemoryStateStore }

// TestBuildOnFleetResumesAfterEndpointRestart runs a real FleetManager's
// build through restate, layer snapshots and the builder destroy included,
// and drops the endpoint while the tail runs, standing in for kill -9 of the
// orchestrator mid-layer. the restarted endpoint resumes at that layer on the
// same builder: one builder is created, the first layer runs once, and the
// builder is destroyed at the end.
func TestBuildOnFleetResumesAfterEndpointRestart(t *testing.T) {
	p := &guestProvider{holdOn: "date >", held: make(chan struct{}), envs: map[string]*guestEnv{}}
	// a store that is not the memory one stands in for postgres: a durable
	// build needs a record that outlives the process.
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{Provider: p, Prefix: "fuse-",
		StateStore: persistentStore{orchestrator.NewMemoryStateStore()}})
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, orchestrator.Host{
		ID: "h1", URL: "http://h1.test", Backend: orchestrator.BackendFirecracker,
		Capacity: orchestrator.HostCapacity{CPUs: 8, RamMB: 8192, StorageGB: 100, VMCount: 10, Arch: "amd64"},
	}, p); err != nil {
		t.Fatal(err)
	}

	client, addr, stop := serveBuildFleet(t, fm, "")
	fm.SetDurableDestroyer(client)
	fm.SetDurableSnapshots(client)
	fm.SetDurableBuilds(client)

	in := orchestrator.BuildInput{
		ID:       fmt.Sprintf("bld_fleet_%d", time.Now().UnixNano()),
		Name:     "app",
		TenantID: "tenant-a",
		Spec:     orchestrator.Spec{CPUs: 1, RamMB: 256},
		Manifest: []byte(`{}`),
		Setup: fusefile.SetupPlan{
			Prelude: "set -eu\n",
			Steps:   []string{"apt-get install -y git", "date > /stamp"},
		},
		Keys: []string{"k0", ""},
	}
	if _, _, err := fm.SubmitBuild(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.held:
	case <-time.After(20 * time.Second):
		rec, _ := fm.GetBuild(ctx, in.ID)
		t.Fatalf("tail never started: %+v", rec)
	}
	stop()
	_, _, _ = serveBuildFleet(t, fm, addr)

	deadline := time.Now().Add(30 * time.Second)
	var rec orchestrator.BuildRecord
	for {
		var err error
		if rec, err = fm.GetBuild(ctx, in.ID); err != nil {
			t.Fatal(err)
		}
		if rec.State != orchestrator.BuildRunning || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec.State != orchestrator.BuildSucceeded {
		t.Fatalf("build %s at %s: %s", rec.State, rec.Step, rec.Error)
	}
	if rec.Layers[0].SnapshotID == "" || rec.SnapshotID == "" {
		t.Fatalf("record %+v, want a layer and an artifact", rec)
	}

	p.mu.Lock()
	creates, destroyed := p.creates, append([]string(nil), p.destroyed...)
	scripts := p.envs[rec.BuilderID].scripts
	p.mu.Unlock()
	if creates != 1 {
		t.Errorf("%d builders created, want 1", creates)
	}
	if fmt.Sprint(destroyed) != fmt.Sprint([]string{rec.BuilderID}) {
		t.Errorf("destroyed %v, want the builder %s", destroyed, rec.BuilderID)
	}
	want := []string{in.Setup.Range(0, 1, true), in.Setup.Range(1, 2, false), in.Setup.Range(1, 2, false)}
	if fmt.Sprint(scripts) != fmt.Sprint(want) {
		t.Errorf("execs %q, want the first layer once and the tail run again", scripts)
	}
	if _, ok := fm.GetVM(rec.BuilderID); ok {
		t.Errorf("builder %s still tracked", rec.BuilderID)
	}
}
