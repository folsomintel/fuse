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
// Authorization header against a static Bearer token using
// constant-time comparison. Returns 401 with the standard Error
// envelope on mismatch.
//
// If expectedToken is empty, the middleware is a no-op pass-through
// (insecure/dev mode, matching surfd's Insecure flag pattern).
func BearerAuth(expectedToken string, onFailure AuthFailureFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if expectedToken == "" {
			return next // insecure mode
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				if onFailure != nil {
					onFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusUnauthorized, "unauthorized",
					"missing Authorization header", nil)
				return
			}

			const prefix = "Bearer "
			if !strings.HasPrefix(header, prefix) {
				if onFailure != nil {
					onFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusUnauthorized, "unauthorized",
					"Authorization header must use Bearer scheme", nil)
				return
			}

			token := header[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
				if onFailure != nil {
					onFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusUnauthorized, "unauthorized",
					"invalid token", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
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
