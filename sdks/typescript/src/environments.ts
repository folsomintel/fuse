import type { CallOptions, Transport } from "./transport.js";
import type {
  ComputerAction,
  ComputerDisplay,
  ComputerResult,
  ComputerToolResult,
  CreateRequest,
  EnvironmentInfo,
  Event,
  ExecRequest,
  ExecResult,
  ForkOptions,
  Intervention,
  MigrateOptions,
  ToolResultBlock,
} from "./types.js";
import { HitlKind, isInterventionApproved } from "./types.js";
import {
  FuseError,
  InterventionRejectedError,
  isFuseApiError,
  isHitlDenied,
} from "./errors.js";
import { requireArg } from "./validate.js";
import { streamEvents } from "./events.js";
import { waitIntervention } from "./interventions.js";
import { settleHeld } from "./gates.js";
import {
  retryLocked,
  type LockedCallOptions,
  type ReleaseOptions,
  type TakeoverOptions,
} from "./takeover.js";

/** The server's max page size; list() requests this internally so it walks
 * every page in as few round trips as possible. */
const MAX_PAGE_LIMIT = 200;

/** Filters (and pagination) for environments.list / environments.listPage. */
export interface ListEnvironmentsOptions {
  taskId?: string;
  state?: string;
  hostId?: string;
  /** Page size (server default 50, max 200). Only consulted by listPage —
   * list() always walks every page, so it ignores this and requests the
   * server's max page size. */
  limit?: number;
  /** Opaque cursor from a previous listPage() call's nextCursor. */
  cursor?: string;
}

interface EnvironmentList {
  environments: EnvironmentInfo[];
  next_cursor?: string;
}

/** One page of environments.listPage, plus the cursor to fetch the next one. */
export interface EnvironmentPage {
  environments: EnvironmentInfo[];
  /** Empty/undefined once there are no more results. */
  nextCursor?: string;
}

/** What environments.requestHuman sends. */
export interface HumanRequest {
  /** The request kind: "ask" (the default), "secret", "takeover" or
   * "review". */
  kind?: string;
  /** What the human is asked (ask). */
  question?: string;
  /** Restricts the answer to one of these (ask). */
  choices?: string[];
  /** A declared secret target: its env name or file path (secret). */
  target?: string;
  /** The agent's text shown under the target's label (secret), or with the
   * content up for review (review). */
  message?: string;
  /** How the human takes over: "terminal" or "desktop" (takeover). */
  via?: string;
  /** Why the agent wants a human to take over (takeover). */
  reason?: string;
  /** An absolute guest file to review; exclusive with content (review). */
  path?: string;
  /** Inline text or a diff to review (review). */
  content?: string;
  /** ReviewFormat.Text or ReviewFormat.Diff; unset guesses (review). */
  format?: string;
  /** Shortens the policy's deadline. It can never extend it. */
  timeoutSeconds?: number;
  /** Wait for a decision (the default). false returns the pending
   * intervention at once; wait on it later with interventions.wait. */
  wait?: boolean;
}

/** What environments.requestReview sends: exactly one of path and content. */
export interface ReviewRequest {
  /** An absolute guest file; fuse reads it at request time. */
  path?: string;
  /** Inline text or a diff. */
  content?: string;
  /** ReviewFormat.Text or ReviewFormat.Diff; unset guesses. */
  format?: string;
  message?: string;
  timeoutSeconds?: number;
  /** Wait for the verdict (the default). false returns at once with the
   * pending intervention and approved false. */
  wait?: boolean;
}

/** ReviewResult is a finished review. comments are the reviewer's, on
 * either outcome. */
export interface ReviewResult {
  approved: boolean;
  comments: string;
  intervention: Intervention;
}

/** EnvironmentsService manages microVM lifecycle. */
export class EnvironmentsService {
  constructor(private readonly t: Transport) {}

