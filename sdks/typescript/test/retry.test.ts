import { afterEach, describe, expect, it } from "vitest";
import type { ServerResponse } from "node:http";
import { isUnavailable } from "../src/index.js";
import { retryDelayMs } from "../src/retry.js";
import { readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

function unavailable(res: ServerResponse, retryAfter?: string) {
  res.statusCode = 503;
  if (retryAfter !== undefined) res.setHeader("Retry-After", retryAfter);
  res.setHeader("Content-Type", "application/json");
  res.end(
    JSON.stringify({
      error: { code: "unavailable", message: "no host has sufficient capacity" },
    }),
  );
}

describe("503 retry", () => {
  it("create is sent again under the same key until it is admitted", async () => {
    const keys: string[] = [];
    const bodies: string[] = [];
    current = await serve(async (req, res) => {
      keys.push(req.headers["idempotency-key"] as string);
      bodies.push(await readBody(req));
      if (keys.length < 3) {
        unavailable(res, "0");
        return;
      }
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ id: "vm-1", state: "running" }));
    });

    const env = await current.client.environments.create({ task_id: "t" });

    expect(env.id).toBe("vm-1");
    expect(keys).toHaveLength(3);
    expect(keys[0]).toBeTruthy();
    expect(new Set(keys).size).toBe(1);
    expect(new Set(bodies).size).toBe(1);
  });

  it("throws unavailable once the retries run out", async () => {
    let sends = 0;
    current = await serve((_req, res) => {
      sends++;
      unavailable(res, "0");
    });

    const err = await current.client.environments
      .create({ task_id: "t" })
      .catch((e) => e);

    expect(isUnavailable(err)).toBe(true);
    expect(sends).toBe(4);
  });

  it("retries: 0 throws the first 503", async () => {
    let sends = 0;
    current = await serve(
      (_req, res) => {
        sends++;
        unavailable(res, "0");
      },
      { retries: 0 },
    );

    const err = await current.client.environments
      .create({ task_id: "t" })
      .catch((e) => e);

    expect(isUnavailable(err)).toBe(true);
    expect(sends).toBe(1);
  });

  it("a call without an idempotency key is not retried", async () => {
    let sends = 0;
    current = await serve((_req, res) => {
      sends++;
      unavailable(res, "0");
    });

    const err = await current.client.environments.get("vm-1").catch((e) => e);

    expect(isUnavailable(err)).toBe(true);
    expect(sends).toBe(1);
  });
});

describe("503 retry of a polled operation", () => {
  it("waits the failed operation's retry_after before sending again", async () => {
    let posts = 0;
    let firstFailedAt = 0;
    let retriedAt = 0;
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST") {
        posts++;
        if (posts === 1) {
          res.statusCode = 202;
          res.end(
            JSON.stringify({
              operation_id: "op_1",
              operation: {
                id: "op_1",
                kind: "create",
                state: "running",
                started_at: "2026-11-15T00:00:00Z",
              },
            }),
          );
          return;
        }
        retriedAt = Date.now();
        res.statusCode = 201;
        res.end(JSON.stringify({ id: "vm-1", state: "running" }));
        return;
      }
      firstFailedAt = Date.now();
      res.end(
        JSON.stringify({
          id: "op_1",
          kind: "create",
          state: "failed",
          started_at: "2026-11-15T00:00:00Z",
          finished_at: "2026-11-15T00:01:00Z",
          status_code: 503,
          retry_after: 2,
          error: { code: "unavailable", message: "no host has sufficient capacity" },
        }),
      );
    });

    const env = await current.client.environments.create({ task_id: "t" });

    expect(env.id).toBe("vm-1");
    expect(posts).toBe(2);
    // without retry_after the first backoff is at most 1.5s.
    expect(retriedAt - firstFailedAt).toBeGreaterThanOrEqual(2000);
  }, 10_000);
});

describe("retryDelayMs", () => {
  it("waits at least Retry-After, plus at most half again", () => {
    expect(retryDelayMs("2", 0, () => 0)).toBe(2000);
    expect(retryDelayMs("2", 3, () => 0.999)).toBeLessThanOrEqual(3000);
    expect(retryDelayMs("2", 3, () => 0.999)).toBeGreaterThanOrEqual(2000);
  });

  it("backs off from 1s doubling without Retry-After", () => {
    expect(retryDelayMs(null, 0, () => 0)).toBe(1000);
    expect(retryDelayMs(null, 2, () => 0)).toBe(4000);
    expect(retryDelayMs(null, 20, () => 0)).toBe(30_000);
  });

  it("caps a server's Retry-After at a minute", () => {
    expect(retryDelayMs("3600", 0, () => 0)).toBe(60_000);
  });
});
