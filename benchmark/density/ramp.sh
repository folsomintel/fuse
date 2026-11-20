#!/usr/bin/env bash
# density ramp: create vms in batches until memory or latency gives out.
#
# after each batch it records the live count, MemAvailable, firecracker pss,
# psi, and create and exec p50/p99, then stops at the first limit it hits.
# every vm is destroyed at the end and the leak audit fails the run on any
# delta.
#
#   bash benchmark/density/ramp.sh               # full ramp
#   SMOKE=1 bash benchmark/density/ramp.sh       # about a minute, same code path
#
# scale knobs (SMOKE=1 / full):
#   BATCH            vms per batch (2 / 25)
#   MAX_VMS          stop at this many live vms (4 / 5000)
# limits:
#   MEM_FLOOR_PCT    stop when MemAvailable drops below this % of MemTotal (10)
#   CREATE_P99_LIMIT stop when a batch's create p99 passes this, seconds (60)
#   EXEC_P99_LIMIT   stop when a batch's exec p99 passes this, seconds (10)
#   ERR_LIMIT        stop when this fraction of a batch's creates and execs fail (0.05)
# other:
#   EXEC_SAMPLE      vms exec'd per batch, drawn from all live ones (32)
#   FUSEFILE, OUT, AUDIT_SETTLE, PSS_EVERY
set -uo pipefail
# shellcheck source=benchmark/lib.sh
. "$(dirname "$0")/../lib.sh"

scale BATCH 2 25
scale MAX_VMS 4 5000
MEM_FLOOR_PCT="${MEM_FLOOR_PCT:-10}"
CREATE_P99_LIMIT="${CREATE_P99_LIMIT:-60}"
EXEC_P99_LIMIT="${EXEC_P99_LIMIT:-10}"
ERR_LIMIT="${ERR_LIMIT:-0.05}"
EXEC_SAMPLE="${EXEC_SAMPLE:-32}"
FUSEFILE="${FUSEFILE:-$BENCH_LIB_DIR/Fusefile.bench}"
OUT="${OUT:-$(default_out density-ramp)}"
RUN="ramp$(date +%s)"

bench_start "$OUT" density-ramp || exit 1
IDS="$OUT/ids.txt"
: > "$IDS"
: > "$OUT/ops.jsonl"
: > "$OUT/batches.jsonl"
: > "$OUT/stop.jsonl"

finish() {
  trap - EXIT INT TERM
  echo "destroying $(wc -l < "$IDS" | tr -d ' ') vm(s)..."
  destroy_all "$IDS"
  bench_finish
  local rc=$?
  bench_py report.py ramp "$OUT"
  exit "$rc"
}
trap finish EXIT INT TERM

create_one() { # create_one <batch> <i>
  local t0 t1 id ok=false
  t0=$(now)
  id=$(up_env "$FUSEFILE" --task-id "$RUN-$1-$2")
  if [ -n "$id" ]; then
    echo "$id" >> "$IDS"
    wait_running "$id" && ok=true
  fi
  t1=$(now)
  op_line "$OUT/ops.jsonl" "\"batch\":$1,\"op\":\"create\",\"id\":\"$id\",\"s\":$(dt "$t0" "$t1"),\"ok\":$ok"
}

exec_one() { # exec_one <batch> <id>
  local s ok=true
  s=$(exec_probe "$2") || { ok=false; s=null; }
  op_line "$OUT/ops.jsonl" "\"batch\":$1,\"op\":\"exec\",\"id\":\"$2\",\"s\":$s,\"ok\":$ok"
}

# batch_values <batch> <op>: successful latencies of one op in one batch
batch_values() {
  jq -r --argjson b "$1" --arg op "$2" 'select(.batch==$b and .op==$op and .ok) | .s' "$OUT/ops.jsonl"
}
batch_p99() {
  local f
  f=$(mktemp)
  batch_values "$1" "$2" | sort -n > "$f"
  pct 99 "$f"
  rm -f "$f"
}

live_ids() { jq -r 'select(.op=="create" and .ok) | .id' "$OUT/ops.jsonl"; }

stop() { op_line "$OUT/stop.jsonl" "\"batch\":$1,\"reason\":\"$2\""; echo "stop: $2"; }

# shellcheck disable=SC2153 # BATCH is set by scale
echo "density ramp  batch=$BATCH max=$MAX_VMS out=$OUT"
batch=0
while :; do
  batch=$((batch + 1))
  for i in $(seq 1 "$BATCH"); do create_one "$batch" "$i" & done
  wait_jobs
  # exec into a sample of every live vm, not just the new ones, so a slowdown
  # of the vms already running shows up too.
  while read -r id; do
    exec_one "$batch" "$id" < /dev/null &
  done < <(live_ids | awk -v n="$EXEC_SAMPLE" 'BEGIN{srand()} {a[NR]=$0}
    END{for(i=NR;i>0&&k<n;i--){j=int(rand()*i)+1; print a[j]; a[j]=a[i]; k++}}')
  wait_jobs

  live=$(live_ids | wc -l | tr -d ' ')
  sample=$(host_sample)
  printf '%s\n' "$sample" | jq -c --argjson b "$batch" --argjson live "$live" '{batch:$b, live:$live} + .' >> "$OUT/batches.jsonl"

  read -r ops errors < <(jq -rs --argjson b "$batch" \
    '[.[] | select(.batch==$b)] | "\(length) \(map(select(.ok|not)) | length)"' "$OUT/ops.jsonl")
  cp99=$(batch_p99 "$batch" create)
  ep99=$(batch_p99 "$batch" exec)
  avail=$(printf '%s' "$sample" | jq -r '.mem_available_kb // empty')
  total=$(printf '%s' "$sample" | jq -r '.mem_total_kb // empty')
  echo "  batch $batch: live=$live errors=$errors create_p99=${cp99}s exec_p99=${ep99}s avail=${avail:-NA}kB"

  if awk -v e="$errors" -v n="$ops" -v l="$ERR_LIMIT" 'BEGIN{exit !(n > 0 && e/n > l)}'; then stop "$batch" "errors $errors/$ops ops"; break; fi
  if [ "$cp99" != NA ] && awk -v a="$cp99" -v l="$CREATE_P99_LIMIT" 'BEGIN{exit !(a > l)}'; then stop "$batch" "create p99 ${cp99}s"; break; fi
  if [ "$ep99" != NA ] && awk -v a="$ep99" -v l="$EXEC_P99_LIMIT" 'BEGIN{exit !(a > l)}'; then stop "$batch" "exec p99 ${ep99}s"; break; fi
  if [ -n "$avail" ] && [ -n "$total" ] && [ "$avail" -lt $(( total * MEM_FLOOR_PCT / 100 )) ]; then stop "$batch" "MemAvailable below ${MEM_FLOOR_PCT}%"; break; fi
  if [ "$live" -ge "$MAX_VMS" ]; then stop "$batch" "max vms $MAX_VMS"; break; fi
done
