package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBaseURL is the public Daytona API endpoint.
const DefaultBaseURL = "https://app.daytona.io"

// ErrNotFound is returned when the API responds 404 for a resource lookup.
// Callers typically map this to orchestrator.ErrVMNotFound.
var ErrNotFound = errors.New("daytona: resource not found")

// APIError is returned for any non-2xx response that isn't a 404.
type APIError struct {
	Status   int
	Endpoint string
	Body     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("daytona: %s returned %d: %s", e.Endpoint, e.Status, e.Body)
}

// Client is a thin REST client for Daytona's sandbox + toolbox APIs.
//
// All methods accept a context.Context. Authentication uses the supplied
// API key as a bearer token on every request.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// NewClient constructs a Client. baseURL defaults to DefaultBaseURL when
// empty; trailing slashes are trimmed. hc defaults to a 30-second client
// when nil.
func NewClient(baseURL, apiKey string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, hc: hc}
}

// --- API types ----------------------------------------------------------

// Sandbox mirrors the fields we currently care about from
// /api/sandbox responses. Other fields are ignored on decode.
type Sandbox struct {
	ID           string            `json:"id"`
	State        string            `json:"state"`
	DesiredState string            `json:"desiredState,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// CreateSandboxRequest is the body for POST /api/sandbox. Resource fields
// (CPU/Memory/Disk) are only valid when Snapshot is empty — see the probe
// notes in PROBE_RESULTS.md.
type CreateSandboxRequest struct {
	Labels           map[string]string `json:"labels,omitempty"`
	AutoStopInterval *int              `json:"autoStopInterval,omitempty"`
	CPU              int               `json:"cpu,omitempty"`
	Memory           int               `json:"memory,omitempty"`
	Disk             int               `json:"disk,omitempty"`
	Snapshot         string            `json:"snapshot,omitempty"`
}

// PreviewURL is returned by GET /api/sandbox/{id}/ports/{port}/preview-url.
type PreviewURL struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	SandboxID string `json:"sandboxId,omitempty"`
}

// ExecResponse is returned by POST /api/toolbox/{id}/toolbox/process/execute
// (synchronous one-shot exec).
type ExecResponse struct {
	ExitCode int    `json:"exitCode"`
	Result   string `json:"result"`
}

// SessionExecResponse is returned by POST .../session/{sid}/exec.
type SessionExecResponse struct {
	CmdID string `json:"cmdId"`
}

// --- Sandbox lifecycle -------------------------------------------------

// CreateSandbox provisions a new sandbox.
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	var out Sandbox
	if err := c.doJSON(ctx, http.MethodPost, "/api/sandbox", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSandbox fetches a single sandbox by id. Returns ErrNotFound on 404.
func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var out Sandbox
	if err := c.doJSON(ctx, http.MethodGet, "/api/sandbox/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSandbox tears down a sandbox by id. Returns ErrNotFound on 404.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	// The probe used ?force=true; preserve that since plain DELETE was
	// observed to leave the sandbox in a stuck state in some cases.
	return c.doJSON(ctx, http.MethodDelete, "/api/sandbox/"+url.PathEscape(id)+"?force=true", nil, nil)
}

// ListSandboxes returns all sandboxes visible to the API key.
//
// The Daytona API has been observed to return either a bare JSON array
// or an envelope object; we try the array first and fall back.
func (c *Client) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/sandbox", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("daytona list: read body: %w", err)
	}

	var direct []Sandbox
	if err := json.Unmarshal(body, &direct); err == nil {
		return direct, nil
	}
	var wrapped struct {
		Sandboxes []Sandbox `json:"sandboxes"`
		Items     []Sandbox `json:"items"`
		Data      []Sandbox `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		switch {
		case wrapped.Sandboxes != nil:
			return wrapped.Sandboxes, nil
		case wrapped.Items != nil:
			return wrapped.Items, nil
		case wrapped.Data != nil:
			return wrapped.Data, nil
		}
	}
	return nil, fmt.Errorf("daytona list: unrecognized response shape: %s", truncateBody(body))
}

