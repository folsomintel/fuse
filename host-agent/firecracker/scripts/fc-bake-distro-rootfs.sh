#!/usr/bin/env bash
# Bake a bootable rootfs from an OCI image: fc-bake-distro-rootfs.sh <oci-ref> <name>
#
#   ./scripts/fc-bake-distro-rootfs.sh docker.io/library/debian:12 debian-12
#   ./scripts/fc-bake-distro-rootfs.sh docker.io/library/ubuntu@sha256:<digest> my-ubuntu
#
# writes $IMAGES_DIR/<name>.ext4 (IMAGES_DIR defaults to ./images, the dir
# fc-agent.py resolves a Fusefile `image:` against). a Fusefile then boots it
# with `image: <name>`. fc-bake-distro-catalogue.sh bakes the shipped names.
#
# the image is pulled, systemd, sshd, iptables and ca-certificates are
# installed with its own apt-get or dnf (any other package manager, e.g.
# alpine's apk, is refused), and then it gets what the base bake injects:
# fused + fused.service, podman-static, docker-compose and the containers
# config (fc-bake-lib.sh). the fc-agent reaches the guest over ssh as root,
# so root's authorized_keys is set from ubuntu.id_rsa, the key it uses.
#
# non-interactive (sudo -n) and idempotent: every run starts from a fresh
# unpack and the image lands with an atomic mv, so a failed or interrupted
# run never leaves a partial <name>.ext4. any failure exits nonzero with a
# one-line reason as the last line on stderr.
#
# Requires: sudo (passwordless), podman, tar, mkfs.ext4 (e2fsprogs >= 1.43
# for -d), truncate, curl, ssh-keygen.
#
# Inputs in host-agent/firecracker/: fused, fused.service, ubuntu.id_rsa.
#
# Env: IMAGES_DIR (output dir), FC_BAKE_WORK (work dir, default
# /tmp/fcbake-distro-<name>, so parallel bakes of different names never share
# one), FC_ROOTFS_SIZE (default 4G, sparse).
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$FC_DIR"

# shellcheck source=fc-bake-lib.sh
source "$FC_DIR/scripts/fc-bake-lib.sh"

DIED=
die() { echo "fc-bake-distro-rootfs: $*" >&2; DIED=1; exit 1; }

[ $# -eq 2 ] || die "usage: fc-bake-distro-rootfs.sh <oci-ref> <name>"
REF=$1
NAME=$2

# the name becomes a file under IMAGES_DIR and a Fusefile image name, so it
# must not be able to escape the dir or hide as a dotfile
if ! [[ "$NAME" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || [[ "$NAME" == *..* ]]; then
  die "invalid name '$NAME': want letters, digits, '.', '_' or '-', starting with a letter or digit, no '..'"
fi
[ -n "$REF" ] || die "empty oci ref"
case "$REF" in -*) die "invalid oci ref '$REF'" ;; esac

IMAGES_DIR=${IMAGES_DIR:-$FC_DIR/images}
WORK=${FC_BAKE_WORK:-/tmp/fcbake-distro-$NAME}
SIZE=${FC_ROOTFS_SIZE:-4G}
ROOT="$WORK/rootfs"
OUT="$IMAGES_DIR/$NAME.ext4"
TMP_OUT="$IMAGES_DIR/.$NAME.ext4.tmp"
CTR="fcbake-distro-$NAME"

for c in sudo podman tar mkfs.ext4 truncate curl ssh-keygen; do
  command -v "$c" >/dev/null 2>&1 || die "missing: $c"
done
sudo -n true 2>/dev/null || die "needs passwordless sudo (sudo -n failed)"

[ -f fused ] || die "fused not found in $FC_DIR (run ../shared/fc-build-agent.sh)"
[ -f fused.service ] || die "fused.service not found in $FC_DIR"
[ -f ubuntu.id_rsa ] || die "ubuntu.id_rsa not found in $FC_DIR (run ./scripts/fc-install.sh)"
PUBKEY=$(ssh-keygen -y -f ubuntu.id_rsa 2>/dev/null) || die "cannot read a public key from ubuntu.id_rsa"

# an exit trap, not an err trap: set -e failing inside a sourced lib function
# never reaches a top-level err trap, and the last stderr line must say why
on_exit() {
  local rc=$?
  sudo -n podman rm -f "$CTR" >/dev/null 2>&1 || true
  rm -f "$TMP_OUT" 2>/dev/null || sudo -n rm -f "$TMP_OUT" 2>/dev/null || true
  if [ "$rc" -ne 0 ] && [ -z "$DIED" ]; then
    echo "fc-bake-distro-rootfs: failed during '${BAKE_STEP:-setup}' (exit $rc); $OUT was not written" >&2
  fi
}
trap on_exit EXIT

mkdir -p "$WORK" "$IMAGES_DIR"

log "pull $REF"
# stdin from /dev/null: podman asks which registry a short name means when it
# has a tty, and this must never prompt
sudo -n podman pull -q "$REF" </dev/null >/dev/null || die "podman pull $REF failed"

# --- 1. pick the package manager, refuse anything without apt-get or dnf -----

# shellcheck disable=SC2016
PM=$(sudo -n podman run --rm --network=host --entrypoint /bin/sh "$REF" -c '
  if command -v apt-get >/dev/null 2>&1; then echo apt
  elif command -v dnf >/dev/null 2>&1; then echo dnf
  else . /etc/os-release 2>/dev/null; echo "none:${ID:-unknown}"
  fi' 2>/dev/null) || die "cannot run /bin/sh in $REF (distroless or not a linux image?)"
case "$PM" in
  apt|dnf) ;;
  none:*) die "unsupported distro '${PM#none:}' in $REF: only apt-get (debian, ubuntu) and dnf (fedora) images can be baked" ;;
  *) die "could not detect the package manager in $REF" ;;
