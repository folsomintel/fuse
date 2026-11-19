package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/orchestrator/orchestratortest"
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
	fm, _ := orchestratortest.NewFleet(orchestrator.FleetConfig{Provider: p, Prefix: "fuse-"})
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

// keyedRoute is a route run as an operation that records the restate key
// each run saw and answers with the next status in statuses (the last one
// repeats).
type keyedRoute struct {
	mu       sync.Mutex
	keys     []string
	statuses []int
}

func (k *keyedRoute) serve(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	k.keys = append(k.keys, orchestrator.IdempotencyKey(r.Context()))
	status := k.statuses[0]
	if len(k.statuses) > 1 {
		k.statuses = k.statuses[1:]
	}
	k.mu.Unlock()
	if status >= 400 {
		writeError(w, status, CodeUnavailable, "refused", nil)
		return
	}
	writeJSON(w, status, map[string]string{"ok": "yes"})
}

func (k *keyedRoute) seen() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

func callKeyed(h http.HandlerFunc, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/environments", strings.NewReader(body))
	req.Header.Set(HeaderIdempotencyKey, key)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// TestOperation_5xxRetryRunsUnderANewRestateKey: a keyed call that answered
// 5xx runs again on retry under the next generation of its restate key, so
// restate starts a new invocation instead of replaying the failed one. the
// run that succeeds is then replayed, and a different body is still a 409.
func TestOperation_5xxRetryRunsUnderANewRestateKey(t *testing.T) {
	h := &Handler{}
	route := &keyedRoute{statuses: []int{http.StatusInsufficientStorage, http.StatusServiceUnavailable, http.StatusCreated}}
	serve := h.asOperation(OpCreate, MaxJSONBodyBytes, route.serve)

	for i, want := range []int{http.StatusInsufficientStorage, http.StatusServiceUnavailable, http.StatusCreated} {
		if rr := callKeyed(serve, "k-1", `{"a":1}`); rr.Code != want {
			t.Fatalf("call %d: status = %d, want %d", i, rr.Code, want)
		}
	}
	replay := callKeyed(serve, "k-1", `{"a":1}`)
	if replay.Code != http.StatusCreated || replay.Header().Get(HeaderIdempotentReplayed) != "true" {
		t.Fatalf("after success: status = %d replayed %q, want a replayed 201", replay.Code, replay.Header().Get(HeaderIdempotentReplayed))
	}
	want := []string{"master:create:k-1", "master:create#1:k-1", "master:create#2:k-1"}
	if got := route.seen(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("restate keys = %q, want %q", got, want)
	}

	rr := callKeyed(serve, "k-1", `{"a":2}`)
	if rr.Code != http.StatusConflict || decodeError(t, rr.Body).Error.Details["reason"] != "idempotency_key_reused" {
		t.Fatalf("different body: status = %d, want 409 idempotency_key_reused", rr.Code)
	}
	if n := len(route.seen()); n != 3 {
		t.Fatalf("handler ran %d times, want 3", n)
	}
}

// TestOperation_4xxRetryReplays: a keyed call that answered 4xx is kept, so
// its retry replays the answer instead of running again.
func TestOperation_4xxRetryReplays(t *testing.T) {
	h := &Handler{}
	route := &keyedRoute{statuses: []int{http.StatusNotFound, http.StatusCreated}}
	serve := h.asOperation(OpDestroy, MaxJSONBodyBytes, route.serve)

	callKeyed(serve, "k-1", `{}`)
	rr := callKeyed(serve, "k-1", `{}`)
	if rr.Code != http.StatusNotFound || rr.Header().Get(HeaderIdempotentReplayed) != "true" {
		t.Fatalf("retry: status = %d replayed %q, want a replayed 404", rr.Code, rr.Header().Get(HeaderIdempotentReplayed))
	}
	if got := route.seen(); len(got) != 1 {
		t.Fatalf("handler ran %d times, want 1", len(got))
	}
}

// TestOperation_5xxAfterItsWaitEndedKeepsTheKey: a run that gave up because
// its own timeout passed may still be running on restate, so its retry
// keeps the restate key and attaches instead of starting a second run.
func TestOperation_5xxAfterItsWaitEndedKeepsTheKey(t *testing.T) {
	h := &Handler{OperationTimeout: 20 * time.Millisecond}
	var mu sync.Mutex
	var keys []string
	serve := h.asOperation(OpFork, MaxJSONBodyBytes, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, orchestrator.IdempotencyKey(r.Context()))
		first := len(keys) == 1
		mu.Unlock()
		if first {
			<-r.Context().Done()
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, "gave up", nil)
	})

	for range 3 {
		if rr := callKeyed(serve, "k-1", `{}`); rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rr.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"master:fork:k-1", "master:fork:k-1", "master:fork#1:k-1"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("restate keys = %q, want %q", keys, want)
	}
}

// TestOperationStore_fullKeepsTheFailedGeneration: a retry refused at
// capacity leaves the 5xx it would replace, so the next retry still moves
// past the failed generation.
func TestOperationStore_fullKeepsTheFailedGeneration(t *testing.T) {
	s := newOperationStore()
	failed := &operation{id: newOperationID(), owner: "master", key: "k-1", gen: 1, done: make(chan struct{}),
		finishedAt: time.Now(), rec: &opRecorder{status: http.StatusInternalServerError}}
	close(failed.done)
	s.byID[failed.id] = failed
	s.byKey["master\x00k-1"] = failed
	for len(s.byID) < maxOperations+1 {
		op := &operation{id: newOperationID(), done: make(chan struct{})}
		s.byID[op.id] = op
	}
	retry := &operation{id: newOperationID(), owner: "master", key: "k-1", done: make(chan struct{})}
	if _, err := s.begin(retry); err != errOperationsFull {
		t.Fatalf("begin over capacity: err = %v, want errOperationsFull", err)
	}
	if s.byKey["master\x00k-1"] != failed {
		t.Fatal("a refused retry dropped the failed operation")
	}

	// one slot frees: the retry replaces the failed run at the next generation.
	for id, op := range s.byID {
		if op != failed {
			delete(s.byID, id)
			break
		}
	}
	if _, err := s.begin(retry); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if retry.gen != 2 || s.byKey["master\x00k-1"] != retry {
		t.Fatalf("retry gen = %d, want 2 and registered under the key", retry.gen)
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

func TestOperation_waitOperationsBlocksUntilDone(t *testing.T) {
	h, p := newSlowTestHandler(t)
	router, err := h.Router()
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	rr := doKeyed(t, router, http.MethodPost, "/v1/environments?wait=false", "k-shutdown", createBody(t, "shutdown"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create: status %d, want 202", rr.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.WaitOperations(ctx); err == nil {
		t.Fatal("WaitOperations returned while the create was still running")
	}
	close(p.release)
	if err := h.WaitOperations(context.Background()); err != nil {
		t.Fatalf("WaitOperations after release: %v", err)
	}
}
