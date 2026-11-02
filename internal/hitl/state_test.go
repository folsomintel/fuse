package hitl

import (
	"errors"
	"testing"
	"time"
)

func pending() Intervention {
	return Intervention{
		ID:      "int_1",
		Kind:    KindAsk,
		State:   StatePending,
		Payload: AskPayload{Question: "deploy?", Choices: []string{"yes", "no"}},
	}
}

func TestTransition(t *testing.T) {
	for _, to := range []State{StateApproved, StateRejected, StateExpired, StateCancelled} {
		if err := Transition("x", StatePending, to); err != nil {
			t.Fatalf("pending -> %s: %v", to, err)
		}
		var resolved *ErrResolved
		if err := Transition("x", to, StateApproved); !errors.As(err, &resolved) {
			t.Fatalf("%s -> approved: err = %v, want ErrResolved", to, err)
		}
	}
	if err := Transition("x", StatePending, StatePending); err == nil {
		t.Fatal("pending -> pending must be rejected")
	}
}

func TestDecide(t *testing.T) {
	now := time.Now()
	by := Actor{ID: "master", Source: ActorKey}

	got, err := Decide(pending(), OutcomeApprove, "yes", "", by, now)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.State != StateApproved || got.Decision.Answer != "yes" || got.Decision.DecidedBy != by {
		t.Fatalf("approve produced %+v", got)
	}

	if _, err := Decide(pending(), OutcomeApprove, "maybe", "", by, now); err == nil {
		t.Fatal("approve with an answer outside choices must fail")
	}

	got, err = Decide(pending(), OutcomeReject, "ignored", "", by, now)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.State != StateRejected || got.Decision.Reason == "" || got.Decision.Answer != "" {
		t.Fatalf("reject must carry a reason and no answer, got %+v", got.Decision)
	}

	if _, err := Decide(got, OutcomeApprove, "yes", "", by, now); err == nil {
		t.Fatal("deciding a resolved intervention must fail")
	}
}

func TestExpireAppliesOnTimeout(t *testing.T) {
	now := time.Now()
	in := pending()
	got, err := Expire(in, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateExpired || got.Decision.Outcome != OutcomeReject {
		t.Fatalf("default on_timeout must reject, got %+v", got.Decision)
	}

	in.OnTimeout = OnTimeoutAllow
	got, _ = Expire(in, now)
	if got.Decision.Outcome != OutcomeApprove || got.Decision.DecidedBy != SystemActor {
		t.Fatalf("on_timeout allow must approve as system, got %+v", got.Decision)
	}
}

func TestCancel(t *testing.T) {
	got, err := Cancel(pending(), "environment destroyed", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCancelled || got.Decision.Outcome != OutcomeReject || got.Decision.Reason != "environment destroyed" {
		t.Fatalf("cancel produced %+v", got.Decision)
	}
}

func TestDeadlineTightenOnly(t *testing.T) {
	now := time.Unix(0, 0)
	spec := Spec{TimeoutSeconds: 60}
	if d := Deadline(now, spec, 0); d != now.Add(time.Minute) {
		t.Fatalf("default deadline = %s", d)
	}
	if d := Deadline(now, spec, 30*time.Second); d != now.Add(30*time.Second) {
		t.Fatalf("shorter call timeout must win, got %s", d)
	}
	if d := Deadline(now, spec, time.Hour); d != now.Add(time.Minute) {
		t.Fatalf("longer call timeout must not extend, got %s", d)
	}
	if !Expired(now.Add(time.Minute), now.Add(time.Minute)) || Expired(now, now.Add(time.Second)) {
		t.Fatal("Expired boundary is wrong")
	}
}
