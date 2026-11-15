package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// HeaderIdempotencyKey names the request header that dedupes retried
// mutating calls. the same key with the same request returns the same
// operation; the same key with a different request is a 409.
const HeaderIdempotencyKey = "Idempotency-Key"

// HeaderOperationID carries the id of the operation a mutating route ran as,
// on every answer it gives.
const HeaderOperationID = "Fuse-Operation-Id"

// HeaderIdempotentReplayed is "true" on an answer replayed from an operation
// an earlier request with the same key already finished.
const HeaderIdempotentReplayed = "Idempotent-Replayed"

// operation kinds, one per mutating route.
const (
	OpCreate   = "create"
	OpDestroy  = "destroy"
	OpFork     = "fork"
	OpMigrate  = "migrate"
	OpSnapshot = "snapshot"
	OpRestore  = "restore"
	OpDrain    = "drain"
)

// operation states.
const (
	OpRunning   = "running"
	OpSucceeded = "succeeded"
	OpFailed    = "failed"
)

const (
	// maxIdempotencyKeyLen bounds a caller's key.
	maxIdempotencyKeyLen = 255
	// operationRetention is how long a finished operation, and the key that
	// names it, stays readable. it matches the host agents' key store.
	operationRetention = 24 * time.Hour
	// maxOperations caps the registry. past it the oldest finished
	// operations go first.
	maxOperations = 10000
	// defaultOperationWait is the attach bound when Handler.OperationWait is
	// unset. it stays below the server's default 60s write timeout.
	defaultOperationWait = 50 * time.Second
	// defaultOperationTimeout bounds an operation that no longer has a
	// caller waiting on it.
	defaultOperationTimeout = 2 * time.Hour
)

// operation is one run of a mutating route. the handler writes into rec off
// the request's goroutine, so a caller that disconnects or times out does not
// stop it.
type operation struct {
	id          string
	kind        string
	owner       string
	key         string
	fingerprint string
	startedAt   time.Time
	done        chan struct{}

	mu         sync.Mutex
	step       string
	finishedAt time.Time
	rec        *opRecorder
}

func (op *operation) setStep(step string) {
	op.mu.Lock()
	op.step = step
	op.mu.Unlock()
}

// finished reports whether the handler returned.
func (op *operation) finished() bool {
	select {
	case <-op.done:
		return true
	default:
		return false
	}
}

// view is the operation as the api returns it.
func (op *operation) view() Operation {
	op.mu.Lock()
	defer op.mu.Unlock()
	out := Operation{
		ID:        op.id,
		Kind:      op.kind,
		State:     OpRunning,
		Step:      op.step,
		StartedAt: op.startedAt,
	}
	if !op.finished() {
		return out
	}
	finished := op.finishedAt
	out.FinishedAt = &finished
	out.StatusCode = op.rec.status
	body := bytes.TrimSpace(op.rec.body.Bytes())
	if op.rec.status/100 == 2 {
		out.State = OpSucceeded
		if len(body) > 0 && json.Valid(body) {
			out.Result = json.RawMessage(body)
		}
		return out
	}
	out.State = OpFailed
	var env Error
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		out.Error = &env.Error
	} else {
		out.Error = &ErrorBody{Code: CodeInternal, Message: http.StatusText(op.rec.status)}
	}
	return out
}

// opRecorder is the ResponseWriter an operation's handler writes into.
type opRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *opRecorder) Header() http.Header { return r.header }

