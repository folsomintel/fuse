package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/folsomintel/fuse/internal/telemetry"
)

// the server span continues the caller's trace, is named after the route
// pattern, and a 5xx is logged with the request id.
func TestTracingMiddleware(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) }()

	var buf bytes.Buffer
	logger := slog.New(telemetry.NewLogHandler(slog.NewTextHandler(&buf, nil)))

	var handlerReqID string
	r := chi.NewRouter()
	r.Use(RequestIDMiddleware)
	r.Use(TracingMiddleware(logger))
	r.Post("/v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		handlerReqID = telemetry.RequestID(r.Context())
		w.WriteHeader(http.StatusInternalServerError)
	})

	const parentTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/fuse-abc", nil)
	req.Header.Set("traceparent", "00-"+parentTrace+"-00f067aa0ba902b7-01")
	req.Header.Set(RequestIDHeader, "req_mw")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if handlerReqID != "req_mw" {
		t.Errorf("handler request id %q, want req_mw", handlerReqID)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "POST /v1/environments/{id}" {
		t.Errorf("span name %q", s.Name())
	}
	if s.SpanContext().TraceID().String() != parentTrace {
		t.Errorf("span trace %s, want the caller's %s", s.SpanContext().TraceID(), parentTrace)
	}
	if s.Status().Code.String() != "Error" {
		t.Errorf("span status %v, want Error", s.Status())
	}
	line := buf.String()
	for _, want := range []string{"request failed", "request_id=req_mw", "trace_id=" + parentTrace, "status=500"} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
}
