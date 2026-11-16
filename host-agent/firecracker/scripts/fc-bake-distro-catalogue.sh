#!/usr/bin/env bash
# Bake the shipped distro images: fc-bake-distro-catalogue.sh [name...]
#
#   debian-12      docker.io/library/debian:12
#   ubuntu-24.04   docker.io/library/ubuntu:24.04
#   fedora-40      registry.fedoraproject.org/fedora:40
#
# no names bakes all three. each lands in $IMAGES_DIR/<name>.ext4 via
# fc-bake-distro-rootfs.sh, and a Fusefile boots it with `image: <name>`.
set -euo pipefail

SCRIPTS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ref_for() {
  case "$1" in
    debian-12) echo docker.io/library/debian:12 ;;
    ubuntu-24.04) echo docker.io/library/ubuntu:24.04 ;;
    fedora-40) echo registry.fedoraproject.org/fedora:40 ;;
    *) return 1 ;;
  esac
}

[ $# -gt 0 ] || set -- debian-12 ubuntu-24.04 fedora-40
for name in "$@"; do
  ref_for "$name" >/dev/null \
    || { echo "fc-bake-distro-catalogue: unknown image '$name' (want debian-12, ubuntu-24.04 or fedora-40)" >&2; exit 1; }
done
for name in "$@"; do
  "$SCRIPTS/fc-bake-distro-rootfs.sh" "$(ref_for "$name")" "$name"
done
