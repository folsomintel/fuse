package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Transport starts a client span per request and carries the request id and
// the w3c trace context to the callee.
type Transport struct {
	// Base sends the request; http.DefaultTransport when nil.
	Base http.RoundTripper
	// Peer names the callee in span names, e.g. "host-agent" or "restate".
	Peer string
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	ctx, span := Tracer().Start(req.Context(), t.Peer+" "+req.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", req.Method),
			attribute.String("server.address", req.URL.Host),
			attribute.String("url.path", req.URL.Path),
		),
	)
	// a RoundTripper must not modify the caller's request.
	req = req.Clone(ctx)
	if id := RequestID(ctx); id != "" && req.Header.Get(RequestIDHeader) == "" {
		req.Header.Set(RequestIDHeader, id)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	res, err := base.RoundTrip(req)
	if err != nil {
		EndSpan(span, err)
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", res.StatusCode))
	if res.StatusCode >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("http %d", res.StatusCode))
	}
	span.End()
	return res, nil
}

// InjectHeaders writes the request id and trace context from ctx onto h, for
// requests that are not sent through an http.Client (the raw attach upgrade).
func InjectHeaders(ctx context.Context, h http.Header) {
	if id := RequestID(ctx); id != "" {
		h.Set(RequestIDHeader, id)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}
