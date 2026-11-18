// Package orchestrator defines the interface for sandbox providers and
// implements the boot flow that provisions or restores environments.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
	"github.com/folsomintel/fuse/internal/hitl"
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

	// Arch restricts scheduling to hosts of this CPU architecture, in
	// GOARCH vocabulary ("amd64", "arm64"). Empty means any host: images
	// are resolved host-side, so a spec that doesn't care about arch keeps
	// the pre-arch behavior. A host with no reported arch counts as amd64.
	Arch string

	// GPUs is the count of GPU devices requested: whole devices, or MIG
	// instances when GPUProfile is set. Zero means no GPU.
	GPUs int32

	// GPUKind identifies the requested GPU model (e.g. "a100"). Empty when
	// GPUs is 0.
	GPUKind string

	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// mig-parted vocabulary (e.g. "1g.10gb"), passed through as an mdev
	// device by the qemu host agent (decision D5). Empty means whole-device
	// allocation.
	GPUProfile string

	// GPUUUIDs are the concrete device uuids this VM is bound to. Populated
	// by allocation on hosts that report per-device inventory
	// (Capacity.GPUDevices non-empty) and persisted per-VM so the binding
	// survives an orchestrator restart. Empty on legacy homogeneous hosts
	// that only track a scalar GPU count.
	GPUUUIDs []string `json:"gpu_uuids,omitempty"`

	// MIGInstanceUUIDs are the concrete MIG instance uuids this VM is bound
	// to. Populated by allocation on hosts that report per-instance MIG
	// inventory (Capacity.MIGInstances non-empty) and persisted per-VM so
	// the binding survives an orchestrator restart. Empty when the host
	// only tracks MIG allocation by profile count (the MIGProfiles path).
	// Mirrors GPUUUIDs for whole devices (issue #41).
	MIGInstanceUUIDs []string `json:"mig_instance_uuids,omitempty"`

	// HostID pins this sandbox to an exact host id. Unlike GPUUUIDs and
	// MIGInstanceUUIDs, which allocation fills in, this is caller-settable:
	// it comes from the Fusefile's placement.host. A pin is a hard gate, not
	// an override, so the pinned host still has to be active, run the right
	// backend, and fit the spec.
	HostID string

	// Labels are placement label selectors: every pair must match the target
	// host's declared labels. Caller-settable, from placement.labels.
	Labels map[string]string

	// MaxRuntime overrides FleetConfig.TaskStuckTimeout for this task.
	// Zero means "use the fleet default". This is a leak-detection ceiling,
	// not a liveness check — set it higher than any plausible healthy runtime.
	MaxRuntime time.Duration

	// IdleTimeout destroys this VM once it has gone this long with no exec
	// and no attach session. Zero means "use FleetConfig.DefaultIdleTimeout"
	// (itself zero by default, i.e. no idle expiry). Unlike MaxRuntime this
	// is a liveness check, but only over control-plane activity: in-guest CPU
	// and network traffic are not observed.
	IdleTimeout time.Duration

	// Image names a base rootfs for the provider to boot from, resolved by
	// the provider (e.g. the firecracker host agent looks it up in its own
	// named-rootfs directory). Empty means the provider's default base.
	Image string

	// ImageDigest boots a converted oci image (see images.go). the fleet
	// resolves it at create into Image (the rootfs name on the host) and
	// PinnedHostID (a host where it is ready), so nothing past placement
	// reads it. Mutually exclusive with Image and SeedSnapshotID.
	ImageDigest string

	// SeedSnapshotID boots this VM from a persisted snapshot artifact instead
	// of Image. Unlike a fork there is no live source VM: the artifact is
	// resolved out of the host agent's snapshot store. It is host-local, which
	// is why SeedSnapshotID always travels with PinnedHostID. Empty means boot
	// from Image. Mutually exclusive with Image.
	SeedSnapshotID string

	// ResumeSeed asks the provider to resume the guest from the seed's memory
	// image instead of cold-booting its rootfs. only a live migration sets it,
	// and only with a live SeedSnapshotID already copied to PinnedHostID. it is
	// a request the host may refuse (wrong cpu, network slot taken), never a
	// hint it may ignore: a live snapshot's rootfs is not bootable on its own.
	ResumeSeed bool

	// HugePages backs guest memory with 2M pages. it is fixed at boot, the
	// host needs that much memory reserved as hugepages, and only a vm booted
	// with it can be migrated lazily (MigrateOptions.Lazy).
	HugePages bool

	// PinnedHostID restricts scheduling to a single host. Set when the VM must
	// land where its host-local seed artifact already is. Empty means schedule
	// across the fleet as usual.
	PinnedHostID string

	// Egress is the outbound networking policy. the zero value is direct,
	// which is the pre-egress behaviour, so every stored record and every
	// caller that predates the field deserializes into it without naming
	// it. see internal/egress.
	Egress egress.Spec

	// Hitl is the human-in-the-loop policy, or nil when hitl is off. it
	// lives on the spec so forks and migrates inherit it with the rest.
	// the orchestrator enforces it; nothing of it goes into the guest.
	Hitl *hitl.Spec

	// Ancestors is the fork lineage, oldest first: a fork appends its
	// source's id, a migrate keeps it unchanged (same logical environment).
	// it is what lets an invite scoped to an environment cover its forks.
	Ancestors []string
}

