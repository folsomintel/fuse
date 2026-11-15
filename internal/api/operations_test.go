package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// slowProvider is a fakeProvider whose Create waits for release, standing in
// for a create or migrate that outlasts the attach bound.
type slowProvider struct {
	*fakeProvider
	release chan struct{}
}

func (p *slowProvider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.fakeProvider.Create(ctx, spec)
}

func newSlowTestHandler(t *testing.T) (*Handler, *slowProvider) {
	t.Helper()
	p := &slowProvider{fakeProvider: newFakeProvider(), release: make(chan struct{})}
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{Provider: p, Prefix: "fuse-"})
	return &Handler{Fleet: fm}, p
}

// doKeyed serves a JSON request carrying an Idempotency-Key.
func doKeyed(t *testing.T, h http.Handler, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(HeaderIdempotencyKey, key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func createBody(t *testing.T, task string) CreateEnvironmentRequest {
	return CreateEnvironmentRequest{TaskID: task, ManifestInline: encodeManifest(t)}
}

func TestOperation_sameKeyReturnsSameEnvironment(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	first := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-1"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	second := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-1"))
	if second.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201 replayed. body %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("retry body differs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if got := second.Header().Get(HeaderIdempotentReplayed); got != "true" {
		t.Errorf("%s = %q, want true", HeaderIdempotentReplayed, got)
	}
	id := first.Header().Get(HeaderOperationID)
	if id == "" || second.Header().Get(HeaderOperationID) != id {
		t.Errorf("operation ids %q and %q, want the same non-empty id", id, second.Header().Get(HeaderOperationID))
	}

	// without a key the duplicate runs again and hits the task conflict.
	if rr := doKeyed(t, r, http.MethodPost, "/v1/environments", "", createBody(t, "task-1")); rr.Code != http.StatusConflict {
		t.Errorf("unkeyed duplicate status = %d, want 409", rr.Code)
	}
}

func TestOperation_keyReusedForDifferentRequest(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-1")); rr.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rr.Code)
	}
	rr := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-2"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Details["reason"] != "idempotency_key_reused" {
		t.Errorf("reason = %q, want idempotency_key_reused", env.Error.Details["reason"])
	}
}

