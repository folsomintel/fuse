package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/apikeys"
	"github.com/folsomintel/fuse/internal/hitl"
)

// accessFixture is a router with a master token, an agent's full-access key
// and two hitl environments.
type accessFixture struct {
	t     *testing.T
	r     http.Handler
	keys  *stubKeyStore
	agent string
}

func newAccessFixture(t *testing.T, spec *HitlSpec) *accessFixture {
	t.Helper()
	h, _, _ := newTestHandler(t)
	h.AuthToken = "master"
	keys := &stubKeyStore{liveKey: "fuse_sk_agent", keyID: "ak_agent"}
	h.APIKeys = keys
	f := &accessFixture{t: t, r: mustRouter(t, h), keys: keys, agent: "fuse_sk_agent"}
	if spec == nil {
		spec = &HitlSpec{Requests: HitlRequests{Ask: &HitlAskRequests{}}}
	}
	for _, task := range []string{"task-a", "task-b"} {
		rr := f.do(http.MethodPost, "/v1/environments", f.agent, "", CreateEnvironmentRequest{
			TaskID: task, ManifestInline: encodeManifest(t), Hitl: spec,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", task, rr.Code, rr.Body.String())
		}
	}
	return f
}

// do sends a request as token, naming actor in Fuse-Actor when non-empty.
func (f *accessFixture) do(method, path, token, actor string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if actor != "" {
		req.Header.Set(hitl.HeaderActor, actor)
	}
	rr := httptest.NewRecorder()
	f.r.ServeHTTP(rr, req)
	return rr
}

// ask raises an ask on env as token and returns it.
func (f *accessFixture) ask(env, token, actor string) Intervention {
	f.t.Helper()
	rr := f.do(http.MethodPost, "/v1/environments/"+env+"?action=request", token, actor, RequestHumanRequest{Question: "ship it?"})
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("ask: %d %s", rr.Code, rr.Body.String())
	}
	return decodeIntervention(f.t, rr)
}

// mint creates a key as token and returns its raw secret.
func (f *accessFixture) mint(token string, req CreateAPIKeyRequest) string {
	f.t.Helper()
	rr := f.do(http.MethodPost, "/v1/api-keys", token, "", req)
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("mint: %d %s", rr.Code, rr.Body.String())
	}
	var out CreateAPIKeyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Key
}

func wantForbidden(t *testing.T, rr *httptest.ResponseRecorder, reason string) {
	t.Helper()
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d %s, want 403 %s", rr.Code, rr.Body.String(), reason)
	}
	e := decodeError(t, rr.Body)
	if e.Error.Code != CodeForbidden || e.Error.Details["reason"] != reason {
		t.Fatalf("error = %+v, want forbidden/%s", e.Error, reason)
	}
}

func TestHitlAccess_agentCannotApproveItsOwnAsk(t *testing.T) {
	f := newAccessFixture(t, nil)
	in := f.ask("fuse-task-a", f.agent, "")
	rr := f.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", f.agent, "", DecideInterventionRequest{Answer: "yes"})
	wantForbidden(t, rr, reasonMissingScope)
}

func TestHitlAccess_inviteDecidesOnItsEnvOnly(t *testing.T) {
	f := newAccessFixture(t, nil)
	a := f.ask("fuse-task-a", f.agent, "")
	b := f.ask("fuse-task-b", f.agent, "")
	invite := f.mint("master", CreateAPIKeyRequest{
		Scopes: []string{"hitl:decide"}, EnvID: "fuse-task-a", TTLSeconds: 3600,
		Actor: &ActorLabel{ID: "alice", Name: "Alice"},
	})

	rr := f.do(http.MethodPost, "/v1/interventions/"+a.ID+"?action=approve", invite, "", DecideInterventionRequest{Answer: "yes"})
	if rr.Code != http.StatusOK {
		t.Fatalf("invite decide: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeIntervention(t, rr)
	if by := got.Decision.DecidedBy; by.ID != "alice" || by.Source != "pinned" || by.Name != "Alice" {
		t.Fatalf("decided_by = %+v", by)
	}

	// the other environment is invisible, not forbidden.
	for _, path := range []string{
		"/v1/interventions/" + b.ID,
		"/v1/interventions?env=fuse-task-b",
		"/v1/environments/fuse-task-b",
	} {
		if rr := f.do(http.MethodGet, path, invite, "", nil); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, rr.Code)
		}
	}
	if rr := f.do(http.MethodPost, "/v1/interventions/"+b.ID+"?action=approve", invite, "", DecideInterventionRequest{Answer: "yes"}); rr.Code != http.StatusNotFound {
		t.Errorf("decide other env: %d, want 404", rr.Code)
	}
	// its own environment is still off the hitl routes.
	wantForbidden(t, f.do(http.MethodGet, "/v1/environments/fuse-task-a", invite, "", nil), reasonEnvScope)
	wantForbidden(t, f.do(http.MethodGet, "/v1/environments", invite, "", nil), reasonEnvScope)

	// an unfiltered list only shows its env.
	rr = f.do(http.MethodGet, "/v1/interventions?state=all", invite, "", nil)
	var list InterventionList
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	for _, in := range list.Interventions {
		if in.EnvID != "fuse-task-a" {
			t.Errorf("invite listed %s from %s", in.ID, in.EnvID)
		}
	}
}