// default sizing for a create that leaves cpus/ram_mb/storage_gb at 0. the
// host agents pass these straight to firecracker and qemu, which refuse a
// 0 vcpu or 0 MiB machine, so a zero must never reach the host. storage
// matches the 4G base rootfs fc-bake-rootfs.sh bakes.
const (
	DefaultCPUs      = 1
	DefaultRamMB     = 1024
	DefaultStorageGB = 4
)

// withDefaultResources fills each of cpus/ram_mb/storage_gb left at 0 with
// its default, before the scheduler accounts the spec against a host.
func withDefaultResources(spec Spec) Spec {
	if spec.CPUs == 0 {
		spec.CPUs = DefaultCPUs
	}
	if spec.RamMB == 0 {
		spec.RamMB = DefaultRamMB
	}
	if spec.StorageGB == 0 {
		spec.StorageGB = DefaultStorageGB
	}
	return spec
}

// Protocol is the transport an exposed port is published on.
//
// It is a named type for the same reason SnapshotKind is, but it is open in
// the same way: a host agent newer than this orchestrator may report a
// transport this build has no constant for, and carrying it through beats
// discarding it. Nothing here rejects an unknown value. The Fusefile parser is
// the strict gate (fusefile.validProtocols), so a typo is caught while
// authoring rather than by refusing to talk to a peer.
type Protocol string

const (
	// ProtocolTCP is the default and the fallback. Every expose entry
	// written before this field existed is this, and an empty value read
	// back off the wire or out of the store means this.
	ProtocolTCP Protocol = "tcp"

	// ProtocolUDP publishes udp. The host agents install a separate
	// forwarding rule per transport rather than widening the tcp one, so a
	// service that speaks both needs two expose entries.
	ProtocolUDP Protocol = "udp"
)

// ExposeSpec requests that a guest port be published as a reachable
// endpoint by the provider during StartAgent.
type ExposeSpec struct {
	Port int
	As   string
	// Protocol is "tcp" or "udp". Empty means tcp: the API boundary
	// normalizes it (toOrchestratorExpose), but a record written before the
	// field existed still reads back empty, so consumers fall back rather
	// than assume.
	Protocol Protocol
}

// Endpoint is a published network endpoint for an environment (e.g. an
// ingress port exposed via the Fusefile's `expose` list).
type Endpoint struct {
	As   string // caller-chosen label, e.g. "http"
	URL  string // reachable address, e.g. "http://203.0.113.5:41231"
	Port int    // the guest-side port this endpoint publishes
	// Protocol is the transport the host agent actually published on, not
	// what was requested. Empty from an agent that predates the field, which
	// a reader takes as tcp.
	Protocol Protocol
}

