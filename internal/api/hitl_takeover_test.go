package api

import (
	"net/http"
	"testing"
)

func TestHitlTakeover_scopedKeyLocksAgentExec(t *testing.T) {
	f := newAccessFixture(t, &HitlSpec{Requests: HitlRequests{Takeover: &HitlTakeoverRequests{}}})
	env := "fuse-task-a"
	invite := f.mint("master", CreateAPIKeyRequest{
		Scopes: []string{"hitl:takeover"}, EnvID: env, TTLSeconds: 3600,
		Actor: &ActorLabel{ID: "alice", Name: "Alice"},
	})

	// a decide-only key cannot take over.
	decider := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide"}, EnvID: env, TTLSeconds: 3600})
	wantForbidden(t, f.do(http.MethodPost, "/v1/environments/"+env+"?action=takeover", decider, "", nil), reasonMissingScope)
	// the scoped key may not reach any other environment action.
	wantForbidden(t, f.do(http.MethodPost, "/v1/environments/"+env+"?action=drain", invite, "", nil), reasonEnvScope)

	rr := f.do(http.MethodPost, "/v1/environments/"+env+"?action=takeover", invite, "", TakeoverRequest{Reason: "fix the login"})
	if rr.Code != http.StatusOK {
		t.Fatalf("takeover: %d %s", rr.Code, rr.Body.String())
	}
	in := decodeIntervention(t, rr)
	if in.Kind != "takeover" || in.Payload.Takeover == nil || in.Payload.Takeover.Holder == nil || in.Payload.Takeover.Holder.ID != "alice" {
		t.Fatalf("takeover = %+v", in)
	}

	rr = f.do(http.MethodPost, "/v1/environments/"+env+"?action=exec", "master", "", ExecEnvironmentRequest{Cmd: []string{"true"}})
	if rr.Code != http.StatusLocked {
		t.Fatalf("exec during takeover: %d %s, want 423", rr.Code, rr.Body.String())
	}
	e := decodeError(t, rr.Body)
	if e.Error.Code != CodeLocked || e.Error.Details["intervention_id"] != in.ID {
		t.Fatalf("423 body = %+v, want locked with the intervention id", e.Error)
	}

	rr = f.do(http.MethodPost, "/v1/environments/"+env+"?action=release", invite, "", ReleaseRequest{Note: "logged in"})
	if rr.Code != http.StatusOK {
		t.Fatalf("release: %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeIntervention(t, rr); got.State != "approved" || got.Decision.Answer != "logged in" {
		t.Fatalf("released = %+v", got)
	}
	if rr := f.do(http.MethodPost, "/v1/environments/"+env+"?action=exec", "master", "", ExecEnvironmentRequest{Cmd: []string{"true"}}); rr.Code == http.StatusLocked {
		t.Fatalf("exec after release still locked: %s", rr.Body.String())
	}
}

func TestHitlSecret_needsSecretScopeAndKey(t *testing.T) {
	// the test handler has no TOKEN_ENCRYPTION_KEY, so a policy with secret
	// requests is refused at create.
	h, _, _ := newTestHandler(t)
	h.AuthToken = "master"
	f := &accessFixture{t: t, r: mustRouter(t, h), agent: "master"}
	rr := f.do(http.MethodPost, "/v1/environments", "master", "", CreateEnvironmentRequest{
		TaskID: "task-s", ManifestInline: encodeManifest(t),
		Hitl: &HitlSpec{Requests: HitlRequests{Secret: &HitlSecretRequests{Targets: []HitlSecretTarget{{Env: "GH_TOKEN", Label: "token"}}}}},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("create with secret requests and no key: %d %s, want 400", rr.Code, rr.Body.String())
	}

	// an undeclared kind is still a 400 at the request.
	g := newAccessFixture(t, nil)
	rr = g.do(http.MethodPost, "/v1/environments/fuse-task-a?action=request", g.agent, "", RequestHumanRequest{Kind: "secret", Target: "GH_TOKEN"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("secret on an ask-only env: %d %s, want 400", rr.Code, rr.Body.String())
	}
	in := g.ask("fuse-task-a", g.agent, "")
	wantForbidden(t, g.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=secret", g.agent, "", AnswerSecretRequest{Value: "x"}), reasonMissingScope)
}
