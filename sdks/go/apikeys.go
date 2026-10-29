package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type APIKeysService struct {
	t *transport
}

func newAPIKeysService(t *transport) *APIKeysService {
	return &APIKeysService{t: t}
}

// hitl scopes a key can be limited to. A key with no scopes has full access.
const (
	ScopeHitlRead     = "hitl:read"
	ScopeHitlDecide   = "hitl:decide"
	ScopeHitlSecret   = "hitl:secret"
	ScopeHitlTakeover = "hitl:takeover"
	ScopeHitlActAs    = "hitl:act-as"
)

// createAPIKeyRequest is the body for APIKeysService.Create.
type createAPIKeyRequest struct {
	Label      string      `json:"label,omitempty"`
	Scopes     []string    `json:"scopes,omitempty"`
	EnvID      string      `json:"env_id,omitempty"`
	TTLSeconds int64       `json:"ttl_seconds,omitempty"`
	Actor      *ActorLabel `json:"actor,omitempty"`
}

// CreateAPIKeyOptions configures CreateWithOptions. Zero fields are omitted.
type CreateAPIKeyOptions struct {
	Label string
	// Scopes limits the key to the hitl routes (see the ScopeHitl
	// constants). Nil means a full-access key.
	Scopes []string
	// EnvID limits a scoped key to one environment and its forks.
	EnvID string
	// TTL is how long the key lives, sent as whole seconds. A scoped key
	// must set it.
	TTL time.Duration
	// Actor pins the key to one actor.
	Actor *ActorLabel
}

// Create issues a new API key. The raw secret is returned once in
// CreatedAPIKey.Key and is unrecoverable afterward. Requires the master
// token; the server enforces this.
func (s *APIKeysService) Create(ctx context.Context, label string) (*CreatedAPIKey, error) {
	return s.create(ctx, createAPIKeyRequest{Label: label})
}

// CreateWithOptions issues a new API key, optionally scoped to the hitl
// routes, one environment, a lifetime and a pinned actor. The master token
// may mint any key; a scoped key may mint only keys within its own scopes,
// environment and lifetime.
func (s *APIKeysService) CreateWithOptions(ctx context.Context, opt CreateAPIKeyOptions) (*CreatedAPIKey, error) {
	return s.create(ctx, createAPIKeyRequest{
		Label:      opt.Label,
		Scopes:     opt.Scopes,
		EnvID:      opt.EnvID,
		TTLSeconds: int64(opt.TTL / time.Second),
		Actor:      opt.Actor,
	})
}

func (s *APIKeysService) create(ctx context.Context, reqBody createAPIKeyRequest) (*CreatedAPIKey, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("api keys service is not configured")
	}
	req, err := s.t.newRequest(ctx, http.MethodPost, "/v1/api-keys", nil, reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var key CreatedAPIKey
	if err := json.NewDecoder(resp.Body).Decode(&key); err != nil {
		return nil, fmt.Errorf("decode api key: %w", err)
	}
	return &key, nil
}

// List returns the metadata for all API keys. Requires the master token;
// the server enforces this.
func (s *APIKeysService) List(ctx context.Context) ([]APIKey, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("api keys service is not configured")
	}
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/api-keys", nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out apiKeyList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode api keys: %w", err)
	}
	return out.APIKeys, nil
}

// Revoke deletes the API key with the given id. Requires the master
// token; the server enforces this.
func (s *APIKeysService) Revoke(ctx context.Context, id string) error {
	if s == nil || s.t == nil {
		return errors.New("api keys service is not configured")
	}
	if id == "" {
		return errors.New("id is required")
	}
	path := "/v1/api-keys/" + url.PathEscape(id)
	req, err := s.t.newRequest(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return err
	}
	if err := CheckResponse(resp); err != nil {
		return err
	}
	return nil
}
