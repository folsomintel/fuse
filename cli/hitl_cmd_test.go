package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pendingAsk = `{"id":"int_1","env_id":"fuse-a","kind":"ask","state":"pending",
	"payload":{"question":"deploy to prod?","choices":["yes","no"]},
	"triggered_by":{"id":"key:ak_1","source":"key"},
	"deadline":"2099-01-01T00:00:00Z","on_timeout":"reject"}`

func TestHitlListDefaultsToPending(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		fmt.Fprint(w, `{"interventions":[`+pendingAsk+`]}`)
	}))
	defer srv.Close()

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "list", "--env", "fuse-a"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(query, "state=pending") || !strings.Contains(query, "env=fuse-a") {
		t.Errorf("query = %q", query)
	}
	if !strings.Contains(out, "int_1") || !strings.Contains(out, "deploy to prod?") {
		t.Errorf("output:\n%s", out)
	}
}

func TestHitlListAllDropsStateFilter(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		fmt.Fprint(w, `{"interventions":[]}`)
	}))
	defer srv.Close()
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "list", "--state", "all"})
		return root.Execute()
	})
	if err != nil || strings.Contains(query, "state=") {
		t.Fatalf("query = %q, err = %v", query, err)
	}
}

func TestHitlShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/interventions/int_1" || r.URL.Query().Get("wait") != "" {
			t.Errorf("unexpected %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		fmt.Fprint(w, pendingAsk)
	}))
	defer srv.Close()
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "show", "int_1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"deploy to prod?", "yes, no", "key:ak_1", "then reject"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestHitlAnswerAndReject(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery+" "+strings.TrimSpace(string(body)))
		fmt.Fprint(w, `{"id":"int_1","state":"approved"}`)
	}))
	defer srv.Close()
	cfg := writeConfig(t, srv.URL)

	for _, args := range [][]string{
		{"hitl", "answer", "int_1", "ship", "it"},
		{"hitl", "reject", "int_1", "--reason", "not today"},
	} {
		if _, err := capture(t, func() error {
			root := newRootCmd()
			root.SetArgs(append([]string{"--config", cfg}, args...))
			return root.Execute()
		}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{
		`/v1/interventions/int_1?action=approve {"answer":"ship it"}`,
		`/v1/interventions/int_1?action=reject {"reason":"not today"}`,
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got, want[i])
		}
	}
}