// Environment is a running sandbox.
type Environment interface {
	// Name returns the sandbox identifier.
	Name() string

	// URL returns the sandbox's reachable address for the guest agent.
	URL() string

	// Exec runs argv inside the sandbox and reports the result. A non-zero
	// guest exit code is carried in ExecResult.ExitCode, not returned as an
	// error; err is reserved for transport and provider failures, so callers
	// must check both.
	Exec(ctx context.Context, cmd []string, opts ExecOptions) (ExecResult, error)

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

// Exec timeout bounds. Guest commands are bounded so a hung command cannot
// pin an SSH session on the host agent forever. MaxExecTimeout matches the
// host agent's own per-exec ceiling; asking for more would let the caller's
// deadline outlive the guest command it is waiting on.
const (
	DefaultExecTimeout = 60 * time.Second
	MaxExecTimeout     = 600 * time.Second
)

// ExecOptions tunes a single Exec call.
type ExecOptions struct {
	// Timeout bounds how long the guest command may run. Zero means
	// DefaultExecTimeout. Values above MaxExecTimeout are clamped.
	Timeout time.Duration
}

// ExecResult is the outcome of running a command inside a sandbox. Stdout and
// Stderr are kept separate and byte-exact; ExitCode is the guest command's
// own status, which is meaningful even when it is non-zero.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// AttachSpec describes an interactive attach to a sandbox.
type AttachSpec struct {
	// Cmd is the argv to run. Empty means the guest's login shell.
	Cmd []string

	// TTY requests a pty for the process. Rows and Cols seed its initial
	// window size; further resizes arrive as frames on the stream itself.
	TTY  bool
	Rows uint16
	Cols uint16
}

// Attacher is implemented by environments that can open a raw duplex byte
// stream to a process inside the sandbox. The stream carries the fuse-attach/1
// frame protocol (see docs/attach.md); nothing between the caller and the
// guest interprets those frames, so the orchestrator relays them verbatim.
//
// Environments with no real guest (e.g. in-memory stubs) omit this method;
// callers type-assert and report ErrAttachUnsupported when it is absent.
type Attacher interface {
	Attach(ctx context.Context, spec AttachSpec) (io.ReadWriteCloser, error)
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

// MemoryWatcher is implemented by providers whose guests can resume before all
// of their memory has arrived, so a caller can tell when the source of that
// memory is no longer needed.
type MemoryWatcher interface {
	MemoryStatus(ctx context.Context, vmID string) (MemoryStatus, error)
}

// MemoryStatus is how much of a lazily resumed guest's memory has arrived.
// Done means all of it, and Error means the transfer stopped for good.
type MemoryStatus struct {
	ResidentChunks int    `json:"resident_chunks"`
	TotalChunks    int    `json:"total_chunks"`
	Done           bool   `json:"done"`
	Error          string `json:"error,omitempty"`
}

// SnapshotDeleter is implemented by environments that can delete a
// previously created checkpoint by ID.
type SnapshotDeleter interface {
	DeleteCheckpoint(ctx context.Context, checkpointID string) error
}

// SnapshotDigester is an optional companion to SnapshotCapable for backends
// that hash the artifact while they write it. It exists as a separate
// interface rather than a wider Checkpoint signature because the digest is
// only ever available at creation time: the host agent computes it inline
// over the rootfs it just copied, and nothing recomputes it afterwards, so
// ListCheckpoints cannot supply it and changing Checkpoint's signature would
// churn every backend and caller for a value most of them cannot produce.
//
// An environment that does not implement this (the in-memory stub, qemu)
// simply yields no digest, which is not an error: the digest is an integrity
// check on a later transfer of those bytes, never an identity a lookup
// depends on.
type SnapshotDigester interface {
	CheckpointWithDigest(ctx context.Context, comment string) (Checkpoint, error)
}

// SnapshotKind distinguishes the two things a checkpoint can be. A disk
// snapshot is the rootfs and nothing else, so restoring it cold-boots the
// guest; a live snapshot additionally carries vCPU state and the whole guest
// memory, so restoring it resumes the guest exactly where it stopped.
//
// It is recorded rather than inferred. The two are indistinguishable once
// created unless something wrote down which one was asked for, and guessing
// wrong is not a cosmetic error: restoring a live artifact as a disk one
// silently cold-boots a guest the caller believed it had frozen.
type SnapshotKind string

const (
	// SnapshotKindDisk is the default and the fallback. Everything taken
	// before live snapshots existed is this, and anything that cannot be
	// classified reads back as this.
	SnapshotKindDisk SnapshotKind = "disk"

	// SnapshotKindLive is rootfs plus vCPU state plus the full memory image.
	SnapshotKindLive SnapshotKind = "live"
)

// LiveSnapshotCapable is an optional companion to SnapshotCapable for backends
// that can checkpoint guest memory as well as disk, in one pause window.
//
// It is a separate narrow interface rather than a wider SnapshotCapable for
// the same reason SnapshotDigester is: most backends cannot do this at all.
// Widening SnapshotCapable would be actively harmful here, not merely noisy.
// The qemu provider deliberately implements NEITHER interface (see
// internal/qemu/qemu.go and the guardrail tests in qemu_test.go), because a
// vfio-passed-through GPU cannot be checkpointed at all, and the orchestrator
// relies on that missing-method fact: the `env.(SnapshotCapable)` assertion
// failing is what stops a GPU environment from being snapshotted. Adding a
// method to SnapshotCapable that only firecracker can satisfy would force
// every backend to either implement it or drop out of the interface entirely,
// which is exactly the guardrail being leaned on.
//
// There is deliberately no RestoreLive. The host agent records the kind in the
// snapshot's own metadata and branches on it internally, so the existing
// Restore already restores a live snapshot correctly. A second restore verb
// would let a caller name a kind that disagrees with what was actually
// written, which is the one thing the recorded kind exists to prevent.
type LiveSnapshotCapable interface {
	CheckpointLive(ctx context.Context, comment string) (Checkpoint, error)
}

// MemoryShrinkCapable is implemented by environments whose guest can give
// memory back before a live snapshot (virtio-mem on firecracker). it is the
// same live snapshot CheckpointLive takes, only smaller: the guest unplugs
// what it is not using first, so those blocks never cross the wire, and gets
// them back when it resumes. a separate interface for the same reason
// LiveSnapshotCapable is one; a caller that finds it missing takes a plain
// live snapshot instead.
type MemoryShrinkCapable interface {
	CheckpointLiveShrunk(ctx context.Context, comment string) (Checkpoint, error)
}

// ArtifactDeleter is implemented by providers that can delete a free-standing
// copy in their host's snapshot store by id: a pulled artifact or a
// checkpoint base, neither of which has a vm to ask.
type ArtifactDeleter interface {
	DeleteArtifact(ctx context.Context, snapshotID string) error
}

// DeltaSnapshotCapable is implemented by environments that can take a diff
// live snapshot: only the memory pages and rootfs blocks changed since parent,
// which has to be the last live snapshot the environment took. keepPaused
// leaves the guest frozen afterwards, for the last hop of a migrate; Resume
// thaws it again if that migrate fails. a separate interface for the same
// reason LiveSnapshotCapable is one: most backends cannot do it.
type DeltaSnapshotCapable interface {
	CheckpointDiff(ctx context.Context, comment, parent string, keepPaused bool) (Checkpoint, error)
	Resume(ctx context.Context) error
}

// CapacityProber is implemented by providers that can report the real
// hardware capacity of the host they front (CPU count, total RAM, free
// disk, GPU inventory) instead of trusting operator-declared numbers. The
// registerHost API handler type-asserts the provider to this interface at
// registration time to source capacity for any field the operator left
// unset; providers that cannot probe (e.g. a stub with no real hardware
// behind it) simply return an error.
type CapacityProber interface {
	Capacity(ctx context.Context) (HostCapacity, error)
}

// HTTPStatusError carries the HTTP status code (and trimmed body) from a
// non-2xx host-agent response so callers can branch on the code without
// importing a specific provider package. Providers wrap their raw HTTP
// errors in this type; RegisterHost uses it to tell "agent rejected the
// token" (401) apart from "agent unreachable" or any other failure when the
// register-time capacity probe (see CapacityProber) errors out.
type HTTPStatusError struct {
	Code int
	Body string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

// IsNotFound reports whether err is, or wraps, a provider HTTP 404. Both the
// firecracker and qemu providers surface agent responses as *HTTPStatusError,
// so cleanup paths use this to tell "the resource is already gone" apart from
// a real provider failure. A vanished resource means the work is already done,
// not that the caller should retry forever.
func IsNotFound(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound
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

	// TunnelConfigPath is the guest path of an uploaded tunnel config (see
	// ingress.go). when set, the host agent runs the tunnel sidecar against it
	// as a unit of its own, so it outlives restarts of the agent. empty means
	// no sidecar, which is every environment when no proxy is configured.
	TunnelConfigPath string
}

type BootOptions struct {
	StartupScript string
	GatewayURL    string
	GatewayToken  string
	Expose        []ExposeSpec

	// Healthcheck is the environment-level readiness probe, or nil when the
	// caller declared none. It is shipped into the guest as a file (see
	// FusedAgentSpec) and evaluated there; the orchestrator only reads the
	// verdict back. See health.go.
	Healthcheck *HealthcheckSpec

	// Desktop is the graphical session's geometry, or nil when the caller
	// declared none. It is shipped into the guest as a file (see
	// FusedAgentSpec) and applied there; the orchestrator never renders
	// anything. See desktop.go.
	Desktop *DesktopSpec

	// TunnelConfig is the sidecar's config for an environment published
	// through fuse-proxy, or nil. set by the fleet, never by a caller.
	TunnelConfig []byte

	// Files are caller-supplied guest files (a Fusefile's `copy` block),
	// keyed by absolute guest path. The agent profile merges them into
	// AgentSpec.Files ahead of its own entries, so they are uploaded on the
	// same pass as the manifest and credentials and therefore land before
	// StartupScript runs, and so a caller can never displace a profile file.
	// The API refuses a path under ReservedGuestDir before it reaches here.
	Files map[string][]byte

	// StartupScriptTimeout bounds StartupScript. Zero means
	// DefaultStartupScriptTimeout. The script runs synchronously inside the
	// create request, so this must stay comfortably under the orchestrator's
	// HTTP write timeout (60s by default) for the caller to receive a real
	// error instead of a truncated response.
	StartupScriptTimeout time.Duration

	// HostID is the host the vm was scheduled onto. it is filled in by the
	// fleet, not the caller, so egress provisioning can name the host.
	HostID string

	// EgressRegistry holds the fleet's egress backends, filled in by the
	// fleet. nil means no proxy egress can be provisioned; a proxy spec then
	// fails the boot rather than silently booting direct.
	EgressRegistry *egress.Registry
}

// DefaultStartupScriptTimeout bounds a boot-time startup script when
// BootOptions.StartupScriptTimeout is unset. Chosen to leave headroom under
// the default 60s HTTP write timeout so the API can answer with a classified
// error rather than having the response cut off mid-flight.
const DefaultStartupScriptTimeout = 30 * time.Second

// DefaultMaxStartupScriptTimeout is the largest bound a caller may request via
// BootOptions.StartupScriptTimeout when FleetConfig.MaxStartupScriptTimeout is
// unset.
//
// The ceiling exists because the bound is caller-supplied but the cost is paid
// by the server: the script runs synchronously inside the create request, so a
// caller-chosen timeout is really a caller-chosen hold on a connection. 55s
// keeps the whole create under the default 60s HTTP write timeout while still
// giving authors most of a minute, nearly double the default.
//
// Raising it requires raising --write-timeout to match; an operator who does
// both gets a longer setup budget, and one who raises only this gets a
// truncated response instead of a classified error, which main.go refuses at
// startup rather than discovering per request.
const DefaultMaxStartupScriptTimeout = 55 * time.Second

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

	// Digest is the hex sha256 the host agent computed over the artifact as it
	// wrote it, or "" from an agent (or a backend) that does not hash. It
	// verifies a later transfer of these exact bytes and nothing else: two
	// builds of the same recipe produce different rootfs bytes, so it is never
	// an identity two artifacts can share and never a cache key.
	Digest string

	// Kind is what this checkpoint actually contains: rootfs only, or rootfs
	// plus guest memory. Backends that only ever take disk snapshots leave it
	// empty, which callers read as SnapshotKindDisk; empty is not an error,
	// it is an older agent (or a backend with nothing to distinguish).
	Kind SnapshotKind

	// Files is the memory half of a live checkpoint, file name to hex sha256.
	// it plays the part Digest plays for the rootfs: what a receiving host
	// verifies each file against when the snapshot moves. empty for a disk
	// checkpoint, and for a live one from an agent that predates moving them.
	// for a diff checkpoint the names are the delta files instead.
	Files map[string]string

	// Parent is the checkpoint a diff was taken against, "" for a complete
	// one. a diff is not restorable on its own; it only merges onto a copy of
	// its parent on another host.
	Parent string
}

// reportedEndpoints returns env.Endpoints() when env implements
// EndpointReporter, or nil otherwise. Used by every boot so a
// provider without ingress support (the common case today) contributes no
// endpoints rather than requiring every Environment to implement the method.
func reportedEndpoints(env Environment) []Endpoint {
	if er, ok := env.(EndpointReporter); ok {
		return er.Endpoints()
	}
	return nil
}

// EgressAddresser is implemented by environments that know the two ends of
// their tap: the host side, which is the only address a proxy can bind that
// the guest can reach and the host does not share, and the guest's own. an
// environment that omits it can still run direct egress; a proxy backend
// that needs the addresses gets empty strings and refuses.
type EgressAddresser interface {
	EgressAddresses() (listenIP, guestIP string)
}

// egressAddresses returns env.EgressAddresses() when env implements
// EgressAddresser, or two empty strings otherwise.
func egressAddresses(env Environment) (listenIP, guestIP string) {
	if ea, ok := env.(EgressAddresser); ok {
		return ea.EgressAddresses()
	}
	return "", ""
}

// egressContext is the provider-facing view of a booted vm. the host
// handle is set only for providers that expose the host-side egress wire;
// a backend that runs in this process never needs it.
func egressContext(p Provider, env Environment, in bootInputs) egress.Context {
	ec := egress.Context{VMID: in.spec.Name, HostID: in.opts.HostID}
	ec.ListenIP, ec.GuestIP = egressAddresses(env)
	if ha, ok := p.(egress.HostAgent); ok {
		ec.Host = ha
	}
	return ec
}

// provisionEgress brings up the backend a proxy spec asks for. it runs
// after Create, because the endpoint binds to the vm's tap, and before any
// upload, because the guest files that carry the endpoint have to go up in
// the same pass as the manifest. a direct spec returns a direct status and
// makes no other call into the egress package.
func provisionEgress(ctx context.Context, p Provider, env Environment, in bootInputs) (egress.Status, error) {
	spec := in.spec.Egress.Normalize()
	if !spec.IsProxy() {
		return egress.Status{Mode: egress.ModeDirect}, nil
	}
	if in.opts.EgressRegistry == nil {
		return egress.Status{}, fmt.Errorf("egress: mode %q requested but this orchestrator has no egress providers", spec.Mode)
	}
	ec := egressContext(p, env, in)
	if spec.Secret != "" {
		ec.Credential = in.secretMap[spec.Secret]
		if ec.Credential == "" {
			return egress.Status{}, fmt.Errorf("egress: secret %q named by egress.secret was not supplied", spec.Secret)
		}
	}
	ep, err := in.opts.EgressRegistry.Provision(ctx, spec, ec)
	if err != nil {
		return egress.Status{}, err
	}
	return egress.Status{
		Mode:     spec.Mode,
		Provider: ep.Provider,
		Protocol: ep.Protocol,
		Endpoint: ep.URL,
		Lease:    ep.Lease,
		Health:   egress.Health{State: egress.HealthUnknown},
	}, nil
}

// releaseEgress tears down what provisionEgress brought up, for the boot
// steps that can still fail after it. best effort: the fleet's rollback
// destroys the vm regardless, and a host-side backend is released by the
// host's own destroy path.
func releaseEgress(ctx context.Context, p Provider, env Environment, in bootInputs, status egress.Status) {
	if status.Mode != egress.ModeProxy || in.opts.EgressRegistry == nil {
		return
	}
	_ = in.opts.EgressRegistry.Destroy(ctx, status.Provider, egressContext(p, env, in))
}

// bootInputs bundles the inputs a boot's steps need.
// It exists purely to keep helper signatures readable.
type bootInputs struct {
	spec      Spec
	manifest  []byte
	secretMap map[string]string
	creds     *secrets.VMCredentials
	agentSpec AgentSpec
	opts      BootOptions
}

// maxConcurrentUploads bounds how many guest uploads are in flight at once.
//
// Every upload is an SSH session on the host agent, and until a Fusefile could
// carry a `copy` block the only caller was the agent profile with its three to
// six files, so the fan-out was unbounded because it could not get large. A
// copied source tree is hundreds of files, and opening hundreds of SSH sessions
// at a host agent at once is how a copy turns into a boot timeout.
const maxConcurrentUploads = 8

// uploadFiles uploads every path->bytes entry of files into the env,
// concurrently: each upload is an independent guest file with no shared
// state, so there is no reason to pay their round trips one after another.
// The generic boot path drives all guest uploads (manifest, secrets,
// credentials, and any caller files) through this from AgentSpec.Files; the
// agent profile decides which files exist and where they land.
func uploadFiles(ctx context.Context, env Environment, files map[string][]byte) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(files))
	sem := make(chan struct{}, maxConcurrentUploads)
	for path, data := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(path string, data []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := env.Upload(WithIdempotencyPart(ctx, "upload:"+path), data, path); err != nil {
				errs <- fmt.Errorf("upload %s: %w", path, err)
			}
		}(path, data)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
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
//
// The script is bounded by timeout (DefaultStartupScriptTimeout when zero).
// Boot runs synchronously inside the create request, so an unbounded script
// that never returns would hang the request until the HTTP write timeout cut
// the response, surfacing to the caller as an opaque 500 with a VM already
// running. Exceeding the bound is reported as ErrStartupScriptTimeout, which
// the API classifies as a client error.
func runStartupScript(ctx context.Context, env Environment, script string, timeout time.Duration) error {
	if script == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultStartupScriptTimeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := env.ExecStream(execCtx, io.Discard, io.Discard, "sh", "-lc", script); err != nil {
		// Only the bound we just imposed maps to the sentinel; a deadline
		// inherited from the caller's ctx is a genuine request timeout.
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%w after %s: a startup script must terminate; "+
				"background long-lived processes or declare them under `services`",
				ErrStartupScriptTimeout, timeout)
		}
		return fmt.Errorf("startup script: %w", err)
	}
	return nil
}

