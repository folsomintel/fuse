// Package telemetry carries the per-request correlation id and the
// opentelemetry setup shared by the api, the fleet, the restate handlers and
// the host-agent wire. It imports nothing from the rest of fuse, so every
// layer can use it without an import cycle.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader carries the correlation id on api requests, host-agent
// calls and restate ingress calls.
const RequestIDHeader = "X-Request-ID"

const tracerName = "github.com/folsomintel/fuse"

type requestIDKey struct{}

// WithRequestID returns ctx carrying id. An empty id leaves ctx unchanged.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request id in ctx, or "".
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Tracer is the tracer every fuse span is started from. Before Setup (or
// with tracing off) it hands out noop spans.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// Setup installs the global tracer provider and the w3c propagator.
//
// Tracing is on only when OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set; the exporter speaks otlp/http
// and reads the rest of the standard OTEL_EXPORTER_OTLP_* variables itself.
// OTEL_SERVICE_NAME names the service (default service). With tracing off the
// propagator is still installed, so an upstream traceparent is passed through
// to host agents and restate unchanged.
//
// The returned shutdown flushes pending spans; it is a no-op when tracing is
// off.
func Setup(ctx context.Context, getenv func(string) string, service string) (shutdown func(context.Context) error, enabled bool, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	noop := func(context.Context) error { return nil }
	if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return noop, false, nil
	}
	if name := getenv("OTEL_SERVICE_NAME"); name != "" {
		service = name
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, false, fmt.Errorf("otlp trace exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return noop, false, fmt.Errorf("otel resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, true, nil
}

// EndSpan records err on span (when non-nil) and ends it.
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
