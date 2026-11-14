package hostwire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestValidateAgentURL_Accepts(t *testing.T) {
	valid := []string{
		"http://10.0.0.1:8090",
		"https://agent.internal:8443",
		"http://127.0.0.1:8090/",
		"https://proxy.example.com/agent", // a path prefix is a supported topology
		"http://[2001:db8::1]:8090",
		"http://host-agent",
	}

	for _, raw := range valid {
		if err := ValidateAgentURL(raw); err != nil {
			t.Errorf("ValidateAgentURL(%q) = %v, want nil", raw, err)
		}
	}
}

func TestValidateAgentURL_Rejects(t *testing.T) {
	invalid := []string{
		"",
		"   ",
		"10.0.0.1:8090",                  // parses as scheme "10.0.0.1"
		"ftp://10.0.0.1:8090",            // wrong scheme
		"file:///etc/passwd",             // reads local state
		"unix:///var/run/agent.sock",     // not http
		"http://user:pass@10.0.0.1:8090", // credentials would be sent every request
		"http://10.0.0.1:8090?x=1",       // query is dropped when paths are appended
		"http://10.0.0.1:8090#frag",
		"http://10.0.0.1:99999", // port out of range
		"http://10.0.0.1:abc",
		"http://",
		"https://",
	}

	for _, raw := range invalid {
		if err := ValidateAgentURL(raw); err == nil {
			t.Errorf("ValidateAgentURL(%q) = nil, want an error", raw)
		}
	}
}

// TestNewClient_HasTimeouts guards the reason this client exists at all: an
// http.Client with no Timeout is what let an unresponsive host pin a call.
func TestNewClient_HasTimeouts(t *testing.T) {
	c := NewClient()
	if c.Timeout <= 0 {
		t.Fatal("client has no overall timeout")
	}

	keyed, ok := c.Transport.(*idempotencyTransport)
	if !ok {
		t.Fatalf("transport: got %T, want *idempotencyTransport", c.Transport)
	}
	wrapped, ok := keyed.Base.(*telemetry.Transport)
	if !ok {
		t.Fatalf("transport: got %T, want *telemetry.Transport", keyed.Base)
	}
	tr, ok := wrapped.Base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport: got %T, want *http.Transport", wrapped.Base)
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("transport has no TLS handshake timeout")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("transport has no response header timeout")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("transport has no idle connection timeout")
	}
	// fc-agent closes a keep-alive connection idle for 60s; reusing one past
	// that would send a request into a closed socket.
	if tr.IdleConnTimeout >= 60*time.Second {
		t.Errorf("idle connection timeout %v is not under fc-agent's 60s", tr.IdleConnTimeout)
	}
}

// TestNewClient_RefusesRedirects verifies a host agent cannot bounce the
// orchestrator (and the Authorization header it carries) somewhere else.
func TestNewClient_RefusesRedirects(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits++
		if r.Header.Get("Authorization") != "" {
			t.Error("redirect target received the host agent token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer agent.Close()

	req, err := http.NewRequest(http.MethodGet, agent.URL+"/v1/capacity", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer host-token")

	res, err := NewClient().Do(req) //nolint:bodyclose // the error path returns no body
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("redirect was followed; want an error")
	}
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("redirect error: got %v, want ErrRedirectNotAllowed", err)
	}
	if elsewhereHits != 0 {
		t.Fatalf("redirect target was contacted %d times", elsewhereHits)
	}
}

// TestReadErrorBody_IsBounded verifies a host agent cannot make the
// orchestrator hold an arbitrarily large error message in memory.
func TestReadErrorBody_IsBounded(t *testing.T) {
	huge := strings.NewReader(strings.Repeat("x", MaxErrorBodyBytes*3))

	got := ReadErrorBody(huge)

	if len(got) != MaxErrorBodyBytes {
		t.Fatalf("error body length: got %d, want %d", len(got), MaxErrorBodyBytes)
	}
}

func TestReadErrorBody_TrimsShortBodies(t *testing.T) {
	got := ReadErrorBody(strings.NewReader("  vm not found\n"))
	if got != "vm not found" {
		t.Fatalf("error body: got %q, want %q", got, "vm not found")
	}
}

// TestNewClient_PropagatesRequestIDAndTrace pins what #337 needs from the
// host-agent wire: the api request id and the w3c trace context reach the
// agent on every call, and the caller's request is not modified.
func TestNewClient_PropagatesRequestIDAndTrace(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	var gotID, gotParent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get(telemetry.RequestIDHeader)
		gotParent = r.Header.Get("traceparent")
	}))
	defer srv.Close()

	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	ctx = telemetry.WithRequestID(ctx, "req_abc")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/vm", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()

	if gotID != "req_abc" {
		t.Errorf("X-Request-ID: got %q, want req_abc", gotID)
	}
	traceID := span.SpanContext().TraceID().String()
	if !strings.Contains(gotParent, traceID) {
		t.Errorf("traceparent %q does not carry trace %s", gotParent, traceID)
	}
	if req.Header.Get(telemetry.RequestIDHeader) != "" || req.Header.Get("traceparent") != "" {
		t.Error("transport modified the caller's request headers")
	}
}
