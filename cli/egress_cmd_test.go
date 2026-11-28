package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// runCLI runs the cli against srv and returns stdout and the error.
func runCLI(t *testing.T, srvURL string, args ...string) (string, error) {
	t.Helper()
	cfg := writeConfig(t, srvURL)
	return capture(t, func() error {
		root := newRootCmd()
		root.SetArgs(append([]string{"--config", cfg}, args...))
		return root.Execute()
	})
}

// egressAllowServer answers the put route by echoing the list back, the way
// the orchestrator does after normalizing it, and records each body.
func egressAllowServer(t *testing.T, bodies *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/environments/vm1/egress/allow" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		var req struct {
			Allow []string `json:"allow"`
		}
		_ = json.Unmarshal(b, &req)
		out, _ := json.Marshal(map[string]any{"mode": "direct", "allow": req.Allow})
		_, _ = w.Write(out)
	}))
}

func TestEnvEgressAllowSendsCIDRs(t *testing.T) {
	var bodies []string
	srv := egressAllowServer(t, &bodies)
	defer srv.Close()

	if _, err := runCLI(t, srv.URL, "environment", "egress", "allow", "vm1", "10.0.0.0/8", "1.2.3.4/32"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := `{"allow":["10.0.0.0/8","1.2.3.4/32"]}`; len(bodies) != 1 || strings.TrimSpace(bodies[0]) != want {
		t.Errorf("bodies = %q, want [%s]", bodies, want)
	}
}

// --clear must still send the key, as [], since the server refuses a body
// without it.
func TestEnvEgressAllowClearSendsEmptyList(t *testing.T) {
	var bodies []string
	srv := egressAllowServer(t, &bodies)
	defer srv.Close()

	if _, err := runCLI(t, srv.URL, "environment", "egress", "allow", "vm1", "--clear"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := `{"allow":[]}`; len(bodies) != 1 || strings.TrimSpace(bodies[0]) != want {
		t.Errorf("bodies = %q, want [%s]", bodies, want)
	}
}

// a bare id must not lift the list: a typo would otherwise open egress.
func TestEnvEgressAllowRefusesAmbiguousInput(t *testing.T) {
	var bodies []string
	srv := egressAllowServer(t, &bodies)
	defer srv.Close()

	for _, args := range [][]string{
		{"environment", "egress", "allow", "vm1"},
		{"environment", "egress", "allow", "vm1", "10.0.0.0/8", "--clear"},
	} {
		if _, err := runCLI(t, srv.URL, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
	if len(bodies) != 0 {
		t.Errorf("sent %q, want no request", bodies)
	}
}

func TestEnvEgressAllowConflictFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"conflict","message":"environment uses proxy egress"}}`)
	}))
	defer srv.Close()

	_, err := runCLI(t, srv.URL, "environment", "egress", "allow", "vm1", "10.0.0.0/8")
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("err = %v, want a conflict error", err)
	}
}

func TestEnvEgressAllowJSON(t *testing.T) {
	var bodies []string
	srv := egressAllowServer(t, &bodies)
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "-o", "json", "environment", "egress", "allow", "vm1", "10.0.0.0/8")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"allow"`) || !strings.Contains(out, "10.0.0.0/8") {
		t.Errorf("json output missing the allow list: %s", out)
	}
}

func TestEnvCreateEgressAllowFlag(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fuse-1","state":"provisioning","task_id":"t1","spec":{}}`)
	}))
	defer srv.Close()

	if _, err := runCLI(t, srv.URL, "-o", "json", "environment", "create", "--task-id", "t1",
		"--egress-allow", "10.0.0.0/8", "--egress-allow", "1.2.3.4"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := map[string]any{"mode": "direct", "allow": []any{"10.0.0.0/8", "1.2.3.4"}}
	if !reflect.DeepEqual(gotBody["egress"], want) {
		t.Errorf("egress = %v, want %v", gotBody["egress"], want)
	}
}

func TestEnvCreateOmitsEgressWithoutAllow(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fuse-1","state":"provisioning","task_id":"t1","spec":{}}`)
	}))
	defer srv.Close()

	if _, err := runCLI(t, srv.URL, "-o", "json", "environment", "create", "--task-id", "t1"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, ok := gotBody["egress"]; ok {
		t.Errorf("egress sent without --egress-allow: %v", gotBody)
	}
}

func TestEnvGetRendersEgressAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"vm1","state":"running","task_id":"t","spec":{},
			"egress":{"mode":"direct","allow":["10.0.0.0/8","1.2.3.4/32"]}}`)
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "environment", "get", "vm1")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "allow 10.0.0.0/8, 1.2.3.4/32") {
		t.Errorf("output missing the allow list:\n%s", out)
	}
}

// the authored allowlist has to reach the create body: a Fusefile field the
// cli drops before create is a restriction nobody enforces.
func TestUpSendsEgressAllow(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "Fusefile")
	src := "version: 1\nrun: ./serve.sh\negress:\n  allow:\n    - 10.0.0.0/8\n    - 1.2.3.4\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, srv.URL, "-o", "json", "up", "-f", path, "--task-id", "t", "--no-wait"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := map[string]any{"mode": "direct", "allow": []any{"10.0.0.0/8", "1.2.3.4"}}
	if !reflect.DeepEqual(gotBody["egress"], want) {
		t.Errorf("egress = %v, want %v", gotBody["egress"], want)
	}
}

func TestCompileEmitsEgressAllow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Fusefile")
	src := "version: 1\nrun: ./serve.sh\negress:\n  allow: [10.0.0.0/8]\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCompile(t, "-f", path, "--format", "json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"allow"`) || !strings.Contains(out, "10.0.0.0/8") {
		t.Errorf("compile output missing egress.allow:\n%s", out)
	}
}
