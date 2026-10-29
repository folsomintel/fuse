import type { CallOptions, Transport } from "./transport.js";
import { isInterventionTerminal, type Intervention } from "./types.js";
import { requireArg } from "./validate.js";

/** The server's max page size; list() requests this internally. */
const MAX_PAGE_LIMIT = 200;

/** How long one long-poll asks the server to hold. It stays well under a
 * typical client timeout; the server caps it at 50s. */
const WAIT_POLL = "30s";

/** Filters (and pagination) for interventions.list / interventions.listPage. */
export interface ListInterventionsOptions {
  /** Only interventions raised by this environment. */
  envId?: string;
  /** One of InterventionState. */
  state?: string;
  /** Page size. Only consulted by listPage; list() always walks every page. */
  limit?: number;
  /** Opaque cursor from a previous listPage() call's nextCursor. */
  cursor?: string;
}

interface InterventionList {
  interventions: Intervention[];
  next_cursor?: string;
}

/** One page of interventions.listPage, plus the cursor to fetch the next one. */
export interface InterventionPage {
  interventions: Intervention[];
  /** Empty/undefined once there are no more results. */
  nextCursor?: string;
}

/** Body for interventions.approve. */
export interface ApproveOptions {
  /** An ask needs an answer (one of its choices, when it has any). */
  answer?: string;
  reason?: string;
}

/** Body for interventions.reject. */
export interface RejectOptions {
  /** Fed back to the agent; the server fills one in when empty. */
  reason?: string;
}

/** InterventionsService reads and decides hitl interventions. */
export class InterventionsService {
  constructor(private readonly t: Transport) {}

  /** List every intervention matching options, oldest first, walking every
   * page. */
  async list(
    options: ListInterventionsOptions = {},
    opts: CallOptions = {},
  ): Promise<Intervention[]> {
    const out: Intervention[] = [];
    let cursor = options.cursor;
    for (;;) {
      const page = await this.listPage(
        { ...options, limit: MAX_PAGE_LIMIT, cursor },
        opts,
      );
      out.push(...page.interventions);
      if (!page.nextCursor) break;
      cursor = page.nextCursor;
    }
    return out;
  }

  /** List one page of interventions matching options. */
  async listPage(
    options: ListInterventionsOptions = {},
    opts: CallOptions = {},
  ): Promise<InterventionPage> {
    const out = await this.t.json<InterventionList>("GET", "/v1/interventions", {
      query: {
        env: options.envId,
        state: options.state,
        limit: options.limit ? String(options.limit) : undefined,
        cursor: options.cursor,
      },
      signal: opts.signal,
    });
    return { interventions: out.interventions ?? [], nextCursor: out.next_cursor };
  }

  /** Fetch one intervention without waiting. */
  async get(id: string, opts: CallOptions = {}): Promise<Intervention> {
    return getIntervention(this.t, id, undefined, opts);
  }

  /**
   * Block until the intervention resolves. Every intervention has a deadline,
   * so this always returns eventually. A dropped connection or an
   * orchestrator restart is survived by polling again; the server, not the
   * connection, holds the state. Pass `opts.signal` to stop early.
   */
  async wait(id: string, opts: CallOptions = {}): Promise<Intervention> {
    return waitIntervention(this.t, id, opts);
  }

  /** Resolve a pending intervention as approved. Requires the master token. */
  async approve(
    id: string,
    body: ApproveOptions = {},
    opts: CallOptions = {},
  ): Promise<Intervention> {
    return this.decide(id, "approve", body, opts);
  }

  /** Resolve a pending intervention as rejected. Requires the master token. */
  async reject(
    id: string,
    body: RejectOptions = {},
    opts: CallOptions = {},
  ): Promise<Intervention> {
    return this.decide(id, "reject", { reason: body.reason }, opts);
  }

  private async decide(
    id: string,
    action: string,
    body: ApproveOptions,
    opts: CallOptions,
  ): Promise<Intervention> {
    requireArg(id, "intervention id");
    return this.t.json<Intervention>(
      "POST",
      `/v1/interventions/${encodeURIComponent(id)}`,
      {
        query: { action },
        body: { answer: body.answer || undefined, reason: body.reason || undefined },
        signal: opts.signal,
      },
    );
  }
}

/** waitIntervention long-polls until the intervention is terminal. */
export async function waitIntervention(
  t: Transport,
  id: string,
  opts: CallOptions = {},
): Promise<Intervention> {
  for (;;) {
    const i = await getIntervention(t, id, WAIT_POLL, opts);
    if (isInterventionTerminal(i)) return i;
    opts.signal?.throwIfAborted();
  }
}

function getIntervention(
  t: Transport,
  id: string,
  wait: string | undefined,
  opts: CallOptions,
): Promise<Intervention> {
  requireArg(id, "intervention id");
  return t.json<Intervention>("GET", `/v1/interventions/${encodeURIComponent(id)}`, {
    query: { wait },
    signal: opts.signal,
  });
}
