package durable

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// scriptedFleet records the destroy steps it is driven through. the host
// destroy fails hostFails times per vm before it succeeds; -1 never succeeds.
type scriptedFleet struct {
	fakeFleet
	hostFails int

	mu       sync.Mutex
	tracked  map[string]bool
	calls    map[string][]string
	attempts map[string]int
	failed   map[string]string
}

func newScriptedFleet(hostFails int, vms ...string) *scriptedFleet {
	f := &scriptedFleet{
		hostFails: hostFails,
		tracked:   map[string]bool{},
		calls:     map[string][]string{},
		attempts:  map[string]int{},
		failed:    map[string]string{},
	}
	for _, v := range vms {
		f.tracked[v] = true
	}
	return f
}

func (f *scriptedFleet) record(vm, step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[vm] = append(f.calls[vm], step)
}

func (f *scriptedFleet) steps(vm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls[vm], ",")
}

func (f *scriptedFleet) DestroyBegin(_ context.Context, vm, _ string) (orchestrator.DestroyTarget, error) {
	f.record(vm, "begin")
	f.mu.Lock()
	defer f.mu.Unlock()
	return orchestrator.DestroyTarget{Tracked: f.tracked[vm], HostID: "h1"}, nil
}

func (f *scriptedFleet) DestroyReleaseEgress(_ context.Context, vm string) error {
	f.record(vm, "egress")
	return nil
}

func (f *scriptedFleet) DestroyOnHost(_ context.Context, vm, _ string) error {
	f.record(vm, "host")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[vm]++
	if f.hostFails < 0 || f.attempts[vm] <= f.hostFails {
		return fmt.Errorf("host down (attempt %d)", f.attempts[vm])
	}
	return nil
}

func (f *scriptedFleet) DestroyUnpublishIngress(_ context.Context, vm string) error {
	f.record(vm, "ingress")
	return nil
}

func (f *scriptedFleet) DestroyFinish(_ context.Context, vm, _ string) error {
	f.record(vm, "finish")
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tracked, vm)
	return nil
}

func (f *scriptedFleet) DestroyFailed(_ context.Context, vm, _, lastErr string) error {
	f.record(vm, "dead_letter")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[vm] = lastErr
	return nil
}

// serveFleet serves fleet behind the restate-server named by the test env,
// registers it, and returns a client. skips without restate-server.
func serveFleet(t *testing.T, fleet Fleet, policy RetryPolicy) *Client {
	t.Helper()
	admin := os.Getenv("FUSE_RESTATE_TEST_ADMIN_URL")
	ingressURL := os.Getenv("FUSE_RESTATE_TEST_INGRESS_URL")
	if admin == "" || ingressURL == "" {
		t.Skip("set FUSE_RESTATE_TEST_ADMIN_URL and FUSE_RESTATE_TEST_INGRESS_URL to run against restate-server")
	}
	host := os.Getenv("FUSE_RESTATE_TEST_HANDLER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := Config{Mode: ModeSelf, Listen: addr, IngressURL: ingressURL, AdminURL: admin,
		Retry: map[string]RetryPolicy{EnvironmentObject: policy, HostObject: DefaultRetryPolicy}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = NewEndpoint(cfg, fleet, testLogger).Start(ctx, addr) }()
	waitListening(t, addr)

	client := NewClient(cfg)
	if err := client.Register(ctx, "http://"+addr); err != nil {
		t.Fatalf("register: %v", err)
	}
	return client
}

var fastPolicy = RetryPolicy{InitialInterval: 20 * time.Millisecond, MaxInterval: 50 * time.Millisecond, MaxAttempts: 4}

// TestDestroyThroughRestate: a host that fails twice is retried inside the
// host step until it succeeds, and the earlier steps are not run again.
func TestDestroyThroughRestate(t *testing.T) {
	vm := fmt.Sprintf("fuse-retry-%d", time.Now().UnixNano())
	fleet := newScriptedFleet(2, vm)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Destroy(ctx, vm, orchestrator.DestroyRequest{Reason: "manual_destroy"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got, want := fleet.steps(vm), "begin,egress,host,host,host,ingress,finish"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}

	// a second destroy of the same vm finds it gone and does nothing more.
	if err := client.Destroy(ctx, vm, orchestrator.DestroyRequest{Reason: "manual_destroy"}); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
	if got, want := fleet.steps(vm), "begin,egress,host,host,host,ingress,finish,begin"; got != want {
		t.Fatalf("steps after the repeat %s, want %s", got, want)
	}
}

// TestDestroyDeadLettersThroughRestate: a host that never recovers ends in
// a dead letter with the last error, and the caller gets that error.
func TestDestroyDeadLettersThroughRestate(t *testing.T) {
	vm := fmt.Sprintf("fuse-dead-%d", time.Now().UnixNano())
	fleet := newScriptedFleet(-1, vm)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := client.Destroy(ctx, vm, orchestrator.DestroyRequest{Reason: "manual_destroy"})
	if err == nil {
		t.Fatal("destroy succeeded against a host that never recovers")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("destroy never finished: %v", err)
	}
	want := "begin,egress," + strings.Repeat("host,", fastPolicy.MaxAttempts) + "dead_letter"
	if got := fleet.steps(vm); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	fleet.mu.Lock()
	lastErr := fleet.failed[vm]
	fleet.mu.Unlock()
	if !strings.Contains(lastErr, "host down") {
		t.Fatalf("dead letter reason %q, want the host error", lastErr)
	}
}

// TestDestroyCleanupOfUntrackedVM: a create cleanup names the host, so a vm
// the fleet already dropped is still destroyed there.
func TestDestroyCleanupOfUntrackedVM(t *testing.T) {
	vm := fmt.Sprintf("fuse-cleanup-%d", time.Now().UnixNano())
	fleet := newScriptedFleet(0) // not tracked
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Destroy(ctx, vm, orchestrator.DestroyRequest{Reason: "create_cleanup", HostID: "h1"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got, want := fleet.steps(vm), "begin,egress,host,ingress,finish"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
}
