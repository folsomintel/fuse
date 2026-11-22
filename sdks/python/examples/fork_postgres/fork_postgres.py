# /// script
# requires-python = ">=3.9"
# dependencies = ["folsom-fuse"]
# ///
"""fork a live, warm postgres into n copies and wreck each one differently.

main boots from the Fusefile next to this file (postgres with a fixed 8 GiB
pgbench dataset), has its cache warmed, and is then forked live into n copies
in one call. main keeps running untouched as copy 0; every fork gets its own
destructive job. the driver times fork to first query per copy, compares the
first heavy query on each copy against main's cold and warm runs, samples host
memory throughout, checks main is byte-for-byte unchanged, destroys the copies
and runs the repo's leak audit.

run it on the fuse host itself, as root, from the checkout (the host memory
sampler and leak audit read /proc and the agent's state dir):

    export FUSE_URL=http://127.0.0.1:8080 FUSE_TOKEN=<master token>
    cd sdks/python && uv run python examples/fork_postgres/fork_postgres.py --count 100

see README.md for the prerequisites and what lands in the results dir.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib
import json
import os
import socket
import subprocess
import sys
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from dataclasses import asdict, dataclass
from pathlib import Path
from types import ModuleType
from typing import Any, Optional, cast

from tiles import Tile, header, render_terminal, write_html

import fuse

HERE = Path(__file__).resolve().parent
# examples/fork_postgres -> examples -> python -> sdks -> repo root
REPO = HERE.parents[3]

# rows the row-touching jobs change. pgbench_accounts packs ~61 rows per 8 KiB
# page, so 250k rows is ~4k heap pages; with the new row versions, index
# entries and wal that comes to roughly 100 MiB of private memory per copy.
ROWS = 250_000


@dataclass
class Job:
    name: str
    # what the tile shows
    short: str
    # each statement runs as its own -c, so a later one failing does not roll
    # back an earlier one: that is what leaves the bad migration half-applied
    statements: list[str]
    # prints "t" once the damage has landed
    verify: str


JOBS = [
    Job(
        "drop table",
        "drop",
        ["DROP TABLE pgbench_accounts CASCADE"],
        "SELECT to_regclass('pgbench_accounts') IS NULL",
    ),
    Job(
        "bad migration",
        "migrate",
        [
            f"UPDATE pgbench_accounts SET filler = 'v2' WHERE aid <= {ROWS}",
            # filler is blank padding, so the cast fails and the migration
            # stops halfway with the update already committed
            "ALTER TABLE pgbench_accounts ALTER COLUMN filler TYPE int USING filler::int",
        ],
        "SELECT count(*) = 10 FROM pgbench_accounts WHERE aid <= 10 AND filler = 'v2'",
    ),
    Job(
        # the small tables: a where-less delete on pgbench_accounts would
        # touch every one of its pages, about 7 GiB of private memory
        "delete without where",
        "delete",
        ["DELETE FROM pgbench_tellers", "DELETE FROM pgbench_branches"],
        "SELECT NOT EXISTS (SELECT 1 FROM pgbench_tellers)"
        " AND NOT EXISTS (SELECT 1 FROM pgbench_branches)",
    ),
    Job(
        "truncate everything",
        "truncate",
        ["TRUNCATE pgbench_accounts, pgbench_branches, pgbench_tellers, pgbench_history"],
        "SELECT NOT EXISTS (SELECT 1 FROM pgbench_accounts)",
    ),
    Job(
        # bid repeats 100000 times, so the build fails and, being concurrent,
        # leaves an invalid index behind
        "bad index",
        "index",
        [
            "CREATE UNIQUE INDEX CONCURRENTLY accounts_bid_uniq"
            f" ON pgbench_accounts (bid) WHERE aid <= {ROWS}"
        ],
        "SELECT NOT indisvalid FROM pg_index WHERE indexrelid = to_regclass('accounts_bid_uniq')",
    ),
    Job(
        # the where meant to pick one account picks a quarter million
        "runaway update",
        "update",
        [f"UPDATE pgbench_accounts SET abalance = -1 WHERE aid <= {ROWS}"],
        f"SELECT count(*) = {ROWS} FROM pgbench_accounts WHERE aid <= {ROWS} AND abalance = -1",
    ),
]

# a full read of the big table. explain analyze reports server-side execution
# time and how many blocks came from shared_buffers (hit) vs the disk (read),
# which is the warm-or-cold verdict itself.
HEAVY_SQL = (
    "EXPLAIN (ANALYZE, TIMING OFF, BUFFERS, FORMAT JSON)"
    " SELECT count(*), sum(abalance) FROM pgbench_accounts"
)

# every row of every table, order independent. md5 of this string is main's
# checksum; the counts stay readable in summary.json.
CHECKSUM_SQL = (
    "SELECT string_agg(r, ' ' ORDER BY r) FROM ("
    + " UNION ALL ".join(
        f"SELECT '{t}:' || count(*) || ':' || coalesce(sum(hashtextextended(x::text, 0)), 0)"
        f" FROM {t} x"
        for t in ("pgbench_accounts", "pgbench_branches", "pgbench_tellers", "pgbench_history")
    )
    + ") s(r)"
)

CLOCK_SKEW_LIMIT_S = 5.0


@dataclass
class Copy:
    index: int
    vm_id: str = ""
    job: str = ""
    job_short: str = ""
    status: str = "forking"
    # fork call start -> the copy reads running
    fork_s: Optional[float] = None
    # fork call start -> the copy answers SELECT now() on a fresh connection
    first_query_s: Optional[float] = None
    # the copy's first full read of pgbench_accounts, server side
    query_ms: Optional[float] = None
    blocks_hit: Optional[int] = None
    blocks_read: Optional[int] = None
    clock_skew_s: Optional[float] = None
    amcheck: Optional[str] = None
    error: str = ""


def log(msg: str) -> None:
    print(f"[fork-postgres] {msg}", file=sys.stderr, flush=True)


def psql(
    client: fuse.Client, vm_id: str, *sql: str, stop_on_error: bool = True, timeout_s: int = 120
) -> fuse.ExecResult:
    # a fresh psql per call is a fresh connection: nothing opened before the
    # fork survives into a copy
    cmd = ["runuser", "-u", "postgres", "--", "psql", "-XAtq", "-d", "postgres"]
    cmd += ["-v", f"ON_ERROR_STOP={1 if stop_on_error else 0}"]
    for s in sql:
        cmd += ["-c", s]
    return client.environments.exec(vm_id, fuse.ExecRequest(cmd=cmd, timeout_ms=timeout_s * 1000))


def checked(r: fuse.ExecResult, what: str) -> str:
    if r.exit_code != 0:
        raise RuntimeError(f"{what}: exit {r.exit_code}: {r.stderr.strip()[-300:]}")
    return r.stdout.strip()


def heavy_query(client: fuse.Client, vm_id: str) -> tuple[float, int, int]:
    out = checked(psql(client, vm_id, HEAVY_SQL, timeout_s=600), "heavy query")
    plan = json.loads(out)[0]
    top = plan["Plan"]
    return (
        float(plan["Execution Time"]),
        int(top.get("Shared Hit Blocks", 0)),
        int(top.get("Shared Read Blocks", 0)),
    )


def checksum(client: fuse.Client, vm_id: str) -> tuple[str, str]:
    raw = checked(psql(client, vm_id, CHECKSUM_SQL, timeout_s=600), "checksum")
    return hashlib.md5(raw.encode()).hexdigest(), raw


def wait_running(client: fuse.Client, vm_id: str, timeout_s: float) -> None:
    deadline = time.monotonic() + timeout_s
    while True:
        env = client.environments.get(vm_id)
        if env.state == fuse.STATE_RUNNING:
            return
        if fuse.is_terminal_state(env.state):
            raise RuntimeError(f"{vm_id} is {env.state}: {env.error}")
        if time.monotonic() >= deadline:
            raise TimeoutError(f"{vm_id} still {env.state} after {timeout_s:.0f}s")
        time.sleep(0.5)


def first_now(client: fuse.Client, vm_id: str, timeout_s: float) -> float:
    """seconds the guest's now() is ahead of ours, once postgres answers."""
    deadline = time.monotonic() + timeout_s
    while True:
        before = time.time()
        try:
            r = psql(client, vm_id, "SELECT extract(epoch FROM now())", timeout_s=30)
        except fuse.ApiError:
            # the guest agent can lag the running state by a moment
            r = None
        after = time.time()
        if r is not None and r.exit_code == 0:
            return float(r.stdout.strip()) - (before + after) / 2
        if time.monotonic() >= deadline:
            detail = r.stderr.strip()[-300:] if r is not None else "exec kept failing"
            raise TimeoutError(f"postgres on {vm_id} did not answer: {detail}")
        time.sleep(0.2)


