package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// putAllow sends PUT /v1/environments/{id}/egress/allow with body.
func putAllow(t *testing.T, r http.Handler, vmID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, r, http.MethodPut, "/v1/environments/"+vmID+"/egress/allow", body)
}

func decodeEgressStatus(t *testing.T, rr *httptest.ResponseRecorder) EgressStatus {
	t.Helper()
	var st EgressStatus
	if err := json.NewDecoder(rr.Body).Decode(&st); err != nil {
		t.Fatalf("decode egress status: %v", err)
	}
	return st
}

func getEnv(t *testing.T, r http.Handler, vmID string) Environment {
	t.Helper()
	rr := doJSON(t, r, http.MethodGet, "/v1/environments/"+vmID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get %s: %d %s", vmID, rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

// TestCreateEnvironment_egressAllow: a create's allowlist lands normalized
// on the spec the provider creates from, and on the environment's status.
func TestCreateEnvironment_egressAllow(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "t-allow",
		ManifestInline: encodeManifest(t),
		Egress:         &EgressSpec{Allow: []string{"10.1.2.3/8", "1.2.3.4", "10.0.0.0/8"}},
	})
	want := []string{"10.0.0.0/8", "1.2.3.4/32"}
	if env.Egress == nil || env.Egress.Mode != "direct" || !reflect.DeepEqual(env.Egress.Allow, want) {
		t.Fatalf("egress = %+v, want direct with %v", env.Egress, want)
	}
	if got := p.createdSpec(env.ID).Egress.Allow; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider spec allow = %v, want %v", got, want)
	}
	if got := getEnv(t, r, env.ID).Egress.Allow; !reflect.DeepEqual(got, want) {
		t.Fatalf("read allow = %v, want %v", got, want)
	}
}

