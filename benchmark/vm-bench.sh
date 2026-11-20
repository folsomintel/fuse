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
# run this ON phantom (it uses the local fuse context). nothing is left behind:
# every env created is destroyed at the end (and on ctrl-c).
#
#   N=20 bash vm-bench.sh                 # 20 iterations each (default 10)
#   FUSEFILE=/path/to/Fusefile bash vm-bench.sh
#   FROM_BUILD=snap-...  bash vm-bench.sh # also benchmark warm boot from this build snapshot
set -uo pipefail
# shellcheck source=benchmark/lib.sh
. "$(dirname "$0")/lib.sh"

N="${N:-10}"
FUSEFILE="${FUSEFILE:-/home/debian/fuse/docs/fusefiles/Fusefile.cpu}"
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

snap_metrics() { curl -s "$METRICS_URL" 2>/dev/null | grep -E '^orchestrator_(operations|step)_duration_seconds_(sum|count)'; }

echo "fuse vm benchmark  N=$N fusefile=$FUSEFILE out=$OUT"
snap_metrics > "$OUT/metrics.before" 2>/dev/null

# ---- cold create ----
: > "$OUT/cold_create.txt"
for i in $(seq 1 "$N"); do
  t0=$(now)
  out=$(fuse up "$FUSEFILE" --task-id "bench-cold-$i" -o json 2>>"$OUT/err.log")
  t1=$(now)
  id=$(printf '%s' "$out" | id_of)
  if [ -n "$id" ]; then created+=("$id"); dt "$t0" "$t1" >> "$OUT/cold_create.txt"; echo "  cold $i: $(dt "$t0" "$t1")s ($id)"; else echo "  cold $i: FAILED (see err.log)"; fi
done

# ---- warm create (boot from build artifact) ----
: > "$OUT/warm_create.txt"
if [ -n "$FROM_BUILD" ]; then
  for i in $(seq 1 "$N"); do
    t0=$(now)
    out=$(fuse up "$FUSEFILE" --from-build "$FROM_BUILD" --task-id "bench-warm-$i" -o json 2>>"$OUT/err.log")
    t1=$(now)
    id=$(printf '%s' "$out" | id_of)
    if [ -n "$id" ]; then created+=("$id"); dt "$t0" "$t1" >> "$OUT/warm_create.txt"; echo "  warm $i: $(dt "$t0" "$t1")s ($id)"; fi
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
    [ -n "$fid" ] && wait_running "$fid"
    t1=$(now)
    if [ -n "$fid" ]; then created+=("$fid"); dt "$t0" "$t1" >> "$OUT/fork.txt"; echo "  fork $i: $(dt "$t0" "$t1")s ($fid)"; fi
  done
fi

# ---- destroy (timed, then handled by trap for any stragglers) ----
: > "$OUT/destroy.txt"
for id in ${created[@]+"${created[@]}"}; do
  t0=$(now)
  if fuse environment destroy --yes "$id" >/dev/null 2>>"$OUT/err.log"; then t1=$(now); dt "$t0" "$t1" >> "$OUT/destroy.txt"; else echo "  destroy $id: FAILED (see err.log)"; fi
done
created=()  # already destroyed; stop the trap re-destroying

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
join -j1 \
  <(awk '/_sum{/{gsub(/orchestrator_|_duration_seconds_sum/,""); print $1" "$2}' "$OUT/metrics.after" 2>/dev/null | sort) \
  <(awk '/_count{/{gsub(/orchestrator_|_duration_seconds_count/,""); print $1" "$2}' "$OUT/metrics.after" 2>/dev/null | sort) 2>/dev/null \
  | awk '{ if($3>0) printf "  %-40s %.3fs/op (n=%d)\n", $1, $2/$3, $3 }' || true
echo
echo "raw samples + metrics in: $OUT"
