package durable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
)

// createFleet scripts the create steps and records the destroy a rollback
// sends. it reuses snapFleet's step script: fail[step] errors in turn, block
// holds a step.
type createFleet struct {
	*snapFleet

	destroyed chan string
}

func newCreateFleet() *createFleet {
	return &createFleet{snapFleet: newSnapFleet(), destroyed: make(chan string, 4)}
}

func (f *createFleet) CreateBegin(ctx context.Context, _ string, _ orchestrator.CreateRequest) (orchestrator.CreateState, error) {
	return orchestrator.CreateState{Credentials: []byte("sealed-creds")}, f.enter(ctx, "begin")
}

func (f *createFleet) CreatePlace(ctx context.Context, _ string, _ orchestrator.CreateRequest, st orchestrator.CreateState) (string, error) {
	if string(st.Credentials) != "sealed-creds" {
		return "", fmt.Errorf("place got state %+v, want begin's", st)
	}
	return "h1", f.enter(ctx, "place")
}

func (f *createFleet) CreateArtifact(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ orchestrator.CreateState) (string, error) {
	return "seed-on-h1", f.enter(ctx, "artifact")
}

func (f *createFleet) CreatePublishIngress(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ orchestrator.CreateState) (*orchestrator.CreateIngress, error) {
	return &orchestrator.CreateIngress{TunnelConfig: []byte("sealed-tunnel")}, f.enter(ctx, "ingress")
}

func (f *createFleet) CreateRestoreOrCreate(ctx context.Context, _ string, _ orchestrator.CreateRequest, st orchestrator.CreateState) (bool, error) {
	if st.HostID != "h1" || st.SeedSnapshotID != "seed-on-h1" || st.Ingress == nil || !st.Booted {
		return false, fmt.Errorf("boot got state %+v, want the placement and ingress", st)
	}
	return false, f.enter(ctx, "boot")
}

func (f *createFleet) CreateProvisionEgress(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ orchestrator.CreateState) error {
	return f.enter(ctx, "egress")
}

func (f *createFleet) CreateUpload(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ orchestrator.CreateState) error {
	return f.enter(ctx, "upload")
}

func (f *createFleet) CreateStartupScript(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ orchestrator.CreateState) error {
	return f.enter(ctx, "startup")
}

func (f *createFleet) CreateStartAgent(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ orchestrator.CreateState) ([]orchestrator.Endpoint, error) {
	return []orchestrator.Endpoint{{Port: 8080, URL: "http://x:1"}}, f.enter(ctx, "agent")
}

func (f *createFleet) CreateRunning(ctx context.Context, _ string, _ orchestrator.CreateRequest, st orchestrator.CreateState) error {
	if len(st.Endpoints) != 1 {
		return fmt.Errorf("running got state %+v, want the agent's endpoints", st)
	}
	return f.enter(ctx, "running")
}

func (f *createFleet) CreateReleaseEgress(ctx context.Context, _ string) error {
	return f.enter(ctx, "release_egress")
}

func (f *createFleet) CreateUnpublishIngress(ctx context.Context, _ string) error {
	return f.enter(ctx, "unpublish_ingress")
}

func (f *createFleet) CreateBeginFailed(ctx context.Context, _ string, _ orchestrator.CreateRequest, _ string) error {
	return f.enter(ctx, "begin_failed")
}

func (f *createFleet) CreateAbandon(ctx context.Context, _ string, _ orchestrator.CreateRequest, cause string) error {
	f.mu.Lock()
	f.failed = cause
	f.mu.Unlock()
	return f.enter(ctx, "abandon")
}

// DestroyOnHost records the host the rollback's destroy was sent for.
func (f *createFleet) DestroyOnHost(_ context.Context, vm, hostID string) error {
	f.destroyed <- vm + "@" + hostID
	return nil
}

func createRequest(t *testing.T) orchestrator.CreateRequest {
	t.Helper()
	sealed, err := secrets.EncryptBytes([]byte(`{"secrets":{"K":"plaintext-secret"}}`), tokenTestKey)
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator.CreateRequest{TaskID: "t", Sealed: sealed}
}