func TestHitlAccess_actorHeaders(t *testing.T) {
	f := newAccessFixture(t, nil)
	actAs := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide", "hitl:act-as"}, TTLSeconds: 3600})
	pinned := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide"}, TTLSeconds: 3600, Actor: &ActorLabel{ID: "alice"}})

	in := f.ask("fuse-task-a", f.agent, "")
	rr := f.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", actAs, "user_42", DecideInterventionRequest{Answer: "yes"})
	if rr.Code != http.StatusOK {
		t.Fatalf("act-as decide: %d %s", rr.Code, rr.Body.String())
	}
	if by := decodeIntervention(t, rr).Decision.DecidedBy; by.ID != "user_42" || by.Source != "asserted" || by.KeyID == "" {
		t.Fatalf("decided_by = %+v", by)
	}

	wantForbidden(t, f.do(http.MethodGet, "/v1/interventions", f.agent, "bob", nil), reasonActorNotAllowed)
	wantForbidden(t, f.do(http.MethodGet, "/v1/interventions", pinned, "bob", nil), reasonActorMismatch)
	if rr := f.do(http.MethodGet, "/v1/interventions", pinned, "alice", nil); rr.Code != http.StatusOK {
		t.Fatalf("pinned with its own actor: %d", rr.Code)
	}
}

func TestHitlAccess_sameActorOtherKeyIsSelfApproval(t *testing.T) {
	f := newAccessFixture(t, nil)
	k1 := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide", "hitl:act-as"}, TTLSeconds: 3600})
	k2 := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide", "hitl:act-as"}, TTLSeconds: 3600})

	// master raises as bob; bob may not decide it from any key.
	in := f.ask("fuse-task-a", "master", "bob")
	for _, k := range []string{k1, k2} {
		rr := f.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", k, "bob", DecideInterventionRequest{Answer: "yes"})
		wantForbidden(t, rr, reasonSelfApproval)
	}
	if rr := f.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", k1, "carol", DecideInterventionRequest{Answer: "yes"}); rr.Code != http.StatusOK {
		t.Fatalf("carol decide: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHitlAccess_requireActor(t *testing.T) {
	f := newAccessFixture(t, &HitlSpec{RequireActor: true, Requests: HitlRequests{Ask: &HitlAskRequests{}}})
	in := f.ask("fuse-task-a", f.agent, "")
	wantForbidden(t, f.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", "master", "", DecideInterventionRequest{Answer: "yes"}), reasonActorRequired)
	if rr := f.do(http.MethodPost, "/v1/interventions/"+in.ID+"?action=approve", "master", "alice", DecideInterventionRequest{Answer: "yes"}); rr.Code != http.StatusOK {
		t.Fatalf("named master decide: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHitlAccess_minting(t *testing.T) {
	f := newAccessFixture(t, nil)
	decider := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide"}, EnvID: "fuse-task-a", TTLSeconds: 3600, Actor: &ActorLabel{ID: "alice"}})

	cases := []struct {
		name   string
		token  string
		req    CreateAPIKeyRequest
		status int
		reason string
	}{
		{"full-access key cannot mint", f.agent, CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, TTLSeconds: 60}, 403, reasonMissingScope},
		{"scopes need a ttl", "master", CreateAPIKeyRequest{Scopes: []string{"hitl:read"}}, 400, ""},
		{"ttl capped at 24h", "master", CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, TTLSeconds: 86401}, 400, ""},
		{"unknown scope", "master", CreateAPIKeyRequest{Scopes: []string{"admin"}, TTLSeconds: 60}, 400, ""},
		{"env needs scopes", "master", CreateAPIKeyRequest{EnvID: "fuse-task-a"}, 400, ""},
		{"unknown env", "master", CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, EnvID: "fuse-nope", TTLSeconds: 60}, 404, ""},
		{"no scope escalation", decider, CreateAPIKeyRequest{Scopes: []string{"hitl:takeover"}, EnvID: "fuse-task-a", TTLSeconds: 60}, 403, reasonMintEscalation},
		{"no env escalation", decider, CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, EnvID: "fuse-task-b", TTLSeconds: 60}, 403, reasonMintEscalation},
		{"no outliving", decider, CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, EnvID: "fuse-task-a", TTLSeconds: 7200}, 403, reasonMintEscalation},
		{"no other actor", decider, CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, EnvID: "fuse-task-a", TTLSeconds: 60, Actor: &ActorLabel{ID: "bob"}}, 403, reasonMintEscalation},
		{"no full-access child", decider, CreateAPIKeyRequest{}, 403, reasonMintEscalation},
		{"narrower copy", decider, CreateAPIKeyRequest{Scopes: []string{"hitl:read"}, EnvID: "fuse-task-a", TTLSeconds: 60}, 201, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.do(http.MethodPost, "/v1/api-keys", tc.token, "", tc.req)
			if tc.reason != "" {
				wantForbidden(t, rr, tc.reason)
				return
			}
			if rr.Code != tc.status {
				t.Fatalf("got %d %s, want %d", rr.Code, rr.Body.String(), tc.status)
			}
		})
	}
	// the narrower copy inherited the pinned actor.
	last := f.keys.minted[len(f.keys.minted)-1]
	if last.ActorID != "alice" || last.CreatedBy == "master" {
		t.Fatalf("child key = %+v", last)
	}
}

