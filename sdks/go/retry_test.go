package fuse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// recordSleeps replaces c's retry sleep with one that records each delay and
// returns at once.
func recordSleeps(c *Client) *[]time.Duration {
	var mu sync.Mutex
	delays := &[]time.Duration{}
	c.t.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		*delays = append(*delays, d)
		mu.Unlock()
		return nil
	}
	return delays
}

// a create refused for capacity is sent again under the same key, after at
// least the server's Retry-After, and the retry's answer is the call's.
func TestCreate_retriesUnavailableHonoringRetryAfter(t *testing.T) {
	var keys []string
	var bodies []string
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(keys) < 3 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"no host has sufficient capacity"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"vm-1","state":"running"}`))
	})
	defer done()
	delays := recordSleeps(c)

	env, err := c.Environments.Create(context.Background(), CreateRequest{TaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "vm-1" || len(keys) != 3 {
		t.Fatalf("env %+v after %d sends, want vm-1 after 3", env, len(keys))
	}
	if keys[0] == "" || keys[1] != keys[0] || keys[2] != keys[0] {
		t.Errorf("keys = %q, want one key on every send", keys)
	}
	if bodies[1] != bodies[0] || bodies[2] != bodies[0] || bodies[0] == "" {
		t.Errorf("bodies = %q, want the same body on every send", bodies)
	}
	if len(*delays) != 2 {
		t.Fatalf("slept %d times, want 2", len(*delays))
	}
	for _, d := range *delays {
		if d < 2*time.Second || d > 3*time.Second {
			t.Errorf("delay %s, want Retry-After 2s plus at most half again", d)
		}
	}
}

// without Retry-After the backoff doubles from 1s, and once the retries run
// out the 503 comes back as an unavailable error.
func TestCreate_unavailableAfterRetriesRunOut(t *testing.T) {
	sends := 0
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sends++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"no host has sufficient capacity"}}`))
	})
	defer done()
	delays := recordSleeps(c)

	_, err := c.Environments.Create(context.Background(), CreateRequest{TaskID: "t"})
	if !IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if sends != 1+defaultRetries {
		t.Errorf("%d sends, want %d", sends, 1+defaultRetries)
	}
	for i, d := range *delays {
		base := time.Second << i
		if d < base || d > base+base/2 {
			t.Errorf("delay %d = %s, want %s plus at most half again", i, d, base)
		}
	}
}

// WithRetry(0) returns the first 503.
func TestWithRetryZeroDoesNotRetry(t *testing.T) {
	sends := 0
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sends++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"full"}}`))
	})
	defer done()
	c2, err := New(c.t.baseURL.String(), "tok", WithRetry(0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c2.Environments.Create(context.Background(), CreateRequest{TaskID: "t"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || sends != 1 {
		t.Fatalf("err = %v after %d sends, want the first 503", err, sends)
	}
}

// a context that ends while waiting to retry ends the call.
func TestRetryStopsWhenContextEnds(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"full"}}`))
	})
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Environments.Create(ctx, CreateRequest{TaskID: "t"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
}

// a create that answers 202 and whose operation then fails 503 waits at least
// the failed answer's Retry-After, carried on the operation, before it is
// sent again.
func TestCreate_polledUnavailableHonorsRetryAfter(t *testing.T) {
	posts := 0
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
			posts++
			if posts == 1 {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"operation_id":"op_1","operation":{"id":"op_1","kind":"create","state":"running","started_at":"2026-11-15T00:00:00Z"}}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"vm-1","state":"running"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op_1":
			_, _ = w.Write([]byte(`{"id":"op_1","kind":"create","state":"failed","started_at":"2026-11-15T00:00:00Z","finished_at":"2026-11-15T00:01:00Z","status_code":503,"retry_after":5,"error":{"code":"unavailable","message":"no host has sufficient capacity"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer done()
	delays := recordSleeps(c)

	env, err := c.Environments.Create(context.Background(), CreateRequest{TaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "vm-1" || posts != 2 {
		t.Fatalf("env %+v after %d sends, want vm-1 after 2", env, posts)
	}
	if len(*delays) != 1 || (*delays)[0] < 5*time.Second || (*delays)[0] > 7500*time.Millisecond {
		t.Errorf("delays = %v, want one of Retry-After 5s plus at most half again", *delays)
	}
}
