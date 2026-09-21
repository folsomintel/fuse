package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// Client is the orchestrator's end of the admin api.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

var _ orchestrator.IngressProxy = (*Client)(nil)

// NewClient returns a client with a timeout suited to small control calls.
func NewClient(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, owner string, body any) (orchestrator.IngressGrant, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return orchestrator.IngressGrant{}, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/v1/owners/"+url.PathEscape(owner), reader)
	if err != nil {
		return orchestrator.IngressGrant{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return orchestrator.IngressGrant{}, fmt.Errorf("fuse-proxy: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	switch {
	case res.StatusCode == http.StatusNotFound:
		return orchestrator.IngressGrant{}, orchestrator.ErrIngressNotFound
	case res.StatusCode == http.StatusNoContent:
		return orchestrator.IngressGrant{}, nil
	case res.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return orchestrator.IngressGrant{}, fmt.Errorf("fuse-proxy: http %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	var view ownerView
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		return orchestrator.IngressGrant{}, fmt.Errorf("fuse-proxy: decode response: %w", err)
	}
	grant := orchestrator.IngressGrant{ProxyAddr: view.ProxyAddr, ServerCertPEM: view.ServerCertPEM}
	for _, r := range view.Routes {
		grant.Routes = append(grant.Routes, orchestrator.IngressRoute{Name: r.Name, GuestPort: r.GuestPort, URL: r.URL})
	}
	return grant, nil
}

// Publish implements orchestrator.IngressProxy.
func (c *Client) Publish(ctx context.Context, req orchestrator.IngressRequest) (orchestrator.IngressGrant, error) {
	body := publishBody{Token: req.Token, AdoptFrom: req.AdoptFrom}
	for _, p := range req.Ports {
		body.Ports = append(body.Ports, PortSpec{Name: p.Name, GuestPort: p.GuestPort})
	}
	return c.do(ctx, http.MethodPut, req.Owner, body)
}

// Lookup implements orchestrator.IngressProxy.
func (c *Client) Lookup(ctx context.Context, owner string) (orchestrator.IngressGrant, error) {
	return c.do(ctx, http.MethodGet, owner, nil)
}

// Unpublish implements orchestrator.IngressProxy.
func (c *Client) Unpublish(ctx context.Context, owner string) error {
	_, err := c.do(ctx, http.MethodDelete, owner, nil)
	return err
}
