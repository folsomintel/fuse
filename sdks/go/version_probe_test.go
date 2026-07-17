package fuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersionIdentifiesOrchestrator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			t.Errorf("path = %q, want /v1/version", r.URL.Path)
		}
		w.Header().Set("Server", "fuse-orchestrator/0.4.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"service":"fuse-orchestrator","version":"0.4.0"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !info.IsOrchestrator() {
		t.Errorf("IsOrchestrator() = false, want true (service=%q)", info.Service)
	}
	if info.Version != "0.4.0" {
		t.Errorf("Version = %q, want 0.4.0", info.Version)
	}
	if info.ServerHeader != "fuse-orchestrator/0.4.0" {
		t.Errorf("ServerHeader = %q", info.ServerHeader)
	}
}

func TestVersionNonOrchestratorIsNotMistaken(t *testing.T) {
	// A host agent (or any other service) answers /v1/version differently.
	// The probe must not report it as an orchestrator, and it should still
	// surface the Server header so the caller can name what it reached.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "fc-agent/0.1")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version returned transport error for a reachable server: %v", err)
	}
	if info.IsOrchestrator() {
		t.Error("IsOrchestrator() = true for an fc-agent response, want false")
	}
	if info.ServerHeader != "fc-agent/0.1" {
		t.Errorf("ServerHeader = %q, want fc-agent/0.1", info.ServerHeader)
	}
}

func TestVersionUnreachableReturnsError(t *testing.T) {
	// A dead endpoint must surface a transport error (not nil-info,
	// nil-error) so `connect` can report "no orchestrator ... is it running?".
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // close immediately so the address refuses connections

	c, err := New(url, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err == nil {
		t.Fatal("Version against a closed server returned nil error, want a transport error")
	}
}
