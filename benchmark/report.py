#!/usr/bin/env python3
"""stats, knee detection and svg charts for the density and fan-out runs.

    python3 report.py ramp   <out-dir>    # batches.jsonl + ops.jsonl
    python3 report.py churn  <out-dir>    # arrivals.jsonl
    python3 report.py fanout <out-dir>    # children.jsonl + levels.jsonl
    python3 report.py knee   <arrivals.jsonl> [--factor 2] [--err-limit 0.01]

each suite writes summary.md and *.svg into <out-dir>. `knee` prints the first
rate where p99 or the error rate broke, or nothing. stdlib only.
"""
import argparse
import json
import math
import os
import sys
from html import escape

# categorical slots 1-4, light and dark steps, always assigned in this order
SERIES = [("#2a78d6", "#3987e5"), ("#eb6834", "#d95926"), ("#1baf7a", "#199e70"), ("#eda100", "#c98500")]


def load_jsonl(path):
    if not os.path.exists(path):
        return []
    with open(path) as f:
        return [json.loads(line) for line in f if line.strip()]


def pct(values, p):
    """nearest-rank percentile, the same rule as pct() in lib.sh."""
    vals = sorted(v for v in values if v is not None)
    if not vals:
        return None
    idx = math.ceil(p / 100 * len(vals) - 1e-9)
    return vals[min(max(idx, 1), len(vals)) - 1]


def fmt(v, nd=3):
    return "NA" if v is None else f"{v:.{nd}f}"


def mb(kb):
    return None if kb is None else kb / 1024


# ---------------------------------------------------------------------------
# svg


def nice_ticks(lo, hi, n=5):
    if hi <= lo:
        hi = lo + 1
    raw = (hi - lo) / n
    mag = 10 ** math.floor(math.log10(raw))
    step = next(m * mag for m in (1, 2, 2.5, 5, 10) if m * mag >= raw)
    # first tick at or below lo, last at or above hi, so every point is inside
    ticks = [math.floor(lo / step) * step]
    while ticks[-1] < hi - step * 1e-9:
        ticks.append(round(ticks[-1] + step, 10))
    return ticks


def tick_label(v):
    return f"{int(v)}" if float(v).is_integer() else f"{v:g}"


