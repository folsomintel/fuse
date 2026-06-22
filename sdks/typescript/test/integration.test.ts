// Optional end-to-end tests against a live Fuse control plane. Skipped unless
// FUSE_TEST_BASE_URL is set, so CI's default (hermetic) run never needs an
// external service.
//
//   FUSE_TEST_BASE_URL=http://localhost:8080 FUSE_TEST_TOKEN=... npm test

import { describe, expect, it } from "vitest";
import { FuseClient, isNotFound } from "../src/index.js";

const baseUrl = process.env.FUSE_TEST_BASE_URL;
const token = process.env.FUSE_TEST_TOKEN;

const suite = baseUrl ? describe : describe.skip;

suite("integration (live server)", () => {
  it("creates, fetches, lists, and destroys an environment", async () => {
    // Constructed inside the test so the skipped suite never builds a client.
    const client = new FuseClient({ baseUrl: baseUrl ?? "", token });
    const taskId = `ts-sdk-${process.pid}-${Math.round(performance.now())}`;

    const created = await client.environments.create({
      task_id: taskId,
      spec: { cpus: 1, ram_mb: 512, storage_gb: 10 },
    });
    expect(created.id).toBeTruthy();

    const fetched = await client.environments.get(created.id);
    expect(fetched.id).toBe(created.id);

    const list = await client.environments.list({ taskId });
    expect(list.some((e) => e.id === created.id)).toBe(true);

    await client.environments.destroy(created.id);

    await expect(client.environments.get(created.id)).rejects.toSatisfy(isNotFound);
  });
});
