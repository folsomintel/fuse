package qemu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/folsomintel/fuse/internal/egress"
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
	// Same guardrail, one interface further out: a live snapshot is a strictly
	// larger ask than a disk one, so an env that cannot be checkpointed at all
	// must not answer LiveSnapshotCapable either. If it did, CreateSnapshot's
	// live branch would reach a gpu env that the SnapshotCapable assertion is
	// there to keep out.
	if _, ok := any(env).(orchestrator.LiveSnapshotCapable); ok {
		t.Fatalf("qemu env must NOT implement orchestrator.LiveSnapshotCapable")
	}
}

// TestQEMURemoteEnv_notSnapshotCapable asserts the remote (HTTP-backed) env is
// also not snapshot-capable, independent of the stub.
func TestQEMURemoteEnv_notSnapshotCapable(t *testing.T) {
	var env orchestrator.Environment = &remoteEnv{id: "fuse-t1", url: "qemu://fuse-t1"}
	if _, ok := env.(orchestrator.SnapshotCapable); ok {
		t.Fatalf("remoteEnv must NOT implement orchestrator.SnapshotCapable")
	}
	if _, ok := env.(orchestrator.LiveSnapshotCapable); ok {
		t.Fatalf("remoteEnv must NOT implement orchestrator.LiveSnapshotCapable")
	}
}

// TestProviderImplementsInterface asserts New returns an orchestrator.Provider.
// TestQEMUProvider_notEgressHostAgent is the egress guardrail: the qemu agent
// refuses proxy mode outright, so the provider must not answer
// egress.HostAgent. the orchestrator asserts on that interface to decide
// whether a host-side backend can be driven; a qemu provider that implemented
// it would let a proxy-mode create reach a host that cannot enforce it.
func TestQEMUProvider_notEgressHostAgent(t *testing.T) {
	p := New(Config{})
	if _, ok := any(p).(egress.HostAgent); ok {
		t.Fatalf("qemu provider must NOT implement egress.HostAgent")
	}
}

// TestCreateOmitsEgressForDirect pins that a direct spec sends byte-for-byte
// the create request an older agent expects, and a proxy spec carries the
// mode so the agent can refuse it explicitly.
func TestCreateOmitsEgressForDirect(t *testing.T) {
	if w := egressWireFor(orchestrator.Spec{}); w != nil {
		t.Fatalf("direct spec produced egress wire %+v, want nil", w)
	}
	w := egressWireFor(orchestrator.Spec{Egress: egress.Spec{Mode: egress.ModeProxy, Provider: "mock"}})
	if w == nil || w.Mode != "proxy" {
		t.Fatalf("proxy spec produced egress wire %+v, want mode proxy", w)
	}
}

func TestProviderImplementsInterface(t *testing.T) {
	var _ orchestrator.Provider = New(Config{})
}

// TestCapacityQueriesHostAgent asserts Capacity() calls GET /v1/capacity and
// maps the response onto orchestrator.HostCapacity.
func TestCapacityQueriesHostAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capacity" || r.Method != http.MethodGet {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(capacityResponse{CPUs: 16, RamMB: 65536, StorageGB: 500})
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	got, err := p.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	want := orchestrator.HostCapacity{CPUs: 16, RamMB: 65536, StorageGB: 500}
	if !reflect.DeepEqual(got, want) {
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

// TestCreateForwardsGPUProfile asserts a fractional MIG request is forwarded
// to the host agent as gpu_profile (decision D5).
func TestCreateForwardsGPUProfile(t *testing.T) {
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
	if _, err := p.Create(context.Background(), orchestrator.Spec{
		Name: "fuse-t1", GPUs: 2, GPUKind: "a100", GPUProfile: "1g.10gb",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got.GPUs != 2 {
		t.Errorf("create payload gpus = %d, want 2", got.GPUs)
	}
	if got.GPUProfile != "1g.10gb" {
		t.Errorf("create payload gpu_profile = %q, want 1g.10gb", got.GPUProfile)
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

func TestStartAgentForwardsExposeAndReportsEndpoints(t *testing.T) {
	var got startAgentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vm/fuse-t1/start-agent" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(startAgentResponse{Endpoints: []endpointWire{{As: "web", URL: "gpu.test:20000", Port: 8080}}})
	}))
	defer srv.Close()

	provider := New(Config{BaseURL: srv.URL})
	env := &remoteEnv{id: "fuse-t1", client: provider}
	if err := env.StartAgent(context.Background(), orchestrator.AgentSpec{
		Expose: []orchestrator.ExposeSpec{{Port: 8080, As: "web"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(got.Expose) != 1 || got.Expose[0].Port != 8080 || got.Expose[0].As != "web" {
		t.Fatalf("expose payload = %#v", got.Expose)
	}
	endpoints := env.Endpoints()
	if len(endpoints) != 1 || endpoints[0].URL != "gpu.test:20000" {
		t.Fatalf("endpoints = %#v", endpoints)
	}
}

// TestStartAgentSendsProtocol covers both directions of the protocol field:
// what the orchestrator sends to the agent, and what it makes of the agent's
// answer. An unset protocol is normalized to tcp on the way out, so the agent
// never has to derive the default, and an empty protocol in the reply reads
// back as tcp, which is what an agent predating the field always published.
func TestStartAgentSendsProtocol(t *testing.T) {
	cases := []struct {
		name         string
		spec         orchestrator.ExposeSpec
		reply        string
		wantSent     string
		wantReported orchestrator.Protocol
	}{
		{
			name:         "udp is sent and reported",
			spec:         orchestrator.ExposeSpec{Port: 5353, As: "dns", Protocol: orchestrator.ProtocolUDP},
			reply:        "udp",
			wantSent:     "udp",
			wantReported: orchestrator.ProtocolUDP,
		},
		{
			name:         "an unset protocol is sent as tcp",
			spec:         orchestrator.ExposeSpec{Port: 8080, As: "web"},
			reply:        "tcp",
			wantSent:     "tcp",
			wantReported: orchestrator.ProtocolTCP,
		},
		{
			name:         "an agent that omits the protocol reports tcp",
			spec:         orchestrator.ExposeSpec{Port: 8080, As: "web", Protocol: orchestrator.ProtocolTCP},
			reply:        "",
			wantSent:     "tcp",
			wantReported: orchestrator.ProtocolTCP,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got startAgentRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/vm/fuse-t1/start-agent" {
					http.NotFound(w, r)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				_ = json.NewEncoder(w).Encode(startAgentResponse{Endpoints: []endpointWire{{
					As: tc.spec.As, URL: "gpu.test:20000", Port: tc.spec.Port, Protocol: tc.reply,
				}}})
			}))
			defer srv.Close()

			provider := New(Config{BaseURL: srv.URL})
			env := &remoteEnv{id: "fuse-t1", client: provider}
			if err := env.StartAgent(context.Background(), orchestrator.AgentSpec{
				Expose: []orchestrator.ExposeSpec{tc.spec},
			}); err != nil {
				t.Fatal(err)
			}
			if len(got.Expose) != 1 || got.Expose[0].Protocol != tc.wantSent {
				t.Fatalf("sent expose = %#v, want protocol %q", got.Expose, tc.wantSent)
			}
			endpoints := env.Endpoints()
			if len(endpoints) != 1 || endpoints[0].Protocol != tc.wantReported {
				t.Fatalf("endpoints = %#v, want protocol %q", endpoints, tc.wantReported)
			}
		})
	}
}
