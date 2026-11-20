#!/usr/bin/env bash
# test-smoke.sh: run every suite in SMOKE=1 mode against fake-fuse, check the
# outputs, then check that a leaked vm fails the run. needs bash, jq, python3;
# no host, no orchestrator.
#
#   bash benchmark/tests/test-smoke.sh
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
BENCH="$(cd "$HERE/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
ln -s "$HERE/fake-fuse" "$TMP/bin/fuse"
export PATH="$TMP/bin:$PATH"
export FC_STATE_DIR="$TMP/agent-state" SNAPSHOTS_DIR="$TMP/agent-state/snapshots"
export BENCH_NETNS_DIR="$TMP/netns" RESTATE_ADMIN_URL="http://127.0.0.1:1" BENCH_ORCH_DEFAULTS="$TMP/none"
export SMOKE=1 AUDIT_SETTLE=2 FAKE_DELAY=0.05 HOST=fake
unset DATABASE_URL

fails=0
pass() { echo "  ok   $*"; }
fail() { echo "  FAIL $*"; fails=$((fails + 1)); }

expect_files() { # expect_files <out> <file...>
  local out="$1"; shift
  for f in "$@"; do
    if [ -s "$out/$f" ]; then pass "$(basename "$out")/$f"; else fail "$(basename "$out")/$f missing or empty"; fi
  done
}

run_suite() { # run_suite <name> <script> <expected rc> [env...]
  local name="$1" script="$2" want="$3"; shift 3
  local out="$TMP/$name" rc
  echo "== $name"
  env "$@" OUT="$out" bash "$BENCH/$script" > "$TMP/$name.log" 2>&1
  rc=$?
  if [ "$rc" = "$want" ]; then pass "exit $rc"; else fail "exit $rc, want $want (log: $(cat "$TMP/$name.log"))"; fi
}

is() { # is <got> <want> <desc>
  if [ "$1" = "$2" ]; then pass "$3"; else fail "$3 (got $1, want $2)"; fi
}
has() { # has <pattern> <file> <desc>
  if grep -q "$1" "$2"; then pass "$3"; else fail "$3"; fi
}

run_suite ramp density/ramp.sh 0
expect_files "$TMP/ramp" env.json samples.jsonl batches.jsonl ops.jsonl leak.json summary.md ramp-memory.svg ramp-latency.svg
is "$(jq -r .ok "$TMP/ramp/leak.json")" true "ramp leak audit clean"
is "$(jq -s length "$TMP/ramp/batches.jsonl")" 2 "ramp ran 2 batches (smoke: 2 per batch up to 4)"
is "$(jq -c '[.smoke, .suite]' "$TMP/ramp/env.json")" '[true,"density-ramp"]' "env.json marks the suite and smoke"
has "stop: max vms 4" "$TMP/ramp/summary.md" "ramp stopped at MAX_VMS"

run_suite churn density/churn.sh 0
expect_files "$TMP/churn" arrivals.jsonl leak.json summary.md churn-latency.svg churn-errors.svg
# 1/s and 2/s for 10 s
is "$(jq -s length "$TMP/churn/arrivals.jsonl")" 30 "churn scheduled 30 arrivals"
is "$(jq -s 'map(select(.ok)) | length' "$TMP/churn/arrivals.jsonl")" 30 "every arrival completed"

run_suite fanout fork-fanout/fanout.sh 0
expect_files "$TMP/fanout" children.jsonl levels.jsonl leak.json summary.md fanout-exec.svg fanout-memory.svg
is "$(jq -s 'map(select(.ok)) | length' "$TMP/fanout/children.jsonl")" 3 "fanout forked 1 + 2 children"
is "$(find "$SNAPSHOTS_DIR" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')" 0 "seed snapshot deleted"

run_suite fanout-fresh fork-fanout/fanout.sh 0 SEED=fresh
is "$(jq -r .ok "$TMP/fanout-fresh/leak.json")" true "fresh forks' snapshots cleaned"

# a destroy that silently keeps the vm must fail the run
run_suite leak density/ramp.sh 1 FAKE_LEAK_DESTROY=1
is "$(jq -r '.deltas.vm_dirs' "$TMP/leak/leak.json")" 4 "leak audit saw 4 leaked vm dirs"
has "leak audit: FAILED" "$TMP/leak/summary.md" "summary reports the leak"

# exec failures stop the ramp after the first batch
run_suite exec-fail density/ramp.sh 0 FAKE_FAIL_EXEC=1
has "stop: errors" "$TMP/exec-fail/summary.md" "ramp stops on errors"

echo
if [ "$fails" = 0 ]; then echo "all smoke checks passed"; else echo "$fails smoke check(s) failed"; exit 1; fi

