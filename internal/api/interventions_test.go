package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hitlEnv creates an environment that declares ask requests and returns the
// router and env id.
func hitlEnv(t *testing.T, h *Handler) (http.Handler, string) {
	t.Helper()
	r := mustRouter(t, h)
	rr := doAuthJSON(t, r, http.MethodPost, "/v1/environments", "", CreateEnvironmentRequest{
		TaskID:         "task-hitl",
		ManifestInline: encodeManifest(t),
		Hitl:           &HitlSpec{TimeoutSeconds: 300, Requests: HitlRequests{Ask: &HitlAskRequests{}}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	return r, "fuse-task-hitl"
}

func doAuthJSON(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeIntervention(t *testing.T, rr *httptest.ResponseRecorder) Intervention {
	t.Helper()
	var in Intervention
	if err := json.Unmarshal(rr.Body.Bytes(), &in); err != nil {
		t.Fatalf("decode intervention: %v (%s)", err, rr.Body.String())
	}
	return in
}

func TestCreateEnvironment_rejectsInvalidHitl(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
		Hitl:           &HitlSpec{OnTimeout: "maybe", Requests: HitlRequests{Ask: &HitlAskRequests{}}},
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "on_timeout") {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRequestHuman_askRoundTrip(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r, envID := hitlEnv(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+envID+"?action=request",
		RequestHumanRequest{Kind: "ask", Question: "deploy?", Choices: []string{"yes", "no"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", rr.Code, rr.Body.String())
	}
	in := decodeIntervention(t, rr)
	if in.State != "pending" || in.EnvID != envID || in.Payload.Question != "deploy?" || in.TriggeredBy.ID != "master" {
		t.Fatalf("pending intervention = %+v", in)
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/interventions?env="+envID+"&state=pending", nil)
	var list InterventionList
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if rr.Code != http.StatusOK || len(list.Interventions) != 1 || list.Interventions[0].ID != in.ID {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", DecideInterventionRequest{Answer: "maybe"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("answer outside choices: %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", DecideInterventionRequest{Answer: "yes"})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/interventions/"+in.ID+"?wait=5s", nil)
	got := decodeIntervention(t, rr)
	if got.State != "approved" || got.Decision == nil || got.Decision.Answer != "yes" || got.Decision.Outcome != "approve" {
		t.Fatalf("after approve: %+v", got)
	}

	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+in.ID+"?action=reject", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second decision: %d %s", rr.Code, rr.Body.String())
	}
}

func TestRequestHuman_undeclaredIs400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-plain", ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d", rr.Code)
	}
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-plain?action=request",
		RequestHumanRequest{Kind: "ask", Question: "q"})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "hitl") {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-plain?action=request",
		RequestHumanRequest{Kind: "secret", Question: "q"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d %s", rr.Code, rr.Body.String())
	}
}

func TestGetIntervention_waitTimesOutPending(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r, envID := hitlEnv(t, h)
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/"+envID+"?action=request", RequestHumanRequest{Question: "q"})
	in := decodeIntervention(t, rr)

	start := time.Now()
	rr = doJSON(t, r, http.MethodGet, "/v1/interventions/"+in.ID+"?wait=1", nil)
	if rr.Code != http.StatusOK || decodeIntervention(t, rr).State != "pending" {
		t.Fatalf("wait: %d %s", rr.Code, rr.Body.String())
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("wait returned before the wait elapsed")
	}

	if rr := doJSON(t, r, http.MethodGet, "/v1/interventions/"+in.ID+"?wait=soon", nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad wait: %d", rr.Code)
	}
	if rr := doJSON(t, r, http.MethodGet, "/v1/interventions/int_nope", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rr.Code)
	}
	if rr := doJSON(t, r, http.MethodGet, "/v1/interventions?state=weird", nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad state filter: %d", rr.Code)
	}
}

// an api key can raise a request but only the master token can decide one,
// so an agent holding an ordinary key cannot answer itself.
func TestInterventionAction_masterOnly(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.AuthToken = "master"
	h.APIKeys = &stubKeyStore{liveKey: "fuse_sk_agent", keyID: "ak_agent"}
	r := mustRouter(t, h)

	rr := doAuthJSON(t, r, http.MethodPost, "/v1/environments", "fuse_sk_agent", CreateEnvironmentRequest{
		TaskID:         "task-hitl",
		ManifestInline: encodeManifest(t),
		Hitl:           &HitlSpec{Requests: HitlRequests{Ask: &HitlAskRequests{}}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	rr = doAuthJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-hitl?action=request", "fuse_sk_agent",
		RequestHumanRequest{Question: "may i?"})
	in := decodeIntervention(t, rr)
	if in.TriggeredBy.ID != "key:ak_agent" || in.TriggeredBy.KeyID != "ak_agent" {
		t.Fatalf("triggered_by = %+v", in.TriggeredBy)
	}

	rr = doAuthJSON(t, r, http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", "fuse_sk_agent",
		DecideInterventionRequest{Answer: "yes"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("key decide: %d %s", rr.Code, rr.Body.String())
	}
	rr = doAuthJSON(t, r, http.MethodPost, "/v1/interventions/"+in.ID+"?action=reject", "master",
		DecideInterventionRequest{Reason: "not today"})
	if rr.Code != http.StatusOK {
		t.Fatalf("master decide: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeIntervention(t, rr)
	if got.State != "rejected" || got.Decision.Reason != "not today" || got.Decision.DecidedBy.ID != "master" {
		t.Fatalf("decision = %+v", got.Decision)
	}
}

func TestParseWait(t *testing.T) {
	cases := map[string]time.Duration{"": 0, "5": 5 * time.Second, "2s": 2 * time.Second, "10m": maxInterventionWait}
	for raw, want := range cases {
		got, ok := parseWait(raw)
		if !ok || got != want {
			t.Errorf("parseWait(%q) = %s, %v; want %s", raw, got, ok, want)
		}
	}
	if _, ok := parseWait("-1s"); ok {
		t.Error("negative wait accepted")
	}
}
