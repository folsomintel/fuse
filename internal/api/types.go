// Package api is the HTTP (REST) surface of the orchestrator. It wraps
// FleetManager and Environment operations behind a chi-based router,
// serves the harness, and is consumed by operations tooling. The shape
// of every request and response mirrors apps/orchestrator/api/openapi.yaml,
// which is the human-readable contract for this API.
//
// Types in this file are hand-maintained and intentionally decoupled
// from the internal FleetManager/orchestrator types so that the wire
// contract can evolve independently of the in-process API. The
// conversion helpers in convert.go enforce that separation.
package api

import "time"

// ResourceSpec is the JSON shape of the hardware spec attached to an
// environment create request or response body.
//
// Image names a base rootfs for the provider to boot the VM from (a name
// the firecracker host agent resolves against its own named-rootfs
// directory). Empty means the provider's default base.
type ResourceSpec struct {
	CPUs              int32  `json:"cpus,omitempty"`
	RamMB             int32  `json:"ram_mb,omitempty"`
	StorageGB         int32  `json:"storage_gb,omitempty"`
	Region            string `json:"region,omitempty"`
	MaxRuntimeSeconds int64  `json:"max_runtime_seconds,omitempty"`
	Image             string `json:"image,omitempty"`
}

// ExposeSpec requests that a guest port be published as a reachable
// endpoint.
type ExposeSpec struct {
	Port int    `json:"port"`
	As   string `json:"as,omitempty"`
}

// Endpoint is a published network endpoint for an environment.
type Endpoint struct {
	As   string `json:"as,omitempty"`
	URL  string `json:"url"`
	Port int    `json:"port"`
}

// CreateEnvironmentRequest is the JSON body accepted by
// POST /v1/environments.
//
// ManifestInline is optional raw manifest JSON, base64-encoded.
// When omitted, the orchestrator uses a minimal internal manifest.
type CreateEnvironmentRequest struct {
	TaskID         string            `json:"task_id"`
	Spec           ResourceSpec      `json:"spec"`
	ManifestInline string            `json:"manifest_inline"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	StartupScript  string            `json:"startup_script,omitempty"`
	GatewayURL     string            `json:"gateway_url,omitempty"`
	GatewayToken   string            `json:"gateway_token,omitempty"`
	Expose         []ExposeSpec      `json:"expose,omitempty"`
}

// Environment is the JSON shape returned for a single VM.
type Environment struct {
	ID        string       `json:"id"`
	State     string       `json:"state"`
	TaskID    string       `json:"task_id"`
	HostID    string       `json:"host_id,omitempty"`
	URL       string       `json:"url"`
	Spec      ResourceSpec `json:"spec"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Error     string       `json:"error,omitempty"`
	Endpoints []Endpoint   `json:"endpoints,omitempty"`
}

// EnvironmentList is the response body for GET /v1/environments.
type EnvironmentList struct {
	Environments []Environment `json:"environments"`
}

// CreateSnapshotRequest is the optional body for POST
// /v1/environments/{vm}/snapshots.
type CreateSnapshotRequest struct {
	Comment          string            `json:"comment,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	RetentionSeconds int64             `json:"retention_seconds,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ExportRef        string            `json:"export_ref,omitempty"`
	ExportStatus     string            `json:"export_status,omitempty"`
}

// ForkEnvironmentRequest is the optional body for
// POST /v1/environments/{vmId}?action=fork.
type ForkEnvironmentRequest struct {
	ReuseSnapshotID string `json:"reuse_snapshot_id,omitempty"`
	Comment         string `json:"comment,omitempty"`
}

// SnapshotExport is the JSON shape of an optional exported snapshot artifact.
type SnapshotExport struct {
	Destination string    `json:"destination"`
	Status      string    `json:"status,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// Snapshot is the JSON shape of a persisted snapshot record.
type Snapshot struct {
	ID               string           `json:"id"`
	VMID             string           `json:"vm_id"`
	TaskID           string           `json:"task_id,omitempty"`
	TenantID         string           `json:"tenant_id,omitempty"`
	ParentSnapshotID string           `json:"parent_snapshot_id,omitempty"`
	Mode             string           `json:"mode,omitempty"`
	State            string           `json:"state,omitempty"`
	Comment          string           `json:"comment,omitempty"`
	SizeBytes        int64            `json:"size_bytes,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at,omitempty"`
	RetentionUntil   *time.Time       `json:"retention_until,omitempty"`
	LastError        string           `json:"last_error,omitempty"`
	ExportRef        string           `json:"export_ref,omitempty"`
	Exports          []SnapshotExport `json:"exports,omitempty"`
}

// SnapshotList is the response body for GET /v1/snapshots.
type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// Error is the JSON envelope returned for every non-2xx response. It
// intentionally wraps a single inner Error object so clients have one
// place to look for machine-readable failure metadata.
type Error struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner payload of an Error envelope.
type ErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// Standard error codes. The REST boundary uses these to give callers a
// stable vocabulary for programmatic handling.
const (
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeInvalidArgument = "invalid_argument"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
	CodeUnauthorized    = "unauthorized"
)

// ── API key types ──────────────────────────────────────────────────

// CreateAPIKeyRequest is the JSON body accepted by POST /v1/api-keys.
// label is an optional human memory aid ("ci", "partner-acme").
type CreateAPIKeyRequest struct {
	Label string `json:"label,omitempty"`
}

// APIKey is the JSON shape of a key's metadata. The raw secret is never
// included here — it appears only once, in CreateAPIKeyResponse.
type APIKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreateAPIKeyResponse is returned by POST /v1/api-keys. Key is the raw
// secret and is shown exactly once — it cannot be recovered afterward.
type CreateAPIKeyResponse struct {
	APIKey
	Key string `json:"key"`
}

// APIKeyList is the response body for GET /v1/api-keys.
type APIKeyList struct {
	APIKeys []APIKey `json:"api_keys"`
}

// ── Host management types ──────────────────────────────────────────

// HostCapacity is the wire shape of a host's resource envelope.
type HostCapacity struct {
	CPUs      int `json:"cpus"`
	RamMB     int `json:"ram_mb"`
	StorageGB int `json:"storage_gb"`
	VMCount   int `json:"vm_count"`
}

// RegisterHostRequest is the JSON body accepted by POST /v1/hosts.
type RegisterHostRequest struct {
	ID       string       `json:"id"`
	URL      string       `json:"url"`
	Token    string       `json:"token,omitempty"`
	Region   string       `json:"region,omitempty"`
	Capacity HostCapacity `json:"capacity"`
}

// HostInfo is the JSON shape returned for a single host.
type HostInfo struct {
	ID        string       `json:"id"`
	URL       string       `json:"url"`
	Region    string       `json:"region,omitempty"`
	State     string       `json:"state"`
	Capacity  HostCapacity `json:"capacity"`
	Allocated HostCapacity `json:"allocated"`
	LastSeen  time.Time    `json:"last_seen"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// HostList is the response body for GET /v1/hosts.
type HostList struct {
	Hosts []HostInfo `json:"hosts"`
}
