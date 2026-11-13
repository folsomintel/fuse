package durable

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
	"github.com/restatedev/sdk-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/folsomintel/fuse/internal/telemetry"
)

type recordedSteps struct {
	mu        sync.Mutex
	completed map[string]int
	retried   map[string]int
}

func (r *recordedSteps) StepCompleted(op, step string, _ time.Duration, _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed[op+"/"+step]++
}

func (r *recordedSteps) StepRetried(op, step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retried[op+"/"+step]++
}

type probeResult struct {
	RequestID string `json:"request_id"`
	TraceID   string `json:"trace_id"`
}

// TestRunThroughRestate drives durable.Run behind a real restate-server: the
// request id and the trace context sent to ingress reach the step body, a
// retryable failure is counted as a retry, and a replayed step is not re-run.
// skipped unless both urls are set (see TestPingThroughRestate). the handler
// listens on FUSE_RESTATE_TEST_HANDLER_HOST (default 127.0.0.1), which must be
// reachable from restate-server.
func TestRunThroughRestate(t *testing.T) {
	admin := os.Getenv("FUSE_RESTATE_TEST_ADMIN_URL")
	ingressURL := os.Getenv("FUSE_RESTATE_TEST_INGRESS_URL")
	if admin == "" || ingressURL == "" {
		t.Skip("set FUSE_RESTATE_TEST_ADMIN_URL and FUSE_RESTATE_TEST_INGRESS_URL to run against restate-server")
	}
	host := os.Getenv("FUSE_RESTATE_TEST_HANDLER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}

	tp := sdktrace.NewTracerProvider()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) }()

	steps := &recordedSteps{completed: map[string]int{}, retried: map[string]int{}}
	SetStepMetrics(steps)
	defer SetStepMetrics(nil)

	// the service name is unique per run so a stale deployment from an earlier
	// run never answers.
	svc := fmt.Sprintf("TraceProbe%d", time.Now().UnixNano())
	var mu sync.Mutex
	attempts := map[string]int{}
	probe := restate.NewService(svc,
		restate.WithInvocationRetryPolicy(restate.WithInitialRetryInterval(50*time.Millisecond)),
	).Handler("Go", restate.NewServiceHandler(
		func(ctx restate.Context, key string) (probeResult, error) {
			// fails once with a retryable error, then succeeds.
			res, err := Run(ctx, "probe", "first", func(sctx context.Context) (probeResult, error) {
				mu.Lock()
				attempts[key]++
				n := attempts[key]
				mu.Unlock()
				if n == 1 {
					return probeResult{}, fmt.Errorf("transient")
				}
				return probeResult{
					RequestID: telemetry.RequestID(sctx),
					TraceID:   trace.SpanContextFromContext(sctx).TraceID().String(),
				}, nil
			})
			if err != nil {
				return probeResult{}, err
			}
			// a second step: on the retried attempt "first" is replayed, so
			// only this one and the successful "first" run then.
			_, err = Run(ctx, "probe", "second", func(context.Context) (bool, error) { return true, nil })
			return res, err
		}))

	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.NewRestate().Bind(probe).Start(ctx, addr) }()
	waitListening(t, addr)

	body := fmt.Sprintf(`{"uri":"http://%s","force":true}`, addr)
	res, err := (&http.Client{Timeout: 30 * time.Second}).Post(admin+"/deployments", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode/100 != 2 {
		t.Fatalf("register: http %d: %s", res.StatusCode, reg)
	}

	callCtx, span := tp.Tracer("test").Start(ctx, "api request")
	callCtx = telemetry.WithRequestID(callCtx, "req_durable")
	client := NewClient(Config{IngressURL: ingressURL})
	got, err := ingress.Service[string, probeResult](client.ingress, svc, "Go").Request(callCtx, "k1")
	span.End()
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	if got.RequestID != "req_durable" {
		t.Errorf("step saw request id %q, want req_durable", got.RequestID)
	}
	if want := span.SpanContext().TraceID().String(); got.TraceID != want {
		t.Errorf("step trace %s, want the caller's %s", got.TraceID, want)
	}
	steps.mu.Lock()
	defer steps.mu.Unlock()
	if steps.completed["probe/first"] != 2 || steps.retried["probe/first"] != 1 {
		t.Errorf("first: completed %d retried %d, want 2 and 1", steps.completed["probe/first"], steps.retried["probe/first"])
	}
	if steps.completed["probe/second"] != 1 || steps.retried["probe/second"] != 0 {
		t.Errorf("second: completed %d retried %d, want 1 and 0", steps.completed["probe/second"], steps.retried["probe/second"])
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoint never listened on %s", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
