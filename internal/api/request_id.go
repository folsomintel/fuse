package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader is the canonical HTTP header used to carry a
// per-request correlation ID. Clients (typically the control-server)
// may set this on inbound requests to propagate an upstream ID;
// otherwise the middleware generates a fresh one.
const RequestIDHeader = "X-Request-ID"

// requestIDKey is the typed context key under which the per-request
// ID is stored. Using a struct{} type avoids string-key collisions
// across packages.
type requestIDKey struct{}

// maxRequestIDLen bounds how much client-supplied X-Request-ID we
// will trust. 128 bytes is far more than any sane correlation ID and
// keeps log lines from blowing up on adversarial input.
const maxRequestIDLen = 128

// RequestID returns the per-request ID from ctx, or "" if absent.
// Use this in handlers and downstream callbacks (auth/IP audit) to
// correlate log lines and audit events with a single request.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// withRequestID returns a derived context carrying id under the
// typed request-ID key.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// validRequestID reports whether s looks like a safe correlation ID:
// non-empty, ≤ maxRequestIDLen, and made up of [A-Za-z0-9_-] only.
// Anything else is treated as adversarial / malformed and replaced
// with a generated ID. We deliberately do not echo arbitrary client
// strings into response headers or log lines.
func validRequestID(s string) bool {
	if s == "" || len(s) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
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
// (validating it against [validRequestID]) and either trusts it or
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
		if !validRequestID(id) {
			id = generateRequestID()
		}

		// Set the response header before invoking the next handler
		// so even handlers that flush early (e.g. SSE streams) carry
		// the ID. WriteHeader is not called here — that's the
		// downstream handler's job.
		w.Header().Set(RequestIDHeader, id)

		ctx := withRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
