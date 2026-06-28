package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// MetricsMiddleware records RED (Rate, Errors, Duration) metrics for
// every HTTP request using the chi route pattern as the "route" label,
// keeping cardinality bounded regardless of path parameters.
func MetricsMiddleware(
	requestsTotal *prometheus.CounterVec,
	requestDuration *prometheus.HistogramVec,
	requestsInFlight prometheus.Gauge,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestsInFlight.Inc()
			defer requestsInFlight.Dec()

			start := time.Now()
			ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(ww, r)

			elapsed := time.Since(start).Seconds()

			// Use chi's route pattern so /v1/environments/{vmId} is the
			// label, not /v1/environments/fuse-abc123.
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unknown"
			}
			method := r.Method
			code := strconv.Itoa(ww.status)

			requestsTotal.WithLabelValues(route, method, code).Inc()
			requestDuration.WithLabelValues(route, method).Observe(elapsed)
		})
	}
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so streaming handlers (e.g. SSE) keep
// working behind this middleware. Without it, a handler's
// w.(http.Flusher) assertion fails on the wrapper and the stream is
// rejected. Delegates to the underlying writer when it supports flushing.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped ResponseWriter so http.ResponseController can
// reach transport capabilities (SetWriteDeadline, Hijack, ...) through the
// wrapper. See the SSE handler's use of http.NewResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
