package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

// Client calls restate ingress, and in mode cloud the admin api.
type Client struct {
	url      string
	adminURL string
	apiKey   string
	http     *http.Client
	ingress  *ingress.Client
}

// NewClient returns a client for cfg's ingress. a non-empty cfg.APIKey is sent
// as a bearer token on every request.
func NewClient(cfg Config) *Client {
	hc := &http.Client{Timeout: 30 * time.Second}
	opts := []ingress.ClientOption{ingress.WithHttpClient(hc)}
	if cfg.APIKey != "" {
		opts = append(opts, ingress.WithAuthKey(cfg.APIKey))
	}
	return &Client{
		url:      cfg.IngressURL,
		adminURL: cfg.AdminURL,
		apiKey:   cfg.APIKey,
		http:     hc,
		ingress:  ingress.NewClient(cfg.IngressURL, opts...),
	}
}

// authorize adds the api key, when there is one.
func (c *Client) authorize(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

// Check reports whether ingress answers its health endpoint. /ready calls it.
func (c *Client) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/restate/health", nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("restate ingress unreachable: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("restate ingress health: http %d", res.StatusCode)
	}
	return nil
}

// Ping invokes <object>/<key>/Ping through ingress and waits for the result.
func (c *Client) Ping(ctx context.Context, object, key string) (PingResult, error) {
	return ingress.Object[restate.Void, PingResult](c.ingress, object, key, "Ping").
		Request(ctx, restate.Void{})
}

// Register registers the handler endpoint at uri with the admin api. force
// replaces a deployment already registered at uri, which is what a restart
// or upgrade on the same address needs; invocations pinned to the old one
// then replay on the new code.
func (c *Client) Register(ctx context.Context, uri string) error {
	body, err := json.Marshal(map[string]any{"uri": uri, "force": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.adminURL+"/deployments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("restate admin unreachable: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("restate admin refused %s: http %d: %s", uri, res.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
