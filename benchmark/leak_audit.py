#!/usr/bin/env python3
"""leak audit: count everything a vm can leave behind, before and after a run.

    python3 leak_audit.py snapshot --out leak-before.json
    python3 leak_audit.py check --before leak-before.json --out leak.json --settle 60

check re-counts until every probe matches the baseline or --settle seconds
pass, then exits 1 if any count still differs. a probe that could not run
(tool missing, no permission) is null; null on both sides is reported as
skipped, a probe that worked before and not after is a failure.

network namespaces are counted two ways so the audit holds before and after
per-vm netns (#419): named ones in /run/netns, and distinct net namespaces
held by any process. today both stay flat; with a netns per vm a leaked one
shows up in either.
"""
import argparse
import json
import os
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import sampler  # noqa: E402

FC_DIR = os.environ.get("BENCH_FC_DIR", os.path.join(HERE, "..", "host-agent", "firecracker"))
STATE_DIR = os.environ.get("FC_STATE_DIR", os.path.join(FC_DIR, "agent-state"))
SNAPSHOTS_DIR = os.environ.get("SNAPSHOTS_DIR", os.path.join(STATE_DIR, "snapshots"))
NETNS_DIR = os.environ.get("BENCH_NETNS_DIR", "/run/netns")
ORCH_DEFAULTS = os.environ.get("BENCH_ORCH_DEFAULTS", "/etc/default/orchestrator")
RESTATE_ADMIN = os.environ.get("RESTATE_ADMIN_URL", "http://127.0.0.1:9070")
PROC = os.environ.get("BENCH_PROC", "/proc")


def run(cmd, timeout=30):
    """stdout of cmd, or None if it is missing or fails."""
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return p.stdout if p.returncode == 0 else None


def count_entries(path):
    """visible entries in a dir; a missing dir holds nothing."""
    try:
        return len([n for n in os.listdir(path) if not n.startswith(".")])
    except FileNotFoundError:
        return 0
    except OSError:
        return None


def processes_matching(needle):
    n = 0
    try:
        names = os.listdir(PROC)
    except OSError:
        return None
    for name in names:
        if not name.isdigit():
            continue
        cmd = (sampler.read(os.path.join(PROC, name, "cmdline")) or "").replace("\0", " ")
        if needle in cmd:
            n += 1
    return n


def live_netns():
    """distinct network namespaces any process is in (needs root to see all)."""
    seen = set()
    try:
        names = os.listdir(PROC)
    except OSError:
        return None
    for name in names:
        if name.isdigit():
            try:
                seen.add(os.readlink(os.path.join(PROC, name, "ns", "net")))
            except OSError:
                pass
    return len(seen) or None


def iptables_rules(tool):
    out = run([tool])
    if out is None:
        return None
    return sum(1 for line in out.splitlines() if line.startswith("-A "))


def line_count(cmd):
    out = run(cmd)
    return None if out is None else len([line for line in out.splitlines() if line.strip()])


def database_url():
    if os.environ.get("DATABASE_URL"):
        return os.environ["DATABASE_URL"]
    for line in (sampler.read(ORCH_DEFAULTS) or "").splitlines():
        if line.startswith("DATABASE_URL="):
            return line.split("=", 1)[1].strip().strip('"')
    return None


def psql_count(sql):
    url = database_url()
    if not url:
        return None
    out = run(["psql", url, "-tAc", sql])
    try:
        return int(out.strip())
    except (AttributeError, ValueError):
        return None


def restate_inflight():
    body = json.dumps({"query": "SELECT count(*) AS n FROM sys_invocation WHERE status != 'completed'"})
    req = urllib.request.Request(
        RESTATE_ADMIN + "/query", data=body.encode(), method="POST",
        headers={"Content-Type": "application/json", "Accept": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return int(json.load(r)["rows"][0]["n"])
    except Exception:
        return None


PROBES = {
    "fc_processes": lambda: len(sampler.fc_pids(PROC)) if os.path.isdir(PROC) else None,
    "uffd_processes": lambda: processes_matching("fc-uffd"),
    "netns_named": lambda: count_entries(NETNS_DIR),
    "netns_live": live_netns,
    "links": lambda: line_count(["ip", "-o", "link", "show"]),
    "iptables_rules": lambda: iptables_rules("iptables-save"),
    "ip6tables_rules": lambda: iptables_rules("ip6tables-save"),
    "mounts": lambda: line_count(["cat", os.path.join(PROC, "self", "mounts")]),
    "vm_dirs": lambda: count_entries(os.path.join(STATE_DIR, "vms")),
    "snapshot_dirs": lambda: count_entries(SNAPSHOTS_DIR),
    "db_vms": lambda: psql_count("SELECT count(*) FROM orchestrator_vms WHERE state <> 'destroyed'"),
    "db_snapshots": lambda: psql_count("SELECT count(*) FROM orchestrator_snapshots"),
    "restate_inflight": restate_inflight,
}


def snapshot(probes=None):
    out = {}
    for name, fn in (probes or PROBES).items():
        try:
            out[name] = fn()
        except Exception:
            out[name] = None
    return out


def compare(before, after):
    """(deltas, lost, skipped): deltas maps probe -> after - before."""
    deltas, lost, skipped = {}, [], []
    for k, b in before.items():
        a = after.get(k)
        if b is None and a is None:
            skipped.append(k)
        elif a is None:
            lost.append(k)
        elif b is not None and a != b:
            deltas[k] = a - b
    return deltas, lost, skipped


def check(before, settle, interval=5.0, probes=None, clock=time.monotonic, sleep=time.sleep):
    deadline = clock() + settle
    while True:
        after = snapshot(probes)
        deltas, lost, skipped = compare(before, after)
        if (not deltas and not lost) or clock() >= deadline:
            break
        sleep(interval)
    return {"ok": not deltas and not lost, "before": before, "after": after,
            "deltas": deltas, "lost": lost, "skipped": skipped}


def main(argv=None):
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("snapshot")
    s.add_argument("--out", required=True)
    c = sub.add_parser("check")
    c.add_argument("--before", required=True)
    c.add_argument("--out", required=True)
    c.add_argument("--settle", type=float, default=60)
    args = ap.parse_args(argv)

    if args.cmd == "snapshot":
        snap = snapshot()
        with open(args.out, "w") as f:
            json.dump(snap, f, indent=2)
        missing = [k for k, v in snap.items() if v is None]
        if missing:
            print(f"leak audit: no baseline for {', '.join(missing)} (skipped)", file=sys.stderr)
        return 0

    with open(args.before) as f:
        before = json.load(f)
    res = check(before, args.settle)
    with open(args.out, "w") as f:
        json.dump(res, f, indent=2)
    if res["ok"]:
        print(f"leak audit: clean ({len(before) - len(res['skipped'])} probes, skipped: {', '.join(res['skipped']) or 'none'})")
        return 0
    for k, d in res["deltas"].items():
        print(f"leak audit: {k} {before[k]} -> {res['after'][k]} ({d:+d})", file=sys.stderr)
    for k in res["lost"]:
        print(f"leak audit: {k} could not be counted after the run", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
