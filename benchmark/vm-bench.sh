#!/usr/bin/env bash
# fuse vm lifecycle benchmark
# measures real end-to-end wall-clock latency for the ops people care about:
#   cold create (fuse up, base image + setup + boot -> running)
#   warm create (fuse up --from-build, boot from a build artifact, skips setup)
#   fork        (copy-on-write clone of a running env)
#   destroy
# reports min / p50 / p90 / p95 / p99 / max / mean per op, plus the orchestrator's
# own server-side per-step breakdown (where the time goes).
#
# run this on the benchmark host (it uses the local fuse context). nothing is
# left behind: every env created is destroyed at the end (and on ctrl-c). it
# exits non-zero if any create, fork or destroy failed.
#
#   N=20 bash vm-bench.sh                 # 20 iterations each (default 10)
#   FUSEFILE=/path/to/Fusefile bash vm-bench.sh
#   FROM_BUILD=snap-...  bash vm-bench.sh # also benchmark warm boot from this build snapshot
set -uo pipefail
# shellcheck source=benchmark/lib.sh
. "$(dirname "$0")/lib.sh"

N="${N:-10}"
FUSEFILE="${FUSEFILE:-$BENCH_LIB_DIR/Fusefile.bench}"
FROM_BUILD="${FROM_BUILD:-}"          # optional build-snapshot id for the warm-boot test
OUT="${OUT:-$HOME/fuse-bench-$(date +%s)}"
METRICS_URL="${METRICS_URL:-http://127.0.0.1:8080/metrics}"
mkdir -p "$OUT"

created=()   # ids to clean up
cleanup() {
  echo "cleaning up ${#created[@]} env(s)..."
  for id in ${created[@]+"${created[@]}"}; do
    [ -n "$id" ] && fuse environment destroy --yes "$id" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT INT TERM

report_row() { # report_row <label> <file>
  local f="$2"; [ -s "$f" ] || { printf "| %-12s | no samples |\n" "$1"; return; }
  sort -n "$f" -o "$f"
  local n min max mean
  n=$(wc -l <"$f" | tr -d ' '); min=$(head -1 "$f"); max=$(tail -1 "$f")
  mean=$(awk '{s+=$1} END{printf "%.3f", s/NR}' "$f")
  printf "| %-12s | %3s | %6s | %6s | %6s | %6s | %6s | %6s |\n" \
    "$1" "$n" "$min" "$(pct 50 "$f")" "$(pct 90 "$f")" "$(pct 95 "$f")" "$max" "$mean"
}

failed=0
snap_metrics() { curl -s --max-time 10 "$METRICS_URL" 2>/dev/null | grep -E '^orchestrator_(operations|step)_duration_seconds_(sum|count)'; }

echo "fuse vm benchmark  N=$N fusefile=$FUSEFILE out=$OUT"
snap_metrics > "$OUT/metrics.before" 2>/dev/null

# ---- cold create ----
: > "$OUT/cold_create.txt"
for i in $(seq 1 "$N"); do
  t0=$(now)
  id=$(up_env "$FUSEFILE" --task-id "bench-cold-$i")
  [ -n "$id" ] && created+=("$id")
  if [ -n "$id" ] && wait_running "$id"; then
    t1=$(now); dt "$t0" "$t1" >> "$OUT/cold_create.txt"; echo "  cold $i: $(dt "$t0" "$t1")s ($id)"
  else
    echo "  cold $i: FAILED (see err.log)"; failed=$((failed + 1))
  fi
done

# ---- warm create (boot from build artifact) ----
: > "$OUT/warm_create.txt"
if [ -n "$FROM_BUILD" ]; then
  for i in $(seq 1 "$N"); do
    t0=$(now)
    id=$(up_env "$FUSEFILE" --from-build "$FROM_BUILD" --task-id "bench-warm-$i")
    [ -n "$id" ] && created+=("$id")
    if [ -n "$id" ] && wait_running "$id"; then
      t1=$(now); dt "$t0" "$t1" >> "$OUT/warm_create.txt"; echo "  warm $i: $(dt "$t0" "$t1")s ($id)"
    else
      echo "  warm $i: FAILED (see err.log)"; failed=$((failed + 1))
    fi
  done
fi

# ---- fork (CoW clone of the first running env) ----
: > "$OUT/fork.txt"
base="${created[0]:-}"
if [ -n "$base" ]; then
  wait_running "$base" || true
  for i in $(seq 1 "$N"); do
    t0=$(now)
    out=$(fuse environment fork "$base" --comment "bench-fork-$i" -o json 2>>"$OUT/err.log")
    fid=$(printf '%s' "$out" | id_of)
    [ -n "$fid" ] && created+=("$fid")
    if [ -n "$fid" ] && wait_running "$fid"; then
      t1=$(now); dt "$t0" "$t1" >> "$OUT/fork.txt"; echo "  fork $i: $(dt "$t0" "$t1")s ($fid)"
    else
      echo "  fork $i: FAILED (see err.log)"; failed=$((failed + 1))
    fi
  done
fi

# ---- destroy (timed; a failed one stays in created for the trap to retry) ----
: > "$OUT/destroy.txt"
left=()
for id in ${created[@]+"${created[@]}"}; do
  t0=$(now)
  if fuse environment destroy --yes "$id" >/dev/null 2>>"$OUT/err.log"; then
    t1=$(now); dt "$t0" "$t1" >> "$OUT/destroy.txt"
  else
    echo "  destroy $id: FAILED (see err.log)"; failed=$((failed + 1)); left+=("$id")
  fi
done
created=(${left[@]+"${left[@]}"})

snap_metrics > "$OUT/metrics.after" 2>/dev/null

# ---- report ----
echo
echo "## fuse vm lifecycle benchmark (seconds, end-to-end wall clock)"
echo
echo "| op           |   n |    min |    p50 |    p90 |    p95 |    max |   mean |"
echo "|--------------|-----|--------|--------|--------|--------|--------|--------|"
report_row "cold create" "$OUT/cold_create.txt"
[ -s "$OUT/warm_create.txt" ] && report_row "warm create" "$OUT/warm_create.txt"
report_row "fork"        "$OUT/fork.txt"
report_row "destroy"     "$OUT/destroy.txt"
echo
echo "server-side per-step breakdown (mean, from orchestrator metrics delta):"
# after minus before, so ops from earlier runs on this orchestrator drop out
awk '
  FNR==1 { file++ }
  /_duration_seconds_(sum|count)\{/ {
    key=$1; sub(/_duration_seconds_(sum|count)/, "", key); sub(/^orchestrator_/, "", key)
    kind = ($1 ~ /_sum\{/) ? "sum" : "count"
    v[kind, key] += (file == 1 ? -$2 : $2); keys[key]=1
  }
  END { for (k in keys) if (v["count", k] > 0) printf "  %-40s %.3fs/op (n=%d)\n", k, v["sum", k]/v["count", k], v["count", k] }
' "$OUT/metrics.before" "$OUT/metrics.after" 2>/dev/null | sort || true
echo
echo "raw samples + metrics in: $OUT"
[ "$failed" = 0 ] || { echo "$failed op(s) failed" >&2; exit 1; }
