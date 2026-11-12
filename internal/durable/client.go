package durable

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

// Client calls restate ingress.
type Client struct {
	url     string
	http    *http.Client
	ingress *ingress.Client
}

// NewClient returns a client for the ingress at url.
func NewClient(url string) *Client {
	hc := &http.Client{Timeout: 30 * time.Second}
	return &Client{
		url:     url,
		http:    hc,
		ingress: ingress.NewClient(url, ingress.WithHttpClient(hc)),
	}
}

// Check reports whether ingress answers its health endpoint. /ready calls it.
func (c *Client) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/restate/health", nil)
	if err != nil {
		return err
	}
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