  /** List tracked environments, optionally filtered. Transparently walks
   * every result page. For explicit single-page control (e.g. a cursor from
   * a previous call), use listPage. */
  async list(
    options: ListEnvironmentsOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentInfo[]> {
    const out: EnvironmentInfo[] = [];
    let cursor = options.cursor;
    for (;;) {
      const page = await this.listPage(
        { ...options, limit: MAX_PAGE_LIMIT, cursor },
        opts,
      );
      out.push(...page.environments);
      if (!page.nextCursor) break;
      cursor = page.nextCursor;
    }
    return out;
  }

  /** List one page of tracked environments, optionally filtered. */
  async listPage(
    options: ListEnvironmentsOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentPage> {
    const out = await this.t.json<EnvironmentList>("GET", "/v1/environments", {
      query: {
        task_id: options.taskId,
        state: options.state,
        host_id: options.hostId,
        limit: options.limit ? String(options.limit) : undefined,
        cursor: options.cursor,
      },
      signal: opts.signal,
    });
    return { environments: out.environments ?? [], nextCursor: out.next_cursor };
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

  /** Fork an environment into a new one; returns the new environment. When a
   * hitl gate holds the fork, it waits for the decision and returns the
   * environment the orchestrator then forked. */
  async fork(
    vmId: string,
    body: ForkOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    const out = await this.t.gated<EnvironmentInfo>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "fork" }, body, signal: opts.signal },
    );
    if (!out.held) return out.value;
    return this.heldEnvironment(out.held, opts);
  }

