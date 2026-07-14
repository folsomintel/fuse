package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// provisionEnv creates a VM through the real handler path so exec tests start
// from a genuinely Running VM, and hands back the fake env behind it.
func provisionEnv(t *testing.T, h http.Handler, p *fakeProvider) *fakeEnv {
	t.Helper()
	if rr := doJSON(t, h, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs["fuse-task-1"]
	if !ok {
		t.Fatal("fake env fuse-task-1 not found after create")
	}
	return e
}

func decodeExec(t *testing.T, body io.Reader) ExecEnvironmentResponse {
	t.Helper()
	var res ExecEnvironmentResponse
	if err := json.NewDecoder(body).Decode(&res); err != nil {
		t.Fatalf("decode exec response: %v", err)
	}
	return res
}

func TestEnvironmentAction_Exec_HappyPath(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)
	env.execResult = orchestrator.ExecResult{Stdout: []byte("hello\n")}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"echo", "hello"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}

	res := decodeExec(t, rr.Body)
	if res.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello\n")
	}

	got := env.execInvocations()
	if len(got) != 1 {
		t.Fatalf("exec called %d times, want 1", len(got))
	}
	if len(got[0]) != 2 || got[0][0] != "echo" || got[0][1] != "hello" {
		t.Errorf("argv = %q, want [echo hello]", got[0])
	}
}

// The whole point of the endpoint: a command that fails is a successful HTTP
// call reporting a failure, not an HTTP error.
func TestEnvironmentAction_Exec_NonZeroExitIs200(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)
	env.execResult = orchestrator.ExecResult{
		ExitCode: 2,
		Stdout:   []byte("out\n"),
		Stderr:   []byte("no such file\n"),
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"cat", "/nope"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a non-zero guest exit. body: %s", rr.Code, rr.Body.String())
	}

	res := decodeExec(t, rr.Body)
	if res.ExitCode != 2 {
		t.Errorf("exit_code = %d, want 2", res.ExitCode)
	}
	if res.Stdout != "out\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out\n")
	}
	if res.Stderr != "no such file\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "no such file\n")
	}
}

func TestEnvironmentAction_Exec_ShellWrapsInSh(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec",
		ExecEnvironmentRequest{Shell: "ls /tmp | wc -l"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}

	got := env.execInvocations()
	if len(got) != 1 {
		t.Fatalf("exec called %d times, want 1", len(got))
	}
	want := []string{"sh", "-lc", "ls /tmp | wc -l"}
	if len(got[0]) != len(want) {
		t.Fatalf("argv = %q, want %q", got[0], want)
	}
	for i := range want {
		if got[0][i] != want[i] {
			t.Fatalf("argv = %q, want %q", got[0], want)
		}
	}
}

func TestEnvironmentAction_Exec_TimeoutForwarded(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"sleep", "1"}, TimeoutMS: 5000})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}

	env.mu.Lock()
	opts := env.execOpts
	env.mu.Unlock()
	if len(opts) != 1 {
		t.Fatalf("exec called %d times, want 1", len(opts))
	}
	if opts[0].Timeout.Milliseconds() != 5000 {
		t.Errorf("timeout = %v, want 5s", opts[0].Timeout)
	}
}

func TestEnvironmentAction_Exec_BadRequests(t *testing.T) {
	tests := []struct {
		name string
		body ExecEnvironmentRequest
	}{
		{"neither cmd nor shell", ExecEnvironmentRequest{}},
		{"both cmd and shell", ExecEnvironmentRequest{Cmd: []string{"ls"}, Shell: "ls"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, p := newTestHandler(t)
			r := mustRouter(t, h)
			provisionEnv(t, r, p)

			rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec", tt.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
			}
			if code := decodeError(t, rr.Body).Error.Code; code != CodeInvalidArgument {
				t.Errorf("code = %q, want %q", code, CodeInvalidArgument)
			}
		})
	}
}

func TestEnvironmentAction_Exec_UnknownVMIs404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-nope?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"true"}})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", rr.Code, rr.Body.String())
	}
	if code := decodeError(t, rr.Body).Error.Code; code != CodeNotFound {
		t.Errorf("code = %q, want %q", code, CodeNotFound)
	}
}

func TestEnvironmentAction_Exec_DrainingVMIs409(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	provisionEnv(t, r, p)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil); rr.Code != http.StatusOK {
		t.Fatalf("drain status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"true"}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if code := decodeError(t, rr.Body).Error.Code; code != CodeConflict {
		t.Errorf("code = %q, want %q", code, CodeConflict)
	}
}

// A provider with no guest must say so rather than report a fabricated
// success, so the stub path maps to 501 and not 200 or 500.
func TestEnvironmentAction_Exec_UnsupportedProviderIs501(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := provisionEnv(t, r, p)
	env.execErr = orchestrator.ErrExecUnsupported

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=exec",
		ExecEnvironmentRequest{Cmd: []string{"true"}})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501. body: %s", rr.Code, rr.Body.String())
	}
	if code := decodeError(t, rr.Body).Error.Code; code != CodeUnimplemented {
		t.Errorf("code = %q, want %q", code, CodeUnimplemented)
	}
}

