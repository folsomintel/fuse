package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/egress"
)

var errBackendRefused = errors.New("backend refused to start")

// storedEvents copies the audit events the memory store holds for one vm.
func storedEvents(store *MemoryStateStore, vmID string) []EventRecord {
	store.mu.Lock()
	defer store.mu.Unlock()
	var out []EventRecord
	for _, e := range store.events {
		if e.EntityType == "vm" && e.EntityID == vmID {
			out = append(out, e)
		}
	}
	return out
}

// egressHealthFleet boots one proxy-mode vm through the mock backend and
// returns everything a probe test needs. the logger writes to buf so a test
// can assert what never reaches a log line.
func egressHealthFleet(t *testing.T) (*FleetManager, *egress.Mock, VMInfo, *bytes.Buffer) {
	t.Helper()
	mock := newEgressMock()
	var buf bytes.Buffer
	fm := NewFleetManager(FleetConfig{
		Provider:       newMockProvider(),
		Prefix:         "fuse-",
		EgressRegistry: egress.NewRegistry(mock),
		Logger:         slog.New(slog.NewTextHandler(&buf, nil)),
	})
	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}
	info, err := fm.ProvisionAndAssign(context.Background(), "t-health", spec, egressManifest, nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return fm, mock, *info, &buf
}

func vmEgressHealth(t *testing.T, fm *FleetManager, vmID string) egress.Health {
	t.Helper()
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.vms[vmID].egress.Health
}

func TestReconcileEgressHealthProbesTheEndpoint(t *testing.T) {
	fm, _, info, _ := egressHealthFleet(t)
	if h := vmEgressHealth(t, fm, info.ID); h.State != egress.HealthUnknown {
		t.Fatalf("health before the first probe = %+v, want unknown", h)
	}

	summary := ReconcileSummary{}
	fm.reconcileEgressHealth(context.Background(), &summary)

	h := vmEgressHealth(t, fm, info.ID)
	if h.State != egress.HealthHealthy || h.Reason != "" || h.CheckedAt.IsZero() {
		t.Fatalf("health after probe = %+v, want healthy with a timestamp", h)
	}
	if summary.EgressChecked != 1 || summary.EgressUnhealthy != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if n := summary.EgressEndpoints[EgressEndpointKey{Provider: egress.MockName, State: egress.HealthHealthy}]; n != 1 {
		t.Fatalf("endpoint gauge = %v, want one healthy mock", summary.EgressEndpoints)
	}
	// the verdict reaches the snapshot a reader sees.
	got, _ := fm.GetVM(info.ID)
	if got.Egress.Health.State != egress.HealthHealthy {
		t.Fatalf("VMInfo egress health = %+v", got.Egress.Health)
	}
}

