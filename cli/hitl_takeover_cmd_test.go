package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pendingSecret = `{"id":"int_s","env_id":"fuse-a","kind":"secret","state":"pending",
	"payload":{"secret":{"target":"GH_TOKEN","label":"GitHub token for the deploy bot","message":"paste it here"}},
	"triggered_by":{"id":"key:ak_1","source":"key"},
	"deadline":"2099-01-01T00:00:00Z","on_timeout":"reject"}`

func TestHitlSecretReadsStdinAndNeverPrintsIt(t *testing.T) {
	const value = "ghp_supersecret"
	var sent map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interventions/int_s":
			fmt.Fprint(w, pendingSecret)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/interventions/int_s" && r.URL.Query().Get("action") == "secret":
			_ = json.NewDecoder(r.Body).Decode(&sent)
			fmt.Fprint(w, strings.Replace(pendingSecret, `"state":"pending"`, `"state":"approved","decision":{"outcome":"approve","answer":"done","decided_by":{"id":"master","source":"key"}}`, 1))
		default:
			t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()

	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "secret", "int_s"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sent["value"] != value {
		t.Fatalf("sent %v, want the stdin value without its newline", sent)
	}
	if strings.Contains(out, value) {
		t.Fatalf("value printed:\n%s", out)
	}
}

func TestHitlReleaseSendsNote(t *testing.T) {
	var body map[string]string
	var action string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action = r.URL.Query().Get("action")
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"id":"int_t","env_id":"fuse-a","kind":"takeover","state":"approved",
			"decision":{"outcome":"approve","answer":"fixed the login","decided_by":{"id":"master","source":"key"}}}`)
	}))
	defer srv.Close()
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "release", "fuse-a", "--note", "fixed the login"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if action != "release" || body["note"] != "fixed the login" {
		t.Fatalf("action %q body %v", action, body)
	}
	_ = out
}

func TestHitlTakeoverNoShellTakesLock(t *testing.T) {
	var action string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action = r.URL.Query().Get("action")
		fmt.Fprint(w, `{"id":"int_t","env_id":"fuse-a","kind":"takeover","state":"pending",
			"payload":{"takeover":{"via":"terminal","holder":{"id":"master","source":"key"}}},
			"deadline":"2099-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()
	_, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", writeConfig(t, srv.URL), "hitl", "takeover", "fuse-a", "--no-shell"})
		return root.Execute()
	})
	if err != nil || action != "takeover" {
		t.Fatalf("action %q err %v", action, err)
	}
}
