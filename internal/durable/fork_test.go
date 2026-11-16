package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// forkFleet scripts the fork steps on top of snapFleet, whose snapshot steps
// take the seed: fail[step] errors in turn, block holds a step. it records
// the destroy a rollback sends and the seed it deletes.
type forkFleet struct {
	*snapFleet

	reuse     orchestrator.SnapshotRecord
	destroyed chan string
	deleted   string
}

func newForkFleet() *forkFleet {
	return &forkFleet{snapFleet: newSnapFleet(), destroyed: make(chan string, 4)}
}

func (f *forkFleet) ForkBegin(ctx context.Context, vmID string, req orchestrator.ForkRequest) (orchestrator.ForkPlan, error) {
	plan := orchestrator.ForkPlan{SourceVMID: req.SourceVMID, SourceHostID: "h1", NewVMID: vmID, TaskID: req.TaskID, Comment: req.Comment}
	if req.ReuseSnapshotID != "" {
		plan.Seed = f.reuse
	}
	return plan, f.enter(ctx, "fork_begin")
}

func (f *forkFleet) ForkPlaceholder(ctx context.Context, _ orchestrator.ForkPlan) error {
	return f.enter(ctx, "placeholder")
}

func (f *forkFleet) ForkPlace(ctx context.Context, _ orchestrator.ForkPlan, seed orchestrator.SnapshotRecord) (string, error) {
	if seed.SnapshotID == "" || seed.State != orchestrator.SnapshotStateReady {
		return "", fmt.Errorf("place got seed %+v, want a ready one", seed)
	}
	return "h2", f.enter(ctx, "place")
}

func (f *forkFleet) ForkArtifact(ctx context.Context, _ orchestrator.ForkPlan, seed orchestrator.SnapshotRecord, hostID string) (string, error) {
	return seed.SnapshotID + "-on-" + hostID, f.enter(ctx, "artifact")
}

func (f *forkFleet) ForkCreate(ctx context.Context, _ orchestrator.ForkPlan, seed orchestrator.SnapshotRecord, hostID, localID string) error {
	if hostID != "h2" || localID != seed.SnapshotID+"-on-h2" {
		return fmt.Errorf("create got host %q copy %q, want the placement and the copy", hostID, localID)
	}
	return f.enter(ctx, "create")
}

func (f *forkFleet) ForkIngress(ctx context.Context, _ orchestrator.ForkPlan) ([]byte, error) {
	return []byte("sealed-tunnel"), f.enter(ctx, "ingress")
}

func (f *forkFleet) ForkCredentials(ctx context.Context, _ orchestrator.ForkPlan) (orchestrator.RotatedCredentials, error) {
	return orchestrator.RotatedCredentials{Sealed: []byte("sealed-creds")}, f.enter(ctx, "credentials")
}

func (f *forkFleet) ForkUpload(ctx context.Context, _ orchestrator.ForkPlan, started orchestrator.MigrateStarted) error {
	if string(started.Ingress) != "sealed-tunnel" || string(started.Credentials.Sealed) != "sealed-creds" {
		return fmt.Errorf("upload got %+v, want the ingress and credentials", started)
	}
	return f.enter(ctx, "upload")
}

func (f *forkFleet) ForkStartAgent(ctx context.Context, _ orchestrator.ForkPlan, _ orchestrator.MigrateStarted) error {
	return f.enter(ctx, "start_agent")
}

func (f *forkFleet) ForkRunning(ctx context.Context, _ orchestrator.ForkPlan, _ orchestrator.MigrateStarted) error {
	return f.enter(ctx, "running")
}

func (f *forkFleet) ForkTask(ctx context.Context, _ orchestrator.ForkPlan) error {
	return f.enter(ctx, "task")
}

func (f *forkFleet) ForkLineage(ctx context.Context, _ orchestrator.ForkPlan, _ orchestrator.SnapshotRecord) error {
	return f.enter(ctx, "lineage")
}

func (f *forkFleet) ForkUnpublishIngress(ctx context.Context, _ orchestrator.ForkPlan) error {
	return f.enter(ctx, "unpublish_ingress")
}

