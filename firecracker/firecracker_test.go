// TODO: Add error injection tests — mock the HTTP server to return 500s,
// connection resets, and timeouts. Verify the client surfaces useful errors
// and (once retry logic is added) retries correctly.

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/surf-dev/surf/apps/orchestrator/internal/core"
)

// ── Stub provider tests ─────────────────────────────────────────────────

func TestNew_stub_when_no_baseurl(t *testing.T) {
	p := New(Config{})
	if p.stub == nil {
		t.Fatal("expected stub provider when BaseURL is empty")
	}
}

func TestNew_stub_explicit(t *testing.T) {
	p := New(Config{UseStub: true, BaseURL: "http://localhost"})
	if p.stub == nil {
		t.Fatal("expected stub provider when UseStub=true")
	}
}

func TestNew_real_when_baseurl(t *testing.T) {
	p := New(Config{BaseURL: "http://localhost:8080"})
	if p.stub != nil {
		t.Fatal("expected real provider when BaseURL is set")
	}
}

func TestStub_create_and_get(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	env, err := p.Create(ctx, orchestrator.Spec{Name: "vm-1", CPUs: 2})
	if err != nil {
		t.Fatal(err)
	}
	if env.Name() != "vm-1" {
		t.Fatalf("expected name vm-1, got %q", env.Name())
	}
	if env.URL() != "fc://vm-1" {
		t.Fatalf("expected fc://vm-1, got %q", env.URL())
	}

	// Get the same env.
	got, err := p.Get(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != "vm-1" {
		t.Fatalf("expected vm-1, got %q", got.Name())
	}
}

func TestStub_create_duplicate_fails(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	_, err := p.Create(ctx, orchestrator.Spec{Name: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.Create(ctx, orchestrator.Spec{Name: "vm-1"})
	if err == nil {
		t.Fatal("expected error for duplicate create")
	}
}

func TestStub_get_not_found(t *testing.T) {
	p := New(Config{UseStub: true})
	_, err := p.Get(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error for missing env")
	}
}

func TestStub_destroy(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	p.Create(ctx, orchestrator.Spec{Name: "vm-1"})
	if err := p.Destroy(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}

	_, err := p.Get(ctx, "vm-1")
	if err == nil {
		t.Fatal("expected not found after destroy")
	}
}

func TestStub_list(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	p.Create(ctx, orchestrator.Spec{Name: "surf-a"})
	p.Create(ctx, orchestrator.Spec{Name: "surf-b"})
	p.Create(ctx, orchestrator.Spec{Name: "other-c"})

	all, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 envs, got %d", len(all))
	}

	filtered, err := p.List(ctx, "surf-")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Fatalf("expected 2 envs with prefix 'surf-', got %d", len(filtered))
	}
}

func TestStub_close(t *testing.T) {
	p := New(Config{UseStub: true})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStub_upload_and_checkpoint(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	env, _ := p.Create(ctx, orchestrator.Spec{Name: "vm-1"})

	if err := env.Upload(ctx, []byte("hello"), "/data/file.txt"); err != nil {
		t.Fatal(err)
	}

	sc, ok := env.(orchestrator.SnapshotCapable)
	if !ok {
		t.Fatal("env does not implement SnapshotCapable")
	}
	cpID, err := sc.Checkpoint(ctx, "first snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if cpID == "" {
		t.Fatal("expected non-empty checkpoint ID")
	}

	cps, err := sc.ListCheckpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(cps))
	}
	if cps[0].Comment != "first snapshot" {
		t.Fatalf("expected comment, got %q", cps[0].Comment)
	}
}

func TestStub_restore(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	env, _ := p.Create(ctx, orchestrator.Spec{Name: "vm-1"})
	sc, ok := env.(orchestrator.SnapshotCapable)
	if !ok {
		t.Fatal("env does not implement SnapshotCapable")
	}
	cpID, _ := sc.Checkpoint(ctx, "snap")

	if err := sc.Restore(ctx, cpID); err != nil {
		t.Fatal(err)
	}

	// Restore with unknown checkpoint.
	if err := sc.Restore(ctx, "nonexistent"); err == nil {
		t.Fatal("expected error for unknown checkpoint")
	}
}

func TestStub_start_agent(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	env, _ := p.Create(ctx, orchestrator.Spec{Name: "vm-1"})
	if err := env.StartAgent(ctx, orchestrator.AgentSpec{Command: "run-agent"}); err != nil {
		t.Fatal(err)
	}
}

// ── Remote provider tests (using httptest) ──────────────────────────────

func TestRemote_create(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vm" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(createVMResponse{VMID: "vm-123", URL: "http://vm-123.local"})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "test-token"})
	env, err := p.Create(context.Background(), orchestrator.Spec{Name: "vm-123", CPUs: 2})
	if err != nil {
		t.Fatal(err)
	}
	if env.Name() != "vm-123" {
		t.Fatalf("expected vm-123, got %q", env.Name())
	}
	if env.URL() != "http://vm-123.local" {
		t.Fatalf("expected URL, got %q", env.URL())
	}
}

func TestRemote_destroy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	if err := p.Destroy(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
}

func TestRemote_list(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listVMResponse{}
		resp.VMs = []struct {
			VMID string `json:"vm_id"`
			URL  string `json:"url"`
		}{
			{VMID: "vm-1", URL: "http://vm-1"},
			{VMID: "vm-2", URL: "http://vm-2"},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	envs, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("expected 2 envs, got %d", len(envs))
	}
}

