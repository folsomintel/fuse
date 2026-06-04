#!/usr/bin/env bash
# Rebuild rootfs-surfd.ext4 from rootfs.ext4 + surfd + podman-static + iptables.
# Idempotent. Requires: podman on host (for the iptables bundle extraction),
# sudo, mount, truncate, e2fsck, resize2fs, curl, tar.
#
# Inputs in the working directory:
#   rootfs.ext4          base Firecracker CI rootfs
#   surfd                statically linked Linux/amd64 surfd binary
#
# Output:
#   rootfs-surfd.ext4    baked image the fc-agent boots per VM
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$FC_DIR"

BASE=rootfs.ext4
OUT=rootfs-surfd.ext4
SIZE=${FC_ROOTFS_SIZE:-4G}
PODMAN_STATIC_VERSION=${PODMAN_STATIC_VERSION:-v5.8.1}
PODMAN_STATIC_URL="https://github.com/mgoltzsche/podman-static/releases/download/${PODMAN_STATIC_VERSION}/podman-linux-amd64.tar.gz"
DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v2.40.3}
DOCKER_COMPOSE_URL="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-x86_64"
MOUNT_POINT=${FC_BAKE_MOUNT:-/tmp/fcbake}
WORK=${FC_BAKE_WORK:-/tmp/fcbake-work}

log() { printf '\033[1;36m[bake] %s\033[0m\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
for c in sudo mount umount truncate e2fsck resize2fs curl tar podman; do need "$c"; done

[ -f "$BASE" ]  || { echo "$BASE not found — run ./fc-install.sh first" >&2; exit 1; }
[ -f surfd ]   || { echo "surfd not found — download from your artifact store" >&2; exit 1; }

cleanup() {
  sudo -n umount "$MOUNT_POINT" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$WORK"

log "copy base -> $OUT"
cp -f "$BASE" "$OUT"

log "grow $OUT to $SIZE"
sudo -n truncate -s "$SIZE" "$OUT"
sudo -n e2fsck -f -y "$OUT" >/dev/null
sudo -n resize2fs "$OUT" >/dev/null

log "mount loopback at $MOUNT_POINT"
sudo -n mkdir -p "$MOUNT_POINT"
sudo -n mount -o loop "$OUT" "$MOUNT_POINT"

# --- 1. surfd + systemd unit + /surf + /var/tmp + CA bundle ------------------

log "inject surfd + systemd unit"
sudo -n mkdir -p \
  "$MOUNT_POINT/surf" \
  "$MOUNT_POINT/usr/local/bin" \
  "$MOUNT_POINT/var/tmp" \
  "$MOUNT_POINT/var/lib/containers" \
  "$MOUNT_POINT/run/containers" \
  "$MOUNT_POINT/etc/ssl/certs" \
  "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants" \
  "$MOUNT_POINT/etc/containers"
sudo -n chmod 1777 "$MOUNT_POINT/var/tmp"
sudo -n install -m 0755 surfd "$MOUNT_POINT/usr/local/bin/surfd"
sudo -n install -m 0644 surfd.service "$MOUNT_POINT/etc/systemd/system/surfd.service"
sudo -n ln -sf /etc/systemd/system/surfd.service \
  "$MOUNT_POINT/etc/systemd/system/multi-user.target.wants/surfd.service"
echo '# populated by fc-agent start-surfd' | \
  sudo -n tee "$MOUNT_POINT/etc/default/surfd" >/dev/null

if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  sudo -n install -m 0644 /etc/ssl/certs/ca-certificates.crt \
    "$MOUNT_POINT/etc/ssl/certs/ca-certificates.crt"
fi

# --- 2. podman-static --------------------------------------------------------

if [ ! -f "$WORK/podman-static.tgz" ]; then
  log "download podman-static $PODMAN_STATIC_VERSION"
  curl -fsSL -o "$WORK/podman-static.tgz" "$PODMAN_STATIC_URL"
fi
log "inject podman-static"
sudo -n tar -xzf "$WORK/podman-static.tgz" -C "$MOUNT_POINT" --strip-components=1

log "enable podman.socket (docker-compose needs the API)"
sudo -n mkdir -p "$MOUNT_POINT/etc/systemd/system/sockets.target.wants"
sudo -n ln -sf /usr/local/lib/systemd/system/podman.socket \
  "$MOUNT_POINT/etc/systemd/system/sockets.target.wants/podman.socket"

# --- 2b. docker-compose v2 (podman compose provider) ------------------------

if [ ! -f "$WORK/docker-compose" ]; then
  log "download docker-compose $DOCKER_COMPOSE_VERSION"
  curl -fsSL -o "$WORK/docker-compose" "$DOCKER_COMPOSE_URL"
fi
log "inject docker-compose as podman compose provider"
sudo -n mkdir -p "$MOUNT_POINT/usr/libexec/docker/cli-plugins"
sudo -n install -m 0755 "$WORK/docker-compose" \
  "$MOUNT_POINT/usr/libexec/docker/cli-plugins/docker-compose"
sudo -n ln -sf /usr/libexec/docker/cli-plugins/docker-compose \
  "$MOUNT_POINT/usr/local/bin/docker-compose"

# --- 3. iptables bundle (extracted from ubuntu:22.04 via host podman) --------

if [ ! -f "$WORK/iptables-full.tar" ]; then
  log "build iptables bundle from ubuntu:22.04"
  sudo -n podman run --rm -v "$WORK":/out docker.io/library/ubuntu:22.04 bash -c '
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
for n in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
  sudo -n ln -sf xtables-legacy-multi "$MOUNT_POINT/usr/sbin/$n"
done

# --- 4. containers.conf / storage.conf ---------------------------------------

log "write containers.conf (host netns + no firewall)"
sudo -n tee "$MOUNT_POINT/etc/containers/containers.conf" >/dev/null <<'CONF'
[containers]
netns = "host"

[engine]
compose_warning_logs = false

[network]
firewall_driver = "none"
CONF

log "write storage.conf (native overlay)"
sudo -n tee "$MOUNT_POINT/etc/containers/storage.conf" >/dev/null <<'CONF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/var/lib/containers/storage"
[storage.options.overlay]
mountopt = "nodev"
CONF

# --- 5. sanity ---------------------------------------------------------------

log "sanity check"
sudo -n test -x "$MOUNT_POINT/usr/local/bin/surfd"
sudo -n test -x "$MOUNT_POINT/usr/local/bin/podman"
sudo -n test -x "$MOUNT_POINT/usr/libexec/docker/cli-plugins/docker-compose"
sudo -n test -L "$MOUNT_POINT/usr/sbin/iptables"
sudo -n test -f "$MOUNT_POINT/usr/lib/x86_64-linux-gnu/xtables/libxt_comment.so"

sudo -n umount "$MOUNT_POINT"
trap - EXIT

log "done -> $OUT ($(du -h "$OUT" | cut -f1))"
