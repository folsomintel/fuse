package qemu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestQEMUEnv_notSnapshotCapable is the task 3.1 guardrail: the qemu
// environment must satisfy orchestrator.Environment but must NOT implement
// orchestrator.SnapshotCapable, so the orchestrator's snapshot/fork type
// assertions reject GPU envs (D4). Covers both the stub and remote envs.
func TestQEMUEnv_notSnapshotCapable(t *testing.T) {
	p := New(Config{}) // empty BaseURL -> stub
	env, err := p.Create(context.Background(), orchestrator.Spec{Name: "fuse-t1", GPUs: 1, GPUKind: "a100"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, ok := any(env).(orchestrator.Environment); !ok {
		t.Fatalf("qemu env does not implement orchestrator.Environment")
	}
	if _, ok := any(env).(orchestrator.SnapshotCapable); ok {
		t.Fatalf("qemu env must NOT implement orchestrator.SnapshotCapable")
	}
}

// TestQEMURemoteEnv_notSnapshotCapable asserts the remote (HTTP-backed) env is
// also not snapshot-capable, independent of the stub.
func TestQEMURemoteEnv_notSnapshotCapable(t *testing.T) {
	var env orchestrator.Environment = &remoteEnv{id: "fuse-t1", url: "qemu://fuse-t1"}
	if _, ok := env.(orchestrator.SnapshotCapable); ok {
		t.Fatalf("remoteEnv must NOT implement orchestrator.SnapshotCapable")
	}
}

// TestProviderImplementsInterface asserts New returns an orchestrator.Provider.
func TestProviderImplementsInterface(t *testing.T) {
	var _ orchestrator.Provider = New(Config{})
}

// TestCreateForwardsGPUSpec is the task 3.2 assertion: a create with GPUs > 0
// sends gpus and gpu_kind to the host agent.
func TestCreateForwardsGPUSpec(t *testing.T) {
	var got createVMRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vm" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(createVMResponse{VMID: "fuse-t1", URL: "https://guest.local"})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	if _, err := p.Create(context.Background(), orchestrator.Spec{Name: "fuse-t1", GPUs: 2, GPUKind: "a100"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got.GPUs != 2 {
		t.Errorf("create payload gpus = %d, want 2", got.GPUs)
	}
	if got.GPUKind != "a100" {
		t.Errorf("create payload gpu_kind = %q, want a100", got.GPUKind)
	}
}

// TestCreateOmitsGPUWhenZero is the task 3.2 negative case: GPUs == 0 must not
// emit gpus/gpu_kind on the wire (omitempty).
func TestCreateOmitsGPUWhenZero(t *testing.T) {
	var rawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(createVMResponse{VMID: "fuse-t1", URL: "https://guest.local"})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	if _, err := p.Create(context.Background(), orchestrator.Spec{Name: "fuse-t1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, present := rawBody["gpus"]; present {
		t.Errorf("create payload should omit gpus when zero, got %v", rawBody["gpus"])
	}
	if _, present := rawBody["gpu_kind"]; present {
		t.Errorf("create payload should omit gpu_kind when empty, got %v", rawBody["gpu_kind"])
	}
}

// TestStubRecordsGPUSpec verifies the stub path (used by the PR-1 factory
// placeholder and dev) captures the requested GPU spec for inspection.
func TestStubCreateAndGet(t *testing.T) {
	p := New(Config{UseStub: true})
	ctx := context.Background()
	if _, err := p.Create(ctx, orchestrator.Spec{Name: "fuse-t1", GPUs: 1, GPUKind: "a100"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	env, err := p.Get(ctx, "fuse-t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if env.Name() != "fuse-t1" {
		t.Errorf("name = %q, want fuse-t1", env.Name())
	}
	se, ok := env.(*stubEnv)
	if !ok {
		t.Fatalf("expected *stubEnv, got %T", env)
	}
	if se.gpus != 1 || se.gpuKind != "a100" {
		t.Errorf("stub captured gpus=%d kind=%q, want 1/a100", se.gpus, se.gpuKind)
	}
}
