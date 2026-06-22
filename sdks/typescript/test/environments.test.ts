import { afterEach, describe, expect, it } from "vitest";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("environments", () => {
  it("create posts JSON to /v1/environments and decodes the response", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let auth: string | undefined;
    let body = "";
    current = await serve(async (req, res) => {
      method = req.method;
      path = pathOf(req);
      auth = req.headers.authorization;
      body = await readBody(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"running","task_id":"task-1","url":"https://x"}`);
    });

    const env = await current.client.environments.create({ task_id: "task-1" });

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments");
    expect(auth).toBe("Bearer tok");
    expect(JSON.parse(body)).toEqual({ task_id: "task-1" });
    expect(env.id).toBe("vm-1");
    expect(env.state).toBe("running");
  });

  it("list sends snake_case query and unwraps the envelope", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"environments":[{"id":"vm-1","state":"running","task_id":"task-1","url":"u"},` +
          `{"id":"vm-2","state":"draining","task_id":"task-1","url":"u"}]}`,
      );
    });

    const envs = await current.client.environments.list({ taskId: "task-1" });

    expect(method).toBe("GET");
    expect(path).toBe("/v1/environments");
    expect(query).toBe("task_id=task-1");
    expect(envs.map((e) => e.id)).toEqual(["vm-1", "vm-2"]);
  });

  it("drain posts action=drain and returns the updated environment", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"id":"vm-1","state":"draining","task_id":"task-1","url":"u"}`);
    });

    const env = await current.client.environments.drain("vm-1");

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1");
    expect(query).toBe("action=drain");
    expect(env.state).toBe("draining");
  });

  it("rotateToken posts action=rotate-token and resolves on 204", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let query: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      query = queryOf(req);
      res.statusCode = 204;
      res.end();
    });

    await expect(
      current.client.environments.rotateToken("vm-1"),
    ).resolves.toBeUndefined();

    expect(method).toBe("POST");
    expect(path).toBe("/v1/environments/vm-1");
    expect(query).toBe("action=rotate-token");
  });

  it("destroy issues DELETE and resolves on 204", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.statusCode = 204;
      res.end();
    });

    await current.client.environments.destroy("vm-1");

    expect(method).toBe("DELETE");
    expect(path).toBe("/v1/environments/vm-1");
  });
});
