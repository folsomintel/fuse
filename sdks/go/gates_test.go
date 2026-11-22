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

const heldExecBody = `{"intervention_id":"int_1","intervention":{"id":"int_1","env_id":"vm-1","kind":"exec","gate":"force-push","mode":"ask","state":"pending","payload":{"exec":{"argv":["git","push","--force"]}}}}`

func TestExecWaitsOnHeldIntervention(t *testing.T) {
	var polls atomic.Int32
	var onHeld string
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/vm-1":
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, heldExecBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interventions/int_1":
			switch polls.Add(1) {
			case 1:
				io.WriteString(w, `{"id":"int_1","kind":"exec","mode":"ask","state":"pending","payload":{"exec":{"argv":["git"]}}}`)
			case 2:
				// approved, but not run yet: not settled.
				io.WriteString(w, `{"id":"int_1","kind":"exec","mode":"ask","state":"approved","payload":{"exec":{"argv":["git"]}},"decision":{"outcome":"approve","edited_argv":["git","push","--force-with-lease"]}}`)
			default:
				io.WriteString(w, `{"id":"int_1","kind":"exec","mode":"ask","state":"approved","payload":{"exec":{"argv":["git"]}},"decision":{"outcome":"approve"},"result":{"exec":{"exit_code":0,"stdout":"pushed","stderr":""}}}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()

	res, err := c.Environments.Exec(context.Background(), "vm-1", ExecRequest{
		Cmd:    []string{"git", "push", "--force"},
		OnHeld: func(in *Intervention) { onHeld = in.ID },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "pushed" || onHeld != "int_1" || polls.Load() != 3 {
		t.Fatalf("res = %+v onHeld=%q polls=%d", res, onHeld, polls.Load())
	}
}

func TestExecNoWaitReturnsPending(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("NoWait should not poll: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, heldExecBody)
	})
	defer cleanup()

	_, err := c.Environments.Exec(context.Background(), "vm-1", ExecRequest{Cmd: []string{"git"}, NoWait: true})
	var pending *PendingInterventionError
	if !errors.As(err, &pending) || pending.Intervention.ID != "int_1" || pending.Intervention.Gate != "force-push" {
		t.Fatalf("err = %v", err)
	}
}

func TestHeldRejectedAndDenied(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"forbidden","message":"keep it","details":{"reason":"hitl_denied","gate":"no-destroy","intervention_id":"int_9"}}}`)
		case r.Method == http.MethodPost && r.URL.Query().Get("action") == "drain":
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"intervention_id":"int_2","intervention":{"id":"int_2","kind":"lifecycle","mode":"ask","state":"pending","payload":{"lifecycle":{"action":"drain"}}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interventions/int_2":
			io.WriteString(w, `{"id":"int_2","kind":"lifecycle","mode":"ask","state":"rejected","payload":{"lifecycle":{"action":"drain"}},"decision":{"outcome":"reject","reason":"not now"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()

	err := c.Environments.Destroy(context.Background(), "vm-1")
	if !IsHitlDenied(err) {
		t.Fatalf("destroy err = %v, want hitl_denied", err)
	}
	if apiErr, _ := AsAPIError(err); apiErr.Message != "keep it" || apiErr.Details["gate"] != "no-destroy" {
		t.Fatalf("deny = %+v", apiErr)
	}

	_, err = c.Environments.Drain(context.Background(), "vm-1")
	var rejected *InterventionRejectedError
	if !errors.As(err, &rejected) || rejected.Intervention.Decision.Reason != "not now" {
		t.Fatalf("drain err = %v", err)
	}
}

func TestHeldForkReturnsNewEnvironment(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Get("action") == "fork":
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"intervention_id":"int_3","intervention":{"id":"int_3","kind":"lifecycle","mode":"ask","state":"pending","payload":{"lifecycle":{"action":"fork"}}}}`)
		case r.URL.Path == "/v1/interventions/int_3":
			io.WriteString(w, `{"id":"int_3","kind":"lifecycle","mode":"ask","state":"approved","payload":{"lifecycle":{"action":"fork"}},"decision":{"outcome":"approve"},"result":{"env_id":"vm-2"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/vm-2":
			io.WriteString(w, `{"id":"vm-2","state":"running"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()

	envs, err := c.Environments.Fork(context.Background(), "vm-1", ForkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 || envs[0].ID != "vm-2" {
		t.Fatalf("envs = %+v", envs)
	}
}

func TestApproveWithSendsEditAndGrant(t *testing.T) {
	var body map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "approve" {
			t.Errorf("action = %q", r.URL.Query().Get("action"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		io.WriteString(w, `{"id":"int_1","state":"approved"}`)
	})
	defer cleanup()

	if _, err := c.Interventions.ApproveWith(context.Background(), "int_1", ApproveOptions{
		EditedArgv: []string{"git", "push"}, Grant: "ttl:30m",
	}); err != nil {
		t.Fatal(err)
	}
	argv, _ := body["edited_argv"].([]any)
	if len(argv) != 2 || body["grant"] != "ttl:30m" {
		t.Fatalf("body = %v", body)
	}
}

func TestForkSendsLiveAndCountAndReturnsChildren(t *testing.T) {
	var body map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "fork" {
			t.Errorf("action = %q", r.URL.Query().Get("action"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"children":[{"id":"vm-2","state":"running"},{"id":"vm-3","state":"running"}]}`)
	})
	defer cleanup()

	envs, err := c.Environments.Fork(context.Background(), "vm-1", ForkOptions{Live: true, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if body["live"] != true || body["count"] != float64(2) {
		t.Fatalf("body = %v, want live and count", body)
	}
	if len(envs) != 2 || envs[0].ID != "vm-2" || envs[1].ID != "vm-3" {
		t.Fatalf("envs = %+v", envs)
	}
	if _, err := c.Environments.Fork(context.Background(), "vm-1", ForkOptions{Count: -1}); err == nil {
		t.Fatal("negative count was sent")
	}
}
