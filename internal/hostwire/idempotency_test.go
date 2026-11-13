package hostwire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeAgent answers each attempt from a script and records what it saw.
type fakeAgent struct {
	mu     sync.Mutex
	script []func() (*http.Response, error)
	keys   []string
	bodies []string
}

func (f *fakeAgent) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, req.Header.Get(IdempotencyHeader))
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.bodies = append(f.bodies, body)
	step := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	return step()
}

func status(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("x"))}, nil
	}
}

func dropped() (*http.Response, error) { return nil, errors.New("connection reset") }

func send(t *testing.T, tr http.RoundTripper, ctx context.Context, method, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://agent/v1/vm", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	return tr.RoundTrip(req)
}

func TestKeyedRequestRetriesWithSameKeyAndBody(t *testing.T) {
	f := &fakeAgent{script: []func() (*http.Response, error){dropped, status(503), status(200)}}
	tr := &idempotencyTransport{Base: f, delay: 1}
	ctx := WithIdempotencyKey(context.Background(), "inv_1:create.boot")

	res, err := send(t, tr, ctx, http.MethodPost, `{"name":"a"}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("got %v %v, want 200", res, err)
	}
	if len(f.keys) != 3 {
		t.Fatalf("attempts: got %d, want 3", len(f.keys))
	}
	for i := range f.keys {
		if f.keys[i] != "inv_1:create.boot/1" {
			t.Errorf("attempt %d key %q", i, f.keys[i])
		}
		if f.bodies[i] != `{"name":"a"}` {
			t.Errorf("attempt %d body %q", i, f.bodies[i])
		}
	}
}

func TestKeyedRetryIsBounded(t *testing.T) {
	f := &fakeAgent{script: []func() (*http.Response, error){status(500)}}
	tr := &idempotencyTransport{Base: f, delay: 1}
	res, err := send(t, tr, WithIdempotencyKey(context.Background(), "k"), http.MethodPost, "{}")
	if err != nil || res.StatusCode != 500 {
		t.Fatalf("got %v %v, want the last 500", res, err)
	}
	if len(f.keys) != keyedMaxAttempts {
		t.Fatalf("attempts: got %d, want %d", len(f.keys), keyedMaxAttempts)
	}
}

func TestKeyedNotRetriedOnFinalAnswers(t *testing.T) {
	for _, code := range []int{200, 409, 501} {
		f := &fakeAgent{script: []func() (*http.Response, error){status(code)}}
		tr := &idempotencyTransport{Base: f, delay: 1}
		if _, err := send(t, tr, WithIdempotencyKey(context.Background(), "k"), http.MethodPost, "{}"); err != nil {
			t.Fatal(err)
		}
		if len(f.keys) != 1 {
			t.Errorf("%d: attempts %d, want 1", code, len(f.keys))
		}
	}
}

// non-keyed calls are exactly what they were: one attempt, no header.
func TestUnkeyedRequestUnchanged(t *testing.T) {
	f := &fakeAgent{script: []func() (*http.Response, error){dropped}}
	tr := &idempotencyTransport{Base: f, delay: 1}
	if _, err := send(t, tr, context.Background(), http.MethodPost, "{}"); err == nil {
		t.Fatal("want the transport error")
	}
	if len(f.keys) != 1 || f.keys[0] != "" {
		t.Fatalf("got keys %q, want one unkeyed attempt", f.keys)
	}
}

// a read is never keyed or retried, even under a key.
func TestGetNotKeyed(t *testing.T) {
	f := &fakeAgent{script: []func() (*http.Response, error){status(503)}}
	tr := &idempotencyTransport{Base: f, delay: 1}
	if _, err := send(t, tr, WithIdempotencyKey(context.Background(), "k"), http.MethodGet, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || f.keys[0] != "" {
		t.Fatalf("got keys %q", f.keys)
	}
}

// each call in a step gets its own key, in order, so a re-run of the step
// keys them identically.
func TestCallsInOneScopeGetSequentialKeys(t *testing.T) {
	run := func() []string {
		f := &fakeAgent{script: []func() (*http.Response, error){status(200)}}
		tr := &idempotencyTransport{Base: f, delay: 1}
		ctx := WithIdempotencyKey(context.Background(), "inv_1:create.boot")
		for range 3 {
			if _, err := send(t, tr, ctx, http.MethodPost, "{}"); err != nil {
				t.Fatal(err)
			}
		}
		return f.keys
	}
	first, second := run(), run()
	want := []string{"inv_1:create.boot/1", "inv_1:create.boot/2", "inv_1:create.boot/3"}
	for i := range want {
		if first[i] != want[i] || second[i] != want[i] {
			t.Fatalf("got %q then %q, want %q", first, second, want)
		}
	}
}

// a caller-set header wins over the context key.
func TestExplicitHeaderKept(t *testing.T) {
	f := &fakeAgent{script: []func() (*http.Response, error){status(200)}}
	tr := &idempotencyTransport{Base: f, delay: 1}
	req, _ := http.NewRequest(http.MethodDelete, "http://agent/v1/vm/a", nil)
	req.Header.Set(IdempotencyHeader, "mine")
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if f.keys[0] != "mine" {
		t.Fatalf("got %q", f.keys[0])
	}
}

func TestOutOfCharsetKeyIsHashed(t *testing.T) {
	ctx := WithIdempotencyKey(context.Background(), "bad key\n")
	got := nextIdempotencyKey(ctx)
	if !idempotencyKeyRE.MatchString(strings.TrimSuffix(got, "/1")) || strings.Contains(got, " ") {
		t.Fatalf("got %q", got)
	}
}

func TestCanceledContextStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(WithIdempotencyKey(context.Background(), "k"))
	f := &fakeAgent{script: []func() (*http.Response, error){func() (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	}}}
	tr := &idempotencyTransport{Base: f, delay: 1}
	if _, err := send(t, tr, ctx, http.MethodPost, "{}"); err == nil {
		t.Fatal("want an error")
	}
	if len(f.keys) != 1 {
		t.Fatalf("attempts %d, want 1", len(f.keys))
	}
}

// the agents must accept every key this package sends; a change to one rule
// cannot drift from the other two.
func TestAgentsShareIdempotencyKeyRule(t *testing.T) {
	want := `IDEMPOTENCY_KEY_RE = re.compile(r"` + idempotencyKeyChars + `{1,256}")`
	header := `IDEMPOTENCY_HEADER = "` + IdempotencyHeader + `"`
	for _, f := range []string{
		"../../host-agent/firecracker/fc-agent.py",
		"../../host-agent/qemu/qemu-agent.py",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{want, header} {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s: want %s", f, w)
			}
		}
	}
}
