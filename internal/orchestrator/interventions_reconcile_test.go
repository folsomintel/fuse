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
	metrics := &captureMetrics{}
	fm, vmID := hitlFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute, Metrics: metrics}, askSpec())
	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	// idle well past the timeout, as if the human were taking their time.
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-time.Hour)
	fm.mu.Unlock()

	fm.reconcile(context.Background())
	fm.reconcile(context.Background())

	if got := metrics.last(); got.IdleVMsSuspected != 0 || got.IdleVMsFailed != 0 {
		t.Fatalf("idle reaper fired on an env with a pending ask: %+v", got)
	}
	if _, ok := fm.GetVM(vmID); !ok {
		t.Fatal("env reaped while an ask was pending")
	}
}

func TestRecoveredFleetResumesPending(t *testing.T) {
	store := NewMemoryStateStore()
	fm, vmID := hitlFleet(t, FleetConfig{StateStore: store}, askSpec())
	in, _ := ask(fm, vmID)

	// a fresh manager over the same store stands in for a restart.
	restarted := NewFleetManager(FleetConfig{StateStore: store, Provider: newMockProvider()})
	got, err := restarted.GetIntervention(context.Background(), in.ID)
	if err != nil || got.State != hitl.StatePending {
		t.Fatalf("after restart: %+v, %v", got, err)
	}
	got, err = restarted.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "no", Reason: "", By: testActor})
	if err != nil || got.Decision.Answer != "no" {
		t.Fatalf("decide after restart: %+v, %v", got, err)
	}
}
