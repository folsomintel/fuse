#!/usr/bin/env bash
# Unit test for fc-bake-distro-rootfs.sh against stubbed sudo, podman, curl,
# ssh-keygen and mkfs.ext4: argument validation, the unsupported-distro
# refusal, failure reporting, the atomic write, the per-name work dir and an
# image whose symlinks point out of the tree.
# The real bake (pull, apt/dnf, boot) is not exercised here.
#
# No network, no root, no VMs:  ./host-agent/firecracker/tests/test-bake-distro-rootfs.sh
set -uo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
# check <name> <cond...>
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }

# a copy of host-agent/firecracker with the inputs the bake wants
FC="$WORK/fc"
mkdir -p "$FC/scripts" "$FC/images"
cp "$SRC/scripts/fc-bake-distro-rootfs.sh" "$SRC/scripts/fc-bake-lib.sh" "$FC/scripts/"
cp "$SRC/fused.service" "$FC/"
printf '#!/bin/sh\n' > "$FC/fused"
echo key > "$FC/ubuntu.id_rsa"
BAKE="$FC/scripts/fc-bake-distro-rootfs.sh"

# the fs the stub container exports: just what the sanity check looks for
EXPORT="$WORK/export"
mkdir -p "$EXPORT/usr/sbin" "$EXPORT/etc" "$EXPORT/root"
for b in sshd xtables-legacy-multi; do printf '#!/bin/sh\n' > "$EXPORT/usr/sbin/$b"; chmod +x "$EXPORT/usr/sbin/$b"; done

STUBS="$WORK/bin"
mkdir -p "$STUBS"
cat > "$STUBS/sudo" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = "-n" ] && shift
exec "$@"
EOF
cat > "$STUBS/podman" <<'EOF'
#!/usr/bin/env bash
echo "podman $*" >> "$STUB_LOG"
case "$1" in
  pull) [ -z "${STUB_PULL_FAIL:-}" ] || { echo "Error: pull denied" >&2; exit 125; } ;;
  run)
    if [ "$2" = "--rm" ]; then echo "${STUB_PM:-apt}"
    else [ -z "${STUB_INSTALL_FAIL:-}" ] || exit 100
    fi ;;
  export) tar -cf - -C "$STUB_EXPORT" . ;;
esac
exit 0
EOF
cat > "$STUBS/curl" <<'EOF'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done
case "$url" in
  *podman*)
    d=$(mktemp -d); mkdir -p "$d/p/usr/local/bin"
    printf '#!/bin/sh\n' > "$d/p/usr/local/bin/podman"; chmod +x "$d/p/usr/local/bin/podman"
    tar -czf "$out" -C "$d" p; rm -rf "$d" ;;
  *) printf '#!/bin/sh\n' > "$out" ;;
esac
EOF
cat > "$STUBS/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
echo "ssh-rsa AAAAstub test"
EOF
cat > "$STUBS/mkfs.ext4" <<'EOF'
#!/usr/bin/env bash
[ -z "${STUB_MKFS_FAIL:-}" ] || exit 1
prev=
for a in "$@"; do [ "$prev" != -d ] || dir=$a; prev=$a; out=$a; done
# keep the tree mkfs was handed, for the hostile image checks
[ -z "${STUB_SNAP:-}" ] || cp -a "$dir" "$STUB_SNAP"
echo "ext4 $*" > "$out"
EOF
cat > "$STUBS/truncate" <<'EOF'
#!/usr/bin/env bash
: > "${!#}"
EOF
# macos has no flock; a no-op stand-in keeps the rest of the test running there
if ! command -v flock >/dev/null 2>&1; then
  printf '#!/bin/sh\nexit 0\n' > "$STUBS/flock"
  NO_FLOCK=1
