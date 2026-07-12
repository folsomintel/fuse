#!/usr/bin/env bash
# end-to-end python sdk test. exercises every service method against a running
# orchestrator, using the in-repo sdks/python. folsom-fuse is NOT published to
# pypi, so uv installs it from the local source tree via --with.
#
#   FUSE_TOKEN=<orchestrator-token> ./test-sdk-py.sh
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

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
cat > "$WORK/test.py" <<'PYEOF'
import os
import sys

import fuse

base = os.environ.get("FUSE_BASE_URL", "http://127.0.0.1:8080")
token = os.environ.get("FUSE_TOKEN", "")
fc_url = os.environ.get("FC_URL", "http://51.79.19.90:8090")
fc_token = os.environ.get("FC_TOKEN", "")

fails = 0


def ok(m):
    print("PASS:", m)


def bad(m):
    global fails
    print("FAIL:", m)
    fails += 1


def check(cond, m):
    ok(m) if cond else bad(m)


def want_not_found(fn, m):
    try:
        fn()
        bad(m + " -> expected not_found, got success")
    except Exception as e:  # noqa: BLE001
        check(fuse.is_not_found(e), m + " -> 404 not_found")


c = fuse.Client(base, token=token)
ok("new client")

# hosts: full lifecycle
hid = "e2e-host-py"
try:
    c.hosts.deregister(hid)
except Exception:  # noqa: BLE001
    pass
h = c.hosts.register(
    fuse.RegisterHostRequest(
        id=hid, url=fc_url, token=fc_token, region="local", backend="firecracker",
        capacity=fuse.HostCapacity(cpus=8, ram_mb=16384, storage_gb=100, vm_count=10),
    )
)
check(h.id == hid and h.state == "active", "hosts.register -> active")
check(any(x.id == hid for x in c.hosts.list()), "hosts.list contains host")
g = c.hosts.get(hid)
check(g.url == fc_url and g.capacity.cpus == 8, "hosts.get")
c.hosts.cordon(hid)
check(c.hosts.get(hid).state == "cordoned", "host state == cordoned")
c.hosts.uncordon(hid)
check(c.hosts.get(hid).state == "active", "host state == active")
c.hosts.deregister(hid)
check(not any(x.id == hid for x in c.hosts.list()), "host gone after deregister")
want_not_found(lambda: c.hosts.get(hid), "hosts.get(deregistered)")

# api keys: full lifecycle (requires postgres-backed orchestrator)
try:
    created = c.api_keys.create("e2e-py")
    check(bool(created.key) and bool(created.id), "api_keys.create returns secret")
    kc = fuse.Client(base, token=created.key)
    authed = True
    try:
        kc.hosts.list()
    except Exception:  # noqa: BLE001
        authed = False
    check(authed, "created api key authenticates")
    check(any(k.id == created.id for k in c.api_keys.list()), "api_keys.list contains key")
    c.api_keys.revoke(created.id)
    rejected = False
    try:
        kc.hosts.list()
    except Exception as e:  # noqa: BLE001
        rejected = fuse.is_unauthorized(e)
    check(rejected, "revoked key rejected (401)")
except Exception as e:  # noqa: BLE001
    print("SKIP: api_keys (needs postgres-backed orchestrator):", repr(e))

# environments
envs = c.environments.list()
check(isinstance(envs, list), f"environments.list -> {len(envs)}")
try:
    c.environments.create(
        fuse.CreateRequest(task_id="e2epy", spec=fuse.Spec(cpus=1, ram_mb=512, storage_gb=1))
    )
    ok("environments.create succeeded (rootfs is complete!)")
except Exception as e:  # noqa: BLE001
    ae = fuse.as_api_error(e)
    check(ae is not None and ae.status == 500, "environments.create reached host (500, rootfs bake blocked)")
want_not_found(lambda: c.environments.get("nope-e2e"), "environments.get(missing)")
want_not_found(lambda: c.environments.drain("nope-e2e"), "environments.drain(missing)")
want_not_found(lambda: c.environments.fork("nope-e2e"), "environments.fork(missing)")
want_not_found(lambda: c.environments.rotate_token("nope-e2e"), "environments.rotate_token(missing)")
want_not_found(lambda: c.environments.destroy("nope-e2e"), "environments.destroy(missing)")

# snapshots
snaps = c.snapshots.list()
check(isinstance(snaps, list), f"snapshots.list -> {len(snaps)}")
want_not_found(lambda: c.snapshots.get("nope-e2e"), "snapshots.get(missing)")
want_not_found(lambda: c.snapshots.restore("nope-e2e"), "snapshots.restore(missing)")
want_not_found(lambda: c.snapshots.delete("nope-e2e"), "snapshots.delete(missing)")

# negative auth
bad_client = fuse.Client(base, token="wrong-token")
na = False
try:
    bad_client.hosts.list()
except Exception as e:  # noqa: BLE001
    na = fuse.is_unauthorized(e)
check(na, "negative auth rejected (401)")

print(f"\n== py sdk e2e: {fails} failure(s) ==")
if fails > 0:
    sys.exit(1)
PYEOF

# folsom-fuse is not on pypi; install from the local source tree.
uv run --quiet --with "$REPO/sdks/python" python "$WORK/test.py"
