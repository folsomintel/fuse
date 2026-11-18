package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// vmWriteFailStore fails every vm upsert while fail is set.
type vmWriteFailStore struct {
	*MemoryStateStore
	mu   sync.Mutex
	fail bool
}

func (s *vmWriteFailStore) UpsertVM(ctx context.Context, r VMRecord) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return errors.New("db down")
	}
	return s.MemoryStateStore.UpsertVM(ctx, r)
}

// TestExpiryCheckReapFailsWhenWriteFails: a reaping check whose vm write
// fails puts the vm back and fails, so restate retries it; the retry reaps.
func TestExpiryCheckReapFailsWhenWriteFails(t *testing.T) {
	store := &vmWriteFailStore{MemoryStateStore: NewMemoryStateStore()}
	fm, vmID := idleFleet(t, FleetConfig{Provider: newMockProvider(), DefaultIdleTimeout: 5 * time.Minute, StateStore: store}, "task-1", 30*time.Minute)
	withDurableExpiry(fm)
	ctx := context.Background()

	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	if _, err := fm.ExpiryCheck(ctx, vmID, "idle_timeout"); err == nil {
		t.Fatal("reap with a failing store did not fail the step")
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state after a failed reap %s, want running", s)
	}

	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	v, err := fm.ExpiryCheck(ctx, vmID, "idle_timeout")
	if err != nil || !v.Reap {
		t.Fatalf("retried check = %+v, %v; want a reap", v, err)
	}
	recs, err := store.ListVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored VMState
	for _, r := range recs {
		if r.ID == vmID {
			stored = r.State
		}
	}
	if stored != VMStateDestroying {
		t.Fatalf("stored vm state = %q, want destroying", stored)
	}
	letters, _ := store.ListDeadLetters(ctx)
	if len(letters) != 1 || letters[0].Kind != DeadLetterIdleTimeout {
		t.Fatalf("dead letters = %+v, want one idle_timeout", letters)
	}
}

// slowVMStore holds every vm upsert until release is closed.
type slowVMStore struct {
	*MemoryStateStore
	release chan struct{}
	done    chan struct{}
}

func (s *slowVMStore) UpsertVM(ctx context.Context, r VMRecord) error {
	<-s.release
	defer close(s.done)
	return s.MemoryStateStore.UpsertVM(ctx, r)
}

// TestStopWaitsForBackgroundWrites: Stop returns only once a background
// persist in flight has landed.
func TestStopWaitsForBackgroundWrites(t *testing.T) {
	store := &slowVMStore{MemoryStateStore: NewMemoryStateStore(), release: make(chan struct{}), done: make(chan struct{})}
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", StateStore: store, ReconcileInterval: time.Hour})
	fm.Start(context.Background())
	fm.mu.Lock()
	fm.vms["fuse-a"] = &vm{id: "fuse-a", state: VMStateRunning, createdAt: time.Now(), updatedAt: time.Now()}
	fm.mu.Unlock()
	fm.persistVMBackground("fuse-a")

	stopped := make(chan struct{})
	go func() {
		fm.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned with a write in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the write landed")
	}
	select {
	case <-store.done:
	default:
		t.Fatal("Stop returned before the write finished")
	}
}

// TestHostWriteSkipsRemovedHost: a background host write that runs after the
// host was removed does not write its row back.
func TestHostWriteSkipsRemovedHost(t *testing.T) {
	store := NewMemoryStateStore()
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", StateStore: store})
	ctx := context.Background()
	if err := fm.RegisterHost(ctx, gpuFleetHost("h1", 1, "a100"), newMockProvider()); err != nil {
		t.Fatal(err)
	}
	if err := fm.RemoveHost(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := fm.writeHostRecord(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetHost(ctx, "h1"); err == nil {
		t.Fatal("removed host written back")
	}
}