fi
chmod +x "$STUBS"/*

export PATH="$STUBS:$PATH" STUB_LOG="$WORK/podman.log" STUB_EXPORT="$EXPORT"
export IMAGES_DIR="$FC/images"

# run <args...>: rc in RC, stderr's last line in LAST
run() {
  : > "$STUB_LOG"
  FC_BAKE_WORK=${FC_BAKE_WORK_OVERRIDE-$WORK/work} "$BAKE" "$@" >/dev/null 2>"$WORK/err"
  RC=$?
  LAST=$(awk 'NF{l=$0} END{print l}' "$WORK/err")
}
# the lock file is kept on purpose: removing it would race the next bake
images() { (cd "$IMAGES_DIR" && find . -mindepth 1 -maxdepth 1 ! -name '*.lock' | sed 's|^\./||'); }
no_partial() { [ -z "$(images)" ]; }

printf '\n== arguments ==\n'
run
check "no args exits nonzero with usage" [ "$RC" -ne 0 ]
check "usage on stderr" grep -q usage <<<"$LAST"
for n in a/b .. ../x .hidden a..b "" "-x" "a b"; do
  run docker.io/library/debian:12 "$n"
  check "name '$n' refused" [ "$RC" -ne 0 ]
  check "name '$n' says invalid name" grep -q "invalid name" <<<"$LAST"
done
check "a refused name never reaches podman" [ ! -s "$STUB_LOG" ]

printf '\n== unsupported distro ==\n'
STUB_PM="none:alpine" run docker.io/library/alpine:3.20 alpine
check "alpine exits nonzero" [ "$RC" -ne 0 ]
check "alpine reason names the distro" grep -q "unsupported distro 'alpine'" <<<"$LAST"
check "alpine leaves nothing in IMAGES_DIR" no_partial

printf '\n== failures report one line ==\n'
STUB_PULL_FAIL=1 run docker.io/library/debian:12 debian-12
check "pull failure exits nonzero" [ "$RC" -ne 0 ]
check "pull failure reason" grep -q "podman pull docker.io/library/debian:12 failed" <<<"$LAST"
STUB_INSTALL_FAIL=1 run docker.io/library/debian:12 debian-12
check "install failure reason" grep -q "installing packages with apt" <<<"$LAST"
STUB_MKFS_FAIL=1 run docker.io/library/debian:12 debian-12
check "mkfs failure exits nonzero" [ "$RC" -ne 0 ]
check "mkfs failure reason" grep -q "mkfs.ext4 -d failed" <<<"$LAST"
check "mkfs failure leaves no partial image" no_partial
rm "$EXPORT/usr/sbin/sshd"
run docker.io/library/debian:12 debian-12
check "missing sshd fails the sanity check" grep -q "/usr/sbin/sshd missing" "$WORK/err"
check "sanity failure ends with the step it died in" grep -q "failed during 'sanity check'" <<<"$LAST"
check "sanity failure leaves no partial image" no_partial
printf '#!/bin/sh\n' > "$EXPORT/usr/sbin/sshd"; chmod +x "$EXPORT/usr/sbin/sshd"

printf '\n== hostile image symlinks stay inside the tree ==\n'
# every link points at VICTIM, which stands in for the host: absolute links,
# a relative one that climbs past the root, and a final-component link
VICTIM="$WORK/victim"
mkdir -p "$VICTIM/ssh" "$VICTIM/tmp" "$VICTIM/usrlocal/bin" "$VICTIM/systemd" "$VICTIM/sbin" "$VICTIM/run"
echo "host key" > "$VICTIM/ssh/authorized_keys"
echo host > "$VICTIM/hostname"
: > "$VICTIM/run/.containerenv"
chmod 0755 "$VICTIM/tmp"
HOSTILE="$WORK/hostile"
mkdir -p "$HOSTILE/usr/sbin" "$HOSTILE/usr/share" "$HOSTILE/etc" "$HOSTILE/root"
for b in sshd xtables-legacy-multi; do printf '#!/bin/sh\n' > "$HOSTILE/usr/sbin/$b"; chmod +x "$HOSTILE/usr/sbin/$b"; done
ln -s "$VICTIM/ssh" "$HOSTILE/root/.ssh"
# /var/tmp, not /tmp: VICTIM lives under /tmp on linux, so a hostile /tmp
# link would loop (which the bake refuses) instead of escaping
mkdir -p "$HOSTILE/var"
ln -s "$VICTIM/tmp" "$HOSTILE/var/tmp"
ln -s "$VICTIM/run" "$HOSTILE/run"
ln -s "$VICTIM/usrlocal" "$HOSTILE/usr/local"
ln -s "../../../../../../../../..$VICTIM/systemd" "$HOSTILE/etc/systemd"
ln -s "$VICTIM/sbin" "$HOSTILE/usr/sbin/iptables"
ln -s "$VICTIM/hostname" "$HOSTILE/etc/hostname"
# an ordinary relative link (fedora's /etc/ssl/certs is one) still resolves
ln -s ../usr/share/containers "$HOSTILE/etc/containers"
fingerprint() { ls -lRA "$VICTIM"; find "$VICTIM" -type f -exec cat {} +; }
BEFORE=$(fingerprint)
SNAP="$WORK/snap"
STUB_EXPORT="$HOSTILE" STUB_SNAP="$SNAP" run docker.io/library/debian:12 hostile
check "hostile image bakes" [ "$RC" -eq 0 ]
check "nothing outside the tree was written" [ "$(fingerprint)" = "$BEFORE" ]
check "root's key lands inside the tree" grep -q AAAAstub "$SNAP$VICTIM/ssh/authorized_keys"
check "podman-static lands inside the tree" [ -x "$SNAP$VICTIM/usrlocal/bin/podman" ]
check "fused unit link lands inside the tree" [ -L "$SNAP$VICTIM/systemd/system/multi-user.target.wants/fused.service" ]
check "iptables link replaced, not followed" [ "$(readlink "$SNAP/usr/sbin/iptables")" = xtables-legacy-multi ]
check "hostname written through the link inside the tree" grep -qx fuse "$SNAP$VICTIM/hostname"
check "relative link still resolves" [ -f "$SNAP/usr/share/containers/containers.conf" ]
rm -rf "$SNAP" "$IMAGES_DIR/hostile.ext4"

printf '\n== bake ==\n'
DIGEST=docker.io/library/debian@sha256:$(printf '%064d' 0)
run "$DIGEST" debian-12
check "digest-pinned ref bakes" [ "$RC" -eq 0 ]
check "digest ref reaches podman pull verbatim" grep -qF "podman pull -q $DIGEST" "$STUB_LOG"
check "image written" [ -s "$IMAGES_DIR/debian-12.ext4" ]
check "only the image is left in IMAGES_DIR" [ "$(images)" = "debian-12.ext4" ]
check "unpacked root removed" [ ! -e "$WORK/work/rootfs" ]
run "$DIGEST" debian-12
check "re-run with the same args succeeds" [ "$RC" -eq 0 ]
check "re-run still leaves only the image" [ "$(images)" = "debian-12.ext4" ]

if [ -z "${NO_FLOCK:-}" ]; then
  printf '\n== a second bake of the same name is refused ==\n'
  # stand in for a running bake: hold the lock, with its temp image present
  echo partial > "$IMAGES_DIR/.debian-12.ext4.tmp"
  # exec, so the pid killed below is the one holding the lock fd
  ( exec 8>>"$IMAGES_DIR/.debian-12.ext4.lock"; flock 8; exec sleep 30 ) &
  HOLDER=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    flock -n "$IMAGES_DIR/.debian-12.ext4.lock" true || break
    sleep 0.1
  done
  run "$DIGEST" debian-12
  check "concurrent bake exits nonzero" [ "$RC" -ne 0 ]
  check "concurrent bake says another is running" grep -q "another bake of 'debian-12' is running" <<<"$LAST"
  check "concurrent bake never reaches podman" [ ! -s "$STUB_LOG" ]
  check "concurrent bake leaves the other run's temp image" [ -f "$IMAGES_DIR/.debian-12.ext4.tmp" ]
  kill "$HOLDER" 2>/dev/null; wait "$HOLDER" 2>/dev/null
  rm -f "$IMAGES_DIR/.debian-12.ext4.tmp"
  run "$DIGEST" debian-12
  check "bake runs once the lock is free" [ "$RC" -eq 0 ]
fi

NAME="fcbake-test-$$"
FC_BAKE_WORK_OVERRIDE="" run docker.io/library/debian:12 "$NAME"
check "default work dir is keyed by name" [ -d "/tmp/fcbake-distro-$NAME" ]
rm -rf "/tmp/fcbake-distro-$NAME"
rm -f "$IMAGES_DIR/$NAME.ext4"

printf '\n== desktop bake refuses a non-22.04 base ==\n'
# the desktop script finds its units at ../../ops/systemd, so it gets a tree
# of its own; mount does nothing and the "mounted" base is a plain dir
DFC="$WORK/repo/host-agent/firecracker"
mkdir -p "$DFC/scripts" "$WORK/repo/ops/systemd"
cp "$SRC/scripts/fc-bake-desktop-rootfs.sh" "$DFC/scripts/"
for r in fuse-display-run fuse-wm-run; do : > "$DFC/$r"; done
for u in fuse-display fuse-wm fuse-panel fuse-vnc; do : > "$WORK/repo/ops/systemd/$u.service"; done
for c in mount umount e2fsck resize2fs chroot; do printf '#!/bin/sh\nexit 0\n' > "$STUBS/$c"; chmod +x "$STUBS/$c"; done
DMNT="$WORK/desktop-mnt"
mkdir -p "$DMNT/etc"
printf 'ID=debian\nVERSION_ID="12"\n' > "$DMNT/etc/os-release"
echo base > "$DFC/base.ext4"
echo "old desktop" > "$DFC/out.ext4"
FC_BAKE_MOUNT="$DMNT" FC_BAKE_WORK="$WORK/desktop-work" \
  "$DFC/scripts/fc-bake-desktop-rootfs.sh" "$DFC/base.ext4" "$DFC/out.ext4" >/dev/null 2>"$WORK/err"
RC=$?
check "debian base refused" [ "$RC" -ne 0 ]
check "refusal names the base release" grep -q "is debian-12, not ubuntu-22.04" "$WORK/err"
check "refused base leaves the existing output alone" grep -qx "old desktop" "$DFC/out.ext4"
check "refused base leaves no temp image" [ ! -e "$DFC/.out.ext4.tmp" ]

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
