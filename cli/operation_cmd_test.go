package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnvMigratePrintsOperationAndFollowsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			if r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("migrate sent no Idempotency-Key")
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"operation_id":"op_1","operation":{"id":"op_1","kind":"migrate","state":"running","started_at":"2026-11-15T00:00:00Z"}}`)
		case r.URL.Path == "/v1/operations/op_1":
			fmt.Fprint(w, `{"id":"op_1","kind":"migrate","state":"succeeded","started_at":"2026-11-15T00:00:00Z","status_code":201,"result":{"id":"fuse-migrate-1","state":"running"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	path := writeConfig(t, srv.URL)
	t.Cleanup(func() { app.output = outputTable })
	out, errOut, err := captureBoth(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", path, "-o", "json", "environment", "migrate", "vm-1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(errOut, "operation op_1") || !strings.Contains(errOut, "fuse operation wait op_1") {
		t.Errorf("stderr does not name the operation:\n%s", errOut)
	}
	if !strings.Contains(out, "fuse-migrate-1") {
		t.Errorf("stdout is missing the migrated environment:\n%s", out)
	}
}

func TestOperationWaitFailedExitsNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"op_1","kind":"destroy","state":"failed","started_at":"2026-11-15T00:00:00Z","status_code":404,"error":{"code":"not_found","message":"vm not found"}}`)
	}))
	defer srv.Close()

	path := writeConfig(t, srv.URL)
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", path, "operation", "wait", "op_1"})
		return root.Execute()
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not found error", err)
	}
	if !strings.Contains(out, "op_1") || !strings.Contains(out, "404") {
		t.Errorf("output missing the operation:\n%s", out)
	}
}

func TestOperationShowJSON(t *testing.T) {
	var gotWait string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWait = r.URL.Query().Get("wait")
		fmt.Fprint(w, `{"id":"op_1","kind":"migrate","state":"running","step":"transfer","started_at":"2026-11-15T00:00:00Z"}`)
	}))
	defer srv.Close()

	path := writeConfig(t, srv.URL)
	// -o json lands on the global app state; put it back for later tests.
	t.Cleanup(func() { app.output = outputTable })
	out, err := capture(t, func() error {
		root := newRootCmd()
		root.SetArgs([]string{"--config", path, "-o", "json", "operation", "show", "op_1"})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotWait != "" {
		t.Errorf("show sent wait=%q, want none", gotWait)
	}
	if !strings.Contains(out, `"step": "transfer"`) && !strings.Contains(out, `"step":"transfer"`) {
		t.Errorf("output missing step:\n%s", out)
	}
}
