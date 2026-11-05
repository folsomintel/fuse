// hitl gates: waiting out a call a gate held, mirroring sdks/go/gates.go.

import type { CallOptions, Transport } from "./transport.js";
import { isInterventionApproved, type Intervention } from "./types.js";
import {
  HeldActionError,
  InterventionRejectedError,
  PendingInterventionError,
} from "./errors.js";
import { waitIntervention } from "./interventions.js";

/** Per-call overrides of the client's hitl wait options. */
export interface HeldOptions {
  /** false throws a PendingInterventionError instead of waiting. */
  wait?: boolean;
  onHeld?: (held: Intervention) => void;
}

/**
 * settleHeld waits out a held call: it reports the hold, waits until the
 * intervention settles, and turns anything but a successful run into an
 * error. Only opts.signal bounds the wait; every intervention has a
 * deadline, so it always ends.
 */
export async function settleHeld(
  t: Transport,
  held: Intervention,
  o: HeldOptions,
  opts: CallOptions,
): Promise<Intervention> {
  const onHeld = o.onHeld ?? t.onHeld;
  onHeld?.(held);
  if (o.wait === false || t.hitlNoWait) throw new PendingInterventionError(held);
  const i = await waitIntervention(t, held.id, opts);
  if (!isInterventionApproved(i)) throw new InterventionRejectedError(i);
  if (i.result?.error) throw new HeldActionError(i);
  return i;
}
