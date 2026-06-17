package fuse

import "time"

// Spec is the hardware/runtime spec for a microVM.
type Spec struct {
	CPUs              int32  `json:"cpus,omitempty"`
	RamMB             int32  `json:"ram_mb,omitempty"`
	StorageGB         int32  `json:"storage_gb,omitempty"`
	Region            string `json:"region,omitempty"`
	MaxRuntimeSeconds int64  `json:"max_runtime_seconds,omitempty"`
}

// CreateRequest is the body for client.Create.
type CreateRequest struct {
	TaskID         string            `json:"task_id"`
	Spec           Spec              `json:"spec"`
	ManifestInline string            `json:"manifest_inline,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	StartupScript  string            `json:"startup_script,omitempty"`
	GatewayURL     string            `json:"gateway_url,omitempty"`
	GatewayToken   string            `json:"gateway_token,omitempty"`
}

// EnvironmentInfo is the server's view of a single microVM.
type EnvironmentInfo struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	TaskID    string    `json:"task_id"`
	HostID    string    `json:"host_id,omitempty"`
	URL       string    `json:"url"`
	Spec      Spec      `json:"spec"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Error     string    `json:"error,omitempty"`
}

type environmentList struct {
	Environments []EnvironmentInfo `json:"environments"`
}

// Lifecycle states for EnvironmentInfo.State and Event.State.
const (
	StateProvisioning = "provisioning"
	StateRunning      = "running"
	StateDraining     = "draining"
	StateDestroying   = "destroying"
	StateDestroyed    = "destroyed"
	StateFailed       = "failed"
)

// IsTerminalState reports whether state is a terminal lifecycle state.
func IsTerminalState(state string) bool {
	return state == StateDestroyed || state == StateFailed
}

// Event is one item from env.Events. It matches the server's wire
// payload. Err is set only on a stream-level failure, as the final
// event before the channel closes.
type Event struct {
	ID        string    `json:"id"`
	Kind      string    `json:"event"`
	VMID      string    `json:"vm_id"`
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Err       error     `json:"-"`
}

// SnapshotRequest is the optional body for env.Snapshot.
type SnapshotRequest struct {
	Comment          string            `json:"comment,omitempty"`
	Mode             string            `json:"mode,omitempty"`
	RetentionSeconds int64             `json:"retention_seconds,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ExportRef        string            `json:"export_ref,omitempty"`
	ExportStatus     string            `json:"export_status,omitempty"`
}

// SnapshotExport is an optional exported snapshot artifact.
type SnapshotExport struct {
	Destination string    `json:"destination"`
	Status      string    `json:"status,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// Snapshot is a persisted snapshot record.
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

type snapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// HostCapacity is a host's resource envelope.
type HostCapacity struct {
	CPUs      int `json:"cpus"`
	RamMB     int `json:"ram_mb"`
	StorageGB int `json:"storage_gb"`
	VMCount   int `json:"vm_count"`
}

// RegisterHostRequest is the body for client.RegisterHost.
type RegisterHostRequest struct {
	ID       string       `json:"id"`
	URL      string       `json:"url"`
	Token    string       `json:"token,omitempty"`
	Region   string       `json:"region,omitempty"`
	Capacity HostCapacity `json:"capacity"`
}

// Host is the server's view of a registered host.
type Host struct {
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

type hostList struct {
	Hosts []Host `json:"hosts"`
}

// APIKey is a key's metadata. The raw secret appears only in
// CreatedAPIKey.Key, returned once at creation.
type APIKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreatedAPIKey is returned by client.CreateAPIKey. Key is unrecoverable
// after this response.
type CreatedAPIKey struct {
	APIKey
	Key string `json:"key"`
}

type apiKeyList struct {
	APIKeys []APIKey `json:"api_keys"`
}
