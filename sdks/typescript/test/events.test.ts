import { afterEach, describe, expect, it } from "vitest";
import { serve, type TestServer } from "./server.js";
import { type Event, isNotFound } from "../src/index.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

async function collect(stream: AsyncIterable<Event>): Promise<Event[]> {
  const out: Event[] = [];
  for await (const ev of stream) out.push(ev);
  return out;
}

describe("events", () => {
  it("yields events, ignores keepalives, and ends after a terminal state", async () => {
    current = await serve((req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(
        `id: 1\ndata: {"event":"state","vm_id":"v","state":"running","updated_at":"t"}\n\n`,
      );
      res.write(`: keepalive\n\n`);
      res.write(
        `data: {"event":"state","vm_id":"v","state":"destroyed","updated_at":"t"}\n\n`,
      );
      res.end();
    });

    const events = await collect(await current.client.environments.events("v"));

    expect(events.map((e) => e.state)).toEqual(["running", "destroyed"]);
  });

  it("reassembles events split across chunk boundaries", async () => {
    current = await serve((req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(`data: {"event":"state","vm_id":"v","st`);
      setTimeout(() => {
        res.write(`ate":"running","updated_at":"t"}\n\n`);
        res.write(
          `data: {"event":"state","vm_id":"v","state":"destroyed","updated_at":"t"}\n\n`,
        );
        res.end();
      }, 15);
    });

    const events = await collect(await current.client.environments.events("v"));

    expect(events.map((e) => e.state)).toEqual(["running", "destroyed"]);
  });

  it("rejects with a FuseApiError on a connect-time failure", async () => {
    current = await serve((req, res) => {
      res.statusCode = 404;
      res.setHeader("Content-Type", "application/json");
      res.end(`{"error":{"code":"not_found","message":"nope"}}`);
    });

    let caught: unknown;
    try {
      await current.client.environments.events("missing");
    } catch (err) {
      caught = err;
    }

    expect(isNotFound(caught)).toBe(true);
  });

  it("stops cleanly when the caller aborts", async () => {
    current = await serve((req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(
        `data: {"event":"state","vm_id":"v","state":"running","updated_at":"t"}\n\n`,
      );
      // intentionally leave the stream open
    });

    const ac = new AbortController();
    const stream = await current.client.environments.events("v", { signal: ac.signal });
    const seen: string[] = [];
    for await (const ev of stream) {
      seen.push(ev.state);
      ac.abort();
    }

    expect(seen).toEqual(["running"]);
  });
});
