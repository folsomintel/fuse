# shellcheck shell=bash
# lib.sh - helpers shared by the benchmark scripts. source it, do not run it.
#
#   . "$(dirname "$0")/lib.sh"      # from benchmark/
#   . "$(dirname "$0")/../lib.sh"   # from benchmark/<suite>/

BENCH_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export PATH="$PATH:/usr/local/bin"

# the agent's snapshot store: SNAPSHOTS_DIR from its .env when set, the same
# lookup setup.sh does, so the leak audit and env.json look where it writes.
if [ -z "${SNAPSHOTS_DIR:-}" ]; then
  SNAPSHOTS_DIR="$(grep -E '^SNAPSHOTS_DIR=' "$BENCH_LIB_DIR/../host-agent/firecracker/.env" 2>/dev/null | cut -d= -f2- || true)"
  if [ -n "$SNAPSHOTS_DIR" ]; then export SNAPSHOTS_DIR; fi
fi

now()   { date +%s.%N; }
dt()    { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.3f", b-a}'; }

# extract an env id from `-o json` output, tolerant of field name
id_of() { jq -r '.id // .environment.id // .vmId // .vm_id // .vm.id // empty' 2>/dev/null; }

# poll until the env reports a running/ready/active state (bounded)
wait_running() {
  local id="$1" deadline=$(( $(date +%s) + ${2:-180} ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local s
    s=$(fuse environment get "$id" -o json 2>/dev/null | jq -r '.state // .status // empty' 2>/dev/null)
    case "$s" in running|ready|active|RUNNING|READY|ACTIVE) return 0;; failed|FAILED) return 1;; esac
    sleep 0.2
  done
  return 1
}

pct() { # pct <p> <sorted-file>
  awk -v p="$1" '{a[NR]=$1} END{if(NR==0){print "NA";exit}
    idx=int((p/100)*NR+0.9999); if(idx<1)idx=1; if(idx>NR)idx=NR; printf "%.3f", a[idx]}' "$2"
}

# python for the sampler, leak audit and report: stdlib only, so the host's
# system python3 is enough.
bench_py() { python3 "$BENCH_LIB_DIR/$1" "${@:2}"; }

# meminfo_mb <key>: a /proc/meminfo field in MB, 0 when there is no /proc
meminfo_mb() { awk -v k="$1:" '$1==k{v=int($2/1024)} END{print v+0}' /proc/meminfo 2>/dev/null || echo 0; }

# write_env_json <out-file> <dir>: fingerprint this host. <dir> is the fuse
# checkout, whose filesystem and git sha are recorded. every probe tolerates a
# missing tool so a dry run off-linux still writes a file.
write_env_json() {
  local out="$1" dir="$2" governor ksm snap_dir os
  # shellcheck disable=SC1091 # host file, absent off-linux
  os="$( (. /etc/os-release && echo "$PRETTY_NAME") 2>/dev/null || uname -s)"
  governor="$(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo none)"
  case "$(cat /sys/kernel/mm/ksm/run 2>/dev/null)" in 0) ksm=off;; 1) ksm=on;; *) ksm=absent;; esac
  snap_dir="${SNAPSHOTS_DIR:-$dir/host-agent/firecracker/agent-state/snapshots}"
  [ -d "$snap_dir" ] || snap_dir="$dir"
  jq -n \
    --arg utc "$(date -u +%FT%TZ)" \
    --arg hostname "$(hostname -s)" \
    --arg cpu "$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null)" \
    --argjson threads "$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)" \
    --argjson ram_mb "$(meminfo_mb MemTotal)" \
    --argjson swap_mb "$(meminfo_mb SwapTotal)" \
    --arg kernel "$(uname -r)" \
    --arg os "$os" \
    --arg fs "$(findmnt -no FSTYPE -T "$dir" 2>/dev/null)" \
    --arg backing "$(findmnt -no SOURCE -T "$dir" 2>/dev/null)" \
    --arg snap_fs "$(findmnt -no FSTYPE -T "$snap_dir" 2>/dev/null)" \
    --arg disk "$(lsblk -dno MODEL,ROTA 2>/dev/null | awk '{rota=$NF; $NF=""; printf "%s(rota=%s) ", $0, rota}')" \
    --arg firecracker "$(/usr/local/bin/firecracker --version 2>/dev/null | awk 'NR==1')" \
    --arg guest_kernel "${FC_GUEST_KERNEL:-5.10}" \
    --arg fuse_ref "${FUSE_REF:-$(git -C "$dir" rev-parse --abbrev-ref HEAD 2>/dev/null)}" \
    --arg fuse_sha "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" \
    --arg fuse_cli "$(fuse --version 2>/dev/null)" \
    --arg governor "$governor" \
    --arg ksm "$ksm" \
    --argjson cpu_overcommit "${BENCH_CPU_OVERCOMMIT:-0}" \
    --arg control_plane "${BENCH_CONTROL_PLANE:-co-located}" \
    '{utc:$utc, hostname:$hostname, cpu:$cpu, threads:$threads, ram_mb:$ram_mb,
      kernel:$kernel, os:$os,
      storage:{fs:$fs, backing:$backing, snapshot_store_fs:$snap_fs, disks:$disk},
      firecracker:$firecracker, guest_kernel:$guest_kernel,
      fuse:{ref:$fuse_ref, sha:$fuse_sha, cli:$fuse_cli},
      tuning:{governor:$governor, ksm:$ksm, swap_mb:$swap_mb, cpu_overcommit:$cpu_overcommit},
      control_plane:$control_plane}' > "$out"
}

