package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/folsomintel/fuse/internal/telemetry"
)

// an event row written while serving an api request carries its request id,
// and a caller's own request_id is not overwritten.
func TestAppendEventCarriesRequestID(t *testing.T) {
	store := NewMemoryStateStore()
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", StateStore: store})

	ctx := telemetry.WithRequestID(context.Background(), "req_evt")
	payload := map[string]any{"k": "v"}
	fm.AuditEvent(ctx, "api", "x", "a.one", payload)
	fm.AuditEvent(ctx, "api", "x", "a.two", map[string]any{"request_id": "upstream"})
	fm.AuditEvent(context.Background(), "api", "x", "a.three", nil)

	if _, ok := payload["request_id"]; ok {
		t.Error("appendEvent wrote into the caller's payload map")
	}
	want := map[string]string{"a.one": "req_evt", "a.two": "upstream", "a.three": ""}
	for _, ev := range store.events {
		var p map[string]any
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		got, _ := p["request_id"].(string)
		if got != want[ev.EventType] {
			t.Errorf("%s: request_id %q, want %q", ev.EventType, got, want[ev.EventType])
		}
	}
	if len(store.events) != 3 {
		t.Fatalf("got %d events, want 3", len(store.events))
	}
}

// a failed operation is logged with the request id and leaves an errored
// fleet span named after the op.
func TestOperationFailureIsTracedAndLogged(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	var buf bytes.Buffer
	logger := slog.New(telemetry.NewLogHandler(slog.NewTextHandler(&buf, nil)))
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-", Logger: logger})

	ctx := telemetry.WithRequestID(context.Background(), "req_op")
	if err := fm.DestroyVM(ctx, "fuse-missing"); err == nil {
		t.Fatal("destroy of a missing vm succeeded")
	}

	line := buf.String()
	for _, want := range []string{"operation failed", "op=destroy", "request_id=req_op", "trace_id="} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "fleet.destroy" {
		t.Fatalf("spans = %v, want one fleet.destroy", spans)
	}
	if spans[0].Status().Code.String() != "Error" {
		t.Errorf("span status %v, want Error", spans[0].Status())
	}
}
