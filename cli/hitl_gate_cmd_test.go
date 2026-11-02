package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestHitlApproveWithEditAfterDash(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "approve" {
			t.Errorf("action = %q", r.URL.Query().Get("action"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		fmt.Fprint(w, `{"id":"int_1","state":"approved","decision":{"outcome":"approve","edited_argv":["git","push","--force-with-lease"],"grant":"once"}}`)
	}))
	defer srv.Close()

	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "approve", "int_1", "--edit", "--", "git", "push", "--force-with-lease"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var argv []string
	for _, a := range body["edited_argv"].([]any) {
		argv = append(argv, a.(string))
	}
	if !slices.Equal(argv, []string{"git", "push", "--force-with-lease"}) || body["answer"] != nil {
		t.Fatalf("body = %v", body)
	}
}

func TestHitlApproveGrantAndAskAnswer(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		fmt.Fprint(w, `{"id":"int_1","state":"approved"}`)
	}))
	defer srv.Close()

	for _, args := range [][]string{
		{"hitl", "approve", "int_1", "--grant", "env"},
		{"hitl", "approve", "int_1", "yes"},
	} {
		if _, err := capture(t, func() error {
			root := newRootCmd()
			root.SetArgs(append([]string{"--config", writeConfig(t, srv.URL)}, args...))
			return root.Execute()
		}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if bodies[0]["grant"] != "env" || bodies[1]["answer"] != "yes" {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestHitlApproveArgvNeedsEdit(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--config", writeConfig(t, "http://127.0.0.1:1"), "hitl", "approve", "int_1", "--", "ls"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "needs --edit") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderArgvQuotes(t *testing.T) {
	if got := renderArgv([]string{"sh", "-lc", "git push --force"}); got != `sh -lc "git push --force"` {
		t.Fatalf("renderArgv = %q", got)
	}
}