# bench_start <out-dir> <suite>: env.json, leak baseline, 1 hz sampler.
# bench_finish stops the sampler and runs the leak audit; its exit status is
# the audit's, so a leak fails the run.
SAMPLER_PID=""
bench_start() {
  BENCH_OUT="$1"
  mkdir -p "$BENCH_OUT"
  write_env_json "$BENCH_OUT/env.json" "${BENCH_REPO:-$(cd "$BENCH_LIB_DIR/.." && pwd)}"
  jq --arg suite "$2" --argjson smoke "$([ "${SMOKE:-}" = 1 ] && echo true || echo false)" \
    '. + {suite:$suite, smoke:$smoke}' "$BENCH_OUT/env.json" > "$BENCH_OUT/env.json.tmp" \
    && mv "$BENCH_OUT/env.json.tmp" "$BENCH_OUT/env.json"
  bench_py leak_audit.py snapshot --out "$BENCH_OUT/leak-before.json" || return 1
  bench_py sampler.py --out "$BENCH_OUT/samples.jsonl" --interval "${SAMPLE_INTERVAL:-1}" &
  SAMPLER_PID=$!
}

# wait_jobs: wait for every background job except the sampler, which a bare
# `wait` would block on forever.
wait_jobs() {
  local p
  for p in $(jobs -p); do
    [ "$p" = "$SAMPLER_PID" ] || wait "$p" 2>/dev/null
  done
}

# inflight: running background jobs, not counting the sampler
inflight() { jobs -rp | grep -vcx "${SAMPLER_PID:-none}"; }

bench_finish() {
  if [ -n "$SAMPLER_PID" ]; then
    kill "$SAMPLER_PID" 2>/dev/null || true
    wait "$SAMPLER_PID" 2>/dev/null || true
    SAMPLER_PID=""
  fi
  bench_py leak_audit.py check --before "$BENCH_OUT/leak-before.json" \
    --out "$BENCH_OUT/leak.json" --settle "${AUDIT_SETTLE:-60}"
}

# destroy_all <file-of-ids>: destroy every id in the file, a few at a time.
destroy_all() {
  [ -s "$1" ] || return 0
  # shellcheck disable=SC2016 # $1 expands in the inner sh
  xargs -P "${DESTROY_PARALLEL:-16}" -I{} sh -c 'fuse environment destroy --yes "$1" >/dev/null 2>&1 || true' _ {} < "$1"
}

# exec_probe <id>: run a trivial command in the guest, print seconds taken.
# fails if the command did not exit 0.
exec_probe() {
  local t0 t1
  t0=$(now)
  fuse environment exec "$1" --timeout 60s -- true >/dev/null 2>&1 || return 1
  t1=$(now)
  dt "$t0" "$t1"
}

# percentile summary of a file of numbers: "<n> <p50> <p99>" (NA when empty)
p50_p99() {
  local f="$1" s
  s=$(mktemp)
  sort -n "$f" > "$s"
  printf '%s %s %s\n' "$(wc -l < "$s" | tr -d ' ')" "$(pct 50 "$s")" "$(pct 99 "$s")"
  rm -f "$s"
}

# scale <var> <smoke-value> <full-value>: set a scale knob unless the caller
# already did. SMOKE=1 picks the small value; nothing else differs between a
# smoke run and a full one.
scale() {
  [ -n "${!1:-}" ] && return 0
  if [ "${SMOKE:-}" = 1 ]; then printf -v "$1" '%s' "$2"; else printf -v "$1" '%s' "$3"; fi
}

# default_out <suite>: benchmark/results/<suite>-<host>-<utc>
default_out() {
  printf '%s/results/%s-%s-%s' "$BENCH_LIB_DIR" "$1" "$(hostname -s)" "$(date -u +%Y%m%dT%H%M%SZ)"
}

# op_line <file> <json-fields>: append one jsonl record. lines are short, so
# concurrent appends from background jobs do not interleave.
op_line() { printf '{%s}\n' "$2" >> "$1"; }

# host_sample: one sampler record on stdout
host_sample() { bench_py sampler.py --once; }

# json number or null
jnum() { case "$1" in ''|NA) echo null;; *) echo "$1";; esac; }
