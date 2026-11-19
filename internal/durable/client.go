package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/telemetry"
)

// Client calls restate ingress, and in mode cloud the admin api.
type Client struct {
	url      string
	adminURL string
	apiKey   string
	http     *http.Client
	ingress  *ingress.Client
	// longIngress has no request timeout, for calls that wait on an
	// invocation as long as its work takes (an artifact move). the caller's
	// ctx bounds them.
	longIngress *ingress.Client
}

// NewClient returns a client for cfg's ingress. a non-empty cfg.APIKey is sent
// as a bearer token on every request.
func NewClient(cfg Config) *Client {
	// the transport carries the request id and trace context, so restate's
	// invocation joins the api request's trace and the handler sees the id.
	hc := &http.Client{Timeout: 30 * time.Second, Transport: unavailable{&telemetry.Transport{Peer: "restate"}}}
	long := &http.Client{Transport: unavailable{&telemetry.Transport{Peer: "restate"}}}
	return &Client{
		url:         cfg.IngressURL,
		adminURL:    cfg.AdminURL,
		apiKey:      cfg.APIKey,
		http:        hc,
		ingress:     ingress.NewClient(cfg.IngressURL, ingressOptions(cfg, hc)...),
		longIngress: ingress.NewClient(cfg.IngressURL, ingressOptions(cfg, long)...),
	}
}

// unavailable marks a request that never reached restate with
// orchestrator.ErrRestateUnavailable, so the api answers 503 and the caller
// knows nothing started: one that failed before it was written (refused, no
// route, a dial that timed out, a proxy that would not connect). a failure
// after the request was written is left as it is: restate may have accepted
// the invocation and still be running it. ingress keeps the error chain.
type unavailable struct{ next http.RoundTripper }

func (u unavailable) RoundTrip(req *http.Request) (*http.Response, error) {
	var written atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			written.Store(true)
		}
	}}
	res, err := u.next.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil && !written.Load() {
		return nil, fmt.Errorf("%w: %w", orchestrator.ErrRestateUnavailable, err)
	}
	if a, ok := req.Context().Value(answerKey{}).(*answer); ok && res != nil && res.Header.Get(restateIDHeader) != "" {
		a.invocation.Store(true)
	}
	return res, err
}

// restateIDHeader names the invocation on every answer restate gives for
// one, a recorded failure or a replay of it included. an error the ingress
// makes itself (an unknown service, overload) carries none.
const restateIDHeader = "X-Restate-Id"

type answerKey struct{}

// answer notes whether the response to a call came from its invocation.
type answer struct{ invocation atomic.Bool }

// watchAnswer returns ctx whose ingress call notes on the answer whether
// restate answered it from the invocation.
func watchAnswer(ctx context.Context) (context.Context, *answer) {
	a := &answer{}
	return context.WithValue(ctx, answerKey{}, a), a
}

// settled marks out, the fleet error a call's failure err was turned into,
// as orchestrator.SettledError when restate recorded err for the invocation:
// the handler ran and failed for good, now or on an earlier call under the
// same key. a failure before restate answered (unreachable, a client
// timeout, a dropped connection, ctx) or one its ingress made itself stays
// as it is, so the api keeps the call's key and a retry attaches to the
// invocation. a restate-server that discloses the error's source says so
// itself; an older one is known by the invocation id on its answer.
func settled(a *answer, err, out error) error {
	var ierr ingress.Error
	if out == nil || !errors.As(err, &ierr) {
		return out
	}
	if !ierr.IsInvocationError() && !a.invocation.Load() {
		return out
	}
	return orchestrator.Settled(out)
}

func ingressOptions(cfg Config, hc *http.Client) []ingress.ClientOption {
	opts := []ingress.ClientOption{ingress.WithHttpClient(hc)}
	if cfg.APIKey != "" {
		opts = append(opts, ingress.WithAuthKey(cfg.APIKey))
	}
	return opts
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

// WaitReady polls until ingress answers and a Ping reaches the Environment
// object through it, which only works once this orchestrator's deployment is
// registered. it gives up after timeout and says which half failed: restate
// unreachable, or reachable with no deployment serving Environment.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		attempt, stop := context.WithTimeout(ctx, 5*time.Second)
		if err := c.Check(attempt); err != nil {
			last = err
		} else if _, err := c.Ping(attempt, EnvironmentObject, "startup"); err != nil {
			last = fmt.Errorf("restate is up but %s does not answer: is this orchestrator's handler endpoint registered and reachable from restate? (see docs/runbooks/restate.mdx): %w", EnvironmentObject, err)
		} else {
			stop()
			return nil
		}
		stop()
		select {
		case <-ctx.Done():
			return fmt.Errorf("restate not ready after %s: %w", timeout, last)
		case <-time.After(time.Second):
		}
	}
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

var _ orchestrator.Durable = (*Client)(nil)
