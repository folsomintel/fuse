package fuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWithActorCopySendsHeaders(t *testing.T) {
	var seen []http.Header
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		io.WriteString(w, `{"api_keys":[]}`)
	})
	defer cleanup()

	as := c.WithActor(ActorLabel{ID: "u_1", Name: "Ada"})
	if _, err := as.APIKeys.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.APIKeys.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen[0].Get("Fuse-Actor") != "u_1" || seen[0].Get("Fuse-Actor-Name") != "Ada" {
		t.Fatalf("copy headers = %v", seen[0])
	}
	if seen[0].Get("Authorization") != "Bearer tok" {
		t.Fatalf("copy lost the bearer: %v", seen[0])
	}
	if seen[1].Get("Fuse-Actor") != "" || seen[1].Get("Fuse-Actor-Name") != "" {
		t.Fatalf("original client sent actor headers: %v", seen[1])
	}
}

func TestActorOptionSendsHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		io.WriteString(w, `{"api_keys":[]}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "tok", WithActor(ActorLabel{ID: "u_2"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.APIKeys.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Get("Fuse-Actor") != "u_2" {
		t.Fatalf("Fuse-Actor = %q", got.Get("Fuse-Actor"))
	}
	if _, ok := got["Fuse-Actor-Name"]; ok {
		t.Fatal("empty name was sent")
	}
}

func TestCreateScopedAPIKey(t *testing.T) {
	var body map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"k_1","key":"fuse_x","created_at":"2026-10-01T00:00:00Z",
			"scopes":["hitl:read","hitl:decide"],"env_id":"vm-1","expires_at":"2026-10-01T01:00:00Z",
			"actor":{"id":"u_1","name":"Ada"},"created_by":"master"}`)
	})
	defer cleanup()

	key, err := c.APIKeys.CreateWithOptions(context.Background(), CreateAPIKeyOptions{
		Label:  "reviewer",
		Scopes: []string{ScopeHitlRead, ScopeHitlDecide},
		EnvID:  "vm-1",
		TTL:    90 * time.Minute,
		Actor:  &ActorLabel{ID: "u_1", Name: "Ada"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["label"] != "reviewer" || body["env_id"] != "vm-1" || body["ttl_seconds"] != float64(5400) {
		t.Fatalf("request body = %v", body)
	}
	if scopes, _ := body["scopes"].([]any); len(scopes) != 2 || scopes[0] != "hitl:read" {
		t.Fatalf("scopes on the wire = %v", body["scopes"])
	}
	if actor, _ := body["actor"].(map[string]any); actor["id"] != "u_1" || actor["name"] != "Ada" {
		t.Fatalf("actor on the wire = %v", body["actor"])
	}
	if key.Key != "fuse_x" || len(key.Scopes) != 2 || key.EnvID != "vm-1" || key.CreatedBy != "master" {
		t.Fatalf("key = %+v", key)
	}
	if key.ExpiresAt == nil || !key.ExpiresAt.Equal(time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires_at = %v", key.ExpiresAt)
	}
	if key.Actor == nil || key.Actor.ID != "u_1" || key.Actor.Name != "Ada" {
		t.Fatalf("actor = %+v", key.Actor)
	}
}

func TestCreateAPIKeyLabelOnly(t *testing.T) {
	var raw string
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = strings.TrimSpace(string(b))
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"k_1","key":"fuse_x"}`)
	})
	defer cleanup()
	key, err := c.APIKeys.Create(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	if raw != `{"label":"ci"}` {
		t.Fatalf("body = %s", raw)
	}
	if key.Scopes != nil || key.Actor != nil || key.ExpiresAt != nil {
		t.Fatalf("full-access key = %+v", key)
	}
}

func TestInterventionsAudit(t *testing.T) {
	var queries []string
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hitl/audit" {
			t.Errorf("path = %s", r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("cursor") == "" {
			io.WriteString(w, `{"entries":[{"seq":7,"event":"decided","intervention_id":"int_1","env_id":"vm-1",
				"actor":{"id":"u_1","name":"Ada","source":"key","key_id":"k_1"},"remote_addr":"10.0.0.1:5000",
				"at":"2026-10-01T00:00:00Z","prev_hash":"aa","hash":"bb"}],"next_cursor":"c1"}`)
			return
		}
		io.WriteString(w, `{"entries":[{"seq":8,"event":"expired","actor":{"id":"system","source":"system"},"at":"2026-10-01T00:01:00Z","prev_hash":"bb","hash":"cc"}]}`)
	})
	defer cleanup()

	page, err := c.Interventions.Audit(context.Background(), AuditOptions{EnvID: "vm-1", Actor: "u_1", Since: "24h", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	q := queries[0]
	for _, want := range []string{"env=vm-1", "actor=u_1", "since=24h", "limit=10"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q is missing %s", q, want)
		}
	}
	if page.NextCursor != "c1" || len(page.Entries) != 1 {
		t.Fatalf("page = %+v", page)
	}
	e := page.Entries[0]
	if e.Seq != 7 || e.Event != AuditDecided || e.InterventionID != "int_1" || e.EnvID != "vm-1" ||
		e.Actor.KeyID != "k_1" || e.Actor.Name != "Ada" || e.RemoteAddr != "10.0.0.1:5000" ||
		e.PrevHash != "aa" || e.Hash != "bb" || !e.At.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("entry = %+v", e)
	}

	all, err := c.Interventions.AuditAll(context.Background(), AuditOptions{})
	if err != nil || len(all) != 2 || all[1].Event != AuditExpired {
		t.Fatalf("all = %+v, %v", all, err)
	}
}

func TestListInterventionsActorFilter(t *testing.T) {
	var q string
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		io.WriteString(w, `{"interventions":[]}`)
	})
	defer cleanup()
	if _, err := c.Interventions.ListPage(context.Background(), ListInterventionsOptions{Actor: "u_1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "actor=u_1") {
		t.Fatalf("query = %q", q)
	}
}

func TestForbiddenSelfApproval(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"forbidden","message":"an intervention cannot be decided by the key or actor that raised it","details":{"reason":"self_approval"}}}`)
	})
	defer cleanup()
	_, err := c.Interventions.Approve(context.Background(), "int_1", "yes", "")
	if !IsForbidden(err) || ForbiddenReason(err) != ForbiddenSelfApproval || ForbiddenScope(err) != "" {
		t.Fatalf("err = %v (forbidden %v, reason %q)", err, IsForbidden(err), ForbiddenReason(err))
	}
	if IsUnauthorized(err) {
		t.Fatal("forbidden read as unauthorized")
	}
}

