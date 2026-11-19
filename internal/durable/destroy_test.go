package durable

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// scriptedFleet records the destroy steps it is driven through. the host
// destroy fails hostFails times per vm before it succeeds; -1 never succeeds.
type scriptedFleet struct {
	fakeFleet
	hostFails int
	// hostBlock, when set, holds every host destroy until it is closed.
	hostBlock chan struct{}

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

func (f *scriptedFleet) DestroyOnHost(ctx context.Context, vm, _ string) error {
	f.record(vm, "host")
	if f.hostBlock != nil {
		select {
		case <-f.hostBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	admin, ingressURL := restateTestURLs(t)
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
		Retry: map[string]RetryPolicy{EnvironmentObject: policy, HostObject: DefaultRetryPolicy, ArtifactsObject: policy}}
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

// restateTestURLs returns the admin and ingress urls of the restate-server
// named by the test env, and skips without one.
func restateTestURLs(t *testing.T) (admin, ingressURL string) {
	t.Helper()
	admin = os.Getenv("FUSE_RESTATE_TEST_ADMIN_URL")
	ingressURL = os.Getenv("FUSE_RESTATE_TEST_INGRESS_URL")
	if admin == "" || ingressURL == "" {
		t.Skip("set FUSE_RESTATE_TEST_ADMIN_URL and FUSE_RESTATE_TEST_INGRESS_URL to run against restate-server")
	}
	return admin, ingressURL
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
	if !orchestrator.IsSettled(err) {
		t.Fatalf("dead letter %v is not marked settled, so a keyed retry would replay it", err)
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

// TestDestroyPastTheClientTimeoutKeepsItsKey: Destroy runs on the ingress
// client with a 30s timeout, so a long destroy fails at the caller while
// restate goes on running it. that failure is not settled, so the api keeps
// the key, and a retry under it attaches to the running destroy instead of
// queueing a second one.
func TestDestroyPastTheClientTimeoutKeepsItsKey(t *testing.T) {
	vm := fmt.Sprintf("fuse-slow-%d", time.Now().UnixNano())
	fleet := newScriptedFleet(0, vm)
	fleet.hostBlock = make(chan struct{})
	client := serveFleet(t, fleet, fastPolicy)
	// a short client timeout stands in for the 30s one.
	short := *client
	short.ingress = ingress.NewClient(client.url, ingress.WithHttpClient(
		&http.Client{Timeout: 300 * time.Millisecond, Transport: unavailable{http.DefaultTransport}}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx = orchestrator.WithIdempotencyKey(ctx, "master:destroy:k-"+vm)
	req := orchestrator.DestroyRequest{Reason: "manual_destroy"}
	err := short.Destroy(ctx, vm, req)
	if err == nil {
		t.Fatal("destroy answered before the host was released")
	}
	if orchestrator.IsSettled(err) || errors.Is(err, orchestrator.ErrRestateUnavailable) {
		t.Fatalf("client timeout %v is marked settled or unavailable; want neither", err)
	}

	close(fleet.hostBlock)
	if err := client.Destroy(ctx, vm, req); err != nil {
		t.Fatalf("retry under the same key: %v", err)
	}
	if got, want := fleet.steps(vm), "begin,egress,host,ingress,finish"; got != want {
		t.Fatalf("steps %s, want %s: the retry should attach, not destroy again", got, want)
	}
}

// TestIngressOwnErrorIsNotSettled: an error restate's ingress makes itself,
// with no invocation behind it, is not marked settled.
func TestIngressOwnErrorIsNotSettled(t *testing.T) {
	client := serveFleet(t, newScriptedFleet(0), fastPolicy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, a := watchAnswer(ctx)
	_, err := ingress.Object[string, string](client.ingress, "NoSuchObject", "k", "Nope").Request(ctx, "x")
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		t.Fatalf("err %v, want an ingress error", err)
	}
	if orchestrator.IsSettled(settled(a, err, err)) {
		t.Fatalf("ingress error %v marked settled", err)
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

// TestDestroyCleanupOnHostlessFleet: on a fleet with no registered hosts a
// booted create's cleanup carries no host, and is still destroyed on the
// default provider; a plain destroy of an untracked vm with no host is a
// destroy that already finished.
func TestDestroyCleanupOnHostlessFleet(t *testing.T) {
	fleet := newScriptedFleet(0) // not tracked
	client := serveFleet(t, fleet, fastPolicy)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	vm := fmt.Sprintf("fuse-hostless-%d", time.Now().UnixNano())
	if err := client.Destroy(ctx, vm, orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonCreateCleanup}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got, want := fleet.steps(vm), "begin,egress,host,ingress,finish"; got != want {
		t.Fatalf("cleanup steps %s, want %s", got, want)
	}

	gone := fmt.Sprintf("fuse-gone-%d", time.Now().UnixNano())
	if err := client.Destroy(ctx, gone, orchestrator.DestroyRequest{Reason: "api"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := fleet.steps(gone); strings.Contains(got, "host") {
		t.Fatalf("destroy of a finished vm reached the host: %s", got)
	}
}
