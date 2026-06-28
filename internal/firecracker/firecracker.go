// Package firecracker implements a Firecracker-based Provider. It supports a
// remote HTTP host-agent client and a stub in-memory fallback for dev/tests so
// Fleet/Boot can remain provider-agnostic.
package firecracker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/core"
)

// httpStatusError carries the HTTP status code (and trimmed body) from a
// non-2xx host-agent response so callers can branch on the code (e.g. 404 to
// trigger the /start-agent → /start-surfd fallback). Its Error() string is
// byte-identical to the previous fmt.Errorf("http %d: %s", ...) so anything
// matching on the message text keeps working.
type httpStatusError struct {
	Code int
	Body string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

// fused guest paths for the FROZEN host-agent /start-surfd wire. The external
// firecracker host agent cannot be changed, so its request still carries the
// structured manifest/secrets/TLS guest paths. These constants exist solely to
// populate that frozen wire and mirror the core fused profile's path constants
// (see orchestrator/agent_profile.go). The firecracker package cannot import
// the orchestrator's unexported consts, hence the duplication by design.
const (
	fusedManifestGuestPath = "/fuse/manifest.json"
	fusedSecretsGuestPath  = "/fuse/secrets.json"
	fusedTLSCertGuestPath  = "/fuse/tls/cert.pem"
	fusedTLSKeyGuestPath   = "/fuse/tls/key.pem"
)

// Config configures the Firecracker provider client.
type Config struct {
	BaseURL     string       // host agent base URL, e.g. https://agent.local
	Token       string       // bearer token for auth
	HTTPClient  *http.Client // optional; defaults to http.DefaultClient
	UseStub     bool         // force in-memory stub (for dev/tests)
	DownloadURL string       // optional URL to fetch the guest agent binary (forwarded to /start-agent)
}

// Provider implements orchestrator.Provider backed by a Firecracker host agent
// API. If UseStub is true or BaseURL is empty, it falls back to an in-memory
// stub that simulates behavior without real microVMs.
type Provider struct {
	baseURL     string
	token       string
	client      *http.Client
	downloadURL string

	stub *stubProvider
}

// New creates a Firecracker provider.
//
// TODO: Default to a custom http.Client with a 30s timeout instead of
// http.DefaultClient (which has no timeout). Long-running calls like
// Create should use per-request context deadlines, but a client-level
// timeout prevents leaked connections on unresponsive hosts.
func New(cfg Config) *Provider {
	p := &Provider{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		token:       cfg.Token,
		client:      cfg.HTTPClient,
		downloadURL: cfg.DownloadURL,
	}
	if p.client == nil {
		p.client = http.DefaultClient
	}
	if cfg.UseStub || p.baseURL == "" {
		p.stub = newStub()
	}
	return p
}

var _ orchestrator.Provider = (*Provider)(nil)

// Create provisions a new sandbox.
func (p *Provider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.Create(ctx, spec)
	}

	reqBody := createVMRequest{
		Name:      spec.Name,
		CPUs:      spec.CPUs,
		MemoryMB:  spec.RamMB,
		StorageGB: spec.StorageGB,
		Region:    spec.Region,
	}
	var resp createVMResponse
	if err := p.doJSON(ctx, http.MethodPost, "/v1/vm", reqBody, &resp); err != nil {
		return nil, fmt.Errorf("firecracker create vm: %w", err)
	}

	env := &remoteEnv{
		id:     resp.VMID,
		url:    resp.URL,
		client: p,
	}
	if env.url == "" {
		env.url = fmt.Sprintf("fc://%s", resp.VMID)
	}
	return env, nil
}

// Get returns an existing sandbox.
func (p *Provider) Get(ctx context.Context, name string) (orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.Get(ctx, name)
	}
	path := fmt.Sprintf("/v1/vm/%s", name)
	var resp getVMResponse
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("firecracker get vm: %w", err)
	}
	return &remoteEnv{id: resp.VMID, url: resp.URL, client: p}, nil
}

