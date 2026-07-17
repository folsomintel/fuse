package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// VersionInfo is the orchestrator self-identification returned by
// GET /v1/version. A fuse orchestrator answers with Service
// "fuse-orchestrator"; anything else (a host agent, an unrelated
// service, a proxy) will not, which is how `fuse connect` tells them
// apart before it has a confirmed token.
type VersionInfo struct {
	Service string `json:"service"`
	Version string `json:"version"`

	// ServerHeader is the raw Server response header (e.g.
	// "fuse-orchestrator/0.4.0" or "fc-agent/0.1"). Populated from any
	// HTTP response, even a non-2xx one, so a caller can name the wrong
	// service it actually reached.
	ServerHeader string `json:"-"`

	// StatusCode is the HTTP status of the /v1/version response.
	StatusCode int `json:"-"`
}

// IsOrchestrator reports whether the probed endpoint identified itself as
// a fuse orchestrator.
func (v *VersionInfo) IsOrchestrator() bool {
	return v != nil && v.Service == "fuse-orchestrator"
}

// Version probes GET /v1/version (unauthenticated) and reports what the
// endpoint says it is. A transport-level error (connection refused, DNS
// failure, timeout) is returned as err; a reachable-but-wrong service
// returns a non-nil *VersionInfo with IsOrchestrator() == false so the
// caller can distinguish "nothing there" from "wrong thing there".
func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	if c == nil || c.t == nil {
		return nil, errors.New("client is not configured")
	}
	req, err := c.t.newRequest(ctx, http.MethodGet, "/v1/version", nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.t.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	info := &VersionInfo{
		ServerHeader: resp.Header.Get("Server"),
		StatusCode:   resp.StatusCode,
	}
	// Best-effort decode: a non-fuse service may not answer with our
	// shape (or with JSON at all), which is itself the signal that this
	// is not an orchestrator. Errors here are intentionally ignored.
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBodyBytes)).Decode(info)
	return info, nil
}
