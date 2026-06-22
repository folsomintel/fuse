package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestMetricsMiddleware() func(http.Handler) http.Handler {
	return MetricsMiddleware(
		prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_requests_total"}, []string{"route", "method", "code"}),
		prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_request_duration_seconds"}, []string{"route", "method"}),
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_requests_in_flight"}),
	)
}

// TestMetricsMiddlewarePreservesFlusher guards against the regression where the
// statusWriter wrapper hid http.Flusher from streaming handlers, causing SSE to
// fail with "streaming unsupported by transport".
func TestMetricsMiddlewarePreservesFlusher(t *testing.T) {
	var sawFlusher bool
	h := newTestMetricsMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !sawFlusher {
		t.Fatal("handler behind MetricsMiddleware did not see an http.Flusher")
	}
}

// flushRecorder records whether Flush was called on the underlying writer.
type flushRecorder struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestStatusWriterFlushAndUnwrap(t *testing.T) {
	rec := &flushRecorder{ResponseWriter: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}

	f, ok := any(sw).(http.Flusher)
	if !ok {
		t.Fatal("statusWriter does not implement http.Flusher")
	}
	f.Flush()
	if !rec.flushed {
		t.Fatal("Flush did not propagate to the wrapped writer")
	}

	if sw.Unwrap() != rec {
		t.Fatal("Unwrap did not return the wrapped writer")
	}

	// http.ResponseController must also be able to flush through the wrapper.
	rec.flushed = false
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush: %v", err)
	}
	if !rec.flushed {
		t.Fatal("ResponseController.Flush did not propagate through Unwrap")
	}
}