def fork_live(
    client: fuse.Client, vm_id: str, count: int, key: str, comment: str
) -> list[fuse.EnvironmentInfo]:
    # the live --count fork lands with #422. until then ForkOptions has no
    # live/count fields, and pydantic would silently drop them and run a plain
    # single fork, so refuse instead. the cast is for the same reason: on main
    # fork() is still typed as returning one environment.
    if "count" not in fuse.ForkOptions.model_fields:
        raise SystemExit(
            f"folsom-fuse {fuse.VERSION} has no live fork --count (#422): "
            "run this against an sdk that has it"
        )
    opts = fuse.ForkOptions(live=True, count=count, comment=comment)  # type: ignore[call-arg]
    children = client.environments.fork(vm_id, opts, idempotency_key=key)
    return cast("list[fuse.EnvironmentInfo]", children)


def fuse_cli(argv: list[str], fuse_bin: str, context: str, timeout_s: int) -> str:
    cmd = [fuse_bin] + (["--context", context] if context else []) + argv
    # stderr passes through, so a long `fuse build` shows its progress
    p = subprocess.run(cmd, stdout=subprocess.PIPE, text=True, timeout=timeout_s)
    if p.returncode != 0:
        raise SystemExit(f"{' '.join(cmd)} exited {p.returncode}")
    return p.stdout.strip()