func TestForbiddenMissingScope(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"forbidden","message":"needs hitl:decide","details":{"reason":"missing_scope","scope":"hitl:decide"}}}`)
	})
	defer cleanup()
	_, err := c.Interventions.Reject(context.Background(), "int_1", "no")
	if ForbiddenReason(err) != ForbiddenMissingScope || ForbiddenScope(err) != ScopeHitlDecide {
		t.Fatalf("err = %v", err)
	}
	if ForbiddenReason(nil) != "" {
		t.Fatal("nil error has a reason")
	}
}

func TestCreateCarriesHitlAccess(t *testing.T) {
	var body map[string]any
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"id":"vm-1","state":"running"}`)
	})
	defer cleanup()
	_, err := c.Environments.Create(context.Background(), CreateRequest{
		TaskID: "t",
		Hitl: &HitlSpec{
			Requests:     HitlRequests{Ask: &HitlAskRequests{}},
			RequireActor: true,
			Notify:       []HitlWebhook{{URL: "https://hooks.example/fuse", Secret: "HOOK_SECRET"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	hitl, _ := body["hitl"].(map[string]any)
	if hitl == nil || hitl["require_actor"] != true {
		t.Fatalf("hitl on the wire = %v", body["hitl"])
	}
	notify, _ := hitl["notify"].([]any)
	if len(notify) != 1 {
		t.Fatalf("notify = %v", hitl["notify"])
	}
	hook, _ := notify[0].(map[string]any)
	if hook["url"] != "https://hooks.example/fuse" || hook["secret"] != "HOOK_SECRET" {
		t.Fatalf("hook = %v", hook)
	}
}
