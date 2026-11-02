import { afterEach, describe, expect, it } from "vitest";
import { isInterventionApproved, isInterventionTerminal } from "../src/index.js";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

function intervention(state: string, decision?: string): string {
  const base =
    `"id":"iv-1","env_id":"vm-1","kind":"ask","state":"${state}",` +
    `"payload":{"question":"deploy?","choices":["yes","no"]},` +
    `"triggered_by":{"id":"key:k1","source":"key","key_id":"k1"},` +
    `"deadline":"2026-01-01T00:10:00Z","on_timeout":"reject",` +
    `"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"`;
  return decision ? `{${base},"decision":${decision}}` : `{${base}}`;
}

const approved =
  `{"outcome":"approve","answer":"yes","decided_by":{"id":"master","source":"key"},` +
  `"decided_at":"2026-01-01T00:01:00Z"}`;

interface Seen {
  method?: string;
  path: string;
  query: string;
  body: string;
}

describe("interventions", () => {
  it("requestHuman raises then polls until decided", async () => {
    const seen: Seen[] = [];
    let polls = 0;
    current = await serve(async (req, res) => {
      seen.push({
        method: req.method,
        path: pathOf(req),
        query: queryOf(req),
        body: await readBody(req),
      });
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST") {
        res.statusCode = 201;
        res.end(intervention("pending"));
        return;
      }
      polls++;
      res.end(polls < 2 ? intervention("pending") : intervention("approved", approved));
    });

    const out = await current.client.environments.requestHuman("vm-1", {
      question: "deploy?",
      choices: ["yes", "no"],
      timeoutSeconds: 60,
    });

    expect(seen[0]?.method).toBe("POST");
    expect(seen[0]?.path).toBe("/v1/environments/vm-1");
    expect(seen[0]?.query).toBe("action=request");
    expect(JSON.parse(seen[0]?.body ?? "")).toEqual({
      kind: "ask",
      question: "deploy?",
      choices: ["yes", "no"],
      timeout_seconds: 60,
    });
    expect(seen.slice(1).map((s) => [s.method, s.path, s.query])).toEqual([
      ["GET", "/v1/interventions/iv-1", "wait=30s"],
      ["GET", "/v1/interventions/iv-1", "wait=30s"],
    ]);
    expect(out.state).toBe("approved");
    expect(out.decision?.answer).toBe("yes");
    expect(isInterventionTerminal(out)).toBe(true);
    expect(isInterventionApproved(out)).toBe(true);
  });

  it("requestHuman with wait false returns the pending handle without polling", async () => {
    const methods: Array<string | undefined> = [];
    current = await serve((req, res) => {
      methods.push(req.method);
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(intervention("pending"));
    });

    const out = await current.client.environments.requestHuman("vm-1", {
      question: "deploy?",
      wait: false,
    });

    expect(methods).toEqual(["POST"]);
    expect(out.state).toBe("pending");
    expect(isInterventionTerminal(out)).toBe(false);
  });

  it("approve and reject send the action query and body", async () => {
    const seen: Seen[] = [];
    current = await serve(async (req, res) => {
      seen.push({
        method: req.method,
        path: pathOf(req),
        query: queryOf(req),
        body: await readBody(req),
      });
      res.setHeader("Content-Type", "application/json");
      res.end(intervention("approved", approved));
    });

    await current.client.interventions.approve("iv-1", { answer: "yes", reason: "ok" });
    await current.client.interventions.reject("iv-1", { reason: "not now" });

    expect(seen.map((s) => [s.method, s.path, s.query])).toEqual([
      ["POST", "/v1/interventions/iv-1", "action=approve"],
      ["POST", "/v1/interventions/iv-1", "action=reject"],
    ]);
    expect(seen.map((s) => JSON.parse(s.body))).toEqual([
      { answer: "yes", reason: "ok" },
      { reason: "not now" },
    ]);
  });

  it("list walks every page", async () => {
    const queries: string[] = [];
    current = await serve((req, res) => {
      const q = new URLSearchParams(queryOf(req));
      queries.push(queryOf(req));
      res.setHeader("Content-Type", "application/json");
      if (!q.get("cursor")) {
        res.end(`{"interventions":[${intervention("pending")}],"next_cursor":"c2"}`);
        return;
      }
      res.end(`{"interventions":[${intervention("approved", approved)}]}`);
    });

    const all = await current.client.interventions.list({ envId: "vm-1", state: "" });

    expect(all.map((i) => i.state)).toEqual(["pending", "approved"]);
    expect(queries).toEqual(["env=vm-1&limit=200", "env=vm-1&limit=200&cursor=c2"]);
  });

  it("create carries hitl on the wire", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"running","task_id":"task-1","url":"https://x"}`);
    });

    await current.client.environments.create({
      task_id: "task-1",
      hitl: { timeout_seconds: 120, on_timeout: "allow", requests: { ask: {} } },
    });

    expect(JSON.parse(body)).toEqual({
      task_id: "task-1",
      hitl: { timeout_seconds: 120, on_timeout: "allow", requests: { ask: {} } },
    });
  });
});