// Destroy tears down a sandbox.
func (p *Provider) Destroy(ctx context.Context, name string) error {
	if p.stub != nil {
		return p.stub.Destroy(ctx, name)
	}
	path := fmt.Sprintf("/v1/vm/%s", name)
	return p.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// List returns all sandboxes matching the given prefix.
func (p *Provider) List(ctx context.Context, prefix string) ([]orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.List(ctx, prefix)
	}
	path := "/v1/vm"
	if prefix != "" {
		path += "?prefix=" + prefix
	}
	var resp listVMResponse
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("firecracker list vms: %w", err)
	}
	envs := make([]orchestrator.Environment, 0, len(resp.VMs))
	for _, vm := range resp.VMs {
		envs = append(envs, &remoteEnv{id: vm.VMID, url: vm.URL, client: p})
	}
	return envs, nil
}

// Close releases resources.
func (p *Provider) Close() error {
	if p.stub != nil {
		return p.stub.Close()
	}
	return nil
}

// remoteEnv represents a Firecracker VM managed by the host agent.
type remoteEnv struct {
	id     string
	url    string
	client *Provider

	// authToken is the per-VM bearer token callers must present when
	// reaching url. It is populated by Boot via SetToken once
	// VMCredentials have been generated (and refreshed by token
	// rotation). Empty until then.
	tokenMu   sync.RWMutex
	authToken string
}

var (
	_ orchestrator.Environment = (*remoteEnv)(nil)
	_ orchestrator.TokenSetter = (*remoteEnv)(nil)
)

func (e *remoteEnv) Name() string { return e.id }
func (e *remoteEnv) URL() string  { return e.url }

// Token returns the per-VM auth token (VMCredentials.AuthToken)
// previously installed via SetToken. Empty until Boot/rotation sets it.
func (e *remoteEnv) Token() string {
	e.tokenMu.RLock()
	defer e.tokenMu.RUnlock()
	return e.authToken
}

// SetToken records the per-VM auth token so subsequent Token() calls
// return it. Called by orchestrator.Boot and RotateToken via the
// orchestrator.TokenSetter interface.
func (e *remoteEnv) SetToken(token string) {
	e.tokenMu.Lock()
	e.authToken = token
	e.tokenMu.Unlock()
}

func (e *remoteEnv) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	req := execRequest{Cmd: append([]string{name}, args...)}
	var resp execResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/exec", e.id), req, &resp); err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	out := append(resp.Stdout, resp.Stderr...)
	if resp.ExitCode != 0 {
		return out, fmt.Errorf("exec exit %d", resp.ExitCode)
	}
	return out, nil
}

func (e *remoteEnv) ExecStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	req := execRequest{Cmd: append([]string{name}, args...)}
	var resp execResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/exec", e.id), req, &resp); err != nil {
		return fmt.Errorf("exec stream: %w", err)
	}
	if len(resp.Stdout) > 0 {
		_, _ = stdout.Write(resp.Stdout)
	}
	if len(resp.Stderr) > 0 {
		_, _ = stderr.Write(resp.Stderr)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("exec exit %d", resp.ExitCode)
	}
	return nil
}

func (e *remoteEnv) Upload(ctx context.Context, data []byte, path string) error {
	req := uploadRequest{Path: path, ContentB64: base64.StdEncoding.EncodeToString(data)}
	return e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/upload", e.id), req, nil)
}

// StartAgent launches the guest agent. It FIRST tries the generalized
// /start-agent endpoint (which additionally supports DownloadURL so OSS users
// can fetch the agent binary and skip a rootfs re-bake). Older host agents
// (e.g. the live host) only expose /start-surfd and return 404 for unknown
// actions; in that case we transparently FALL BACK to the FROZEN /start-surfd
// wire. Any non-404 error propagates.
//
// The structured fused-profile guest paths come from the local frozen-wire
// constants (the host agent owns the launch mechanism, not spec.Command), and
// only the generic spec fields (auth token, gateway) pass through.
func (e *remoteEnv) StartAgent(ctx context.Context, spec orchestrator.AgentSpec) error {
	downloadURL := spec.DownloadURL
	if downloadURL == "" {
		downloadURL = e.client.downloadURL
	}

	agentReq := startAgentRequest{
		ManifestPath: fusedManifestGuestPath,
		SecretsPath:  fusedSecretsGuestPath,
		TLSCertPath:  fusedTLSCertGuestPath,
		TLSKeyPath:   fusedTLSKeyGuestPath,
		AuthToken:    spec.AuthToken,
		Gateway:      spec.Gateway,
		GatewayToken: spec.GatewayToken,
		DownloadURL:  downloadURL,
	}
	err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/start-agent", e.id), agentReq, nil)
	if err == nil {
		return nil
	}
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusNotFound {
		return err
	}

	// Fall back to the FROZEN /start-surfd wire (unchanged payload).
	req := startSurfdRequest{
		ManifestPath: fusedManifestGuestPath,
		SecretsPath:  fusedSecretsGuestPath,
		TLSCertPath:  fusedTLSCertGuestPath,
		TLSKeyPath:   fusedTLSKeyGuestPath,
		AuthToken:    spec.AuthToken,
		Gateway:      spec.Gateway,
		GatewayToken: spec.GatewayToken,
	}
	return e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/start-surfd", e.id), req, nil)
}

