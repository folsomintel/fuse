// SSE event streaming, porting the state machine in sdks/go/events.go to an
// async iterator. The stream is opened (and its status checked) eagerly; the
// returned AsyncIterable yields each Event and ends after a terminal-state
// event (destroyed/failed) or on clean EOF. Stream-level failures are thrown
// from the iterator. Caller abort is treated as normal termination.

import type { Transport } from "./transport.js";
import { FuseError } from "./errors.js";
import { type Event, isTerminalState } from "./types.js";

const DATA_PREFIX = "data:";

/** Open the SSE stream for an environment and return an async-iterable of events. */
export async function streamEvents(
  t: Transport,
  vmId: string,
  signal?: AbortSignal,
): Promise<AsyncIterable<Event>> {
  const path = `/v1/environments/${encodeURIComponent(vmId)}/events`;
  const res = await t.stream(path, undefined, signal);
  return parseEventStream(res, signal);
}

function parseEventStream(res: Response, signal?: AbortSignal): AsyncIterable<Event> {
  return {
    async *[Symbol.asyncIterator](): AsyncGenerator<Event> {
      const body = res.body;
      if (!body) return;

      const reader = body.getReader();
      const decoder = new TextDecoder("utf-8");
      let buffer = "";
      let data = "";
      let hasData = false;

      try {
        while (true) {
          let chunk;
          try {
            chunk = await reader.read();
          } catch (err) {
            // Caller-initiated abort is a clean end, not an error.
            if (signal?.aborted) return;
            throw new FuseError("event stream read failed", { cause: err });
          }
          if (chunk.done) {
            // Clean EOF with no terminal state: just end (matches Go).
            return;
          }

          buffer += decoder.decode(chunk.value, { stream: true });

          let nl: number;
          while ((nl = buffer.indexOf("\n")) >= 0) {
            let line = buffer.slice(0, nl);
            buffer = buffer.slice(nl + 1);
            if (line.endsWith("\r")) line = line.slice(0, -1); // tolerate CRLF

            if (line === "") {
              // Blank line terminates the current event.
              if (!hasData) continue;
              const payload = data;
              data = "";
              hasData = false;

              let event: Event;
              try {
                event = JSON.parse(payload) as Event;
              } catch (err) {
                throw new FuseError("decode event", { cause: err });
              }
              yield event;
              if (isTerminalState(event.state)) return;
            } else if (line.startsWith(":")) {
              // Comment / keepalive line — skip.
              continue;
            } else if (line.startsWith(DATA_PREFIX)) {
              // Strip the prefix and a single optional leading space; multiple
              // data: lines concatenate without a separator (matches server).
              let p = line.slice(DATA_PREFIX.length);
              if (p.startsWith(" ")) p = p.slice(1);
              data += p;
              hasData = true;
            }
            // Other fields (id:, event:, ...) are ignored.
          }
        }
      } finally {
        // Release the underlying socket if the consumer breaks early.
        try {
          await reader.cancel();
        } catch {
          // ignore
        }
      }
    },
  };
}
