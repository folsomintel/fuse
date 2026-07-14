// Wire types for the Fuse API. Field names match the JSON sent on the wire
// (snake_case), so request/response bodies pass through JSON.stringify /
// JSON.parse untouched — a faithful 1:1 mirror of the Go SDK's struct tags
// (see sdks/go/types.go). List *options* (see each service) use camelCase
// keys and are translated to snake_case query params by the service.

/** Spec is the hardware/runtime spec for a microVM. */
export interface Spec {
  cpus?: number;
  ram_mb?: number;
  storage_gb?: number;
  gpus?: number;
  gpu_kind?: string;
  region?: string;
  max_runtime_seconds?: number;
  image?: string;
}

/** ExposeSpec requests that a guest port be published at boot. */
export interface ExposeSpec {
  port: number;
  as?: string;
}

/** Endpoint is a published port with its externally reachable URL. */
export interface Endpoint {
  as?: string;
  url: string;
  port: number;
}

/** CreateRequest is the body for environments.create. */
export interface CreateRequest {
  task_id: string;
  spec?: Spec;
  manifest_inline?: string;
  secrets?: Record<string, string>;
  startup_script?: string;
  gateway_url?: string;
  gateway_token?: string;
  expose?: ExposeSpec[];
}

/** EnvironmentInfo is the server's view of a single microVM. */
export interface EnvironmentInfo {
  id: string;
  state: string;
  task_id: string;
  host_id?: string;
  url: string;
  spec: Spec;
  created_at: string;
  updated_at: string;
  error?: string;
  endpoints?: Endpoint[];
}

/** ForkOptions is the optional body for environments.fork. */
export interface ForkOptions {
  reuse_snapshot_id?: string;
  comment?: string;
}

/** ExecRequest is the body for environments.exec. Exactly one of cmd or shell must be set. */
export interface ExecRequest {
  /**
   * The argv to run in the guest, e.g. ["ls", "-l"]. Argv needs no quoting
   * rules and cannot be turned into an injection by interpolating a value, so
   * prefer it.
   */
  cmd?: string[];

  /**
   * Runs the string under `sh -lc`. Use it only for what argv cannot express:
   * pipelines, redirects, and globs.
   */
  shell?: string;

  /**
   * Bounds the command inside the guest. Zero or unset takes the server
   * default; the server clamps anything above its ceiling.
   */
  timeout_ms?: number;
}

/**
 * ExecResult is the outcome of a guest command.
 *
 * A non-zero exit_code is a successful call: the command ran and failed. Only a
 * thrown error means the command could not be run at all.
 */
export interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
}

/**
 * Event is one item from environments.events. It matches the server's SSE
 * wire payload. Stream-level failures are thrown from the async iterator
 * rather than delivered as an in-band event.
 */
export interface Event {
  id: string;
  /** Event kind. v1 only emits "state". */
  event: string;
  vm_id: string;
  state: string;
  url?: string;
  error?: string;
  updated_at: string;
}

/** SnapshotRequest is the optional body for snapshots.create. */
export interface SnapshotRequest {
  comment?: string;
  mode?: string;
  retention_seconds?: number;
  metadata?: Record<string, string>;
  export_ref?: string;
  export_status?: string;
}

/** SnapshotExport is an optional exported snapshot artifact. */
export interface SnapshotExport {
  destination: string;
  status?: string;
  requested_at?: string;
  updated_at?: string;
  last_error?: string;
}

/** Snapshot is a persisted snapshot record. */
export interface Snapshot {
  id: string;
  vm_id: string;
  task_id?: string;
  tenant_id?: string;
  parent_snapshot_id?: string;
  mode?: string;
  state?: string;
  comment?: string;
  size_bytes?: number;
  created_at: string;
  updated_at?: string;
  retention_until?: string;
  last_error?: string;
  export_ref?: string;
  exports?: SnapshotExport[];
}

/** HostCapacity is a host's resource envelope. */
export interface HostCapacity {
  cpus: number;
  ram_mb: number;
  storage_gb: number;
  vm_count: number;
  gpus?: number;
  gpu_kind?: string;
}

/** RegisterHostRequest is the body for hosts.register. */
export interface RegisterHostRequest {
  id: string;
  url: string;
  token?: string;
  region?: string;
  backend?: string;
  capacity?: HostCapacity;
}

/** Host is the server's view of a registered host. */
export interface Host {
  id: string;
  url: string;
  region?: string;
  state: string;
  backend?: string;
  capacity: HostCapacity;
  allocated: HostCapacity;
  last_seen: string;
  created_at: string;
  updated_at: string;
}

/**
 * APIKey is a key's metadata. The raw secret appears only in
 * CreatedAPIKey.key, returned once at creation.
 */
export interface APIKey {
  id: string;
  label?: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
}

/**
 * CreatedAPIKey is returned by apiKeys.create. `key` is the raw secret and is
 * unrecoverable after this response.
 */
export interface CreatedAPIKey extends APIKey {
  key: string;
}

/** Lifecycle states for EnvironmentInfo.state and Event.state. */
export const State = {
  Provisioning: "provisioning",
  Running: "running",
  Draining: "draining",
  Destroying: "destroying",
  Destroyed: "destroyed",
  Failed: "failed",
} as const;

export type State = (typeof State)[keyof typeof State];

/** isTerminalState reports whether state is a terminal lifecycle state. */
export function isTerminalState(state: string): boolean {
  return state === State.Destroyed || state === State.Failed;
}
