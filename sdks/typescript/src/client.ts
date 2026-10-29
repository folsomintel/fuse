import { Transport, type FetchLike } from "./transport.js";
import { EnvironmentsService } from "./environments.js";
import { SnapshotsService } from "./snapshots.js";
import { HostsService } from "./hosts.js";
import { ApiKeysService } from "./apiKeys.js";
import { InterventionsService } from "./interventions.js";
import { VERSION } from "./version.js";
import type { ActorLabel } from "./types.js";

/** Options for constructing a FuseClient. */
export interface FuseClientOptions {
  /** Base URL of the Fuse control plane, e.g. "https://fuse.example.com". Required. */
  baseUrl: string;
  /** Bearer token (master token or an API key). Omit for unauthenticated endpoints. */
  token?: string;
  /** Custom fetch implementation. Defaults to the global fetch (Node 18+). */
  fetch?: FetchLike;
  /** User-Agent header. Defaults to "fuse-ts/<version>". Ignored by browsers. */
  userAgent?: string;
  /** Generator for the X-Request-ID header, called once per request. Empty results are omitted. */
  requestId?: () => string;
  /** Default per-request timeout in milliseconds. Does NOT apply to events() streams. */
  timeoutMs?: number;
  /** Extra default headers merged into every request. */
  headers?: Record<string, string>;
  /** The human this client acts for, sent as the Fuse-Actor and
   * Fuse-Actor-Name headers on every request. */
  actor?: ActorLabel;
}

/**
 * FuseClient is the entry point for the Fuse API. It groups the resource
 * services that share a single transport.
 *
 * @example
 * const client = new FuseClient({ baseUrl: "https://fuse.example.com", token });
 * const env = await client.environments.create({ task_id: "task-1" });
 */
export class FuseClient {
  readonly environments: EnvironmentsService;
  readonly snapshots: SnapshotsService;
  readonly hosts: HostsService;
  readonly apiKeys: ApiKeysService;
  readonly interventions: InterventionsService;
  private readonly options: FuseClientOptions;

  constructor(options: FuseClientOptions) {
    this.options = options;
    const transport = new Transport({
      baseUrl: options.baseUrl,
      token: options.token,
      fetch: options.fetch,
      userAgent: options.userAgent ?? `fuse-ts/${VERSION}`,
      requestId: options.requestId,
      timeoutMs: options.timeoutMs,
      headers: options.headers,
      actor: options.actor,
    });
    this.environments = new EnvironmentsService(transport);
    this.snapshots = new SnapshotsService(transport);
    this.hosts = new HostsService(transport);
    this.apiKeys = new ApiKeysService(transport);
    this.interventions = new InterventionsService(transport);
  }

  /**
   * withActor returns a copy of this client, with the same options otherwise,
   * that sends `actor` as the Fuse-Actor headers. This client is unchanged.
   */
  withActor(actor: ActorLabel): FuseClient {
    return new FuseClient({ ...this.options, actor });
  }
}
