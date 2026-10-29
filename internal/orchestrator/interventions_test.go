package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

var testActor = hitl.Actor{ID: "master", Source: hitl.ActorKey}

// hitlFleet builds a fleet with one running vm that declares ask requests.
func hitlFleet(t *testing.T, cfg FleetConfig, spec *hitl.Spec) (*FleetManager, string) {
	t.Helper()
	if cfg.TaskStuckTimeout == 0 {
		cfg.TaskStuckTimeout = 24 * time.Hour
	}
	fm, vmID := fleetWithRunningVM(t, cfg, "task-hitl", time.Minute)
	fm.mu.Lock()
	fm.vms[vmID].spec.Hitl = spec
	fm.mu.Unlock()
	return fm, vmID
}

func askSpec() *hitl.Spec {
	return &hitl.Spec{TimeoutSeconds: 600, Requests: hitl.Requests{Ask: &hitl.AskRequests{}}}
}

func ask(fm *FleetManager, vmID string) (hitl.Intervention, error) {
	return fm.RequestHuman(context.Background(), vmID, HumanRequest{
		Kind: hitl.KindAsk,
		Ask:  hitl.AskPayload{Question: "deploy to prod?", Choices: []string{"yes", "no"}},
		By:   testActor,
	})
}

func TestRequestHumanRequiresDeclaredKind(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, nil)
	if _, err := ask(fm, vmID); !errors.Is(err, ErrHitlNotDeclared) {
		t.Fatalf("no hitl block: err = %v, want ErrHitlNotDeclared", err)
	}
	if _, err := ask(fm, "fuse-missing"); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("unknown env: err = %v, want ErrVMNotFound", err)
	}
}

func TestRequestHumanRejectsBadInput(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	_, err := fm.RequestHuman(context.Background(), vmID, HumanRequest{Kind: hitl.KindAsk, By: testActor})
	if !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("empty question: err = %v, want ErrInvalidIntervention", err)
	}
	_, err = fm.RequestHuman(context.Background(), vmID, HumanRequest{
		Kind: hitl.KindAsk, Ask: hitl.AskPayload{Question: "q"}, Timeout: time.Second, By: testActor,
	})
	if !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("1s timeout: err = %v, want ErrInvalidIntervention", err)
	}
}

func TestAskAnsweredWakesWaiter(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, err := ask(fm, vmID)
	if err != nil {
		t.Fatal(err)
	}
	if in.State != hitl.StatePending || in.Deadline.IsZero() {
		t.Fatalf("new ask = %+v", in)
	}

	done := make(chan hitl.Intervention, 1)
	go func() {
		got, _ := fm.WaitIntervention(context.Background(), in.ID, 10*time.Second)
		done <- got
	}()
	time.Sleep(50 * time.Millisecond)

	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "maybe", Reason: "", By: testActor}); !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("answer outside choices: err = %v", err)
	}
	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "yes", Reason: "", By: testActor}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.State != hitl.StateApproved || got.Decision == nil || got.Decision.Answer != "yes" {
			t.Fatalf("waiter got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken by the decision")
	}

	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeReject, Answer: "", Reason: "no", By: testActor}); !errors.Is(err, ErrInterventionResolved) {
		t.Fatalf("second decision: err = %v, want ErrInterventionResolved", err)
	}
}

func TestRejectCarriesReason(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, _ := ask(fm, vmID)
	got, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeReject, Answer: "", Reason: "", By: testActor})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != hitl.StateRejected || got.Decision.Reason == "" {
		t.Fatalf("reject = %+v", got.Decision)
	}
}

