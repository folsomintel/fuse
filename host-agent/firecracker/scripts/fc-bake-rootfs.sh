#!/usr/bin/env bash
# Rebuild rootfs-fused.ext4 from rootfs.ext4 + fused + podman-static + iptables.
# Idempotent. Requires: podman on host (for the iptables bundle extraction),
# sudo, mount, truncate, e2fsck, resize2fs, curl, tar.
#
# Inputs in the working directory:
#   rootfs.ext4          base Firecracker CI rootfs
#   fused                statically linked Linux/amd64 agent binary (the reference agent)
#   fused.service        systemd unit that supervises the agent in the guest
#
# Output:
#   rootfs-fused.ext4    baked image the fc-agent boots per VM
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$FC_DIR"

BASE=rootfs.ext4
OUT=rootfs-fused.ext4
SIZE=${FC_ROOTFS_SIZE:-4G}
MOUNT_POINT=${FC_BAKE_MOUNT:-/tmp/fcbake}
WORK=${FC_BAKE_WORK:-/tmp/fcbake-work}

# log, need and the fused/podman/compose/containers.conf steps shared with
# fc-bake-distro-rootfs.sh
# shellcheck source=fc-bake-lib.sh
source "$FC_DIR/scripts/fc-bake-lib.sh"
for c in sudo mount umount truncate e2fsck resize2fs curl tar podman; do need "$c"; done

[ -f "$BASE" ]  || { echo "$BASE not found — run ./scripts/fc-install.sh first" >&2; exit 1; }
[ -f fused ]   || { echo "fused not found — run ../shared/fc-build-agent.sh (or drop your own agent binary named 'fused' here)" >&2; exit 1; }
[ -f fused.service ] || { echo "fused.service not found — it ships in host-agent/firecracker/; restore it or provide your agent's unit" >&2; exit 1; }

cleanup() {
  sudo -n umount "$MOUNT_POINT" 2>/dev/null || true
}
trap cleanup EXIT

# under `set -e` a failing command aborts with no output, which historically made
# this script look like it exited cleanly mid-bake. always say where it died.
trap 'rc=$?; [ $rc -ne 0 ] && echo "[bake] FAILED at line $LINENO (exit $rc); image is incomplete" >&2' ERR

# e2fsck exits 1 when it corrected errors and 2 when it corrected errors and
# wants a reboot. both are success for our purposes; only >=4 is a real failure.
# without this, `set -e` kills the bake right after the grow, silently.
fsck_ok() {
  local rc=0
  sudo -n e2fsck -f -y "$1" >/dev/null || rc=$?
  [ "$rc" -le 2 ] || { echo "[bake] e2fsck failed on $1 (exit $rc)" >&2; return 1; }
}

mkdir -p "$WORK"

log "copy base -> $OUT"
cp -f "$BASE" "$OUT"

log "grow $OUT to $SIZE"
sudo -n truncate -s "$SIZE" "$OUT"
fsck_ok "$OUT"
sudo -n resize2fs "$OUT" >/dev/null

log "mount loopback at $MOUNT_POINT"
sudo -n mkdir -p "$MOUNT_POINT"
sudo -n mount -o loop "$OUT" "$MOUNT_POINT"

# --- 1. fused, ca bundle, podman-static, docker-compose, containers config ---

bake_inject_common "$MOUNT_POINT" "$WORK"

# --- 2. iptables bundle (extracted from ubuntu:22.04 via host podman) --------

if [ ! -f "$WORK/iptables-full.tar" ]; then
  log "build iptables bundle from ubuntu:22.04"
  # host network: skips netavark firewall setup, which needs nft/iptables on the host
  sudo -n podman run --rm --network=host -v "$WORK":/out docker.io/library/ubuntu:22.04 bash -c '
    set -e
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq >/dev/null
    apt-get install -y --no-install-recommends iptables libnetfilter-conntrack3 >/dev/null 2>&1
    tar -cf /out/iptables-full.tar -C / \
      usr/sbin/iptables usr/sbin/iptables-legacy \
      usr/sbin/iptables-legacy-save usr/sbin/iptables-legacy-restore \
      usr/sbin/iptables-save usr/sbin/iptables-restore \
      usr/sbin/ip6tables usr/sbin/ip6tables-legacy \
      usr/sbin/ip6tables-save usr/sbin/ip6tables-restore \
      usr/sbin/xtables-legacy-multi usr/sbin/xtables-nft-multi \
      usr/sbin/iptables-nft usr/sbin/ip6tables-nft \
      usr/lib/x86_64-linux-gnu/xtables \
      usr/lib/x86_64-linux-gnu/libxtables.so.12 \
      usr/lib/x86_64-linux-gnu/libxtables.so.12.4.0 \
      usr/lib/x86_64-linux-gnu/libip4tc.so.2 \
      usr/lib/x86_64-linux-gnu/libip4tc.so.2.0.0 \
      usr/lib/x86_64-linux-gnu/libip6tc.so.2 \
      usr/lib/x86_64-linux-gnu/libip6tc.so.2.0.0 \
      usr/lib/x86_64-linux-gnu/libnftnl.so.11 \
      usr/lib/x86_64-linux-gnu/libnftnl.so.11.6.0 \
      usr/lib/x86_64-linux-gnu/libmnl.so.0 \
      usr/lib/x86_64-linux-gnu/libmnl.so.0.2.0 \
      usr/lib/x86_64-linux-gnu/libnfnetlink.so.0 \
      usr/lib/x86_64-linux-gnu/libnfnetlink.so.0.2.0 \
      usr/lib/x86_64-linux-gnu/libnetfilter_conntrack.so.3 \
      usr/lib/x86_64-linux-gnu/libnetfilter_conntrack.so.3.8.0
  '
fi
log "inject iptables"
sudo -n tar -xf "$WORK/iptables-full.tar" -C "$MOUNT_POINT"
bake_legacy_iptables "$MOUNT_POINT"

# --- 3. sanity ---------------------------------------------------------------

log "sanity check"
bake_check_common "$MOUNT_POINT"
bake_check "$MOUNT_POINT" -f /usr/lib/x86_64-linux-gnu/xtables/libxt_comment.so

sudo -n umount "$MOUNT_POINT"
trap - EXIT

log "done -> $OUT ($(du -h "$OUT" | cut -f1))"
