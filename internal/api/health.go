package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// defaultHealthCheckTimeout bounds each readiness dependency check.
// Chosen to be longer than typical Postgres round-trips but short
// enough to fit comfortably inside k8s default probe periodSeconds=10.
const defaultHealthCheckTimeout = 2 * time.Second

// readinessErrorMaxLen caps per-check error messages so we don't leak
// stack traces or huge driver errors into probe responses.
const readinessErrorMaxLen = 200

// Healthcheck wires /health, /ready, and /v1/version handlers.
//
// /health:     liveness probe. Always 200; no dependencies.
// /ready:      readiness probe. 200 when state store + fleet are reachable.
//
//	503 with a JSON breakdown when any check fails.
//
// /v1/version: identifies the process as the fuse orchestrator.
//
// All three are plaintext / unauthenticated by design — k8s and ALB probes
// don't (and shouldn't) carry bearer tokens, and a CLI probing an unknown
// URL can't carry a confirmed token yet either. This handler is mounted on
// the outer mux in server/main.go alongside /metrics, outside the auth +
// CIDR middleware chain installed by Handler.Router.
type Healthcheck struct {
	// Fleet is the FleetManager whose readiness we report. A nil
	// pointer is treated as "not initialized" and fails readiness.
	Fleet *orchestrator.FleetManager

	// Store is the durable state backend (Postgres in prod, in-memory
	// in tests). Readiness performs a cheap ListVMs against it.
	Store orchestrator.StateStore

	// CheckTimeout bounds each readiness dependency check. If zero,
	// defaults to defaultHealthCheckTimeout.
	CheckTimeout time.Duration

	// BuildVersion is the orchestrator build version reported by
	// /v1/version. Empty renders as "dev" (matching main.version's zero
	// value).
	BuildVersion string
}

// Liveness reports that the process is up and able to serve HTTP.
// It deliberately does no work and depends on nothing — kubelet uses
// this to decide whether to restart the pod.
func (h *Healthcheck) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Version identifies this process as the fuse orchestrator, unauthenticated
// so a caller can tell it apart from a host agent (or nothing at all)
// before it has a valid token. `fuse connect` probes this before saving a
// context.
func (h *Healthcheck) Version(w http.ResponseWriter, _ *http.Request) {
	v := h.BuildVersion
	if v == "" {
		v = "dev"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "fuse-orchestrator",
		"version": v,
	})
}

// Readiness reports whether the orchestrator can currently serve
// traffic. It checks:
//
//   - state_store: a bounded ListVMs call. Failures here typically
//     mean Postgres is unreachable or the connection pool is wedged.
//   - fleet: that the FleetManager is non-nil. (We can't easily check
//     "Started" without poking at unexported state; a nil manager is
//     the realistic failure mode at boot.)
//
// On any failure the response is 503 with a per-check breakdown so
// operators can see at a glance which dependency is unhappy.
func (h *Healthcheck) Readiness(w http.ResponseWriter, r *http.Request) {
	timeout := h.CheckTimeout
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}

	checks := map[string]string{
		"state_store": "ok",
		"fleet":       "ok",
	}
	allOK := true

	// Fleet check. Cheap — just a nil guard. We intentionally do not
	// reach into FleetManager internals for a "started" flag because
	// that would require coupling the API package to unexported
	// fields. A nil pointer is the only realistic failure mode here.
	if h.Fleet == nil {
		checks["fleet"] = "fleet not initialized"
		allOK = false
	}

	// State store check. Use a bounded context so a wedged DB can't
	// hold the probe open for the server's global write timeout.
	if h.Store == nil {
		checks["state_store"] = "state store not configured"
		allOK = false
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		if _, err := h.Store.ListVMs(ctx); err != nil {
			checks["state_store"] = truncateErr(err.Error())
			allOK = false
		}
		cancel()
	}

	status := "ready"
	httpStatus := http.StatusOK
	if !allOK {
		status = "not_ready"
		httpStatus = http.StatusServiceUnavailable
	}

	// Use writeJSON-compatible shape but emit explicitly so the body
	// schema matches the openapi spec exactly.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"checks": checks,
	})
}

// truncateErr clips an error message so it cannot blow out the
// readiness body or smuggle a stack trace into the response.
func truncateErr(s string) string {
	if len(s) <= readinessErrorMaxLen {
		return s
	}
	return s[:readinessErrorMaxLen] + "…(truncated)"
}
