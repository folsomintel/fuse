package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/folsomintel/fuse/internal/telemetry"
)

// RequestIDHeader is the canonical HTTP header used to carry a
// per-request correlation ID. Clients (typically the control-server)
// may set this on inbound requests to propagate an upstream ID;
// otherwise the middleware generates a fresh one.
const RequestIDHeader = telemetry.RequestIDHeader

// RequestID returns the per-request ID from ctx, or "" if absent.
// Use this in handlers and downstream callbacks (auth/IP audit) to
// correlate log lines and audit events with a single request. The id
// lives in internal/telemetry so the fleet, the host-agent wire and the
// restate handlers can read it too.
func RequestID(ctx context.Context) string {
	return telemetry.RequestID(ctx)
}

// generateRequestID returns a fresh "req_<32hex>" identifier backed
// by 16 bytes from crypto/rand. The "req_" prefix makes IDs
// trivially greppable in log aggregations and distinguishes
// orchestrator-generated IDs from upstream-supplied ones.
//
// crypto/rand.Read on Go 1.24+ is documented to never fail; we still
// fall back to an empty-but-prefixed sentinel so the middleware can
// always make progress.
func generateRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Should never happen on supported platforms; emit a
		// recognisable sentinel rather than panic so the request
		// can still proceed.
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// RequestIDMiddleware reads X-Request-ID from the inbound request
// (validating it against [telemetry.ValidRequestID]) and either trusts it or
// generates a fresh ID. It then:
//
//  1. Sets the ID on the response via [RequestIDHeader] before any
//     downstream handler can write the response body.
//  2. Stores the ID in the request context under the typed key, so
//     downstream code can fetch it with [RequestID].
//
// This middleware should be mounted as the outermost layer of the
// orchestrator router so every other middleware (CIDR, auth, metrics)
// and every handler observes the same ID. Always succeeds — never
// short-circuits the chain.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !telemetry.ValidRequestID(id) {
			id = generateRequestID()
		}

		// Set the response header before invoking the next handler
		// so even handlers that flush early (e.g. SSE streams) carry
		// the ID. WriteHeader is not called here — that's the
		// downstream handler's job.
		w.Header().Set(RequestIDHeader, id)

		ctx := telemetry.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
