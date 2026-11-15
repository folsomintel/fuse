package fuse

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

func TestCreate_sendsIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"vm-1","state":"running"}`))
	})
	defer done()

	ctx := context.Background()
	if _, err := c.Environments.Create(ctx, CreateRequest{TaskID: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Environments.Create(ctx, CreateRequest{TaskID: "t"}, WithIdempotencyKey("mine")); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[1] != "mine" {
		t.Fatalf("keys = %q, want an auto key then \"mine\"", keys)
	}
}

func TestCreate_followsAcceptedOperation(t *testing.T) {
	polls := 0
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
			w.Header().Set("Location", "/v1/operations/op_1")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation_id":"op_1","operation":{"id":"op_1","kind":"create","state":"running","started_at":"2026-11-15T00:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op_1":
			if r.URL.Query().Get("wait") == "" {
				t.Errorf("poll without ?wait=")
			}
			polls++
			if polls == 1 {
				_, _ = w.Write([]byte(`{"id":"op_1","kind":"create","state":"running","step":"boot","started_at":"2026-11-15T00:00:00Z"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"op_1","kind":"create","state":"succeeded","started_at":"2026-11-15T00:00:00Z","finished_at":"2026-11-15T00:01:00Z","status_code":201,"result":{"id":"vm-1","state":"running"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer done()

	var accepted string
	c.t.onOperation = func(a *OperationAccepted) { accepted = a.OperationID }
	env, err := c.Environments.Create(context.Background(), CreateRequest{TaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if accepted != "op_1" {
		t.Errorf("onOperation got %q, want op_1", accepted)
	}
	if env.ID != "vm-1" || polls != 2 {
		t.Fatalf("env = %+v after %d polls, want vm-1 after 2", env, polls)
	}
}

func TestDestroy_followedFailureIsAPIError(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation_id":"op_1","operation":{"id":"op_1","kind":"destroy","state":"running","started_at":"2026-11-15T00:00:00Z"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"op_1","kind":"destroy","state":"failed","started_at":"2026-11-15T00:00:00Z","status_code":404,"error":{"code":"not_found","message":"vm not found"}}`))
	})
	defer done()

	err := c.Environments.Destroy(context.Background(), "vm-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "not_found" {
		t.Fatalf("err = %v, want a 404 not_found APIError", err)
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false", err)
	}
}

func TestOperations_getAndWait(t *testing.T) {
	polls := 0
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/op_1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"operation not found"}}`))
			return
		}
		polls++
		if polls < 3 {
			_, _ = w.Write([]byte(`{"id":"op_1","kind":"migrate","state":"running","step":"transfer","started_at":"2026-11-15T00:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"op_1","kind":"migrate","state":"succeeded","started_at":"2026-11-15T00:00:00Z","status_code":201,"result":{"id":"vm-2"}}`))
	})
	defer done()

	ctx := context.Background()
	op, err := c.Operations.Get(ctx, "op_1", GetOperationOptions{})
	if err != nil || op.State != OperationRunning || op.Step != "transfer" || op.Done() {
		t.Fatalf("get = %+v, %v; want running at transfer", op, err)
	}
	op, err = c.Operations.Wait(ctx, "op_1")
	if err != nil || op.State != OperationSucceeded || op.StatusCode != 201 {
		t.Fatalf("wait = %+v, %v; want succeeded 201", op, err)
	}
	if _, err := c.Operations.Get(ctx, "op_nope", GetOperationOptions{}); !IsNotFound(err) {
		t.Errorf("get unknown = %v, want not found", err)
	}
}

func TestFork_heldResponseStillSettles(t *testing.T) {
	// a 202 without operation_id is a hitl hold and keeps going down the
	// held path.
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"intervention_id":"in_1","intervention":{"id":"in_1","state":"pending"}}`))
		case r.URL.Path == "/v1/interventions/in_1":
			_, _ = w.Write([]byte(`{"id":"in_1","state":"approved","decision":{"outcome":"approve"},"result":{"env_id":"vm-2"}}`))
		case r.URL.Path == "/v1/environments/vm-2":
			_, _ = w.Write([]byte(`{"id":"vm-2","state":"running"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer done()

	env, err := c.Environments.Fork(context.Background(), "vm-1", ForkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "vm-2" {
		t.Fatalf("env = %+v, want vm-2", env)
	}
}
