import { afterEach, describe, expect, it } from "vitest";
import type { ServerResponse } from "node:http";
import { ImageState, isNotFound } from "../src/index.js";
import { serve, type TestServer } from "./server.js";

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

describe("images on a host", () => {
  it("getOnHost reads that host's record", async () => {
    const paths: (string | undefined)[] = [];
    current = await serve(async (req, res) => {
      paths.push(req.url);
      if (req.url === `/v1/hosts/gone/images/${DIGEST}`) {
        json(res, 404, {
          error: { code: "not_found", message: 'host "gone" is not registered' },
        });
        return;
      }
      json(res, 200, {
        digest: DIGEST,
        ref: "ubuntu",
        manifest_digest: "sha256:x",
        host_id: "fc-2",
        state: "converting",
      });
    });

    const img = await current.client.images.getOnHost("fc-2", DIGEST);
    expect(paths[0]).toBe(`/v1/hosts/fc-2/images/${DIGEST}`);
    expect(img.host_id).toBe("fc-2");
    expect(img.state).toBe(ImageState.Converting);

    const err = await current.client.images
      .getOnHost("gone", DIGEST)
      .catch((e: unknown) => e);
    expect(isNotFound(err)).toBe(true);
    await expect(current.client.images.getOnHost("", DIGEST)).rejects.toThrow();
  });
});
