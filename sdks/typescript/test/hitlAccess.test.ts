import { afterEach, describe, expect, it } from "vitest";
import {
  FuseApiError,
  FuseClient,
  ForbiddenError,
  ForbiddenReason,
  HitlScope,
  isForbidden,
  type FetchLike,
} from "../src/index.js";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

const keysBody = `{"api_keys":[]}`;

describe("hitl access", () => {
  it("withActor sends the actor headers and the original client does not", async () => {
    const seen: Array<[string | undefined, string | undefined]> = [];
    current = await serve((req, res) => {
      seen.push([
        req.headers["fuse-actor"] as string | undefined,
        req.headers["fuse-actor-name"] as string | undefined,
      ]);
      res.setHeader("Content-Type", "application/json");
      res.end(keysBody);
    });

    const alice = current.client.withActor({ id: "u-alice", name: "Alice" });
    await alice.apiKeys.list();
    await current.client.apiKeys.list();

    expect(seen).toEqual([
      ["u-alice", "Alice"],
      [undefined, undefined],
    ]);
  });

  it("the actor client option sends the actor headers", async () => {
    let headers: Record<string, string> | undefined;
    const fetch: FetchLike = async (_input, init) => {
      headers = init?.headers as Record<string, string>;
      return new Response(keysBody, { headers: { "Content-Type": "application/json" } });
    };
    const client = new FuseClient({
      baseUrl: "http://fuse.test",
      fetch,
      actor: { id: "u-bob" },
    });

    await client.apiKeys.list();

    expect(headers?.["Fuse-Actor"]).toBe("u-bob");
    expect(headers?.["Fuse-Actor-Name"]).toBeUndefined();
  });

  it("scoped create sends scopes, env_id, ttl_seconds and actor and parses the new fields", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"key-2","label":"invite","created_at":"2026-01-01T00:00:00Z",` +
          `"scopes":["hitl:read","hitl:decide"],"env_id":"vm-1",` +
          `"expires_at":"2026-01-01T01:00:00Z","actor":{"id":"u-alice","name":"Alice"},` +
          `"created_by":"master","key":"secret-raw"}`,
      );
    });

    const key = await current.client.apiKeys.create({
      label: "invite",
      scopes: [HitlScope.Read, HitlScope.Decide],
      envId: "vm-1",
      ttlSeconds: 3600,
      actor: { id: "u-alice", name: "Alice" },
    });

    expect(JSON.parse(body)).toEqual({
      label: "invite",
      scopes: ["hitl:read", "hitl:decide"],
      env_id: "vm-1",
      ttl_seconds: 3600,
      actor: { id: "u-alice", name: "Alice" },
    });
    expect(key.scopes).toEqual(["hitl:read", "hitl:decide"]);
    expect(key.env_id).toBe("vm-1");
    expect(key.expires_at).toBe("2026-01-01T01:00:00Z");
    expect(key.actor).toEqual({ id: "u-alice", name: "Alice" });
    expect(key.created_by).toBe("master");
    expect(key.key).toBe("secret-raw");
  });

  it("create with a label still sends only the label", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"key-1","created_at":"2026-01-01T00:00:00Z","key":"k"}`);
    });

    await current.client.apiKeys.create("ci");

    expect(JSON.parse(body)).toEqual({ label: "ci" });
  });

  it("auditPage sends filters and parses entries and next_cursor", async () => {
    let path = "";
    let query = "";
    current = await serve((req, res) => {
      path = pathOf(req);
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"entries":[{"seq":7,"event":"decided","intervention_id":"iv-1","env_id":"vm-1",` +
          `"actor":{"id":"u-alice","name":"Alice","source":"key","key_id":"k1"},` +
          `"remote_addr":"10.0.0.1:5000","at":"2026-01-01T00:01:00Z",` +
          `"prev_hash":"00ab","hash":"cd12"}],"next_cursor":"c2"}`,
      );
    });

    const page = await current.client.interventions.auditPage({
      envId: "vm-1",
      actor: "u-alice",
      since: "24h",
      limit: 10,
      cursor: "c1",
    });

    expect(path).toBe("/v1/hitl/audit");
    expect(new URLSearchParams(query)).toEqual(
      new URLSearchParams("env=vm-1&actor=u-alice&since=24h&limit=10&cursor=c1"),
    );
    expect(page.nextCursor).toBe("c2");
    expect(page.entries).toHaveLength(1);
    const e = page.entries[0]!;
    expect(e.seq).toBe(7);
    expect(e.event).toBe("decided");
    expect(e.intervention_id).toBe("iv-1");
    expect(e.actor).toEqual({
      id: "u-alice",
      name: "Alice",
      source: "key",
      key_id: "k1",
    });
    expect(e.remote_addr).toBe("10.0.0.1:5000");
    expect(e.prev_hash).toBe("00ab");
    expect(e.hash).toBe("cd12");
  });

  it("audit walks every page", async () => {
    const cursors: Array<string | null> = [];
    current = await serve((req, res) => {
      const cursor = new URLSearchParams(queryOf(req)).get("cursor");
      cursors.push(cursor);
      res.setHeader("Content-Type", "application/json");
      const row = (seq: number) =>
        `{"seq":${seq},"event":"raised","actor":{"id":"system","source":"system"},` +
        `"at":"2026-01-01T00:00:00Z","prev_hash":"00","hash":"01"}`;
      res.end(
        cursor ? `{"entries":[${row(2)}]}` : `{"entries":[${row(1)}],"next_cursor":"c2"}`,
      );
    });

    const rows = await current.client.interventions.audit();

    expect(rows.map((r) => r.seq)).toEqual([1, 2]);
    expect(cursors).toEqual([null, "c2"]);
  });

  it("list sends the actor filter", async () => {
    let query = "";
    current = await serve((req, res) => {
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"interventions":[]}`);
    });

    await current.client.interventions.listPage({ actor: "u-alice" });

    expect(new URLSearchParams(query).get("actor")).toBe("u-alice");
  });

  it("a 403 forbidden surfaces as ForbiddenError with its reason", async () => {
    current = await serve((_req, res) => {
      res.statusCode = 403;
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"error":{"code":"forbidden","message":"cannot approve your own request",` +
          `"details":{"reason":"self_approval"}}}`,
      );
    });

    const err = await current.client.interventions
      .approve("iv-1", { answer: "yes" })
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ForbiddenError);
    expect(err).toBeInstanceOf(FuseApiError);
    expect(isForbidden(err)).toBe(true);
    const f = err as ForbiddenError;
    expect(f.status).toBe(403);
    expect(f.reason).toBe(ForbiddenReason.SelfApproval);
    expect(f.scope).toBeUndefined();
  });

  it("a missing_scope forbidden carries the scope", async () => {
    current = await serve((_req, res) => {
      res.statusCode = 403;
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"error":{"code":"forbidden","message":"needs hitl:decide",` +
          `"details":{"reason":"missing_scope","scope":"hitl:decide"}}}`,
      );
    });

    const err = await current.client.interventions
      .approve("iv-1")
      .catch((e: unknown) => e);

    expect(isForbidden(err)).toBe(true);
    expect((err as ForbiddenError).reason).toBe(ForbiddenReason.MissingScope);
    expect((err as ForbiddenError).scope).toBe("hitl:decide");
  });

  it("create serializes hitl.notify and require_actor", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"vm-1","state":"running","task_id":"t","url":"","spec":{},` +
          `"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
      );
    });

    await current.client.environments.create({
      task_id: "t",
      secrets: { HOOK: "s3cret" },
      hitl: {
        requests: { ask: {} },
        require_actor: true,
        notify: [{ url: "https://hooks.example.com/fuse", secret: "HOOK" }],
      },
    });

    expect(JSON.parse(body).hitl).toEqual({
      requests: { ask: {} },
      require_actor: true,
      notify: [{ url: "https://hooks.example.com/fuse", secret: "HOOK" }],
    });
  });
});
