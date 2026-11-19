#!/usr/bin/env bash
# setup.sh - turn a fresh bare-metal linux box into a fuse benchmark host and
# record a baseline, in one command:
#
#   curl -fsSL https://raw.githubusercontent.com/folsomintel/fuse/main/benchmark/setup.sh | sudo bash
#
# what it does (idempotent, safe to re-run):
#   1. preflight: root, x86_64, apt/dnf, hardware virtualization + /dev/kvm
#   2. packages: git, jq, xfsprogs, sudo, docker (restate-server runs in it)
#   3. storage: puts the fuse checkout on a reflink-capable filesystem, so
#      rootfs and snapshot copies are reflinks, not full copies
#   4. host tuning: fd/pid/map/conntrack/neighbour limits, performance
#      governor, ksm off
#   5. fuse: clones the repo and runs `fc-agent.sh bootstrap` (agent,
#      postgres, restate, orchestrator, fused bake), with the auto-updater off
#      so the version stays pinned for the run
#   6. registers this box as host local-fc with benchmark capacity, installs
#      the fuse cli and connects it
#   7. fingerprints the host (env.json), runs vm-bench.sh at N=1 as a smoke
#      test, then at N=BENCH_N, and writes everything to
#      benchmark/results/<hostname>-<utc>/
#
# env knobs:
#   FUSE_REF             branch or tag to clone (default: main)
#   FUSE_REPO_URL        git url (default: https://github.com/folsomintel/fuse)
#   BENCH_DIR            where the checkout lives (default: /srv/fuse)
#   BENCH_DEVICE         a spare block device to format as xfs for BENCH_DIR.
#                        destructive: refused if mounted or if it already holds
#                        a filesystem, unless BENCH_FORCE=1
#   BENCH_IMG_GB         size of the xfs loop image used when BENCH_DIR's
#                        filesystem cannot reflink and no BENCH_DEVICE is given
#                        (default: 60% of free space, capped at 400)
#   BENCH_CPU_OVERCOMMIT vcpus the scheduler may place per host cpu (default: 4)
#   BENCH_MAX_VMS        vm_count capacity to register (default: 249, the agent cap)
#   BENCH_N              vm-bench iterations for the baseline (default: 10)
#   BENCH_SKIP_RUN=1     set up the host but skip the smoke + baseline runs
#   FC_GUEST_KERNEL      passed to fc-install.sh (default 5.10; 6.1 for vmgenid
#                        and memory hotplug)
#   PUBLIC_HOST          address printed in the connect line (see bootstrap)
set -euo pipefail

FUSE_REF="${FUSE_REF:-main}"
FUSE_REPO_URL="${FUSE_REPO_URL:-https://github.com/folsomintel/fuse}"
BENCH_DIR="${BENCH_DIR:-/srv/fuse}"
BENCH_DEVICE="${BENCH_DEVICE:-}"
BENCH_IMG="/var/lib/fuse-bench/fuse-xfs.img"
BENCH_CPU_OVERCOMMIT="${BENCH_CPU_OVERCOMMIT:-4}"
BENCH_MAX_VMS="${BENCH_MAX_VMS:-249}"
BENCH_N="${BENCH_N:-10}"
REPO_DIR="$BENCH_DIR/fuse"
FC_DIR="$REPO_DIR/host-agent/firecracker"
ORCH_URL="http://127.0.0.1:8080"
HOST_ID="local-fc"

