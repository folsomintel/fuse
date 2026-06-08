package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/andrewn6/fuse/api"
	"github.com/andrewn6/fuse/firecracker"
	"github.com/andrewn6/fuse/internal/core"
)

// remoteTarget returns the Firecracker host URL+token to run the e2e against, or
// ("","") to use the hermetic in-memory stub. Precedence:
//   - FUSE_E2E_FIRECRACKER_URL / _TOKEN (explicit), else
//   - FUSE_E2E_REMOTE=1 → read FIRECRACKER_BASE_URL / FIRECRACKER_TOKEN from ../.env
//
// Remote is opt-in so `go test ./...` never deploys real VMs by accident.
func remoteTarget() (string, string) {
	if u := os.Getenv("FUSE_E2E_FIRECRACKER_URL"); u != "" {
		return u, os.Getenv("FUSE_E2E_FIRECRACKER_TOKEN")
	}
	if os.Getenv("FUSE_E2E_REMOTE") == "1" {
		env := loadDotEnv("../.env")
		return env["FIRECRACKER_BASE_URL"], env["FIRECRACKER_TOKEN"]
	}
	return "", ""
}

// loadDotEnv parses a simple KEY=VALUE .env file (quotes stripped, # comments
// and blank lines ignored). Returns an empty map if the file is absent.
func loadDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// setupServer assembles the full Fuse stack the way server/main.go does and
// returns a running httptest server plus a cleanup func.
//
// Provider selection mirrors the real binary: a real Firecracker host when
// FUSE_E2E_FIRECRACKER_URL is set, otherwise the in-memory stub (hermetic).
func setupServer(t *testing.T) *httptest.Server {
	t.Helper()

	url, token := remoteTarget()
	// AGENT_DOWNLOAD_URL lets the host fetch the agent binary into the guest at
	// boot (no baked-in fused needed). Honored only against a real host.
	downloadURL := os.Getenv("AGENT_DOWNLOAD_URL")
	var provider orchestrator.Provider
	if url != "" {
		t.Logf("e2e: targeting real Firecracker host %s (download_url=%q)", url, downloadURL)
		provider = firecracker.New(firecracker.Config{BaseURL: url, Token: token, DownloadURL: downloadURL})
	} else {
		provider = firecracker.New(firecracker.Config{UseStub: true})
	}

	store := orchestrator.NewMemoryStateStore()
	metrics := orchestrator.NewPrometheusMetrics(prometheus.NewRegistry())

	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatalf("gen encryption key: %v", err)
	}

	hostFactory := func(u, tok string) orchestrator.Provider {
		return firecracker.New(firecracker.Config{BaseURL: u, Token: tok})
	}

	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:            provider,
		StateStore:          store,
		Prefix:              "fuse-",
		TokenEncryptionKey:  encKey,
		HostProviderFactory: hostFactory,
		Metrics:             metrics,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	fm.Start(ctx)

	h := &api.Handler{Fleet: fm, NewProvider: hostFactory}
	router, err := h.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}

	hc := &api.Healthcheck{Fleet: fm, Store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", hc.Liveness)
	mux.HandleFunc("/ready", hc.Readiness)
	mux.Handle("/", router)

	srv := httptest.NewServer(mux)
	// Registered first, so (t.Cleanup is LIFO) it runs AFTER any per-test
	// cleanupEnv DELETEs — the orchestrator stays up while envs are torn down.
	t.Cleanup(func() {
		srv.Close()
		cancel()
		fm.Stop()
	})
	return srv
}

// runID returns a short random hex suffix so task IDs (and therefore the real
// VM names `fuse-<task>`) are unique per run — a failed run against a real host
// can't 409 the next one on a leaked name.
func runID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanupEnv registers a best-effort DELETE so a created VM is always torn down,
// even if a later assertion fails mid-test (prevents leaks on the real host).
func cleanupEnv(t *testing.T, c *http.Client, base, id string) {
	t.Cleanup(func() { _, _ = req(t, c, "DELETE", base+"/v1/environments/"+id, nil) })
}

