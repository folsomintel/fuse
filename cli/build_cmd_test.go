package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

// buildServer fakes the orchestrator for `fuse build`: one amd64 host, a
// build that answers with final, and the create request it was sent.
type buildServer struct {
	final string

	mu   sync.Mutex
	sent *fuse.CreateBuildRequest
	gets int
}

func (s *buildServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/hosts":
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": []fuse.Host{host("h1", "amd64")}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/builds":
		var req fuse.CreateBuildRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.sent = &req
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"bld_1","name":"app","state":"running","step":"queued","layers":[]}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/builds/bld_1":
		s.mu.Lock()
		s.gets++
		s.mu.Unlock()
		_, _ = w.Write([]byte(s.final))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no route"}}`))
	}
}

const succeededBuild = `{"id":"bld_1","name":"app","state":"succeeded","step":"done","builder_id":"fuse-build-bld_1","cache_hits":1,"seed_snapshot_id":"snap-l0","snapshot_id":"snap-final","layers":[{"index":0,"status":"hit"},{"index":1,"status":"miss"}]}`

func runBuild(t *testing.T, srv *buildServer, args ...string) (string, string, error) {
	t.Helper()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := writeConfig(t, ts.URL)
	return captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs(append([]string{"--config", cfg, "build"}, args...))
		return root.Execute()
	})
}

// the orchestrator runs the build, but the layer keys it looks up are still
// the ones this client derives, and the scripts it runs are the build script
// cut at the step boundaries. so the cache hits a server-side build gets are
// the ones the client-side `fuse build` got for the same Fusefile.
func TestBuildSubmitsTheClientSideLayerPlan(t *testing.T) {
	path := writeCachedFusefile(t, t.TempDir())
	srv := &buildServer{final: succeededBuild}
	stdout, stderr, err := runBuild(t, srv, path)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, stderr)
	}
	if strings.TrimSpace(stdout) != "snap-final" {
		t.Errorf("stdout %q, want the artifact id", stdout)
	}
	if !strings.Contains(stderr, "reusing 1 cached layer(s) from snap-l0") {
		t.Errorf("stderr %q, want the cache hit reported", stderr)
	}

	req := srv.sent
	if req == nil {
		t.Fatal("no build was submitted")
	}
	if req.Arch != "amd64" || req.Name == "" {
		t.Errorf("request arch %q name %q", req.Arch, req.Name)
	}

	f := parseFusefile(t, path)
	lp, err := buildLayerPlan(path, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Setup.Steps) != len(lp.Steps) {
		t.Fatalf("sent %d steps, plan has %d", len(req.Setup.Steps), len(lp.Steps))
	}
	// pinned: these are the keys `fuse build --plan` on main derived for this
	// Fusefile before the build moved server-side. a change here is a cache
	// invalidation for every existing layer, not a refactor.
	golden := []string{
		"ad71584802f148186c71208691c51f16b65d96af3d11fb223ac5d81387880467",
		"3ef2ca8a5f6cb5ee5de50386a8fa7389c5bff38ef51b6fc82ac30586fd0e49e1",
	}
	for i, s := range req.Setup.Steps {
		if s.Key != lp.Steps[i].Key {
			t.Errorf("step %d key %q, plan key %q", i, s.Key, lp.Steps[i].Key)
		}
		if s.Key != golden[i] {
			t.Errorf("step %d key %q, want the pinned %q", i, s.Key, golden[i])
		}
	}

	c, err := fusefile.Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	setup := fusefile.SetupPlan{Prelude: req.Setup.Prelude, Files: req.Setup.Files}
	for _, s := range req.Setup.Steps {
		setup.Steps = append(setup.Steps, s.Script)
	}
	if got := setup.Range(0, len(setup.Steps), true); got != c.BuildScript {
		t.Errorf("the sent setup does not render the build script:\n got %q\nwant %q", got, c.BuildScript)
	}
}

func TestBuildNoCacheSendsNoKeys(t *testing.T) {
	srv := &buildServer{final: succeededBuild}
	if _, stderr, err := runBuild(t, srv, writeCachedFusefile(t, t.TempDir()), "--no-cache"); err != nil {
		t.Fatalf("build: %v\n%s", err, stderr)
	}
	for i, s := range srv.sent.Setup.Steps {
		if s.Key != "" {
			t.Errorf("step %d sent key %q with --no-cache", i, s.Key)
		}
	}
	if srv.sent.Arch != "" {
		t.Errorf("arch %q sent with the cache off", srv.sent.Arch)
	}
}

func TestBuildDetachPrintsTheID(t *testing.T) {
	srv := &buildServer{final: succeededBuild}
	stdout, _, err := runBuild(t, srv, writeCachedFusefile(t, t.TempDir()), "--detach")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "bld_1" {
		t.Errorf("stdout %q, want the build id", stdout)
	}
	if srv.gets != 0 {
		t.Errorf("--detach followed the build (%d gets)", srv.gets)
	}
}

func TestBuildFailureShowsTheStepOutput(t *testing.T) {
	srv := &buildServer{final: `{"id":"bld_1","state":"failed","step":"tail","error":"setup tail exited 1","output":"npm ERR! missing script\n","layers":[]}`}
	_, stderr, err := runBuild(t, srv, writeCachedFusefile(t, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "setup tail exited 1") {
		t.Fatalf("err = %v, want the build's error", err)
	}
	if !strings.Contains(stderr, "npm ERR! missing script") {
		t.Errorf("stderr %q, want the failing step's output", stderr)
	}
}

func parseFusefile(t *testing.T, path string) *fusefile.Fusefile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fusefile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := fusefile.ResolveFiles(f, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	return f
}
