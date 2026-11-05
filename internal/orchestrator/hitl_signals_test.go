package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

func signalSpec(gates ...hitl.Gate) *hitl.Spec {
	return &hitl.Spec{TimeoutSeconds: 600, Requests: hitl.Requests{Ask: &hitl.AskRequests{}}, Gates: gates}
}

func pendingOf(t *testing.T, fm *FleetManager, vmID string, k hitl.Kind) []hitl.Intervention {
	t.Helper()
	list, _, err := fm.ListInterventions(context.Background(), InterventionListFilter{EnvID: vmID})
	if err != nil {
		t.Fatal(err)
	}
	var out []hitl.Intervention
	for _, in := range list {
		if in.Kind == k {
			out = append(out, in)
		}
	}
	return out
}

func TestFailingExecsRaiseStallAndPauseAgent(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, signalSpec(hitl.Gate{Name: "flailing", On: hitl.KindStall, Match: hitl.GateMatch{Failures: 3}}))
	ctx := context.Background()
	env := fm.vms[vmID].env.(*mockEnv)
	env.execResult = ExecResult{ExitCode: 1}

	for i := 0; i < 3; i++ {
		if _, err := fm.Exec(ctx, vmID, []string{"false"}, ExecOptions{}); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
	stalls := pendingOf(t, fm, vmID, hitl.KindStall)
	if len(stalls) != 1 || stalls[0].State != hitl.StatePending || stalls[0].Signal.Failures != 3 || stalls[0].Gate != "flailing" {
		t.Fatalf("stalls = %+v, want one pending after 3 failures", stalls)
	}
	_, err := fm.Exec(ctx, vmID, []string{"false"}, ExecOptions{})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.InterventionID != stalls[0].ID {
		t.Fatalf("exec while stalled: err = %v, want locked by the stall", err)
	}

	// a human acks it; the agent runs again.
	if _, err := fm.DecideIntervention(ctx, stalls[0].ID, DecideRequest{Outcome: hitl.OutcomeApprove, Reason: "looked, carry on", By: humanActor}); err != nil {
		t.Fatal(err)
	}
	if _, err := fm.Exec(ctx, vmID, []string{"false"}, ExecOptions{}); err != nil {
		t.Fatalf("exec after ack: %v", err)
	}
}

func TestSuccessResetsFailureRun(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, signalSpec(hitl.Gate{Name: "flailing", On: hitl.KindStall, Match: hitl.GateMatch{Failures: 3}}))
	ctx := context.Background()
	env := fm.vms[vmID].env.(*mockEnv)
	for _, code := range []int{1, 1, 0, 1, 1} {
		env.execResult = ExecResult{ExitCode: code}
		if _, err := fm.Exec(ctx, vmID, []string{"x"}, ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := pendingOf(t, fm, vmID, hitl.KindStall); len(got) != 0 {
		t.Fatalf("stall raised across a success: %+v", got)
	}
}

func TestNotifyStallDoesNotLock(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, signalSpec(hitl.Gate{Name: "flailing", On: hitl.KindStall, Mode: hitl.ModeNotify, Match: hitl.GateMatch{Failures: 2}}))
	ctx := context.Background()
	fm.vms[vmID].env.(*mockEnv).execResult = ExecResult{ExitCode: 2}
	for i := 0; i < 3; i++ {
		if _, err := fm.Exec(ctx, vmID, []string{"x"}, ExecOptions{}); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
	got := pendingOf(t, fm, vmID, hitl.KindStall)
	if len(got) != 1 || got[0].State != hitl.StateApproved {
		t.Fatalf("notify stall = %+v, want one terminal record", got)
	}
}

func TestQuietStallRaisedOncePerSilence(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, signalSpec(hitl.Gate{Name: "quiet", On: hitl.KindStall, Match: hitl.GateMatch{QuietSeconds: 900}}))
	ctx := context.Background()
	fm.hitlMu.Lock()
	fm.signalStateLocked(vmID).lastCall = time.Now().Add(-20 * time.Minute)
	fm.hitlMu.Unlock()

	fm.reconcileSignals(ctx)
	fm.reconcileSignals(ctx)
	stalls := pendingOf(t, fm, vmID, hitl.KindStall)
	if len(stalls) != 1 || stalls[0].Signal.QuietSeconds < 1200 {
		t.Fatalf("quiet stalls = %+v, want exactly one", stalls)
	}
}

