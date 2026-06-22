import type { CallOptions, Transport } from "./transport.js";
import type { CreateRequest, EnvironmentInfo, Event } from "./types.js";
import { requireArg } from "./validate.js";
import { streamEvents } from "./events.js";

/** Filters for environments.list. */
export interface ListEnvironmentsOptions {
  taskId?: string;
  state?: string;
  hostId?: string;
}

interface EnvironmentList {
  environments: EnvironmentInfo[];
}

/** EnvironmentsService manages microVM lifecycle. */
export class EnvironmentsService {
  constructor(private readonly t: Transport) {}

  /** List tracked environments, optionally filtered. */
  async list(
    options: ListEnvironmentsOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentInfo[]> {
    const out = await this.t.json<EnvironmentList>("GET", "/v1/environments", {
      query: { task_id: options.taskId, state: options.state, host_id: options.hostId },
      signal: opts.signal,
    });
    return out.environments ?? [];
  }

  /** Fetch a single environment by VM id. */
  async get(vmId: string, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    return this.t.json<EnvironmentInfo>(
      "GET",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { signal: opts.signal },
    );
  }

  /** Provision a new environment. Blocks until running or creation fails. */
  async create(body: CreateRequest, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    return this.t.json<EnvironmentInfo>("POST", "/v1/environments", {
      body,
      signal: opts.signal,
    });
  }

  /** Gracefully drain an environment; returns the updated environment. */
  async drain(vmId: string, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    return this.t.json<EnvironmentInfo>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "drain" }, signal: opts.signal },
    );
  }

  /** Rotate the per-VM credentials and push them to the guest. */
  async rotateToken(vmId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(vmId, "vm id");
    await this.t.noContent("POST", `/v1/environments/${encodeURIComponent(vmId)}`, {
      query: { action: "rotate-token" },
      signal: opts.signal,
    });
  }

  /** Forcefully destroy an environment. Idempotent. */
  async destroy(vmId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(vmId, "vm id");
    await this.t.noContent("DELETE", `/v1/environments/${encodeURIComponent(vmId)}`, {
      signal: opts.signal,
    });
  }

  /**
   * Open the SSE event stream for an environment. The returned promise rejects
   * with a FuseApiError on a connect-time failure (e.g. not_found); on success
   * it resolves to an AsyncIterable that yields each Event and ends after a
   * terminal-state event. Pass `opts.signal` to cancel; there is no built-in
   * timeout on the stream.
   *
   * @example
   * for await (const ev of await client.environments.events(id)) {
   *   console.log(ev.state);
   * }
   */
  async events(vmId: string, opts: CallOptions = {}): Promise<AsyncIterable<Event>> {
    requireArg(vmId, "vm id");
    return streamEvents(this.t, vmId, opts.signal);
  }
}
