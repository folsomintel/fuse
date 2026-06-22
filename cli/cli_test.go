package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fuse "github.com/andrewn6/fuse/sdks/go"
)

func TestParseKeyVals(t *testing.T) {
	m, err := parseKeyVals([]string{"a=1", "b=x=y"})
	if err != nil {
		t.Fatal(err)
	}
	if m["a"] != "1" || m["b"] != "x=y" {
		t.Errorf("got %v", m)
	}
	if _, err := parseKeyVals([]string{"bad"}); err == nil {
		t.Error("expected error for missing =")
	}
	if m, err := parseKeyVals(nil); err != nil || m != nil {
		t.Errorf("nil input should be nil map, got %v %v", m, err)
	}
}

func TestMaybeFile(t *testing.T) {
	if got, _ := maybeFile("plain"); got != "plain" {
		t.Errorf("plain string changed: %q", got)
	}
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := maybeFile("@" + p)
	if err != nil || got != "hello" {
		t.Errorf("file read = %q, %v", got, err)
	}
	if _, err := maybeFile("@/no/such/file"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "-", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB", 1048576: "1.0 MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFriendlyError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&fuse.APIError{Status: 404, Code: "not_found", Message: "host gone"}, "not found"},
		{&fuse.APIError{Status: 403, Code: "forbidden", Message: "nope"}, "forbidden"},
		{&fuse.APIError{Status: 401, Code: "unauthorized", Message: "bad token"}, "unauthorized"},
		{&fuse.APIError{Status: 409, Code: "conflict", Message: "vm not running"}, "conflict"},
		{&fuse.APIError{Status: 400, Code: "invalid_argument", Message: "task_id required"}, "invalid argument"},
		{fmt.Errorf("plain error"), "plain error"},
	}
	for _, c := range cases {
		got := friendly(c.err)
		if got == nil || !strings.Contains(got.Error(), c.want) {
			t.Errorf("friendly(%v) = %v, want contains %q", c.err, got, c.want)
		}
	}
	if friendly(nil) != nil {
		t.Error("friendly(nil) should be nil")
	}
}

// writeConfig writes a minimal config pointing at baseURL and returns its path.
func writeConfig(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("current_context: t\ncontexts:\n  - name: t\n    base_url: %s\n    token: test-token\n", baseURL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := fn()
	_ = w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data), err
}

func TestHostsListJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hosts" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want Bearer test-token", got)
		}
		fmt.Fprint(w, `{"hosts":[{"id":"h1","url":"http://h1","state":"active","capacity":{"cpus":4,"ram_mb":8192,"storage_gb":100,"vm_count":10},"allocated":{"cpus":1,"ram_mb":1024,"storage_gb":10,"vm_count":2}}]}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", cfg, "-o", "json", "hosts", "list"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"h1"`) || !strings.Contains(out, `"active"`) {
		t.Errorf("output missing host fields: %s", out)
	}
}

func TestHostGetNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"not_found","message":"host missing"}}`)
	}))
	defer srv.Close()

	cfg := writeConfig(t, srv.URL)
	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "host", "get", "missing"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestEnvListScopedToActiveHost(t *testing.T) {
	var gotHostQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHostQuery = r.URL.Query().Get("host_id")
		fmt.Fprint(w, `{"environments":[]}`)
	}))
	defer srv.Close()

	// config with an active host set; env list should default to it.
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("current_context: t\ncontexts:\n  - name: t\n    base_url: %s\n    token: test-token\n    active_host: prod-1\n", srv.URL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", path, "-o", "json", "environment", "list"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotHostQuery != "prod-1" {
		t.Errorf("host_id query = %q, want prod-1", gotHostQuery)
	}
}

func TestConnectAndContextCurrent(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	root := newRootCmd()
	root.SetArgs([]string{"--config", cfg, "connect", "http://localhost:9999", "--token", "abc", "--name", "dev"})
	if err := root.Execute(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	out, err := capture(t, func() error {
		r := newRootCmd()
		r.SetArgs([]string{"--config", cfg, "-o", "json", "context", "current"})
		return r.Execute()
	})
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if !strings.Contains(out, `"dev"`) || strings.Contains(out, "abc") {
		t.Errorf("context current json wrong (token must not leak): %s", out)
	}
}