func expiryFleet(t *testing.T) (*FleetManager, string) {
	t.Helper()
	metrics := &captureMetrics{}
	fm, vmID := hitlFleet(t, FleetConfig{DefaultIdleTimeout: 10 * time.Minute, Metrics: metrics},
		signalSpec(hitl.Gate{Name: "extend", On: hitl.KindExpiry, Match: hitl.GateMatch{BeforeSeconds: 300}}))
	// 7 minutes idle: the reap is 3 minutes out, inside the 5 minute window.
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-7 * time.Minute)
	fm.mu.Unlock()
	return fm, vmID
}

func TestExpiryApprovedExtendsEnv(t *testing.T) {
	fm, vmID := expiryFleet(t)
	ctx := context.Background()
	fm.reconcileSignals(ctx)
	fm.reconcileSignals(ctx)
	exp := pendingOf(t, fm, vmID, hitl.KindExpiry)
	if len(exp) != 1 || exp[0].State != hitl.StatePending || exp[0].Signal.Reaper != hitl.ReaperIdle {
		t.Fatalf("expiries = %+v, want one pending for the idle reaper", exp)
	}
	if exp[0].Deadline.After(exp[0].Signal.ReapAt.Add(time.Second)) {
		t.Fatalf("expiry deadline %s is after the reap %s", exp[0].Deadline, *exp[0].Signal.ReapAt)
	}

	out, err := fm.DecideIntervention(ctx, exp[0].ID, DecideRequest{Outcome: hitl.OutcomeApprove, Extend: 2 * time.Hour, By: humanActor})
	if err != nil {
		t.Fatal(err)
	}
	if out.Signal.ExtendedUntil == nil || time.Until(*out.Signal.ExtendedUntil) < 119*time.Minute {
		t.Fatalf("approved expiry = %+v, want a 2h extension", out.Signal)
	}

	// idle well past the window, but inside the extension: not reaped.
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-time.Hour)
	fm.mu.Unlock()
	fm.reconcile(ctx)
	fm.reconcile(ctx)
	if _, ok := fm.GetVM(vmID); !ok {
		t.Fatal("extended env was reaped")
	}

	// the extension survives a restart.
	fm.hitlMu.Lock()
	fm.hitlSignals = nil
	fm.hitlMu.Unlock()
	fm.recoverExtensions(ctx)
	if !fm.extendedEnvs(time.Now())[vmID] {
		t.Fatal("extension lost on restart")
	}
}

func TestExpiryUnansweredLetsReapProceed(t *testing.T) {
	fm, vmID := expiryFleet(t)
	ctx := context.Background()
	fm.reconcileSignals(ctx)
	exp := pendingOf(t, fm, vmID, hitl.KindExpiry)
	if len(exp) != 1 {
		t.Fatalf("expiries = %+v", exp)
	}

	// the reaper waits while the human decides.
	fm.mu.Lock()
	fm.vms[vmID].lastActivityAt = time.Now().Add(-time.Hour)
	fm.mu.Unlock()
	fm.reconcileIdleVMs(ctx, &ReconcileSummary{})
	fm.reconcileIdleVMs(ctx, &ReconcileSummary{})
	if _, ok := fm.GetVM(vmID); !ok {
		t.Fatal("env reaped while its expiry was pending")
	}

	// nobody answers: the expiry times out as a reject and the reap goes on.
	rec, _ := fm.store.GetIntervention(ctx, exp[0].ID)
	rec.Deadline = time.Now().Add(-time.Second)
	_ = fm.store.UpsertIntervention(ctx, rec)
	got, _ := fm.GetIntervention(ctx, exp[0].ID)
	if got.State != hitl.StateExpired || got.Decision.Outcome != hitl.OutcomeReject {
		t.Fatalf("timed out expiry = %+v, want expired with reject", got)
	}
	fm.reconcileIdleVMs(ctx, &ReconcileSummary{})
	fm.reconcileIdleVMs(ctx, &ReconcileSummary{})
	v, ok := fm.GetVM(vmID)
	if ok && v.State != VMStateDestroying {
		t.Fatalf("env not reaped after its expiry timed out: %+v", v)
	}
}
