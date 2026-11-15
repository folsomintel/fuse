package durable

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// restateTerminal is a failure no retry can change, as a setup step that
// exited non-zero is.
func restateTerminal(msg string) error { return restate.ToTerminalError(errors.New(msg)) }

// buildFleet scripts the build steps: it records each one, can block one
// until released, and fails one with its scripted errors in turn. done gets
// the outcome when the build is recorded finished or failed.
type buildFleet struct {
	fakeFleet

	mu       sync.Mutex
	calls    []string
	fail     map[string][]error
	block    map[string]chan struct{}
	entered  chan string
	done     chan string
	cause    string
	outcome  string
	artifact string
}

func newBuildFleet() *buildFleet {
	return &buildFleet{
		fail:    map[string][]error{},
		block:   map[string]chan struct{}{},
		entered: make(chan string, 16),
		done:    make(chan string, 1),
	}
}

func (f *buildFleet) enter(ctx context.Context, step string) error {
	f.mu.Lock()
	f.calls = append(f.calls, step)
	block := f.block[step]
	f.mu.Unlock()
	if block != nil {
		select {
		case f.entered <- step:
		default:
		}
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if errs := f.fail[step]; len(errs) > 0 {
		err := errs[0]
		if len(errs) > 1 {
			f.fail[step] = errs[1:]
		} else if errs[0] != errAlways {
			delete(f.fail, step)
		}
		return err
	}
	return nil
}

func (f *buildFleet) steps() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *buildFleet) BuildResolveCache(ctx context.Context, _ orchestrator.BuildInput) (orchestrator.BuildPlan, error) {
	return orchestrator.BuildPlan{Arch: "amd64", Execs: []orchestrator.BuildExec{
		{Step: "layer_0", Label: "setup[0]", From: 0, To: 1, Files: true, Layer: 0},
		{Step: "tail", Label: "setup tail", From: 1, To: 2, Layer: -1},
	}}, f.enter(ctx, "resolve_cache")
}

func (f *buildFleet) BuildCreateBuilder(ctx context.Context, in orchestrator.BuildInput, _ orchestrator.BuildPlan) (string, error) {
	return "fuse-build-" + in.ID, f.enter(ctx, "create_builder")
}

func (f *buildFleet) BuildExec(ctx context.Context, _ orchestrator.BuildInput, _ string, ex orchestrator.BuildExec) error {
	return f.enter(ctx, ex.Step)
}

func (f *buildFleet) BuildSnapshotLayer(ctx context.Context, _ orchestrator.BuildInput, _ string, layer int) (string, error) {
	return fmt.Sprintf("snap-layer-%d", layer), f.enter(ctx, fmt.Sprintf("snapshot_layer_%d", layer))
}

func (f *buildFleet) BuildSnapshot(ctx context.Context, _ orchestrator.BuildInput, _ string) (string, error) {
	return "snap-final", f.enter(ctx, "snapshot")
}

func (f *buildFleet) BuildDestroyBuilder(ctx context.Context, _ orchestrator.BuildInput, outcome string) error {
	f.mu.Lock()
	f.outcome = outcome
	f.mu.Unlock()
	return f.enter(ctx, "destroy_builder")
}

func (f *buildFleet) BuildFinish(ctx context.Context, _ orchestrator.BuildInput, snapshotID string) error {
	if err := f.enter(ctx, "finish"); err != nil {
		return err
	}
	f.mu.Lock()
	f.artifact = snapshotID
	f.mu.Unlock()
	f.done <- "succeeded"
	return nil
}

func (f *buildFleet) BuildFailed(ctx context.Context, _ orchestrator.BuildInput, cause string) error {
	if err := f.enter(ctx, "failed"); err != nil {
		return err
	}
	f.mu.Lock()
	f.cause = cause
	f.mu.Unlock()
	f.done <- "failed"
	return nil
}

// serveBuildFleet serves fleet at addr ("" picks a port) with the fast policy
// on the Build workflow, and returns a client, the address and a stop that
// drops the endpoint the way a kill -9 of the orchestrator would.
func serveBuildFleet(t *testing.T, fleet Fleet, addr string) (*Client, string, func()) {
	t.Helper()
	admin, ingressURL := restateTestURLs(t)
	if addr == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		_ = ln.Close()
	}
	cfg := Config{Mode: ModeSelf, Listen: addr, IngressURL: ingressURL, AdminURL: admin,
		Retry: map[string]RetryPolicy{BuildWorkflow: fastPolicy}}
	h, err := Handler(cfg, fleet, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	protocols.SetHTTP1(true)
	srv := &http.Server{Handler: h, Protocols: &protocols}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Register(ctx, "http://"+addr); err != nil {
		t.Fatalf("register: %v", err)
	}
	return client, addr, func() { _ = srv.Close() }
}

