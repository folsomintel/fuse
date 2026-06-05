package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/surf-dev/surf/apps/orchestrator/internal/core"
)

// errStore is a StateStore that returns a configurable error from
// ListVMs and panics on every other call. We embed *MemoryStateStore
// only to satisfy the StateStore interface for methods we don't
// exercise here; a panic on accidental use would surface a test bug.
type errStore struct {
	*orchestrator.MemoryStateStore
	listErr error
}

func newErrStore(err error) *errStore {
	return &errStore{
		MemoryStateStore: orchestrator.NewMemoryStateStore(),
		listErr:          err,
	}
}

func (s *errStore) ListVMs(_ context.Context) ([]orchestrator.VMRecord, error) {
	return nil, s.listErr
}

// hangStore.ListVMs blocks until the context is cancelled, simulating
// a wedged DB connection. Used to verify the readiness check honours
// its own timeout rather than blocking forever.
type hangStore struct {
	*orchestrator.MemoryStateStore
}

func newHangStore() *hangStore {
	return &hangStore{MemoryStateStore: orchestrator.NewMemoryStateStore()}
}

func (s *hangStore) ListVMs(ctx context.Context) ([]orchestrator.VMRecord, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func newFleetForTest(t *testing.T) *orchestrator.FleetManager {
	t.Helper()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		StateStore: orchestrator.NewMemoryStateStore(),
	})
	return fm
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v (raw=%q)", err, rr.Body.String())
	}
	return out
}

func TestLiveness_AlwaysOK(t *testing.T) {
	hc := &Healthcheck{} // no deps required
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	hc.Liveness(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want json", ct)
	}
	body := decodeBody(t, rr)
	if body["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", body["status"])
	}
}

func TestReadiness_OKWhenAllChecksPass(t *testing.T) {
	hc := &Healthcheck{
		Fleet: newFleetForTest(t),
		Store: orchestrator.NewMemoryStateStore(),
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	hc.Readiness(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["status"] != "ready" {
		t.Fatalf("status = %v, want ready", body["status"])
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("checks not a map: %v", body["checks"])
	}
	if checks["state_store"] != "ok" || checks["fleet"] != "ok" {
		t.Fatalf("checks = %v, want both ok", checks)
	}
}

func TestReadiness_503WhenStateStoreFails(t *testing.T) {
	hc := &Healthcheck{
		Fleet: newFleetForTest(t),
		Store: newErrStore(errors.New("connection refused")),
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	hc.Readiness(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	body := decodeBody(t, rr)
	if body["status"] != "not_ready" {
		t.Fatalf("status = %v, want not_ready", body["status"])
	}
	checks := body["checks"].(map[string]any)
	if checks["fleet"] != "ok" {
		t.Fatalf("fleet check = %v, want ok", checks["fleet"])
	}
	msg, _ := checks["state_store"].(string)
	if !strings.Contains(msg, "connection refused") {
		t.Fatalf("state_store = %q, want it to contain 'connection refused'", msg)
	}
}

func TestReadiness_503WhenFleetNil(t *testing.T) {
	hc := &Healthcheck{
		Fleet: nil,
		Store: orchestrator.NewMemoryStateStore(),
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	hc.Readiness(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	body := decodeBody(t, rr)
	checks := body["checks"].(map[string]any)
	msg, _ := checks["fleet"].(string)
	if !strings.Contains(msg, "fleet not initialized") {
		t.Fatalf("fleet = %q, want 'fleet not initialized'", msg)
	}
	if checks["state_store"] != "ok" {
		t.Fatalf("state_store = %v, want ok", checks["state_store"])
	}
}

// TestReadiness_RespectsTimeout proves that a wedged dependency does
// not stall the probe past its configured CheckTimeout. We use a very
// short timeout so the test stays fast even on a loaded CI machine.
func TestReadiness_RespectsTimeout(t *testing.T) {
	hc := &Healthcheck{
		Fleet:        newFleetForTest(t),
		Store:        newHangStore(),
		CheckTimeout: 100 * time.Millisecond,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)

	start := time.Now()
	hc.Readiness(rr, req)
	elapsed := time.Since(start)

	// Should bail at ~CheckTimeout, well under 1s. We give a generous
	// upper bound to absorb scheduler jitter without flaking.
	if elapsed > time.Second {
		t.Fatalf("readiness took %v, want <1s (timeout=100ms)", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	body := decodeBody(t, rr)
	checks := body["checks"].(map[string]any)
	if checks["state_store"] == "ok" {
		t.Fatalf("state_store = ok, want a timeout error")
	}
}

// TestReadiness_NoAuthHeader verifies the handler doesn't require any
// Authorization header. (This is enforced architecturally by mounting
// outside auth middleware, but we still pin it down here.)
func TestReadiness_NoAuthHeader(t *testing.T) {
	hc := &Healthcheck{
		Fleet: newFleetForTest(t),
		Store: orchestrator.NewMemoryStateStore(),
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	// Intentionally no req.Header.Set("Authorization", ...).
	hc.Readiness(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no auth header", rr.Code)
	}
}

// TestReadiness_TruncatesLongError ensures we don't leak full driver
// stack traces into the probe body.
func TestReadiness_TruncatesLongError(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	hc := &Healthcheck{
		Fleet: newFleetForTest(t),
		Store: newErrStore(errors.New(huge)),
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	hc.Readiness(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	body := decodeBody(t, rr)
	checks := body["checks"].(map[string]any)
	msg, _ := checks["state_store"].(string)
	// Body is capped to readinessErrorMaxLen plus the truncation
	// suffix; allow some slack but assert it's well under the raw
	// length.
	if len(msg) > readinessErrorMaxLen+64 {
		t.Fatalf("state_store error length = %d, want <=%d", len(msg), readinessErrorMaxLen+64)
	}
}
