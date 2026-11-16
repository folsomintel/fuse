#!/usr/bin/env bash
# Unit test for fc-bake-distro-rootfs.sh against stubbed sudo, podman, curl,
# ssh-keygen and mkfs.ext4: argument validation, the unsupported-distro
# refusal, failure reporting, the atomic write and the per-name work dir.
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
for a in "$@"; do out=$a; done
echo "ext4 $*" > "$out"
EOF
cat > "$STUBS/truncate" <<'EOF'
#!/usr/bin/env bash
: > "${!#}"
EOF
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
no_partial() { [ -z "$(ls -A "$IMAGES_DIR")" ]; }

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

printf '\n== bake ==\n'
DIGEST=docker.io/library/debian@sha256:$(printf '%064d' 0)
run "$DIGEST" debian-12
check "digest-pinned ref bakes" [ "$RC" -eq 0 ]
check "digest ref reaches podman pull verbatim" grep -qF "podman pull -q $DIGEST" "$STUB_LOG"
check "image written" [ -s "$IMAGES_DIR/debian-12.ext4" ]
check "only the image is left in IMAGES_DIR" [ "$(ls -A "$IMAGES_DIR")" = "debian-12.ext4" ]
check "unpacked root removed" [ ! -e "$WORK/work/rootfs" ]
run "$DIGEST" debian-12
check "re-run with the same args succeeds" [ "$RC" -eq 0 ]
check "re-run still leaves only the image" [ "$(ls -A "$IMAGES_DIR")" = "debian-12.ext4" ]

NAME="fcbake-test-$$"
FC_BAKE_WORK_OVERRIDE="" run docker.io/library/debian:12 "$NAME"
check "default work dir is keyed by name" [ -d "/tmp/fcbake-distro-$NAME" ]
rm -rf "/tmp/fcbake-distro-$NAME"
rm -f "$IMAGES_DIR/$NAME.ext4"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
