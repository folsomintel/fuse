package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// This covers the one hop the api and host-agent tests cannot reach between
// them: the provider talking to a host agent. Everything here runs over a real
// socket against a stand-in host agent.

func TestRemoteEnvExec_ReportsExitCodeAndStreams(t *testing.T) {
	var got execRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vm/vm-1/exec" {
			t.Errorf("path = %q, want /v1/vm/vm-1/exec", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer host-token" {
			t.Errorf("authorization = %q, want the host token", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// The host agent base64s stdout/stderr on the wire; encoding/json
		// decodes a base64 string straight into []byte.
		_, _ = io.WriteString(w, `{"exit_code":3,"stdout":"b3V0","stderr":"ZXJy"}`)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "host-token"})
	env := &remoteEnv{id: "vm-1", client: p}

	res, err := env.Exec(context.Background(), []string{"sh", "-lc", "exit 3"},
		orchestrator.ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("exec returned an error for a non-zero guest exit: %v", err)
	}

	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "out" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out")
	}
	if string(res.Stderr) != "err" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err")
	}

	if len(got.Cmd) != 3 || got.Cmd[0] != "sh" {
		t.Errorf("cmd = %q, want [sh -lc exit 3]", got.Cmd)
	}
	if got.TimeoutMS != 5000 {
		t.Errorf("timeout_ms = %d, want 5000", got.TimeoutMS)
	}
}

func TestRemoteEnvAttach_UpgradesAndRelays(t *testing.T) {
	gotQuery := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vm/vm-1/attach" {
			t.Errorf("path = %q, want /v1/vm/vm-1/attach", r.URL.Path)
		}
		if up := r.Header.Get("Upgrade"); up != hostwire.AttachProto {
			t.Errorf("upgrade = %q, want %q", up, hostwire.AttachProto)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer host-token" {
			t.Errorf("authorization = %q, want the host token", auth)
		}
		gotQuery <- r.URL.RawQuery

		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
			hostwire.AttachProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()

		// Echo whatever arrives, so the test can prove bytes flow both ways.
		_, _ = io.Copy(conn, buf.Reader)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "host-token"})
	env := &remoteEnv{id: "vm-1", client: p}

	stream, err := env.Attach(context.Background(), orchestrator.AttachSpec{
		Cmd:  []string{"sh", "-c", "echo hi there"},
		TTY:  true,
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Repeated cmd params are what keep "echo hi there" a single argv element.
	want := "cmd=sh&cmd=-c&cmd=echo+hi+there&cols=80&rows=24&tty=1"
	if q := <-gotQuery; q != want {
		t.Errorf("query = %q, want %q", q, want)
	}

	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want ping", got)
	}
}

// A host agent that refuses the upgrade must produce an error, not a stream the
// caller would then read garbage from.
func TestRemoteEnvAttach_RefusedUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "vm not found")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "host-token"})
	env := &remoteEnv{id: "vm-1", client: p}

	if _, err := env.Attach(context.Background(), orchestrator.AttachSpec{TTY: true}); err == nil {
		t.Fatal("attach: want an error when the host agent refuses, got nil")
	}
}

// The stub is what a provider degrades to when BaseURL is unset. It must say so
// rather than answer every command with a fabricated success.
func TestStubEnvExec_ReportsUnsupported(t *testing.T) {
	p := New(Config{}) // no BaseURL -> stub
	env, err := p.Create(context.Background(), orchestrator.Spec{Name: "vm-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = env.Exec(context.Background(), []string{"true"}, orchestrator.ExecOptions{})
	if err == nil {
		t.Fatal("stub exec: want ErrExecUnsupported, got a success")
	}

	// And it must not pretend to be attachable either.
	if _, ok := env.(orchestrator.Attacher); ok {
		t.Error("stub env implements Attacher; it has no guest to attach to")
	}
}
