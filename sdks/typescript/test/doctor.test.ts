import { afterEach, describe, expect, it } from "vitest";
import { serve, pathOf, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

describe("environments.doctor", () => {
  it("reads the self-check report from the doctor sub-path", async () => {
    current = await serve(async (req, res) => {
      expect(req.method).toBe("GET");
      expect(pathOf(req)).toBe("/v1/environments/vm-1/doctor");
      res.setHeader("content-type", "application/json");
      res.end(
        JSON.stringify({
          status: "fail",
          vm_id: "vm-1",
          version: "v1.4.0",
          checks: [
            { name: "display", status: "ok", message: ":1 1280x800" },
            { name: "vnc", status: "fail", message: "vnc server not reachable" },
            { name: "healthcheck", status: "skip" },
          ],
        }),
      );
    });

    const res = await current.client.environments.doctor("vm-1");
    expect(res.status).toBe("fail");
    expect(res.version).toBe("v1.4.0");
    expect(res.checks).toHaveLength(3);
    expect(res.checks[0]).toEqual({
      name: "display",
      status: "ok",
      message: ":1 1280x800",
    });
    expect(res.checks[2]?.status).toBe("skip");
  });

  it("rejects an empty vm id before any request is made", async () => {
    current = await serve(async () => {
      throw new Error("no request should be made");
    });
    await expect(current.client.environments.doctor("")).rejects.toThrow();
  });
});
