#!/usr/bin/env bash
# fork fan-out: n = 1..max children of one heavy live vm.
#
# one parent boots from Fusefile.heavy and dirties HEAVY_MB of guest memory.
# for each n in LEVELS, n children are forked at once from one seed snapshot;
# per child it records the fork call, and the time from the level's start to
# the first exec that succeeds. per level it records host memory before and
# after (MemAvailable drop, firecracker pss). children are destroyed between
# levels; the parent, the seed and every vm are gone before the leak audit.
#
# each child reports whether /dev/shm/heavy survived: "live" means it resumed
# the parent's memory, "cold" means it booted the rootfs (today's fork, until
# #422 allows a live seed).
#
#   bash benchmark/fork-fanout/fanout.sh
#   SMOKE=1 bash benchmark/fork-fanout/fanout.sh     # about a minute
#
# scale knobs (SMOKE=1 / full):
#   LEVELS       children per level ("1 2" / "1 2 4 8 16 32 64 128 256")
#   HEAVY_MB     memory the parent dirties (64 / 1536)
# other:
#   SEED         disk: one snapshot, children fork with --reuse-snapshot (default)
#                live: same with a --live snapshot (needs #420/#422)
#                fresh: plain fork, every child takes its own snapshot
#   MEM_FLOOR_PCT (10), ERR_LIMIT (0.05)
#   HOST, PARENT_FUSEFILE, OUT, AUDIT_SETTLE, PSS_EVERY
set -uo pipefail
# shellcheck source=benchmark/lib.sh
. "$(dirname "$0")/../lib.sh"

scale LEVELS "1 2" "1 2 4 8 16 32 64 128 256"
scale HEAVY_MB 64 1536
SEED="${SEED:-disk}"
MEM_FLOOR_PCT="${MEM_FLOOR_PCT:-10}"
ERR_LIMIT="${ERR_LIMIT:-0.05}"
HOST="${HOST:-local-fc}"
PARENT_FUSEFILE="${PARENT_FUSEFILE:-$(dirname "$0")/Fusefile.heavy}"
OUT="${OUT:-$(default_out fork-fanout)}"
RUN="fanout$(date +%s)"

case "$SEED" in disk|live|fresh) ;; *) echo "SEED must be disk, live or fresh" >&2; exit 2;; esac

bench_start "$OUT" fork-fanout || exit 1
IDS="$OUT/ids.txt"          # children, all levels
: > "$IDS"
: > "$OUT/children.jsonl"
: > "$OUT/levels.jsonl"
parent=""

# snapshot ids taken of the parent: the seed, plus one per child with SEED=fresh
parent_snapshots() {
  fuse snapshot list --vm-id "$parent" --all-pages -o json 2>/dev/null \
    | jq -r '(if type=="array" then . else (.items // .snapshots // []) end)[] | (.id // .snapshot_id)'
}

finish() {
  trap - EXIT INT TERM
  wait_jobs
  destroy_all "$IDS"
  if [ -n "$parent" ]; then
    for s in $(parent_snapshots); do fuse snapshot delete --yes "$s" >/dev/null 2>&1 || true; done
    fuse environment destroy --yes "$parent" >/dev/null 2>&1 || true
  fi
  bench_finish
  local rc=$?
  bench_py report.py fanout "$OUT"
  exit "$rc"
}
trap finish EXIT INT TERM

# wait_snapshot <id>: until the snapshot is ready (bounded)
wait_snapshot() {
  local deadline=$(( $(date +%s) + 900 )) s
  while [ "$(date +%s)" -lt "$deadline" ]; do
    s=$(fuse snapshot get "$1" -o json 2>/dev/null | jq -r '.state // empty')
    case "$s" in ready) return 0;; error) return 1;; esac
    sleep 1
  done
  return 1
}

