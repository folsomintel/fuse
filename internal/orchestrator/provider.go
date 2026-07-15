// Package orchestrator defines the interface for sandbox providers and
// implements the boot flow that provisions or restores environments.
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// Provider manages sandboxed environments.
type Provider interface {
	// Create provisions a new sandbox.
	Create(ctx context.Context, spec Spec) (Environment, error)

	// Get returns a handle to an existing sandbox by name.
	Get(ctx context.Context, name string) (Environment, error)

	// Destroy tears down a sandbox.
	Destroy(ctx context.Context, name string) error

	// List returns all sandboxes matching the given prefix.
	List(ctx context.Context, prefix string) ([]Environment, error)

	// Close releases provider resources.
	Close() error
}

// Spec describes the resources needed for a sandbox.
type Spec struct {
	Name      string // e.g. "fuse-{task-id}"
	CPUs      int
	RamMB     int
	StorageGB int
	Region    string

	// GPUs is the count of whole GPU devices requested. Zero means no GPU.
	GPUs int32

	// GPUKind identifies the requested GPU model (e.g. "a100"). Empty when
	// GPUs is 0.
	GPUKind string

	// MaxRuntime overrides FleetConfig.TaskStuckTimeout for this task.
	// Zero means "use the fleet default". This is a leak-detection ceiling,
	// not a liveness check — set it higher than any plausible healthy runtime.
	MaxRuntime time.Duration

	// Image names a base rootfs for the provider to boot from, resolved by
	// the provider (e.g. the firecracker host agent looks it up in its own
	// named-rootfs directory). Empty means the provider's default base.
	Image string
}

// ExposeSpec requests that a guest port be published as a reachable
// endpoint by the provider during StartAgent.
type ExposeSpec struct {
	Port int
	As   string
}

// Endpoint is a published network endpoint for an environment (e.g. an
// ingress port exposed via the Fusefile's `expose` list).
type Endpoint struct {
	As   string // caller-chosen label, e.g. "http"
	URL  string // reachable address, e.g. "http://203.0.113.5:41231"
	Port int    // the guest-side port this endpoint publishes
}

// Environment is a running sandbox.
type Environment interface {
	// Name returns the sandbox identifier.
	Name() string

	// URL returns the sandbox's reachable address for the guest agent.
	URL() string

	// Exec runs a command and returns combined output.
	Exec(ctx context.Context, name string, args ...string) ([]byte, error)

	// ExecStream runs a command with stdout/stderr wired to writers.
	ExecStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error

	// Upload writes data to a file path inside the sandbox.
	Upload(ctx context.Context, data []byte, path string) error

	// StartAgent launches the configured guest agent inside the sandbox.
	StartAgent(ctx context.Context, spec AgentSpec) error

	// Token returns the per-sandbox auth token that callers must include
	// when reaching URL(). Empty string for providers whose URL
	// self-authenticates.
	Token() string
}

// TokenSetter is implemented by environments whose per-sandbox auth
// token can be updated after construction (e.g. set during Boot once
// credentials are generated, or refreshed by token rotation). Boot and
// RotateToken type-assert to this interface so the Environment surface
// stays read-only for callers.
type TokenSetter interface {
	SetToken(token string)
}

// SnapshotCapable is implemented by environments that support
// checkpoint/restore. Providers that cannot snapshot (e.g. some
// container-based providers) simply omit these methods; callers must
// type-assert to SnapshotCapable before invoking them.
type SnapshotCapable interface {
	Checkpoint(ctx context.Context, comment string) (string, error)
	Restore(ctx context.Context, checkpointID string) error
	ListCheckpoints(ctx context.Context) ([]Checkpoint, error)
}

// SnapshotForkable is implemented by providers that can create a brand-new
// environment seeded from an existing checkpoint of another vm. this is the
// capability a true fork needs; the firecracker provider does not implement
// it yet, so ForkEnvironment reports fork as unsupported at runtime until a
// host wire endpoint exists.
type SnapshotForkable interface {
	CreateFromCheckpoint(ctx context.Context, spec Spec, srcVMID, checkpointID string) (Environment, error)
}

// SnapshotDeleter is implemented by environments that can delete a
// previously created checkpoint by ID.
type SnapshotDeleter interface {
	DeleteCheckpoint(ctx context.Context, checkpointID string) error
}

// CapacityProber is implemented by providers that can report the real
// hardware capacity of the host they front (CPU count, total RAM, free
// disk) instead of trusting operator-declared numbers. RegisterHost type-
// asserts to this interface at registration time to source capacity for any
// field the operator left unset; providers that cannot probe (e.g. a stub
// with no real hardware behind it) simply return an error.
type CapacityProber interface {
	Capacity(ctx context.Context) (HostCapacity, error)
}

