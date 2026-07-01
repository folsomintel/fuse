package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFusefile writes a minimal valid Fusefile with a required secret (via
// both a service env reference and the top-level secrets list) to dir and
// returns its path.
func writeFusefile(t *testing.T, dir string) string {
	t.Helper()
	src := `version: 1
resources:
  memory: 2GB
services:
  db:
    image: postgres:16
    env:
      PGPASSWORD:
        secret: pg_password
run: ./start.sh
secrets:
  - pg_password
`
	path := filepath.Join(dir, "Fusefile")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpCreatesEnvironmentFromFusefile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want Bearer test-token", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secret", "pg_password=shh",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"vm1"`) {
		t.Errorf("output missing environment id: %s", out)
	}

	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	spec, ok := gotBody["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type: %v", gotBody["spec"])
	}
	if ramMB, _ := spec["ram_mb"].(float64); ramMB != 2048 {
		t.Errorf("spec.ram_mb = %v, want 2048", spec["ram_mb"])
	}

	manifestInline, _ := gotBody["manifest_inline"].(string)
	manifestJSON, err := base64.StdEncoding.DecodeString(manifestInline)
	if err != nil {
		t.Fatalf("manifest_inline is not valid base64: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		t.Fatalf("manifest_inline did not decode to json: %v", err)
	}
	if manifest["version"] != "1" {
		t.Errorf("manifest version = %v, want %q", manifest["version"], "1")
	}
	machine, ok := manifest["machine"].(map[string]any)
	if !ok || machine["workspace"] != "/workspace" {
		t.Errorf("manifest missing machine.workspace: %v", manifest["machine"])
	}

	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "shh" {
		t.Errorf("secrets.pg_password = %v, want shh", secrets["pg_password"])
	}
}

func TestUpMissingRequiredSecretFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not have been called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	fusefilePath := writeFusefile(t, t.TempDir())
	cfg := writeConfig(t, srv.URL)

	root := newRootCmd()
	root.SetArgs([]string{
		"--config", cfg,
		"up", "-f", fusefilePath,
		"--task-id", "t",
		"--no-wait",
	})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing required secrets") {
		t.Fatalf("want missing required secrets error, got %v", err)
	}
}

func TestUpSecretsFile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	// Write secrets file with comment line, blank line, and real entry.
	tmpdir := t.TempDir()
	secretsPath := filepath.Join(tmpdir, "secrets.txt")
	secretsContent := "# database password\n\npg_password=fromfile\n"
	if err := os.WriteFile(secretsPath, []byte(secretsContent), 0o600); err != nil {
		t.Fatal(err)
	}

	fusefilePath := writeFusefile(t, tmpdir)
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secrets-file", secretsPath,
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"vm1"`) {
		t.Errorf("output missing environment id: %s", out)
	}

	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "fromfile" {
		t.Errorf("secrets.pg_password = %v, want fromfile", secrets["pg_password"])
	}
}

func TestUpSecretFlagOverridesFile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		fmt.Fprint(w, `{"id":"vm1","state":"pending","task_id":"t","url":"","spec":{}}`)
	}))
	defer srv.Close()

	// Write secrets file with pg_password=fromfile.
	tmpdir := t.TempDir()
	secretsPath := filepath.Join(tmpdir, "secrets.txt")
	if err := os.WriteFile(secretsPath, []byte("pg_password=fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fusefilePath := writeFusefile(t, tmpdir)
	cfg := writeConfig(t, srv.URL)

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{
			"--config", cfg, "-o", "json",
			"up", "-f", fusefilePath,
			"--task-id", "t",
			"--secrets-file", secretsPath,
			"--secret", "pg_password=fromflag",
			"--no-wait",
		})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"vm1"`) {
		t.Errorf("output missing environment id: %s", out)
	}

	if gotBody == nil {
		t.Fatalf("server was never called")
	}

	secrets, ok := gotBody["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets missing or wrong type: %v", gotBody["secrets"])
	}
	if secrets["pg_password"] != "fromflag" {
		t.Errorf("secrets.pg_password = %v, want fromflag (flag should override file)", secrets["pg_password"])
	}
}
