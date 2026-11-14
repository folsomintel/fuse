package hostwire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"
)

// IdempotencyHeader carries the key a host agent dedupes a mutating request
// on. a repeat with the same key gets the first response back instead of
// running again, so a step can be retried after a lost response.
const IdempotencyHeader = "Idempotency-Key"

// idempotencyKeyChars is the charset fc-agent and qemu-agent accept, as
// IDEMPOTENCY_KEY_RE with a 256 character bound. a base key outside it is
// hashed into it rather than dropped, and is held to 200 so the "/n" suffix
// still fits the agents' bound.
const idempotencyKeyChars = `[A-Za-z0-9_.:/-]`

var idempotencyKeyRE = regexp.MustCompile(`^` + idempotencyKeyChars + `{1,200}$`)

// retry bounds for keyed requests. only keyed requests are retried: the agent
// replays a finished one, so a retry cannot double its effect.
const (
	keyedMaxAttempts = 3
	keyedRetryDelay  = 250 * time.Millisecond
)

type idempotencyCtxKey struct{}

type idempotencyScope struct {
	base string
	n    atomic.Int64
}

// WithIdempotencyKey returns ctx whose mutating host-agent requests carry
// idempotency keys derived from key. each request made under ctx gets
// key/1, key/2, ... in the order it is sent, so a step that makes several
// calls keys each one apart, and the same step run again keys them the same
// way. durable.Run sets it to the restate invocation id and the step name;
// callers outside restate pass their own. an empty key leaves ctx unchanged.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	if !idempotencyKeyRE.MatchString(key) {
		sum := sha256.Sum256([]byte(key))
		key = hex.EncodeToString(sum[:])
	}
	return context.WithValue(ctx, idempotencyCtxKey{}, &idempotencyScope{base: key})
}

// nextIdempotencyKey returns the key for the next mutating request under ctx,
// or "" when ctx carries none.
func nextIdempotencyKey(ctx context.Context) string {
	s, _ := ctx.Value(idempotencyCtxKey{}).(*idempotencyScope)
	if s == nil {
		return ""
	}
	return s.base + "/" + strconv.FormatInt(s.n.Add(1), 10)
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// idempotencyTransport keys mutating requests and retries keyed ones on a
// transport error or a 5xx. requests without a key pass through unchanged.
type idempotencyTransport struct {
	Base http.RoundTripper
	// delay is the first retry's wait, doubled per retry; keyedRetryDelay
	// when zero.
	delay time.Duration
}

func (t *idempotencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !mutating(req.Method) {
		return t.Base.RoundTrip(req)
	}
	key := req.Header.Get(IdempotencyHeader)
	if key == "" {
		key = nextIdempotencyKey(req.Context())
	}
	if key == "" {
		return t.Base.RoundTrip(req)
	}
	// a RoundTripper must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(IdempotencyHeader, key)
	// a body that cannot be rewound is sent once; the key still lets the
	// caller's own retry replay.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	delay := t.delay
	if delay == 0 {
		delay = keyedRetryDelay
	}
	for attempt := 1; ; attempt++ {
		res, err := t.Base.RoundTrip(req)
		if !retryable(res, err) || attempt == keyedMaxAttempts || !replayable || req.Context().Err() != nil {
			return res, err
		}
		if res != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, MaxErrorBodyBytes))
			_ = res.Body.Close()
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(delay):
		}
		delay *= 2
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
	}
}

// retryable reports whether a keyed attempt is worth repeating: the request
// may not have reached the agent, or the agent failed it without storing a
// result. 501 is the agent saying it never will, so it is final.
func retryable(res *http.Response, err error) bool {
	if err != nil {
		return true
	}
	return res.StatusCode >= 500 && res.StatusCode != http.StatusNotImplemented
}
