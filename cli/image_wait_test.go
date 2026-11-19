package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// twoHostImages holds one image ready on host local and converting on host
// local2 until local2's third read. the fleet-wide GET answers with the best
// record, local's ready one, the way the orchestrator does. legacy answers
// the host-scoped route with route_not_found, as an orchestrator older than
// it does.
type twoHostImages struct {
	legacy bool

	mu         sync.Mutex
	hostReads  int
	fleetReads int
}

func (s *twoHostImages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	image := func(host, state string) string {
		return fmt.Sprintf(`{"digest":%q,"ref":"ubuntu:24.04","manifest_digest":"sha256:x","host_id":%q,"state":%q}`, testImageDigest, host, state)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/images":
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, image("local2", "converting"))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/hosts/local2/images/"+testImageDigest && !s.legacy:
		s.hostReads++
		if s.hostReads < 3 {
			fmt.Fprint(w, image("local2", "converting"))
			return
		}
		fmt.Fprint(w, image("local2", "ready"))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/images/"+testImageDigest:
		s.fleetReads++
		fmt.Fprint(w, image("local", "ready"))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"code":"route_not_found","message":"no such route: %s %s"}}`, r.Method, r.URL.Path)
	}
}

func runImageConvert(t *testing.T, srv http.Handler, args ...string) (string, error) {
	t.Helper()
	prev := imagePollInterval
	imagePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { imagePollInterval = prev })
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := writeConfig(t, ts.URL)
	stdout, stderr, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs(append([]string{"--config", cfg, "image", "convert", "ubuntu:24.04"}, args...))
		return root.Execute()
	})
	return stdout + stderr, err
}

// a convert pinned to local2 waits for local2's own record, not for the
// fleet's best one, which is local's ready image.
func TestImageConvertPinnedWaitsOnItsHost(t *testing.T) {
	for _, pin := range [][]string{{"--host", "local2"}, {"--region", "eu-west"}, {"--label", "zone=a"}} {
		srv := &twoHostImages{}
		out, err := runImageConvert(t, srv, pin...)
		if err != nil {
			t.Fatalf("%v: convert: %v", pin, err)
		}
		if !strings.Contains(out, "ready on host local2") {
			t.Fatalf("%v: output %q, want ready on host local2", pin, out)
		}
		if srv.hostReads != 3 || srv.fleetReads != 0 {
			t.Fatalf("%v: %d host reads and %d fleet reads, want 3 and 0", pin, srv.hostReads, srv.fleetReads)
		}
	}
}

// an unpinned convert keeps the fleet-wide read: any host's ready image is
// one an unpinned create can boot.
func TestImageConvertUnpinnedReadsTheFleet(t *testing.T) {
	srv := &twoHostImages{}
	if _, err := runImageConvert(t, srv); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if srv.hostReads != 0 || srv.fleetReads != 1 {
		t.Fatalf("%d host reads and %d fleet reads, want 0 and 1", srv.hostReads, srv.fleetReads)
	}
}

// an orchestrator without the host-scoped route answers route_not_found, and
// the wait falls back to the fleet-wide read instead of failing.
func TestImageConvertPinnedFallsBackOnAnOlderOrchestrator(t *testing.T) {
	srv := &twoHostImages{legacy: true}
	if _, err := runImageConvert(t, srv, "--host", "local2"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if srv.fleetReads != 1 {
		t.Fatalf("%d fleet reads, want 1", srv.fleetReads)
	}
}
