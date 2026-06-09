package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// AuthFailureFunc is invoked once for every rejected authentication
// attempt. It receives the per-request correlation ID (see
// [RequestIDMiddleware]) so audit events and log lines can be tied
// back to the originating request, even when the rejection happens
// before the handler runs.
//
// The requestID is always non-empty when [RequestIDMiddleware] is
// mounted upstream of this middleware (the standard router order).
type AuthFailureFunc func(remoteAddr, method, path, requestID string)

// IPRejectFunc is invoked once for every request rejected by the
// CIDR allowlist. Same shape and contract as [AuthFailureFunc].
type IPRejectFunc func(remoteAddr, method, path, requestID string)

// BearerAuth returns a chi-compatible middleware that validates the
// caller's token against a static Bearer token using constant-time
// comparison. Returns 401 with the standard Error envelope on mismatch.
//
// The token is read from the Authorization header ("Bearer <token>")
// for CLI/server callers, or — when no usable header is present — from
// the SessionCookieName cookie for browser callers (a separate SPA
// stores the token in an HttpOnly cookie via POST /login rather than
// in JavaScript). Either source is compared against the same token.
//
// If expectedToken is empty, the middleware is a no-op pass-through
// (insecure/dev mode, matching fused's Insecure flag pattern).
func BearerAuth(expectedToken string, onFailure AuthFailureFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if expectedToken == "" {
			return next // insecure mode
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := tokenFromRequest(r)
			if !ok {
				if onFailure != nil {
					onFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusUnauthorized, CodeUnauthorized,
					"missing or malformed credentials", nil)
				return
			}

			if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
				if onFailure != nil {
					onFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusUnauthorized, CodeUnauthorized,
					"invalid token", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// tokenFromRequest extracts the caller's bearer token from either the
// Authorization header or the session cookie. The header takes
// precedence; the cookie is only consulted when the header is absent.
// Returns ("", false) when neither yields a usable token.
func tokenFromRequest(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	if header := r.Header.Get("Authorization"); header != "" {
		if !strings.HasPrefix(header, prefix) {
			return "", false
		}
		return header[len(prefix):], true
	}

	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}

	return "", false
}

// CIDRAllowlist returns a middleware that rejects requests whose
// remote address is not within any of the given CIDR blocks. Returns
// 403 with the standard Error envelope on rejection.
//
// If cidrs is empty, the middleware is a no-op pass-through (open
// access). CIDRs are parsed at construction time; an invalid CIDR
// returns an error so the server fails fast at startup rather than
// silently admitting traffic.
func CIDRAllowlist(cidrs []string, onReject IPRejectFunc) (func(http.Handler) http.Handler, error) {
	if len(cidrs) == 0 {
		return func(next http.Handler) http.Handler { return next }, nil
	}

	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		nets = append(nets, ipNet)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := net.ParseIP(host)
			if ip == nil {
				if onReject != nil {
					onReject(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusForbidden, "forbidden",
					"could not parse remote address", nil)
				return
			}

			for _, n := range nets {
				if n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}

			if onReject != nil {
				onReject(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
			}
			writeError(w, http.StatusForbidden, "forbidden",
				"source address not in allowlist", nil)
		})
	}, nil
}
