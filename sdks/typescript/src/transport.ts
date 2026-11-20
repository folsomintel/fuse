// The transport layer: builds requests (URL, headers, auth, X-Request-ID,
// JSON body), applies an optional client-default timeout, executes fetch, and
// turns non-2xx responses into FuseApiError. Mirrors the transport in
// sdks/go/environments.go. SSE streams use stream() which deliberately applies
// no timeout (the Go SDK uses a separate Timeout:0 client for events).

import { FuseError, errorFromResponse } from "./errors.js";
import { DEFAULT_RETRIES, retryDelayMs, sleep } from "./retry.js";
import {
  OperationState,
  type ActorLabel,
  type Intervention,
  type Operation,
  type OperationAccepted,
} from "./types.js";

const REQUEST_ID_HEADER = "X-Request-ID";
const ACTOR_HEADER = "Fuse-Actor";
const ACTOR_NAME_HEADER = "Fuse-Actor-Name";
const IDEMPOTENCY_KEY_HEADER = "Idempotency-Key";

/** How long one operation poll asks the server to hold. The server caps it
 * at its own attach bound. */
const OPERATION_POLL_WAIT = "30s";

/** A minimal fetch signature, satisfied by the global fetch and most polyfills. */
export type FetchLike = (input: string | URL, init?: RequestInit) => Promise<Response>;

/** Per-call options accepted by every service method. */
export interface CallOptions {
  /** Abort the request (and, for events, the stream). */
  signal?: AbortSignal;
}

/** Per-call options of a mutating call (create, destroy, fork, migrate,
 * drain, snapshot create and restore), which runs as an operation. */
export interface MutatingCallOptions extends CallOptions {
  /** Sent as the Idempotency-Key header, so a retry of the same request with
   * the same key returns the first call's result instead of running again.
   * Without it every call sends a fresh random key. */
  idempotencyKey?: string;
}

export interface TransportConfig {
  baseUrl: string;
  token?: string;
  fetch?: FetchLike;
  userAgent?: string;
  requestId?: () => string;
  timeoutMs?: number;
  headers?: Record<string, string>;
  actor?: ActorLabel;
  hitlNoWait?: boolean;
  onHeld?: (held: Intervention) => void;
  onOperation?: (accepted: OperationAccepted) => void;
  retries?: number;
}

/** Gated is the answer to a call a hitl gate may hold: the action's own
 * response body, or the held intervention of a 202. */
export type Gated<T> = { held: Intervention } | { held?: undefined; value: T };

type Query = Record<string, string | undefined>;

interface RequestSpec {
  query?: Query;
  body?: unknown;
  signal?: AbortSignal;
  /** Set on a mutating call: sent as Idempotency-Key, and a 202 operation is
   * followed until it finishes. */
  idempotencyKey?: string;
  /** Skip the client's default timeout; only signal bounds the request. */
  noTimeout?: boolean;
}

/** idempotencyKey is the key a mutating call sends: the caller's, or a fresh
 * random one generated once per call. */
export function idempotencyKey(opts: MutatingCallOptions): string {
  if (opts.idempotencyKey) return opts.idempotencyKey;
  // node 18 has no global crypto without --experimental-global-webcrypto.
  if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
  const part = () => Math.random().toString(36).slice(2);
  return `${Date.now().toString(36)}-${part()}${part()}`;
}

/** Combine zero or more AbortSignals into one, without relying on AbortSignal.any (Node 20+). */
export function combineSignals(signals: Array<AbortSignal | undefined>): {
  signal?: AbortSignal;
  cleanup: () => void;
} {
  const active = signals.filter((s): s is AbortSignal => s != null);
  if (active.length === 0) return { cleanup: () => {} };
  if (active.length === 1) return { signal: active[0], cleanup: () => {} };

  const controller = new AbortController();
  const onAbort = (event: Event) => {
    const target = event.target as AbortSignal;
    controller.abort(target.reason);
    cleanup();
  };
  const cleanup = () => {
    for (const s of active) s.removeEventListener("abort", onAbort);
  };
  for (const s of active) {
    if (s.aborted) {
      controller.abort(s.reason);
      cleanup();
      return { signal: controller.signal, cleanup: () => {} };
    }
    s.addEventListener("abort", onAbort);
  }
  return { signal: controller.signal, cleanup };
}

export class Transport {
  private readonly baseUrl: URL;
  private readonly token?: string;
  private readonly fetchImpl: FetchLike;
  private readonly userAgent?: string;
  private readonly requestId?: () => string;
  private readonly timeoutMs?: number;
  private readonly defaultHeaders?: Record<string, string>;
  private readonly actor?: ActorLabel;
  /** The client's hitl wait options. */
  readonly hitlNoWait: boolean;
  readonly onHeld?: (held: Intervention) => void;
  private readonly onOperation?: (accepted: OperationAccepted) => void;
  private readonly retries: number;

