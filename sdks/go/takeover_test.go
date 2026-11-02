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

const lockedBody = `{"error":{"code":"locked","message":"environment vm-1 is locked","details":{"intervention_id":"int_t","kind":"takeover"}}}`

func TestExecWaitsOutTakeoverLock(t *testing.T) {
	var execs, polls atomic.Int32
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/vm-1":
			if execs.Add(1) == 1 {
				w.WriteHeader(http.StatusLocked)
				io.WriteString(w, lockedBody)
				return
			}
			io.WriteString(w, `{"exit_code":0,"stdout":"ok\n","stderr":""}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interventions/int_t":
			if polls.Add(1) == 1 {
				io.WriteString(w, `{"id":"int_t","kind":"takeover","state":"pending"}`)
				return
			}
			io.WriteString(w, `{"id":"int_t","kind":"takeover","state":"approved","decision":{"outcome":"approve","answer":"done fixing","decided_by":{"id":"alice","source":"key"}}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()

	res, err := c.Environments.Exec(context.Background(), "vm-1", ExecRequest{Cmd: []string{"echo", "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "ok\n" || execs.Load() != 2 || polls.Load() != 2 {
		t.Fatalf("res = %+v (execs %d, polls %d), want a retry after the lock ended", res, execs.Load(), polls.Load())
	}
}

func TestExecNoWaitReturnsLockedError(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		io.WriteString(w, lockedBody)
	})
	defer cleanup()

	_, err := c.Environments.Exec(context.Background(), "vm-1", ExecRequest{Cmd: []string{"true"}, NoWait: true})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.InterventionID != "int_t" || locked.Kind != HitlKindTakeover || !IsLocked(err) {
		t.Fatalf("err = %v, want a LockedError for int_t", err)
	}
	if _, err := c.Environments.Computer(context.Background(), "vm-1", ComputerActionRequest{Action: "screenshot"}); err == nil {
		t.Fatal("computer during a takeover with waiting off: want an error")
	}
}

func TestSecretAndTakeoverCalls(t *testing.T) {
	var got []map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body == nil {
			body = map[string]any{}
		}
		body["_path"], body["_action"] = r.URL.Path, r.URL.Query().Get("action")
		got = append(got, body)
		switch r.URL.Query().Get("action") {
		case "request":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"int_s","kind":"secret","state":"pending","payload":{"secret":{"target":"GH_TOKEN","label":"GitHub token"}}}`)
		case "secret":
			io.WriteString(w, `{"id":"int_s","kind":"secret","state":"approved","decision":{"outcome":"approve","answer":"done","decided_by":{"id":"alice","source":"key"}}}`)
		case "takeover":
			io.WriteString(w, `{"id":"int_t","kind":"takeover","state":"pending","payload":{"takeover":{"via":"terminal","holder":{"id":"alice","source":"key"},"started_at":"2026-11-01T00:00:00Z"}}}`)
		case "release":
			io.WriteString(w, `{"id":"int_t","kind":"takeover","state":"approved","decision":{"outcome":"approve","answer":"fixed it","decided_by":{"id":"alice","source":"key"}}}`)
		}
	})
	defer cleanup()
	ctx := context.Background()

	in, err := c.Environments.RequestHuman(ctx, "vm-1", HumanRequest{Kind: HitlKindSecret, Target: "GH_TOKEN", Message: "need a token", NoWait: true})
	if err != nil || in.Payload.Secret == nil || in.Payload.Secret.Label != "GitHub token" {
		t.Fatalf("secret request = %+v, %v", in, err)
	}
	if _, ok := got[0]["question"]; ok || got[0]["kind"] != "secret" || got[0]["target"] != "GH_TOKEN" {
		t.Fatalf("secret request body = %v", got[0])
	}
	out, err := c.Interventions.AnswerSecret(ctx, "int_s", "ghp_x")
	if err != nil || out.Decision.Answer != SecretAnswer {
		t.Fatalf("answer = %+v, %v", out, err)
	}
	if got[1]["_path"] != "/v1/interventions/int_s" || got[1]["value"] != "ghp_x" {
		t.Fatalf("answer body = %v", got[1])
	}

	held, err := c.Environments.Takeover(ctx, "vm-1", TakeoverOptions{Reason: "fix login"})
	if err != nil || held.Payload.Takeover.Holder.ID != "alice" || held.Payload.Takeover.StartedAt == nil {
		t.Fatalf("takeover = %+v, %v", held, err)
	}
	rel, err := c.Environments.Release(ctx, "vm-1", "fixed it")
	if err != nil || rel.Decision.Answer != "fixed it" || got[3]["note"] != "fixed it" {
		t.Fatalf("release = %+v, %v (body %v)", rel, err, got[3])
	}
}
