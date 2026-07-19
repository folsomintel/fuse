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

  it("register decodes mig_profiles and gpu_devices on capacity", async () => {
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"gpu-1","url":"https://h","state":"active","backend":"qemu",` +
          `"capacity":{"cpus":64,"ram_mb":262144,"storage_gb":2000,"vm_count":8,` +
          `"gpus":2,"gpu_kind":"a100","mig_profiles":{"1g.10gb":14},` +
          `"gpu_devices":[{"uuid":"GPU-abc","model":"A100-SXM4-40GB",` +
          `"pci_bus_id":"0000:07:00.0","memory_mb":40960,"driver_version":"550.54.15",` +
          `"cuda_version":"12.4","compute_cap":"8.0","mig_capable":true,` +
          `"mig_mode":"enabled","iommu_group":"42"}]},` +
          `"allocated":{"cpus":0,"ram_mb":0,"storage_gb":0,"vm_count":0},` +
          `"last_seen":"2024-01-01T00:00:00Z","created_at":"2024-01-01T00:00:00Z",` +
          `"updated_at":"2024-01-01T00:00:00Z"}`,
      );
    });

    const host = await current.client.hosts.register({
      id: "gpu-1",
      url: "https://h",
      backend: "qemu",
      capacity: {
        cpus: 64,
        ram_mb: 262144,
        storage_gb: 2000,
        vm_count: 8,
        gpus: 2,
        gpu_kind: "a100",
        mig_profiles: { "1g.10gb": 14 },
      },
    });

    expect(host.capacity.mig_profiles).toEqual({ "1g.10gb": 14 });
    expect(host.capacity.gpu_devices?.length).toBe(1);
    const dev = host.capacity.gpu_devices?.[0];
    expect(dev?.uuid).toBe("GPU-abc");
    expect(dev?.model).toBe("A100-SXM4-40GB");
    expect(dev?.pci_bus_id).toBe("0000:07:00.0");
    expect(dev?.memory_mb).toBe(40960);
    expect(dev?.driver_version).toBe("550.54.15");
    expect(dev?.cuda_version).toBe("12.4");
    expect(dev?.compute_cap).toBe("8.0");
    expect(dev?.mig_capable).toBe(true);
    expect(dev?.mig_mode).toBe("enabled");
    expect(dev?.iommu_group).toBe("42");
    // the orchestrator only populates gpu_devices on capacity, never allocated.
    expect(host.allocated.gpu_devices).toBeUndefined();
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