def line_chart(title, xlabel, ylabel, series, marker=None, width=720, height=380):
    """series: [(name, [(x, y), ...])], at most 4. marker: (x, label) for a
    dashed vertical line. one y axis; both themes via prefers-color-scheme."""
    series = [(n, [(x, y) for x, y in pts if x is not None and y is not None]) for n, pts in series[:4]]
    pts = [p for _, s in series for p in s]
    ml, mr, mt, mb_ = 64, 150, 48, 52
    pw, ph = width - ml - mr, height - mt - mb_
    xs = [p[0] for p in pts] or [0, 1]
    ys = [p[1] for p in pts] or [0, 1]
    xt = nice_ticks(min(xs), max(xs))
    yt = nice_ticks(min(0, min(ys)), max(ys))
    x0, x1, y0, y1 = xt[0], xt[-1], yt[0], yt[-1]

    def sx(x):
        return ml + (x - x0) / ((x1 - x0) or 1) * pw

    def sy(y):
        return mt + ph - (y - y0) / ((y1 - y0) or 1) * ph

    css = ["svg{font-family:system-ui,sans-serif;font-size:12px}",
           ".bg{fill:#fcfcfb}.ink{fill:#0b0b0b}.ink2{fill:#52514e}.grid{stroke:#e4e3df}.axis{stroke:#8a8984}",
           ".mark{stroke:#52514e}"]
    dark = [".bg{fill:#1a1a19}.ink{fill:#f0efec}.ink2{fill:#c3c2b7}.grid{stroke:#383835}.axis{stroke:#6b6a66}",
            ".mark{stroke:#c3c2b7}"]
    for i, (light, dk) in enumerate(SERIES):
        css.append(f".s{i}{{stroke:{light};fill:{light}}}")
        dark.append(f".s{i}{{stroke:{dk};fill:{dk}}}")
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {height}" width="{width}" height="{height}" role="img">',
           f"<title>{escape(title)}</title>",
           "<style>" + "".join(css) + "@media (prefers-color-scheme: dark){" + "".join(dark) + "}</style>",
           f'<rect class="bg" width="{width}" height="{height}"/>',
           f'<text class="ink" x="{ml}" y="24" font-size="15" font-weight="600">{escape(title)}</text>']
    for t in yt:
        out.append(f'<line class="grid" x1="{ml}" x2="{ml + pw}" y1="{sy(t):.1f}" y2="{sy(t):.1f}"/>')
        out.append(f'<text class="ink2" x="{ml - 8}" y="{sy(t) + 4:.1f}" text-anchor="end">{tick_label(t)}</text>')
    for t in xt:
        out.append(f'<text class="ink2" x="{sx(t):.1f}" y="{mt + ph + 18}" text-anchor="middle">{tick_label(t)}</text>')
    out.append(f'<line class="axis" x1="{ml}" x2="{ml + pw}" y1="{mt + ph}" y2="{mt + ph}"/>')
    out.append(f'<text class="ink2" x="{ml + pw / 2:.1f}" y="{height - 12}" text-anchor="middle">{escape(xlabel)}</text>')
    out.append(f'<text class="ink2" transform="translate(16 {mt + ph / 2:.1f}) rotate(-90)" text-anchor="middle">{escape(ylabel)}</text>')
    if marker is not None and marker[0] is not None:
        mx = sx(marker[0])
        out.append(f'<line class="mark" x1="{mx:.1f}" x2="{mx:.1f}" y1="{mt}" y2="{mt + ph}" stroke-dasharray="4 4" stroke-width="1.5"/>')
        out.append(f'<text class="ink" x="{mx - 4:.1f}" y="{mt + ph - 6}" text-anchor="end">{escape(marker[1])}</text>')
    for i, (name, s) in enumerate(series):
        s = sorted(s)
        if len(s) > 1:
            d = " ".join(f"{'M' if j == 0 else 'L'}{sx(x):.1f},{sy(y):.1f}" for j, (x, y) in enumerate(s))
            out.append(f'<path class="s{i}" d="{d}" fill="none" stroke-width="2" style="fill:none"/>')
        for x, y in s:
            out.append(f'<circle class="s{i}" cx="{sx(x):.1f}" cy="{sy(y):.1f}" r="4"><title>{escape(name)}: {tick_label(x)}, {y:.3g}</title></circle>')
        # legend, right of the plot, in the same order as the series
        ly = mt + 8 + i * 20
        out.append(f'<line class="s{i}" x1="{ml + pw + 16}" x2="{ml + pw + 32}" y1="{ly}" y2="{ly}" stroke-width="2"/>')
        out.append(f'<text class="ink" x="{ml + pw + 38}" y="{ly + 4}">{escape(name)}</text>')
    if not pts:
        out.append(f'<text class="ink2" x="{ml + pw / 2:.1f}" y="{mt + ph / 2:.1f}" text-anchor="middle">no samples</text>')
    out.append("</svg>")
    return "\n".join(out) + "\n"


def write(path, text):
    with open(path, "w") as f:
        f.write(text)


def md_table(header, rows):
    lines = ["| " + " | ".join(header) + " |", "|" + "|".join("---" for _ in header) + "|"]
    lines += ["| " + " | ".join(str(c) for c in r) + " |" for r in rows]
    return "\n".join(lines)


def samples_charts(out):
    """host timeline from the 1 hz sampler, shared by every suite."""
    samples = load_jsonl(os.path.join(out, "samples.jsonl"))
    if not samples:
        return
    t0 = samples[0]["t"]
    write(os.path.join(out, "host-memory.svg"), line_chart(
        "host memory over the run", "seconds", "MB",
        [("MemAvailable", [(s["t"] - t0, mb(s.get("mem_available_kb"))) for s in samples]),
         ("firecracker pss", [(s["t"] - t0, mb(s.get("fc_pss_kb"))) for s in samples])]))
    write(os.path.join(out, "host-psi.svg"), line_chart(
        "pressure stall (avg10)", "seconds", "% of time stalled",
        [("memory some", [(s["t"] - t0, s.get("psi_mem_some")) for s in samples]),
         ("memory full", [(s["t"] - t0, s.get("psi_mem_full")) for s in samples]),
         ("cpu some", [(s["t"] - t0, s.get("psi_cpu_some")) for s in samples]),
         ("io some", [(s["t"] - t0, s.get("psi_io_some")) for s in samples])]))


def leak_line(out):
    path = os.path.join(out, "leak.json")
    if not os.path.exists(path):
        return "leak audit: not run"
    with open(path) as f:
        leak = json.load(f)
    if leak.get("ok"):
        return f"leak audit: clean (skipped: {', '.join(leak.get('skipped') or []) or 'none'})"
    parts = [f"{k} {v:+d}" for k, v in leak.get("deltas", {}).items()] + [f"{k} lost" for k in leak.get("lost", [])]
    return "leak audit: FAILED (" + ", ".join(parts) + ")"


# ---------------------------------------------------------------------------
# suites


