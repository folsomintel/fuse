import {
  combineSignals,
  idempotencyKey,
  type CallOptions,
  type MutatingCallOptions,
  type Transport,
} from "./transport.js";
import { BuildState, type Build, type CreateBuildRequest } from "./types.js";
import { requireArg } from "./validate.js";

/** How long one build poll asks the server to hold, in seconds. */
const BUILD_POLL_WAIT_SECONDS = 30;

/** Options for builds.get. */
export interface GetBuildOptions {
  /** Attach until the build is no longer running, for at most this long, as
   * a duration such as "30s" (capped by the server). Unset answers at once. */
  wait?: string;
}

/** Options for builds.wait. */
export interface WaitBuildOptions {
  /** Stop waiting after this many milliseconds. The build keeps running. */
  timeout?: number;
}

/** BuildsService runs builds that bake a setup into a snapshot. */
export class BuildsService {
  constructor(private readonly t: Transport) {}

  /** Start a build. It answers at once with the running build. */
  async create(body: CreateBuildRequest, opts: MutatingCallOptions = {}): Promise<Build> {
    return this.t.json<Build>("POST", "/v1/builds", {
      body,
      signal: opts.signal,
      idempotencyKey: idempotencyKey(opts),
    });
  }

  /** Fetch a build, finished or still running. */
  async get(
    id: string,
    options: GetBuildOptions = {},
    opts: CallOptions = {},
  ): Promise<Build> {
    requireArg(id, "build id");
    return this.t.json<Build>("GET", `/v1/builds/${encodeURIComponent(id)}`, {
      query: { wait: options.wait },
      signal: opts.signal,
      noTimeout: true,
    });
  }

  /**
   * Block until the build is no longer running and return it. A failed build
   * is returned, not thrown: its error and output say why. `timeout` and
   * `opts.signal` bound the wait; the build keeps running if either ends it
   * first.
   */
  async wait(
    id: string,
    options: WaitBuildOptions = {},
    opts: CallOptions = {},
  ): Promise<Build> {
    requireArg(id, "build id");
    const timeout = options.timeout && options.timeout > 0 ? options.timeout : 0;
    const deadline = timeout ? Date.now() + timeout : 0;
    const { signal, cleanup } = combineSignals([
      opts.signal,
      timeout ? AbortSignal.timeout(timeout) : undefined,
    ]);
    try {
      for (;;) {
        let seconds = BUILD_POLL_WAIT_SECONDS;
        // never ask the server to hold past the remaining timeout.
        if (deadline) {
          seconds = Math.min(
            seconds,
            Math.max(1, Math.ceil((deadline - Date.now()) / 1000)),
          );
        }
        const build = await this.get(id, { wait: `${seconds}s` }, { signal });
        if (build.state !== BuildState.Running) return build;
        signal?.throwIfAborted();
      }
    } finally {
      cleanup();
    }
  }
}