// TestCreateThroughRestate: a host create that fails twice is retried inside
// its step, the steps before it are not run again, and each step sees the
// state the earlier ones returned.
func TestCreateThroughRestate(t *testing.T) {
	fleet := newCreateFleet()
	fleet.fail["boot"] = []error{errors.New("agent 503"), errors.New("agent 503")}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Create(ctx, uniqueVM("create"), createRequest(t)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, want := fleet.steps(), "begin,place,artifact,ingress,boot,boot,boot,egress,upload,startup,agent,running"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

// TestCreateRollsBackThroughRestate: a startup script that fails for good
// releases egress, unpublishes ingress and abandons the vm, then destroys it
// on its host through Environment.Destroy; the caller gets the fleet's
// sentinel back.
func TestCreateRollsBackThroughRestate(t *testing.T) {
	fleet := newCreateFleet()
	fleet.fail["startup"] = []error{fmt.Errorf("provision vm x: %w after 1s", orchestrator.ErrStartupScriptTimeout)}
	client := serveFleet(t, fleet, fastPolicy)
	vm := uniqueVM("create-rollback")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := client.Create(ctx, vm, createRequest(t))
	if !errors.Is(err, orchestrator.ErrStartupScriptTimeout) {
		t.Fatalf("create: %v, want ErrStartupScriptTimeout", err)
	}
	if !strings.HasPrefix(err.Error(), "provision vm x: ") {
		t.Fatalf("error %q, want the fleet's message", err)
	}
	if got, want := fleet.steps(), "begin,place,artifact,ingress,boot,egress,upload,startup,release_egress,unpublish_ingress,abandon"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if !strings.Contains(fleet.failed, "startup script") {
		t.Fatalf("abandoned with %q, want the startup failure", fleet.failed)
	}
	select {
	case got := <-fleet.destroyed:
		if got != vm+"@h1" {
			t.Fatalf("destroyed %s, want %s@h1", got, vm)
		}
	case <-ctx.Done():
		t.Fatal("the rollback never destroyed the vm")
	}
}

// a create that fails before the boot step has nothing on the host, so the
// rollback abandons it without a destroy.
func TestCreateUnplaceableThroughRestate(t *testing.T) {
	fleet := newCreateFleet()
	fleet.fail["place"] = []error{fmt.Errorf("%w: no eligible host fits", orchestrator.ErrNoCapacity)}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Create(ctx, uniqueVM("create-noplace"), createRequest(t)); !errors.Is(err, orchestrator.ErrNoCapacity) {
		t.Fatalf("create: %v, want ErrNoCapacity", err)
	}
	if got, want := fleet.steps(), "begin,place,release_egress,unpublish_ingress,abandon"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	select {
	case got := <-fleet.destroyed:
		t.Fatalf("destroyed %s for a vm that never booted", got)
	case <-time.After(500 * time.Millisecond):
	}
}

// a step that keeps failing with a retryable error runs out of attempts and
// rolls the create back too.
func TestCreateRetriesRunOutThroughRestate(t *testing.T) {
	fleet := newCreateFleet()
	fleet.fail["agent"] = []error{errAlways}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Create(ctx, uniqueVM("create-agent"), createRequest(t)); err == nil {
		t.Fatal("create succeeded with an agent that never starts")
	}
	steps := fleet.steps()
	if !strings.HasSuffix(steps, strings.Repeat("agent,", fastPolicy.MaxAttempts)+"release_egress,unpublish_ingress,abandon") {
		t.Fatalf("steps %s, want %d agent attempts then the rollback", steps, fastPolicy.MaxAttempts)
	}
}

// TestCreateSurvivesEndpointRestart stands in for kill -9 of the
// orchestrator after the vm is created and before its files are uploaded:
// the endpoint drops while the upload runs, and the restarted one resumes at
// the upload and finishes the create. while the invocation is held there its
// journal is read: it holds the sealed request and never the plaintext.
func TestCreateSurvivesEndpointRestart(t *testing.T) {
	first := newCreateFleet()
	first.block["upload"] = make(chan struct{})
	defer close(first.block["upload"])
	client, addr, stop := serveFleetAt(t, first, "")
	vm := uniqueVM("create-restart")
	req := createRequest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Create(ctx, vm, req) }()
	<-first.entered

	journal := readJournalOf(t, ctx, EnvironmentObject, vm, "boot")
	if !bytes.Contains(journal, []byte(base64.StdEncoding.EncodeToString(req.Sealed))) && !bytes.Contains(journal, req.Sealed) {
		t.Fatalf("journal of %s does not hold the sealed request; the check below would be vacuous", vm)
	}
	if bytes.Contains(journal, []byte("plaintext-secret")) {
		t.Fatalf("journal of %s holds a plaintext secret", vm)
	}

	stop()
	second := newCreateFleet()
	_, _, _ = serveFleetAt(t, second, addr)
	if err := <-done; err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, want := second.steps(), "upload,startup,agent,running"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
}

// TestCreateJournalOverhead logs what restate adds to a create's happy path:
// the round trip through ingress and one journaled entry per step, on a
// create whose steps do nothing. a real boot takes seconds, so this is the
// number to hold against the 5% budget.
func TestCreateJournalOverhead(t *testing.T) {
	fleet := newCreateFleet()
	client := serveFleet(t, fleet, fastPolicy)
	req := orchestrator.CreateRequest{TaskID: "t", Sealed: []byte("x")}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const n = 50
	var total time.Duration
	for i := range n {
		start := time.Now()
		if err := client.Create(ctx, uniqueVM(fmt.Sprintf("create-bench-%d", i)), req); err != nil {
			t.Fatal(err)
		}
		total += time.Since(start)
	}
	t.Logf("restate create overhead: %s mean over %d creates of 11 journaled steps", total/n, n)
}

// a create holds its in-flight mark while it runs and drops it once it ends,
// so reconcile can tell it from a provisioning vm no create will finish. a
// begin that fails for good cleans up after itself without a rollback.
func TestCreateInFlightThroughRestate(t *testing.T) {
	fleet := newCreateFleet()
	fleet.block["upload"] = make(chan struct{})
	client := serveFleet(t, fleet, fastPolicy)
	vm := uniqueVM("create-mark")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Create(ctx, vm, createRequest(t)) }()
	<-fleet.entered
	if inFlight, err := client.CreateInFlight(ctx, vm); err != nil || !inFlight {
		t.Fatalf("in flight during the create: %v %v, want true", inFlight, err)
	}
	close(fleet.block["upload"])
	if err := <-done; err != nil {
		t.Fatalf("create: %v", err)
	}
	if inFlight, err := client.CreateInFlight(ctx, vm); err != nil || inFlight {
		t.Fatalf("in flight after the create: %v %v, want false", inFlight, err)
	}

	refused := newCreateFleet()
	refused.fail["begin"] = []error{fmt.Errorf("%w: task t", orchestrator.ErrTaskAlreadyAssigned)}
	client = serveFleet(t, refused, fastPolicy)
	if err := client.Create(ctx, uniqueVM("create-refused"), createRequest(t)); !errors.Is(err, orchestrator.ErrTaskAlreadyAssigned) {
		t.Fatalf("create: %v, want ErrTaskAlreadyAssigned", err)
	}
	if got := refused.steps(); got != "begin,begin_failed" {
		t.Fatalf("steps %s, want begin,begin_failed", got)
	}
}
