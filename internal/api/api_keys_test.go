package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// stubKeyStore is an in-memory APIKeyStore for handler/middleware tests.
type stubKeyStore struct {
	// liveKey, when non-empty, is the one raw token Authenticate accepts.
	liveKey string
	keyID   string

	created   []string // labels passed to Create
	listOut   []orchestrator.APIKeyRecord
	revokeErr error
	revoked   []string // ids passed to Revoke
}

func (s *stubKeyStore) Authenticate(_ context.Context, rawToken string) (string, bool) {
	if s.liveKey != "" && rawToken == s.liveKey {
		return s.keyID, true
	}
	return "", false
}

func (s *stubKeyStore) Create(_ context.Context, label string, now time.Time) (orchestrator.APIKeyRecord, string, error) {
	s.created = append(s.created, label)
	return orchestrator.APIKeyRecord{ID: "ak_test", Label: label, CreatedAt: now}, "fuse_sk_rawsecret", nil
}

func (s *stubKeyStore) List(_ context.Context) ([]orchestrator.APIKeyRecord, error) {
	return s.listOut, nil
}

func (s *stubKeyStore) Revoke(_ context.Context, id string, _ time.Time) error {
	s.revoked = append(s.revoked, id)
	return s.revokeErr
}

// TestBearerAuth_APIKeyAccepted verifies a live API key authenticates when
// the token is not the master token.
func TestBearerAuth_APIKeyAccepted(t *testing.T) {
	keys := &stubKeyStore{liveKey: "fuse_sk_live", keyID: "ak_1"}
	var gotPrincipal Principal
	mw := BearerAuth("master", keys, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, _ = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer fuse_sk_live")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("live key: got %d, want 200", rec.Code)
	}
	if gotPrincipal.Master {
		t.Error("key request should not be Master")
	}
	if gotPrincipal.KeyID != "ak_1" {
		t.Errorf("principal KeyID: got %q, want ak_1", gotPrincipal.KeyID)
	}
}

// TestBearerAuth_MasterStillWins verifies the master token authenticates as
// Master even when a key store is present, and does not consult the store.
func TestBearerAuth_MasterStillWins(t *testing.T) {
	keys := &stubKeyStore{liveKey: "fuse_sk_live", keyID: "ak_1"}
	var gotPrincipal Principal
	mw := BearerAuth("master", keys, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, _ = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer master")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("master token: got %d, want 200", rec.Code)
	}
	if !gotPrincipal.Master {
		t.Error("master token should yield Master principal")
	}
	if gotPrincipal.KeyID != "" {
		t.Errorf("master principal should have no KeyID, got %q", gotPrincipal.KeyID)
	}
}

// TestBearerAuth_UnknownKeyRejected verifies a token matching neither the
// master nor any live key is rejected, and fires the audit hook.
func TestBearerAuth_UnknownKeyRejected(t *testing.T) {
	keys := &stubKeyStore{liveKey: "fuse_sk_live", keyID: "ak_1"}
	var failed bool
	mw := BearerAuth("master", keys, func(_, _, _, _ string) { failed = true })
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer fuse_sk_revoked")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: got %d, want 401", rec.Code)
	}
	if !failed {
		t.Error("OnAuthFailure was not called for unknown key")
	}
}

// TestBearerAuth_KeyOnlyNoMaster verifies key auth works with no master
// token configured (master path skipped, store still consulted).
func TestBearerAuth_KeyOnlyNoMaster(t *testing.T) {
	keys := &stubKeyStore{liveKey: "fuse_sk_live", keyID: "ak_1"}
	mw := BearerAuth("", keys, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer fuse_sk_live")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("key-only auth: got %d, want 200", rec.Code)
	}
}

// TestCreateAPIKey_MasterReturnsKeyOnce verifies the master operator can
// mint a key and the raw secret appears in the response.
func TestCreateAPIKey_MasterReturnsKeyOnce(t *testing.T) {
	keys := &stubKeyStore{}
	h := &Handler{AuthToken: "master", APIKeys: keys}
	router := mustRouter(t, h)

	body, _ := json.Marshal(CreateAPIKeyRequest{Label: "ci"})
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer master")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp CreateAPIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Key == "" {
		t.Error("create response must include the raw key")
	}
	if resp.Label != "ci" {
		t.Errorf("label: got %q, want ci", resp.Label)
	}
	if len(keys.created) != 1 || keys.created[0] != "ci" {
		t.Errorf("store.Create labels: got %v, want [ci]", keys.created)
	}
}

// TestCreateAPIKey_KeyHolderForbidden verifies a caller authenticated WITH
// an API key cannot mint more keys (master-only management).
func TestCreateAPIKey_KeyHolderForbidden(t *testing.T) {
	keys := &stubKeyStore{liveKey: "fuse_sk_live", keyID: "ak_1"}
	h := &Handler{AuthToken: "master", APIKeys: keys}
	router := mustRouter(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer fuse_sk_live") // key, not master
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("key-holder mint: got %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(keys.created) != 0 {
		t.Errorf("store.Create should not have been called, got %v", keys.created)
	}
}

// TestListAPIKeys_NeverLeaksSecret verifies list returns metadata and the
// JSON has no "key" field (the APIKey type carries no secret).
func TestListAPIKeys_NeverLeaksSecret(t *testing.T) {
	now := time.Now().UTC()
	keys := &stubKeyStore{listOut: []orchestrator.APIKeyRecord{
		{ID: "ak_1", Label: "ci", CreatedAt: now},
	}}
	h := &Handler{AuthToken: "master", APIKeys: keys}
	router := mustRouter(t, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/api-keys", nil)
	req.Header.Set("Authorization", "Bearer master")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: got %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"key"`) {
		t.Errorf("list response must not contain a raw key field: %s", rec.Body.String())
	}
	var list APIKeyList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.APIKeys) != 1 || list.APIKeys[0].ID != "ak_1" {
		t.Errorf("list contents: got %+v", list.APIKeys)
	}
}

// TestRevokeAPIKey_NotFound verifies an unknown id yields 404.
func TestRevokeAPIKey_NotFound(t *testing.T) {
	keys := &stubKeyStore{revokeErr: orchestrator.ErrAPIKeyNotFound}
	h := &Handler{AuthToken: "master", APIKeys: keys}
	router := mustRouter(t, h)

	req := httptest.NewRequest(http.MethodDelete, "/v1/api-keys/ak_missing", nil)
	req.Header.Set("Authorization", "Bearer master")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: got %d, want 404", rec.Code)
	}
}

// TestRevokeAPIKey_Success verifies a known id is revoked with 204.
func TestRevokeAPIKey_Success(t *testing.T) {
	keys := &stubKeyStore{}
	h := &Handler{AuthToken: "master", APIKeys: keys}
	router := mustRouter(t, h)

	req := httptest.NewRequest(http.MethodDelete, "/v1/api-keys/ak_1", nil)
	req.Header.Set("Authorization", "Bearer master")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", rec.Code)
	}
	if len(keys.revoked) != 1 || keys.revoked[0] != "ak_1" {
		t.Errorf("store.Revoke ids: got %v, want [ak_1]", keys.revoked)
	}
}

// TestAPIKeyRoutes_AbsentWithoutStore verifies the management routes are not
// registered when no key store is configured (404, not 401/403).
func TestAPIKeyRoutes_AbsentWithoutStore(t *testing.T) {
	h := &Handler{AuthToken: "master"} // no APIKeys
	router := mustRouter(t, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/api-keys", nil)
	req.Header.Set("Authorization", "Bearer master")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("api-keys without store: got %d, want 404", rec.Code)
	}
}