def ramp_rows(out):
    """per-batch stats: host state from batches.jsonl, latencies from ops.jsonl."""
    batches = load_jsonl(os.path.join(out, "batches.jsonl"))
    ops = load_jsonl(os.path.join(out, "ops.jsonl"))
    rows = []
    for b in batches:
        mine = [o for o in ops if o.get("batch") == b["batch"]]
        create = [o["s"] for o in mine if o["op"] == "create" and o.get("ok")]
        exe = [o["s"] for o in mine if o["op"] == "exec" and o.get("ok")]
        errors = sum(1 for o in mine if not o.get("ok"))
        rows.append(dict(b, create_p50=pct(create, 50), create_p99=pct(create, 99),
                         exec_p50=pct(exe, 50), exec_p99=pct(exe, 99), errors=errors))
    return rows


def report_ramp(out):
    rows = ramp_rows(out)
    live = [(r["live"], r) for r in rows]
    write(os.path.join(out, "ramp-memory.svg"), line_chart(
        "host memory vs live vms", "live vms", "MB",
        [("MemAvailable", [(n, mb(r.get("mem_available_kb"))) for n, r in live]),
         ("firecracker pss", [(n, mb(r.get("fc_pss_kb"))) for n, r in live])]))
    write(os.path.join(out, "ramp-latency.svg"), line_chart(
        "create and exec latency vs live vms", "live vms", "seconds",
        [("create p50", [(n, r["create_p50"]) for n, r in live]),
         ("create p99", [(n, r["create_p99"]) for n, r in live]),
         ("exec p50", [(n, r["exec_p50"]) for n, r in live]),
         ("exec p99", [(n, r["exec_p99"]) for n, r in live])]))
    samples_charts(out)
    stop = load_jsonl(os.path.join(out, "stop.jsonl"))
    last = rows[-1] if rows else {}
    per_vm = None
    if last.get("fc_pss_kb") and last.get("fc_count"):
        per_vm = last["fc_pss_kb"] / last["fc_count"] / 1024
    md = ["## density ramp", "",
          f"max live: {last.get('live', 0)}, stop: {stop[-1]['reason'] if stop else 'NA'}, "
          f"firecracker pss per vm at the top: {fmt(per_vm, 1)} MB", "",
          md_table(["batch", "live", "errors", "MemAvailable MB", "fc pss MB", "psi mem some", "create p50", "create p99", "exec p50", "exec p99"],
                   [[r["batch"], r["live"], r["errors"], fmt(mb(r.get("mem_available_kb")), 0), fmt(mb(r.get("fc_pss_kb")), 0),
                     fmt(r.get("psi_mem_some"), 2), fmt(r["create_p50"]), fmt(r["create_p99"]), fmt(r["exec_p50"]), fmt(r["exec_p99"])]
                    for r in rows]),
          "", leak_line(out), ""]
    write(os.path.join(out, "summary.md"), "\n".join(md))
    return rows


def churn_rows(arrivals):
    rows = []
    for rate in sorted({a["rate"] for a in arrivals}):
        mine = [a for a in arrivals if a["rate"] == rate]
        ok = [a for a in mine if a.get("ok")]
        rows.append({
            "rate": rate, "n": len(mine), "errors": len(mine) - len(ok),
            "err_rate": (len(mine) - len(ok)) / len(mine) if mine else 0.0,
            "create_p50": pct([a.get("create_s") for a in ok], 50),
            "create_p99": pct([a.get("create_s") for a in ok], 99),
            "exec_p99": pct([a.get("exec_s") for a in ok], 99),
            "total_p99": pct([a.get("total_s") for a in ok], 99),
        })
    return rows


def find_knee(rows, factor=2.0, err_limit=0.01):
    """first rate where errors pass err_limit or create p99 passes factor x
    the p99 at the lowest rate. None when the sweep never broke."""
    if not rows:
        return None
    base = rows[0]["create_p99"]
    for r in rows:
        if r["err_rate"] > err_limit:
            return r["rate"]
        if base is not None and r["create_p99"] is not None and r["create_p99"] > factor * base:
            return r["rate"]
    return None


