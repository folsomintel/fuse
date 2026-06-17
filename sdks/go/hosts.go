package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

type HostsService struct {
	t *transport
}

func newHostsService(t *transport) *HostsService {
	return &HostsService{t: t}
}

func (s *HostsService) Register(ctx context.Context, req RegisterHostRequest) (*Host, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("hosts service is not configured")
	}
	httpReq, err := s.t.newRequest(ctx, http.MethodPost, "/v1/hosts", nil, req)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(httpReq)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var host Host
	if err := json.NewDecoder(resp.Body).Decode(&host); err != nil {
		return nil, fmt.Errorf("decode host: %w", err)
	}
	return &host, nil
}

func (s *HostsService) List(ctx context.Context) ([]Host, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("hosts service is not configured")
	}
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/hosts", nil, nil)
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
	defer resp.Body.Close()
	var out hostList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode hosts: %w", err)
	}
	return out.Hosts, nil
}

func (s *HostsService) Get(ctx context.Context, hostID string) (*Host, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("hosts service is not configured")
	}
	if hostID == "" {
		return nil, errors.New("host id is required")
	}
	path := "/v1/hosts/" + url.PathEscape(hostID)
	req, err := s.t.newRequest(ctx, http.MethodGet, path, nil, nil)
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
	defer resp.Body.Close()
	var host Host
	if err := json.NewDecoder(resp.Body).Decode(&host); err != nil {
		return nil, fmt.Errorf("decode host: %w", err)
	}
	return &host, nil
}

func (s *HostsService) Cordon(ctx context.Context, hostID string) error {
	return s.action(ctx, hostID, "cordon")
}

func (s *HostsService) Uncordon(ctx context.Context, hostID string) error {
	return s.action(ctx, hostID, "uncordon")
}

func (s *HostsService) Deregister(ctx context.Context, hostID string) error {
	if s == nil || s.t == nil {
		return errors.New("hosts service is not configured")
	}
	if hostID == "" {
		return errors.New("host id is required")
	}
	path := "/v1/hosts/" + url.PathEscape(hostID)
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

func (s *HostsService) action(ctx context.Context, hostID, action string) error {
	if s == nil || s.t == nil {
		return errors.New("hosts service is not configured")
	}
	if hostID == "" {
		return errors.New("host id is required")
	}
	if action == "" {
		return errors.New("action is required")
	}
	path := "/v1/hosts/" + url.PathEscape(hostID)
	values := url.Values{}
	values.Set("action", action)
	req, err := s.t.newRequest(ctx, http.MethodPost, path, values, nil)
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
