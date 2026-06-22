import type { CallOptions, Transport } from "./transport.js";
import type { Snapshot, SnapshotRequest } from "./types.js";
import { requireArg } from "./validate.js";

/** Filters for snapshots.list. */
export interface ListSnapshotsOptions {
  vmId?: string;
  taskId?: string;
  tenantId?: string;
  state?: string;
}

interface SnapshotList {
  snapshots: Snapshot[];
}

/** SnapshotsService manages microVM snapshots. */
export class SnapshotsService {
  constructor(private readonly t: Transport) {}

  /** Create a snapshot of a running environment. */
  async create(
    vmId: string,
    body: SnapshotRequest = {},
    opts: CallOptions = {},
  ): Promise<Snapshot> {
    requireArg(vmId, "vm id");
    return this.t.json<Snapshot>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}/snapshots`,
      { body, signal: opts.signal },
    );
  }

  /** List snapshots, optionally filtered. */
  async list(
    options: ListSnapshotsOptions = {},
    opts: CallOptions = {},
  ): Promise<Snapshot[]> {
    const out = await this.t.json<SnapshotList>("GET", "/v1/snapshots", {
      query: {
        vm_id: options.vmId,
        task_id: options.taskId,
        tenant_id: options.tenantId,
        state: options.state,
      },
      signal: opts.signal,
    });
    return out.snapshots ?? [];
  }

  /** Fetch a single snapshot by id. */
  async get(snapshotId: string, opts: CallOptions = {}): Promise<Snapshot> {
    requireArg(snapshotId, "snapshot id");
    return this.t.json<Snapshot>(
      "GET",
      `/v1/snapshots/${encodeURIComponent(snapshotId)}`,
      { signal: opts.signal },
    );
  }

  /** Delete a snapshot. Idempotent; the snapshot must be a leaf. */
  async delete(snapshotId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(snapshotId, "snapshot id");
    await this.t.noContent("DELETE", `/v1/snapshots/${encodeURIComponent(snapshotId)}`, {
      signal: opts.signal,
    });
  }

  /** Restore an environment from a snapshot. */
  async restore(snapshotId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(snapshotId, "snapshot id");
    await this.t.noContent("POST", `/v1/snapshots/${encodeURIComponent(snapshotId)}`, {
      query: { action: "restore" },
      signal: opts.signal,
    });
  }
}
