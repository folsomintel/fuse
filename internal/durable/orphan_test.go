package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// orphanFleet scripts the orphan destroy steps: the host destroy fails while
// fail is set, and every step is counted.
type orphanFleet struct {
	fakeFleet
	mu       sync.Mutex
	fail     bool
	tracked  bool
	attempts int
	finished int
	failures []int
	dead     int
}

func (f *orphanFleet) OrphanCheck(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.tracked, nil
}

func (f *orphanFleet) OrphanDestroyOnHost(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.fail {
		return errors.New("host down")
	}
	return nil
}

func (f *orphanFleet) OrphanDestroyed(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished++
	return nil
}

func (f *orphanFleet) OrphanFailed(_ context.Context, _ string, attempt int, _ string, deadLetter bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, attempt)
	if deadLetter {
		f.dead++
	}
	return nil
}

// TestDestroyOrphanCountSurvivesRestart: the failed attempts are counted in
// object state, so an orchestrator that restarts mid-retry carries on from
// the count it had and dead-letters at the limit, then stops attempting.
func TestDestroyOrphanCountSurvivesRestart(t *testing.T) {
	vm := fmt.Sprintf("fuse-orphan-%d", time.Now().UnixNano())
	req := orchestrator.OrphanDestroyRequest{HostID: "h1", MaxRetries: 5}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := &orphanFleet{fail: true}
	client, _, stop := serveFleetAt(t, first, "")
	for i := 0; i < 2; i++ {
		res, err := client.DestroyOrphan(ctx, vm, req)
		if err != nil || res.DeadLettered {
			t.Fatalf("attempt %d = %+v, %v", i+1, res, err)
		}
	}
	stop()

	// the restarted orchestrator: a new process with a new fleet.
	second := &orphanFleet{fail: true}
	client = serveFleet(t, second, fastPolicy)
	var res orchestrator.OrphanDestroyResult
	for i := 0; i < 3; i++ {
		var err error
		if res, err = client.DestroyOrphan(ctx, vm, req); err != nil {
			t.Fatal(err)
		}
	}
	if !res.DeadLettered {
		t.Fatal("fifth failed attempt did not dead-letter")
	}
	if fmt.Sprint(second.failures) != "[3 4 5]" || second.dead != 1 {
		t.Fatalf("after restart failures = %v dead = %d, want [3 4 5] and 1", second.failures, second.dead)
	}

	// dead-lettered: answered from state, the host is not tried again.
	if res, err := client.DestroyOrphan(ctx, vm, req); err != nil || !res.DeadLettered {
		t.Fatalf("after dead letter = %+v, %v", res, err)
	}
	if second.attempts != 3 {
		t.Fatalf("host attempts after restart = %d, want 3", second.attempts)
	}
}

// TestDestroyOrphanSuccessClearsCount: a success after failures finishes the
// orphan and clears its count; a vm the fleet now tracks is left alone.
func TestDestroyOrphanSuccessClearsCount(t *testing.T) {
	vm := fmt.Sprintf("fuse-orphan-ok-%d", time.Now().UnixNano())
	req := orchestrator.OrphanDestroyRequest{MaxRetries: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fleet := &orphanFleet{fail: true}
	client := serveFleet(t, fleet, fastPolicy)
	if _, err := client.DestroyOrphan(ctx, vm, req); err != nil {
		t.Fatal(err)
	}
	fleet.mu.Lock()
	fleet.fail = false
	fleet.mu.Unlock()
	if res, err := client.DestroyOrphan(ctx, vm, req); err != nil || res.DeadLettered {
		t.Fatalf("success = %+v, %v", res, err)
	}
	if fleet.finished != 1 {
		t.Fatalf("finished = %d, want 1", fleet.finished)
	}

	// the count was cleared: one more failure is attempt 1, not 2.
	fleet.mu.Lock()
	fleet.fail = true
	fleet.mu.Unlock()
	if res, err := client.DestroyOrphan(ctx, vm, req); err != nil || res.DeadLettered {
		t.Fatalf("after clear = %+v, %v", res, err)
	}
	if fmt.Sprint(fleet.failures) != "[1 1]" {
		t.Fatalf("failures = %v, want [1 1]", fleet.failures)
	}

	fleet.mu.Lock()
	fleet.tracked = true
	fleet.mu.Unlock()
	before := fleet.attempts
	if _, err := client.DestroyOrphan(ctx, vm, req); err != nil {
		t.Fatal(err)
	}
	if fleet.attempts != before {
		t.Fatal("a tracked vm was destroyed as an orphan")
	}
}