// AgentSpec is the generic, provider-agnostic description of the guest agent
// to launch inside a sandbox. fused is expressed as one configuration of this
// spec via FusedAgentSpec (see agent_profile.go); nothing fuse-specific is
// hardcoded in the core boot path.
type AgentSpec struct {
	Files        map[string][]byte // arbitrary files to upload into the guest (path -> bytes)
	DownloadURL  string            // fetch the agent binary (e.g. GitHub releases)
	Command      string            // how to launch the daemon
	AuthToken    string            // pass-through bearer token
	Gateway      string            // pass-through gateway websocket URL
	GatewayToken string            // pass-through gateway token
	DrainCommand string            // command run inside the guest for graceful shutdown ('' => skip)
	Expose       []ExposeSpec      // guest ports to publish as reachable endpoints, if any
}

type BootOptions struct {
	StartupScript string
	GatewayURL    string
	GatewayToken  string
	Expose        []ExposeSpec
}

// EndpointReporter is implemented by environments that can report additional
// network endpoints published during StartAgent (e.g. via ingress/expose).
// Providers that don't support ingress simply omit it, in which case Boot
// reports no endpoints — consistent with how SnapshotCapable/SnapshotForkable
// are optional per-provider capabilities.
type EndpointReporter interface {
	Endpoints() []Endpoint
}

// Checkpoint is a snapshot of a sandbox.
type Checkpoint struct {
	ID        string
	Comment   string
	SizeBytes int64
	CreatedAt time.Time
}

// BootResult is returned after provisioning or restoring an environment.
type BootResult struct {
	Env                Environment
	BootTime           time.Duration
	FromCache          bool       // true if restored from checkpoint
	AuthTokenEncrypted []byte     // AES-GCM encrypted per-VM auth token for persistence
	DrainCommand       string     // graceful-shutdown command for the configured agent ('' => skip)
	Endpoints          []Endpoint // published endpoints, if the provider reported any
}

// reportedEndpoints returns env.Endpoints() when env implements
// EndpointReporter, or nil otherwise. Shared by bootFresh/bootRestore so a
// provider without ingress support (the common case today) contributes no
// endpoints rather than requiring every Environment to implement the method.
func reportedEndpoints(env Environment) []Endpoint {
	if er, ok := env.(EndpointReporter); ok {
		return er.Endpoints()
	}
	return nil
}

// bootInputs bundles the inputs needed by bootFresh and bootRestore.
// It exists purely to keep helper signatures readable.
type bootInputs struct {
	spec      Spec
	manifest  []byte
	secretMap map[string]string
	creds     *secrets.VMCredentials
	agentSpec AgentSpec
	opts      BootOptions
}

// uploadFiles uploads every path->bytes entry of files into the env. The
// generic boot path drives all guest uploads (manifest, secrets, credentials)
// through this from AgentSpec.Files; the agent profile decides which files
// exist and where they land.
func uploadFiles(ctx context.Context, env Environment, files map[string][]byte) error {
	for path, data := range files {
		if err := env.Upload(ctx, data, path); err != nil {
			return fmt.Errorf("upload %s: %w", path, err)
		}
	}
	return nil
}

// setTokenIfSupported records the per-VM auth token on the env (when it
// implements TokenSetter) so env.Token() returns the active token without a
// separate fetch. Credential files themselves are uploaded via AgentSpec.Files
// (see FusedAgentSpec); this helper only handles the in-memory token side
// effect. Reused by Boot (fresh/restore) and RotateToken.
func setTokenIfSupported(env Environment, creds *secrets.VMCredentials) {
	if creds == nil {
		return
	}
	if ts, ok := env.(TokenSetter); ok {
		ts.SetToken(creds.AuthToken)
	}
}

// runStartupScript executes BootOptions.StartupScript inside the env, if set.
func runStartupScript(ctx context.Context, env Environment, script string) error {
	if script == "" {
		return nil
	}
	if err := env.ExecStream(ctx, io.Discard, io.Discard, "sh", "-lc", script); err != nil {
		return fmt.Errorf("startup script: %w", err)
	}
	return nil
}

