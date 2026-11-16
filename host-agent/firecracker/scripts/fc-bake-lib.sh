# shellcheck shell=bash
# shared steps of the rootfs bakes. sourced, not run, by fc-bake-rootfs.sh
# (the ci ubuntu base) and fc-bake-distro-rootfs.sh (debian, ubuntu and fedora
# from an oci ref), so what every image carries is defined once.
#
# every function takes the guest root (a mounted image or an unpacked tree)
# and expects the caller's cwd to be host-agent/firecracker, where fused and
# fused.service live.

PODMAN_STATIC_VERSION=${PODMAN_STATIC_VERSION:-v5.8.1}
PODMAN_STATIC_URL="https://github.com/mgoltzsche/podman-static/releases/download/${PODMAN_STATIC_VERSION}/podman-linux-amd64.tar.gz"
DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v2.40.3}
DOCKER_COMPOSE_URL="https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-x86_64"

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

# bake_inject_common <root> <work>: fused + its unit, the dirs fused and podman
# expect, the ca bundle, podman-static, docker-compose and the containers
# config. iptables is not here: how a rootfs gets it depends on its distro.
bake_inject_common() {
  local root=$1 work=$2

  log "inject fused + systemd unit"
  sudo -n mkdir -p \
    "$root/fuse" \
    "$root/usr/local/bin" \
    "$root/var/tmp" \
    "$root/var/lib/containers" \
    "$root/run/containers" \
    "$root/etc/default" \
    "$root/etc/ssl/certs" \
    "$root/etc/systemd/system/multi-user.target.wants" \
    "$root/etc/containers"
  sudo -n chmod 1777 "$root/var/tmp"
  sudo -n install -m 0755 fused "$root/usr/local/bin/fused"
  sudo -n install -m 0644 fused.service "$root/etc/systemd/system/fused.service"
  sudo -n ln -sf /etc/systemd/system/fused.service \
    "$root/etc/systemd/system/multi-user.target.wants/fused.service"
  echo '# populated by fc-agent start-agent' | \
    sudo -n tee "$root/etc/default/fused" >/dev/null

  if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    sudo -n install -m 0644 /etc/ssl/certs/ca-certificates.crt \
      "$root/etc/ssl/certs/ca-certificates.crt"
  fi

  log "download podman-static $PODMAN_STATIC_VERSION (cached)"
  bake_fetch "$PODMAN_STATIC_URL" "$work/podman-static.tgz"
  log "inject podman-static"
  sudo -n tar -xzf "$work/podman-static.tgz" -C "$root" --strip-components=1

  log "enable podman.socket (docker-compose needs the API)"
  sudo -n mkdir -p "$root/etc/systemd/system/sockets.target.wants"
  sudo -n ln -sf /usr/local/lib/systemd/system/podman.socket \
    "$root/etc/systemd/system/sockets.target.wants/podman.socket"

  log "download docker-compose $DOCKER_COMPOSE_VERSION (cached)"
  bake_fetch "$DOCKER_COMPOSE_URL" "$work/docker-compose"
  log "inject docker-compose as podman compose provider"
  sudo -n mkdir -p "$root/usr/libexec/docker/cli-plugins"
  sudo -n install -m 0755 "$work/docker-compose" \
    "$root/usr/libexec/docker/cli-plugins/docker-compose"
  sudo -n ln -sf /usr/libexec/docker/cli-plugins/docker-compose \
    "$root/usr/local/bin/docker-compose"

  log "write containers.conf (host netns + no firewall)"
  sudo -n tee "$root/etc/containers/containers.conf" >/dev/null <<'CONF'
[containers]
netns = "host"

[engine]
compose_warning_logs = false

[network]
firewall_driver = "none"
CONF

  log "write storage.conf (native overlay)"
  sudo -n tee "$root/etc/containers/storage.conf" >/dev/null <<'CONF'
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
    sudo -n ln -sf xtables-legacy-multi "$1/usr/sbin/$n"
  done
}

# bake_check <root> <test-flag> <path>: name the missing artefact. a bare
# `test` under `set -e` aborts with no output, which is how a half-baked image
# previously reached a boot attempt and only failed later with "agent binary
# not found at /usr/local/bin/fused".
bake_check() {
  sudo -n test "$2" "$1$3" \
    || { echo "[bake] sanity check FAILED: $3 missing or wrong type ($2)" >&2; exit 1; }
}

# bake_check_common <root>: what bake_inject_common and bake_legacy_iptables put in.
bake_check_common() {
  bake_check "$1" -x /usr/local/bin/fused
  bake_check "$1" -x /usr/local/bin/podman
  bake_check "$1" -x /usr/libexec/docker/cli-plugins/docker-compose
  bake_check "$1" -L /usr/sbin/iptables
}
