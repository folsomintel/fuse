package firecracker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestClientContract validates HTTP paths, auth header, and basic lifecycle calls
// against a fake host agent.
func TestClientContract(t *testing.T) {
	t.Helper()

	var captured []string

	h := http.NewServeMux()

	h.HandleFunc("/v1/vm", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"vm_id":"vm-1","url":""}`))
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"vms":[{"vm_id":"vm-1","url":""}]}`))
			return
		}
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	})

	h.HandleFunc("/v1/vm/vm-1/upload", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})

	h.HandleFunc("/v1/vm/vm-1/exec", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		resp := execResponse{ExitCode: 0, Stdout: []byte("ok"), Stderr: nil}
		_ = json.NewEncoder(w).Encode(resp)
	})

	h.HandleFunc("/v1/vm/vm-1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshot_id":"snap-1"}`))
	})

	h.HandleFunc("/v1/vm/vm-1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshots":[{"snapshot_id":"snap-1","comment":"c","created_at":"2026-01-01T00:00:00Z"}]}`))
	})

	h.HandleFunc("/v1/vm/vm-1/restore", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})

	h.HandleFunc("/v1/vm/vm-1", func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(h)
	defer ts.Close()

	client := New(Config{BaseURL: ts.URL, Token: "token"})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env, err := client.Create(ctx, orchestrator.Spec{Name: "vm-1", CPUs: 1, RamMB: 256, StorageGB: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := client.List(ctx, "vm-1"); err != nil {
		t.Fatalf("list: %v", err)
	}

	if err := env.Upload(ctx, []byte("data"), "/tmp/file"); err != nil {
		t.Fatalf("upload: %v", err)
	}

	res, err := env.Exec(ctx, []string{"echo", "ok"}, orchestrator.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "ok") {
		t.Fatalf("unexpected exec stdout: %q", res.Stdout)
	}

	sc, ok := env.(orchestrator.SnapshotCapable)
	if !ok {
		t.Fatalf("env does not implement SnapshotCapable")
	}
	snap, err := sc.Checkpoint(ctx, "c")
	if err != nil || snap == "" {
		t.Fatalf("snapshot: %v snap=%s", err, snap)
	}

	if _, err := sc.ListCheckpoints(ctx); err != nil {
		t.Fatalf("list snapshots: %v", err)
	}

	if err := sc.Restore(ctx, snap); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if err := client.Destroy(ctx, env.Name()); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	// Ensure auth header was sent on create path at minimum.
	foundAuth := false
	for _, c := range captured {
		if strings.HasPrefix(c, "POST /v1/vm") {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Fatalf("did not capture expected create call, got %+v", captured)
	}

	// Validate that base64 decoding works via Exec response.
	if got := base64.StdEncoding.EncodeToString([]byte("ok")); got == string(res.Stdout) {
		t.Fatalf("exec output appears base64; expected decoded bytes")
	}
}

// TestCapacityQueriesHostAgent asserts Capacity() calls GET /v1/capacity and
// maps the response onto orchestrator.HostCapacity.
func TestCapacityQueriesHostAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capacity" || r.Method != http.MethodGet {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(capacityResponse{CPUs: 8, RamMB: 16384, StorageGB: 100})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	got, err := p.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	want := orchestrator.HostCapacity{CPUs: 8, RamMB: 16384, StorageGB: 100}
	if got != want {
		t.Errorf("capacity = %+v, want %+v", got, want)
	}
}

// TestCapacityStubModeErrors asserts the in-memory stub (no real host agent)
// refuses to probe rather than fabricating hardware numbers.
func TestCapacityStubModeErrors(t *testing.T) {
	p := New(Config{}) // empty BaseURL -> stub
	if _, err := p.Capacity(context.Background()); err == nil {
		t.Fatal("expected error probing capacity in stub mode, got nil")
	}
}