def load_bench(bench_dir: Path) -> tuple[Optional[ModuleType], Optional[ModuleType]]:
    # the host sampler and leak audit live in benchmark/; use them in place
    if not (bench_dir / "leak_audit.py").is_file():
        log(f"no benchmark harness at {bench_dir}: host memory and leak audit are skipped")
        return None, None
    sys.path.insert(0, str(bench_dir))
    return importlib.import_module("sampler"), importlib.import_module("leak_audit")


def pct(values: list[float], p: float) -> Optional[float]:
    # nearest rank, the same rule as benchmark/lib.sh's pct
    if not values:
        return None
    s = sorted(values)
    idx = min(len(s), max(1, int(p / 100 * len(s) + 0.9999)))
    return round(s[idx - 1], 3)


def mem_used_mb(sample: dict[str, Any]) -> Optional[int]:
    total, avail = sample.get("mem_total_kb"), sample.get("mem_available_kb")
    if total is None or avail is None:
        return None
    return int((total - avail) / 1024)


class Run:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.client = fuse.Client(os.environ["FUSE_URL"], os.environ["FUSE_TOKEN"], timeout=660)
        self.run_id = f"fork-postgres-{uuid.uuid4().hex[:6]}"
        self.key = args.key or self.run_id
        self.out = Path(args.out) if args.out else self.default_out()
        self.out.mkdir(parents=True, exist_ok=True)
        self.sampler, self.leak_audit = load_bench(Path(args.bench_dir))
        self.lock = threading.Lock()
        self.stop = threading.Event()
        self.latest: dict[str, Any] = {}
        self.copies: list[Copy] = [Copy(index=0, status="main", job="main", job_short="main")]
        self.t_start = time.monotonic()
        self.t_fork = 0.0
        self.created_main = False
        self.main_id = ""
        self.summary: dict[str, Any] = {"run": self.run_id, "idempotency_key": self.key}

    def default_out(self) -> Path:
        # next to the benchmark results (gitignored), named like lib.sh's
        # default_out; outside a checkout, ./results
        stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
        name = f"fork-postgres-{socket.gethostname().split('.')[0]}-{stamp}"
        base = REPO / "benchmark" / "results"
        if not (REPO / "benchmark").is_dir():
            base = Path("results")
        return base / name

    # host memory, 1 hz, in the same jsonl shape as benchmark/sampler.py

    def sample_loop(self) -> None:
        if self.sampler is None:
            return
        tick = 0
        with open(self.out / "samples.jsonl", "a") as f:
            while not self.stop.is_set():
                s = self.sampler.sample(with_pss=tick % 5 == 0)
                with self.lock:
                    self.latest = s
                f.write(json.dumps(s) + "\n")
                f.flush()
                tick += 1
                self.stop.wait(1.0)

    def mem_now(self) -> dict[str, Any]:
        if self.sampler is None:
            return {}
        s: dict[str, Any] = self.sampler.sample(with_pss=True)
        total, pss = s.get("mem_total_kb"), s.get("fc_pss_kb")
        return {
            "mem_used_mb": mem_used_mb(s),
            "mem_total_mb": None if total is None else total // 1024,
            "fc_pss_mb": None if pss is None else pss // 1024,
            "fc_count": s.get("fc_count"),
        }

    # tile view

    def tiles(self) -> tuple[list[Tile], str]:
        with self.lock:
            rows = [
                Tile(
                    c.index,
                    c.status,
                    c.job,
                    c.job_short,
                    c.fork_s,
                    c.first_query_s,
                    c.query_ms,
                    c.error,
                )
                for c in self.copies
            ]
            mem = dict(self.latest)
        return rows, header(rows, mem, time.monotonic() - self.t_start)

    def tile_loop(self) -> None:
        tty = sys.stdout.isatty()
        last_line = 0.0
        while not self.stop.is_set():
            self.draw(tty, final=False)
            if not tty and time.monotonic() - last_line >= 5:
                print(self.tiles()[1], flush=True)
                last_line = time.monotonic()
            self.stop.wait(1.0)

    def draw(self, tty: bool, final: bool) -> None:
        rows, head = self.tiles()
        if tty:
            sys.stdout.write(render_terminal(rows, head))
            sys.stdout.flush()
        write_html(str(self.out / "tiles.html"), rows, head, final)

    # main

    def boot_main(self) -> str:
        a = self.args
        if a.main:
            wait_running(self.client, a.main, 60)
            return str(a.main)
        build = a.from_build or fuse_cli(
            ["build", "-f", a.fusefile, "--name", "fork-postgres"], a.fuse_bin, a.context, 7200
        )
        log(f"build artifact {build}")
        # the cli refuses --from-build next to an `image:`, since both name the
        # rootfs to boot, so main boots from a copy of the Fusefile without it.
        # the copy sits beside the original, since the cli resolves relative
        # source paths against the file passed with -f
        src = Path(a.fusefile)
        boot = src.with_name(f".{src.name}.{self.run_id}.boot")
        lines = src.read_text().splitlines(keepends=True)
        boot.write_text("".join(ln for ln in lines if not ln.startswith("image:")))
        try:
            out = fuse_cli(
                [
                    "up",
                    "-f",
                    str(boot),
                    "--from-build",
                    build,
                    "--task-id",
                    self.run_id,
                    "--no-wait",
                    "-o",
                    "json",
                ],
                a.fuse_bin,
                a.context,
                900,
            )
        finally:
            boot.unlink(missing_ok=True)
        vm_id = json.loads(out).get("id", "")
        if not vm_id:
            raise SystemExit(f"fuse up printed no environment id: {out[:300]}")
        # recorded before the wait, so main is destroyed even if it never runs
        self.main_id = str(vm_id)
        self.created_main = True
        wait_running(self.client, self.main_id, 600)
        return self.main_id

    def prepare_main(self) -> None:
        main = self.copies[0]
        main.vm_id = self.main_id
        skew = first_now(self.client, self.main_id, 120)
        summary: dict[str, Any] = {"vm_id": self.main_id, "clock_skew_s": round(skew, 3)}
        if self.created_main:
            # a fresh boot: nothing is in shared_buffers or the guest page cache
            cold_ms, hit, read = heavy_query(self.client, self.main_id)
            summary["cold"] = {"query_ms": cold_ms, "blocks_hit": hit, "blocks_read": read}
            log(f"main cold query {cold_ms:.0f} ms ({read} blocks from disk)")
        t = time.monotonic()
        blocks = checked(
            self.client.environments.exec(
                self.main_id,
                fuse.ExecRequest(cmd=["/usr/local/bin/pg-warm"], timeout_ms=600_000),
            ),
            "pg-warm",
        )
        summary["warm_s"] = round(time.monotonic() - t, 3)
        summary["warm_blocks"] = int(blocks or 0)
        warm_ms, hit, read = heavy_query(self.client, self.main_id)
        summary["warm"] = {"query_ms": warm_ms, "blocks_hit": hit, "blocks_read": read}
        main.query_ms = warm_ms
        log(f"main warm query {warm_ms:.0f} ms ({hit} blocks from shared_buffers)")
        md5, raw = checksum(self.client, self.main_id)
        summary["checksum_before"] = {"md5": md5, "tables": raw}
        self.summary["main"] = summary

    # copies

    def run_copy(self, c: Copy, amcheck: bool, heavy: threading.Semaphore) -> None:
        try:
            # readiness is timed for every copy at once; only the heavy work
            # below waits for a --parallel slot, so a queued copy's fork time
            # never includes another copy's jobs
            wait_running(self.client, c.vm_id, 600)
            c.fork_s = round(time.monotonic() - self.t_fork, 3)
            c.status = "up"
            c.clock_skew_s = round(first_now(self.client, c.vm_id, 120), 3)
            c.first_query_s = round(time.monotonic() - self.t_fork, 3)
            with heavy:
                self.wreck_copy(c, amcheck)
        except Exception as e:  # one copy failing must not stop the others
            c.status = "failed"
            c.error = f"{type(e).__name__}: {e}"

    def wreck_copy(self, c: Copy, amcheck: bool) -> None:
        c.query_ms, c.blocks_hit, c.blocks_read = heavy_query(self.client, c.vm_id)
        if amcheck:
            c.status = "checking"
            r = self.client.environments.exec(
                c.vm_id,
                fuse.ExecRequest(
                    cmd=["runuser", "-u", "postgres", "--", "pg_amcheck", "-d", "postgres"],
                    timeout_ms=600_000,
                ),
            )
            c.amcheck = "ok" if r.exit_code == 0 else "failed"
            if r.exit_code != 0:
                c.error = f"pg_amcheck: {(r.stdout + r.stderr).strip()[-300:]}"
        c.status = "wrecking"
        j = JOBS[(c.index - 1) % len(JOBS)]
        job = psql(self.client, c.vm_id, *j.statements, stop_on_error=False, timeout_s=600)
        landed = checked(psql(self.client, c.vm_id, j.verify), "verify") == "t"
        if not landed:
            c.error = c.error or f"job did not land: {job.stderr.strip()[-300:]}"
        c.status = "wrecked" if landed and c.amcheck != "failed" else "failed"

    def fork_and_wreck(self) -> None:
        a = self.args
        self.summary["mem_before_fork"] = self.mem_now()
        self.t_fork = time.monotonic()
        children = fork_live(self.client, self.main_id, a.count, self.key, self.key)
        fork_call_s = round(time.monotonic() - self.t_fork, 3)
        self.summary["fork_call_s"] = fork_call_s
        log(f"fork call returned {len(children)} children in {fork_call_s:.1f}s")
        with self.lock:
            for i, env in enumerate(children, start=1):
                j = JOBS[(i - 1) % len(JOBS)]
                c = Copy(index=i, vm_id=env.id, job=j.name, job_short=j.short)
                if env.state == fuse.STATE_FAILED:
                    c.status, c.error = "failed", env.error or "fork failed"
                self.copies.append(c)
        # amcheck an evenly spaced sample of the copies, before their jobs
        n = len(children)
        sample = set(range(1, n + 1, max(1, n // a.amcheck))[: a.amcheck]) if a.amcheck else set()
        heavy = threading.Semaphore(a.parallel or max(1, n))
        pool = ThreadPoolExecutor(max_workers=max(1, n))
        try:
            futures = [
                pool.submit(self.run_copy, c, c.index in sample, heavy)
                for c in self.copies[1:]
                if c.status != "failed"
            ]
            for f in futures:
                f.result()
        finally:
            # on ctrl-c, do not wait for every copy before tearing down
            pool.shutdown(wait=False, cancel_futures=True)
        self.summary["mem_after_jobs"] = self.mem_now()

    # teardown

    def teardown(self) -> None:
        ids = [c.vm_id for c in self.copies[1:] if c.vm_id]
        if ids:
            log(f"destroying {len(ids)} copies")

        def destroy(vm_id: str) -> None:
            try:
                self.client.environments.destroy(vm_id)
            except fuse.ApiError as e:
                if not fuse.is_not_found(e):
                    log(f"destroy {vm_id}: {e}")

        def delete_snapshot(snapshot_id: str) -> None:
            try:
                self.client.snapshots.delete(snapshot_id)
            except fuse.ApiError as e:
                if not fuse.is_not_found(e):
                    log(f"delete snapshot {snapshot_id}: {e}")

        def delete_lineage(vm_id: str) -> set[str]:
            # each copy's fork-seed-* lineage record outlives its vm and is a
            # child of the seed, so it has to go before the seed can. its
            # parent names the seed this run's fork took.
            seeds = set()
            for s in self.client.snapshots.list(vm_id=vm_id):
                if s.parent_snapshot_id:
                    seeds.add(s.parent_snapshot_id)
                delete_snapshot(s.id)
            return seeds

        with ThreadPoolExecutor(max_workers=16) as pool:
            list(pool.map(destroy, ids))
            seeds = set().union(*pool.map(delete_lineage, ids))
        # only the seed of this run's fork, never another snapshot of main
        for seed in seeds:
            delete_snapshot(seed)

    def audit(self, baseline: Optional[dict[str, Any]]) -> None:
        if self.leak_audit is None or baseline is None:
            self.summary["leak_audit"] = None
            return
        res: dict[str, Any] = self.leak_audit.check(baseline, self.args.settle)
        with open(self.out / "leak.json", "w") as f:
            json.dump(res, f, indent=2)
        self.summary["leak_audit"] = {
            "ok": res["ok"],
            "deltas": res["deltas"],
            "lost": res["lost"],
            "skipped": res["skipped"],
        }
        log("leak audit: " + ("clean" if res["ok"] else f"leaked {res['deltas']} {res['lost']}"))

    def summarize(self) -> None:
        forks = self.copies[1:]
        ok = [c for c in forks if c.first_query_s is not None]

        def p50_p99(vals: list[float]) -> dict[str, Optional[float]]:
            return {"p50": pct(vals, 50), "p99": pct(vals, 99)}

        skews = [abs(c.clock_skew_s) for c in ok if c.clock_skew_s is not None]
        self.summary.update(
            copies=len(forks),
            copies_up=len(ok),
            copies_wrecked=sum(1 for c in forks if c.status == "wrecked"),
            copies_failed=sum(1 for c in forks if c.status == "failed"),
            fork_s=p50_p99([c.fork_s for c in ok if c.fork_s is not None]),
            fork_to_first_query_s=p50_p99(
                [c.first_query_s for c in ok if c.first_query_s is not None]
            ),
            first_heavy_query_ms=p50_p99([c.query_ms for c in ok if c.query_ms is not None]),
            first_heavy_query_blocks_read_max=max(
                (c.blocks_read for c in ok if c.blocks_read is not None), default=None
            ),
            # a copy that read any block from disk on its first heavy query did
            # not come up warm
            copies_cold=sum(1 for c in ok if c.blocks_read),
            clock_skew_max_s=max(skews, default=None),
            clock_ok=all(s <= CLOCK_SKEW_LIMIT_S for s in skews),
            amcheck={
                "sampled": sum(1 for c in forks if c.amcheck is not None),
                "failed": sum(1 for c in forks if c.amcheck == "failed"),
            },
        )
        with open(self.out / "copies.json", "w") as f:
            json.dump([asdict(c) for c in self.copies], f, indent=2)
        with open(self.out / "summary.json", "w") as f:
            json.dump(self.summary, f, indent=2)

    def main(self) -> int:
        log(f"run {self.run_id}, results in {self.out}")
        try:
            return self.run()
        finally:
            # a main this run created goes on every exit path, failures too
            if self.created_main and not self.args.keep_main:
                try:
                    self.client.environments.destroy(self.main_id)
                except fuse.ApiError as e:
                    log(f"destroy main {self.main_id}: {e}")

    def run(self) -> int:
        a = self.args
        self.main_id = self.boot_main()
        log(f"main is {self.main_id}")
        self.prepare_main()
        # baseline with main up: after the copies are gone the host should be
        # back to exactly this
        baseline = self.leak_audit.snapshot() if self.leak_audit else None
        if baseline is not None:
            with open(self.out / "leak-before.json", "w") as f:
                json.dump(baseline, f, indent=2)

        threads = [threading.Thread(target=self.sample_loop, daemon=True)]
        if not a.no_tiles:
            threads.append(threading.Thread(target=self.tile_loop, daemon=True))
        for t in threads:
            t.start()
        try:
            self.fork_and_wreck()
            md5, raw = checksum(self.client, self.main_id)
            before = self.summary["main"]["checksum_before"]["md5"]
            self.summary["main"]["checksum_after"] = {"md5": md5, "tables": raw}
            self.summary["main"]["untouched"] = md5 == before
            self.copies[0].status = "main" if md5 == before else "failed"
        finally:
            self.stop.set()
            for t in threads:
                t.join()
            if not a.no_tiles:
                self.draw(sys.stdout.isatty(), final=True)
            try:
                self.teardown()
                self.audit(baseline)
            finally:
                self.summarize()

        s = self.summary
        log(
            f"{s['copies_up']}/{s['copies']} copies up, fork to first query p50 "
            f"{s['fork_to_first_query_s']['p50']}s, first heavy query p50 "
            f"{s['first_heavy_query_ms']['p50']} ms, main untouched: {s['main']['untouched']}"
        )
        leak = s["leak_audit"]
        ok = (
            s["copies_failed"] == 0
            and s["copies_cold"] == 0
            and s["main"]["untouched"]
            and s["clock_ok"]
            and (leak is None or leak["ok"])
        )
        return 0 if ok else 1


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0] if __doc__ else None)
    ap.add_argument("--count", type=int, default=100, help="copies to fork (default 100)")
    ap.add_argument(
        "--key",
        default="",
        help="Idempotency-Key for the fork; rerun with the same key and --main "
        "to get the same children back (default: the run id)",
    )
    ap.add_argument("--main", default="", help="use this running environment as main")
    ap.add_argument(
        "--from-build",
        default="",
        help="boot main from this `fuse build` artifact instead of building",
    )
    ap.add_argument("--fusefile", default=str(HERE / "Fusefile"))
    ap.add_argument("--fuse-bin", default="fuse", help="cli used for build and up")
    ap.add_argument("--context", default="", help="cli context, if not the active one")
    ap.add_argument("--amcheck", type=int, default=5, help="copies to run pg_amcheck on")
    ap.add_argument(
        "--parallel",
        type=int,
        default=0,
        help="copies running their heavy work at once (default: all of them)",
    )
    ap.add_argument("--out", default="", help="results dir (default benchmark/results/...)")
    ap.add_argument(
        "--bench-dir",
        default=str(REPO / "benchmark"),
        help="where sampler.py and leak_audit.py live",
    )
    ap.add_argument(
        "--settle", type=float, default=60, help="seconds the leak audit waits for counts to return"
    )
    ap.add_argument("--keep-main", action="store_true", help="leave main running at the end")
    ap.add_argument("--no-tiles", action="store_true", help="no terminal grid or tiles.html")
    args = ap.parse_args(argv)
    if args.count < 1:
        ap.error("--count must be at least 1")
    if args.main and args.from_build:
        ap.error("--main and --from-build are mutually exclusive")
    return args


if __name__ == "__main__":
    sys.exit(Run(parse_args()).main())
