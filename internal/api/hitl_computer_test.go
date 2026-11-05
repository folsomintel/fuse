package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestComputerGate_heldWith202(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-computer",
		ManifestInline: encodeManifest(t),
		Hitl: &HitlSpec{Gates: []HitlGate{
			{Name: "typing", On: "computer", Match: HitlGateMatch{Action: []string{"type"}}},
			{Name: "no-card", On: "computer", Match: HitlGateMatch{TextContains: []string{"4242"}}, Mode: "deny"},
		}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	envID := "fuse-task-computer"

	rr = doJSON(t, r, http.MethodPost, "/v1/environments/"+envID+"/computer", map[string]any{"action": "type", "text": "hello"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("type: %d %s", rr.Code, rr.Body.String())
	}
	var held HeldResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &held); err != nil {
		t.Fatal(err)
	}
	c := held.Intervention.Payload.Computer
	if held.Intervention.Kind != "computer" || c == nil || c.Action != "type" || c.Text != "hello" || !strings.Contains(string(c.Input), `"hello"`) {
		t.Fatalf("held = %+v", held.Intervention)
	}

	rr = doJSON(t, r, http.MethodPost, "/v1/environments/"+envID+"/computer", map[string]any{"action": "type", "text": "4242"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("card: %d %s", rr.Code, rr.Body.String())
	}

	text := "goodbye"
	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+held.InterventionID+"?action=approve", DecideInterventionRequest{EditedText: &text})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	in := decodeIntervention(t, rr)
	if in.Decision.EditedText == nil || *in.Decision.EditedText != text {
		t.Fatalf("decision = %+v", in.Decision)
	}
}

func TestReviewRequest_rejectedWithComments(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-review",
		ManifestInline: encodeManifest(t),
		Hitl:           &HitlSpec{Requests: HitlRequests{Review: &HitlReviewRequests{}}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	diff := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-x\n+y\n"
	rr = doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-review?action=request", RequestHumanRequest{Kind: "review", Content: diff, Message: "fix"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", rr.Code, rr.Body.String())
	}
	in := decodeIntervention(t, rr)
	if in.Kind != "review" || in.Payload.Review == nil || in.Payload.Review.Format != "diff" || in.Payload.Review.Content != diff {
		t.Fatalf("review = %+v", in.Payload)
	}
	rr = doJSON(t, r, http.MethodPost, "/v1/interventions/"+in.ID+"?action=reject", DecideInterventionRequest{Reason: "y should stay x"})
	if rr.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeIntervention(t, rr); got.Decision.Reason != "y should stay x" || got.Decision.Outcome != "reject" {
		t.Fatalf("decision = %+v", got.Decision)
	}
}

func TestCreateEnvironment_hitlDesktopAndDeciderChecks(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	cases := map[string]*HitlSpec{
		"desktop needs a desktop block": {Requests: HitlRequests{Takeover: &HitlTakeoverRequests{Via: []string{"desktop"}}}},
		"is not in secrets": {Gates: []HitlGate{{Name: "g", On: "computer", Match: HitlGateMatch{Action: []string{"type"}},
			Decide: &HitlDecider{URL: "https://decide.example", Secret: "NOPE"}}}},
	}
	for want, spec := range cases {
		rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
			TaskID: "task-bad-hitl", ManifestInline: encodeManifest(t), Hitl: spec,
		})
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("%s: %d %s", want, rr.Code, rr.Body.String())
		}
	}
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-desktop-hitl", ManifestInline: encodeManifest(t),
		Desktop: &DesktopSpec{Width: 1280, Height: 800},
		Hitl:    &HitlSpec{Requests: HitlRequests{Takeover: &HitlTakeoverRequests{Via: []string{"desktop"}}}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("desktop takeover with a desktop block: %d %s", rr.Code, rr.Body.String())
	}
}