func TestExpiryAppliesOnTimeout(t *testing.T) {
	spec := askSpec()
	spec.OnTimeout = hitl.OnTimeoutAllow
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	in, _ := ask(fm, vmID)

	// back-date the deadline instead of waiting for it.
	rec, _ := fm.store.GetIntervention(context.Background(), in.ID)
	rec.Deadline = time.Now().Add(-time.Second)
	_ = fm.store.UpsertIntervention(context.Background(), rec)

	got, err := fm.WaitIntervention(context.Background(), in.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != hitl.StateExpired || got.Decision.Outcome != hitl.OutcomeApprove {
		t.Fatalf("expired ask = %+v / %+v", got, got.Decision)
	}
	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "yes", Reason: "", By: testActor}); !errors.Is(err, ErrInterventionResolved) {
		t.Fatalf("decide after expiry: err = %v", err)
	}
}

func TestShortCallTimeoutExpiresOnTime(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, err := fm.RequestHuman(context.Background(), vmID, HumanRequest{
		Kind: hitl.KindAsk, Ask: hitl.AskPayload{Question: "q"}, Timeout: hitl.MinTimeout, By: testActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := in.Deadline.Sub(in.CreatedAt); d != hitl.MinTimeout {
		t.Fatalf("call timeout did not shorten the deadline: %s", d)
	}
}

func TestDestroyCancelsPending(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, _ := ask(fm, vmID)
	if err := fm.DestroyVM(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	got, err := fm.GetIntervention(context.Background(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != hitl.StateCancelled || got.Decision.Outcome != hitl.OutcomeReject {
		t.Fatalf("after destroy: %+v", got)
	}
}

func TestPayloadSealedAtRest(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	fm, vmID := hitlFleet(t, FleetConfig{TokenEncryptionKey: key}, askSpec())
	in, _ := ask(fm, vmID)
	if _, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeApprove, Answer: "yes", Reason: "ship it", By: testActor}); err != nil {
		t.Fatal(err)
	}

	rec, _ := fm.store.GetIntervention(context.Background(), in.ID)
	if !rec.Sealed {
		t.Fatal("record not marked sealed")
	}
	for name, b := range map[string][]byte{"payload": rec.Payload, "result": rec.Result, "reason": rec.Reason} {
		if bytes.Contains(b, []byte("deploy")) || bytes.Contains(b, []byte("yes")) || bytes.Contains(b, []byte("ship")) {
			t.Fatalf("%s stored in the clear: %q", name, b)
		}
	}

	got, err := fm.GetIntervention(context.Background(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Payload.Question != "deploy to prod?" || got.Decision.Answer != "yes" || got.Decision.Reason != "ship it" {
		t.Fatalf("round trip lost content: %+v %+v", got.Payload, got.Decision)
	}
}

func TestListInterventionsPaginates(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	for i := 0; i < 3; i++ {
		if _, err := ask(fm, vmID); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := fm.ListInterventions(context.Background(), InterventionListFilter{EnvID: vmID, Limit: 2})
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("first page: %d items, next %q, err %v", len(page), next, err)
	}
	rest, next, err := fm.ListInterventions(context.Background(), InterventionListFilter{EnvID: vmID, Limit: 2, Cursor: next})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("second page: %d items, next %q, err %v", len(rest), next, err)
	}
	if page[1].ID >= rest[0].ID {
		t.Fatal("pages are not in id order")
	}
	none, _, _ := fm.ListInterventions(context.Background(), InterventionListFilter{State: hitl.StateApproved})
	if len(none) != 0 {
		t.Fatalf("state filter returned %d", len(none))
	}
}

func TestMigrateMovesPending(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	in, _ := ask(fm, vmID)
	fm.moveInterventions(context.Background(), vmID, "fuse-moved")
	got, _ := fm.GetIntervention(context.Background(), in.ID)
	if got.EnvID != "fuse-moved" || got.State != hitl.StatePending {
		t.Fatalf("after move: %+v", got)
	}
}

func TestInterventionEventPublished(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, askSpec())
	sub, cancel := fm.SubscribeEnvironmentEvents(vmID)
	defer cancel()
	in, _ := ask(fm, vmID)
	select {
	case ev := <-sub:
		if ev.Kind != "intervention" || ev.Intervention == nil || ev.Intervention.ID != in.ID || ev.State != VMStateRunning {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no intervention event")
	}
}