func TestOperation_invalidKey(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	rr := doKeyed(t, r, http.MethodPost, "/v1/environments", strings.Repeat("a", 256), createBody(t, "task-1"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestOperation_waitFalseReturnsHandle(t *testing.T) {
	h, p := newSlowTestHandler(t)
	r := mustRouter(t, h)

	rr := doKeyed(t, r, http.MethodPost, "/v1/environments?wait=false", "k-1", createBody(t, "task-1"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body %s", rr.Code, rr.Body.String())
	}
	var acc OperationAccepted
	if err := json.NewDecoder(rr.Body).Decode(&acc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if acc.OperationID == "" || acc.Operation.State != OpRunning || acc.Operation.Kind != OpCreate {
		t.Fatalf("accepted = %+v, want a running create", acc)
	}
	if loc := rr.Header().Get("Location"); loc != "/v1/operations/"+acc.OperationID {
		t.Errorf("Location = %q", loc)
	}

	running := doJSON(t, r, http.MethodGet, "/v1/operations/"+acc.OperationID, nil)
	var op Operation
	_ = json.NewDecoder(running.Body).Decode(&op)
	if running.Code != http.StatusOK || op.State != OpRunning {
		t.Fatalf("get while running = %d %+v", running.Code, op)
	}

	close(p.release)
	done := doJSON(t, r, http.MethodGet, "/v1/operations/"+acc.OperationID+"?wait=5s", nil)
	op = Operation{}
	if err := json.NewDecoder(done.Body).Decode(&op); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if op.State != OpSucceeded || op.StatusCode != http.StatusCreated || op.FinishedAt == nil {
		t.Fatalf("operation = %+v, want succeeded 201", op)
	}
	var env Environment
	if err := json.Unmarshal(op.Result, &env); err != nil || env.ID != "fuse-task-1" {
		t.Errorf("result = %s, want environment fuse-task-1", op.Result)
	}
}

func TestOperation_boundHitReturnsHandleAndRetryAttaches(t *testing.T) {
	h, p := newSlowTestHandler(t)
	h.OperationWait = 50 * time.Millisecond
	r := mustRouter(t, h)

	rr := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-1"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 once the bound is hit", rr.Code)
	}
	id := rr.Header().Get(HeaderOperationID)

	// a retry with the same key attaches to the running operation instead of
	// starting a second create.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(p.release)
	}()
	h.OperationWait = 5 * time.Second
	retry := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-1"))
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201. body %s", retry.Code, retry.Body.String())
	}
	if retry.Header().Get(HeaderOperationID) != id {
		t.Errorf("retry ran as %q, want the first operation %q", retry.Header().Get(HeaderOperationID), id)
	}
}

func TestOperation_failedCarriesError(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doKeyed(t, r, http.MethodDelete, "/v1/environments/fuse-missing?wait=false", "k-1", nil)
	id := rr.Header().Get(HeaderOperationID)
	if id == "" {
		t.Fatalf("no operation id on %d", rr.Code)
	}
	got := doJSON(t, r, http.MethodGet, "/v1/operations/"+id+"?wait=5s", nil)
	var op Operation
	if err := json.NewDecoder(got.Body).Decode(&op); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if op.Kind != OpDestroy || op.State != OpFailed || op.StatusCode != http.StatusNotFound {
		t.Fatalf("operation = %+v, want a failed 404 destroy", op)
	}
	if op.Error == nil || op.Error.Code != CodeNotFound {
		t.Errorf("error = %+v, want not_found", op.Error)
	}
}

func TestOperation_otherKeyCannotRead(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doKeyed(t, r, http.MethodPost, "/v1/environments", "k-1", createBody(t, "task-1"))
	id := rr.Header().Get(HeaderOperationID)

	req := httptest.NewRequest(http.MethodGet, "/v1/operations/"+id, nil)
	req = req.WithContext(withPrincipal(req.Context(), Principal{KeyID: "other"}))
	got := httptest.NewRecorder()
	r.ServeHTTP(got, req)
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a key that did not start it", got.Code)
	}
}

func TestOperation_unknown(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	if rr := doJSON(t, r, http.MethodGet, "/v1/operations/op_nope", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestOperation_keyForwardedToFleet(t *testing.T) {
	h := &Handler{}
	var gotKey string
	next := func(w http.ResponseWriter, r *http.Request) {
		gotKey = orchestrator.IdempotencyKey(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/environments/x", nil)
	req.Header.Set(HeaderIdempotencyKey, "k-1")
	rr := httptest.NewRecorder()
	h.asOperation(OpDestroy, MaxJSONBodyBytes, next)(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}
	if gotKey != "master:destroy:k-1" {
		t.Errorf("forwarded key = %q, want master:destroy:k-1", gotKey)
	}
}

func TestOperationStore_refusesNewWorkAtCapacity(t *testing.T) {
	s := newOperationStore()
	for i := 0; i < maxOperations; i++ {
		op := &operation{id: newOperationID(), done: make(chan struct{})}
		s.byID[op.id] = op
	}
	op := &operation{id: newOperationID(), done: make(chan struct{})}
	if _, err := s.begin(op); err != errOperationsFull {
		t.Fatalf("begin at capacity: err = %v, want errOperationsFull", err)
	}
}

func TestOperationStore_getDropsExpired(t *testing.T) {
	s := newOperationStore()
	op := &operation{id: newOperationID(), done: make(chan struct{})}
	close(op.done)
	op.finishedAt = time.Now().Add(-operationRetention - time.Minute)
	s.byID[op.id] = op
	if _, ok := s.get(op.id); ok {
		t.Fatal("get returned an operation past its retention")
	}
	if _, ok := s.byID[op.id]; ok {
		t.Fatal("expired operation still in the store")
	}
}
