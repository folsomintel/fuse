package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnvironmentsExec(t *testing.T) {
	var gotBody ExecRequest
	var gotPath, gotQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ExecResult{ExitCode: 0, Stdout: "hi\n"})
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	res, err := c.Environments.Exec(context.Background(), "fuse-1", ExecRequest{Cmd: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if gotPath != "/v1/environments/fuse-1" {
		t.Errorf("path = %q, want /v1/environments/fuse-1", gotPath)
	}
	if gotQuery != "action=exec" {
		t.Errorf("query = %q, want action=exec", gotQuery)
	}
	if len(gotBody.Cmd) != 2 || gotBody.Cmd[0] != "echo" {
		t.Errorf("cmd = %q, want [echo hi]", gotBody.Cmd)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hi\n")
	}
}

// A failing guest command must come back as a result, not an error, or a caller
// cannot tell it apart from an unreachable host.
func TestEnvironmentsExec_NonZeroExitIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ExecResult{ExitCode: 4, Stderr: "nope\n"})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	res, err := c.Environments.Exec(context.Background(), "fuse-1", ExecRequest{Shell: "false"})
	if err != nil {
		t.Fatalf("exec returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 4 {
		t.Errorf("exit code = %d, want 4", res.ExitCode)
	}
	if res.Stderr != "nope\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "nope\n")
	}
}

func TestEnvironmentsExec_Validation(t *testing.T) {
	c, _ := New("http://example.invalid", "tok")
	ctx := context.Background()

	tests := []struct {
		name string
		vmID string
		in   ExecRequest
	}{
		{"no vm id", "", ExecRequest{Cmd: []string{"ls"}}},
		{"neither cmd nor shell", "fuse-1", ExecRequest{}},
		{"both cmd and shell", "fuse-1", ExecRequest{Cmd: []string{"ls"}, Shell: "ls"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Environments.Exec(ctx, tt.vmID, tt.in); err == nil {
				t.Fatal("want a validation error, got nil")
			}
		})
	}
}

func TestEnvironmentsExec_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"conflict","message":"vm not running"}}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	if _, err := c.Environments.Exec(context.Background(), "fuse-1", ExecRequest{Cmd: []string{"ls"}}); err == nil {
		t.Fatal("want an error for a 409, got nil")
	}
}

// A guest command can outlast the default client timeout. Exec must run on the
// no-timeout client and bound itself by context instead, or a long --timeout is
// unreachable. Here the regular client's timeout is tiny and the server is
// slow: exec must still succeed because it does not use that client.
func TestEnvironmentsExec_UsesNoTimeoutClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ExecResult{Stdout: "slow\n"})
	}))
	defer srv.Close()

	// A regular client that would give up in 20ms - far shorter than the
	// server's 120ms - so a call routed through it would fail.
	c, err := New(srv.URL, "tok", WithHTTPClient(&http.Client{Timeout: 20 * time.Millisecond}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	res, err := c.Environments.Exec(context.Background(), "fuse-1",
		ExecRequest{Cmd: []string{"sleep", "1"}, TimeoutMS: 5000})
	if err != nil {
		t.Fatalf("exec failed - it used the timed-out client: %v", err)
	}
	if res.Stdout != "slow\n" {
		t.Errorf("stdout = %q, want the full result", res.Stdout)
	}
}

// The caller's own shorter deadline must still win over exec's derived one.
func TestEnvironmentsExec_CallerDeadlineStillWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(ExecResult{})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := c.Environments.Exec(ctx, "fuse-1", ExecRequest{Cmd: []string{"sleep", "1"}, TimeoutMS: 600000})
	if err == nil {
		t.Fatal("want the caller's 30ms deadline to fire, got nil")
	}
}

// ── Attach ────────────────────────────────────────────────────────

func TestFrameRoundTrip(t *testing.T) {
	// A server that echoes every stdin frame back as a stdout frame, then
	// reports an exit code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != AttachProto {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + AttachProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()

		peer := &AttachStream{conn: conn, r: buf.Reader}
		for {
			f, err := peer.ReadFrame()
			if err != nil {
				return
			}
			switch f.Type {
			case FrameStdin:
				if string(f.Payload) == "quit\n" {
					payload, _ := json.Marshal(ExitPayload{ExitCode: 9})
					_ = peer.WriteFrame(FrameExit, payload)
					return
				}
				_ = peer.WriteFrame(FrameStdout, f.Payload)
			case FrameResize:
				_ = peer.WriteFrame(FrameStdout, append([]byte("resized:"), f.Payload...))
			}
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	stream, err := c.Environments.Attach(context.Background(), "fuse-1", AttachOptions{
		Cmd:  []string{"sh"},
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := stream.ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if f.Type != FrameStdout || string(f.Payload) != "hello" {
		t.Errorf("frame = %d/%q, want stdout/hello", f.Type, f.Payload)
	}

	if err := stream.Resize(50, 100); err != nil {
		t.Fatalf("resize: %v", err)
	}
	f, err = stream.ReadFrame()
	if err != nil {
		t.Fatalf("read after resize: %v", err)
	}
	if got := string(f.Payload); got != `resized:{"cols":100,"rows":50}` {
		t.Errorf("resize echo = %q", got)
	}

	if _, err := stream.Write([]byte("quit\n")); err != nil {
		t.Fatalf("write quit: %v", err)
	}
	f, err = stream.ReadFrame()
	if err != nil {
		t.Fatalf("read exit: %v", err)
	}
	if f.Type != FrameExit {
		t.Fatalf("frame type = %d, want exit", f.Type)
	}
	var exit ExitPayload
	if err := json.Unmarshal(f.Payload, &exit); err != nil {
		t.Fatalf("decode exit: %v", err)
	}
	if exit.ExitCode != 9 {
		t.Errorf("exit code = %d, want 9", exit.ExitCode)
	}
}

// The attach query is what carries the spec, since an upgrade is a GET with no
// body. Repeated cmd params are what preserve argv boundaries.
func TestAttach_QueryCarriesSpec(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.RawQuery
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	_, _ = c.Environments.Attach(context.Background(), "fuse-1", AttachOptions{
		Cmd:  []string{"sh", "-c", "echo hi there"},
		Rows: 24,
		Cols: 80,
	})

	q := <-got
	want := "cmd=sh&cmd=-c&cmd=echo+hi+there&cols=80&rows=24&tty=1"
	if q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
}

// An error before the upgrade must surface in the same shape as every other
// call, not as a bare "unexpected status".
func TestAttach_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, `{"error":{"code":"unimplemented","message":"attach not supported by provider"}}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	_, err := c.Environments.Attach(context.Background(), "fuse-1", AttachOptions{})
	if err == nil {
		t.Fatal("want an error for a 501, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", apiErr.Status)
	}
}

func TestAttach_Validation(t *testing.T) {
	c, _ := New("http://example.invalid", "tok")
	if _, err := c.Environments.Attach(context.Background(), "", AttachOptions{}); err == nil {
		t.Fatal("want an error for an empty vm id, got nil")
	}
}

// A frame claiming a gigabyte payload must be refused rather than allocated.
func TestReadFrame_RejectsOversizedPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + AttachProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		// type=stdout, length = 1 GiB, with no payload behind it.
		_, _ = conn.Write([]byte{FrameStdout, 0, 0, 0, 0x40, 0, 0, 0})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "tok")
	stream, err := c.Environments.Attach(context.Background(), "fuse-1", AttachOptions{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.ReadFrame(); err == nil {
		t.Fatal("want an error for an oversized frame, got nil")
	}
}
