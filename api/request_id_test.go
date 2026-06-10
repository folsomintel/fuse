package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// generatedIDPattern matches the format produced by generateRequestID.
// Tests assert against this rather than the literal length so a future
// bump to the random-byte size is a one-line change.
var generatedIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

// newRequestIDRouter mounts only RequestIDMiddleware in front of a
// handler that captures the in-context ID. Returns the router plus a
// pointer that the handler writes through, so each test can read the
// observed ID without juggling channels.
func newRequestIDRouter(t *testing.T, observed *string) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(RequestIDMiddleware)
	r.Get("/echo", func(w http.ResponseWriter, req *http.Request) {
		*observed = RequestID(req.Context())
		w.WriteHeader(http.StatusOK)
	})
	return r
}

func TestRequestID_PropagatesValid(t *testing.T) {
	var observed string
	r := newRequestIDRouter(t, &observed)

	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set(RequestIDHeader, "abc-123")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Result().StatusCode; got != http.StatusOK {
		t.Fatalf("status: got %d, want 200", got)
	}
	if got := rec.Header().Get(RequestIDHeader); got != "abc-123" {
		t.Fatalf("response %s: got %q, want %q", RequestIDHeader, got, "abc-123")
	}
	if observed != "abc-123" {
		t.Fatalf("ctx RequestID: got %q, want %q", observed, "abc-123")
	}
}

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var observed string
	r := newRequestIDRouter(t, &observed)

	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	hdr := rec.Header().Get(RequestIDHeader)
	if !generatedIDPattern.MatchString(hdr) {
		t.Fatalf("response %s: %q does not match %s", RequestIDHeader, hdr, generatedIDPattern)
	}
	if observed != hdr {
		t.Fatalf("ctx ID %q != response header %q (must match)", observed, hdr)
	}
}

func TestRequestID_RejectsMalformed(t *testing.T) {
	var observed string
	r := newRequestIDRouter(t, &observed)

	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set(RequestIDHeader, "$$$weird$$$")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	hdr := rec.Header().Get(RequestIDHeader)
	if hdr == "$$$weird$$$" {
		t.Fatalf("response %s echoed malformed client value", RequestIDHeader)
	}
	if !generatedIDPattern.MatchString(hdr) {
		t.Fatalf("response %s: %q does not match generated pattern", RequestIDHeader, hdr)
	}
	if observed != hdr {
		t.Fatalf("ctx ID %q != response header %q", observed, hdr)
	}
}

func TestRequestID_BoundsLength(t *testing.T) {
	var observed string
	r := newRequestIDRouter(t, &observed)

	long := strings.Repeat("a", 200)
	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set(RequestIDHeader, long)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	hdr := rec.Header().Get(RequestIDHeader)
	if hdr == long {
		t.Fatalf("response %s echoed oversized client value (len=%d)", RequestIDHeader, len(hdr))
	}
	if !generatedIDPattern.MatchString(hdr) {
		t.Fatalf("response %s: %q does not match generated pattern", RequestIDHeader, hdr)
	}
}

func TestRequestID_AvailableInContext(t *testing.T) {
	var observed string
	r := newRequestIDRouter(t, &observed)

	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if observed == "" {
		t.Fatal("RequestID(ctx) returned empty inside handler")
	}
	if observed != rec.Header().Get(RequestIDHeader) {
		t.Fatalf("ctx ID %q != response header %q", observed, rec.Header().Get(RequestIDHeader))
	}
}

// TestRequestID_AbsentContext checks the ergonomics of the helper for
// callers that pass a context without the middleware (e.g. unit
// tests or background workers): it must return "" not panic.
func TestRequestID_AbsentContext(t *testing.T) {
	if got := RequestID(nil); got != "" { //nolint:staticcheck // intentionally testing nil-ctx ergonomics
		t.Fatalf("RequestID(nil): got %q, want \"\"", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := RequestID(req.Context()); got != "" {
		t.Fatalf("RequestID(empty ctx): got %q, want \"\"", got)
	}
}

// TestAuthFailureIncludesRequestID verifies that the OnAuthFailure
// callback receives the same request ID that lands in the response
// header, proving the middleware ordering in Router() (request-ID
// outermost, auth inner) holds end-to-end.
func TestAuthFailureIncludesRequestID(t *testing.T) {
	var seenAddr, seenMethod, seenPath, seenReqID string
	cb := func(remoteAddr, method, path, requestID string) {
		seenAddr = remoteAddr
		seenMethod = method
		seenPath = path
		seenReqID = requestID
	}

	r := chi.NewRouter()
	r.Use(RequestIDMiddleware)
	r.Use(BearerAuth("expected-token", nil, cb))
	r.Get("/v1/foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	const upstreamID = "upstream-42"
	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.Header.Set(RequestIDHeader, upstreamID)
	// No Authorization header -> auth must fail.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Result().StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", got)
	}
	if seenReqID != upstreamID {
		t.Fatalf("callback request_id: got %q, want %q", seenReqID, upstreamID)
	}
	if rec.Header().Get(RequestIDHeader) != upstreamID {
		t.Fatalf("response header: got %q, want %q",
			rec.Header().Get(RequestIDHeader), upstreamID)
	}
	if seenMethod != http.MethodGet || seenPath != "/v1/foo" {
		t.Fatalf("callback method/path: got %s %s, want GET /v1/foo", seenMethod, seenPath)
	}
	if seenAddr == "" {
		t.Fatal("callback remoteAddr empty")
	}
}

// TestIPRejectIncludesRequestID is the CIDR-allowlist counterpart to
// TestAuthFailureIncludesRequestID.
func TestIPRejectIncludesRequestID(t *testing.T) {
	var seenReqID string
	cb := func(remoteAddr, method, path, requestID string) {
		seenReqID = requestID
	}

	mw, err := CIDRAllowlist([]string{"10.0.0.0/8"}, cb)
	if err != nil {
		t.Fatalf("CIDRAllowlist: %v", err)
	}

	r := chi.NewRouter()
	r.Use(RequestIDMiddleware)
	r.Use(mw)
	r.Get("/v1/foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.RemoteAddr = "192.168.1.1:1234" // outside 10.0.0.0/8
	req.Header.Set(RequestIDHeader, "ip-test-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Result().StatusCode; got != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", got)
	}
	if seenReqID != "ip-test-1" {
		t.Fatalf("callback request_id: got %q, want %q", seenReqID, "ip-test-1")
	}
}

// TestValidRequestID exercises the validator's edge cases directly so
// future tweaks to the character class are caught immediately.
func TestValidRequestID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"abc", true},
		{"abc-123_XYZ", true},
		{"req_0123456789abcdef", true},
		{"has space", false},
		{"has.dot", false},
		{"has/slash", false},
		{"has:colon", false},
		{strings.Repeat("a", maxRequestIDLen), true},
		{strings.Repeat("a", maxRequestIDLen+1), false},
	}
	for _, tc := range cases {
		if got := validRequestID(tc.in); got != tc.want {
			t.Errorf("validRequestID(%q): got %v, want %v", tc.in, got, tc.want)
		}
	}
}
