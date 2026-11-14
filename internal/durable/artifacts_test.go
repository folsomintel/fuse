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

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// moveFleet scripts the move steps. pull returns pullErrs in turn, then
// succeeds; block, when set, holds a pull until it is closed.
type moveFleet struct {
	fakeFleet
	committed string

	mu       sync.Mutex
	calls    []string
	pullErrs []error
	block    chan struct{}
	pulling  chan struct{}
}

func (f *moveFleet) record(step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, step)
}

func (f *moveFleet) steps() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *moveFleet) ArtifactMoveCommitted(context.Context, orchestrator.ArtifactMoveJob) (string, error) {
	f.record("check")
	return f.committed, nil
}

func (f *moveFleet) ArtifactPull(ctx context.Context, job orchestrator.ArtifactMoveJob) (orchestrator.ArtifactMoved, error) {
	f.record("pull")
	if f.pulling != nil {
		select {
		case f.pulling <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return orchestrator.ArtifactMoved{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pullErrs) > 0 {
		err := f.pullErrs[0]
		f.pullErrs = f.pullErrs[1:]
		return orchestrator.ArtifactMoved{}, err
	}
	return orchestrator.ArtifactMoved{SnapshotID: job.LocalID, SizeBytes: 42}, nil
}

func (f *moveFleet) ArtifactRecord(_ context.Context, job orchestrator.ArtifactMoveJob, moved orchestrator.ArtifactMoved) (string, error) {
	f.record("record")
	return moved.SnapshotID, nil
}

func (f *moveFleet) ArtifactDiscard(context.Context, orchestrator.ArtifactMoveJob) error {
	f.record("discard")
	return nil
}

func testJob(name string) orchestrator.ArtifactMoveJob {
	id := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	return orchestrator.ArtifactMoveJob{
		SnapshotID: id + "-src", SourceSnapshotID: id + "-src", SourceHostID: "h1",
		HostID: "h2", LocalID: id + "-dst", Digest: "d1",
	}
}

// pinState reads Pinned for each id.
func pinState(t *testing.T, ctx context.Context, client *Client, ids []string) []bool {
	t.Helper()
	out := make([]bool, len(ids))
	for i, id := range ids {
		p, err := client.ArtifactPinned(ctx, id)
		if err != nil {
			t.Fatalf("pinned %s: %v", id, err)
		}
		out[i] = p
	}
	return out
}

// waitUnpinned polls until no id is pinned: releases are one-way sends.
func waitUnpinned(t *testing.T, ctx context.Context, client *Client, ids []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		held := false
		for _, p := range pinState(t, ctx, client, ids) {
			held = held || p
		}
		if !held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pins on %v never released", ids)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMoveThroughRestate: a pull that fails twice is retried inside the pull
// step, the copy is recorded once, and the pins are held while it runs and
// released after.
func TestMoveThroughRestate(t *testing.T) {
	fleet := &moveFleet{
		pullErrs: []error{errors.New("peer reset"), errors.New("peer reset")},
		block:    make(chan struct{}),
		pulling:  make(chan struct{}, 1),
	}
	client := serveFleet(t, fleet, fastPolicy)
	job := testJob("art-move")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var got string
	go func() {
		var err error
		got, err = client.MoveArtifact(ctx, job)
		done <- err
	}()

	<-fleet.pulling
	for i, p := range pinState(t, ctx, client, job.PinIDs()) {
		if !p {
			t.Fatalf("%s not pinned during the pull", job.PinIDs()[i])
		}
	}
	close(fleet.block)

	if err := <-done; err != nil {
		t.Fatalf("move: %v", err)
	}
	if got != job.LocalID {
		t.Fatalf("move returned %q, want %q", got, job.LocalID)
	}
	if got, want := fleet.steps(), "check,pull,pull,pull,record"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	waitUnpinned(t, ctx, client, job.PinIDs())
}

// TestMoveDiscardsAfterPermanentFailure: an agent that refuses the pull is not
// retried, the partial copy is discarded and the pins are released.
func TestMoveDiscardsAfterPermanentFailure(t *testing.T) {
	fleet := &moveFleet{pullErrs: []error{&orchestrator.HTTPStatusError{Code: 409, Body: "digest mismatch"}}}
	client := serveFleet(t, fleet, fastPolicy)
	job := testJob("art-refused")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := client.MoveArtifact(ctx, job)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("move error %v, want the agent's refusal", err)
	}
	if got, want := fleet.steps(), "check,pull,discard"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	waitUnpinned(t, ctx, client, job.PinIDs())
}

// TestMoveDiscardsAfterRetriesRunOut: a peer that never answers is retried
// under the policy, then the partial copy is discarded.
func TestMoveDiscardsAfterRetriesRunOut(t *testing.T) {
	errs := make([]error, fastPolicy.MaxAttempts)
	for i := range errs {
		errs[i] = errors.New("peer unreachable")
	}
	fleet := &moveFleet{pullErrs: errs}
	client := serveFleet(t, fleet, fastPolicy)
	job := testJob("art-unreachable")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.MoveArtifact(ctx, job); err == nil {
		t.Fatal("move succeeded against a peer that never answers")
	}
	want := "check," + strings.Repeat("pull,", fastPolicy.MaxAttempts) + "discard"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	waitUnpinned(t, ctx, client, job.PinIDs())
}

// TestMoveFindsCommittedCopy: a copy already recorded on the destination is
// returned without a pull.
func TestMoveFindsCommittedCopy(t *testing.T) {
	job := testJob("art-done")
	fleet := &moveFleet{committed: job.LocalID}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := client.MoveArtifact(ctx, job)
	if err != nil || got != job.LocalID {
		t.Fatalf("move = %q, %v; want %q", got, err, job.LocalID)
	}
	if got := fleet.steps(); got != "check" {
		t.Fatalf("steps %s, want check", got)
	}
}

// TestMoveSurvivesEndpointRestart stands in for kill -9 of the orchestrator
// mid-pull: the handler endpoint drops every connection while the pull runs,
// and a new endpoint on the same address takes the move over. the pins are
// restate state, so the restarted process still sees them held, and the move
// completes once.
func TestMoveSurvivesEndpointRestart(t *testing.T) {
	first := &moveFleet{block: make(chan struct{}), pulling: make(chan struct{}, 1)}
	defer close(first.block)
	client, addr, stop := serveFleetAt(t, first, "")
	job := testJob("art-restart")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var got string
	go func() {
		var err error
		got, err = client.MoveArtifact(ctx, job)
		done <- err
	}()

	<-first.pulling
	stop()

	second := &moveFleet{block: make(chan struct{}), pulling: make(chan struct{}, 1)}
	_, _, _ = serveFleetAt(t, second, addr)
	<-second.pulling
	for i, p := range pinState(t, ctx, client, job.PinIDs()) {
		if !p {
			t.Fatalf("%s not pinned after the restart", job.PinIDs()[i])
		}
	}
	close(second.block)

	if err := <-done; err != nil {
		t.Fatalf("move: %v", err)
	}
	if got != job.LocalID {
		t.Fatalf("move returned %q, want %q", got, job.LocalID)
	}
	// the restarted endpoint replays check and the pins from the journal and
	// runs only the unfinished pull, then the record.
	if got, want := second.steps(), "pull,record"; got != want {
		t.Fatalf("steps after the restart %s, want %s", got, want)
	}
	waitUnpinned(t, ctx, client, job.PinIDs())
}

// serveFleetAt serves fleet on addr ("" picks one) with an endpoint whose
// connections stop() drops at once, the way a killed process drops them.
func serveFleetAt(t *testing.T, fleet Fleet, addr string) (*Client, string, func()) {
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
		Retry: map[string]RetryPolicy{ArtifactsObject: fastPolicy}}
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

// TestPinLapsesThroughRestate: a pin nothing releases lapses on its timer, and
// a released one is gone at once while another holder's stays.
func TestPinLapsesThroughRestate(t *testing.T) {
	client := serveFleet(t, fakeFleet{}, fastPolicy)
	id := fmt.Sprintf("art-pin-%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.PinArtifacts(ctx, "a", time.Hour, id); err != nil {
		t.Fatal(err)
	}
	if err := client.PinArtifacts(ctx, "b", time.Second, id); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseArtifacts(ctx, "a", id); err != nil {
		t.Fatal(err)
	}
	if p := pinState(t, ctx, client, []string{id}); !p[0] {
		t.Fatal("released one holder and lost the other's pin")
	}
	waitUnpinned(t, ctx, client, []string{id})
}
