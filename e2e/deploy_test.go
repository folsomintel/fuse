package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/andrewn6/fuse/api"
	"github.com/andrewn6/fuse/firecracker"
	"github.com/andrewn6/fuse/internal/core"
)

// setupServer assembles the full Fuse stack the way server/main.go does and
// returns a running httptest server plus a cleanup func.
//
// Provider selection mirrors the real binary: a real Firecracker host when
// FUSE_E2E_FIRECRACKER_URL is set, otherwise the in-memory stub (hermetic).
func setupServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	var provider orchestrator.Provider
	if url := os.Getenv("FUSE_E2E_FIRECRACKER_URL"); url != "" {
		t.Logf("e2e: targeting real Firecracker host %s", url)
		provider = firecracker.New(firecracker.Config{
			BaseURL: url,
			Token:   os.Getenv("FUSE_E2E_FIRECRACKER_TOKEN"),
		})
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
	cleanup := func() {
		srv.Close()
		cancel()
		fm.Stop()
	}
	return srv, cleanup
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
	srv, cleanup := setupServer(t)
	defer cleanup()
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
	code, body := req(t, c, "POST", base+"/v1/environments", map[string]any{
		"task_id": "e2e-1",
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
	code, body = req(t, c, "GET", base+"/v1/environments?task_id=e2e-1", nil)
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