log()  { printf '\033[1;36m[bench-setup] %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ok %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m  ! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m  x %s\033[0m\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
log "1/7 preflight"
[ "$(id -u)" -eq 0 ] || die "run as root: curl ... | sudo bash"
[ "$(uname -m)" = "x86_64" ] || die "x86_64 only (the baked rootfs is amd64), got $(uname -m)"
if command -v apt-get >/dev/null 2>&1; then PKG=apt
elif command -v dnf >/dev/null 2>&1; then PKG=dnf
else die "need apt-get or dnf"; fi

CPU_MODEL="$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo)"
if grep -qw vmx /proc/cpuinfo; then KVM_MOD=kvm_intel
elif grep -qw svm /proc/cpuinfo; then KVM_MOD=kvm_amd
else die "no vmx/svm cpu flag: this is a vm without nested virtualization, or virtualization is off in the bios"; fi
[ -e /dev/kvm ] || modprobe "$KVM_MOD" || true
[ -e /dev/kvm ] || die "/dev/kvm missing after modprobe $KVM_MOD"
ok "cpu: $CPU_MODEL ($(nproc) threads), kvm via $KVM_MOD"
# firecracker's supported list is intel skylake+ / amd zen2+; older parts
# usually work, and the smoke run below is the real test.
warn "firecracker officially supports intel skylake+ and amd zen2+; if the smoke run fails on this cpu, that is the first suspect"

# ---------------------------------------------------------------------------
log "2/7 packages"
if [ "$PKG" = apt ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y -q
  apt-get install -y -q --no-install-recommends git jq xfsprogs sudo curl ca-certificates util-linux docker.io
else
  dnf install -y -q git jq xfsprogs sudo curl ca-certificates util-linux moby-engine
fi
systemctl enable --now docker >/dev/null 2>&1 || die "could not start docker"
ok "git, jq, xfsprogs, sudo, docker"

# ---------------------------------------------------------------------------
log "3/7 storage (reflink-capable filesystem for $BENCH_DIR)"
# reflink_ok <dir>: can this directory's filesystem reflink a file?
reflink_ok() {
  local probe="$1/.reflink-probe" rc=0
  echo probe > "$probe" || return 1
  cp --reflink=always "$probe" "$probe.copy" 2>/dev/null || rc=1
  rm -f "$probe" "$probe.copy"
  return "$rc"
}

mkdir -p "$BENCH_DIR"
if reflink_ok "$BENCH_DIR"; then
  ok "$BENCH_DIR already reflinks ($(findmnt -no FSTYPE -T "$BENCH_DIR"))"
elif [ -n "$BENCH_DEVICE" ]; then
  [ -b "$BENCH_DEVICE" ] || die "BENCH_DEVICE=$BENCH_DEVICE is not a block device"
  findmnt -S "$BENCH_DEVICE" >/dev/null 2>&1 && die "$BENCH_DEVICE is mounted; refusing to format it"
  if [ -n "$(blkid -o value -s TYPE "$BENCH_DEVICE" 2>/dev/null)" ] && [ "${BENCH_FORCE:-}" != "1" ]; then
    die "$BENCH_DEVICE already holds a filesystem; set BENCH_FORCE=1 to wipe it"
  fi
  mkfs.xfs -f -q -m reflink=1 "$BENCH_DEVICE"
  UUID="$(blkid -o value -s UUID "$BENCH_DEVICE")"
  grep -q "$UUID" /etc/fstab || echo "UUID=$UUID $BENCH_DIR xfs noatime 0 0" >> /etc/fstab
  mount "$BENCH_DIR"
  ok "formatted $BENCH_DEVICE as xfs (reflink) on $BENCH_DIR"
else
  # no spare device: an xfs filesystem in a sparse loop image on the root disk.
  # one extra layer of indirection, visible as the loop source in env.json.
  mkdir -p "$(dirname "$BENCH_IMG")"
  if [ ! -f "$BENCH_IMG" ]; then
    FREE_GB=$(df -Pk "$(dirname "$BENCH_IMG")" | awk 'NR==2{printf "%d", $4/1048576}')
    IMG_GB="${BENCH_IMG_GB:-$(( FREE_GB * 60 / 100 ))}"
    [ "$IMG_GB" -gt 400 ] && [ -z "${BENCH_IMG_GB:-}" ] && IMG_GB=400
    [ "$IMG_GB" -ge 40 ] || die "only ${FREE_GB}G free; need room for a 40G+ image (or set BENCH_DEVICE)"
    truncate -s "${IMG_GB}G" "$BENCH_IMG"
    mkfs.xfs -q -m reflink=1 "$BENCH_IMG"
  fi
  grep -q "$BENCH_IMG" /etc/fstab || echo "$BENCH_IMG $BENCH_DIR xfs loop,noatime 0 0" >> /etc/fstab
  mountpoint -q "$BENCH_DIR" || mount "$BENCH_DIR"
  ok "xfs loop image $BENCH_IMG mounted on $BENCH_DIR"
fi
reflink_ok "$BENCH_DIR" || die "$BENCH_DIR still cannot reflink"

# ---------------------------------------------------------------------------
log "4/7 host tuning"
modprobe nf_conntrack 2>/dev/null || true
cat > /etc/sysctl.d/90-fuse-bench.conf <<'EOF'
# fuse benchmark host: room for thousands of vms, taps and connections
fs.file-max = 4194304
fs.nr_open = 4194304
fs.inotify.max_user_instances = 8192
kernel.pid_max = 4194304
vm.max_map_count = 1048576
net.core.somaxconn = 4096
net.netfilter.nf_conntrack_max = 1048576
net.ipv4.neigh.default.gc_thresh1 = 8192
net.ipv4.neigh.default.gc_thresh2 = 32768
net.ipv4.neigh.default.gc_thresh3 = 65536
EOF
sysctl --system >/dev/null 2>&1 || warn "some sysctls did not apply (see: sysctl --system)"
mkdir -p /etc/systemd/system.conf.d
printf '[Manager]\nDefaultLimitNOFILE=1048576\n' > /etc/systemd/system.conf.d/90-fuse-bench.conf
systemctl daemon-reexec
# governor and ksm do not persist across reboot; re-run this script after one.
GOVERNOR="none"
for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
  if [ -w "$g" ] && echo performance > "$g" 2>/dev/null; then GOVERNOR="performance"; fi
done
KSM="absent"
if [ -w /sys/kernel/mm/ksm/run ]; then echo 0 > /sys/kernel/mm/ksm/run; KSM="off"; fi
ok "sysctls, nofile 1048576, governor=$GOVERNOR, ksm=$KSM"

# ---------------------------------------------------------------------------
log "5/7 fuse ($FUSE_REF) + bootstrap"
if [ -d "$REPO_DIR/.git" ]; then
  git -C "$REPO_DIR" fetch -q origin
  git -C "$REPO_DIR" checkout -q "$FUSE_REF"
  git -C "$REPO_DIR" pull -q --ff-only 2>/dev/null || true   # a tag has nothing to pull
else
  git clone -q --branch "$FUSE_REF" "$FUSE_REPO_URL" "$REPO_DIR"
fi
ok "checkout $REPO_DIR at $(git -C "$REPO_DIR" rev-parse --short HEAD)"
# no updater: the version must not move mid-benchmark. registration is done
# below with benchmark capacity instead of bootstrap's defaults.
(cd "$FC_DIR" && ./scripts/fc-agent.sh bootstrap --no-updater --no-register)

# ---------------------------------------------------------------------------
log "6/7 register host + cli"
TOK="$(grep -E '^ORCH_AUTH_TOKEN=' /etc/default/orchestrator | cut -d= -f2-)"
[ -n "$TOK" ] || die "no ORCH_AUTH_TOKEN in /etc/default/orchestrator; bootstrap did not finish"
AGENT_TOK="$(grep -E '^FC_AGENT_TOKEN=' "$FC_DIR/.env" | cut -d= -f2-)"
for _ in $(seq 1 60); do
  curl -fsS -H "Authorization: Bearer $TOK" "$ORCH_URL/v1/hosts" >/dev/null 2>&1 && break
  sleep 1
done
CPUS=$(nproc)
RAM_MB=$(awk '/^MemTotal/{printf "%d", $2/1024}' /proc/meminfo)
STORAGE_GB=$(df -Pk "$BENCH_DIR" | awk 'NR==2{printf "%d", $2/1048576}')
# re-registering overwrites the host record, so a re-run picks up new knobs.
curl -fsS -X POST "$ORCH_URL/v1/hosts" \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$HOST_ID\",\"url\":\"http://127.0.0.1:8090\",\"token\":\"$AGENT_TOK\",\"region\":\"local\",\"backend\":\"firecracker\",\"capacity\":{\"cpus\":$(( CPUS * BENCH_CPU_OVERCOMMIT )),\"ram_mb\":$RAM_MB,\"storage_gb\":$STORAGE_GB,\"vm_count\":$BENCH_MAX_VMS}}" \
  >/dev/null || die "host register failed"
ok "host $HOST_ID: $(( CPUS * BENCH_CPU_OVERCOMMIT )) vcpus (${BENCH_CPU_OVERCOMMIT}x), ${RAM_MB} MB, ${STORAGE_GB} GB, ${BENCH_MAX_VMS} vms"

command -v fuse >/dev/null 2>&1 || bash "$REPO_DIR/ops/install-fuse.sh"
export PATH="$PATH:/usr/local/bin"
fuse connect "$ORCH_URL" --token "$TOK" --name bench >/dev/null
ok "fuse cli $(fuse --version 2>/dev/null) connected"

# ---------------------------------------------------------------------------
log "7/7 fingerprint + runs"
RES="$REPO_DIR/benchmark/results/$(hostname -s)-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RES"
jq -n \
  --arg utc "$(date -u +%FT%TZ)" \
  --arg hostname "$(hostname -s)" \
  --arg cpu "$CPU_MODEL" \
  --argjson threads "$CPUS" \
  --argjson ram_mb "$RAM_MB" \
  --arg kernel "$(uname -r)" \
  --arg os "$(. /etc/os-release && echo "$PRETTY_NAME")" \
  --arg fs "$(findmnt -no FSTYPE -T "$BENCH_DIR")" \
  --arg backing "$(findmnt -no SOURCE -T "$BENCH_DIR")" \
  --arg disk "$(lsblk -dno MODEL,ROTA 2>/dev/null | awk '{rota=$NF; $NF=""; printf "%s(rota=%s) ", $0, rota}')" \
  --arg firecracker "$(/usr/local/bin/firecracker --version 2>/dev/null | head -n1)" \
  --arg guest_kernel "${FC_GUEST_KERNEL:-5.10}" \
  --arg fuse_ref "$FUSE_REF" \
  --arg fuse_sha "$(git -C "$REPO_DIR" rev-parse HEAD)" \
  --arg fuse_cli "$(fuse --version 2>/dev/null)" \
  --arg governor "$GOVERNOR" \
  --arg ksm "$KSM" \
  --argjson swap_mb "$(awk '/^SwapTotal/{printf "%d", $2/1024}' /proc/meminfo)" \
  --argjson cpu_overcommit "$BENCH_CPU_OVERCOMMIT" \
  '{utc:$utc, hostname:$hostname, cpu:$cpu, threads:$threads, ram_mb:$ram_mb,
    kernel:$kernel, os:$os, storage:{fs:$fs, backing:$backing, disks:$disk},
    firecracker:$firecracker, guest_kernel:$guest_kernel,
    fuse:{ref:$fuse_ref, sha:$fuse_sha, cli:$fuse_cli},
    tuning:{governor:$governor, ksm:$ksm, swap_mb:$swap_mb, cpu_overcommit:$cpu_overcommit},
    control_plane:"co-located"}' > "$RES/env.json"
ok "fingerprint: $RES/env.json"

if [ "${BENCH_SKIP_RUN:-}" = "1" ]; then
  log "BENCH_SKIP_RUN=1: host is ready, skipping runs"
else
  BENCH="$REPO_DIR/benchmark/vm-bench.sh"
  FUSEFILE="$REPO_DIR/benchmark/Fusefile.bench"
  # smoke first: the same script at N=1 must create, fork and destroy cleanly
  # before the full run is worth its time.
  N=1 HOST="$HOST_ID" FUSEFILE="$FUSEFILE" OUT="$RES/smoke" bash "$BENCH" | tee "$RES/smoke.md"
  for f in cold_create fork destroy; do
    [ -s "$RES/smoke/$f.txt" ] || die "smoke run produced no $f sample; see $RES/smoke/err.log"
  done
  ok "smoke passed"
  N="$BENCH_N" HOST="$HOST_ID" FUSEFILE="$FUSEFILE" OUT="$RES/baseline" bash "$BENCH" | tee "$RES/baseline.md"
fi

echo
echo "======================================================================"
echo " fuse benchmark host ready"
echo "   results : $RES"
echo "   rerun   : N=20 HOST=$HOST_ID FUSEFILE=$REPO_DIR/benchmark/Fusefile.bench bash $REPO_DIR/benchmark/vm-bench.sh"
echo "   laptop  : fuse connect http://${PUBLIC_HOST:-<this-box>}:8080 --token $TOK"
echo "======================================================================"