  /**
   * Migrate an environment to another host; returns the new environment. The
   * source is drained and destroyed once the new one is running.
   */
  async migrate(
    vmId: string,
    body: MigrateOptions = {},
    opts: CallOptions = {},
  ): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    const out = await this.t.gated<EnvironmentInfo>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "migrate" }, body, signal: opts.signal },
    );
    if (!out.held) return out.value;
    return this.heldEnvironment(out.held, opts);
  }

  /** heldEnvironment waits out a held fork or migrate and returns the
   * environment it produced. */
  private async heldEnvironment(
    held: Intervention,
    opts: CallOptions,
  ): Promise<EnvironmentInfo> {
    const i = await settleHeld(this.t, held, {}, opts);
    if (!i.result?.env_id) {
      throw new FuseError(`intervention ${i.id} settled without an environment`);
    }
    return this.get(i.result.env_id, opts);
  }

  /**
   * Run a command inside a running environment's guest and return its exit code
   * with stdout and stderr kept separate.
   *
   * A non-zero exit_code is resolved, not thrown: the command ran and failed.
   * A thrown error means the command could not be run at all.
   *
   * Exec requires the master token.
   *
   * When a hitl gate holds the exec, exec waits for the decision (bounded
   * only by `opts.signal`); the orchestrator runs the approved (possibly
   * edited) argv and exec returns its result. A rejection throws an
   * InterventionRejectedError, a deny gate an error isHitlDenied matches.
   * `wait: false` throws a PendingInterventionError at once instead.
   *
   * While a human holds a takeover (or an ask-mode stall is pending) the
   * server answers 423; exec waits for that intervention to settle and runs
   * the command then. `wait: false` throws a LockedError at once instead.
   *
   * @example
   * const out = await client.environments.exec(id, { cmd: ["ls", "-l"] });
   * if (out.exit_code !== 0) console.error(out.stderr);
   */
  async exec(
    vmId: string,
    body: ExecRequest,
    opts: CallOptions = {},
  ): Promise<ExecResult> {
    requireArg(vmId, "vm id");
    const hasCmd = (body.cmd?.length ?? 0) > 0;
    const hasShell = (body.shell ?? "") !== "";
    if (!hasCmd && !hasShell) {
      throw new FuseError("one of cmd or shell is required");
    }
    if (hasCmd && hasShell) {
      throw new FuseError("cmd and shell are mutually exclusive");
    }
    const { wait, onHeld, ...wire } = body;
    // a 423 (a human holds the environment) waits and retries; a 202 (a gate
    // holds this exec) waits for the decision and its run.
    const out = await retryLocked(
      this.t,
      () =>
        this.t.gated<ExecResult>("POST", `/v1/environments/${encodeURIComponent(vmId)}`, {
          query: { action: "exec" },
          body: wire,
          signal: opts.signal,
        }),
      wait,
      opts,
    );
    if (!out.held) return out.value;
    const i = await settleHeld(this.t, out.held, { wait, onHeld }, opts);
    if (!i.result?.exec) {
      throw new FuseError(`intervention ${i.id} settled without an exec result`);
    }
    return i.result.exec;
  }

  /**
   * Relay one computer-use action to the environment's desktop and return the
   * result, usually carrying a base64 PNG screenshot. Requires an environment
   * booted from a desktop image; on any other image the server answers 503
   * with a reason.
   *
   * Like exec, it waits out a takeover or a pending stall (a 423) and then
   * acts; `opts.wait: false` throws a LockedError at once instead. When a
   * computer gate holds the action (a 202), it waits for the decision and
   * returns what the orchestrator got performing it (with the human's edited
   * text, if any); a rejection throws an InterventionRejectedError, and
   * `opts.wait: false` throws a PendingInterventionError at once instead.
   *
   * @example
   * const res = await client.environments.computer(id, {
   *   action: "left_click",
   *   coordinate: [512, 384],
   * });
   */
  async computer(
    vmId: string,
    action: ComputerAction,
    opts: LockedCallOptions = {},
  ): Promise<ComputerResult> {
    requireArg(vmId, "vm id");
    requireArg(action?.action, "action");
    return this.postComputer(vmId, action, opts);
  }

  /**
   * Execute one computer tool_use against the environment and shape the
   * answer for the Messages API: the tool_use block's `input` goes in
   * verbatim (it is already the wire shape the computer endpoint takes), and
   * content blocks for the tool_result come out.
   *
   * An action the environment refuses (a malformed coordinate, a display
   * that is not up) resolves to `is_error` content rather than throwing: the
   * agent loop should feed it to the model, which is the party that can
   * correct it. So does a hitl deny gate or a human rejecting a held action. A throw is reserved for failures the model cannot fix —
   * transport, auth, an unknown environment.
   *
   * @example
   * for (const block of msg.content) {
   *   if (block.type === "tool_use" && block.name === "computer") {
   *     const result = await client.environments.computerToolResult(id, block.input);
   *     toolResults.push({ type: "tool_result", tool_use_id: block.id, ...result });
   *   }
   * }
   */
  async computerToolResult(
    vmId: string,
    input: unknown,
    opts: LockedCallOptions = {},
  ): Promise<ComputerToolResult> {
    requireArg(vmId, "vm id");
    const action = (input as { action?: string } | null)?.action ?? "";
    if (action === "") {
      return {
        is_error: true,
        content: [{ type: "text", text: "tool input names no action" }],
      };
    }
    let res: ComputerResult;
    try {
      res = await this.postComputer(vmId, input, opts);
    } catch (err) {
      if (
        isFuseApiError(err) &&
        (err.status === 400 ||
          err.status === 409 ||
          err.status === 503 ||
          isHitlDenied(err))
      ) {
        return {
          is_error: true,
          content: [{ type: "text", text: err.message || "the computer action failed" }],
        };
      }
      if (err instanceof InterventionRejectedError) {
        return {
          is_error: true,
          content: [
            { type: "text", text: `a human rejected this action: ${err.message}` },
          ],
        };
      }
      throw err;
    }
    const content: ToolResultBlock[] = [];
    if (res.output) content.push({ type: "text", text: res.output });
    if (res.screenshot) {
      content.push({
        type: "image",
        source: {
          type: "base64",
          media_type: "image/png",
          data: res.screenshot,
        },
      });
    }
    if (content.length === 0) content.push({ type: "text", text: "done" });
    return { content };
  }

  private async postComputer(
    vmId: string,
    body: unknown,
    opts: LockedCallOptions,
  ): Promise<ComputerResult> {
    // a 423 (a human holds the environment) waits and retries; a 202 (a gate
    // holds this action) waits for the decision and its run.
    const out = await retryLocked(
      this.t,
      () =>
        this.t.gated<ComputerResult>(
          "POST",
          `/v1/environments/${encodeURIComponent(vmId)}/computer`,
          { body, signal: opts.signal },
        ),
      opts.wait,
      opts,
    );
    if (!out.held) return out.value;
    const i = await settleHeld(
      this.t,
      out.held,
      { wait: opts.wait, onHeld: opts.onHeld },
      opts,
    );
    if (!i.result?.computer) {
      throw new FuseError(`intervention ${i.id} settled without a computer result`);
    }
    return i.result.computer;
  }

  /**
   * Ask a human for something the environment's hitl policy declares. By
   * default it waits (by long-polling) until the intervention resolves
   * (answered, rejected, expired or cancelled); read the result off the
   * returned intervention's decision. Pass `wait: false` to get the pending
   * handle instead. Pass `opts.signal` to stop waiting early.
   *
   * @example
   * const res = await client.environments.requestHuman(id, {
   *   question: "deploy to prod?",
   *   choices: ["yes", "no"],
   * });
   * if (isInterventionApproved(res)) console.log(res.decision?.answer);
   */
  async requestHuman(
    vmId: string,
    req: HumanRequest,
    opts: CallOptions = {},
  ): Promise<Intervention> {
    requireArg(vmId, "vm id");
    const pending = await this.t.json<Intervention>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      {
        query: { action: "request" },
        body: {
          kind: req.kind || "ask",
          question: req.question || undefined,
          choices: req.choices,
          target: req.target || undefined,
          message: req.message || undefined,
          via: req.via || undefined,
          reason: req.reason || undefined,
          path: req.path || undefined,
          content: req.content || undefined,
          format: req.format || undefined,
          timeout_seconds: req.timeoutSeconds || undefined,
        },
        signal: opts.signal,
      },
    );
    if (req.wait === false) return pending;
    return waitIntervention(this.t, pending.id, opts);
  }

  /**
   * Submit a guest file or a text/diff blob for a human to approve or reject
   * with comments, and wait for the verdict. Needs hitl.requests.review.
   * With `wait: false` it returns at once with the pending intervention and
   * approved false.
   *
   * @example
   * const r = await client.environments.requestReview(id, {
   *   content: diff,
   *   format: ReviewFormat.Diff,
   * });
   * if (!r.approved) console.log(r.comments);
   */
  async requestReview(
    vmId: string,
    req: ReviewRequest,
    opts: CallOptions = {},
  ): Promise<ReviewResult> {
    const i = await this.requestHuman(
      vmId,
      {
        kind: HitlKind.Review,
        path: req.path,
        content: req.content,
        format: req.format,
        message: req.message,
        timeoutSeconds: req.timeoutSeconds,
        wait: req.wait,
      },
      opts,
    );
    return {
      approved: isInterventionApproved(i),
      comments: i.decision?.reason ?? "",
      intervention: i,
    };
  }

  /**
   * Take over the environment as a human: claims the agent's pending
   * takeover request if there is one, else opens a new one. While held, the
   * agent's exec and computer calls wait. Returns the held intervention.
   * Needs the hitl:takeover scope; a conflict when someone else holds it.
   */
  async takeover(
    vmId: string,
    body: TakeoverOptions = {},
    opts: CallOptions = {},
  ): Promise<Intervention> {
    requireArg(vmId, "vm id");
    return this.t.json<Intervention>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      {
        query: { action: "takeover" },
        body: { via: body.via || undefined, reason: body.reason || undefined },
        signal: opts.signal,
      },
    );
  }

  /**
   * End a takeover and hand the environment back to the agent. The note is
   * the takeover's answer. Returns the approved intervention; a conflict when
   * nothing is held.
   */
  async release(
    vmId: string,
    body: ReleaseOptions = {},
    opts: CallOptions = {},
  ): Promise<Intervention> {
    requireArg(vmId, "vm id");
    return this.t.json<Intervention>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      {
        query: { action: "release" },
        body: { note: body.note || undefined },
        signal: opts.signal,
      },
    );
  }

  /** Report whether the environment has a live display and at what geometry.
   * up is false with a reason on an image with no desktop. */
  async computerDisplay(vmId: string, opts: CallOptions = {}): Promise<ComputerDisplay> {
    requireArg(vmId, "vm id");
    return this.t.json<ComputerDisplay>(
      "GET",
      `/v1/environments/${encodeURIComponent(vmId)}/computer`,
      { signal: opts.signal },
    );
  }

  /**
   * Phase-1 only: quiesces the guest workload and moves the environment to
   * "draining" so you can inspect its output, but does not destroy it. Call
   * destroy() afterward to remove it - a drained environment left
   * undestroyed keeps running (and billing) forever.
   */
  async drain(vmId: string, opts: CallOptions = {}): Promise<EnvironmentInfo> {
    requireArg(vmId, "vm id");
    const out = await this.t.gated<EnvironmentInfo>(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "drain" }, signal: opts.signal },
    );
    if (!out.held) return out.value;
    await settleHeld(this.t, out.held, {}, opts);
    return this.get(vmId, opts);
  }

  /** Rotate the per-VM credentials and push them to the guest. */
  async rotateToken(vmId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(vmId, "vm id");
    const held = await this.t.gatedNoContent(
      "POST",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { query: { action: "rotate-token" }, signal: opts.signal },
    );
    if (held) await settleHeld(this.t, held, {}, opts);
  }

  /** Forcefully destroy an environment. Idempotent. When a hitl gate holds
   * the destroy, it waits for the decision. */
  async destroy(vmId: string, opts: CallOptions = {}): Promise<void> {
    requireArg(vmId, "vm id");
    const held = await this.t.gatedNoContent(
      "DELETE",
      `/v1/environments/${encodeURIComponent(vmId)}`,
      { signal: opts.signal },
    );
    if (held) await settleHeld(this.t, held, {}, opts);
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
