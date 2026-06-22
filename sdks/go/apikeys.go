package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

type APIKeysService struct {
	t *transport
}

func newAPIKeysService(t *transport) *APIKeysService {
	return &APIKeysService{t: t}
}

// createAPIKeyRequest is the body for APIKeysService.Create.
type createAPIKeyRequest struct {
	Label string `json:"label,omitempty"`
}

// Create issues a new API key. The raw secret is returned once in
// CreatedAPIKey.Key and is unrecoverable afterward. Requires the master
// token; the server enforces this.
func (s *APIKeysService) Create(ctx context.Context, label string) (*CreatedAPIKey, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("api keys service is not configured")
	}
	reqBody := createAPIKeyRequest{Label: label}
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