func TestRemote_http_error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "server error")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	_, err := p.Create(context.Background(), orchestrator.Spec{Name: "fail"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	// doJSON must wrap non-2xx responses in *httpStatusError carrying the
	// status code, and its Error() string must remain byte-identical to the
	// previous fmt.Errorf("http %d: %s", ...) so text-matching callers keep
	// working.
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected *httpStatusError, got %T: %v", err, err)
	}
	if statusErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected code 500, got %d", statusErr.Code)
	}
	if statusErr.Error() != "http 500: server error" {
		t.Fatalf("unexpected Error() string: %q", statusErr.Error())
	}
}

// ── StartAgent: /start-agent happy path + 404 fallback ──────────────────

func TestStartAgent_start_agent_happy_path(t *testing.T) {
	var (
		gotStartAgent  bool
		gotStartSurfd  bool
		gotDownloadURL string
		gotBinaryPath  string
		gotListen      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vm/vm-1/start-agent":
			gotStartAgent = true
			var req startAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode start-agent body: %v", err)
			}
			gotDownloadURL = req.DownloadURL
			gotBinaryPath = req.BinaryPath
			gotListen = req.Listen
			w.WriteHeader(http.StatusOK)
		case "/v1/vm/vm-1/start-surfd":
			gotStartSurfd = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, DownloadURL: "https://example.com/surfd"})
	env := &remoteEnv{id: "vm-1", client: p}

	if err := env.StartAgent(context.Background(), orchestrator.AgentSpec{AuthToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if !gotStartAgent {
		t.Fatal("expected /start-agent to be called")
	}
	if gotStartSurfd {
		t.Fatal("did not expect /start-surfd fallback on 200")
	}
	// Provider-configured DownloadURL must be forwarded when spec has none.
	if gotDownloadURL != "https://example.com/surfd" {
		t.Fatalf("expected download_url forwarded, got %q", gotDownloadURL)
	}
	// binary_path / listen stay unset (omitempty) so the host uses its defaults.
	if gotBinaryPath != "" || gotListen != "" {
		t.Fatalf("expected binary_path/listen unset, got %q / %q", gotBinaryPath, gotListen)
	}
}

func TestStartAgent_spec_download_url_overrides_provider(t *testing.T) {
	var gotDownloadURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vm/vm-1/start-agent" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req startAgentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotDownloadURL = req.DownloadURL
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, DownloadURL: "https://provider.example/surfd"})
	env := &remoteEnv{id: "vm-1", client: p}

	spec := orchestrator.AgentSpec{DownloadURL: "https://spec.example/surfd"}
	if err := env.StartAgent(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if gotDownloadURL != "https://spec.example/surfd" {
		t.Fatalf("expected spec download_url to win, got %q", gotDownloadURL)
	}
}

func TestStartAgent_404_falls_back_to_start_surfd(t *testing.T) {
	var (
		gotStartAgent bool
		gotStartSurfd bool
		surfdBody     startSurfdRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vm/vm-1/start-agent":
			// Mimic the live host: unknown action falls through to 404.
			gotStartAgent = true
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "not found")
		case "/v1/vm/vm-1/start-surfd":
			gotStartSurfd = true
			if err := json.NewDecoder(r.Body).Decode(&surfdBody); err != nil {
				t.Fatalf("decode start-surfd body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, DownloadURL: "https://example.com/surfd"})
	env := &remoteEnv{id: "vm-1", client: p}

	spec := orchestrator.AgentSpec{
		AuthToken:    "tok",
		Gateway:      "wss://gw",
		GatewayToken: "gw-tok",
	}
	if err := env.StartAgent(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !gotStartAgent {
		t.Fatal("expected /start-agent to be attempted first")
	}
	if !gotStartSurfd {
		t.Fatal("expected fallback to /start-surfd after 404")
	}
	// The fallback payload must be the FROZEN start-surfd wire.
	want := startSurfdRequest{
		ManifestPath: surfdManifestGuestPath,
		SecretsPath:  surfdSecretsGuestPath,
		TLSCertPath:  surfdTLSCertGuestPath,
		TLSKeyPath:   surfdTLSKeyGuestPath,
		AuthToken:    "tok",
		Gateway:      "wss://gw",
		GatewayToken: "gw-tok",
	}
	if surfdBody != want {
		t.Fatalf("start-surfd wire mismatch:\n got %+v\nwant %+v", surfdBody, want)
	}
}

func TestStartAgent_non_404_error_propagates(t *testing.T) {
	var gotStartSurfd bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vm/vm-1/start-agent":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
		case "/v1/vm/vm-1/start-surfd":
			gotStartSurfd = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	env := &remoteEnv{id: "vm-1", client: p}

	err := env.StartAgent(context.Background(), orchestrator.AgentSpec{})
	if err == nil {
		t.Fatal("expected non-404 error to propagate")
	}
	if gotStartSurfd {
		t.Fatal("must not fall back to /start-surfd on non-404 error")
	}
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected *httpStatusError code 500, got %T: %v", err, err)
	}
}

func TestRemote_auth_header(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-token" {
			t.Errorf("expected Bearer my-token, got %q", auth)
		}
		json.NewEncoder(w).Encode(createVMResponse{VMID: "vm-1"})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "my-token"})
	p.Create(context.Background(), orchestrator.Spec{Name: "vm-1"})
}
