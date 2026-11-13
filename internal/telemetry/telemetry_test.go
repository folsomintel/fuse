package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetupOffWithoutEndpoint(t *testing.T) {
	shutdown, enabled, err := Setup(context.Background(), func(string) string { return "" }, "svc")
	if err != nil || enabled {
		t.Fatalf("Setup with no endpoint: enabled=%v err=%v", enabled, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// spans stay noops: nothing is recorded or exported.
	_, span := Tracer().Start(context.Background(), "x")
	if span.SpanContext().IsValid() {
		t.Error("tracing off still produced a valid span")
	}
	if otel.GetTextMapPropagator().Fields() == nil {
		t.Error("propagator not installed")
	}
}

func TestLogHandlerAddsRequestID(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewLogHandler(slog.NewTextHandler(&buf, nil))).With("k", "v")
	l.InfoContext(WithRequestID(context.Background(), "req_log"), "hello")
	l.Info("no ctx")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], "request_id=req_log") || !strings.Contains(lines[0], "k=v") {
		t.Errorf("first line %q lacks request_id or the With attr", lines[0])
	}
	if strings.Contains(lines[1], "request_id") {
		t.Errorf("line without a ctx carries a request id: %q", lines[1])
	}
}

func TestWithRequestIDEmptyKeepsCtx(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_a")
	if got := RequestID(WithRequestID(ctx, "")); got != "req_a" {
		t.Errorf("empty id replaced the existing one: %q", got)
	}
}
