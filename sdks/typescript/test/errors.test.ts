import { afterEach, describe, expect, it } from "vitest";
import { serve, type TestServer } from "./server.js";
import { FuseApiError, isFuseApiError, isNotFound } from "../src/index.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("errors", () => {
  it("parses the error envelope and surfaces a FuseApiError", async () => {
    current = await serve((req, res) => {
      res.statusCode = 404;
      res.setHeader("Content-Type", "application/json");
      res.setHeader("X-Request-ID", "req-1");
      res.end(`{"error":{"code":"not_found","message":"x","details":{"id":"missing"}}}`);
    });

    let caught: unknown;
    try {
      await current.client.environments.get("missing");
    } catch (err) {
      caught = err;
    }

    expect(isFuseApiError(caught)).toBe(true);
    expect(isNotFound(caught)).toBe(true);
    const err = caught as FuseApiError;
    expect(err.status).toBe(404);
    expect(err.code).toBe("not_found");
    expect(err.requestId).toBe("req-1");
    expect(err.details).toEqual({ id: "missing" });
    expect(err.message).toContain("not_found");
  });

  it("falls back to status text when the body is not an envelope", async () => {
    current = await serve((req, res) => {
      res.statusCode = 503;
      res.end("upstream down");
    });

    let caught: unknown;
    try {
      await current.client.hosts.list();
    } catch (err) {
      caught = err;
    }

    const err = caught as FuseApiError;
    expect(err.status).toBe(503);
    expect(err.body).toBe("upstream down");
  });
});