func (e *remoteEnv) Checkpoint(ctx context.Context, comment string) (string, error) {
	req := snapshotRequest{Comment: comment, IncludeRAM: false}
	var resp snapshotResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/snapshot", e.id), req, &resp); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	return resp.SnapshotID, nil
}

func (e *remoteEnv) Restore(ctx context.Context, checkpointID string) error {
	req := restoreRequest{SnapshotID: checkpointID, IncludeRAM: false}
	return e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/restore", e.id), req, nil)
}

func (e *remoteEnv) DeleteCheckpoint(ctx context.Context, checkpointID string) error {
	return e.client.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/v1/vm/%s/snapshots/%s", e.id, checkpointID), nil, nil)
}

func (e *remoteEnv) ListCheckpoints(ctx context.Context) ([]orchestrator.Checkpoint, error) {
	var resp listSnapshotsResponse
	if err := e.client.doJSON(ctx, http.MethodGet, fmt.Sprintf("/v1/vm/%s/snapshots", e.id), nil, &resp); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	out := make([]orchestrator.Checkpoint, 0, len(resp.Snapshots))
	for _, s := range resp.Snapshots {
		out = append(out, orchestrator.Checkpoint{
			ID:        s.SnapshotID,
			Comment:   s.Comment,
			SizeBytes: s.SizeBytes,
			CreatedAt: s.CreatedAt,
		})
	}
	return out, nil
}

// HTTP helpers and request/response shapes.

type createVMRequest struct {
	Name      string `json:"name"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memory_mb"`
	StorageGB int    `json:"storage_gb"`
	Region    string `json:"region"`
}

type createVMResponse struct {
	VMID string `json:"vm_id"`
	URL  string `json:"url"`
}

type getVMResponse struct {
	VMID string `json:"vm_id"`
	URL  string `json:"url"`
}

type listVMResponse struct {
	VMs []struct {
		VMID string `json:"vm_id"`
		URL  string `json:"url"`
	} `json:"vms"`
}

type uploadRequest struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

type execRequest struct {
	Cmd []string `json:"cmd"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
}

type startSurfdRequest struct {
	ManifestPath string `json:"manifest_path"`
	SecretsPath  string `json:"secrets_path"`
	TLSCertPath  string `json:"tls_cert_path,omitempty"`
	TLSKeyPath   string `json:"tls_key_path,omitempty"`
	AuthToken    string `json:"auth_token,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	GatewayToken string `json:"gateway_token,omitempty"`
}

// startAgentRequest is the generalized launch wire. It carries the same fields
// as startSurfdRequest plus optional download_url/binary_path/listen so OSS
// users can fetch the agent binary and/or run a custom in-guest daemon.
type startAgentRequest struct {
	ManifestPath string `json:"manifest_path"`
	SecretsPath  string `json:"secrets_path"`
	TLSCertPath  string `json:"tls_cert_path,omitempty"`
	TLSKeyPath   string `json:"tls_key_path,omitempty"`
	AuthToken    string `json:"auth_token,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	GatewayToken string `json:"gateway_token,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
	BinaryPath   string `json:"binary_path,omitempty"`
	Listen       string `json:"listen,omitempty"`
}

type snapshotRequest struct {
	Comment    string `json:"comment,omitempty"`
	IncludeRAM bool   `json:"include_ram"`
}

type snapshotResponse struct {
	SnapshotID string `json:"snapshot_id"`
}

type restoreRequest struct {
	SnapshotID string `json:"snapshot_id"`
	IncludeRAM bool   `json:"include_ram"`
}

type listSnapshotsResponse struct {
	Snapshots []struct {
		SnapshotID string    `json:"snapshot_id"`
		Comment    string    `json:"comment,omitempty"`
		SizeBytes  int64     `json:"size_bytes,omitempty"`
		CreatedAt  time.Time `json:"created_at,omitempty"`
	} `json:"snapshots"`
}