// restoreLatest restores env from its latest checkpoint. it reports false,
// and leaves env alone, when there is nothing to restore: the provider is not
// SnapshotCapable, it has no checkpoints, or the restore failed.
func restoreLatest(ctx context.Context, env Environment) bool {
	sc, ok := env.(SnapshotCapable)
	if !ok {
		return false
	}
	checkpoints, err := sc.ListCheckpoints(ctx)
	if err != nil || len(checkpoints) == 0 {
		return false
	}
	return sc.Restore(ctx, checkpoints[len(checkpoints)-1].ID) == nil
}

// restoreOrCreate is the first step of a boot: a vm by spec.Name that p
// already has is restored from its latest checkpoint, and anything else is
// created. fromCache reports a restore. an existing vm with nothing to
// restore is kept rather than created over, which is what a retried durable
// boot step needs (create-if-absent). fork's durable boot reuses this shape
// with CreateFromCheckpoint.
func restoreOrCreate(ctx context.Context, p Provider, spec Spec) (env Environment, fromCache bool, err error) {
	if existing, getErr := p.Get(ctx, spec.Name); getErr == nil {
		return existing, restoreLatest(ctx, existing), nil
	}
	env, err = p.Create(ctx, spec)
	if err != nil && spec.ImageDigest != "" && IsNotFound(err) {
		// resolveImage saw the image ready, and a convert on the host evicted
		// it before this create reached the agent.
		err = fmt.Errorf("%w: %s was evicted from the host before the create reached it; convert it again: %w",
			ErrImageNotReady, spec.ImageDigest, err)
	}
	return env, false, err
}