// req performs an HTTP request against the test server and returns the status
// code and raw body. JSON-encodes body when non-nil.
func req(t *testing.T, c *http.Client, method, url string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// TestE2E_FullDeployLifecycle deploys an environment through the assembled REST
// API and walks the entire lifecycle: probes → create → get → list → snapshot →
// list snapshots → restore → drain → destroy → confirm gone.
func TestE2E_FullDeployLifecycle(t *testing.T) {
	srv := setupServer(t)
	c := srv.Client()
	base := srv.URL

	// --- probes ---
	if code, b := req(t, c, "GET", base+"/health", nil); code != http.StatusOK {
		t.Fatalf("/health = %d: %s", code, b)
	}
	if code, b := req(t, c, "GET", base+"/ready", nil); code != http.StatusOK {
		t.Fatalf("/ready = %d: %s", code, b)
	}

	// --- 1. create (deploy) ---
	task := "e2e-" + runID()
	code, body := req(t, c, "POST", base+"/v1/environments", map[string]any{
		"task_id": task,
		"spec":    map[string]any{"cpus": 2, "ram_mb": 512, "storage_gb": 1, "region": "local"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d (want 201): %s", code, body)
	}
	var env api.Environment
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode create: %v (%s)", err, body)
	}
	if env.State != "running" {
		t.Fatalf("created state = %q, want running", env.State)
	}
	if env.ID == "" || env.URL == "" {
		t.Fatalf("created env missing id/url: %+v", env)
	}
	id := env.ID
	cleanupEnv(t, c, base, id)
	t.Logf("deployed %s state=%s url=%s", id, env.State, env.URL)

	// --- 2. get ---
	code, body = req(t, c, "GET", base+"/v1/environments/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d (want 200): %s", code, body)
	}
	json.Unmarshal(body, &env)
	if env.State != "running" {
		t.Fatalf("get state = %q, want running", env.State)
	}

	// --- 3. list (filtered by task) ---
	code, body = req(t, c, "GET", base+"/v1/environments?task_id="+task, nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d (want 200): %s", code, body)
	}
	var list api.EnvironmentList
	json.Unmarshal(body, &list)
	if len(list.Environments) != 1 || list.Environments[0].ID != id {
		t.Fatalf("list = %+v, want exactly [%s]", list.Environments, id)
	}

	// --- 4. snapshot ---
	code, body = req(t, c, "POST", base+"/v1/environments/"+id+"/snapshots", map[string]any{
		"comment": "e2e checkpoint",
	})
	if code != http.StatusCreated {
		t.Fatalf("snapshot = %d (want 201): %s", code, body)
	}
	var snap api.Snapshot
	json.Unmarshal(body, &snap)
	if snap.ID == "" || snap.VMID != id {
		t.Fatalf("snapshot missing id or wrong vm_id: %+v", snap)
	}
	t.Logf("snapshot %s of %s", snap.ID, snap.VMID)

	// --- 5. list snapshots ---
	code, body = req(t, c, "GET", base+"/v1/snapshots?vm_id="+id, nil)
	if code != http.StatusOK {
		t.Fatalf("list snapshots = %d (want 200): %s", code, body)
	}
	var snaps api.SnapshotList
	json.Unmarshal(body, &snaps)
	found := false
	for _, s := range snaps.Snapshots {
		if s.ID == snap.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot %s not in list %+v", snap.ID, snaps.Snapshots)
	}

	// --- 6. restore ---
	code, body = req(t, c, "POST", base+"/v1/snapshots/"+snap.ID+"?action=restore", nil)
	if code != http.StatusNoContent {
		t.Fatalf("restore = %d (want 204): %s", code, body)
	}

	// --- 7. drain (graceful first phase of teardown) ---
	code, body = req(t, c, "POST", base+"/v1/environments/"+id+"?action=drain", nil)
	if code != http.StatusOK {
		t.Fatalf("drain = %d (want 200): %s", code, body)
	}
	json.Unmarshal(body, &env)
	if env.State != "draining" {
		t.Fatalf("drain state = %q, want draining", env.State)
	}

	// --- 8. destroy ---
	code, body = req(t, c, "DELETE", base+"/v1/environments/"+id, nil)
	if code != http.StatusNoContent {
		t.Fatalf("destroy = %d (want 204): %s", code, body)
	}

	// --- 9. confirm gone ---
	code, body = req(t, c, "GET", base+"/v1/environments/"+id, nil)
	if code != http.StatusNotFound {
		t.Fatalf("get after destroy = %d (want 404): %s", code, body)
	}
}

// hostTarget returns the URL+token to register as a host. Remote: the real
// Firecracker host; stub: a placeholder (host-CRUD never calls the provider).
func hostTarget() (string, string) {
	u, tok := remoteTarget()
	if u == "" {
		u = "http://stub-host:8090"
	}
	return u, tok
}

// TestE2E_HostsAPI exercises the full host-management surface:
// register → list → get → cordon → uncordon → remove → confirm gone.
func TestE2E_HostsAPI(t *testing.T) {
	srv := setupServer(t)
	c := srv.Client()
	base := srv.URL

	hostURL, hostTok := hostTarget()
	hostID := "host-e2e-1"

	// register
	code, body := req(t, c, "POST", base+"/v1/hosts", map[string]any{
		"id":    hostID,
		"url":   hostURL,
		"token": hostTok,
		"capacity": map[string]any{
			"cpus": 64, "ram_mb": 131072, "storage_gb": 2048, "vm_count": 100,
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("register host = %d (want 201): %s", code, body)
	}
	var host api.HostInfo
	json.Unmarshal(body, &host)
	if host.ID != hostID || host.State != "active" {
		t.Fatalf("registered host = %+v", host)
	}

	// list
	code, body = req(t, c, "GET", base+"/v1/hosts", nil)
	if code != http.StatusOK {
		t.Fatalf("list hosts = %d: %s", code, body)
	}
	var hosts api.HostList
	json.Unmarshal(body, &hosts)
	if len(hosts.Hosts) != 1 || hosts.Hosts[0].ID != hostID {
		t.Fatalf("list hosts = %+v", hosts.Hosts)
	}

	// get
	code, body = req(t, c, "GET", base+"/v1/hosts/"+hostID, nil)
	if code != http.StatusOK {
		t.Fatalf("get host = %d: %s", code, body)
	}

	// cordon (mark unschedulable) then uncordon
	if code, body = req(t, c, "POST", base+"/v1/hosts/"+hostID+"?action=cordon", nil); code != http.StatusNoContent {
		t.Fatalf("cordon = %d (want 204): %s", code, body)
	}
	if code, body = req(t, c, "GET", base+"/v1/hosts/"+hostID, nil); code != http.StatusOK {
		t.Fatalf("get after cordon = %d (want 200): %s", code, body)
	}
	json.Unmarshal(body, &host)
	if host.State != "cordoned" {
		t.Fatalf("after cordon state = %q, want cordoned", host.State)
	}
	if code, body = req(t, c, "POST", base+"/v1/hosts/"+hostID+"?action=uncordon", nil); code != http.StatusNoContent {
		t.Fatalf("uncordon = %d (want 204): %s", code, body)
	}

	// remove + confirm gone
	if code, body = req(t, c, "DELETE", base+"/v1/hosts/"+hostID, nil); code != http.StatusNoContent {
		t.Fatalf("remove host = %d (want 204): %s", code, body)
	}
	if code, _ = req(t, c, "GET", base+"/v1/hosts/"+hostID, nil); code != http.StatusNotFound {
		t.Fatalf("get removed host = %d (want 404)", code)
	}
}

// TestE2E_RotateToken deploys an environment and rotates its per-VM auth token.
func TestE2E_RotateToken(t *testing.T) {
	srv := setupServer(t)
	c := srv.Client()
	base := srv.URL

	id := mustCreate(t, c, base, "rotate")

	code, body := req(t, c, "POST", base+"/v1/environments/"+id+"?action=rotate-token", nil)
	if code != http.StatusNoContent {
		t.Fatalf("rotate-token = %d (want 204): %s", code, body)
	}
}

// TestE2E_Events subscribes to an environment's SSE stream and confirms it
// delivers at least one event (the initial state snapshot).
func TestE2E_Events(t *testing.T) {
	srv := setupServer(t)
	c := srv.Client()
	base := srv.URL

	id := mustCreate(t, c, base, "events")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/environments/"+id+"/events", nil)
	resp, err := c.Do(r)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events stream = %d, want 200", resp.StatusCode)
	}

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				got <- line
				return
			}
		}
	}()

	select {
	case <-got:
		// received the snapshot/state event — stream works
	case <-time.After(4 * time.Second):
		t.Fatal("no SSE event received within 4s")
	}
}

// mustCreate provisions an environment and returns its ID, failing the test on
// any non-201 response.
func mustCreate(t *testing.T, c *http.Client, base, task string) string {
	t.Helper()
	task = task + "-" + runID()
	code, body := req(t, c, "POST", base+"/v1/environments", map[string]any{
		"task_id": task,
		"spec":    map[string]any{"cpus": 1, "ram_mb": 256, "storage_gb": 1, "region": "local"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create %s = %d (want 201): %s", task, code, body)
	}
	var env api.Environment
	json.Unmarshal(body, &env)
	if env.ID == "" {
		t.Fatalf("create %s: no id in %s", task, body)
	}
	cleanupEnv(t, c, base, env.ID)
	return env.ID
}
