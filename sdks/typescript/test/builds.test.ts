import { afterEach, describe, expect, it } from "vitest";
import type { IncomingMessage, ServerResponse } from "node:http";
import { isNotFound, type Build, type Event } from "../src/index.js";
import { pathOf, queryOf, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

function json(res: ServerResponse, status: number, body: unknown) {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(body));
}

function build(state: string, extra: Partial<Build> = {}): Build {
  return {
    id: "b_1",
    name: "app",
    state,
    cache_hits: 0,
    layers: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...extra,
  };
}

async function readBody(req: IncomingMessage): Promise<string> {
  let text = "";
  for await (const chunk of req) text += chunk;
  return text;
}

const REQ = {
  name: "app",
  arch: "x86_64",
  setup: { prelude: "set -e", steps: [{ script: "apt-get update", key: "k1" }] },
};

describe("builds", () => {
  it("create sends the body and a generated key", async () => {
    let key: string | undefined;
    let method: string | undefined;
    let path = "";
    let body: unknown;
    current = await serve(async (req, res) => {
      key = req.headers["idempotency-key"] as string | undefined;
      method = req.method;
      path = pathOf(req);
      body = JSON.parse(await readBody(req));
      res.setHeader("Location", "/v1/builds/b_1");
      json(res, 202, build("running", { step: "resolve_cache" }));
    });

    const b = await current.client.builds.create(REQ);

    expect(method).toBe("POST");
    expect(path).toBe("/v1/builds");
    expect(body).toEqual(REQ);
    expect(key).toBeTruthy();
    expect(b.id).toBe("b_1");
    expect(b.state).toBe("running");
    expect(b.step).toBe("resolve_cache");
  });

  it("create sends the caller's key", async () => {
    let key: string | undefined;
    current = await serve((req, res) => {
      key = req.headers["idempotency-key"] as string | undefined;
      json(res, 202, build("running"));
    });

    await current.client.builds.create(REQ, { idempotencyKey: "my-key" });

    expect(key).toBe("my-key");
  });

  it("get decodes a build with its layers", async () => {
    let seen = "";
    current = await serve((req, res) => {
      seen = `${pathOf(req)}?${queryOf(req)}`;
      json(
        res,
        200,
        build("succeeded", {
          cache_hits: 1,
          seed_snapshot_id: "s_0",
          snapshot_id: "s_2",
          finished_at: "2026-01-01T00:01:00Z",
          layers: [
            { index: 0, key: "k1", status: "hit", snapshot_id: "s_1" },
            { index: 1, status: "skip" },
          ],
        }),
      );
    });

    const b = await current.client.builds.get("b_1", { wait: "30s" });

    expect(seen).toBe("/v1/builds/b_1?wait=30s");
    expect(b.cache_hits).toBe(1);
    expect(b.snapshot_id).toBe("s_2");
    expect(b.layers).toHaveLength(2);
    expect(b.layers[0]).toEqual({
      index: 0,
      key: "k1",
      status: "hit",
      snapshot_id: "s_1",
    });
    expect(b.layers[1]?.status).toBe("skip");
  });

  it("get of an unknown build is not found", async () => {
    current = await serve((_req, res) => {
      json(res, 404, { error: { code: "not_found", message: "build not found" } });
    });

    const err = await current.client.builds.get("nope").catch((e) => e);
    expect(isNotFound(err)).toBe(true);
  });

  it("wait polls until the build finishes and returns a failed one", async () => {
    const polls: string[] = [];
    const finals = [
      build("running", { step: "layer_0" }),
      build("failed", { error: "step 1 exited 2", output: "boom" }),
    ];
    current = await serve((req, res) => {
      polls.push(queryOf(req));
      json(res, 200, finals.shift());
    });

    const b = await current.client.builds.wait("b_1");

    expect(polls).toEqual(["wait=30s", "wait=30s"]);
    expect(b.state).toBe("failed");
    expect(b.output).toBe("boom");
  });

  it("wait never asks the server to hold past its timeout", async () => {
    const polls: string[] = [];
    current = await serve((req, res) => {
      polls.push(queryOf(req));
      json(res, 200, build("succeeded"));
    });

    await current.client.builds.wait("b_1", { timeout: 5000 });

    expect(polls).toEqual(["wait=5s"]);
  });

  it("wait stops at its timeout", async () => {
    current = await serve((_req, res) => {
      setTimeout(() => json(res, 200, build("running")), 200);
    });

    await expect(current.client.builds.wait("b_1", { timeout: 50 })).rejects.toThrow();
  });

  it("an event decodes its build member", async () => {
    current = await serve((_req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(
        `data: {"event":"build","vm_id":"v","state":"running","updated_at":"t","build":{"id":"b_1","state":"running","step":"layer_0"}}\n\n`,
      );
      res.end();
    });

    const out: Event[] = [];
    for await (const ev of await current.client.environments.events("v")) out.push(ev);

    expect(out[0]?.build).toEqual({ id: "b_1", state: "running", step: "layer_0" });
  });
});