// bootRun is one boot between its steps: restore-or-create, egress, upload,
// startup script and start agent. the durable create runs each as a
// journaled step over a bootRun it rebuilds from the vm.
type bootRun struct {
	p         Provider
	in        bootInputs
	env       Environment
	fromCache bool
	egress    egress.Status
}

// provisionEgress brings up the egress the spec asks for. releaseFirst
// releases whatever backend may still remember the vm before provisioning:
// a restored vm's stale endpoint, or a durable step's earlier attempt.
func (b *bootRun) provisionEgress(ctx context.Context, releaseFirst bool) error {
	if releaseFirst {
		releaseEgress(ctx, b.p, b.env, b.in, egress.Status{Mode: b.in.spec.Egress.Normalize().Mode, Provider: b.in.spec.Egress.Provider})
	}
	status, err := provisionEgress(ctx, b.p, b.env, b.in)
	if err != nil {
		return err
	}
	b.egress = status
	return nil
}

// upload writes every file the agent profile declared (manifest, secrets,
// credentials, caller files, tunnel config) plus the egress files into the
// guest, then records the token on the env so env.Token() returns it.
func (b *bootRun) upload(ctx context.Context) error {
	if err := uploadFiles(ctx, b.env, withEgressFiles(b.in.agentSpec.Files, b.egress, b.env)); err != nil {
		return err
	}
	setTokenIfSupported(b.env, b.in.creds)
	return nil
}

// startup runs the caller's startup script, if any.
func (b *bootRun) startup(ctx context.Context) error {
	return runStartupScript(ctx, b.env, b.in.opts.StartupScript, b.in.opts.StartupScriptTimeout)
}

// startAgent launches the guest agent and returns the endpoints it published.
func (b *bootRun) startAgent(ctx context.Context) ([]Endpoint, error) {
	if err := b.env.StartAgent(ctx, b.in.agentSpec); err != nil {
		return nil, err
	}
	return reportedEndpoints(b.env), nil
}
