// Package daytona implements a Daytona-backed orchestrator.Provider.
//
// The provider drives Daytona's sandbox + toolbox HTTP API (see client.go)
// to create, inspect, and destroy sandboxes, and to upload files / launch
// processes inside them. Sandbox identity is tracked via the "surf-name"
// label on each Daytona sandbox so that orchestrator's Spec.Name (e.g.
// "surf-{task-id}") survives our local restart.
//
// Guest agent transport on Daytona: Daytona's preview proxy (AWS ALB) rejects
// raw application/grpc — see PROBE_RESULTS.md. The default surfd profile wraps
// the daemon with gRPC-Web on the same port so callers reach it via the
// preview URL. URL() returns the preview HTTPS URL (cached at create-time) and
// Token() returns the X-Daytona-Preview-Token callers must send.
package daytona

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/surf-dev/surf/apps/orchestrator"
)

// surfNameLabel is how we tag sandboxes with their orchestrator-side Spec.Name.
// Daytona has no native "name" attribute on sandboxes — only id + labels —
// so we round-trip our identity through this label.
const surfNameLabel = "surf-name"

// agentSessionID is the toolbox session that runs the guest agent's
// long-running process. Caller-supplied; deterministic per sandbox so we can
// reattach. The value is opaque.
const agentSessionID = "agent"

// agentPort is the port the guest agent listens on inside the Daytona sandbox.
// Must be inside Daytona's preview-proxy range (3000-9999) — that range is a
// Daytona property, not an agent property.
const agentPort = 3000

// agentBinaryPath is where a download-based agent binary lands inside the
// sandbox. It must match the binary path referenced by AgentSpec.Command
// (assembled by the core agent profile).
const agentBinaryPath = "/home/daytona/surfd"

// Config configures the Daytona provider client.
type Config struct {
	BaseURL     string // Daytona API base URL (default: client.DefaultBaseURL).
	APIKey      string // Daytona API key (Bearer token).
	HTTPClient  *http.Client
	DownloadURL string // Optional. If set, Boot fetches the guest agent binary into the sandbox before StartAgent.
}

// Provider is the Daytona-backed orchestrator.Provider implementation.
type Provider struct {
	cfg    Config
	client *Client
}

// New constructs a Daytona Provider. cfg.APIKey is required; passing an
// empty key is allowed for compile-checking but every API call will fail.
func New(cfg Config) *Provider {
	return &Provider{
		cfg:    cfg,
		client: NewClient(cfg.BaseURL, cfg.APIKey, cfg.HTTPClient),
	}
}

// --- orchestrator.Provider ---

// Create provisions a fresh Daytona sandbox tagged with the supplied
// Spec.Name, fetches its preview URL/token (cached on the returned env),
// and returns a *sandbox handle. Resource sizing (CPU/RamMB/StorageGB)
// is currently ignored: Daytona's default snapshot rejects sizing and
// custom snapshots are a Phase-2 concern (see ORCHESTRATOR_PLAN B5).
func (p *Provider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	zero := 0
	req := CreateSandboxRequest{
		Labels:           map[string]string{surfNameLabel: spec.Name},
		AutoStopInterval: &zero, // never auto-stop; orchestrator owns lifecycle
	}
	sb, err := p.client.CreateSandbox(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("daytona: create sandbox: %w", err)
	}

	env := newSandbox(p, sb.ID, spec.Name)
	if err := env.fetchPreviewLocked(ctx); err != nil {
		// If we can't even fetch the preview URL, the sandbox is unusable
		// to us — best-effort destroy and surface the error.
		_ = p.client.DeleteSandbox(context.Background(), sb.ID)
		return nil, fmt.Errorf("daytona: fetch preview url: %w", err)
	}
	return env, nil
}

