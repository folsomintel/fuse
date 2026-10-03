#!/usr/bin/env bash
# One-shot installer: downloads firecracker + kernel + rootfs into this dir.
# Safe to re-run — skips anything already present.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$FC_DIR"

ARCH=$(uname -m)
CI_BASE="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}"

# the pinned release, FIRECRACKER_VERSION in fc-agent.py. installed when
# missing or when the host runs anything else, so a fresh host and an old one
# end up on the same build.
FC_PIN="$(sed -n 's/^FIRECRACKER_VERSION = "\(v[0-9][0-9.]*\)"$/\1/p' fc-agent.py)"
[ -n "$FC_PIN" ] || { echo "[install] no FIRECRACKER_VERSION in fc-agent.py" >&2; exit 1; }
FC_HAVE="$(/usr/local/bin/firecracker --version 2>/dev/null | awk 'NR==1{print $2}')"
if [ "$FC_HAVE" != "$FC_PIN" ]; then
  echo "[install] firecracker $FC_PIN (have: ${FC_HAVE:-none})"
  TMP=$(mktemp -d)
  TGZ="firecracker-${FC_PIN}-${ARCH}.tgz"
  REL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_PIN}"
  curl -fsSL -o "$TMP/$TGZ" "$REL/$TGZ"
  curl -fsSL -o "$TMP/$TGZ.sha256.txt" "$REL/$TGZ.sha256.txt"
  # refuse a truncated or tampered download before anything is installed.
  if ! (cd "$TMP" && sha256sum -c --status "$TGZ.sha256.txt"); then
    echo "[install] checksum mismatch for $TGZ; nothing installed" >&2
    rm -rf "$TMP"
    exit 1
  fi
  tar -xzf "$TMP/$TGZ" -C "$TMP"
  sudo install -m0755 "$TMP/release-${FC_PIN}-${ARCH}/firecracker-${FC_PIN}-${ARCH}" /usr/local/bin/firecracker
  rm -rf "$TMP"
fi

# opt-in: FC_GUEST_KERNEL=6.1 installs the 6.1 ci kernel, which has the
# virtio-mem driver memory hotplug (FC_MEM_HOTPLUG=1 on the agent) needs; 5.10
# does not. an existing vmlinux.bin is kept either way, so switching a host
# means removing it first.
if [ "${FC_GUEST_KERNEL:-5.10}" = "6.1" ]; then
  KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14/${ARCH}/vmlinux-6.1.155"
else
  KERNEL_URL="$CI_BASE/vmlinux-5.10.223"
fi
[ -f vmlinux.bin ]  || curl -fsSL -o vmlinux.bin  "$KERNEL_URL"
[ -f rootfs.ext4 ]  || curl -fsSL -o rootfs.ext4  "$CI_BASE/ubuntu-22.04.ext4"
[ -f ubuntu.id_rsa ] || { curl -fsSL -o ubuntu.id_rsa "$CI_BASE/ubuntu-22.04.id_rsa"; chmod 600 ubuntu.id_rsa; }

# opt-in: reserve 2M hugepages for vms created with huge_pages. the kernel
# takes this memory away from everything else, so it is sized by the operator
# (FC_HUGEPAGES_MB=16384 reserves 16 GiB), never guessed.
if [ -n "${FC_HUGEPAGES_MB:-}" ]; then
  echo "[install] reserving ${FC_HUGEPAGES_MB} MiB of 2M hugepages"
  echo "vm.nr_hugepages = $(( FC_HUGEPAGES_MB / 2 ))" | sudo tee /etc/sysctl.d/90-fuse-hugepages.conf >/dev/null
  sudo sysctl --system >/dev/null
fi

echo "[install] ready. Next: ./fc-up.sh && ./fc-test.sh"