// GetPreviewURL fetches the public preview URL + token for a sandbox port.
// The token must be sent as the X-Daytona-Preview-Token header on
// subsequent requests to URL.
func (c *Client) GetPreviewURL(ctx context.Context, id string, port int) (*PreviewURL, error) {
	path := fmt.Sprintf("/api/sandbox/%s/ports/%d/preview-url", url.PathEscape(id), port)
	var out PreviewURL
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Toolbox: exec + files ---------------------------------------------

// Execute runs a shell command synchronously inside the sandbox.
func (c *Client) Execute(ctx context.Context, id, command string) (*ExecResponse, error) {
	body := map[string]string{"command": command}
	var out ExecResponse
	path := fmt.Sprintf("/api/toolbox/%s/toolbox/process/execute", url.PathEscape(id))
	if err := c.doJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Upload writes content to the given absolute path inside the sandbox.
//
// NOTE: Daytona's documented bulk-upload endpoint
// (POST /toolbox/files/bulk-upload) returned 200 in the probe but did NOT
// actually persist files. Use single-file upload at
// POST /toolbox/files/upload?path=<abs-path> with multipart form field
// "file". See PROBE_RESULTS.md.
func (c *Client) Upload(ctx context.Context, id, path string, content []byte) error {
	if path == "" {
		return errors.New("daytona upload: empty path")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return fmt.Errorf("daytona upload: form file: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("daytona upload: write part: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("daytona upload: close multipart: %w", err)
	}

	q := url.Values{}
	q.Set("path", path)
	endpoint := fmt.Sprintf("/api/toolbox/%s/toolbox/files/upload?%s", url.PathEscape(id), q.Encode())

	resp, err := c.do(ctx, http.MethodPost, endpoint, &buf, http.Header{
		"Content-Type": []string{mw.FormDataContentType()},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// --- Toolbox: long-running session API ---------------------------------

// CreateSession creates a long-running shell session inside the sandbox.
// sessionID is caller-supplied (Daytona uses it as the resource id).
func (c *Client) CreateSession(ctx context.Context, id, sessionID string) error {
	body := map[string]string{"sessionId": sessionID}
	path := fmt.Sprintf("/api/toolbox/%s/toolbox/process/session", url.PathEscape(id))
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// SessionExec runs a command inside a session. When runAsync is true the
// call returns immediately with the cmdId; logs/exit can be retrieved via
// SessionLogs / GetSession.
func (c *Client) SessionExec(ctx context.Context, id, sessionID, command string, runAsync bool) (*SessionExecResponse, error) {
	body := map[string]any{"command": command, "runAsync": runAsync}
	path := fmt.Sprintf("/api/toolbox/%s/toolbox/process/session/%s/exec",
		url.PathEscape(id), url.PathEscape(sessionID))
	var out SessionExecResponse
	if err := c.doJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SessionLogs returns a streaming reader of stdout+stderr for a command.
// Caller MUST close the returned ReadCloser. Path is from PROBE_RESULTS.md
// (text/plain, single concatenated stream).
func (c *Client) SessionLogs(ctx context.Context, id, sessionID, cmdID string) (io.ReadCloser, error) {
	path := fmt.Sprintf("/api/toolbox/%s/toolbox/process/session/%s/command/%s/logs",
		url.PathEscape(id), url.PathEscape(sessionID), url.PathEscape(cmdID))
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// --- HTTP plumbing -----------------------------------------------------

// doJSON marshals body to JSON, sends the request, and decodes a JSON
// response into out (which may be nil for void endpoints).
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	hdr := http.Header{}
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("daytona %s %s: marshal body: %w", method, path, err)
		}
		reader = bytes.NewReader(buf)
		hdr.Set("Content-Type", "application/json")
	}
	resp, err := c.do(ctx, method, path, reader, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("daytona %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// do builds a request, attaches auth + extra headers, executes, and
// converts non-2xx responses into ErrNotFound (404) or *APIError (other).
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("daytona %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daytona %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	return nil, &APIError{
		Status:   resp.StatusCode,
		Endpoint: method + " " + path,
		Body:     truncateBody(bodyBytes),
	}
}

func truncateBody(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "...(truncated)"
	}
	return string(b)
}