func TestSetEgressAllow_replacesAndLifts(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := createEnv(t, r, CreateEnvironmentRequest{TaskID: "t-put", ManifestInline: encodeManifest(t)})

	rr := putAllow(t, r, env.ID, SetEgressAllowRequest{Allow: []string{"192.168.1.7/24", "8.8.8.8"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
	}
	want := []string{"192.168.1.0/24", "8.8.8.8/32"}
	if st := decodeEgressStatus(t, rr); st.Mode != "direct" || !reflect.DeepEqual(st.Allow, want) {
		t.Fatalf("status = %+v, want direct with %v", st, want)
	}
	// the host got the list already normalized, as the api validated it.
	if len(p.allowCalls) != 1 || !reflect.DeepEqual(p.allowCalls[0], want) {
		t.Fatalf("host calls = %v, want one with %v", p.allowCalls, want)
	}
	if got := getEnv(t, r, env.ID).Egress.Allow; !reflect.DeepEqual(got, want) {
		t.Fatalf("read allow = %v, want %v", got, want)
	}

	rr = putAllow(t, r, env.ID, SetEgressAllowRequest{Allow: []string{}})
	if rr.Code != http.StatusOK {
		t.Fatalf("lift: %d %s", rr.Code, rr.Body.String())
	}
	if st := decodeEgressStatus(t, rr); st.Allow != nil {
		t.Fatalf("lifted status allow = %v, want none", st.Allow)
	}
	if len(p.allowCalls) != 2 || len(p.allowCalls[1]) != 0 {
		t.Fatalf("lift host call = %v, want an empty list", p.allowCalls)
	}
	if got := getEnv(t, r, env.ID).Egress.Allow; got != nil {
		t.Fatalf("read allow after lift = %v, want none", got)
	}

	// null lifts too: it is how a nil slice marshals in go.
	req := httptest.NewRequest(http.MethodPut, "/v1/environments/"+env.ID+"/egress/allow", strings.NewReader(`{"allow":null}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || len(p.allowCalls) != 3 || len(p.allowCalls[2]) != 0 {
		t.Fatalf("null lift: %d %s, host calls %v", rr.Code, rr.Body.String(), p.allowCalls)
	}
}

func TestSetEgressAllow_refusals(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		allowErr error
		status   int
		code     string
		reached  bool
	}{
		{"missing allow", `{}`, nil, http.StatusBadRequest, CodeInvalidArgument, false},
		{"allow not a list", `{"allow":"10.0.0.0/8"}`, nil, http.StatusBadRequest, CodeInvalidArgument, false},
		{"bad cidr", `{"allow":["10.0.0.0/8","nope"]}`, nil, http.StatusBadRequest, CodeInvalidArgument, false},
		{"too many", `{"allow":[` + manyCIDRs(257) + `]}`, nil, http.StatusBadRequest, CodeInvalidArgument, false},
		{"agent 400", `{"allow":["10.0.0.0/8"]}`, &orchestrator.HTTPStatusError{Code: 400, Body: "not an ipv4 cidr"}, http.StatusBadRequest, CodeInvalidArgument, true},
		{"agent 409", `{"allow":["10.0.0.0/8"]}`, &orchestrator.HTTPStatusError{Code: 409, Body: "proxy egress"}, http.StatusConflict, CodeConflict, true},
		{"agent 404", `{"allow":["10.0.0.0/8"]}`, &orchestrator.HTTPStatusError{Code: 404, Body: "vm not found"}, http.StatusNotFound, CodeNotFound, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, p := newTestHandler(t)
			r := mustRouter(t, h)
			env := createEnv(t, r, CreateEnvironmentRequest{
				TaskID:         "t-refuse",
				ManifestInline: encodeManifest(t),
				Egress:         &EgressSpec{Allow: []string{"1.1.1.1"}},
			})
			p.allowErr = tc.allowErr

			req := httptest.NewRequest(http.MethodPut, "/v1/environments/"+env.ID+"/egress/allow", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d. body: %s", rr.Code, tc.status, rr.Body.String())
			}
			if e := decodeError(t, rr.Body); e.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", e.Error.Code, tc.code)
			}
			if reached := len(p.allowCalls) > 0; reached != tc.reached {
				t.Fatalf("host reached = %v, want %v", reached, tc.reached)
			}
			// a refused set keeps the list the host still enforces.
			if got := getEnv(t, r, env.ID).Egress.Allow; !reflect.DeepEqual(got, []string{"1.1.1.1/32"}) {
				t.Fatalf("allow after refusal = %v, want the create's", got)
			}
		})
	}
}

func manyCIDRs(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"10.%d.%d.0/24"`, i/256, i%256)
	}
	return strings.Join(parts, ",")
}

func TestSetEgressAllow_unknownEnvIs404(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	rr := putAllow(t, r, "fuse-missing", SetEgressAllowRequest{Allow: []string{"10.0.0.0/8"}})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", rr.Code, rr.Body.String())
	}
	if len(p.allowCalls) != 0 {
		t.Fatalf("host reached for an unknown env: %v", p.allowCalls)
	}
}

func TestSetEgressAllow_proxyEnvIs409(t *testing.T) {
	h, _ := newEgressTestHandler(t)
	r := mustRouter(t, h)
	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "t-proxy-allow",
		ManifestInline: encodeManifest(t),
		Egress:         &EgressSpec{Mode: "proxy", Provider: "mock"},
	})
	rr := putAllow(t, r, env.ID, SetEgressAllowRequest{Allow: []string{"10.0.0.0/8"}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if e := decodeError(t, rr.Body); e.Error.Code != CodeConflict || !strings.Contains(e.Error.Message, "proxy") {
		t.Fatalf("error = %+v, want a proxy conflict", e.Error)
	}
}

func TestSetEgressAllow_notRunningIs409(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := createEnv(t, r, CreateEnvironmentRequest{TaskID: "t-drained", ManifestInline: encodeManifest(t)})
	drainEnv(t, r, env.ID)
	rr := putAllow(t, r, env.ID, SetEgressAllowRequest{Allow: []string{"10.0.0.0/8"}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	if len(p.allowCalls) != 0 {
		t.Fatalf("host reached for a drained env: %v", p.allowCalls)
	}
}

// TestSetEgressAllow_providerWithoutSupportIs501: a provider whose agent
// cannot replace a list is a capability gap, like exec on the stub.
func TestSetEgressAllow_providerWithoutSupportIs501(t *testing.T) {
	h, _, _ := newPlainHandler(t)
	r := mustRouter(t, h)
	env := createEnv(t, r, CreateEnvironmentRequest{TaskID: "t-plain", ManifestInline: encodeManifest(t)})
	rr := putAllow(t, r, env.ID, SetEgressAllowRequest{Allow: []string{"10.0.0.0/8"}})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501. body: %s", rr.Code, rr.Body.String())
	}
}

// TestSetEgressAllow_idempotencyKeyReplays: a retry under the same key gets
// the first answer back without a second host call.
func TestSetEgressAllow_idempotencyKeyReplays(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := createEnv(t, r, CreateEnvironmentRequest{TaskID: "t-idem", ManifestInline: encodeManifest(t)})

	send := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/v1/environments/"+env.ID+"/egress/allow", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderIdempotencyKey, "allow-1")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}
	first := send(`{"allow":["10.0.0.0/8"]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	again := send(`{"allow":["10.0.0.0/8"]}`)
	if again.Code != http.StatusOK || again.Header().Get(HeaderIdempotentReplayed) != "true" {
		t.Fatalf("replay: %d replayed=%q %s", again.Code, again.Header().Get(HeaderIdempotentReplayed), again.Body.String())
	}
	if len(p.allowCalls) != 1 {
		t.Fatalf("host calls = %d, want 1", len(p.allowCalls))
	}
	if other := send(`{"allow":[]}`); other.Code != http.StatusConflict {
		t.Fatalf("same key, other body: %d, want 409", other.Code)
	}
}

// TestForkEnvironment_carriesTheCurrentAllowlist: a fork is created with the
// list its source enforces now, not the one it was created with.
func TestForkEnvironment_carriesTheCurrentAllowlist(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)
	env := createEnv(t, r, CreateEnvironmentRequest{
		TaskID:         "t-fork-allow",
		ManifestInline: encodeManifest(t),
		Egress:         &EgressSpec{Allow: []string{"1.1.1.1"}},
	})
	if rr := putAllow(t, r, env.ID, SetEgressAllowRequest{Allow: []string{"10.0.0.0/8"}}); rr.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+env.ID+"?action=fork", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork: %d %s", rr.Code, rr.Body.String())
	}
	var out ForkEnvironmentResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Children) != 1 {
		t.Fatalf("children = %+v", out.Children)
	}
	child := out.Children[0]
	want := []string{"10.0.0.0/8"}
	if child.Egress == nil || !reflect.DeepEqual(child.Egress.Allow, want) {
		t.Fatalf("fork egress = %+v, want allow %v", child.Egress, want)
	}
	if got := p.createdSpec(child.ID).Egress.Allow; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork created with allow %v, want %v", got, want)
	}
}