// Get resolves a sandbox by Spec.Name (label lookup). Returns
// orchestrator.ErrVMNotFound when no matching sandbox exists or when the
// sole match has been destroyed.
func (p *Provider) Get(ctx context.Context, name string) (orchestrator.Environment, error) {
	sandboxes, err := p.client.ListSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("daytona: list sandboxes: %w", err)
	}
	for i := range sandboxes {
		sb := &sandboxes[i]
		if sb.Labels[surfNameLabel] != name {
			continue
		}
		if isDestroyed(sb) {
			continue
		}
		env := newSandbox(p, sb.ID, name)
		if err := env.fetchPreviewLocked(ctx); err != nil {
			// Sandbox exists but preview fetch failed — return env without
			// URL/Token; caller can retry. Don't fail the whole Get.
			return env, nil
		}
		return env, nil
	}
	return nil, orchestrator.ErrVMNotFound
}

// Destroy tears down the sandbox identified by Spec.Name. Idempotent:
// returns nil if the sandbox is already gone.
func (p *Provider) Destroy(ctx context.Context, name string) error {
	sandboxes, err := p.client.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("daytona: list sandboxes: %w", err)
	}
	for i := range sandboxes {
		sb := &sandboxes[i]
		if sb.Labels[surfNameLabel] != name || isDestroyed(sb) {
			continue
		}
		if err := p.client.DeleteSandbox(ctx, sb.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("daytona: delete sandbox %s: %w", sb.ID, err)
		}
	}
	return nil
}

// List returns every active sandbox whose surf-name starts with prefix.
func (p *Provider) List(ctx context.Context, prefix string) ([]orchestrator.Environment, error) {
	sandboxes, err := p.client.ListSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("daytona: list sandboxes: %w", err)
	}
	out := make([]orchestrator.Environment, 0, len(sandboxes))
	for i := range sandboxes {
		sb := &sandboxes[i]
		name := sb.Labels[surfNameLabel]
		if name == "" || isDestroyed(sb) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, newSandbox(p, sb.ID, name))
	}
	return out, nil
}

// Close releases provider resources. Currently a no-op since the HTTP
// client is shared and its idle connections are reaped by Go runtime.
func (p *Provider) Close() error { return nil }

// isDestroyed reports whether the API has already torn the sandbox down
// (or has been asked to). We treat both as "doesn't exist" for our
// purposes so the orchestrator can re-Create cleanly.
func isDestroyed(sb *Sandbox) bool {
	switch strings.ToLower(sb.State) {
	case "destroyed", "destroying", "deleted", "deleting":
		return true
	}
	switch strings.ToLower(sb.DesiredState) {
	case "destroyed", "deleted":
		return true
	}
	return false
}

// --- orchestrator.Environment + TokenSetter ---

// sandbox is the per-sandbox handle returned from Create / Get / List.
// It caches the preview URL + token (which require an API round-trip to
// fetch) and the per-VM auth token set by orchestrator.Boot.
type sandbox struct {
	provider *Provider
	id       string
	name     string

	mu           sync.RWMutex
	previewURL   string
	previewToken string
	authToken    string

	// agentStarted tracks whether we've already called CreateSession +
	// SessionExec for the guest agent's long-running process. Idempotency
	// net for retries / re-Boot.
	agentStarted bool

	// createdDirs caches the parent directories we've already mkdir'd inside
	// the sandbox, keyed by directory path. Avoids a redundant mkdir on every
	// Upload while staying correct regardless of upload order (AgentSpec.Files
	// iterates a map, so order is non-deterministic).
	createdDirs map[string]bool
}

func newSandbox(p *Provider, id, name string) *sandbox {
	return &sandbox{provider: p, id: id, name: name}
}

// Compile-time interface checks.
var (
	_ orchestrator.Provider    = (*Provider)(nil)
	_ orchestrator.Environment = (*sandbox)(nil)
	_ orchestrator.TokenSetter = (*sandbox)(nil)
)

func (s *sandbox) Name() string { return s.name }

// URL returns the public preview URL for the guest agent. May be empty until
// the preview lookup completes; orchestrator code that depends on it should
// call this after Provider.Create returns successfully.
func (s *sandbox) URL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.previewURL
}

