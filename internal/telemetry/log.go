package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler adds request_id and trace_id to every record logged with a
// context that carries them (logger.InfoContext and friends).
type logHandler struct {
	slog.Handler
}

// NewLogHandler wraps h so context-aware log calls carry the request id and,
// when a span is recording, the trace id.
func NewLogHandler(h slog.Handler) slog.Handler {
	return logHandler{Handler: h}
}

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{Handler: h.Handler.WithGroup(name)}
}
