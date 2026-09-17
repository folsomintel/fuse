package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandler_HealthIsUnauthenticated(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "secret-token", []byte("{}"), 0, false, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" || body["vm_id"] != "fuse-1" {
		t.Fatalf("/health body = %+v", body)
	}
}

func TestHandler_InfoRequiresToken(t *testing.T) {
	manifest := []byte(`{"version":"1"}`)
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "secret-token", manifest, 3, false, nil))
	defer srv.Close()

	// No token -> 401.
	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/info (no token) = %d, want 401", resp.StatusCode)
	}

	// Correct token -> 200 with the booted-with details.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/info", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/info (auth): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/v1/info (auth) = %d, want 200", resp2.StatusCode)
	}
	var info map[string]any
	json.NewDecoder(resp2.Body).Decode(&info)
	if info["secret_count"].(float64) != 3 || info["manifest_bytes"].(float64) != float64(len(manifest)) {
		t.Fatalf("/v1/info body = %+v", info)
	}
}

// TestHandler_InfoReportsEgress verifies /v1/info surfaces the egress policy
// document the orchestrator uploads, and omits the field entirely when there
// is no such file, so a caller can tell "direct" from "predates egress".
func TestHandler_InfoReportsEgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "egress.json")
	if err := os.WriteFile(path, []byte(`{"mode":"proxy","provider":"mock","endpoint":"socks5h://10.200.3.1:1080"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1", egress: path}, "", nil, 0, false, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info: %v", err)
	}
	defer resp.Body.Close()
	var info map[string]any
	json.NewDecoder(resp.Body).Decode(&info)
	eg, ok := info["egress"].(map[string]any)
	if !ok || eg["mode"] != "proxy" || eg["endpoint"] != "socks5h://10.200.3.1:1080" {
		t.Fatalf("/v1/info egress = %+v, want the uploaded document", info["egress"])
	}

	// no file: no field, not a default.
	srv2 := httptest.NewServer(newHandler(config{vmID: "fuse-1", egress: filepath.Join(dir, "missing.json")}, "", nil, 0, false, nil))
	defer srv2.Close()
	resp2, err := http.Get(srv2.URL + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info: %v", err)
	}
	defer resp2.Body.Close()
	var info2 map[string]any
	json.NewDecoder(resp2.Body).Decode(&info2)
	if _, present := info2["egress"]; present {
		t.Fatalf("/v1/info reported egress %+v with no policy file", info2["egress"])
	}
}

// TestHandler_InfoOpenWhenNoToken verifies that with no auth token configured
// (dev/insecure), /v1/info is reachable without a header.
func TestHandler_InfoOpenWhenNoToken(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "", nil, 0, false, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/info (no auth configured) = %d, want 200", resp.StatusCode)
	}
}
