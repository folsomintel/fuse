package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/egress"
)

// allowTestProvider is migrateTestProvider that also replaces allowlists
// and records the spec every vm was created with.
type allowTestProvider struct {
	*migrateTestProvider
	mu    sync.Mutex
	specs map[string]Spec
	calls [][]string
	err   error
}

func newAllowTestProvider() *allowTestProvider {
	return &allowTestProvider{migrateTestProvider: newMigrateTestProvider(), specs: map[string]Spec{}}
}

func (p *allowTestProvider) Create(ctx context.Context, spec Spec) (Environment, error) {
	p.record(spec)
	return p.migrateTestProvider.Create(ctx, spec)
}

func (p *allowTestProvider) CreateFromCheckpoint(ctx context.Context, spec Spec, srcVMID, checkpointID string) (Environment, error) {
	p.record(spec)
	return p.migrateTestProvider.CreateFromCheckpoint(ctx, spec, srcVMID, checkpointID)
}

func (p *allowTestProvider) record(spec Spec) {
	p.mu.Lock()
	p.specs[spec.Name] = spec
	p.mu.Unlock()
}

func (p *allowTestProvider) spec(name string) Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.specs[name]
}

func (p *allowTestProvider) SetEgressAllow(_ context.Context, _ string, cidrs []string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, cidrs)
	if p.err != nil {
		return nil, p.err
	}
	return cidrs, nil
}

func provisionAllowVM(t *testing.T, fm *FleetManager, taskID string, allow []string) string {
	t.Helper()
	manifest := []byte(`{"version":"1","services":{}}`)
	spec := Spec{Egress: egress.Spec{Mode: egress.ModeDirect, Allow: allow}}
	if _, err := fm.ProvisionAndAssign(context.Background(), taskID, spec, manifest, nil, BootOptions{}); err != nil {
		t.Fatalf("provision vm: %v", err)
	}
	return "fuse-" + taskID
}

func TestSetEgressAllow_forkCarriesTheCurrentList(t *testing.T) {
	provider := newAllowTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	srcID := provisionAllowVM(t, fm, "task-1", []string{"1.1.1.1/32"})

	want := []string{"10.0.0.0/8"}
	if _, err := fm.SetEgressAllow(context.Background(), srcID, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	newID, err := forkOne(context.Background(), fm, srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got := provider.spec(newID).Egress.Allow; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork created with allow %v, want %v", got, want)
	}
	if info, _ := fm.GetVM(newID); !reflect.DeepEqual(info.Spec.Egress.Allow, want) {
		t.Fatalf("fork tracks allow %v, want %v", info.Spec.Egress.Allow, want)
	}
}

func TestSetEgressAllow_migrateCarriesTheCurrentList(t *testing.T) {
	provider := newAllowTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	srcID := provisionAllowVM(t, fm, "task-1", []string{"1.1.1.1/32"})

	want := []string{"10.0.0.0/8", "8.8.8.8/32"}
	if _, err := fm.SetEgressAllow(context.Background(), srcID, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	newID, err := fm.MigrateVM(context.Background(), srcID, MigrateOptions{})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := provider.spec(newID).Egress.Allow; !reflect.DeepEqual(got, want) {
		t.Fatalf("migrate target created with allow %v, want %v", got, want)
	}
	if info, _ := fm.GetVM(newID); !reflect.DeepEqual(info.Spec.Egress.Allow, want) {
		t.Fatalf("migrate target tracks allow %v, want %v", info.Spec.Egress.Allow, want)
	}
}

// TestSetEgressAllow_hostRefusalKeepsTheList: the host is asked first, so a
// list it refused is never recorded.
func TestSetEgressAllow_hostRefusalKeepsTheList(t *testing.T) {
	provider := newAllowTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionAllowVM(t, fm, "task-1", []string{"1.1.1.1/32"})

	provider.err = &HTTPStatusError{Code: 409, Body: "proxy egress"}
	_, err := fm.SetEgressAllow(context.Background(), vmID, []string{"10.0.0.0/8"})
	if !errors.Is(err, ErrEgressAllowConflict) {
		t.Fatalf("err = %v, want ErrEgressAllowConflict", err)
	}
	provider.err = &HTTPStatusError{Code: 400, Body: "bad cidr"}
	if _, err := fm.SetEgressAllow(context.Background(), vmID, []string{"10.0.0.0/8"}); !errors.Is(err, ErrEgressAllowInvalid) {
		t.Fatalf("err = %v, want ErrEgressAllowInvalid", err)
	}
	if info, _ := fm.GetVM(vmID); !reflect.DeepEqual(info.Spec.Egress.Allow, []string{"1.1.1.1/32"}) {
		t.Fatalf("allow after refusals = %v, want the create's", info.Spec.Egress.Allow)
	}
}

// TestSetEgressAllow_survivesRestart: the list set while the vm ran is what
// a restarted orchestrator reads back, from the memory store's record and
// from the postgres egress column alike.
func TestSetEgressAllow_survivesRestart(t *testing.T) {
	store := NewMemoryStateStore()
	provider := newAllowTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-", StateStore: store})
	vmID := provisionAllowVM(t, fm, "task-1", nil)

	want := []string{"10.0.0.0/8"}
	if _, err := fm.SetEgressAllow(context.Background(), vmID, want); err != nil {
		t.Fatalf("set: %v", err)
	}

	restarted := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", StateStore: store, TokenEncryptionKey: []byte(inlineFleetKey)})
	if err := restarted.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, ok := restarted.GetVM(vmID)
	if !ok || !reflect.DeepEqual(info.Spec.Egress.Allow, want) {
		t.Fatalf("recovered allow = %v (found %v), want %v", info.Spec.Egress.Allow, ok, want)
	}

	// the postgres row keeps the list in egress_json, next to the status.
	raw, err := marshalEgress(egress.Status{Mode: egress.ModeDirect}, want)
	if err != nil {
		t.Fatal(err)
	}
	status, allow, err := unmarshalEgress(raw)
	if err != nil || status.Mode != egress.ModeDirect || !reflect.DeepEqual(allow, want) {
		t.Fatalf("round trip = %+v %v %v, want direct with %v", status, allow, err, want)
	}
	// a row written before the field reads as unrestricted.
	if _, allow, err := unmarshalEgress([]byte(`{"mode":"direct"}`)); err != nil || allow != nil {
		t.Fatalf("legacy row allow = %v, %v, want none", allow, err)
	}
}

func TestSetEgressAllow_refusesBeforeTheHost(t *testing.T) {
	provider := newAllowTestProvider()
	fm := newInlineFleet(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionAllowVM(t, fm, "task-1", nil)

	if _, err := fm.SetEgressAllow(context.Background(), vmID, []string{"::1"}); !errors.Is(err, ErrEgressAllowInvalid) {
		t.Fatalf("ipv6: err = %v, want ErrEgressAllowInvalid", err)
	}
	if _, err := fm.SetEgressAllow(context.Background(), "fuse-missing", nil); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("missing: err = %v, want ErrVMNotFound", err)
	}
	fm.mu.Lock()
	fm.migrating = map[string]int{vmID: 1}
	fm.mu.Unlock()
	if _, err := fm.SetEgressAllow(context.Background(), vmID, nil); !errors.Is(err, ErrEgressAllowConflict) {
		t.Fatalf("migrating: err = %v, want ErrEgressAllowConflict", err)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("host reached: %v", provider.calls)
	}
}