// bootRestore attempts to restore env from its latest checkpoint and bring
// the guest agent back up. Returns (nil, nil) on a clean fall-through to fresh
// provision (e.g. provider not SnapshotCapable, no checkpoints, restore
// failed). Returns (result, nil) on successful restore. Returns (nil, err)
// only for hard errors that should abort Boot entirely (e.g. startup script
// failure).
func bootRestore(ctx context.Context, existing Environment, in bootInputs, start time.Time, encToken []byte) (*BootResult, error) {
	sc, ok := existing.(SnapshotCapable)
	if !ok {
		return nil, nil
	}
	checkpoints, cpErr := sc.ListCheckpoints(ctx)
	if cpErr != nil || len(checkpoints) == 0 {
		return nil, nil
	}
	latest := checkpoints[len(checkpoints)-1]
	if restoreErr := sc.Restore(ctx, latest.ID); restoreErr != nil {
		return nil, nil
	}

	// Re-upload the agent's files (manifest/secrets/credentials all live in
	// AgentSpec.Files, populated by the profile). Credential files carry the
	// SetToken side effect via setTokenIfSupported below.
	_ = uploadFiles(ctx, existing, in.agentSpec.Files)
	setTokenIfSupported(existing, in.creds)
	if err := runStartupScript(ctx, existing, in.opts.StartupScript); err != nil {
		return nil, err
	}
	_ = existing.StartAgent(ctx, in.agentSpec)
	return &BootResult{
		Env:                existing,
		BootTime:           time.Since(start),
		FromCache:          true,
		AuthTokenEncrypted: encToken,
		DrainCommand:       in.agentSpec.DrainCommand,
		Endpoints:          reportedEndpoints(existing),
	}, nil
}

// bootFresh provisions a new environment via p.Create, uploads the agent's
// files (manifest/secrets/credentials, all carried in AgentSpec.Files), runs
// the startup script, and starts the agent.
func bootFresh(ctx context.Context, p Provider, in bootInputs, start time.Time, encToken []byte) (*BootResult, error) {
	env, err := p.Create(ctx, in.spec)
	if err != nil {
		return nil, err
	}

	// Upload everything the agent profile declared. For fused this is the
	// manifest, secrets JSON, and (when present) the TLS/auth credential
	// files. The guest is responsible for mounting any sensitive paths on
	// tmpfs (see PRD-08 for the fused profile's /fuse contract).
	if err := uploadFiles(ctx, env, in.agentSpec.Files); err != nil {
		return nil, err
	}
	// Credential files were uploaded above; record the token on the env so
	// env.Token() returns it.
	setTokenIfSupported(env, in.creds)

	if err := runStartupScript(ctx, env, in.opts.StartupScript); err != nil {
		return nil, err
	}

	if err := env.StartAgent(ctx, in.agentSpec); err != nil {
		return nil, err
	}

	return &BootResult{
		Env:                env,
		BootTime:           time.Since(start),
		AuthTokenEncrypted: encToken,
		DrainCommand:       in.agentSpec.DrainCommand,
		Endpoints:          reportedEndpoints(env),
	}, nil
}

// Boot provisions or restores an environment, uploads the agent's files, and
// starts the configured guest agent. If a matching checkpoint exists, it
// restores from it. Otherwise creates fresh. When encryptionKey is non-nil (32
// bytes), per-VM TLS credentials and an auth token are generated and injected.
// The encrypted token is returned in BootResult for persistence.
//
// The guest agent is described by an AgentSpec; today that is always the fused
// profile (FusedAgentSpec), which carries the fused manifest/secrets/TLS files
// and launch command. Boot itself is profile-agnostic.
func Boot(ctx context.Context, p Provider, spec Spec, manifest []byte, secretMap map[string]string, opts BootOptions, encryptionKey []byte) (*BootResult, error) {
	start := time.Now()

	// Generate per-VM credentials when an encryption key is provided.
	var creds *secrets.VMCredentials
	var encToken []byte
	if len(encryptionKey) == 32 {
		var err error
		creds, err = secrets.GenerateVMCredentials(spec.Name)
		if err != nil {
			return nil, fmt.Errorf("generate vm credentials: %w", err)
		}
		encToken, err = secrets.EncryptToken(creds.AuthToken, encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt vm token: %w", err)
		}
	}

	// Build the fused profile: this is the SINGLE place the fused /fuse/*
	// paths and launch command are assembled (see agent_profile.go). Credential
	// files are included in AgentSpec.Files and uploaded generically; the
	// in-memory token side effect is applied by setTokenIfSupported in
	// bootFresh/bootRestore.
	agentSpec := FusedAgentSpec(manifest, secretMap, creds, opts)

	in := bootInputs{
		spec:      spec,
		manifest:  manifest,
		secretMap: secretMap,
		creds:     creds,
		agentSpec: agentSpec,
		opts:      opts,
	}

	// Try restoring from an existing checkpoint first. bootRestore returns
	// (nil, nil) when restore is not applicable (provider not SnapshotCapable,
	// no checkpoints, or restore failed) so we fall through to fresh provision.
	if existing, err := p.Get(ctx, spec.Name); err == nil {
		result, err := bootRestore(ctx, existing, in, start, encToken)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
	}

	return bootFresh(ctx, p, in, start, encToken)
}