// TODO: Add retry logic with exponential backoff for transient failures
// (HTTP 500/502/503, connection reset). Idempotent operations (GET, DELETE)
// are safe to retry immediately; POST /v1/vm should only retry if the
// response was never received (connection error, not a 500 with a body).
func (p *Provider) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		return &httpStatusError{Code: res.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if respBody == nil {
		return nil
	}
	dec := json.NewDecoder(res.Body)
	return dec.Decode(respBody)
}

// stubProvider is the in-memory fallback used when BaseURL is empty or UseStub is set.
type stubProvider struct {
	mu   sync.Mutex
	envs map[string]*stubEnv
}

func newStub() *stubProvider {
	return &stubProvider{envs: make(map[string]*stubEnv)}
}

func (p *stubProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.envs[spec.Name]; exists {
		return nil, fmt.Errorf("env %s already exists", spec.Name)
	}
	env := &stubEnv{name: spec.Name, url: fmt.Sprintf("fc://%s", spec.Name)}
	p.envs[spec.Name] = env
	return env, nil
}

func (p *stubProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("env %s not found", name)
	}
	return env, nil
}

func (p *stubProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *stubProvider) List(_ context.Context, prefix string) ([]orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	envs := make([]orchestrator.Environment, 0, len(p.envs))
	for name, env := range p.envs {
		if prefix == "" || (len(name) >= len(prefix) && strings.HasPrefix(name, prefix)) {
			envs = append(envs, env)
		}
	}
	return envs, nil
}

func (p *stubProvider) Close() error { return nil }

// stubEnv simulates a VM in memory.
type stubEnv struct {
	name string
	url  string

	mu          sync.Mutex
	files       map[string][]byte
	checkpoints []orchestrator.Checkpoint
	authToken   string
}

var (
	_ orchestrator.Environment = (*stubEnv)(nil)
	_ orchestrator.TokenSetter = (*stubEnv)(nil)
)

func (e *stubEnv) Name() string { return e.name }
func (e *stubEnv) URL() string  { return e.url }

// Token returns the per-VM auth token previously installed via SetToken.
func (e *stubEnv) Token() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.authToken
}

// SetToken records the per-VM auth token so subsequent Token() calls
// return it.
func (e *stubEnv) SetToken(token string) {
	e.mu.Lock()
	e.authToken = token
	e.mu.Unlock()
}

// Exec simulates a successful guest command. The in-memory stub has no real
// guest, so it returns empty output and no error — enough for the dev/stub
// server to drive the full VM lifecycle (including drain) end to end.
func (e *stubEnv) Exec(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, nil
}

// ExecStream simulates a successful guest command (no output written). Like
// Exec, the stub succeeds so the dev server can exercise drain and other
// guest-exec paths without a real microVM.
func (e *stubEnv) ExecStream(_ context.Context, _, _ io.Writer, _ string, _ ...string) error {
	return nil
}

func (e *stubEnv) Upload(_ context.Context, data []byte, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.files == nil {
		e.files = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	e.files[path] = cp
	return nil
}

func (e *stubEnv) StartAgent(_ context.Context, _ orchestrator.AgentSpec) error { return nil }

func (e *stubEnv) Checkpoint(_ context.Context, comment string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := fmt.Sprintf("cp-%d", len(e.checkpoints)+1)
	var sizeBytes int64
	for _, file := range e.files {
		sizeBytes += int64(len(file))
	}
	e.checkpoints = append(e.checkpoints, orchestrator.Checkpoint{
		ID:        id,
		Comment:   comment,
		SizeBytes: sizeBytes,
		CreatedAt: time.Now(),
	})
	return id, nil
}

func (e *stubEnv) Restore(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, cp := range e.checkpoints {
		if cp.ID == checkpointID {
			return nil
		}
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}

func (e *stubEnv) DeleteCheckpoint(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, cp := range e.checkpoints {
		if cp.ID != checkpointID {
			continue
		}
		e.checkpoints = append(e.checkpoints[:i], e.checkpoints[i+1:]...)
		return nil
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}

func (e *stubEnv) ListCheckpoints(_ context.Context) ([]orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cps := make([]orchestrator.Checkpoint, len(e.checkpoints))
	copy(cps, e.checkpoints)
	return cps, nil
}
