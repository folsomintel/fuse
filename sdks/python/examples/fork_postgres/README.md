# fork a live postgres into 100 warm copies

One running Postgres with about 8 GiB of data is copied live N times (100 by
default) in a single fork call. Every copy resumes from the same memory image,
so it answers its first query at warm-cache speed. Each copy then wrecks itself
a different way, and the original keeps running untouched the whole time.

This is the example for #427 (epic #425). It needs the live `--count` fork from
#422: on an SDK without it, the driver stops with an error before it forks.

## Files

- `Fusefile`: Postgres 15 on Debian 12 with `pgbench -i -s 550` loaded (about
  8 GiB of table and index) and `pg-warm`, which reads the whole dataset into
  `shared_buffers`.
- `fork_postgres.py`: the driver (python SDK).
- `tiles.py`: the tile view, as a terminal grid and as `tiles.html`.

## Prerequisites

- A Firecracker host whose snapshot store is on xfs or btrfs. On ext4 every
  copy gets a full copy of the 20 GB disk (see #423's `benchmark/setup.sh`).
- A 20G Debian 12 rootfs baked on that host. `resources.disk` only reserves
  space and never grows the guest disk, and the default image is 4G:

  ```bash
  cd host-agent/firecracker
  FC_ROOTFS_SIZE=20G ./scripts/fc-bake-distro-rootfs.sh docker.io/library/debian:12 debian-12-pg
  ```

- Host capacity for N+1 environments of 1 vCPU, 12 GB of memory and 20 GB of
  disk, as the scheduler counts them. The memory only fits once #420 counts
  the shared image once. Every copy still reserves the full 20 GB of disk.
- The `fuse` CLI logged in to the same orchestrator that `FUSE_URL` points at
  (or pass `--context`). The driver uses the CLI to build the Fusefile and boot
  main from it, and the SDK for everything else.
- Run it on the host, as root, from a fuse checkout. The host memory sampler
  and the leak audit (`benchmark/sampler.py`, `benchmark/leak_audit.py`) read
  `/proc`, the agent's state dir and the orchestrator database. Off the host,
  pass `--bench-dir ''` to skip both.

## Run it

```bash
export FUSE_URL=http://127.0.0.1:8080 FUSE_TOKEN=<master token>
cd sdks/python
uv run python examples/fork_postgres/fork_postgres.py              # 100 copies
uv run python examples/fork_postgres/fork_postgres.py --count 4    # a quick check first
```

The first run spends several minutes in `fuse build` loading the data. The
Fusefile turns on the layer cache, so later builds are a lookup. Other flags:

- `--from-build <id>` boots main from an artifact you already built.
- `--main <id>` uses an environment that is already running as main and skips
  the cold query.
- `--key <key>` sets the fork's Idempotency-Key. Running again with the same
  key, `--count` and `--main` returns the same children instead of new ones.
- `--keep-main` leaves main running at the end.
- `--amcheck N` sets how many copies get `pg_amcheck`. The default is 5.

## What it does

1. Builds the Fusefile and boots main from the artifact.
2. Times a full read of `pgbench_accounts` on main while its cache is cold,
   runs `pg-warm`, then times the same read warm. `EXPLAIN (ANALYZE, BUFFERS)`
   reports how many blocks came from `shared_buffers` and how many from disk.
3. Checksums every row of main, then takes the leak audit baseline.
4. Forks main with `live=True, count=N` under one Idempotency-Key. Main is
   copy 0 and is never stopped.
5. On each copy it opens a fresh connection and runs `SELECT now()` (fork to
   first query, plus clock skew against the host), then does the full read
   (the copy's first heavy query, which should be warm). It runs `pg_amcheck`
   on an evenly spaced sample, and then one destructive job per copy, in
   rotation: drop table, a bad migration left half applied, delete without
   where (on the small tables), truncate everything, a unique index build
   that fails and leaves an invalid index, and a runaway update. Each job
   that touches rows changes 250k of them, which is roughly 100 MiB of private
   memory per copy.
6. Checksums main again and compares the two checksums.
7. Destroys every copy, deletes the live seed snapshot the fork took of main,
   and runs the leak audit against the baseline.

The run exits 1 if any copy failed, any copy read a block from disk on its
first heavy query (it came up cold), main changed, a copy's clock was more than
5 s off, or the leak audit found something.

## What it writes

Results go to `benchmark/results/fork-postgres-<host>-<utc>/`, next to the
other benchmark results (gitignored), or to `--out`:

- `summary.json`: copy counts, fork call time, fork and fork to first query
  p50/p99, first heavy query p50/p99 against main's cold and warm times, max
  clock skew, `pg_amcheck` results, host memory before the fork and after the
  jobs, main's checksums, and the leak audit verdict.
- `copies.json`: one record per copy.
- `samples.jsonl`: host memory at 1 Hz, in `benchmark/sampler.py`'s format.
- `leak-before.json`, `leak.json`: the leak audit baseline and result.
- `tiles.html`: the tile view. It reloads every 2 s until the run ends.

The terminal grid shows one tile per copy (copy number, fork time, first
heavy query, job), colored by state: blue up, yellow checking or wrecking, red
wrecked, purple failed, green for main. Host memory and the VM count are on
the header line. Copies placed on other hosts don't show up in the host memory
numbers.