  constructor(cfg: TransportConfig) {
    if (!cfg.baseUrl) {
      throw new FuseError("base url is required");
    }
    let parsed: URL;
    try {
      parsed = new URL(cfg.baseUrl);
    } catch {
      throw new FuseError("base url is invalid");
    }
    this.baseUrl = parsed;
    this.token = cfg.token;
    this.userAgent = cfg.userAgent;
    this.requestId = cfg.requestId;
    this.timeoutMs = cfg.timeoutMs;
    this.defaultHeaders = cfg.headers;
    this.actor = cfg.actor;
    this.hitlNoWait = cfg.hitlNoWait ?? false;
    this.onHeld = cfg.onHeld;
    this.onOperation = cfg.onOperation;
    this.retries = Math.max(cfg.retries ?? DEFAULT_RETRIES, 0);

    const provided = cfg.fetch;
    if (provided) {
      this.fetchImpl = provided;
    } else if (typeof globalThis.fetch === "function") {
      this.fetchImpl = globalThis.fetch.bind(globalThis) as FetchLike;
    } else {
      throw new FuseError(
        "global fetch is not available; pass a fetch implementation via the `fetch` option (Node 18+ required)",
      );
    }
  }

  /** Perform a request and decode a JSON response body. */
  async json<T>(method: string, path: string, spec: RequestSpec = {}): Promise<T> {
    const { signal: combined, cleanup } = spec.noTimeout
      ? { signal: spec.signal, cleanup: () => {} }
      : this.withTimeout(spec.signal);
    try {
      const res = await this.send(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      return await this.decodeJson<T>(res);
    } finally {
      cleanup();
    }
  }

  /**
   * Perform a call a hitl gate may hold. A 202 answers with the held
   * intervention; anything else 2xx with the action's own (possibly empty)
   * body.
   */
  async gated<T>(
    method: string,
    path: string,
    spec: RequestSpec = {},
  ): Promise<Gated<T>> {
    const { signal: combined, cleanup } = this.withTimeout(spec.signal);
    try {
      const res = await this.send(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      if (res.status === 202) return { held: await this.decodeHeld(res) };
      return { value: await this.decodeJson<T>(res) };
    } finally {
      cleanup();
    }
  }

  /** Like noContent for a call a hitl gate may hold: it resolves to the held
   * intervention of a 202, or undefined when the action itself answered. */
  async gatedNoContent(
    method: string,
    path: string,
    spec: RequestSpec = {},
  ): Promise<Intervention | undefined> {
    const { signal: combined, cleanup } = this.withTimeout(spec.signal);
    try {
      const res = await this.send(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      if (res.status === 202) return await this.decodeHeld(res);
      await res.body?.cancel().catch(() => {});
      return undefined;
    } finally {
      cleanup();
    }
  }

  /** Perform a request and return the response body as text. */
  async text(method: string, path: string, spec: RequestSpec = {}): Promise<string> {
    const { signal: combined, cleanup } = this.withTimeout(spec.signal);
    try {
      const res = await this.execute(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      return await res.text();
    } finally {
      cleanup();
    }
  }

  /**
   * send performs a call and follows its 202 operation. A keyed call (one
   * with an idempotency key) answered 503 is sent again under the same key,
   * up to the client's retries, waiting at least the server's Retry-After
   * plus jitter between sends.
   */
  private async send(
    method: string,
    path: string,
    spec: RequestSpec,
    signal: AbortSignal | undefined,
  ): Promise<Response> {
    for (let attempt = 0; ; attempt++) {
      const res = await this.follow(await this.execute(method, path, spec, signal), spec);
      if (!spec.idempotencyKey || res.status !== 503 || attempt >= this.retries)
        return res;
      const delay = retryDelayMs(res.headers.get("Retry-After"), attempt);
      await res.body?.cancel().catch(() => {});
      await sleep(delay, signal);
    }
  }

  /**
   * follow turns a mutating call's 202 operation into the answer the call
   * would have given directly: it polls the operation until it finishes and
   * rebuilds a response from its status_code and result or error. A 202
   * holding a hitl hold comes back unchanged, as does any other answer.
   * Only the caller's signal bounds the polls; the operation keeps running
   * if it aborts first.
   */
  private async follow(res: Response, spec: RequestSpec): Promise<Response> {
    if (!spec.idempotencyKey || res.status !== 202) return res;
    const text = await res.text();
    let accepted: OperationAccepted | undefined;
    try {
      accepted = JSON.parse(text) as OperationAccepted;
    } catch {
      // not json: leave it to the caller's decode
    }
    // the rebuilt body differs in length from the one received.
    const headers = new Headers(res.headers);
    headers.delete("content-length");
    if (!accepted?.operation_id) return new Response(text, { status: 202, headers });
    this.onOperation?.(accepted);
    const op = await this.waitOperation(accepted.operation_id, spec.signal);
    const status = op.status_code || 500;
    let body: string | null = null;
    if (status >= 200 && status < 300) {
      if (op.result !== undefined) body = JSON.stringify(op.result);
    } else if (op.error) {
      body = JSON.stringify({ error: op.error });
    }
    // a 204 cannot carry a body.
    if (status === 204) body = null;
    return new Response(body, { status, headers });
  }

  /** Fetch one operation, attaching for up to wait (e.g. "30s") first. */
  getOperation(
    id: string,
    wait: string | undefined,
    signal?: AbortSignal,
  ): Promise<Operation> {
    return this.json<Operation>("GET", `/v1/operations/${encodeURIComponent(id)}`, {
      query: { wait },
      signal,
      noTimeout: true,
    });
  }

  /** Poll an operation until it finishes. */
  async waitOperation(id: string, signal?: AbortSignal): Promise<Operation> {
    for (;;) {
      const op = await this.getOperation(id, OPERATION_POLL_WAIT, signal);
      if (op.state !== OperationState.Running) return op;
      signal?.throwIfAborted();
    }
  }

  private async decodeHeld(res: Response): Promise<Intervention> {
    const body = await this.decodeJson<{ intervention?: Intervention }>(res);
    if (!body?.intervention?.id) throw new FuseError("decode held response");
    return body.intervention;
  }

  /** Perform a request that returns no body (e.g. 204 actions). */
  async noContent(method: string, path: string, spec: RequestSpec = {}): Promise<void> {
    const { signal: combined, cleanup } = this.withTimeout(spec.signal);
    try {
      const res = await this.send(method, path, spec, combined);
      if (!res.ok) throw await errorFromResponse(res);
      // Release the connection even if the server sent a body.
      await res.body?.cancel().catch(() => {});
    } finally {
      cleanup();
    }
  }

  /**
   * Open a long-lived stream (SSE). Applies NO timeout — only the caller's
   * signal cancels it — and checks the status eagerly so connect-time errors
   * (404/401/...) surface as a FuseApiError before any iteration.
   */
  async stream(
    path: string,
    query: Query | undefined,
    signal?: AbortSignal,
  ): Promise<Response> {
    const res = await this.execute(
      "GET",
      path,
      { query, signal },
      signal,
      "text/event-stream",
    );
    if (!res.ok) throw await errorFromResponse(res);
    return res;
  }

  private async execute(
    method: string,
    path: string,
    spec: RequestSpec,
    signal: AbortSignal | undefined,
    accept?: string,
  ): Promise<Response> {
    const url = this.buildUrl(path, spec.query);
    const hasBody = spec.body !== undefined && spec.body !== null;
    const headers = this.buildHeaders(hasBody, accept);
    if (spec.idempotencyKey) headers[IDEMPOTENCY_KEY_HEADER] = spec.idempotencyKey;
    const init: RequestInit = { method, headers };
    if (hasBody) init.body = JSON.stringify(spec.body);
    if (signal) init.signal = signal;

    try {
      return await this.fetchImpl(url, init);
    } catch (err) {
      // A caller- or timeout-initiated abort propagates as-is; everything else
      // is wrapped so consumers get a consistent FuseError with a cause.
      if (signal?.aborted) throw err;
      throw new FuseError(`request failed: ${method} ${url}`, { cause: err });
    }
  }

  private buildUrl(path: string, query?: Query): string {
    const p = path.startsWith("/") ? path : "/" + path;
    const u = new URL(p, this.baseUrl);
    if (query) {
      for (const [key, value] of Object.entries(query)) {
        if (value !== undefined && value !== "") u.searchParams.set(key, value);
      }
    }
    return u.toString();
  }

  private buildHeaders(hasBody: boolean, accept?: string): Record<string, string> {
    const headers: Record<string, string> = { ...(this.defaultHeaders ?? {}) };
    if (hasBody) headers["Content-Type"] = "application/json";
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    if (this.userAgent) headers["User-Agent"] = this.userAgent;
    if (this.requestId) {
      const id = this.requestId();
      if (id) headers[REQUEST_ID_HEADER] = id;
    }
    if (accept) headers["Accept"] = accept;
    if (this.actor?.id) {
      headers[ACTOR_HEADER] = this.actor.id;
      if (this.actor.name) headers[ACTOR_NAME_HEADER] = this.actor.name;
    }
    return headers;
  }

  private async decodeJson<T>(res: Response): Promise<T> {
    const text = await res.text();
    if (!text) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch (err) {
      throw new FuseError("decode response body", { cause: err });
    }
  }

  private withTimeout(callerSignal?: AbortSignal): {
    signal?: AbortSignal;
    cleanup: () => void;
  } {
    const signals: Array<AbortSignal | undefined> = [callerSignal];
    if (this.timeoutMs && this.timeoutMs > 0) {
      signals.push(AbortSignal.timeout(this.timeoutMs));
    }
    return combineSignals(signals);
  }
}
