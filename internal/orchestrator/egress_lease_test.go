package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
)

// TestRecoverRestoresWireGuardReservation: a vm persisted with a wireguard
// lease takes its reservation back when the orchestrator restarts, so the
// same key is still refused on a second host.
func TestRecoverRestoresWireGuardReservation(t *testing.T) {
	store := NewMemoryStateStore()
	ctx := context.Background()
	status := egress.Status{Mode: egress.ModeProxy, Provider: egress.WireGuardName, Protocol: egress.ProtocolSOCKS5,
		Endpoint: "socks5h://10.200.3.1:1080", Lease: "abc123def456"}
	now := time.Now()
	if err := store.UpsertVM(ctx, VMRecord{ID: "fuse-wg", State: VMStateDraining, HostID: "h-1", Egress: status, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	gone := status
	gone.Lease = "0123456789ab"
	if err := store.UpsertVM(ctx, VMRecord{ID: "fuse-gone", State: VMStateDestroying, HostID: "h-3", Egress: gone, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	recs, err := store.ListVMs(ctx)
	if err != nil || len(recs) != 2 || recs[0].Egress.Lease == "" {
		t.Fatalf("stored egress = %+v, %v; want the leases kept", recs, err)
	}

	wg := egress.NewWireGuard()
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", StateStore: store, EgressRegistry: egress.NewRegistry(wg)})
	if err := fm.recoverState(ctx); err != nil {
		t.Fatal(err)
	}
	// the restored reservation is held by fuse-wg on h-1, so a restore of
	// the same lease from another host is refused as a provision would be.
	err = wg.Restore("h-2", "fuse-other", status.Lease)
	if err == nil || !strings.Contains(err.Error(), "host h-1") {
		t.Fatalf("same key on a second host after restart err = %v, want refused", err)
	}
	// a destroying vm keeps its lease until it goes, and its reap, which no
	// destroy precedes when it vanished from its host, releases it.
	if err := wg.Restore("h-2", "fuse-other", gone.Lease); err == nil {
		t.Fatal("lease of a destroying vm not restored")
	}
	fm.reapDestroyedVMs(ctx, map[string]bool{}, vmListing{})
	fm.bg.Wait()
	if err := wg.Restore("h-2", "fuse-other", gone.Lease); err != nil {
		t.Fatalf("reaped vm still holds its key: %v", err)
	}
}

// TestEgressLeaseRoundTrips: the stored egress keeps the lease.
func TestEgressLeaseRoundTrips(t *testing.T) {
	raw, err := marshalEgress(egress.Status{Mode: egress.ModeProxy, Provider: "wireguard", Lease: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalEgress(raw)
	if err != nil || got.Lease != "abc" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}
