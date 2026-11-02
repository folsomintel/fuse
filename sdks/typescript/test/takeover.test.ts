import { afterEach, describe, expect, it } from "vitest";
import { LockedError, isLocked } from "../src/index.js";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

interface Seen {
  method?: string;
  path: string;
  query: string;
  body: string;
}

function intervention(
  id: string,
  kind: string,
  state: string,
  payload: string,
  decision?: string,
): string {
  const base =
    `"id":"${id}","env_id":"vm-1","kind":"${kind}","state":"${state}",` +
    `"payload":${payload},` +
    `"triggered_by":{"id":"key:k1","source":"key","key_id":"k1"},` +
    `"deadline":"2026-01-01T00:10:00Z","on_timeout":"reject",` +
    `"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"`;
  return decision ? `{${base},"decision":${decision}}` : `{${base}}`;
}

function approved(answer: string): string {
  return (
    `{"outcome":"approve","answer":"${answer}","decided_by":{"id":"master","source":"key"},` +
    `"decided_at":"2026-01-01T00:01:00Z"}`
  );
}

const secretPayload = `{"secret":{"target":"GH_TOKEN","label":"github token","message":"need push"}}`;
const takeoverPayload = `{"takeover":{"via":"terminal","reason":"captcha"}}`;
const heldPayload =
  `{"takeover":{"via":"terminal","reason":"captcha",` +
  `"holder":{"id":"master","source":"key"},"started_at":"2026-01-01T00:00:30Z"}}`;

const lockedBody = JSON.stringify({
  error: {
    code: "locked",
    message: "a human holds the environment",
    details: { intervention_id: "iv-t", kind: "takeover" },
  },
});

async function record(req: Parameters<Parameters<typeof serve>[0]>[0], seen: Seen[]) {
  seen.push({
    method: req.method,
    path: pathOf(req),
    query: queryOf(req),
    body: await readBody(req),
  });
}

describe("hitl secret requests", () => {
  it("requestHuman secret raises and waits; answerSecret posts the value", async () => {
    const seen: Seen[] = [];
    let answered = false;
    current = await serve(async (req, res) => {
      await record(req, seen);
      res.setHeader("Content-Type", "application/json");
      const last = seen[seen.length - 1]!;
      if (last.path === "/v1/environments/vm-1") {
        res.statusCode = 201;
        res.end(intervention("iv-s", "secret", "pending", secretPayload));
        return;
      }
      if (req.method === "POST") {
        answered = true;
        res.end(
          intervention("iv-s", "secret", "approved", secretPayload, approved("done")),
        );
        return;
      }
      res.end(
        answered
          ? intervention("iv-s", "secret", "approved", secretPayload, approved("done"))
          : intervention("iv-s", "secret", "pending", secretPayload),
      );
    });

    const pending = await current.client.environments.requestHuman("vm-1", {
      kind: "secret",
      target: "GH_TOKEN",
      message: "need push",
      wait: false,
    });
    expect(JSON.parse(seen[0]!.body)).toEqual({
      kind: "secret",
      target: "GH_TOKEN",
      message: "need push",
    });
    expect(pending.payload.secret).toEqual({
      target: "GH_TOKEN",
      label: "github token",
      message: "need push",
    });

    const out = await current.client.interventions.answerSecret("iv-s", "ghp_x");
    const answer = seen[1]!;
    expect([answer.method, answer.path, answer.query]).toEqual([
      "POST",
      "/v1/interventions/iv-s",
      "action=secret",
    ]);
    expect(JSON.parse(answer.body)).toEqual({ value: "ghp_x" });
    expect(out.state).toBe("approved");

    const done = await current.client.interventions.wait(pending.id);
    expect(done.decision?.answer).toBe("done");
  });
});

