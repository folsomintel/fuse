package durable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/firecracker"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestHostOpsThroughRestate drives a real fleet's host register, cordon and
// remove through Host/{host_id}: the registered host gets a provider built
// from its record, the token reaches the journal only encrypted, and a
// refusal comes back as the fleet's sentinel.
func TestHostOpsThroughRestate(t *testing.T) {
	store := orchestrator.NewMemoryStateStore()
	var built []string
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:           firecracker.New(firecracker.Config{UseStub: true}),
		StateStore:         store,
		Prefix:             "fuse-",
		TokenEncryptionKey: tokenTestKey,
		HostProviderFactory: func(url, token string, _ orchestrator.HostBackend) orchestrator.Provider {
			built = append(built, token)
			return firecracker.New(firecracker.Config{UseStub: true})
		},
	})
	client, _, stop := serveFleetAt(t, fm, "")
	defer stop()
	fm.SetDurableHosts(client)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hostID := "h-" + time.Now().Format("150405.000000")
	host := orchestrator.Host{
		ID: hostID, URL: "http://h1.test", Token: "agent-secret",
		Capacity: orchestrator.HostCapacity{CPUs: 4, RamMB: 4096, StorageGB: 40, VMCount: 4},
	}
	if err := fm.RegisterHost(ctx, host, firecracker.New(firecracker.Config{UseStub: true})); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := fm.GetHost(hostID)
	if !ok || got.Token != "agent-secret" || got.State != orchestrator.HostActive {
		t.Fatalf("registered host = %+v, %v", got, ok)
	}
	if len(built) != 1 || built[0] != "agent-secret" {
		t.Fatalf("provider built with %v, want the decrypted token once", built)
	}

	if err := fm.CordonHost(ctx, hostID); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	rec, err := store.GetHost(ctx, hostID)
	if err != nil || rec.State != orchestrator.HostCordoned {
		t.Fatalf("stored host = %+v, %v; want cordoned", rec, err)
	}
	if string(rec.TokenEncrypted) == "agent-secret" {
		t.Fatal("token stored in the clear")
	}

	if err := fm.RemoveHost(ctx, hostID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := fm.GetHost(hostID); ok {
		t.Fatal("host still registered")
	}
	if _, err := store.GetHost(ctx, hostID); err == nil {
		t.Fatal("host row not deleted")
	}

	if err := fm.RemoveHost(ctx, hostID); !errors.Is(err, orchestrator.ErrHostNotFound) {
		t.Fatalf("remove again = %v, want ErrHostNotFound", err)
	}
	if err := fm.CordonHost(ctx, "missing-"+hostID); !errors.Is(err, orchestrator.ErrHostNotFound) {
		t.Fatalf("cordon missing = %v, want ErrHostNotFound", err)
	}
}