func TestReconcileEgressHealthDegradesAndNeverFallsBack(t *testing.T) {
	fm, mock, info, buf := egressHealthFleet(t)
	summary := ReconcileSummary{}
	fm.reconcileEgressHealth(context.Background(), &summary)

	// the backend dies under a running environment.
	if !mock.Kill(info.ID) {
		t.Fatal("nothing to kill")
	}
	summary = ReconcileSummary{}
	fm.reconcileEgressHealth(context.Background(), &summary)

	h := vmEgressHealth(t, fm, info.ID)
	if h.State != egress.HealthUnhealthy || !strings.Contains(h.Reason, "dial") {
		t.Fatalf("health after kill = %+v, want unhealthy with a dial reason", h)
	}
	if summary.EgressUnhealthy != 1 {
		t.Fatalf("summary = %+v, want one unhealthy", summary)
	}
	// nothing acts on it: the vm is still running, still proxy mode, and
	// the backend record is still held so teardown can release it. that is
	// the anti-fallback guard: no egress, never direct egress.
	got, ok := fm.GetVM(info.ID)
	if !ok || got.State != VMStateRunning || got.Egress.Mode != egress.ModeProxy {
		t.Fatalf("vm after degraded probe = %+v", got)
	}
	if active := mock.Active(); len(active) != 1 {
		t.Fatalf("backend record released by a health failure: %v", active)
	}
	// the transition is logged by provider and state only; the endpoint
	// never reaches a log line.
	logs := buf.String()
	if !strings.Contains(logs, "egress health changed") || !strings.Contains(logs, "to=unhealthy") {
		t.Fatalf("no transition logged:\n%s", logs)
	}
	if strings.Contains(logs, info.Egress.Endpoint) {
		t.Fatalf("endpoint reached a log line:\n%s", logs)
	}

	// the reason and endpoint only ever appear on the environment itself.
	if got.Egress.Health.Reason == "" {
		t.Fatal("reason missing from the environment's egress health")
	}
	if err := fm.DestroyVM(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if active := mock.Active(); len(active) != 0 {
		t.Fatalf("destroy after degradation left listeners: %v", active)
	}
}

func TestReconcileEgressHealthSkipsDirectAndUnregistered(t *testing.T) {
	rec := &recordingEgress{}
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", EgressRegistry: egress.NewRegistry(rec)})
	if _, err := fm.ProvisionAndAssign(context.Background(), "t-direct", Spec{CPUs: 1, RamMB: 256}, egressManifest, nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	summary := ReconcileSummary{}
	fm.reconcileEgressHealth(context.Background(), &summary)
	if summary.EgressChecked != 0 || summary.EgressEndpoints != nil {
		t.Fatalf("direct vm was probed: %+v", summary)
	}

	// a fleet with no registry has nothing to probe and must not panic.
	none := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})
	none.reconcileEgressHealth(context.Background(), &summary)
}

// egressMetricsRecorder captures the two counters that fire outside the
// reconcile tick.
type egressMetricsRecorder struct {
	captureMetrics
	provisionFailed []string
	teardownFailed  []string
}

func (r *egressMetricsRecorder) EgressProvisionFailed(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.provisionFailed = append(r.provisionFailed, provider)
}

func (r *egressMetricsRecorder) EgressTeardownFailed(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.teardownFailed = append(r.teardownFailed, provider)
}

func TestEgressProvisionFailureCountsAndEmits(t *testing.T) {
	mock := newEgressMock()
	rec := &egressMetricsRecorder{}
	store := NewMemoryStateStore()
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", EgressRegistry: egress.NewRegistry(mock), Metrics: rec, StateStore: store})
	mock.FailNextProvision(errBackendRefused)

	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}
	if _, err := fm.ProvisionAndAssign(context.Background(), "t-fail", spec, egressManifest, nil, BootOptions{}); err == nil {
		t.Fatal("expected the create to fail")
	}
	waitForEgress(t, "provision failure counter", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.provisionFailed) == 1
	})
	if rec.provisionFailed[0] != egress.MockName {
		t.Fatalf("counter labelled %q, want %q", rec.provisionFailed[0], egress.MockName)
	}
	// the event names the provider and carries no endpoint (there is none).
	waitForEgress(t, "vm.egress_failed event", func() bool {
		for _, e := range storedEvents(store, "fuse-t-fail") {
			if e.EventType == "vm.egress_failed" && strings.Contains(string(e.Payload), egress.MockName) {
				return true
			}
		}
		return false
	})
}

func TestEgressProvisionedEventOnRunning(t *testing.T) {
	mock := newEgressMock()
	store := NewMemoryStateStore()
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", EgressRegistry: egress.NewRegistry(mock), StateStore: store})
	spec := Spec{CPUs: 1, RamMB: 256, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}
	info, err := fm.ProvisionAndAssign(context.Background(), "t-ok", spec, egressManifest, nil, BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events := storedEvents(store, info.ID)
	var found string
	for _, e := range events {
		if e.EventType == "vm.egress_provisioned" {
			found = string(e.Payload)
		}
	}
	if found == "" {
		t.Fatalf("no vm.egress_provisioned event in %+v", events)
	}
	if !strings.Contains(found, `"provider":"mock"`) || strings.Contains(found, info.Egress.Endpoint) {
		t.Fatalf("event payload = %s, want the provider and never the endpoint", found)
	}
}