func uniqueBuild(name string) orchestrator.BuildInput {
	return orchestrator.BuildInput{ID: fmt.Sprintf("bld_%s_%d", name, time.Now().UnixNano()), Name: name}
}

func waitOutcome(t *testing.T, f *buildFleet) string {
	t.Helper()
	select {
	case out := <-f.done:
		return out
	case <-time.After(30 * time.Second):
		t.Fatalf("build did not finish; steps %s", f.steps())
		return ""
	}
}

// TestBuildThroughRestate: a cold build runs each step once, in order, with a
// transient exec failure retried inside its step, and destroys its builder
// before it is recorded finished.
func TestBuildThroughRestate(t *testing.T) {
	fleet := newBuildFleet()
	fleet.fail["layer_0"] = []error{errors.New("agent 503")}
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := uniqueBuild("cold")
	if err := client.SubmitBuild(ctx, in); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// a second submit of the same id attaches to the first.
	if err := client.SubmitBuild(ctx, in); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if out := waitOutcome(t, fleet); out != "succeeded" {
		t.Fatalf("outcome %s", out)
	}
	want := "resolve_cache,create_builder,layer_0,layer_0,snapshot_layer_0,tail,snapshot,destroy_builder,finish"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if fleet.outcome != orchestrator.BuildSucceeded || fleet.artifact != "snap-final" {
		t.Fatalf("destroy outcome %q, finish artifact %q", fleet.outcome, fleet.artifact)
	}
}

// TestBuildFailedStepCompensates: a setup step that fails for good destroys
// the builder, records the failure, and takes no artifact.
func TestBuildFailedStepCompensates(t *testing.T) {
	fleet := newBuildFleet()
	fleet.fail["tail"] = []error{restateTerminal("setup tail exited 1")}
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.SubmitBuild(ctx, uniqueBuild("fail")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if out := waitOutcome(t, fleet); out != "failed" {
		t.Fatalf("outcome %s", out)
	}
	want := "resolve_cache,create_builder,layer_0,snapshot_layer_0,tail,destroy_builder,failed"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if fleet.outcome != orchestrator.BuildFailed {
		t.Fatalf("destroy outcome %q, want failed", fleet.outcome)
	}
	if !strings.Contains(fleet.cause, "setup tail exited 1") {
		t.Fatalf("cause %q", fleet.cause)
	}
}

// TestBuildCompensatesWhenRetriesRunOut: a create that never succeeds runs
// out of the policy's attempts and still destroys whatever it left.
func TestBuildCompensatesWhenRetriesRunOut(t *testing.T) {
	fleet := newBuildFleet()
	fleet.fail["create_builder"] = []error{errAlways}
	client, _, _ := serveBuildFleet(t, fleet, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.SubmitBuild(ctx, uniqueBuild("exhaust")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if out := waitOutcome(t, fleet); out != "failed" {
		t.Fatalf("outcome %s", out)
	}
	want := "resolve_cache," + strings.Repeat("create_builder,", fastPolicy.MaxAttempts) + "destroy_builder,failed"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

// TestBuildSurvivesEndpointRestart stands in for kill -9 of the orchestrator
// mid-layer: the endpoint drops while the tail runs, and the restarted one
// resumes at that layer without resolving the cache, creating a builder or
// snapshotting the first layer again.
func TestBuildSurvivesEndpointRestart(t *testing.T) {
	first := newBuildFleet()
	first.block["tail"] = make(chan struct{})
	defer close(first.block["tail"])
	client, addr, stop := serveBuildFleet(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.SubmitBuild(ctx, uniqueBuild("restart")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-first.entered:
	case <-time.After(20 * time.Second):
		t.Fatalf("tail never started; steps %s", first.steps())
	}
	stop()

	second := newBuildFleet()
	_, _, _ = serveBuildFleet(t, second, addr)
	if out := waitOutcome(t, second); out != "succeeded" {
		t.Fatalf("outcome %s", out)
	}
	if got, want := second.steps(), "tail,snapshot,destroy_builder,finish"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
}