esac
log "package manager: $PM"

# --- 2. install systemd, sshd, iptables in a container, then export it ------

# the guest kernel has no nftables, so iptables must carry the legacy backend;
# fedora ships it as its own package. ssh-keygen -A bakes host keys in, so the
# first boot does not stall generating them.
APT_INSTALL='set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  systemd systemd-sysv dbus openssh-server iproute2 iptables ca-certificates curl procps >/dev/null
apt-get clean
rm -rf /var/lib/apt/lists/*
# the package enables ssh.service (debian) or ssh.socket (ubuntu 24.04); only
# enable the service when neither is
systemctl is-enabled ssh.service >/dev/null 2>&1 \
  || systemctl is-enabled ssh.socket >/dev/null 2>&1 \
  || systemctl enable ssh.service'
DNF_INSTALL='set -e
dnf install -y -q --setopt=install_weak_deps=False \
  systemd dbus-broker openssh-server iproute iptables-legacy ca-certificates procps-ng >/dev/null
dnf clean all >/dev/null
systemctl enable sshd'
if [ "$PM" = apt ]; then INSTALL=$APT_INSTALL; else INSTALL=$DNF_INSTALL; fi
INSTALL="$INSTALL
ssh-keygen -A >/dev/null
test -x /sbin/init || test -x /usr/sbin/init || test -x /lib/systemd/systemd"

log "install systemd + sshd + iptables ($PM)"
sudo -n podman rm -f "$CTR" >/dev/null 2>&1 || true
sudo -n podman run --name "$CTR" --network=host --entrypoint /bin/sh "$REF" -c "$INSTALL" \
  || die "installing packages with $PM in $REF failed"

log "export container fs -> $ROOT"
sudo -n rm -rf "$ROOT"
sudo -n mkdir -p "$ROOT"
sudo -n podman export "$CTR" | sudo -n tar -xf - -C "$ROOT" --numeric-owner \
  || die "exporting the container fs failed"
sudo -n podman rm -f "$CTR" >/dev/null
# mount points the kernel and systemd need, whether or not the export kept
# them. the tree is the image's, so every write from here on goes through
# bake_path / bake_parent (fc-bake-lib.sh) and stays inside it
bake_mkdir "$ROOT" /dev /proc /sys /run /tmp
sudo -n chmod 1777 "$(bake_path "$ROOT" /tmp)"

# --- 3. what every image carries (shared with fc-bake-rootfs.sh) -------------

bake_inject_common "$ROOT" "$WORK"
bake_legacy_iptables "$ROOT"

# --- 4. guest plumbing a container image lacks ------------------------------

log "root ssh key, hostname, init"
bake_mkdir "$ROOT" /root/.ssh
sudo -n chmod 0700 "$(bake_path "$ROOT" /root/.ssh)"
printf '%s\n' "$PUBKEY" | bake_write "$ROOT" /root/.ssh/authorized_keys 0600
echo fuse | bake_write "$ROOT" /etc/hostname
printf '127.0.0.1 localhost fuse\n::1 localhost\n' | bake_write "$ROOT" /etc/hosts
# podman export leaves the bind-mounted resolv.conf behind as a dangling or
# empty file; the fc-agent writes a real one after boot
bake_rm "$ROOT" /etc/resolv.conf
bake_write "$ROOT" /etc/resolv.conf </dev/null
# an empty machine-id makes systemd generate one on first boot
bake_rm "$ROOT" /etc/machine-id
bake_write "$ROOT" /etc/machine-id </dev/null
# podman leaves a marker that makes systemd think it runs in a container
bake_rm "$ROOT" /run/.containerenv /.dockerenv

# --- 5. sanity ---------------------------------------------------------------

log "sanity check"
bake_check_common "$ROOT"
bake_check "$ROOT" -x /usr/sbin/xtables-legacy-multi
bake_check "$ROOT" -x /usr/sbin/sshd
bake_check "$ROOT" -f /root/.ssh/authorized_keys

# --- 6. mkfs, then move into place -------------------------------------------

log "mkfs.ext4 $SIZE -> $OUT"
rm -f "$TMP_OUT" 2>/dev/null || sudo -n rm -f "$TMP_OUT"
truncate -s "$SIZE" "$TMP_OUT"
sudo -n mkfs.ext4 -q -F -L rootfs -d "$ROOT" "$TMP_OUT" >/dev/null \
  || die "mkfs.ext4 -d failed (image larger than $SIZE? raise FC_ROOTFS_SIZE)"
mv -f "$TMP_OUT" "$OUT"
sudo -n rm -rf "$ROOT"

log "done -> $OUT ($(du -h "$OUT" | cut -f1))"
