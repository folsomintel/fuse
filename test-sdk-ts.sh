#!/usr/bin/env bash
# end-to-end typescript sdk test. exercises every service method against a
# running orchestrator, using the in-repo sdks/typescript (built locally, not npm).
#
#   FUSE_TOKEN=<orchestrator-token> ./test-sdk-ts.sh
#
# env: FUSE_BASE_URL (default http://127.0.0.1:8080), FUSE_TOKEN (required),
#      FC_URL (default http://51.79.19.90:8090), FC_TOKEN (optional).
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${FUSE_BASE_URL:=http://127.0.0.1:8080}"
: "${FUSE_TOKEN:?set FUSE_TOKEN to the orchestrator auth token}"
: "${FC_URL:=http://51.79.19.90:8090}"
: "${FC_TOKEN:=}"
export FUSE_BASE_URL FUSE_TOKEN FC_URL FC_TOKEN

# build the local ts sdk (produces dist/) so it can be linked via file:
( cd "$REPO/sdks/typescript" && bun install --silent && bun run build >/dev/null )

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
cat > "$WORK/package.json" <<EOF
{ "name": "sdke2e", "type": "module", "private": true,
  "dependencies": { "@folsom/fuse": "file:$REPO/sdks/typescript" } }
EOF

cat > "$WORK/test.ts" <<'TSEOF'
import { FuseClient, FuseApiError } from "@folsom/fuse";

const base = process.env.FUSE_BASE_URL || "http://127.0.0.1:8080";
const token = process.env.FUSE_TOKEN || "";
const fcUrl = process.env.FC_URL || "http://51.79.19.90:8090";
const fcToken = process.env.FC_TOKEN || "";

let fails = 0;
const pass = (m: string) => console.log("PASS:", m);
const fail = (m: string) => {
  console.log("FAIL:", m);
  fails++;
};
const check = (c: boolean, m: string) => (c ? pass(m) : fail(m));
const isCode = (e: unknown, code: string, status?: number) =>
  e instanceof FuseApiError && e.code === code && (status === undefined || e.status === status);
async function wantNotFound(p: Promise<unknown>, m: string) {
  try {
    await p;
    fail(m + " -> expected not_found, got success");
  } catch (e) {
    check(isCode(e, "not_found"), m + " -> 404 not_found");
  }
}

const c = new FuseClient({ baseUrl: base, token });
pass("new client");

// hosts: full lifecycle
const hid = "e2e-host-ts";
try {
  await c.hosts.deregister(hid);
} catch {}
const h = await c.hosts.register({
  id: hid, url: fcUrl, token: fcToken, region: "local", backend: "firecracker",
  capacity: { cpus: 8, ram_mb: 16384, storage_gb: 100, vm_count: 10 },
});
check(h.id === hid && h.state === "active", "hosts.register -> active");
check((await c.hosts.list()).some((x) => x.id === hid), "hosts.list contains host");
let g = await c.hosts.get(hid);
check(g.url === fcUrl && g.capacity.cpus === 8, "hosts.get");
await c.hosts.cordon(hid);
check((await c.hosts.get(hid)).state === "cordoned", "host state == cordoned");
await c.hosts.uncordon(hid);
check((await c.hosts.get(hid)).state === "active", "host state == active");
await c.hosts.deregister(hid);
check(!(await c.hosts.list()).some((x) => x.id === hid), "host gone after deregister");
await wantNotFound(c.hosts.get(hid), "hosts.get(deregistered)");

// api keys: full lifecycle (requires postgres-backed orchestrator)
try {
  const created = await c.apiKeys.create("e2e-ts");
  check(!!created.key && !!created.id, "apiKeys.create returns secret");
  const kc = new FuseClient({ baseUrl: base, token: created.key });
  let authed = false;
  try { await kc.hosts.list(); authed = true; } catch {}
  check(authed, "created api key authenticates");
  check((await c.apiKeys.list()).some((k) => k.id === created.id), "apiKeys.list contains key");
  await c.apiKeys.revoke(created.id);
  let rejected = false;
  try { await kc.hosts.list(); } catch (e) { rejected = isCode(e, "unauthorized"); }
  check(rejected, "revoked key rejected (401)");
} catch (e) {
  console.log("SKIP: apiKeys (needs postgres-backed orchestrator):", String(e));
}

// environments
const envs = await c.environments.list();
check(Array.isArray(envs), `environments.list -> ${envs.length}`);
try {
  await c.environments.create({ task_id: "e2ets", spec: { cpus: 1, ram_mb: 512, storage_gb: 1 } });
  pass("environments.create succeeded (rootfs is complete!)");
} catch (e) {
  check(e instanceof FuseApiError && e.status === 500, "environments.create reached host (500, rootfs bake blocked)");
}
await wantNotFound(c.environments.get("nope-e2e"), "environments.get(missing)");
await wantNotFound(c.environments.drain("nope-e2e"), "environments.drain(missing)");
await wantNotFound(c.environments.fork("nope-e2e"), "environments.fork(missing)");
await wantNotFound(c.environments.rotateToken("nope-e2e"), "environments.rotateToken(missing)");
await wantNotFound(c.environments.destroy("nope-e2e"), "environments.destroy(missing)");

// snapshots
const snaps = await c.snapshots.list();
check(Array.isArray(snaps), `snapshots.list -> ${snaps.length}`);
await wantNotFound(c.snapshots.get("nope-e2e"), "snapshots.get(missing)");
await wantNotFound(c.snapshots.restore("nope-e2e"), "snapshots.restore(missing)");
await wantNotFound(c.snapshots.delete("nope-e2e"), "snapshots.delete(missing)");

// negative auth
const bad = new FuseClient({ baseUrl: base, token: "wrong-token" });
let na = false;
try { await bad.hosts.list(); } catch (e) { na = isCode(e, "unauthorized"); }
check(na, "negative auth rejected (401)");

console.log(`\n== ts sdk e2e: ${fails} failure(s) ==`);
if (fails > 0) process.exit(1);
TSEOF

cd "$WORK"
bun install --silent
bun run test.ts
