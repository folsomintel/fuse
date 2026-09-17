package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
)

// recordingEgress is an egress.Provider that only counts. it pins that a
// direct-mode boot makes no call into the registry beyond IsProxy.
type recordingEgress struct {
	mu          sync.Mutex
	provisioned int
	destroyed   int
}

func (r *recordingEgress) Name() string { return "recording" }
func (r *recordingEgress) Provision(_ context.Context, _ egress.Context) (egress.Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.provisioned++
	return egress.Endpoint{URL: "socks5h://127.0.0.1:1"}, nil
}
func (r *recordingEgress) Healthcheck(_ context.Context, _ egress.Endpoint) error { return nil }
func (r *recordingEgress) Destroy(_ context.Context, _ egress.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.destroyed++
	return nil
}

func newEgressMock() *egress.Mock {
	m := egress.NewMock()
	m.AllowLoopback = true
	return m
}

var egressManifest = []byte(`{"version":"1","services":{}}`)

// waitForEgress polls cond for up to two seconds. the fleet's cleanup
// destroy runs on a detached goroutine, so a test asserting "no vm
// survives" has to give it a moment.
func waitForEgress(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEgressDirectBootMakesNoRegistryCalls(t *testing.T) {
	rec := &recordingEgress{}
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", EgressRegistry: egress.NewRegistry(rec)})
	ctx := context.Background()

	info, err := fm.ProvisionAndAssign(ctx, "t-direct", Spec{CPUs: 1, RamMB: 256}, egressManifest, nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Egress.Mode != egress.ModeDirect {
		t.Fatalf("direct vm reported egress %+v", info.Egress)
	}
	if err := fm.DestroyVM(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if rec.provisioned != 0 || rec.destroyed != 0 {
		t.Fatalf("direct boot touched the registry: provisioned=%d destroyed=%d", rec.provisioned, rec.destroyed)
	}
}

func TestEgressProxyBootProvisionsAndDestroyReleases(t *testing.T) {
	mock := newEgressMock()
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", EgressRegistry: egress.NewRegistry(mock)})
	ctx := context.Background()

	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}
	info, err := fm.ProvisionAndAssign(ctx, "t-proxy", spec, egressManifest, nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Egress.Mode != egress.ModeProxy || info.Egress.Provider != egress.MockName || info.Egress.Protocol != egress.ProtocolSOCKS5 {
		t.Fatalf("resolved egress = %+v", info.Egress)
	}
	if !strings.HasPrefix(info.Egress.Endpoint, "socks5h://127.0.0.1:") {
		t.Fatalf("endpoint = %q, want a socks5h url on the env's listen ip", info.Egress.Endpoint)
	}
	if active := mock.Active(); len(active) != 1 || active[0] != info.ID {
		t.Fatalf("mock listeners = %v, want [%s]", active, info.ID)
	}
	// the endpoint survives a read back through the store, minus health.
	records, err := fm.store.ListVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Egress.Endpoint != info.Egress.Endpoint || records[0].Egress.Provider != egress.MockName {
		t.Fatalf("persisted egress = %+v", records[0].Egress)
	}

	if err := fm.DestroyVM(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if active := mock.Active(); len(active) != 0 {
		t.Fatalf("destroy left listeners behind: %v", active)
	}
}

func TestEgressProvisionFailureIsAFailedCreateWithNoVM(t *testing.T) {
	mock := newEgressMock()
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-", EgressRegistry: egress.NewRegistry(mock)})
	ctx := context.Background()

	mock.FailNextProvision(errors.New("backend refused to start"))
	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}
	_, err := fm.ProvisionAndAssign(ctx, "t-fail", spec, egressManifest, nil, BootOptions{})
	if err == nil || !strings.Contains(err.Error(), "backend refused to start") || !strings.Contains(err.Error(), egress.MockName) {
		t.Fatalf("err = %v, want the provider's reason and its name", err)
	}
	if n := len(fm.ListFleet()); n != 0 {
		t.Fatalf("%d vms tracked after a failed create, want 0", n)
	}
	waitForEgress(t, "cleanup destroy of the half-built vm", func() bool { return p.count() == 0 })
	if active := mock.Active(); len(active) != 0 {
		t.Fatalf("failed provision left listeners: %v", active)
	}
	tasks, err := fm.store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].RunStatus != TaskRunFailed {
		t.Fatalf("task records = %+v, want one failed", tasks)
	}
}

func TestEgressUnknownProviderAndNoRegistryAreRefused(t *testing.T) {
	ctx := context.Background()
	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: "warp"}}

	withMock := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", EgressRegistry: egress.NewRegistry(newEgressMock())})
	_, err := withMock.ProvisionAndAssign(ctx, "t-unknown", spec, egressManifest, nil, BootOptions{})
	var unknown *egress.UnknownProviderError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want *egress.UnknownProviderError", err)
	}

	// no registry at all must not fall back to direct.
	none := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})
	_, err = none.ProvisionAndAssign(ctx, "t-none", spec, egressManifest, nil, BootOptions{})
	if err == nil || !strings.Contains(err.Error(), "no egress providers") {
		t.Fatalf("err = %v, want a refusal naming the missing registry", err)
	}
	if n := len(none.ListFleet()); n != 0 {
		t.Fatalf("%d vms tracked after a refused create, want 0", n)
	}
}

func TestEgressSurvivesRestartAndReleasesAfterRecovery(t *testing.T) {
	mock := newEgressMock()
	p := newMockProvider()
	store := NewMemoryStateStore()
	reg := egress.NewRegistry(mock)
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-", StateStore: store, EgressRegistry: reg})
	ctx := context.Background()

	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}
	info, err := fm.ProvisionAndAssign(ctx, "t-restart", spec, egressManifest, nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// a second fleet over the same store and provider is an orchestrator
	// restart. the recovered vm must still know its backend and endpoint.
	fm2 := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-", StateStore: store, EgressRegistry: reg})
	if err := fm2.recoverState(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, ok := fm2.GetVM(info.ID)
	if !ok {
		t.Fatalf("vm %s not recovered", info.ID)
	}
	if recovered.Egress != info.Egress {
		t.Fatalf("recovered egress = %+v, want %+v", recovered.Egress, info.Egress)
	}
	if err := fm2.DestroyVM(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if active := mock.Active(); len(active) != 0 {
		t.Fatalf("destroy after recovery left listeners: %v", active)
	}
}

func TestEgressProvidersListsTheRegistry(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), EgressRegistry: egress.NewRegistry(newEgressMock())})
	if got := fm.EgressProviders(); len(got) != 1 || got[0] != egress.MockName {
		t.Fatalf("EgressProviders() = %v", got)
	}
	if got := NewFleetManager(FleetConfig{Provider: newMockProvider()}).EgressProviders(); len(got) != 0 {
		t.Fatalf("EgressProviders() without a registry = %v, want none", got)
	}
}
