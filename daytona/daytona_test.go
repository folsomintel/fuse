package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/surf-dev/surf/apps/orchestrator"
)

// fakeServer wires a deterministic Daytona mock around an httptest server.
// Tests select behavior per endpoint by setting fields on the returned
// struct.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	createCount  atomic.Int32
	deleteCount  atomic.Int32
	listSandboxes []Sandbox
	previewURL    string
	previewToken  string
	executeCalls  atomic.Int32
	uploadCalls   atomic.Int32
	uploadPaths   []string
	executeBodies []string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t, previewURL: "https://3000-sb.daytonaproxy01.net", previewToken: "tok-123"}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/sandbox", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			f.createCount.Add(1)
			var req CreateSandboxRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			labels := req.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			sb := Sandbox{ID: "sb-new", State: "started", Labels: labels}
			f.listSandboxes = append(f.listSandboxes, sb)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sb)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.listSandboxes)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// /api/sandbox/{id} catch-all — handle DELETE + preview-url subroute.
	mux.HandleFunc("/api/sandbox/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sandbox/")
		// preview URL: <id>/ports/<port>/preview-url
		if strings.Contains(path, "/ports/") && strings.HasSuffix(path, "/preview-url") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(PreviewURL{
				URL:   f.previewURL,
				Token: f.previewToken,
			})
			return
		}
		// DELETE /api/sandbox/{id}
		if r.Method == http.MethodDelete {
			f.deleteCount.Add(1)
			id := strings.SplitN(path, "?", 2)[0]
			for i := range f.listSandboxes {
				if f.listSandboxes[i].ID == id {
					f.listSandboxes[i].State = "destroyed"
					f.listSandboxes[i].DesiredState = "destroyed"
					break
				}
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		// GET /api/sandbox/{id}
		if r.Method == http.MethodGet {
			id := path
			for _, sb := range f.listSandboxes {
				if sb.ID == id {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(sb)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/toolbox/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/toolbox/process/execute") && r.Method == http.MethodPost:
			f.executeCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]string
			_ = json.Unmarshal(body, &parsed)
			f.executeBodies = append(f.executeBodies, parsed["command"])
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecResponse{ExitCode: 0, Result: ""})
		case strings.HasSuffix(r.URL.Path, "/toolbox/files/upload") && r.Method == http.MethodPost:
			f.uploadCalls.Add(1)
			f.uploadPaths = append(f.uploadPaths, r.URL.Query().Get("path"))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/toolbox/process/session") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/toolbox/process/session/") && strings.HasSuffix(r.URL.Path, "/exec"):
			f.executeCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			if cmd, ok := parsed["command"].(string); ok {
				f.executeBodies = append(f.executeBodies, cmd)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(SessionExecResponse{CmdID: "cmd-1"})
		case strings.Contains(r.URL.Path, "/toolbox/process/session/") && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("log line\n"))
		default:
			http.Error(w, "unhandled toolbox path: "+r.URL.Path, http.StatusNotFound)
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) provider() *Provider {
	return New(Config{
		BaseURL:    f.srv.URL,
		APIKey:     "test-key",
		HTTPClient: f.srv.Client(),
	})
}

func TestProvider_SatisfiesInterface(t *testing.T) {
	var _ orchestrator.Provider = New(Config{})
}

func TestProvider_Create_TagsSurfNameLabel(t *testing.T) {
	f := newFakeServer(t)
	p := f.provider()
	env, err := p.Create(context.Background(), orchestrator.Spec{Name: "task-7"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if env.Name() != "task-7" {
		t.Errorf("Name = %q, want task-7", env.Name())
	}
	if env.URL() == "" || env.Token() == "" {
		t.Errorf("expected preview URL+Token to be cached, got url=%q token=%q", env.URL(), env.Token())
	}
	if got := f.listSandboxes[0].Labels[surfNameLabel]; got != "task-7" {
		t.Errorf("surf-name label = %q, want task-7", got)
	}
}

func TestProvider_Get_FiltersDestroyed(t *testing.T) {
	f := newFakeServer(t)
	f.listSandboxes = []Sandbox{
		{ID: "sb-old", State: "destroyed", Labels: map[string]string{surfNameLabel: "task-1"}},
	}
	p := f.provider()
	_, err := p.Get(context.Background(), "task-1")
	if !errors.Is(err, orchestrator.ErrVMNotFound) {
		t.Fatalf("Get: want ErrVMNotFound, got %v", err)
	}
}

func TestProvider_Get_ReturnsLiveSandbox(t *testing.T) {
	f := newFakeServer(t)
	f.listSandboxes = []Sandbox{
		{ID: "sb-1", State: "started", Labels: map[string]string{surfNameLabel: "task-x"}},
	}
	p := f.provider()
	env, err := p.Get(context.Background(), "task-x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if env.Name() != "task-x" {
		t.Errorf("Name = %q", env.Name())
	}
}

func TestProvider_List_FiltersByPrefix(t *testing.T) {
	f := newFakeServer(t)
	f.listSandboxes = []Sandbox{
		{ID: "a", State: "started", Labels: map[string]string{surfNameLabel: "surf-1"}},
		{ID: "b", State: "started", Labels: map[string]string{surfNameLabel: "surf-2"}},
		{ID: "c", State: "started", Labels: map[string]string{surfNameLabel: "other"}},
		{ID: "d", State: "destroyed", Labels: map[string]string{surfNameLabel: "surf-3"}},
	}
	envs, err := f.provider().List(context.Background(), "surf-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("got %d envs, want 2 (surf-1, surf-2; surf-3 destroyed, other doesn't match)", len(envs))
	}
	names := []string{envs[0].Name(), envs[1].Name()}
	if !contains(names, "surf-1") || !contains(names, "surf-2") {
		t.Errorf("got %v, want both surf-1 and surf-2", names)
	}
}

func TestProvider_Destroy_Idempotent(t *testing.T) {
	f := newFakeServer(t)
	f.listSandboxes = []Sandbox{
		{ID: "sb-x", State: "started", Labels: map[string]string{surfNameLabel: "task-d"}},
	}
	p := f.provider()
	if err := p.Destroy(context.Background(), "task-d"); err != nil {
		t.Fatalf("first destroy: %v", err)
	}
	if err := p.Destroy(context.Background(), "task-d"); err != nil {
		t.Fatalf("second destroy (already gone) should be nil, got %v", err)
	}
	if got := f.deleteCount.Load(); got != 1 {
		t.Errorf("delete count = %d, want 1 (second call should be a no-op since state=destroyed)", got)
	}
}

func TestSandbox_Upload_MkdirsParentOncePerDir(t *testing.T) {
	f := newFakeServer(t)
	env, err := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-up"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Two uploads into the same parent dir: the parent is mkdir'd once and
	// cached for the second.
	if err := env.Upload(context.Background(), []byte("body"), "/surf/manifest.json"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := env.Upload(context.Background(), []byte("body"), "/surf/secrets.json"); err != nil {
		t.Fatalf("Upload 2: %v", err)
	}
	if got := f.executeCalls.Load(); got != 1 {
		t.Errorf("execute (mkdir) calls = %d, want 1 (cached after first upload to /surf)", got)
	}
	if !strings.Contains(strings.Join(f.executeBodies, "|"), "mkdir -p '/surf'") {
		t.Errorf("expected mkdir of the upload's parent dir, got %v", f.executeBodies)
	}
	if got := f.uploadCalls.Load(); got != 2 {
		t.Errorf("upload calls = %d, want 2", got)
	}
}

func TestSandbox_Upload_DistinctDirsEachMkdir(t *testing.T) {
	f := newFakeServer(t)
	env, _ := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-u2"})
	// Uploads to two distinct parent dirs each trigger one mkdir; the third
	// (back to the first dir) is cached.
	if err := env.Upload(context.Background(), []byte("x"), "/surf/manifest.json"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := env.Upload(context.Background(), []byte("x"), "/surf/tls/cert.pem"); err != nil {
		t.Fatalf("Upload 2: %v", err)
	}
	if err := env.Upload(context.Background(), []byte("x"), "/surf/secrets.json"); err != nil {
		t.Fatalf("Upload 3: %v", err)
	}
	if got := f.executeCalls.Load(); got != 2 {
		t.Errorf("execute (mkdir) calls = %d, want 2 (/surf and /surf/tls, second /surf cached)", got)
	}
}

func TestSandbox_StartAgent_Idempotent(t *testing.T) {
	f := newFakeServer(t)
	env, _ := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-s"})
	spec := orchestrator.AgentSpec{Command: "SURF_CONTAINER_BIN=/bin/true /home/daytona/surfd --listen 0.0.0.0:3000"}
	if err := env.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	execAfterFirst := f.executeCalls.Load()
	// Second call must be a no-op (no new SessionExec).
	if err := env.StartAgent(context.Background(), spec); err != nil {
		t.Fatalf("second StartAgent: %v", err)
	}
	if got := f.executeCalls.Load(); got != execAfterFirst {
		t.Errorf("second StartAgent triggered %d new execute calls, want 0 (idempotent)", got-execAfterFirst)
	}
}

// TestSandbox_StartAgent_RunsCommandVerbatim asserts that Daytona runs
// AgentSpec.Command verbatim via the session API (the full command line is
// assembled in core's SurfdAgentSpec, not in this package).
func TestSandbox_StartAgent_RunsCommandVerbatim(t *testing.T) {
	f := newFakeServer(t)
	env, _ := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-v"})
	const cmd = "SURF_CONTAINER_BIN=/bin/true /home/daytona/surfd --listen 0.0.0.0:3000 --manifest /surf/manifest.json --insecure"
	if err := env.StartAgent(context.Background(), orchestrator.AgentSpec{Command: cmd}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if !contains(f.executeBodies, cmd) {
		t.Errorf("expected exact agent command %q to be exec'd, got %v", cmd, f.executeBodies)
	}
}

func TestSandbox_StartAgent_DownloadsBinaryWhenURLConfigured(t *testing.T) {
	f := newFakeServer(t)
	p := New(Config{
		BaseURL:     f.srv.URL,
		APIKey:      "k",
		HTTPClient:  f.srv.Client(),
		DownloadURL: "https://example.com/surfd",
	})
	env, err := p.Create(context.Background(), orchestrator.Spec{Name: "task-d"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := env.StartAgent(context.Background(), orchestrator.AgentSpec{
		Command: "/home/daytona/surfd --insecure",
	}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	joined := strings.Join(f.executeBodies, "|")
	if !strings.Contains(joined, "curl -fsSL") {
		t.Errorf("expected curl command for agent download, got: %s", joined)
	}
	if !strings.Contains(joined, "https://example.com/surfd") {
		t.Errorf("expected download URL in command, got: %s", joined)
	}
}

// TestSandbox_StartAgent_DownloadURLFromSpec verifies AgentSpec.DownloadURL
// takes precedence (Config.DownloadURL is the back-compat fallback).
func TestSandbox_StartAgent_DownloadURLFromSpec(t *testing.T) {
	f := newFakeServer(t)
	env, _ := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-ds"})
	if err := env.StartAgent(context.Background(), orchestrator.AgentSpec{
		Command:     "/home/daytona/surfd --insecure",
		DownloadURL: "https://example.com/agent-from-spec",
	}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if !strings.Contains(strings.Join(f.executeBodies, "|"), "https://example.com/agent-from-spec") {
		t.Errorf("expected spec download URL in command, got: %v", f.executeBodies)
	}
}

func TestSandbox_TokenIsPreviewToken(t *testing.T) {
	f := newFakeServer(t)
	f.previewToken = "preview-xyz"
	env, _ := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-t"})
	if got := env.Token(); got != "preview-xyz" {
		t.Errorf("Token = %q, want preview-xyz", got)
	}
	// SetToken is preserved for surfd auth-token but doesn't override preview.
	env.(orchestrator.TokenSetter).SetToken("surfd-internal")
	if got := env.Token(); got != "preview-xyz" {
		t.Errorf("after SetToken, Token = %q, want preview-xyz (preview wins)", got)
	}
}

func TestSandbox_Exec_ReturnsResult(t *testing.T) {
	f := newFakeServer(t)
	env, _ := f.provider().Create(context.Background(), orchestrator.Spec{Name: "task-e"})
	out, err := env.Exec(context.Background(), "echo", "hi")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !bytes.Equal(out, []byte{}) {
		// Mock returns empty result; just ensure no error path.
	}
	last := f.executeBodies[len(f.executeBodies)-1]
	if last != "echo hi" {
		t.Errorf("execute command = %q, want %q", last, "echo hi")
	}
}

func TestShellEscape(t *testing.T) {
	cases := []struct{ in, out string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"it's mine", `'it'\''s mine'`},
	}
	for _, c := range cases {
		if got := shellEscape(c.in); got != c.out {
			t.Errorf("shellEscape(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestIsDestroyed(t *testing.T) {
	cases := []struct {
		sb   Sandbox
		want bool
	}{
		{Sandbox{State: "started"}, false},
		{Sandbox{State: "destroyed"}, true},
		{Sandbox{State: "Destroying"}, true}, // case-insensitive
		{Sandbox{State: "started", DesiredState: "destroyed"}, true},
	}
	for _, c := range cases {
		if got := isDestroyed(&c.sb); got != c.want {
			t.Errorf("isDestroyed(%+v) = %v, want %v", c.sb, got, c.want)
		}
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
