# shellcheck shell=bash
# shared steps of the rootfs bakes. sourced, not run, by fc-bake-rootfs.sh
# (the ci ubuntu base) and fc-bake-distro-rootfs.sh (debian, ubuntu and fedora
# from an oci ref), so what every image carries is defined once.
#
# every function takes the guest root (a mounted image or an unpacked tree)
# and expects the caller's cwd to be host-agent/firecracker, where fused and
# fused.service live.

# the guest runs on the bake host's arch (firecracker has no cross-arch
# guests), so pick the podman and compose builds for this machine.
case "$(uname -m)" in
  aarch64|arm64) PODMAN_STATIC_ARCH=arm64; DOCKER_COMPOSE_ARCH=aarch64 ;;
  *)             PODMAN_STATIC_ARCH=amd64; DOCKER_COMPOSE_ARCH=x86_64 ;;
esac
PODMAN_STATIC_VERSION=${PODMAN_STATIC_VERSION:-v5.8.1}
PODMAN_STATIC_URL="https://github.com/mgoltzsche/podman-static/releases/download/${PODMAN_STATIC_VERSION}/podman-linux-${PODMAN_STATIC_ARCH}.tar.gz"
DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v2.40.3}
DOCKER_COMPOSE_URL="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${DOCKER_COMPOSE_ARCH}"

