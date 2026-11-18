import { afterEach, describe, expect, it } from "vitest";
import type { ServerResponse } from "node:http";
import { FuseApiError, ImageState, isNotFound, type Image } from "../src/index.js";
import { readBody, serve, type TestServer } from "./server.js";

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

const DIGEST = "a".repeat(64);

function image(state: string, extra: Partial<Image> = {}): Image {
  return {
    digest: DIGEST,
    ref: "ubuntu:24.04",
    manifest_digest: "sha256:" + "b".repeat(64),
    host_id: "fc-1",
    state,
    ...extra,
  };
}

describe("images", () => {
  it("convert posts the request and decodes the image", async () => {
    let method: string | undefined;
    let path: string | undefined;
    let body: unknown;
    current = await serve(async (req, res) => {
      method = req.method;
      path = req.url;
      body = JSON.parse(await readBody(req));
      json(res, 202, image("converting"));
    });

    const req = {
      ref: "ubuntu:24.04",
      host_id: "fc-1",
      region: "eu-west",
      labels: { zone: "a" },
    };
    const img = await current.client.images.convert(req);

    expect(method).toBe("POST");
    expect(path).toBe("/v1/images");
    expect(body).toEqual(req);
    expect(img.digest).toBe(DIGEST);
    expect(img.state).toBe(ImageState.Converting);
    expect(img.host_id).toBe("fc-1");
    expect(img.manifest_digest.startsWith("sha256:")).toBe(true);
  });

  it("convert sends an idempotency key, the caller's when given", async () => {
    const keys: (string | undefined)[] = [];
    current = await serve((req, res) => {
      keys.push(req.headers["idempotency-key"] as string | undefined);
      json(res, 202, image("converting"));
    });

    await current.client.images.convert({ ref: "ubuntu:24.04" });
    await current.client.images.convert(
      { ref: "ubuntu:24.04" },
      { idempotencyKey: "my-key" },
    );

    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe("my-key");
  });

  it("convert requires a ref", async () => {
    current = await serve((_req, res) => json(res, 202, image("converting")));
    await expect(current.client.images.convert({ ref: "" })).rejects.toThrow(
      "image ref is required",
    );
  });

  it("get path-escapes the digest and decodes a failed image", async () => {
    let path: string | undefined;
    current = await serve((req, res) => {
      path = req.url;
      json(res, 200, image("failed", { error: "pull failed: manifest unknown" }));
    });

    const img = await current.client.images.get("a/b c");

    expect(path).toBe("/v1/images/a%2Fb%20c");
    expect(img.state).toBe(ImageState.Failed);
    expect(img.error).toBe("pull failed: manifest unknown");
  });

  it("get of an unknown image is not found", async () => {
    current = await serve((_req, res) => {
      json(res, 404, { error: { code: "not_found", message: "image not found" } });
    });

    const err = await current.client.images.get(DIGEST).catch((e) => e);
    expect(err).toBeInstanceOf(FuseApiError);
    expect(isNotFound(err)).toBe(true);
  });

  it("create serializes spec.image_digest", async () => {
    let body = "";
    current = await serve(async (req, res) => {
      body = await readBody(req);
      json(res, 200, {
        id: "vm-1",
        state: "running",
        task_id: "task-1",
        url: "https://x",
      });
    });

    await current.client.environments.create({
      task_id: "task-1",
      spec: { image_digest: DIGEST },
    });

    expect(JSON.parse(body)).toEqual({
      task_id: "task-1",
      spec: { image_digest: DIGEST },
    });
  });
});
