package fuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRequestHumanWaitsForDecision(t *testing.T) {
	var polls atomic.Int32
	var raised map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/vm-1":
			if r.URL.Query().Get("action") != "request" {
				t.Errorf("action = %q", r.URL.Query().Get("action"))
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &raised)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"int_1","env_id":"vm-1","kind":"ask","state":"pending","payload":{"question":"deploy?"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interventions/int_1":
			if r.URL.Query().Get("wait") == "" {
				t.Error("wait was not long-polling")
			}
			// pending on the first poll, decided on the second.
			if polls.Add(1) == 1 {
				io.WriteString(w, `{"id":"int_1","state":"pending"}`)
				return
			}
			io.WriteString(w, `{"id":"int_1","state":"approved","decision":{"outcome":"approve","answer":"yes","decided_by":{"id":"master","source":"key"}}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()

	in, err := c.Environments.RequestHuman(context.Background(), "vm-1", HumanRequest{
		Question: "deploy?", Choices: []string{"yes", "no"}, TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if raised["kind"] != "ask" || raised["question"] != "deploy?" || raised["timeout_seconds"] != float64(60) {
		t.Fatalf("request body = %v", raised)
	}
	if _, ok := raised["NoWait"]; ok {
		t.Fatal("NoWait leaked onto the wire")
	}
	if !in.Terminal() || !in.Approved() || in.Decision.Answer != "yes" || polls.Load() != 2 {
		t.Fatalf("result = %+v (polls %d)", in, polls.Load())
	}
}

func TestRequestHumanNoWait(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("NoWait still polled: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"int_1","state":"pending"}`)
	})
	defer cleanup()
	in, err := c.Environments.RequestHuman(context.Background(), "vm-1", HumanRequest{Question: "q", NoWait: true})
	if err != nil || in.State != InterventionPending || in.Terminal() {
		t.Fatalf("got %+v, %v", in, err)
	}
}

func TestInterventionsDecideAndList(t *testing.T) {
	var got []string
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+strings.TrimSpace(string(body)))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("cursor") == "" {
				io.WriteString(w, `{"interventions":[{"id":"int_1"}],"next_cursor":"c1"}`)
				return
			}
			io.WriteString(w, `{"interventions":[{"id":"int_2"}]}`)
			return
		}
		io.WriteString(w, `{"id":"int_1","state":"rejected","decision":{"outcome":"reject","reason":"no"}}`)
	})
	defer cleanup()

	if _, err := c.Interventions.Approve(context.Background(), "int_1", "yes", ""); err != nil {
		t.Fatal(err)
	}
	in, err := c.Interventions.Reject(context.Background(), "int_1", "no")
	if err != nil || in.Approved() {
		t.Fatalf("reject = %+v, %v", in, err)
	}
	all, err := c.Interventions.List(context.Background(), ListInterventionsOptions{EnvID: "vm-1", State: InterventionPending})
	if err != nil || len(all) != 2 {
		t.Fatalf("list = %+v, %v", all, err)
	}

	want := []string{
		`POST /v1/interventions/int_1?action=approve {"answer":"yes"}`,
		`POST /v1/interventions/int_1?action=reject {"reason":"no"}`,
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("request %d = %q, want %q", i, got[i], w)
		}
	}
	if !strings.Contains(got[2], "env=vm-1") || !strings.Contains(got[2], "state=pending") {
		t.Errorf("list query = %q", got[2])
	}
}

func TestCreateCarriesHitl(t *testing.T) {
	var body map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"id":"vm-1","state":"running"}`)
	})
	defer cleanup()
	_, err := c.Environments.Create(context.Background(), CreateRequest{
		TaskID: "t",
		Hitl:   &HitlSpec{TimeoutSeconds: 120, Requests: HitlRequests{Ask: &HitlAskRequests{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hitl, _ := body["hitl"].(map[string]any)
	if hitl == nil || hitl["timeout_seconds"] != float64(120) || hitl["requests"].(map[string]any)["ask"] == nil {
		t.Fatalf("hitl on the wire = %v", body["hitl"])
	}
}
