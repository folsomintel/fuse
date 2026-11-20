#!/usr/bin/env bash
# churn: open-loop create -> exec -> destroy arrivals at a sweep of rates.
#
# arrivals are scheduled at fixed times (rate per second, for DURATION
# seconds) and launched whether or not earlier ones finished, so a slow
# control plane builds a queue instead of slowing the load down. latency is
# measured from each arrival's scheduled time, not from when it got to run,
# which keeps launcher lag inside the number (no coordinated omission).
# the sweep stops at the knee: the first rate whose create p99 passes
# KNEE_FACTOR x the lowest rate's, or whose errors pass ERR_LIMIT.
#
#   bash benchmark/density/churn.sh
#   SMOKE=1 bash benchmark/density/churn.sh      # about a minute
#
# scale knobs (SMOKE=1 / full):
#   RATES            arrivals per second, one run each ("1 2" / "0.5 1 2 4 8 16 32")
#   DURATION         seconds of arrivals per rate (10 / 120)
# other:
#   HOLD             seconds each vm lives between exec and destroy (0)
#   MAX_INFLIGHT     arrivals past this many in flight are shed and count as errors (500)
#   KNEE_FACTOR (2), ERR_LIMIT (0.01), STOP_AT_KNEE (1)
#   HOST, FUSEFILE, OUT, AUDIT_SETTLE, PSS_EVERY
set -uo pipefail
# shellcheck source=benchmark/lib.sh
. "$(dirname "$0")/../lib.sh"

scale RATES "1 2" "0.5 1 2 4 8 16 32"
scale DURATION 10 120
HOLD="${HOLD:-0}"
MAX_INFLIGHT="${MAX_INFLIGHT:-500}"
export KNEE_FACTOR="${KNEE_FACTOR:-2}" ERR_LIMIT="${ERR_LIMIT:-0.01}"
STOP_AT_KNEE="${STOP_AT_KNEE:-1}"
HOST="${HOST:-local-fc}"
FUSEFILE="${FUSEFILE:-$BENCH_LIB_DIR/Fusefile.bench}"
OUT="${OUT:-$(default_out density-churn)}"
RUN="churn$(date +%s)"

bench_start "$OUT" density-churn || exit 1
ARR="$OUT/arrivals.jsonl"
IDS="$OUT/ids.txt"
: > "$ARR"
: > "$IDS"

finish() {
  trap - EXIT INT TERM
  wait_jobs
  # each lifecycle destroys its own vm; this catches the ones a failure or
  # ctrl-c left behind (destroying a gone vm is harmless).
  destroy_all "$IDS"
  bench_finish
  local rc=$?
  bench_py report.py churn "$OUT"
  exit "$rc"
}
trap finish EXIT INT TERM

arrival() { # arrival <rate> <i> <sched>
  local rate="$1" i="$2" sched="$3" out id t_c t_e t_d c=null e=null d=null tot=null ok=false err=""
  out=$(fuse up "$FUSEFILE" --task-id "$RUN-$rate-$i" --host "$HOST" -o json 2>>"$OUT/err.log")
  id=$(printf '%s' "$out" | id_of)
  if [ -z "$id" ]; then
    err=create
  else
    echo "$id" >> "$IDS"
    if ! wait_running "$id"; then
      err=running
    else
      t_c=$(now); c=$(dt "$sched" "$t_c")
      if fuse environment exec "$id" --timeout 60s -- true >/dev/null 2>&1; then
        t_e=$(now); e=$(dt "$t_c" "$t_e")
        [ "$HOLD" != 0 ] && sleep "$HOLD"
        t_d=$(now)
        if fuse environment destroy --yes "$id" >/dev/null 2>&1; then
          d=$(dt "$t_d" "$(now)"); tot=$(dt "$sched" "$(now)"); ok=true
        else
          err=destroy
        fi
      else
        err="exec"
      fi
    fi
  fi
  op_line "$ARR" "\"rate\":$rate,\"i\":$i,\"sched\":$sched,\"id\":\"$id\",\"create_s\":$c,\"exec_s\":$e,\"destroy_s\":$d,\"total_s\":$tot,\"ok\":$ok,\"err\":\"$err\""
}

echo "churn  rates=($RATES) duration=${DURATION}s hold=${HOLD}s host=$HOST out=$OUT"
for rate in $RATES; do
  n=$(awk -v r="$rate" -v d="$DURATION" 'BEGIN{printf "%d", r*d}')
  start=$(now)
  echo "  rate $rate/s: $n arrivals"
  for i in $(seq 0 $((n - 1))); do
    sched=$(awk -v s="$start" -v i="$i" -v r="$rate" 'BEGIN{printf "%.3f", s + i/r}')
    wait_s=$(awk -v s="$sched" -v t="$(now)" 'BEGIN{d=s-t; printf "%.3f", d>0?d:0}')
    [ "$wait_s" != 0.000 ] && sleep "$wait_s"
    if [ "$(inflight)" -ge "$MAX_INFLIGHT" ]; then
      op_line "$ARR" "\"rate\":$rate,\"i\":$i,\"sched\":$sched,\"id\":\"\",\"create_s\":null,\"exec_s\":null,\"destroy_s\":null,\"total_s\":null,\"ok\":false,\"err\":\"shed\""
      continue
    fi
    arrival "$rate" "$i" "$sched" &
  done
  wait_jobs
  knee=$(bench_py report.py knee "$ARR")
  if [ -n "$knee" ]; then
    echo "  knee at $knee/s"
    [ "$STOP_AT_KNEE" = 1 ] && break
  fi
done