child() { # child <n> <k> <level-start>
  local n="$1" k="$2" t0="$3" out id t_f f=null x=null mem="" ok=false
  if [ "$SEED" = fresh ]; then
    out=$(fuse environment fork "$parent" --comment "$RUN-$n-$k" -o json 2>>"$OUT/err.log")
  else
    out=$(fuse environment fork "$parent" --reuse-snapshot "$seed" --comment "$RUN-$n-$k" -o json 2>>"$OUT/err.log")
  fi
  t_f=$(now)
  id=$(printf '%s' "$out" | id_of)
  if [ -n "$id" ]; then
    echo "$id" >> "$IDS"
    f=$(dt "$t0" "$t_f")
    if wait_running "$id" 300; then
      # first exec that works: the guest agent may lag the running state
      local deadline=$(( $(date +%s) + 120 ))
      while [ "$(date +%s)" -lt "$deadline" ]; do
        if mem=$(fuse environment exec "$id" --timeout 30s --shell 'test -f /dev/shm/heavy && echo live || echo cold' 2>/dev/null); then
          x=$(dt "$t0" "$(now)"); ok=true
          break
        fi
        sleep 0.2
      done
    fi
  fi
  op_line "$OUT/children.jsonl" "\"n\":$n,\"k\":$k,\"id\":\"$id\",\"fork_s\":$f,\"first_exec_s\":$x,\"memory\":\"$(printf '%s' "$mem" | tr -dc '[:lower:]')\",\"ok\":$ok"
}

echo "fork fan-out  levels=($LEVELS) heavy=${HEAVY_MB}MB seed=$SEED host=$HOST out=$OUT"
out=$(fuse up "$PARENT_FUSEFILE" --task-id "$RUN-parent" --host "$HOST" -o json 2>>"$OUT/err.log")
parent=$(printf '%s' "$out" | id_of)
if [ -z "$parent" ] || ! wait_running "$parent"; then
  echo "parent did not start (see $OUT/err.log)" >&2; exit 1
fi
fuse environment exec "$parent" --timeout 10m \
  --shell "dd if=/dev/urandom of=/dev/shm/heavy bs=1M count=$HEAVY_MB status=none" >/dev/null 2>>"$OUT/err.log" \
  || { echo "could not dirty parent memory" >&2; exit 1; }
echo "  parent $parent holds ${HEAVY_MB}MB in /dev/shm"

seed=""
if [ "$SEED" != fresh ]; then
  live_flag=""
  [ "$SEED" = live ] && live_flag=--live
  # shellcheck disable=SC2086 # empty live_flag must vanish
  seed=$(fuse snapshot create "$parent" $live_flag --comment "$RUN-seed" -o json 2>>"$OUT/err.log" | id_of)
  if [ -z "$seed" ] || ! wait_snapshot "$seed"; then
    echo "seed snapshot failed (see $OUT/err.log)" >&2; exit 1
  fi
  echo "  seed snapshot $seed ($SEED)"
fi

for n in $LEVELS; do
  before=$(host_sample)
  t0=$(now)
  for k in $(seq 1 "$n"); do child "$n" "$k" "$t0" & done
  wait_jobs
  after=$(host_sample)
  jq -nc --argjson n "$n" --argjson b "$before" --argjson a "$after" \
    '{n:$n, mem_available_before_kb:$b.mem_available_kb, mem_available_after_kb:$a.mem_available_kb,
      mem_total_kb:$a.mem_total_kb, fc_count_after:$a.fc_count, fc_pss_after_kb:$a.fc_pss_kb,
      psi_mem_some_after:$a.psi_mem_some}' >> "$OUT/levels.jsonl"
  errors=$(jq -r --argjson n "$n" 'select(.n==$n and (.ok|not)) | .k' "$OUT/children.jsonl" | wc -l | tr -d ' ')
  avail=$(printf '%s' "$after" | jq -r '.mem_available_kb // empty')
  total=$(printf '%s' "$after" | jq -r '.mem_total_kb // empty')
  echo "  n=$n: errors=$errors avail=${avail:-NA}kB"

  destroy_all "$IDS"
  : > "$IDS"
  if [ "$SEED" = fresh ]; then
    # each fresh fork left a snapshot of the parent behind
    for s in $(parent_snapshots); do fuse snapshot delete --yes "$s" >/dev/null 2>&1 || true; done
  fi

  if awk -v e="$errors" -v n="$n" -v l="$ERR_LIMIT" 'BEGIN{exit !(e/n > l)}'; then echo "stop: errors $errors/$n"; break; fi
  if [ -n "$avail" ] && [ -n "$total" ] && [ "$avail" -lt $(( total * MEM_FLOOR_PCT / 100 )) ]; then echo "stop: MemAvailable below ${MEM_FLOOR_PCT}%"; break; fi
done
