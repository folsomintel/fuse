import { afterEach, describe, expect, it } from "vitest";
import type { ServerResponse } from "node:http";
import { pathOf, readBody, serve, type TestServer } from "./server.js";
import { isConflict } from "../src/index.js";

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

describe("egress allow", () => {
  it("setEgressAllow puts the list and decodes the egress status", async () => {
    const seen: { method?: string; path: string; key?: string; body: unknown }[] = [];
    current = await serve(async (req, res) => {
      const body = JSON.parse(await readBody(req));
      seen.push({
        method: req.method,
        path: pathOf(req),
        key: req.headers["idempotency-key"] as string | undefined,
        body,
      });
      json(
        res,
        200,
        body.allow.length
          ? { mode: "direct", allow: ["10.0.0.0/8", "1.2.3.4/32"] }
          : { mode: "direct" },
      );
    });

    const st = await current.client.environments.setEgressAllow("vm-1", [
      "10.0.0.0/8",
      "1.2.3.4",
    ]);
    expect(st).toEqual({ mode: "direct", allow: ["10.0.0.0/8", "1.2.3.4/32"] });

    // an empty list lifts the restriction and must still send the key.
    const lifted = await current.client.environments.setEgressAllow("vm-1", []);
    expect(lifted.allow).toBeUndefined();

    expect(seen.map((s) => s.method)).toEqual(["PUT", "PUT"]);
    expect(seen[0]!.path).toBe("/v1/environments/vm-1/egress/allow");
    expect(seen[0]!.key).toBeTruthy();
    expect(seen.map((s) => s.body)).toEqual([
      { allow: ["10.0.0.0/8", "1.2.3.4"] },
      { allow: [] },
    ]);
  });

  it("setEgressAllow follows a 202 operation to the egress status", async () => {
    current = await serve(async (req, res) => {
      if (pathOf(req).startsWith("/v1/operations/")) {
        json(res, 200, {
          id: "op_1",
          kind: "egress.allow",
          state: "succeeded",
          started_at: "2026-01-01T00:00:00Z",
          status_code: 200,
          result: { mode: "direct", allow: ["10.0.0.0/8"] },
        });
        return;
      }
      json(res, 202, {
        operation_id: "op_1",
        operation: {
          id: "op_1",
          kind: "egress.allow",
          state: "running",
          started_at: "2026-01-01T00:00:00Z",
        },
      });
    });

    const st = await current.client.environments.setEgressAllow("vm-1", ["10.0.0.0/8"]);
    expect(st.allow).toEqual(["10.0.0.0/8"]);
  });

  it("a proxy environment's 409 throws a conflict", async () => {
    current = await serve((_req, res) => {
      json(res, 409, {
        error: { code: "conflict", message: "environment uses proxy egress" },
      });
    });

    const err = await current.client.environments
      .setEgressAllow("vm-1", ["10.0.0.0/8"])
      .catch((e) => e);
    expect(isConflict(err)).toBe(true);
  });

  it("create sends egress.allow", async () => {
    let body: unknown;
    current = await serve(async (req, res) => {
      body = JSON.parse(await readBody(req));
      json(res, 201, { id: "vm-1", state: "running" });
    });

    await current.client.environments.create({
      task_id: "t",
      egress: { mode: "direct", allow: ["10.0.0.0/8"] },
    });
    expect(body).toEqual({
      task_id: "t",
      egress: { mode: "direct", allow: ["10.0.0.0/8"] },
    });
  });
});