func TestHitlAccess_expiredKeyRejected(t *testing.T) {
	f := newAccessFixture(t, nil)
	past := time.Now().Add(-time.Minute)
	raw := f.keys.addKey(apikeys.APIKeyRecord{ID: "ak_old", Scopes: []string{"hitl:decide"}, ExpiresAt: &past})
	if rr := f.do(http.MethodGet, "/v1/interventions", raw, "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired key: %d, want 401", rr.Code)
	}
}

func TestHitlAccess_auditLog(t *testing.T) {
	f := newAccessFixture(t, nil)
	invite := f.mint("master", CreateAPIKeyRequest{Scopes: []string{"hitl:decide"}, EnvID: "fuse-task-a", TTLSeconds: 3600, Actor: &ActorLabel{ID: "alice"}})
	f.keys.records[invite] = withFirstUse(f.keys.records[invite])
	a := f.ask("fuse-task-a", f.agent, "")
	f.ask("fuse-task-b", f.agent, "")
	if rr := f.do(http.MethodPost, "/v1/interventions/"+a.ID+"?action=reject", invite, "", DecideInterventionRequest{Reason: "no"}); rr.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rr.Code, rr.Body.String())
	}

	rr := f.do(http.MethodGet, "/v1/hitl/audit", "master", "", nil)
	var all AuditList
	if err := json.Unmarshal(rr.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	var events []string
	entries := make([]hitl.AuditEntry, 0, len(all.Entries))
	for _, e := range all.Entries {
		events = append(events, e.Event)
		prev, _ := hex.DecodeString(e.PrevHash)
		hash, _ := hex.DecodeString(e.Hash)
		entries = append(entries, hitl.AuditEntry{
			Seq: e.Seq, Event: hitl.AuditEvent(e.Event), InterventionID: e.InterventionID, EnvID: e.EnvID,
			Actor:      hitl.Actor{ID: e.Actor.ID, Name: e.Actor.Name, Source: hitl.ActorSource(e.Actor.Source), KeyID: e.Actor.KeyID},
			RemoteAddr: e.RemoteAddr, At: e.At, PrevHash: prev, Hash: hash,
		})
	}
	want := []string{"invite_minted", "raised", "raised", "invite_used", "decided"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
	if err := hitl.VerifyChain(entries); err != nil {
		t.Fatalf("chain from the api does not verify: %v", err)
	}
	if d := all.Entries[4]; d.Actor.ID != "alice" || d.Actor.Source != "pinned" || d.InterventionID != a.ID {
		t.Fatalf("decided row = %+v", d)
	}

	// the invite only sees its env's rows.
	rr = f.do(http.MethodGet, "/v1/hitl/audit", invite, "", nil)
	var scoped AuditList
	_ = json.Unmarshal(rr.Body.Bytes(), &scoped)
	for _, e := range scoped.Entries {
		if e.EnvID != "fuse-task-a" {
			t.Errorf("invite saw a row for %q", e.EnvID)
		}
	}
	if rr := f.do(http.MethodGet, "/v1/hitl/audit?env=fuse-task-b", invite, "", nil); rr.Code != http.StatusNotFound {
		t.Errorf("invite audit of another env: %d, want 404", rr.Code)
	}
}

// withFirstUse marks a stub key so its next request is its first use.
func withFirstUse(rec apikeys.APIKeyRecord) apikeys.APIKeyRecord {
	rec.FirstUse = true
	return rec
}

func TestCreateEnvironment_notifySecretMustBeSupplied(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)
	spec := &HitlSpec{
		Requests: HitlRequests{Ask: &HitlAskRequests{}},
		Notify:   []HitlWebhook{{URL: "https://hooks.example.com/fuse", Secret: "HOOK_SECRET"}},
	}
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-n", ManifestInline: encodeManifest(t), Hitl: spec,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing secret: %d %s", rr.Code, rr.Body.String())
	}
}