def report_churn(out, factor, err_limit):
    rows = churn_rows(load_jsonl(os.path.join(out, "arrivals.jsonl")))
    knee = find_knee(rows, factor, err_limit)
    marker = (knee, f"knee {tick_label(knee)}/s") if knee is not None else None
    write(os.path.join(out, "churn-latency.svg"), line_chart(
        "churn latency from scheduled arrival", "arrivals per second", "seconds",
        [("create p50", [(r["rate"], r["create_p50"]) for r in rows]),
         ("create p99", [(r["rate"], r["create_p99"]) for r in rows]),
         ("lifecycle p99", [(r["rate"], r["total_p99"]) for r in rows])], marker))
    write(os.path.join(out, "churn-errors.svg"), line_chart(
        "churn errors", "arrivals per second", "% of arrivals failed",
        [("errors", [(r["rate"], r["err_rate"] * 100) for r in rows])], marker))
    samples_charts(out)
    md = ["## churn (open loop)", "",
          f"knee: {tick_label(knee) + ' arrivals/s' if knee is not None else 'not reached'} "
          f"(p99 > {factor:g}x the lowest rate's, or errors > {err_limit:.0%})", "",
          md_table(["rate/s", "arrivals", "errors", "create p50", "create p99", "exec p99", "lifecycle p99"],
                   [[tick_label(r["rate"]), r["n"], r["errors"], fmt(r["create_p50"]), fmt(r["create_p99"]),
                     fmt(r["exec_p99"]), fmt(r["total_p99"])] for r in rows]),
          "", leak_line(out), ""]
    write(os.path.join(out, "summary.md"), "\n".join(md))
    return rows, knee


def fanout_rows(out):
    children = load_jsonl(os.path.join(out, "children.jsonl"))
    levels = {lv["n"]: lv for lv in load_jsonl(os.path.join(out, "levels.jsonl"))}
    rows = []
    for n in sorted(levels):
        mine = [c for c in children if c["n"] == n]
        ok = [c for c in mine if c.get("ok")]
        lv = levels[n]
        used = None
        if lv.get("mem_available_before_kb") is not None and lv.get("mem_available_after_kb") is not None:
            used = lv["mem_available_before_kb"] - lv["mem_available_after_kb"]
        rows.append({"n": n, "ok": len(ok), "errors": len(mine) - len(ok),
                     "fork_p50": pct([c.get("fork_s") for c in ok], 50),
                     "first_exec_p50": pct([c.get("first_exec_s") for c in ok], 50),
                     "first_exec_p99": pct([c.get("first_exec_s") for c in ok], 99),
                     "live_memory": sum(1 for c in ok if c.get("memory") == "live"),
                     "host_used_kb": used, "fc_pss_kb": lv.get("fc_pss_after_kb")})
    return rows


def report_fanout(out):
    rows = fanout_rows(out)
    write(os.path.join(out, "fanout-exec.svg"), line_chart(
        "time to first exec per child", "children (n)", "seconds",
        [("first exec p50", [(r["n"], r["first_exec_p50"]) for r in rows]),
         ("first exec p99", [(r["n"], r["first_exec_p99"]) for r in rows])]))
    write(os.path.join(out, "fanout-memory.svg"), line_chart(
        "host memory vs children", "children (n)", "MB",
        [("host used (MemAvailable drop)", [(r["n"], mb(r["host_used_kb"])) for r in rows]),
         ("firecracker pss", [(r["n"], mb(r["fc_pss_kb"])) for r in rows])]))
    samples_charts(out)
    md = ["## fork fan-out", "",
          "children carry the parent's memory when `memory` is live (resumed), cold when they booted the rootfs.", "",
          md_table(["n", "ok", "errors", "memory live", "fork p50", "first exec p50", "first exec p99", "host used MB", "fc pss MB"],
                   [[r["n"], r["ok"], r["errors"], r["live_memory"], fmt(r["fork_p50"]), fmt(r["first_exec_p50"]),
                     fmt(r["first_exec_p99"]), fmt(mb(r["host_used_kb"]), 0), fmt(mb(r["fc_pss_kb"]), 0)] for r in rows]),
          "", leak_line(out), ""]
    write(os.path.join(out, "summary.md"), "\n".join(md))
    return rows


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("suite", choices=["ramp", "churn", "fanout", "knee"])
    ap.add_argument("path")
    ap.add_argument("--factor", type=float, default=float(os.environ.get("KNEE_FACTOR", "2")))
    ap.add_argument("--err-limit", type=float, default=float(os.environ.get("ERR_LIMIT", "0.01")))
    args = ap.parse_args(argv)
    if args.suite == "knee":
        knee = find_knee(churn_rows(load_jsonl(args.path)), args.factor, args.err_limit)
        if knee is not None:
            print(tick_label(knee))
        return 0
    if args.suite == "ramp":
        report_ramp(args.path)
    elif args.suite == "churn":
        report_churn(args.path, args.factor, args.err_limit)
    else:
        report_fanout(args.path)
    with open(os.path.join(args.path, "summary.md")) as f:
        print(f.read())
    return 0


if __name__ == "__main__":
    sys.exit(main())
