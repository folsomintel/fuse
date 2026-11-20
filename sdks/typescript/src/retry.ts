// Retry of a keyed mutating call the server answered 503 (no capacity, no
// host, restate down). Mirrors sdks/go/retry.go.

/** How many times a keyed call answered 503 is sent again by default. */
export const DEFAULT_RETRIES = 3;

/** Backoff of a 503 without Retry-After: 1s doubling, capped at 30s. */
const RETRY_BASE_MS = 1000;
const RETRY_CAP_MS = 30_000;
/** A server's Retry-After is capped at a minute. */
const MAX_RETRY_AFTER_MS = 60_000;

/**
 * retryDelayMs is how long to wait before retry number attempt (from 0) of a
 * 503 whose Retry-After header is retryAfter: the header's seconds when it
 * has them, else the backoff, plus up to half again as jitter. Jitter only
 * ever adds, so the wait never undercuts the server's Retry-After.
 */
export function retryDelayMs(
  retryAfter: string | null,
  attempt: number,
  random: () => number = Math.random,
): number {
  let d = Math.min(RETRY_BASE_MS * 2 ** Math.min(attempt, 10), RETRY_CAP_MS);
  if (retryAfter !== null && /^\d+$/.test(retryAfter.trim())) {
    d = Math.min(Number(retryAfter.trim()) * 1000, MAX_RETRY_AFTER_MS);
  }
  return d + Math.floor(random() * (d / 2 + 1));
}

/** sleep waits ms, or rejects with the signal's reason when it aborts. */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
