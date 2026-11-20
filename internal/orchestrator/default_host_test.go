package orchestrator

import (
	"context"
	"errors"
	"testing"
)

// probingProvider is a mockProvider whose agent reports capacity, as the
// firecracker provider does for a real host agent.
type probingProvider struct {
	*mockProvider
	capacity HostCapacity
	err      error
}

func (p *probingProvider) Capacity(context.Context) (HostCapacity, error) {
	return p.capacity, p.err
}

func newDefaultHostFleet(p Provider, url string) *FleetManager {
	return newInlineFleet(FleetConfig{
		Provider:            p,
		Prefix:              "fuse-",
		DefaultHostURL:      url,
		DefaultHostToken:    "agent-token",
		HostProviderFactory: func(string, string, HostBackend) Provider { return p },
	})
}

// a create on an orchestrator that was pointed at one agent but never given a
// host registers that agent and lands on it, charged like any other host.
func TestCreate_registersDefaultAgentWhenNoHosts(t *testing.T) {
	ctx := context.Background()
	p := &probingProvider{mockProvider: newMockProvider(),
		capacity: HostCapacity{CPUs: 2, RamMB: 4096, StorageGB: 50, Arch: "x86_64", GPUs: 1}}
	fm := newDefaultHostFleet(p, "http://127.0.0.1:8090")

	info, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{CPUs: 1, RamMB: 512, StorageGB: 4}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.HostID != DefaultHostID {
		t.Fatalf("vm landed on %q, want %q", info.HostID, DefaultHostID)
	}
	h, ok := fm.GetHost(DefaultHostID)
	if !ok {
		t.Fatal("default agent was not registered")
	}
	if h.URL != "http://127.0.0.1:8090" || h.Backend != BackendFirecracker {
		t.Errorf("host = %s %s, want the default agent on firecracker", h.URL, h.Backend)
	}
	if h.Capacity.VMCount != 8 || h.Capacity.GPUs != 0 || h.Capacity.Arch != "amd64" {
		t.Errorf("capacity = %+v, want vm_count 8 (4 per cpu), no gpus, arch amd64", h.Capacity)
	}
	if h.Allocated.CPUs != 1 || h.Allocated.VMCount != 1 {
		t.Errorf("allocated = %+v, want the create charged", h.Allocated)
	}
}

// with the default agent registered, a create that does not fit is refused
// with ErrNoCapacity instead of booting past the agent's capacity.
func TestCreate_defaultAgentRefusesWhenFull(t *testing.T) {
	ctx := context.Background()
	p := &probingProvider{mockProvider: newMockProvider(),
		capacity: HostCapacity{CPUs: 2, RamMB: 4096, StorageGB: 50}}
	fm := newDefaultHostFleet(p, "http://127.0.0.1:8090")

	spec := Spec{CPUs: 2, RamMB: 1024, StorageGB: 4}
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", spec, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := fm.ProvisionAndAssign(ctx, "task-2", spec, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("second create err = %v, want ErrNoCapacity", err)
	}
	if n := len(fm.ListHosts()); n != 1 {
		t.Errorf("%d hosts registered, want only the default agent", n)
	}
}

// an agent that cannot report its capacity is not registered, and the create
// is refused as unavailable rather than booted unchecked.
func TestCreate_defaultAgentProbeFailureRefuses(t *testing.T) {
	ctx := context.Background()
	p := &probingProvider{mockProvider: newMockProvider(), err: errors.New("connection refused")}
	fm := newDefaultHostFleet(p, "http://127.0.0.1:8090")

	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{CPUs: 1, RamMB: 512}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrNoHosts) {
		t.Fatalf("create err = %v, want ErrNoHosts", err)
	}
	if n := len(fm.ListHosts()); n != 0 {
		t.Errorf("%d hosts registered, want none", n)
	}
}

// without a default agent url (the in-memory stub) nothing is registered and
// the create takes the single-provider path, as before.
func TestCreate_stubKeepsSingleProviderPath(t *testing.T) {
	ctx := context.Background()
	p := &probingProvider{mockProvider: newMockProvider(),
		capacity: HostCapacity{CPUs: 2, RamMB: 4096, StorageGB: 50}}
	fm := newDefaultHostFleet(p, "")

	info, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{CPUs: 1, RamMB: 512}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.HostID != "" {
		t.Errorf("vm landed on %q, want the single-provider path", info.HostID)
	}
	if n := len(fm.ListHosts()); n != 0 {
		t.Errorf("%d hosts registered, want none", n)
	}
}