# BAKE_STEP is the last step logged, so a failure can say where it died
# shellcheck disable=SC2034
log() { BAKE_STEP=$*; printf '\033[1;36m[bake] %s\033[0m\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }

# bake_fetch <url> <dest>: download once into the work dir. the temp name keeps
# an interrupted download from being cached as a good one.
bake_fetch() {
  [ -f "$2" ] && return 0
  curl -fsSL -o "$2.part" "$1"
  mv -f "$2.part" "$2"
}

# the guest root can be an unpacked image someone else wrote, and every write
# below runs as root on the host. a plain "$root/etc/x" follows the image's
# own symlinks, so a link like /root/.ssh -> /root/.ssh would land the write
# on the host. bake_path and bake_parent resolve a guest path the way the
# guest would, inside root, and every write goes through one of them.

# _bake_resolve <root> <guest-path>: print root plus the guest path with every
# symlink resolved inside root (absolute targets restart at root, '..' stops
# at root). each existing component of the result is a real dir or file. run
# under sudo, since the image's dirs (e.g. /root) may be root-only.
_bake_resolve() {
  local root=$1 rest=$2 cur="" comp link hops=0
  while [ -n "$rest" ]; do
    comp=${rest%%/*}
    if [ "$comp" = "$rest" ]; then rest=""; else rest=${rest#*/}; fi
    case "$comp" in
      ""|.) continue ;;
      ..) cur=${cur%/*}; continue ;;
    esac
    if [ -L "$root$cur/$comp" ]; then
      hops=$((hops + 1))
      [ "$hops" -le 40 ] || { echo "[bake] too many symlinks resolving $2 in $root" >&2; return 1; }
      link=$(readlink "$root$cur/$comp")
      case "$link" in /*) cur="" ;; esac
      rest="$link/$rest"
    else
      cur="$cur/$comp"
    fi
  done
  printf '%s\n' "$root$cur"
}

# bake_path <root> <guest-path>: where a write to guest-path lands, following
# a final symlink too (inside root), the way a write in the guest would
bake_path() {
  sudo -n bash -c "$(declare -f _bake_resolve); _bake_resolve \"\$1\" \"\$2\"" _ "$1" "$2"
}

# bake_parent <root> <guest-path>: the parent resolved, the last name kept as
# is, for what acts on the entry itself (rm, ln -sfn, test -L)
bake_parent() {
  local dir
  dir=$(bake_path "$1" "$(dirname "$2")") || return 1
  printf '%s/%s\n' "$dir" "$(basename "$2")"
}

# bake_write <root> <guest-path> [mode]: stdin into guest-path
bake_write() {
  local dest
  dest=$(bake_path "$1" "$2") || return 1
  sudo -n tee "$dest" >/dev/null
  [ -z "${3:-}" ] || sudo -n chmod "$3" "$dest"
}

# bake_mkdir <root> <guest-path>...: mkdir -p each, inside root
bake_mkdir() {
  local root=$1 p
  shift
  for p in "$@"; do sudo -n mkdir -p "$(bake_path "$root" "$p")"; done
}

# bake_ln <root> <target> <guest-path>: the symlink itself, never through it
bake_ln() {
  sudo -n ln -sfn "$2" "$(bake_parent "$1" "$3")"
}

# bake_rm <root> <guest-path>...: rm -f each entry itself
bake_rm() {
  local root=$1 p
  shift
  for p in "$@"; do sudo -n rm -f "$(bake_parent "$root" "$p")"; done
}

# bake_untar <root> <tgz> <stage> [strip]: tar would follow the image's links
# in paths it extracts through, so unpack into a fresh stage dir first (only
# the archive's own files, whose escaping links gnu tar defers) and copy
# each entry in through bake_path / bake_parent.
bake_untar() {
  local root=$1 tgz=$2 stage=$3 strip=${4:-0} rel
  sudo -n rm -rf "$stage"
  sudo -n mkdir -p "$stage"
  sudo -n tar -xzf "$tgz" -C "$stage" --strip-components="$strip"
  sudo -n find "$stage" -mindepth 1 | while IFS= read -r rel; do
    rel=${rel#"$stage"}
    if sudo -n test -L "$stage$rel"; then
      bake_ln "$root" "$(sudo -n readlink "$stage$rel")" "$rel"
    elif sudo -n test -d "$stage$rel"; then
      bake_mkdir "$root" "$rel"
    else
      sudo -n cp -p "$stage$rel" "$(bake_path "$root" "$rel")"
    fi
  done
  sudo -n rm -rf "$stage"
}

# bake_inject_common <root> <work>: fused + its unit, the dirs fused and podman
# expect, the ca bundle, podman-static, docker-compose and the containers
# config. iptables is not here: how a rootfs gets it depends on its distro.
bake_inject_common() {
  local root=$1 work=$2

  log "inject fused + systemd unit"
  bake_mkdir "$root" \
    /fuse \
    /usr/local/bin \
    /var/tmp \
    /var/lib/containers \
    /run/containers \
    /etc/default \
    /etc/ssl/certs \
    /etc/systemd/system/multi-user.target.wants \
    /etc/containers
  sudo -n chmod 1777 "$(bake_path "$root" /var/tmp)"
  sudo -n install -m 0755 fused "$(bake_path "$root" /usr/local/bin/fused)"
  sudo -n install -m 0644 fused.service "$(bake_path "$root" /etc/systemd/system/fused.service)"
  bake_ln "$root" /etc/systemd/system/fused.service \
    /etc/systemd/system/multi-user.target.wants/fused.service
  echo '# populated by fc-agent start-agent' | bake_write "$root" /etc/default/fused

  if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    sudo -n install -m 0644 /etc/ssl/certs/ca-certificates.crt \
      "$(bake_path "$root" /etc/ssl/certs/ca-certificates.crt)"
  fi

  log "download podman-static $PODMAN_STATIC_VERSION (cached)"
  # the cache name carries version and arch, so a work dir left by an older
  # or other-arch bake never hands this one the wrong binaries.
  local podman_tgz="$work/podman-static-$PODMAN_STATIC_VERSION-$PODMAN_STATIC_ARCH.tgz"
  bake_fetch "$PODMAN_STATIC_URL" "$podman_tgz"
  log "inject podman-static"
  bake_untar "$root" "$podman_tgz" "$work/podman-static.stage" 1

  log "enable podman.socket (docker-compose needs the API)"
  bake_mkdir "$root" /etc/systemd/system/sockets.target.wants
  bake_ln "$root" /usr/local/lib/systemd/system/podman.socket \
    /etc/systemd/system/sockets.target.wants/podman.socket

  log "download docker-compose $DOCKER_COMPOSE_VERSION (cached)"
  local compose_bin="$work/docker-compose-$DOCKER_COMPOSE_VERSION-$DOCKER_COMPOSE_ARCH"
  bake_fetch "$DOCKER_COMPOSE_URL" "$compose_bin"
  log "inject docker-compose as podman compose provider"
  bake_mkdir "$root" /usr/libexec/docker/cli-plugins
  sudo -n install -m 0755 "$compose_bin" \
    "$(bake_path "$root" /usr/libexec/docker/cli-plugins/docker-compose)"
  bake_ln "$root" /usr/libexec/docker/cli-plugins/docker-compose \
    /usr/local/bin/docker-compose

  log "write containers.conf (host netns + no firewall)"
  bake_write "$root" /etc/containers/containers.conf <<'CONF'
[containers]
netns = "host"

[engine]
compose_warning_logs = false

[network]
firewall_driver = "none"
CONF

  log "write storage.conf (native overlay)"
  bake_write "$root" /etc/containers/storage.conf <<'CONF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/var/lib/containers/storage"
[storage.options.overlay]
mountopt = "nodev"
CONF
}

# bake_legacy_iptables <root>: the guest kernel has no nftables, so every
# iptables name points at the legacy multi-call binary.
bake_legacy_iptables() {
  local n
  for n in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
    bake_ln "$1" xtables-legacy-multi "/usr/sbin/$n"
  done
}

# bake_check <root> <test-flag> <path>: name the missing artefact. a bare
# `test` under `set -e` aborts with no output, which is how a half-baked image
# previously reached a boot attempt and only failed later with "agent binary
# not found at /usr/local/bin/fused".
bake_check() {
  local p
  if [ "$2" = -L ]; then p=$(bake_parent "$1" "$3"); else p=$(bake_path "$1" "$3"); fi
  sudo -n test "$2" "$p" \
    || { echo "[bake] sanity check FAILED: $3 missing or wrong type ($2)" >&2; exit 1; }
}

# bake_check_common <root>: what bake_inject_common and bake_legacy_iptables put in.
bake_check_common() {
  bake_check "$1" -x /usr/local/bin/fused
  bake_check "$1" -x /usr/local/bin/podman
  bake_check "$1" -x /usr/libexec/docker/cli-plugins/docker-compose
  bake_check "$1" -L /usr/sbin/iptables
}
