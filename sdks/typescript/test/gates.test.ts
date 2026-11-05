import { afterEach, describe, expect, it } from "vitest";
import {
  FuseClient,
  HeldActionError,
  InterventionRejectedError,
  PendingInterventionError,
  isHitlDenied,
  isInterventionSettled,
  type FetchLike,
  type Intervention,
} from "../src/index.js";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

const heldExecBody =
  `{"intervention_id":"int_1","intervention":{"id":"int_1","env_id":"vm-1","kind":"exec",` +
  `"gate":"force-push","mode":"ask","state":"pending",` +
  `"payload":{"exec":{"argv":["git","push","--force"]}}}}`;

function action(query: string): string | null {
  return new URLSearchParams(query).get("action");
}

describe("hitl gates", () => {
  it("exec waits through approved-without-result until the result lands", async () => {
    let polls = 0;
    let onHeld = "";
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      const path = pathOf(req);
      if (req.method === "POST" && path === "/v1/environments/vm-1") {
        res.statusCode = 202;
        res.end(heldExecBody);
        return;
      }
      if (req.method === "GET" && path === "/v1/interventions/int_1") {
        polls++;
        if (polls === 1) {
          res.end(
            `{"id":"int_1","kind":"exec","mode":"ask","state":"pending","payload":{"exec":{"argv":["git"]}}}`,
          );
        } else if (polls === 2) {
          // approved, but not run yet: not settled.
          res.end(
            `{"id":"int_1","kind":"exec","mode":"ask","state":"approved","payload":{"exec":{"argv":["git"]}},` +
              `"decision":{"outcome":"approve","edited_argv":["git","push","--force-with-lease"]}}`,
          );
        } else {
          res.end(
            `{"id":"int_1","kind":"exec","mode":"ask","state":"approved","payload":{"exec":{"argv":["git"]}},` +
              `"decision":{"outcome":"approve"},"result":{"exec":{"exit_code":0,"stdout":"pushed","stderr":""}}}`,
          );
        }
        return;
      }
      res.statusCode = 500;
      res.end();
    });

    const out = await current.client.environments.exec("vm-1", {
      cmd: ["git", "push", "--force"],
      onHeld: (i) => {
        onHeld = i.id;
      },
    });

    expect(out.stdout).toBe("pushed");
    expect(onHeld).toBe("int_1");
    expect(polls).toBe(3);
  });

  it("exec does not send wait or onHeld on the wire", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"exit_code":0,"stdout":"ok","stderr":""}`);
    });

    const out = await current.client.environments.exec("vm-1", {
      cmd: ["ls"],
      wait: false,
      onHeld: () => {},
    });

    expect(out.stdout).toBe("ok");
    expect(JSON.parse(body)).toEqual({ cmd: ["ls"] });
  });

  it("exec wait false throws PendingInterventionError without polling", async () => {
    const methods: Array<string | undefined> = [];
    current = await serve((req, res) => {
      methods.push(req.method);
      res.statusCode = 202;
      res.setHeader("Content-Type", "application/json");
      res.end(heldExecBody);
    });

    const err = await current.client.environments
      .exec("vm-1", { cmd: ["git"], wait: false })
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(PendingInterventionError);
    const pending = (err as PendingInterventionError).intervention;
    expect(pending.id).toBe("int_1");
    expect(pending.gate).toBe("force-push");
    expect(methods).toEqual(["POST"]);
  });

  it("client hitlNoWait and onHeld apply to every gated call", async () => {
    const seen: string[] = [];
    const held: string[] = [];
    const fetch: FetchLike = async (input, init) => {
      seen.push(`${init?.method} ${new URL(String(input)).pathname}`);
      return new Response(heldExecBody, {
        status: 202,
        headers: { "Content-Type": "application/json" },
      });
    };
    const client = new FuseClient({
      baseUrl: "http://fuse.test",
      fetch,
      hitlNoWait: true,
      onHeld: (i: Intervention) => held.push(i.id),
    });

    const err = await client.environments.destroy("vm-1").catch((e: unknown) => e);

    expect(err).toBeInstanceOf(PendingInterventionError);
    expect(held).toEqual(["int_1"]);
    expect(seen).toEqual(["DELETE /v1/environments/vm-1"]);
  });

  it("destroy detects a hitl_denied 403", async () => {
    current = await serve((_req, res) => {
      res.statusCode = 403;
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"error":{"code":"forbidden","message":"keep it","details":{"reason":"hitl_denied","gate":"no-destroy","intervention_id":"int_9"}}}`,
      );
    });

    const err = await current.client.environments
      .destroy("vm-1")
      .catch((e: unknown) => e);

    expect(isHitlDenied(err)).toBe(true);
    if (!isHitlDenied(err)) return;
    expect(err.message).toContain("keep it");
    expect(err.details?.gate).toBe("no-destroy");
  });

  it("drain held then rejected throws InterventionRejectedError with the reason", async () => {
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST" && action(queryOf(req)) === "drain") {
        res.statusCode = 202;
        res.end(
          `{"intervention_id":"int_2","intervention":{"id":"int_2","kind":"lifecycle","mode":"ask","state":"pending","payload":{"lifecycle":{"action":"drain"}}}}`,
        );
        return;
      }
      if (req.method === "GET" && pathOf(req) === "/v1/interventions/int_2") {
        res.end(
          `{"id":"int_2","kind":"lifecycle","mode":"ask","state":"rejected","payload":{"lifecycle":{"action":"drain"}},"decision":{"outcome":"reject","reason":"not now"}}`,
        );
        return;
      }
      res.statusCode = 500;
      res.end();
    });

    const err = await current.client.environments.drain("vm-1").catch((e: unknown) => e);

    expect(err).toBeInstanceOf(InterventionRejectedError);
    expect((err as InterventionRejectedError).intervention.decision?.reason).toBe(
      "not now",
    );
  });

  it("fork held then approved returns the new environment", async () => {
    const seen: string[] = [];
    current = await serve((req, res) => {
      seen.push(`${req.method} ${pathOf(req)}`);
      res.setHeader("Content-Type", "application/json");
      const path = pathOf(req);
      if (req.method === "POST" && action(queryOf(req)) === "fork") {
        res.statusCode = 202;
        res.end(
          `{"intervention_id":"int_3","intervention":{"id":"int_3","kind":"lifecycle","mode":"ask","state":"pending","payload":{"lifecycle":{"action":"fork"}}}}`,
        );
        return;
      }
      if (path === "/v1/interventions/int_3") {
        res.end(
          `{"id":"int_3","kind":"lifecycle","mode":"ask","state":"approved","payload":{"lifecycle":{"action":"fork"}},"decision":{"outcome":"approve"},"result":{"env_id":"vm-2"}}`,
        );
        return;
      }
      if (req.method === "GET" && path === "/v1/environments/vm-2") {
        res.end(`{"id":"vm-2","state":"running"}`);
        return;
      }
      res.statusCode = 500;
      res.end();
    });

    const env = await current.client.environments.fork("vm-1");

    expect(env.id).toBe("vm-2");
    expect(seen).toEqual([
      "POST /v1/environments/vm-1",
      "GET /v1/interventions/int_3",
      "GET /v1/environments/vm-2",
    ]);
  });

  it("snapshot held then approved with an error throws HeldActionError", async () => {
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST") {
        res.statusCode = 202;
        res.end(
          `{"intervention_id":"int_4","intervention":{"id":"int_4","kind":"lifecycle","mode":"ask","state":"pending","payload":{"lifecycle":{"action":"snapshot"}}}}`,
        );
        return;
      }
      res.end(
        `{"id":"int_4","kind":"lifecycle","mode":"ask","state":"approved","payload":{"lifecycle":{"action":"snapshot"}},"decision":{"outcome":"approve"},"result":{"error":"disk full"}}`,
      );
    });

    const err = await current.client.snapshots.create("vm-1").catch((e: unknown) => e);

    expect(err).toBeInstanceOf(HeldActionError);
    expect((err as HeldActionError).intervention.result?.error).toBe("disk full");
  });

  it("approve sends edited_argv and grant", async () => {
    let query = "";
    let body = "";
    current = await serve(async (req, res) => {
      query = queryOf(req);
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"int_1","state":"approved"}`);
    });

    await current.client.interventions.approve("int_1", {
      editedArgv: ["git", "push"],
      grant: "ttl:30m",
    });

    expect(action(query)).toBe("approve");
    expect(JSON.parse(body)).toEqual({ edited_argv: ["git", "push"], grant: "ttl:30m" });
  });

  it("isInterventionSettled needs a result for an approved ask-mode gate", () => {
    const base = {
      id: "int_1",
      kind: "exec",
      mode: "ask",
      state: "approved",
      payload: { exec: { argv: ["git"] } },
      decision: { outcome: "approve" },
    } as unknown as Intervention;
    expect(isInterventionSettled({ ...base, state: "pending" })).toBe(false);
    expect(isInterventionSettled(base)).toBe(false);
    expect(
      isInterventionSettled({
        ...base,
        result: { exec: { exit_code: 0, stdout: "", stderr: "" }, finished_at: "" },
      }),
    ).toBe(true);
    expect(
      isInterventionSettled({
        ...base,
        state: "rejected",
        decision: { ...base.decision!, outcome: "reject" },
      }),
    ).toBe(true);
    expect(isInterventionSettled({ ...base, mode: "notify" })).toBe(true);
    expect(
      isInterventionSettled({
        ...base,
        kind: "ask",
        mode: undefined,
        payload: { question: "?" },
      }),
    ).toBe(true);
  });
});
