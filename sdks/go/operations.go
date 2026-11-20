package fuse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	operationIDHeader    = "Fuse-Operation-Id"
	// operationPollWait is the ?wait= of each poll while following an
	// operation. the server caps it at its own attach bound.
	operationPollWait = 30 * time.Second
)

// Operation states.
const (
	OperationRunning   = "running"
	OperationSucceeded = "succeeded"
	OperationFailed    = "failed"
)

// Operation is one run of a mutating call (create, destroy, fork, migrate,
// snapshot, restore, drain or image.convert). Once it finished, StatusCode
// with Result (on a 2xx) or Error (otherwise) is what the call itself would
// have answered.
type Operation struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	Step       string     `json:"step,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	StatusCode int        `json:"status_code,omitempty"`
	// Result is the call's response body on success: an EnvironmentInfo for
	// create, fork, migrate and drain, a Snapshot for snapshot, an Image
	// for image.convert, empty for destroy and restore.
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorBody      `json:"error,omitempty"`
}

// ErrorBody is a failed operation's error: the error member of the api's
// error envelope.
type ErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// Done reports whether the operation finished.
func (o *Operation) Done() bool { return o != nil && o.State != OperationRunning }

// OperationAccepted is the 202 body of a mutating call whose operation
// outlasted the server's attach bound.
type OperationAccepted struct {
	OperationID string    `json:"operation_id"`
	Operation   Operation `json:"operation"`
}

// CallOption tunes one mutating call.
type CallOption func(*callOptions)

type callOptions struct {
	idempotencyKey string
}

// WithIdempotencyKey sends key as the call's Idempotency-Key, so a retry of
// the same request with the same key returns the first call's result instead
// of running again. Without it every mutating call sends a fresh random key.
func WithIdempotencyKey(key string) CallOption {
	return func(o *callOptions) { o.idempotencyKey = key }
}

func newIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// doOperation sends a mutating call with its Idempotency-Key and, when the
// server answers 202 with an operation, follows it until it finishes. it
// returns a response standing in for the one the call would have given
// directly, so callers handle it exactly as before. a 503 (no capacity,
// restate down) is sent again under the same key, see WithRetry.
func (t *transport) doOperation(ctx context.Context, req *http.Request, opts []CallOption) (*http.Response, error) {
	var o callOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.idempotencyKey == "" {
		o.idempotencyKey = newIdempotencyKey()
	}
	req.Header.Set(idempotencyKeyHeader, o.idempotencyKey)
	return t.retryUnavailable(ctx, req, func(req *http.Request) (*http.Response, error) {
		return t.doOperationOnce(ctx, req)
	})
}

// doOperationOnce is one send of doOperation, following a 202 to its end.
func (t *transport) doOperationOnce(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := t.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusAccepted {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read accepted response: %w", err)
	}
	var acc OperationAccepted
	if json.Unmarshal(body, &acc) != nil || acc.OperationID == "" {
		// a hitl hold, not an operation: hand it back for readHeld.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	if t.onOperation != nil {
		t.onOperation(&acc)
	}
	op, err := waitOperation(ctx, t, acc.OperationID)
	if err != nil {
		return nil, err
	}
	return operationResponse(op, resp.Header), nil
}

// operationResponse rebuilds the answer a finished operation stands for.
func operationResponse(op *Operation, header http.Header) *http.Response {
	var body []byte
	if op.StatusCode/100 == 2 {
		body = op.Result
	} else if op.Error != nil {
		body, _ = json.Marshal(map[string]*ErrorBody{"error": op.Error})
	}
	status := op.StatusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// waitOperation polls the operation until it finishes. ctx bounds the wait;
// the operation keeps running if it ends first.
func waitOperation(ctx context.Context, t *transport, id string) (*Operation, error) {
	for {
		op, err := getOperation(ctx, t, id, operationPollWait)
		if err != nil {
			return nil, err
		}
		if op.Done() {
			return op, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func getOperation(ctx context.Context, t *transport, id string, wait time.Duration) (*Operation, error) {
	if id == "" {
		return nil, errors.New("operation id is required")
	}
	var q url.Values
	if wait > 0 {
		q = url.Values{"wait": {wait.String()}}
	}
	req, err := t.newRequest(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(id), q, nil)
	if err != nil {
		return nil, err
	}
	// the no-timeout client: a wait can run as long as the client's own
	// timeout. ctx bounds it.
	resp, err := t.doStream(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var op Operation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		return nil, fmt.Errorf("decode operation: %w", err)
	}
	return &op, nil
}

// OperationsService reads the operations mutating calls run as.
type OperationsService struct {
	t *transport
}

func newOperationsService(t *transport) *OperationsService {
	return &OperationsService{t: t}
}

// GetOperationOptions tunes Get.
type GetOperationOptions struct {
	// Wait attaches until the operation finishes, for at most this long
	// (capped by the server). Zero answers at once.
	Wait time.Duration
}

// Get reports an operation, finished or still running.
func (s *OperationsService) Get(ctx context.Context, id string, opt GetOperationOptions) (*Operation, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("operations service is not configured")
	}
	return getOperation(ctx, s.t, id, opt.Wait)
}

// Wait blocks until the operation finishes and returns it. ctx bounds the
// wait; the operation keeps running if ctx ends first. A failed operation is
// returned, not an error: its Error and StatusCode say why.
func (s *OperationsService) Wait(ctx context.Context, id string) (*Operation, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("operations service is not configured")
	}
	return waitOperation(ctx, s.t, id)
}