// Exec is root in the guest and API keys carry no scopes, so a non-master
// principal must be refused.
func TestEnvironmentAction_Exec_NonMasterForbidden(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	provisionEnv(t, r, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/environments/fuse-task-1?action=exec", nil)
	req = req.WithContext(withPrincipal(req.Context(), Principal{Master: false, KeyID: "key-1"}))
	rr := httptest.NewRecorder()
	h.execEnvironment(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rr.Code, rr.Body.String())
	}
	if code := decodeError(t, rr.Body).Error.Code; code != CodeUnauthorized {
		t.Errorf("code = %q, want %q", code, CodeUnauthorized)
	}
}

// ── Attach ────────────────────────────────────────────────────────

// dialAttach performs the client half of the upgrade against a real listening
// server. httptest.ResponseRecorder cannot be hijacked, so attach can only be
// tested over an actual socket.
func dialAttach(t *testing.T, srv *httptest.Server, path string) (net.Conn, *http.Response) {
	t.Helper()

	u := srv.Listener.Addr().String()
	c, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", hostwire.AttachProto)
	if err := req.Write(c); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return c, resp
}

func TestAttachEnvironment_RelaysBothDirections(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	srv := httptest.NewServer(r)
	defer srv.Close()

	env := provisionEnv(t, r, p)

	// guestSide stands in for the host agent: whatever the client writes
	// lands here, and whatever we write here must reach the client.
	guestSide, envSide := net.Pipe()
	defer func() { _ = guestSide.Close() }()
	env.attachStream = envSide

	conn, resp := dialAttach(t, srv, "/v1/environments/fuse-task-1/attach?tty=1&rows=24&cols=80&cmd=sh")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != hostwire.AttachProto {
		t.Errorf("Upgrade = %q, want %q", got, hostwire.AttachProto)
	}

	// The spec must survive the query round trip into the provider.
	env.mu.Lock()
	spec := env.attachSpec
	env.mu.Unlock()
	if !spec.TTY || spec.Rows != 24 || spec.Cols != 80 {
		t.Errorf("spec = %+v, want tty 24x80", spec)
	}
	if len(spec.Cmd) != 1 || spec.Cmd[0] != "sh" {
		t.Errorf("cmd = %q, want [sh]", spec.Cmd)
	}

	// client → guest
	if _, err := conn.Write([]byte("to-guest")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got := make([]byte, len("to-guest"))
	if _, err := io.ReadFull(guestSide, got); err != nil {
		t.Fatalf("guest read: %v", err)
	}
	if string(got) != "to-guest" {
		t.Errorf("guest received %q, want %q", got, "to-guest")
	}

	// guest → client
	if _, err := guestSide.Write([]byte("to-client")); err != nil {
		t.Fatalf("guest write: %v", err)
	}
	back := make([]byte, len("to-client"))
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(back) != "to-client" {
		t.Errorf("client received %q, want %q", back, "to-client")
	}
}

// Closing the guest side must end the session rather than leave the client
// hanging on a stream nothing will ever write to again.
func TestAttachEnvironment_GuestCloseEndsSession(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	srv := httptest.NewServer(r)
	defer srv.Close()

	env := provisionEnv(t, r, p)
	guestSide, envSide := net.Pipe()
	env.attachStream = envSide

	conn, resp := dialAttach(t, srv, "/v1/environments/fuse-task-1/attach?tty=1")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	if _, err := guestSide.Write([]byte("bye")); err != nil {
		t.Fatalf("guest write: %v", err)
	}
	_ = guestSide.Close()

	rest, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("client read to EOF: %v", err)
	}
	if string(rest) != "bye" {
		t.Errorf("client received %q before EOF, want %q", rest, "bye")
	}
}

// Without this the host agent's own refusal comes back as an opaque 500, which
// tells the caller nothing about what they got wrong.
func TestAttachEnvironment_RequiresTTY(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	srv := httptest.NewServer(r)
	defer srv.Close()

	provisionEnv(t, r, p)

	_, resp := dialAttach(t, srv, "/v1/environments/fuse-task-1/attach")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when tty is missing", resp.StatusCode)
	}
}

func TestAttachEnvironment_RequiresUpgradeHeader(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	provisionEnv(t, r, p)

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1/attach?tty=1", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	if code := decodeError(t, rr.Body).Error.Code; code != CodeInvalidArgument {
		t.Errorf("code = %q, want %q", code, CodeInvalidArgument)
	}
}

// The error must be reported as HTTP, which is only possible because the guest
// stream is opened before the connection is hijacked.
func TestAttachEnvironment_UnsupportedProviderIs501(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	srv := httptest.NewServer(r)
	defer srv.Close()

	env := provisionEnv(t, r, p)
	env.attachErr = orchestrator.ErrAttachUnsupported

	_, resp := dialAttach(t, srv, "/v1/environments/fuse-task-1/attach?tty=1")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestAttachEnvironment_UnknownVMIs404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	srv := httptest.NewServer(r)
	defer srv.Close()

	_, resp := dialAttach(t, srv, "/v1/environments/fuse-nope/attach?tty=1")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAttachEnvironment_NonMasterForbidden(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	provisionEnv(t, r, p)

	req := httptest.NewRequest(http.MethodGet, "/v1/environments/fuse-task-1/attach?tty=1", nil)
	req.Header.Set("Upgrade", hostwire.AttachProto)
	req = req.WithContext(withPrincipal(req.Context(), Principal{Master: false, KeyID: "key-1"}))
	rr := httptest.NewRecorder()
	h.attachEnvironment(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rr.Code, rr.Body.String())
	}
}