func (r *opRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *opRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// operationStore holds operations by id and by caller key.
type operationStore struct {
	mu    sync.Mutex
	byID  map[string]*operation
	byKey map[string]*operation
}

func newOperationStore() *operationStore {
	return &operationStore{byID: map[string]*operation{}, byKey: map[string]*operation{}}
}

// begin returns the live operation owner's key names, or registers op. a
// finished operation whose answer was a 5xx is dropped so a retry runs again.
func (s *operationStore) begin(op *operation) (existing *operation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(time.Now())
	if op.key != "" {
		scoped := op.owner + "\x00" + op.key
		if prev, ok := s.byKey[scoped]; ok {
			if !prev.finished() || prev.rec.status < 500 {
				return prev
			}
			s.dropLocked(prev)
		}
		s.byKey[scoped] = op
	}
	s.byID[op.id] = op
	return nil
}

func (s *operationStore) get(id string) (*operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.byID[id]
	return op, ok
}

func (s *operationStore) dropLocked(op *operation) {
	delete(s.byID, op.id)
	if op.key != "" {
		scoped := op.owner + "\x00" + op.key
		if s.byKey[scoped] == op {
			delete(s.byKey, scoped)
		}
	}
}

// sweepLocked drops operations past their retention, then the oldest
// finished ones while the store is over its cap.
func (s *operationStore) sweepLocked(now time.Time) {
	var oldest *operation
	var oldestAt time.Time
	for _, op := range s.byID {
		if !op.finished() {
			continue
		}
		op.mu.Lock()
		at := op.finishedAt
		op.mu.Unlock()
		if now.Sub(at) > operationRetention {
			s.dropLocked(op)
			continue
		}
		if len(s.byID) >= maxOperations && (oldest == nil || at.Before(oldestAt)) {
			oldest, oldestAt = op, at
		}
	}
	if oldest != nil && len(s.byID) >= maxOperations {
		s.dropLocked(oldest)
	}
}

func newOperationID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "op_" + hex.EncodeToString(b[:])
}

// operationOwner is who may read an operation: the key that started it, or
// the master token for its own.
func operationOwner(p Principal) string {
	if p.KeyID != "" {
		return "key:" + p.KeyID
	}
	return "master"
}

// operationWait is the longest a request attaches to an operation.
func (h *Handler) operationWait() time.Duration {
	if h.OperationWait > 0 {
		return h.OperationWait
	}
	return defaultOperationWait
}

func (h *Handler) operationTimeout() time.Duration {
	if h.OperationTimeout > 0 {
		return h.OperationTimeout
	}
	return defaultOperationTimeout
}

func (h *Handler) operationStore() *operationStore {
	h.opsOnce.Do(func() { h.ops = newOperationStore() })
	return h.ops
}

// parseWait reads ?wait=: absent or "true" is the default bound, "false" or
// "0" returns at once, and a duration ("30s") is capped at the default bound.
func (h *Handler) parseWait(r *http.Request) (time.Duration, error) {
	max := h.operationWait()
	raw := r.URL.Query().Get("wait")
	switch raw {
	case "", "true":
		return max, nil
	case "false", "0":
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		secs, serr := strconv.Atoi(raw)
		if serr != nil {
			return 0, errors.New(`wait: want "true", "false" or a duration such as "30s"`)
		}
		d = time.Duration(secs) * time.Second
	}
	if d < 0 {
		return 0, errors.New("wait: must not be negative")
	}
	return min(d, max), nil
}

// validIdempotencyKey reports whether key is 1-255 visible ascii characters.
func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > maxIdempotencyKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// asOperation runs a mutating route as an operation. the handler runs off the
// request's goroutine; the request waits for it up to ?wait= and answers with
// what the handler wrote, or 202 with the operation when the bound comes
// first. an Idempotency-Key makes a retry of the same request attach to the
// first one's operation instead of running again. limit bounds the body,
// which is read up front so the operation owns it.
func (h *Handler) asOperation(kind string, limit int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderIdempotencyKey)
		if key != "" && !validIdempotencyKey(key) {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument,
				HeaderIdempotencyKey+": want 1 to 255 visible ascii characters", nil)
			return
		}
		wait, err := h.parseWait(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
			return
		}
		body, ok := readOperationBody(w, r, limit)
		if !ok {
			return
		}

		owner := operationOwner(principal(r))
		sum := sha256.New()
		sum.Write([]byte(r.Method + " " + r.URL.Path + "?action=" + r.URL.Query().Get("action") + "\n"))
		sum.Write(body)
		op := &operation{
			id:          newOperationID(),
			kind:        kind,
			owner:       owner,
			key:         key,
			fingerprint: hex.EncodeToString(sum.Sum(nil)),
			startedAt:   time.Now().UTC(),
			done:        make(chan struct{}),
			rec:         &opRecorder{header: http.Header{}},
		}
		if prev := h.operationStore().begin(op); prev != nil {
			if prev.fingerprint != op.fingerprint {
				writeError(w, http.StatusConflict, CodeConflict,
					HeaderIdempotencyKey+" was already used for a different request",
					map[string]string{"reason": "idempotency_key_reused", "operation_id": prev.id})
				return
			}
			if prev.finished() {
				w.Header().Set(HeaderIdempotentReplayed, "true")
			}
			h.answerOperation(w, r, prev, wait)
			return
		}

		h.startOperation(r, op, body, next)
		h.answerOperation(w, r, op, wait)
	}
}

