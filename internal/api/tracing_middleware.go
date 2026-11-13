package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/folsomintel/fuse/internal/telemetry"
)

// TracingMiddleware starts a server span per request, continuing an upstream
// traceparent when the caller sent one, and names it after the chi route
// pattern once routing has run. It must sit inside RequestIDMiddleware so the
// span and the failure log line carry the request id.
//
// A 5xx response is logged with the request id and trace id, so a failed
// call's X-Request-ID can be found in the orchestrator log. With tracing off
// the span is a noop and only that log line remains.
func TracingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := telemetry.Tracer().Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.request.method", r.Method),
					attribute.String("request_id", RequestID(ctx)),
				),
			)
			defer span.End()

			ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r.WithContext(ctx))

			route := "unknown"
			if rc := chi.RouteContext(ctx); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			span.SetName(r.Method + " " + route)
			span.SetAttributes(
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", ww.status),
			)
			if ww.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(ww.status))
				if logger != nil {
					logger.WarnContext(ctx, "request failed",
						"method", r.Method, "route", route, "status", ww.status)
				}
			}
		})
	}
}
