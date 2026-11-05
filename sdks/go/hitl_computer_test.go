package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
)

const heldComputerBody = `{"intervention_id":"int_c","intervention":{"id":"int_c","env_id":"vm-1","kind":"computer","gate":"typing","mode":"ask","state":"pending","payload":{"computer":{"action":"type","text":"rm -rf ~","input":{"action":"type","text":"rm -rf ~"}}}}}`

func TestComputerWaitsOnHeldActionAndReturnsTheResult(t *testing.T) {
	var polls atomic.Int32
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/vm-1/computer":
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, heldComputerBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interventions/int_c":
			if polls.Add(1) == 1 {
				io.WriteString(w, `{"id":"int_c","kind":"computer","mode":"ask","state":"approved","payload":{"computer":{"action":"type"}},"decision":{"outcome":"approve","edited_text":"ls ~"}}`)
				return
			}
			io.WriteString(w, `{"id":"int_c","kind":"computer","mode":"ask","state":"approved","payload":{"computer":{"action":"type"}},"decision":{"outcome":"approve","edited_text":"ls ~"},"result":{"computer":{"screenshot":"UE5H"}}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()

	var held string
	res, err := c.Environments.Computer(context.Background(), "vm-1", ComputerActionRequest{
		Action: "type", Text: "rm -rf ~", OnHeld: func(in *Intervention) { held = in.Payload.Computer.Text },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Screenshot != "UE5H" || held != "rm -rf ~" || polls.Load() != 2 {
		t.Fatalf("res = %+v held=%q polls=%d", res, held, polls.Load())
	}
}

func TestComputerNoWaitAndRejection(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, heldComputerBody)
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"id":"int_c","kind":"computer","mode":"ask","state":"rejected","payload":{"computer":{"action":"type"}},"decision":{"outcome":"reject","reason":"not that"}}`)
		}
	})
	defer cleanup()

	_, err := c.Environments.Computer(context.Background(), "vm-1", ComputerActionRequest{Action: "type", Text: "x", NoWait: true})
	var pending *PendingInterventionError
	if !errors.As(err, &pending) || pending.Intervention.Kind != HitlKindComputer {
		t.Fatalf("NoWait: err = %v", err)
	}
	_, err = c.Environments.Computer(context.Background(), "vm-1", ComputerActionRequest{Action: "type", Text: "x"})
	var rejected *InterventionRejectedError
	if !errors.As(err, &rejected) || rejected.Intervention.Decision.Reason != "not that" {
		t.Fatalf("rejected: err = %v", err)
	}
	// the tool helper hands a rejection to the model instead of failing.
	out, err := c.Environments.ComputerToolResult(context.Background(), "vm-1", json.RawMessage(`{"action":"type","text":"x"}`))
	if err != nil || !out.IsError {
		t.Fatalf("tool result = %+v, %v", out, err)
	}
}

func TestRequestReviewReturnsComments(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Get("action") == "request":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["kind"] != "review" || body["content"] != "diff --git a b" || body["format"] != "diff" {
				t.Errorf("request body = %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"int_r","kind":"review","state":"pending","payload":{"review":{"content":"diff --git a b","format":"diff"}}}`)
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"id":"int_r","kind":"review","state":"rejected","payload":{"review":{"content":"diff --git a b","format":"diff"}},"decision":{"outcome":"reject","reason":"hunk 2 drops a check"}}`)
		}
	})
	defer cleanup()

	res, err := c.Environments.RequestReview(context.Background(), "vm-1", ReviewRequest{Content: "diff --git a b", Format: ReviewFormatDiff})
	if err != nil {
		t.Fatal(err)
	}
	if res.Approved || res.Comments != "hunk 2 drops a check" || res.Intervention.Payload.Review.Format != "diff" {
		t.Fatalf("review = %+v", res)
	}
}

func TestDeciderAllowIsSettled(t *testing.T) {
	in := &Intervention{Kind: HitlKindComputer, Mode: GateModeAsk, State: InterventionApproved,
		Payload:  InterventionPayload{Computer: &ComputerPayload{Action: "type"}},
		Decision: &InterventionDecision{Outcome: OutcomeApprove, DecidedBy: Actor{ID: "decider:typing", Source: ActorSourceDecider}}}
	if !in.Settled() {
		t.Fatal("a decider allow has no result to wait for")
	}
	in.Decision.DecidedBy = Actor{ID: "alice", Source: "key"}
	if in.Settled() {
		t.Fatal("a human approval of a computer action waits for its result")
	}
}

func TestApproveSendsEditedText(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Query().Get("action") != "approve" || body["edited_text"] != "ls" {
			t.Errorf("approve body = %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"int_c","state":"approved","decision":{"outcome":"approve","edited_text":"ls"}}`)
	})
	defer cleanup()
	text := "ls"
	in, err := c.Interventions.ApproveWith(context.Background(), "int_c", ApproveOptions{EditedText: &text})
	if err != nil || in.Decision.EditedText == nil || *in.Decision.EditedText != "ls" {
		t.Fatalf("approve = %+v, %v", in, err)
	}
}
