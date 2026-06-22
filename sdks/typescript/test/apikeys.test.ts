import { afterEach, describe, expect, it } from "vitest";
import { pathOf, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("apiKeys", () => {
  it("create posts to /v1/api-keys and returns the one-time key", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"key-1","label":"ci","created_at":"2024-01-01T00:00:00Z","key":"secret-raw"}`,
      );
    });

    const key = await current.client.apiKeys.create("ci");

    expect(method).toBe("POST");
    expect(path).toBe("/v1/api-keys");
    expect(key.id).toBe("key-1");
    expect(key.key).toBe("secret-raw");
  });

  it("list unwraps the api_keys envelope", async () => {
    let method: string | undefined;
    let path: string | undefined;
    current = await serve((req, res) => {
      method = req.method;
      path = pathOf(req);
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"api_keys":[{"id":"key-1","label":"ci","created_at":"2024-01-01T00:00:00Z"}]}`,
      );
    });

    const keys = await current.client.apiKeys.list();

    expect(method).toBe("GET");
    expect(path).toBe("/v1/api-keys");
    expect(keys.map((k) => k.id)).toEqual(["key-1"]);
  });
});