// readOperationBody reads at most limit bytes of r's body.
func readOperationBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, true
	}
	defer boundBodyRead(w)()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writeDecodeError(w, err, limit)
		return nil, false
	}
	return body, true
}

// startOperation runs next for op on its own goroutine, on a context that
// keeps the request's values (principal, request id, trace) but not its
// cancellation. chi's route context is copied because chi pools it once the
// request returns.
func (h *Handler) startOperation(r *http.Request, op *operation, body []byte, next http.HandlerFunc) {
	ctx := context.WithoutCancel(r.Context())
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		cp := chi.NewRouteContext()
		cp.Routes = rctx.Routes
		cp.RoutePath = rctx.RoutePath
		cp.RouteMethod = rctx.RouteMethod
		cp.URLParams.Keys = append([]string(nil), rctx.URLParams.Keys...)
		cp.URLParams.Values = append([]string(nil), rctx.URLParams.Values...)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, cp)
	}
	if op.key != "" {
		ctx = orchestrator.WithIdempotencyKey(ctx, op.owner+":"+op.kind+":"+op.key)
	}
	ctx = orchestrator.WithStepReporter(ctx, op.setStep)
	ctx, cancel := context.WithTimeout(ctx, h.operationTimeout())

	req := r.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	go func() {
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				op.rec.status = 0
				op.rec.body.Reset()
				writeError(op.rec, http.StatusInternalServerError, CodeInternal, "operation panicked", nil)
			}
			if op.rec.status == 0 {
				op.rec.status = http.StatusOK
			}
			op.mu.Lock()
			op.finishedAt = time.Now().UTC()
			op.mu.Unlock()
			close(op.done)
		}()
		next(op.rec, req)
	}()
}

// answerOperation waits up to wait for op, then writes its answer or a 202.
func (h *Handler) answerOperation(w http.ResponseWriter, r *http.Request, op *operation, wait time.Duration) {
	w.Header().Set(HeaderOperationID, op.id)
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-op.done:
		case <-t.C:
		case <-r.Context().Done():
			// the caller left; the operation goes on.
			return
		}
	}
	if !op.finished() {
		w.Header().Set("Location", "/v1/operations/"+op.id)
		writeJSON(w, http.StatusAccepted, OperationAccepted{OperationID: op.id, Operation: op.view()})
		return
	}
	for k, vs := range op.rec.header {
		w.Header()[k] = vs
	}
	w.WriteHeader(op.rec.status)
	_, _ = w.Write(op.rec.body.Bytes())
}

// getOperation reports an operation, attaching for up to ?wait= first.
//
//	@Summary		Get operation
//	@Description	Reports a mutating call's operation: its kind, state, current step and, once finished, the answer the call would have returned. ?wait= attaches until it finishes, bounded below the server's write timeout. An operation is readable by the credential that started it for 24h after it finishes.
//	@Tags			operations
//	@Produce		json
//	@Param			operationId	path		string	true	"Operation identifier"
//	@Param			wait		query		string	false	"How long to wait for the operation to finish, e.g. 30s"
//	@Success		200			{object}	Operation
//	@Failure		400			{object}	Error
//	@Failure		404			{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/operations/{operationId} [get]
func (h *Handler) getOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "operationId")
	op, ok := h.operationStore().get(id)
	if !ok || (op.owner != operationOwner(principal(r)) && !principal(r).Master) {
		writeError(w, http.StatusNotFound, CodeNotFound, "operation not found", nil)
		return
	}
	wait := time.Duration(0)
	if r.URL.Query().Has("wait") {
		var err error
		if wait, err = h.parseWait(r); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
			return
		}
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-op.done:
		case <-t.C:
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, op.view())
}