// Token returns the X-Daytona-Preview-Token paired with URL(). Callers
// MUST attach this on every HTTP/gRPC request to URL(); without it the
// preview proxy returns 401.
func (s *sandbox) Token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Daytona-specific behavior: the preview token IS the auth token from
	// the caller's perspective. Per-VM auth tokens generated by Boot are
	// uploaded into the sandbox (for surfd's own use) but the preview
	// proxy gates inbound traffic on the Daytona token. Returning the
	// preview token keeps "use Token() to reach URL()" working uniformly
	// across Firecracker (per-VM) and Daytona (preview).
	if s.previewToken != "" {
		return s.previewToken
	}
	return s.authToken
}

// SetToken records the surfd auth token uploaded by orchestrator.Boot.
// We keep it for completeness (token rotation hooks rely on it) even
// though Daytona's URL() is gated by the preview token, not this one.
func (s *sandbox) SetToken(token string) {
	s.mu.Lock()
	s.authToken = token
	s.mu.Unlock()
}

// fetchPreviewLocked refreshes the cached preview URL+token. Safe to
// call repeatedly; later calls overwrite earlier values.
func (s *sandbox) fetchPreviewLocked(ctx context.Context) error {
	pv, err := s.provider.client.GetPreviewURL(ctx, s.id, agentPort)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.previewURL = pv.URL
	s.previewToken = pv.Token
	s.mu.Unlock()
	return nil
}

// Exec runs a one-shot command synchronously and returns combined output.
// Note: Daytona's /process/execute endpoint returns a single "result"
// field that conflates stdout+stderr. ExecStream is preferred for any
// caller that cares about the split.
func (s *sandbox) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := buildShellCommand(name, args)
	resp, err := s.provider.client.Execute(ctx, s.id, cmd)
	if err != nil {
		return nil, fmt.Errorf("daytona exec: %w", err)
	}
	if resp.ExitCode != 0 {
		return []byte(resp.Result), fmt.Errorf("daytona exec: command %q exited %d", cmd, resp.ExitCode)
	}
	return []byte(resp.Result), nil
}

// ExecStream runs a command via the Daytona session API so we can tail
// its log stream into stdout/stderr. NOTE: Daytona's session log endpoint
// returns a single text/plain stream — stderr writes are best-effort
// duplicates of stdout. Callers needing a real split should not rely on
// this on Daytona.
func (s *sandbox) ExecStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	sessionID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	if err := s.provider.client.CreateSession(ctx, s.id, sessionID); err != nil {
		return fmt.Errorf("daytona session create: %w", err)
	}
	cmd := buildShellCommand(name, args)
	resp, err := s.provider.client.SessionExec(ctx, s.id, sessionID, cmd, true)
	if err != nil {
		return fmt.Errorf("daytona session exec: %w", err)
	}
	rc, err := s.provider.client.SessionLogs(ctx, s.id, sessionID, resp.CmdID)
	if err != nil {
		return fmt.Errorf("daytona session logs: %w", err)
	}
	defer rc.Close()

	// Tee combined stream to both writers; we don't have a way to split.
	target := io.MultiWriter(nonNilWriter(stdout), nonNilWriter(stderr))
	if _, err := io.Copy(target, rc); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("daytona session log copy: %w", err)
	}
	return nil
}

// Upload writes data to a file path inside the sandbox, ensuring the file's
// parent directory exists first (Daytona's default image has no /surf at the
// rootfs, and arbitrary AgentSpec.Files may target other paths too).
func (s *sandbox) Upload(ctx context.Context, data []byte, path string) error {
	if err := s.ensureGuestDirs(ctx, path); err != nil {
		return err
	}
	return s.provider.client.Upload(ctx, s.id, path, data)
}