func (f *forkFleet) ForkAbandon(ctx context.Context, _ orchestrator.ForkPlan, cause string) error {
	f.mu.Lock()
	f.failed = cause
	f.mu.Unlock()
	return f.enter(ctx, "abandon")
}

func (f *forkFleet) ForkDeleteSeed(ctx context.Context, seed orchestrator.SnapshotRecord) error {
	f.mu.Lock()
	f.deleted = seed.SnapshotID
	f.mu.Unlock()
	return f.enter(ctx, "delete_seed")
}

// DestroyOnHost records the host the rollback's destroy was sent for.
func (f *forkFleet) DestroyOnHost(_ context.Context, vm, hostID string) error {
	f.destroyed <- vm + "@" + hostID
	return nil
}

func forkRequest(src string) orchestrator.ForkRequest {
	return orchestrator.ForkRequest{SourceVMID: src, TaskID: "fork-t", Comment: "c"}
}

// the source's snapshot steps (begin..ready) run between the placeholder and
// the placement.
const forkSnapshotSteps = "begin,checkpoint,record,list,ready"

// TestForkThroughRestate: a host create that fails twice is retried inside
// its step, the seed is taken through the source's snapshot steps, the steps
// before the create are not run again, and each step sees what the earlier
// ones returned.
func TestForkThroughRestate(t *testing.T) {
	fleet := newForkFleet()
	fleet.fail["create"] = []error{errors.New("agent 503"), errors.New("agent 503")}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Fork(ctx, uniqueVM("fork"), forkRequest(uniqueVM("src"))); err != nil {
		t.Fatalf("fork: %v", err)
	}
	want := "fork_begin,placeholder," + forkSnapshotSteps + ",place,artifact,create,create,create,ingress,credentials,upload,start_agent,running,task,lineage"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

// TestForkReuseSkipsSnapshot: a fork from an existing snapshot takes none.
func TestForkReuseSkipsSnapshot(t *testing.T) {
	fleet := newForkFleet()
	fleet.reuse = orchestrator.SnapshotRecord{SnapshotID: "snap-old", State: orchestrator.SnapshotStateReady}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := forkRequest(uniqueVM("src"))
	req.ReuseSnapshotID = "snap-old"
	if err := client.Fork(ctx, uniqueVM("fork-reuse"), req); err != nil {
		t.Fatalf("fork: %v", err)
	}
	want := "fork_begin,placeholder,place,artifact,create,ingress,credentials,upload,start_agent,running,task,lineage"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}

// TestForkPinsSeedThroughRestate: the seed stays pinned against gc while the
// fork waits between steps, and the pin goes once the fork has ended.
func TestForkPinsSeedThroughRestate(t *testing.T) {
	fleet := newForkFleet()
	seedID := fmt.Sprintf("snap-pin-%d", time.Now().UnixNano())
	fleet.reuse = orchestrator.SnapshotRecord{SnapshotID: seedID, State: orchestrator.SnapshotStateReady}
	fleet.block["create"] = make(chan struct{})
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := forkRequest(uniqueVM("src"))
	req.ReuseSnapshotID = seedID
	done := make(chan error, 1)
	go func() { done <- client.Fork(ctx, uniqueVM("fork-pin"), req) }()
	select {
	case <-fleet.entered:
	case <-ctx.Done():
		t.Fatal("fork never reached the create")
	}
	if p := pinState(t, ctx, client, []string{seedID}); !p[0] {
		t.Fatal("seed not pinned while the fork waits")
	}
	close(fleet.block["create"])
	if err := <-done; err != nil {
		t.Fatalf("fork: %v", err)
	}
	waitUnpinned(t, ctx, client, []string{seedID})
}

// TestForkRollsBackThroughRestate: an agent that refuses the fork's
// credentials for good unpublishes ingress, abandons the placeholder,
// destroys the new vm on its host through Environment.Destroy and deletes the
// seed the fork took.
func TestForkRollsBackThroughRestate(t *testing.T) {
	fleet := newForkFleet()
	fleet.fail["start_agent"] = []error{&orchestrator.HTTPStatusError{Code: 400, Body: "bad token"}}
	client := serveFleet(t, fleet, fastPolicy)
	vm := uniqueVM("fork-rollback")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := client.Fork(ctx, vm, forkRequest(uniqueVM("src")))
	if err == nil || err.Error() != "http 400: bad token" {
		t.Fatalf("fork: %v, want the agent's refusal as the fleet wrote it", err)
	}
	want := "fork_begin,placeholder," + forkSnapshotSteps + ",place,artifact,create,ingress,credentials,upload,start_agent,unpublish_ingress,abandon,delete_seed"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if !strings.Contains(fleet.failed, "bad token") {
		t.Fatalf("abandoned with %q, want the agent's refusal", fleet.failed)
	}
	if !strings.HasPrefix(fleet.deleted, "snap-") {
		t.Fatalf("deleted seed %q, want the one the fork took", fleet.deleted)
	}
	select {
	case got := <-fleet.destroyed:
		if got != vm+"@h2" {
			t.Fatalf("destroyed %s, want %s@h2", got, vm)
		}
	case <-ctx.Done():
		t.Fatal("the rollback never destroyed the vm")
	}
}

// a seed snapshot that fails for good leaves nothing on a host and no seed,
// so the rollback only drops the placeholder; the caller gets the snapshot's
// sentinel back.
func TestForkSeedFailsThroughRestate(t *testing.T) {
	fleet := newForkFleet()
	fleet.fail["checkpoint"] = []error{fmt.Errorf("%w: quota", orchestrator.ErrSnapshotQuotaExceeded)}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := client.Fork(ctx, uniqueVM("fork-seed"), forkRequest(uniqueVM("src")))
	if !errors.Is(err, orchestrator.ErrSnapshotQuotaExceeded) {
		t.Fatalf("fork: %v, want ErrSnapshotQuotaExceeded", err)
	}
	if got, want := fleet.steps(), "fork_begin,placeholder,begin,checkpoint,unpublish_ingress,abandon"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	select {
	case got := <-fleet.destroyed:
		t.Fatalf("destroyed %s for a fork that never booted", got)
	case <-time.After(500 * time.Millisecond):
	}
}

// begin's refusals come back as the fleet's sentinel, or its message, and
// nothing runs after them.
func TestForkRefusalsThroughRestate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		sentinel error
	}{
		{"not seedable", fmt.Errorf("%w: snapshot s is a live snapshot", orchestrator.ErrSnapshotNotSeedable), orchestrator.ErrSnapshotNotSeedable},
		{"gpu", fmt.Errorf("%w: vm has a gpu", orchestrator.ErrGPUUnsupported), orchestrator.ErrGPUUnsupported},
		{"not running", errors.New("vm x in state stopped: fork requires running"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fleet := newForkFleet()
			fleet.fail["fork_begin"] = []error{tc.err}
			client := serveFleet(t, fleet, fastPolicy)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := client.Fork(ctx, uniqueVM("fork-refused"), forkRequest(uniqueVM("src")))
			if err == nil || err.Error() != tc.err.Error() {
				t.Fatalf("fork: %v, want %q", err, tc.err)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Fatalf("fork: %v, want %v", err, tc.sentinel)
			}
			if got := fleet.steps(); got != "fork_begin" {
				t.Fatalf("steps %s, want fork_begin only", got)
			}
		})
	}
}

// TestForkSurvivesEndpointRestart: the endpoint dies during the upload, as an
// orchestrator kill -9 would take it. the restarted endpoint replays every
// step before the upload from the journal and runs only the rest.
func TestForkSurvivesEndpointRestart(t *testing.T) {
	first := newForkFleet()
	first.block["upload"] = make(chan struct{})
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Fork(ctx, uniqueVM("fork-restart"), forkRequest(uniqueVM("src"))) }()
	select {
	case <-first.entered:
	case <-ctx.Done():
		t.Fatal("fork never reached the upload")
	}
	stop()

	second := newForkFleet()
	_, _, _ = serveFleetAt(t, second, addr)
	if err := <-done; err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got, want := second.steps(), "upload,start_agent,running,task,lineage"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
}
