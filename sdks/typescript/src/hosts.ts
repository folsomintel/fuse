import type { CallOptions, Transport } from "./transport.js";
import type { Host, RegisterHostRequest } from "./types.js";
import { requireArg } from "./validate.js";

interface HostList {
  hosts: Host[];
}

/** HostsService manages registered Firecracker hosts. */
export class HostsService {
  constructor(private readonly t: Transport) {}

  /** Register (or re-register) a host. Idempotent on id. */
  async register(body: RegisterHostRequest, opts: CallOptions = {}): Promise<Host> {
    return this.t.json<Host>("POST", "/v1/hosts", { body, signal: opts.signal });
  }

  /** List registered hosts. */
  async list(opts: CallOptions = {}): Promise<Host[]> {
    const out = await this.t.json<HostList>("GET", "/v1/hosts", { signal: opts.signal });
    return out.hosts ?? [];
  }

  /** Fetch a single host by id. */
  async get(hostId: string, opts: CallOptions = {}): Promise<Host> {
    requireArg(hostId, "host id");
    return this.t.json<Host>("GET", `/v1/hosts/${encodeURIComponent(hostId)}`, {
      signal: opts.signal,
    });
  }

  /** Mark a host unschedulable; existing VMs keep running. */
  async cordon(hostId: string, opts: CallOptions = {}): Promise<void> {
    await this.action(hostId, "cordon", opts);
  }

  /** Return a cordoned host to active scheduling. */
  async uncordon(hostId: string, opts: CallOptions = {}): Promise<void> {
    await this.action(hostId, "uncordon", opts);
  }

  /** Remove a host. The host must have no running VMs. */
  async deregister(hostId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(hostId, "host id");
    await this.t.noContent("DELETE", `/v1/hosts/${encodeURIComponent(hostId)}`, {
      signal: opts.signal,
    });
  }

  private async action(hostId: string, action: string, opts: CallOptions): Promise<void> {
    requireArg(hostId, "host id");
    await this.t.noContent("POST", `/v1/hosts/${encodeURIComponent(hostId)}`, {
      query: { action },
      signal: opts.signal,
    });
  }
}
