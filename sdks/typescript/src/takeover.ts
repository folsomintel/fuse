// hitl takeover lock: waiting out a 423 on exec and computer calls, mirroring
// sdks/go. while a human holds the environment (or an ask-mode stall is
// pending) those calls are refused; by default the sdk waits for the
// intervention to settle and retries, so agent code needs no changes.

import type { CallOptions, Transport } from "./transport.js";
import type { Intervention } from "./types.js";
import { isLocked } from "./errors.js";
import { waitIntervention } from "./interventions.js";

/** Per-call options for calls a takeover can lock or a gate can hold
 * (computer actions). */
export interface LockedCallOptions extends CallOptions {
  /** Wait out a takeover or a pending stall, and a gate's hold (the
   * default). false throws a LockedError or a PendingInterventionError at
   * once instead. */
  wait?: boolean;
  /** Called with the pending intervention when a gate holds the action,
   * before waiting on it. It overrides the client's onHeld. */
  onHeld?: (held: Intervention) => void;
}

/** Body for environments.takeover. */
export interface TakeoverOptions {
  /** How to take over: TakeoverVia.Terminal (the default) or
   * TakeoverVia.Desktop. */
  via?: string;
  /** Recorded when no agent request is pending to claim. */
  reason?: string;
}

/** Body for environments.release. */
export interface ReleaseOptions {
  /** Handed back to the agent as the takeover's answer. */
  note?: string;
}

/**
 * retryLocked runs call, and on a 423 waits until the named intervention is
 * no longer pending, then runs it again. a fresh 423 waits again. the server
 * bounds every lock by the intervention deadline; opts.signal stops early.
 */
export async function retryLocked<T>(
  t: Transport,
  call: () => Promise<T>,
  wait: boolean | undefined,
  opts: CallOptions,
): Promise<T> {
  for (;;) {
    try {
      return await call();
    } catch (err) {
      if (wait === false || !isLocked(err) || !err.interventionId) throw err;
      await waitIntervention(t, err.interventionId, opts);
      opts.signal?.throwIfAborted();
    }
  }
}
