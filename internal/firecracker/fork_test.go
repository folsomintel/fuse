package firecracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestRemote_createFromCheckpoint pins the fork wire the host agent implements:
// POST /v1/vm/{src}/fork with the snapshot to seed from, and the NEW vm's name.
// Sending the name matters — the host agent invents one when it is absent, and
// the orchestrator later destroys by ITS vm id, so a mismatch would leak the
// microVM forever.
func TestRemote_createFromCheckpoint(t *testing.T) {
	var got forkVMRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vm/src-vm/fork" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode fork request: %v", err)
		}
		json.NewEncoder(w).Encode(createVMResponse{VMID: "fork-1", URL: "http://fork-1.local"})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Token: "test-token"})
	spec := orchestrator.Spec{Name: "fork-1", CPUs: 2, RamMB: 512}
	env, err := p.CreateFromCheckpoint(context.Background(), spec, "src-vm", "snap-7")
	if err != nil {
		t.Fatal(err)
	}

	if got.Name != "fork-1" {
		t.Fatalf("fork request name = %q, want the new vm's name fork-1", got.Name)
	}
	if got.SnapshotID != "snap-7" {
		t.Fatalf("fork request snapshot_id = %q, want snap-7", got.SnapshotID)
	}
	if got.CPUs != 2 || got.MemoryMB != 512 {
		t.Fatalf("fork request sizing = %d cpus / %d MB, want 2 / 512", got.CPUs, got.MemoryMB)
	}

	// the returned handle must address the NEW vm, not the source.
	if env.Name() != "fork-1" {
		t.Fatalf("env name = %q, want fork-1", env.Name())
	}
	if env.URL() != "http://fork-1.local" {
		t.Fatalf("env url = %q, want the fork's url", env.URL())
	}
}

func TestRemote_createFromCheckpoint_hostError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "snapshot not found", http.StatusNotFound)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	_, err := p.CreateFromCheckpoint(context.Background(), orchestrator.Spec{Name: "fork-1"}, "src-vm", "nope")
	if err == nil {
		t.Fatal("expected an error when the host agent rejects the fork")
	}
}

// TestProvider_implementsSnapshotForkable is the assertion ForkEnvironment's
// type switch actually keys off: without it, fork silently reports "provider
// does not support fork" at runtime.
func TestProvider_implementsSnapshotForkable(t *testing.T) {
	var p any = New(Config{BaseURL: "http://host.local"})
	if _, ok := p.(orchestrator.SnapshotForkable); !ok {
		t.Fatal("firecracker Provider must implement orchestrator.SnapshotForkable")
	}
}

func TestStub_createFromCheckpoint(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()

	src, err := p.Create(ctx, orchestrator.Spec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Upload(ctx, []byte("payload"), "/data/file"); err != nil {
		t.Fatal(err)
	}
	cpID, err := src.(orchestrator.SnapshotCapable).Checkpoint(ctx, "seed")
	if err != nil {
		t.Fatal(err)
	}

	forked, err := p.CreateFromCheckpoint(ctx, orchestrator.Spec{Name: "fork"}, "src", cpID)
	if err != nil {
		t.Fatal(err)
	}
	if forked.Name() != "fork" {
		t.Fatalf("forked env name = %q, want fork", forked.Name())
	}

	// the fork carries the source's disk state, and the source is untouched.
	stub := forked.(*stubEnv)
	if string(stub.files["/data/file"]) != "payload" {
		t.Fatalf("fork did not inherit the source's files: %v", stub.files)
	}
	if _, err := p.Get(ctx, "src"); err != nil {
		t.Fatalf("source env should still exist after fork: %v", err)
	}

	// writes to the fork must not bleed back into the source.
	if err := forked.Upload(ctx, []byte("fork-only"), "/data/file"); err != nil {
		t.Fatal(err)
	}
	srcStub := src.(*stubEnv)
	if string(srcStub.files["/data/file"]) != "payload" {
		t.Fatal("writing to the fork mutated the source's files")
	}
}

func TestStub_createFromCheckpoint_unknownSource(t *testing.T) {
	p := New(Config{UseStub: true})
	if _, err := p.CreateFromCheckpoint(context.Background(), orchestrator.Spec{Name: "fork"}, "ghost", "cp-1"); err == nil {
		t.Fatal("expected an error forking from a source that does not exist")
	}
}

func TestStub_createFromCheckpoint_unknownCheckpoint(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()
	if _, err := p.Create(ctx, orchestrator.Spec{Name: "src"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateFromCheckpoint(ctx, orchestrator.Spec{Name: "fork"}, "src", "cp-missing"); err == nil {
		t.Fatal("expected an error forking from a checkpoint the source does not have")
	}
}
