package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

func TestReconcileExpiresPastDeadline(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, _ := ask(fm, vmID)
	rec, _ := fm.store.GetIntervention(context.Background(), in.ID)
	rec.Deadline = time.Now().Add(-time.Second)
	_ = fm.store.UpsertIntervention(context.Background(), rec)

	fm.reconcileInterventions(context.Background())

	rec, _ = fm.store.GetIntervention(context.Background(), in.ID)
	if rec.State != string(hitl.StateExpired) || rec.Outcome != string(hitl.OutcomeReject) {
		t.Fatalf("after sweep: state %s outcome %s", rec.State, rec.Outcome)
	}
}

func TestReconcileCancelsWhenEnvGone(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, _ := ask(fm, vmID)
	// the vm vanishes without going through DestroyVM, as after a crash.
	fm.mu.Lock()
	delete(fm.vms, vmID)
	fm.mu.Unlock()

	fm.reconcileInterventions(context.Background())

	got, _ := fm.GetIntervention(context.Background(), in.ID)
	if got.State != hitl.StateCancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
}

func TestIdleReaperSkipsPendingIntervention(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, askSpec())
	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	// idle well past the timeout, as if the human were taking their time.
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-time.Hour)
	fm.mu.Unlock()

	for _, strike := range []string{"", "idle_timeout"} {
		v, err := fm.ExpiryCheck(context.Background(), vmID, strike)
		if err != nil {
			t.Fatal(err)
		}
		if v.Reap || v.Strike != "" {
			t.Fatalf("idle reaper fired on an env with a pending ask: %+v", v)
		}
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("env %s while an ask was pending, want running", s)
	}
}

func TestRecoveredFleetResumesPending(t *testing.T) {
	store := NewMemoryStateStore()
	fm, vmID := hitlFleet(t, FleetConfig{StateStore: store}, askSpec())
	in, _ := ask(fm, vmID)

	// a fresh manager over the same store stands in for a restart.
	restarted := newInlineFleet(FleetConfig{StateStore: store, Provider: newMockProvider()})
	got, err := restarted.GetIntervention(context.Background(), in.ID)
	if err != nil || got.State != hitl.StatePending {
		t.Fatalf("after restart: %+v, %v", got, err)
	}
	got, err = restarted.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "no", Reason: "", By: testActor})
	if err != nil || got.Decision.Answer != "no" {
		t.Fatalf("decide after restart: %+v, %v", got, err)
	}
}
