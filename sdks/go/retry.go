package fuse

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	// defaultRetries is how many times a mutating call answered 503 is sent
	// again before the 503 is returned.
	defaultRetries = 3
	// retryBase and retryCap bound the backoff of a 503 without Retry-After.
	retryBase = time.Second
	retryCap  = 30 * time.Second
	// maxRetryAfter caps a server's Retry-After.
	maxRetryAfter = time.Minute
)

// WithRetry sets how many times a call that sends an Idempotency-Key
// (create, fork, migrate, drain, destroy, snapshot, restore, image convert,
// build) and is answered 503 is sent again, under the same key, before the
// 503 is returned as an error. Other calls are never retried. Each retry waits at least the
// server's Retry-After, or an exponential backoff without one, plus up to
// half again as jitter. The default is 3; 0 turns retrying off.
func WithRetry(retries int) Option {
	return func(cfg *clientConfig) {
		cfg.retries = max(retries, 0)
	}
}

// retryDelay is how long to wait before retry number attempt (from 0) of a
// 503 whose Retry-After header is retryAfter.
func retryDelay(retryAfter string, attempt int) time.Duration {
	d := min(retryBase<<min(attempt, 10), retryCap)
	if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
		d = min(time.Duration(secs)*time.Second, maxRetryAfter)
	}
	// jitter spreads a burst of refused callers out, and only ever adds, so
	// the wait never undercuts the server's Retry-After.
	return d + rand.N(d/2+1)
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryUnavailable sends req through send, and while the answer is a 503 and
// retries remain, waits and sends it again with a fresh copy of its body.
func (t *transport) retryUnavailable(ctx context.Context, req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	sleep := t.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for attempt := 0; ; attempt++ {
		resp, err := send(req)
		if err != nil || resp.StatusCode != http.StatusServiceUnavailable || attempt >= t.retries {
			return resp, err
		}
		if req.Body != nil && req.GetBody == nil {
			// the body cannot be sent twice.
			return resp, nil
		}
		delay := retryDelay(resp.Header.Get("Retry-After"), attempt)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
		next := req.Clone(ctx)
		if req.GetBody != nil {
			if next.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		req = next
	}
}