describe("hitl takeover", () => {
  it("takeover and release post their actions and bodies", async () => {
    const seen: Seen[] = [];
    current = await serve(async (req, res) => {
      await record(req, seen);
      res.setHeader("Content-Type", "application/json");
      const q = seen[seen.length - 1]!.query;
      if (q === "action=takeover") {
        res.end(intervention("iv-t", "takeover", "pending", heldPayload));
        return;
      }
      res.end(
        intervention("iv-t", "takeover", "approved", heldPayload, approved("fixed it")),
      );
    });

    const held = await current.client.environments.takeover("vm-1", {
      via: "terminal",
      reason: "captcha",
    });
    expect(held.payload.takeover?.holder?.id).toBe("master");
    expect(held.payload.takeover?.started_at).toBe("2026-01-01T00:00:30Z");

    const released = await current.client.environments.release("vm-1", {
      note: "fixed it",
    });
    expect(seen.map((s) => [s.method, s.path, s.query, JSON.parse(s.body)])).toEqual([
      [
        "POST",
        "/v1/environments/vm-1",
        "action=takeover",
        { via: "terminal", reason: "captcha" },
      ],
      ["POST", "/v1/environments/vm-1", "action=release", { note: "fixed it" }],
    ]);
    expect(released.decision?.answer).toBe("fixed it");
  });

  it("requestHuman takeover sends via and reason", async () => {
    const seen: Seen[] = [];
    current = await serve(async (req, res) => {
      await record(req, seen);
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(intervention("iv-t", "takeover", "pending", takeoverPayload));
    });
    await current.client.environments.requestHuman("vm-1", {
      kind: "takeover",
      via: "terminal",
      reason: "captcha",
      wait: false,
    });
    expect(JSON.parse(seen[0]!.body)).toEqual({
      kind: "takeover",
      via: "terminal",
      reason: "captcha",
    });
  });

  it("approve sends extend_seconds; getRecording returns the cast text", async () => {
    const seen: Seen[] = [];
    const cast = '{"version":2,"width":80,"height":24}\n[0.1,"o","hi"]\n';
    current = await serve(async (req, res) => {
      await record(req, seen);
      if (req.method === "GET") {
        res.setHeader("Content-Type", "application/x-asciicast");
        res.end(cast);
        return;
      }
      res.setHeader("Content-Type", "application/json");
      res.end(
        intervention(
          "iv-e",
          "expiry",
          "approved",
          `{"signal":{"reaper":"idle_timeout"}}`,
        ),
      );
    });
    await current.client.interventions.approve("iv-e", { extendSeconds: 1800 });
    expect(JSON.parse(seen[0]!.body)).toEqual({ extend_seconds: 1800 });

    const text = await current.client.interventions.getRecording("rec-1");
    expect([seen[1]!.method, seen[1]!.path]).toEqual([
      "GET",
      "/v1/hitl/recordings/rec-1",
    ]);
    expect(text).toBe(cast);
  });
});

describe("hitl 423 locked", () => {
  it("exec waits out a takeover then retries", async () => {
    const seen: Seen[] = [];
    let execs = 0;
    let polls = 0;
    current = await serve(async (req, res) => {
      await record(req, seen);
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST") {
        execs++;
        if (execs === 1) {
          res.statusCode = 423;
          res.end(lockedBody);
          return;
        }
        res.end(`{"exit_code":0,"stdout":"aGk=","stderr":""}`);
        return;
      }
      polls++;
      res.end(
        polls < 2
          ? intervention("iv-t", "takeover", "pending", heldPayload)
          : intervention("iv-t", "takeover", "approved", heldPayload, approved("")),
      );
    });

    const out = await current.client.environments.exec("vm-1", { cmd: ["ls"] });
    expect(out.exit_code).toBe(0);
    expect(seen.map((s) => [s.method, s.path, s.query])).toEqual([
      ["POST", "/v1/environments/vm-1", "action=exec"],
      ["GET", "/v1/interventions/iv-t", "wait=30s"],
      ["GET", "/v1/interventions/iv-t", "wait=30s"],
      ["POST", "/v1/environments/vm-1", "action=exec"],
    ]);
    // wait is sdk-only and never reaches the wire.
    expect(JSON.parse(seen[3]!.body)).toEqual({ cmd: ["ls"] });
  });

  it("computer waits out a stall then retries", async () => {
    let actions = 0;
    current = await serve(async (req, res) => {
      await readBody(req);
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST") {
        actions++;
        if (actions === 1) {
          res.statusCode = 423;
          res.end(lockedBody.replace('"takeover"', '"stall"'));
          return;
        }
        res.end(`{"screenshot":"cG5n"}`);
        return;
      }
      res.end(intervention("iv-t", "stall", "approved", "{}", approved("")));
    });
    const out = await current.client.environments.computer("vm-1", {
      action: "screenshot",
    });
    expect(out.screenshot).toBe("cG5n");
    expect(actions).toBe(2);
  });

  it("throws LockedError when waiting is off", async () => {
    let calls = 0;
    current = await serve(async (req, res) => {
      calls++;
      await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 423;
      res.end(lockedBody);
    });

    const err = await current.client.environments
      .exec("vm-1", { cmd: ["ls"], wait: false })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LockedError);
    expect(isLocked(err)).toBe(true);
    const locked = err as LockedError;
    expect(locked.status).toBe(423);
    expect(locked.interventionId).toBe("iv-t");
    expect(locked.kind).toBe("takeover");

    const err2 = await current.client.environments
      .computer("vm-1", { action: "screenshot" }, { wait: false })
      .catch((e: unknown) => e);
    expect(err2).toBeInstanceOf(LockedError);
    expect(calls).toBe(2);
  });
});
