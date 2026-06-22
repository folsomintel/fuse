import { afterEach, describe, expect, it } from "vitest";
import { pathOf, queryOf, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("hosts", () => {
  it("register posts to /v1/hosts and decodes the host", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"host-1","url":"https://h","state":"active",` +
          `"capacity":{"cpus":4,"ram_mb":8192,"storage_gb":100,"vm_count":10},` +
          `"allocated":{"cpus":0,"ram_mb":0,"storage_gb":0,"vm_count":0},` +
          `"last_seen":"2024-01-01T00:00:00Z","created_at":"2024-01-01T00:00:00Z",` +
          `"updated_at":"2024-01-01T00:00:00Z"}`,
      );
    });

    const host = await current.client.hosts.register({ id: "host-1", url: "https://h" });

    expect(method).toBe("POST");
    expect(path).toBe("/v1/hosts");
    expect(host.id).toBe("host-1");
  });

  it("list unwraps the hosts envelope", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(`{"hosts":[{"id":"host-1","url":"https://h","state":"active"}]}`);
    });

    const hosts = await current.client.hosts.list();

    expect(method).toBe("GET");
    expect(path).toBe("/v1/hosts");
    expect(hosts.map((h) => h.id)).toEqual(["host-1"]);
  });

  it("cordon posts action=cordon and resolves on 204", async () => {
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

    await current.client.hosts.cordon("host-1");

    expect(method).toBe("POST");
    expect(path).toBe("/v1/hosts/host-1");
    expect(query).toBe("action=cordon");
  });
});
