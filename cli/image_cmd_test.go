package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// imageServer converts one image that is ready on the second poll, or fails,
// and records the create that follows.
type imageServer struct {
	fail bool

	mu      sync.Mutex
	polls   int
	convert map[string]any
	create  map[string]any
}

var testImageDigest = strings.Repeat("a", 64)

func (s *imageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	image := func(state, errMsg string) string {
		return fmt.Sprintf(`{"digest":%q,"ref":"ubuntu:24.04","manifest_digest":"sha256:x","host_id":"fc-1","state":%q,"error":%q}`, testImageDigest, state, errMsg)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/images":
		_ = json.NewDecoder(r.Body).Decode(&s.convert)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, image("converting", ""))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/images/"+testImageDigest:
		s.polls++
		switch {
		case s.polls < 2:
			fmt.Fprint(w, image("converting", ""))
		case s.fail:
			fmt.Fprint(w, image("failed", "fc-bake-distro-rootfs: unsupported distro"))
		default:
			fmt.Fprint(w, image("ready", ""))
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
		_ = json.NewDecoder(r.Body).Decode(&s.create)
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func runUpFrom(t *testing.T, srv *imageServer) error {
	t.Helper()
	prev := imagePollInterval
	imagePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { imagePollInterval = prev })
	ts := httptest.NewServer(srv)
	defer ts.Close()
	path := filepath.Join(t.TempDir(), "Fusefile")
	src := "version: 1\nfrom: ubuntu:24.04\nplacement:\n  host: fc-1\nrun: ./start.sh\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, ts.URL)
	_, _, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "up", "-f", path, "--task-id", "t", "--no-wait"})
		return root.Execute()
	})
	return err
}

// `from:` converts the image, waits for it, and creates with its digest,
// never with an image name.
func TestUpFromConvertsThenCreatesWithTheDigest(t *testing.T) {
	srv := &imageServer{}
	if err := runUpFrom(t, srv); err != nil {
		t.Fatalf("up: %v", err)
	}
	if srv.convert["ref"] != "ubuntu:24.04" || srv.convert["host_id"] != "fc-1" {
		t.Fatalf("convert body = %v, want the ref and the placement host", srv.convert)
	}
	if srv.polls < 2 {
		t.Fatalf("polled %d times, want until ready", srv.polls)
	}
	spec, _ := srv.create["spec"].(map[string]any)
	if spec["image_digest"] != testImageDigest || spec["image"] != nil {
		t.Fatalf("create spec = %v, want image_digest and no image", spec)
	}
}

func TestUpFromFailedConversionDoesNotCreate(t *testing.T) {
	srv := &imageServer{fail: true}
	err := runUpFrom(t, srv)
	if err == nil || !strings.Contains(err.Error(), "unsupported distro") {
		t.Fatalf("err = %v, want the host's reason", err)
	}
	if srv.create != nil {
		t.Fatal("created an environment from an image that failed to convert")
	}
}