// ensureGuestDirs mkdirs the parent directory of an upload path on first use,
// caching each distinct directory so repeated uploads to the same dir don't
// re-mkdir. This generalizes the old /surf-prefix special case: whatever
// AgentSpec.Files declares, its parent dir is prepared. Correct regardless of
// upload order.
func (s *sandbox) ensureGuestDirs(ctx context.Context, path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." || dir == "/" {
		// Upload targets the root — nothing to create.
		return nil
	}

	s.mu.RLock()
	ready := s.createdDirs[dir]
	s.mu.RUnlock()
	if ready {
		return nil
	}

	if _, err := s.provider.client.Execute(ctx, s.id, "mkdir -p "+shellEscape(dir)); err != nil {
		return fmt.Errorf("daytona prepare guest dir %s: %w", dir, err)
	}
	s.mu.Lock()
	if s.createdDirs == nil {
		s.createdDirs = make(map[string]bool)
	}
	s.createdDirs[dir] = true
	s.mu.Unlock()
	return nil
}

// StartAgent launches the guest agent inside the sandbox via the session API
// as a long-running, asynchronous process. Daytona runs AgentSpec.Command
// verbatim (the full command line is assembled in the core agent profile).
// Idempotent: repeated calls return nil after the first successful start.
//
// If a download URL is configured (AgentSpec.DownloadURL, falling back to
// Config.DownloadURL), the agent binary is fetched into agentBinaryPath before
// launch.
func (s *sandbox) StartAgent(ctx context.Context, spec orchestrator.AgentSpec) error {
	s.mu.RLock()
	already := s.agentStarted
	s.mu.RUnlock()
	if already {
		return nil
	}

	if err := s.ensureAgentBinary(ctx, spec.DownloadURL, agentBinaryPath); err != nil {
		return err
	}

	// Recreate the agent session if a previous attempt half-failed.
	// CreateSession is documented as 201; if it returns 409 (already exists)
	// on a retry we proceed anyway. We swallow CreateSession errors here and
	// rely on SessionExec to report any genuine problem.
	_ = s.provider.client.CreateSession(ctx, s.id, agentSessionID)

	if _, err := s.provider.client.SessionExec(ctx, s.id, agentSessionID, spec.Command, true); err != nil {
		return fmt.Errorf("daytona start agent: %w", err)
	}

	s.mu.Lock()
	s.agentStarted = true
	s.mu.Unlock()
	return nil
}

// ensureAgentBinary downloads the guest agent binary from downloadURL (falling
// back to Config.DownloadURL for back-compat) when it isn't already present at
// dest. No-op when no URL is configured — caller is assumed to have
// provisioned a custom snapshot with the agent baked in.
func (s *sandbox) ensureAgentBinary(ctx context.Context, downloadURL, dest string) error {
	url := downloadURL
	if url == "" {
		url = s.provider.cfg.DownloadURL
	}
	if url == "" {
		return nil
	}
	// `set -e; test -x <dest> || (curl -fsSL <url> -o <dest> && chmod +x <dest>)`
	escDest := shellEscape(dest)
	check := fmt.Sprintf(
		`set -e; test -x %s || (curl -fsSL %s -o %s && chmod +x %s)`,
		escDest, shellEscape(url), escDest, escDest,
	)
	if _, err := s.provider.client.Execute(ctx, s.id, check); err != nil {
		return fmt.Errorf("daytona fetch agent: %w", err)
	}
	return nil
}

// --- helpers ---

// buildShellCommand joins an exec name + args into a single shell line.
// Daytona's /process/execute takes a single string, not argv. We assume
// the orchestrator's callers do not pass shell metacharacters; there is
// no portable argv-quoting in shell, so we settle for `name arg1 arg2`
// joined by spaces. This matches the existing Firecracker provider which
// also flattens to a single command line for the host agent.
func buildShellCommand(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, name)
	parts = append(parts, args...)
	return strings.Join(parts, " ")
}

// shellEscape produces a single-quoted shell literal that is safe to
// embed inside a shell pipeline. Internal single quotes are escaped via
// the standard '\'' technique.
func shellEscape(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// nonNilWriter returns w if non-nil, or io.Discard otherwise. Used so we
// can MultiWriter without panicking on a nil stdout/stderr arg.
func nonNilWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
