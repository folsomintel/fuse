#!/usr/bin/env python3
"""host sampler: one json line per tick with memory, psi and firecracker pss.

    python3 sampler.py --out samples.jsonl            # 1 hz until killed
    python3 sampler.py --once                         # one line to stdout

stdlib only. every field is null when its /proc file is missing, so the same
code runs (and is tested) against a fixture tree via --proc.
"""
import argparse
import json
import os
import signal
import sys
import time

FC_COMM = "firecracker"


def read(path):
    try:
        with open(path) as f:
            return f.read()
    except OSError:
        return None


def meminfo(proc):
    """meminfo fields in kB, keyed by name."""
    text = read(os.path.join(proc, "meminfo"))
    out = {}
    for line in (text or "").splitlines():
        parts = line.replace(":", " ").split()
        if len(parts) >= 2 and parts[1].isdigit():
            out[parts[0]] = int(parts[1])
    return out


def psi(proc, resource):
    """{"some": avg10, "full": avg10} from /proc/pressure/<resource>."""
    text = read(os.path.join(proc, "pressure", resource))
    out = {"some": None, "full": None}
    for line in (text or "").splitlines():
        parts = line.split()
        if not parts or parts[0] not in out:
            continue
        for kv in parts[1:]:
            k, _, v = kv.partition("=")
            if k == "avg10":
                out[parts[0]] = float(v)
    return out


def fc_pids(proc):
    pids = []
    try:
        names = os.listdir(proc)
    except OSError:
        return pids
    for name in names:
        if name.isdigit() and (read(os.path.join(proc, name, "comm")) or "").strip() == FC_COMM:
            pids.append(int(name))
    return sorted(pids)


def pss_kb(proc, pid):
    """pss of one process from smaps_rollup, or None if it exited or is unreadable."""
    text = read(os.path.join(proc, str(pid), "smaps_rollup"))
    for line in (text or "").splitlines():
        if line.startswith("Pss:"):
            return int(line.split()[1])
    return None


def ksm_sharing(sys_root):
    v = read(os.path.join(sys_root, "kernel", "mm", "ksm", "pages_sharing"))
    return int(v) if v and v.strip().isdigit() else None


def sample(proc="/proc", sys_root="/sys", with_pss=True, now=None):
    m = meminfo(proc)
    pids = fc_pids(proc)
    pss = pss_missing = None
    if with_pss and m:
        vals = [v for v in (pss_kb(proc, p) for p in pids) if v is not None]
        # a pid can exit between listdir and the read, so a few misses are
        # normal; none readable at all (e.g. not root) means unknown, not 0.
        pss_missing = len(pids) - len(vals)
        pss = sum(vals) if vals or not pids else None
    load = read(os.path.join(proc, "loadavg"))
    swap_used = None
    if "SwapTotal" in m and "SwapFree" in m:
        swap_used = m["SwapTotal"] - m["SwapFree"]
    mem, cpu, io = psi(proc, "memory"), psi(proc, "cpu"), psi(proc, "io")
    return {
        "t": round(now if now is not None else time.time(), 3),
        "mem_total_kb": m.get("MemTotal"),
        "mem_available_kb": m.get("MemAvailable"),
        "shmem_kb": m.get("Shmem"),
        "swap_used_kb": swap_used,
        "psi_mem_some": mem["some"],
        "psi_mem_full": mem["full"],
        "psi_cpu_some": cpu["some"],
        "psi_io_some": io["some"],
        "psi_io_full": io["full"],
        "load1": float(load.split()[0]) if load else None,
        "fc_count": len(pids),
        "fc_pss_kb": pss,
        "fc_pss_missing": pss_missing,
        "ksm_pages_sharing": ksm_sharing(sys_root),
    }


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", help="append jsonl here (default stdout)")
    ap.add_argument("--interval", type=float, default=1.0)
    # pss walks every firecracker's page tables; with thousands of vms that is
    # too slow for every tick, so it is taken every n ticks (null otherwise).
    ap.add_argument("--pss-every", type=int, default=int(os.environ.get("PSS_EVERY", "5")))
    ap.add_argument("--once", action="store_true")
    ap.add_argument("--proc", default=os.environ.get("BENCH_PROC", "/proc"))
    ap.add_argument("--sys", default=os.environ.get("BENCH_SYS", "/sys"))
    args = ap.parse_args(argv)

    if args.once:
        print(json.dumps(sample(args.proc, args.sys)))
        return 0

    stop = []
    signal.signal(signal.SIGTERM, lambda *_: stop.append(1))
    out = open(args.out, "a") if args.out else sys.stdout
    start, tick = time.monotonic(), 0
    try:
        while not stop:
            with_pss = args.pss_every > 0 and tick % args.pss_every == 0
            out.write(json.dumps(sample(args.proc, args.sys, with_pss)) + "\n")
            out.flush()
            tick += 1
            # fixed schedule, so a slow tick does not push every later one back
            delay = start + tick * args.interval - time.monotonic()
            if delay > 0:
                time.sleep(delay)
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
