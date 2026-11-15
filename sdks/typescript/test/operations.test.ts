import { afterEach, describe, expect, it } from "vitest";
import type { IncomingMessage, ServerResponse } from "node:http";
import { isNotFound, type OperationAccepted } from "../src/index.js";
import { pathOf, queryOf, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

const ENV = { id: "vm-1", state: "running" };

function json(res: ServerResponse, status: number, body: unknown) {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(body));
}

function running(id: string, kind = "create") {
  return { id, kind, state: "running", started_at: "2026-01-01T00:00:00Z" };
}

/** accepting answers the mutating call with a 202 operation, then serves
 * polls from finals in order. */
function accepting(kind: string, finals: unknown[], polls: string[]) {
  return (req: IncomingMessage, res: ServerResponse) => {
    if (pathOf(req).startsWith("/v1/operations/")) {
      polls.push(`${pathOf(req)}?${queryOf(req)}`);
      json(res, 200, finals.shift());
      return;
    }
    res.setHeader("Location", "/v1/operations/op_1");
    json(res, 202, { operation_id: "op_1", operation: running("op_1", kind) });
  };
}

describe("idempotency keys", () => {
  it("create sends a generated key when none is given", async () => {
    let key: string | undefined;
    current = await serve((req, res) => {
      key = req.headers["idempotency-key"] as string | undefined;
      json(res, 201, ENV);
    });

    await current.client.environments.create({ task_id: "t" });

    expect(key).toBeTruthy();
  });

  it("create sends the caller's key", async () => {
    let key: string | undefined;
    current = await serve((req, res) => {
      key = req.headers["idempotency-key"] as string | undefined;
      json(res, 201, ENV);
    });

    await current.client.environments.create(
      { task_id: "t" },
      { idempotencyKey: "my-key" },
    );

    expect(key).toBe("my-key");
  });

  it("each call gets a fresh generated key", async () => {
    const keys: string[] = [];
    current = await serve((req, res) => {
      keys.push(req.headers["idempotency-key"] as string);
      res.statusCode = 204;
      res.end();
    });

    await current.client.environments.destroy("vm-1");
    await current.client.snapshots.restore("snap-1");

    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });

  it("non-mutating calls send no key", async () => {
    let key: string | undefined = "unset";
    current = await serve((req, res) => {
      key = req.headers["idempotency-key"] as string | undefined;
      json(res, 200, ENV);
    });

    await current.client.environments.get("vm-1");

    expect(key).toBeUndefined();
  });
});

describe("following operations", () => {
  it("create follows a 202 operation to its environment", async () => {
    const polls: string[] = [];
    const accepted: OperationAccepted[] = [];
    current = await serve(
      accepting(
        "create",
        [
          running("op_1"),
          {
            ...running("op_1"),
            state: "succeeded",
            status_code: 201,
            finished_at: "2026-01-01T00:01:00Z",
            result: ENV,
          },
        ],
        polls,
      ),
      { onOperation: (a) => accepted.push(a) },
    );

    const env = await current.client.environments.create({ task_id: "t" });

    expect(env.id).toBe("vm-1");
    expect(polls).toEqual([
      "/v1/operations/op_1?wait=30s",
      "/v1/operations/op_1?wait=30s",
    ]);
    expect(accepted.map((a) => a.operation_id)).toEqual(["op_1"]);
  });

  it("a followed operation that failed 404 throws not found", async () => {
    current = await serve(
      accepting(
        "fork",
        [
          {
            ...running("op_1", "fork"),
            state: "failed",
            status_code: 404,
            error: { code: "not_found", message: "environment not found" },
          },
        ],
        [],
      ),
    );

    const err = await current.client.environments.fork("vm-1").catch((e) => e);

    expect(isNotFound(err)).toBe(true);
    expect(err.status).toBe(404);
  });

  it("destroy follows a 204 operation to void", async () => {
    current = await serve(
      accepting(
        "destroy",
        [{ ...running("op_1", "destroy"), state: "succeeded", status_code: 204 }],
        [],
      ),
    );

    await expect(current.client.environments.destroy("vm-1")).resolves.toBeUndefined();
  });

  it("a followed operation a gate held goes down the held path", async () => {
    const held = {
      id: "int-1",
      env_id: "vm-1",
      kind: "gate",
      mode: "ask",
      state: "pending",
    };
    current = await serve((req, res) => {
      const path = pathOf(req);
      if (path === "/v1/operations/op_1") {
        json(res, 200, {
          ...running("op_1", "drain"),
          state: "succeeded",
          status_code: 202,
          result: { intervention_id: "int-1", intervention: held },
        });
        return;
      }
      if (path.startsWith("/v1/interventions/")) {
        json(res, 200, {
          ...held,
          state: "approved",
          decision: { outcome: "approve" },
          result: { env_id: "vm-1" },
        });
        return;
      }
      if (req.method === "GET") {
        json(res, 200, { ...ENV, state: "draining" });
        return;
      }
      json(res, 202, { operation_id: "op_1", operation: running("op_1", "drain") });
    });

    const env = await current.client.environments.drain("vm-1");

    expect(env.state).toBe("draining");
  });
});

describe("operations service", () => {
  it("get sends wait and decodes the operation", async () => {
    let seen: string | undefined;
    current = await serve((req, res) => {
      seen = `${pathOf(req)}?${queryOf(req)}`;
      json(res, 200, { ...running("op_1"), step: "boot" });
    });

    const op = await current.client.operations.get("op_1", { wait: "5s" });

    expect(seen).toBe("/v1/operations/op_1?wait=5s");
    expect(op.state).toBe("running");
    expect(op.step).toBe("boot");
  });

  it("get of an unknown operation throws not found", async () => {
    current = await serve((_req, res) => {
      json(res, 404, { error: { code: "not_found", message: "operation not found" } });
    });

    const err = await current.client.operations.get("op_x").catch((e) => e);

    expect(isNotFound(err)).toBe(true);
  });

  it("wait polls until the operation finishes and returns a failure", async () => {
    const finals = [
      running("op_1"),
      {
        ...running("op_1"),
        state: "failed",
        status_code: 409,
        error: { code: "conflict", message: "busy" },
      },
    ];
    let polls = 0;
    current = await serve((_req, res) => {
      polls++;
      json(res, 200, finals.shift());
    });

    const op = await current.client.operations.wait("op_1");

    expect(polls).toBe(2);
    expect(op.state).toBe("failed");
    expect(op.error?.code).toBe("conflict");
  });

  it("wait stops at its timeout", async () => {
    current = await serve((_req, res) => {
      setTimeout(() => json(res, 200, running("op_1")), 200);
    });

    await expect(
      current.client.operations.wait("op_1", { timeout: 50 }),
    ).rejects.toThrow();
  });
});
