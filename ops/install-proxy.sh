#!/usr/bin/env bash
# install-proxy.sh - install and start fuse-proxy, the stable-ingress proxy,
# so bring-up is one script instead of clone-and-build.
#
# fuse-proxy gives every environment a url that does not change when the
# environment moves between hosts. Clients connect to a public port here; the
# guest dials out to this machine and the proxy shuttles bytes between the two.
# See internal/ingress and internal/tunnel.
#
# what it does:
#   1. installs the binary to /usr/local/bin/fuse-proxy (from
#      FUSE_PROXY_BIN_SRC, a local ./fuse-proxy, or the latest release)
#   2. writes /etc/fuse/proxy.env with a generated FUSE_PROXY_ADMIN_TOKEN and
#      the public host (only if the file does not already exist)
#   3. installs the systemd unit and starts it
#   4. prints the two lines to add to the orchestrator's env file
#
# this host needs to be reachable BY BOTH SIDES: clients reach the public port
# range, and every firecracker host's guests reach the tunnel port. The admin
# api is loopback by default and must stay off the public internet -- it mints
# and moves routes.
#
# usage:
#   sudo ./install-proxy.sh
#   FUSE_PROXY_PUBLIC_HOST=proxy.example.com sudo -E ./install-proxy.sh
#   FUSE_PROXY_BIN_SRC=/path/to/fuse-proxy sudo -E ./install-proxy.sh
#   FUSE_REPO=folsomintel/fuse VERSION=v0.4.0 sudo -E ./install-proxy.sh
set -euo pipefail

BIN=/usr/local/bin/fuse-proxy
ENV_DIR=/etc/fuse
ENV_FILE="$ENV_DIR/proxy.env"
UNIT=/etc/systemd/system/fuse-proxy.service
REPO="${FUSE_REPO:-folsomintel/fuse}"
TUNNEL_LISTEN="${FUSE_PROXY_TUNNEL_LISTEN:-:7443}"
ADMIN_LISTEN="${FUSE_PROXY_ADMIN_LISTEN:-127.0.0.1:7080}"
PORTS="${FUSE_PROXY_PORTS:-20000-29999}"
STATE_DIR="${FUSE_PROXY_STATE_DIR:-/var/lib/fuse-proxy}"

log() { echo "[install-proxy] $*"; }
die() { echo "[install-proxy] error: $*" >&2; exit 1; }

# >>> fuse release-asset checksum verification >>>
# This block is embedded, not sourced: ops/install-orchestrator.sh is
# documented as curl-able and run standalone on a fresh host, so a sibling
# library file is not guaranteed to exist on disk. Keep the copies in
# host-agent/firecracker/fc-update.sh, host-agent/firecracker/fc-agent.sh,
# host-agent/local/fuse-local-setup.sh and ops/install-orchestrator.sh
# byte-identical;
# host-agent/firecracker/test-verify-checksum.sh asserts that and exercises the helper.

# sha256_of <file> - print the file's lowercase sha256, or fail if no tool.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    return 1
  fi
}

# verify_asset <file> <asset-name> <checksums-file>
# Fails closed. A missing checksum tool, a missing or empty checksums file, a
# missing entry for this exact asset name, or a digest mismatch all return
# non-zero, so the caller must not install, extract, or execute the asset.
verify_asset() {
  local file="$1" name="$2" sums="$3" want="" got="" sum path
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    echo "checksum verification needs sha256sum or shasum; refusing $name" >&2
    return 1
  fi
  if [ ! -f "$file" ]; then
    echo "asset $name is missing; nothing to verify" >&2
    return 1
  fi
  if [ ! -s "$sums" ]; then
    echo "checksums.txt is missing or empty; refusing $name" >&2
    return 1
  fi
  # goreleaser writes "<sha256>  <asset>"; binary-mode tools prefix the name "*".
  while read -r sum path; do
    case "$path" in
      "$name"|"*$name") want="$sum"; break ;;
    esac
  done < "$sums"
  if [ -z "$want" ]; then
    echo "no checksum entry for $name in checksums.txt; refusing it" >&2
    return 1
  fi
  got="$(sha256_of "$file")" || return 1
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $name: want $want, got $got" >&2
    return 1
  fi
}
# <<< fuse release-asset checksum verification <<<

[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0)"
command -v openssl >/dev/null 2>&1 || die "openssl is required (token generation)"
command -v systemctl >/dev/null 2>&1 || die "systemctl is required (this installer targets systemd hosts)"

# 1. resolve and install the binary.
case "$(uname -m)" in
  x86_64)        ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) die "unsupported arch $(uname -m); set FUSE_PROXY_BIN_SRC=/path/to/fuse-proxy" ;;
esac

if [ -n "${FUSE_PROXY_BIN_SRC:-}" ] && [ -x "$FUSE_PROXY_BIN_SRC" ]; then
  install -m0755 "$FUSE_PROXY_BIN_SRC" "$BIN"
  log "installed binary from $FUSE_PROXY_BIN_SRC"
elif [ -x "./fuse-proxy" ]; then
  install -m0755 "./fuse-proxy" "$BIN"
  log "installed binary from ./fuse-proxy"
else
  command -v curl >/dev/null 2>&1 || die "curl is required to download a release (or set FUSE_PROXY_BIN_SRC)"
  TAG="${VERSION:-}"
  if [ -z "$TAG" ]; then
    TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 \
      | sed -E 's/.*"([^"]+)"$/\1/')
    [ -n "$TAG" ] || die "could not resolve latest release for $REPO; set VERSION or FUSE_PROXY_BIN_SRC"
  fi
  # fuse-proxy ships as a raw binary asset (like fused), not a tarball, so
  # there is nothing to extract: verify, then install it directly.
  ASSET="fuse-proxy_Linux_${ASSET_ARCH}"
  TMPBIN=$(mktemp); SUMS=$(mktemp)
  trap 'rm -f "$TMPBIN" "$SUMS"' EXIT
  log "downloading $ASSET ($TAG)"
  curl -fsSL -o "$TMPBIN" "https://github.com/$REPO/releases/download/$TAG/$ASSET" \
    || die "download failed for $TAG ($ASSET)"
  curl -fsSL -o "$SUMS" "https://github.com/$REPO/releases/download/$TAG/checksums.txt" \
    || die "release $TAG publishes no checksums.txt - refusing to install unverified assets"
  verify_asset "$TMPBIN" "$ASSET" "$SUMS" || die "checksum verification failed for $ASSET ($TAG)"
  log "sha256 verified: $ASSET"
  install -m0755 "$TMPBIN" "$BIN"
  log "installed binary from release $TAG"
fi
log "binary -> $BIN ($("$BIN" -version 2>/dev/null || echo '?'))"

# 2. write the env file, generating the admin token, only if it does not exist
#    so a re-run never clobbers an operator's edits or rotates their token.
#    Rotating it would lock out the orchestrator until its own env file was
#    edited to match.
mkdir -p "$ENV_DIR"
if [ ! -f "$ENV_FILE" ]; then
  ADMIN_TOKEN=$(openssl rand -hex 32)
  PUBLIC_HOST="${FUSE_PROXY_PUBLIC_HOST:-}"
  if [ -z "$PUBLIC_HOST" ]; then
    # Every url this proxy hands out is built from this value, so a wrong
    # guess is visible immediately rather than subtly. Detected as a
    # convenience; behind NAT or with a dns name, pass it explicitly.
    PUBLIC_HOST=$(curl -4 -fsS ifconfig.me 2>/dev/null || true)
    [ -n "$PUBLIC_HOST" ] || PUBLIC_HOST=$(hostname -I 2>/dev/null | awk '{print $1}')
    [ -n "$PUBLIC_HOST" ] || die "could not detect this host's address; set FUSE_PROXY_PUBLIC_HOST"
    log "detected public host $PUBLIC_HOST (override with FUSE_PROXY_PUBLIC_HOST)"
  fi
  umask 077
  cat > "$ENV_FILE" <<EOF
# /etc/fuse/proxy.env - fuse-proxy config. Edit, then:
#   sudo systemctl restart fuse-proxy

# The host part of every environment url. Clients and guests must both reach
# this address. Changing it changes every url this proxy reports.
FUSE_PROXY_PUBLIC_HOST=$PUBLIC_HOST

# Shared secret for the admin api. The orchestrator's FUSE_PROXY_ADMIN_TOKEN
# must equal this value.
FUSE_PROXY_ADMIN_TOKEN=$ADMIN_TOKEN

# udp, guests dial in here. Open it to every firecracker host.
FUSE_PROXY_TUNNEL_LISTEN=$TUNNEL_LISTEN

# tcp, the orchestrator drives this. Keep it off the public internet: it can
# create and move routes.
FUSE_PROXY_ADMIN_LISTEN=$ADMIN_LISTEN

# Public port range handed out to environments, one port per published route.
# Open it to clients. Widen it before you run out; a publish fails when the
# range is exhausted.
FUSE_PROXY_PORTS=$PORTS

# routes.json and the tunnel certificate live here. KEEP THIS DIRECTORY:
# losing it changes every environment url and locks out every running guest,
# because each one pins the certificate's fingerprint.
FUSE_PROXY_STATE_DIR=$STATE_DIR
EOF
  chmod 0600 "$ENV_FILE"
  log "wrote $ENV_FILE (generated admin token)"
else
  log "$ENV_FILE exists; leaving it alone"
  PUBLIC_HOST=$(grep -E '^FUSE_PROXY_PUBLIC_HOST=' "$ENV_FILE" | cut -d= -f2- || true)
  ADMIN_TOKEN=$(grep -E '^FUSE_PROXY_ADMIN_TOKEN=' "$ENV_FILE" | cut -d= -f2- || true)
fi

mkdir -p "$STATE_DIR"
chmod 0700 "$STATE_DIR"

# 3. install the unit. Written inline rather than copied from ops/systemd so a
#    curl'd standalone run works with no repo on disk (same reason
#    install-orchestrator.sh does it).
cat > "$UNIT" <<EOF
[Unit]
Description=Fuse proxy (stable ingress for environments)
Documentation=https://github.com/folsomintel/fuse
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=on-failure
RestartSec=2
LimitNOFILE=65536

NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
EOF
log "wrote $UNIT"

systemctl daemon-reload
systemctl enable --now fuse-proxy.service
sleep 1
systemctl --no-pager --lines=0 status fuse-proxy.service || true

ADMIN_PORT="${ADMIN_LISTEN##*:}"
cat <<EOF

[install-proxy] done.

Add these two lines to the orchestrator's env file (/etc/fuse/orchestrator.env
or /etc/default/orchestrator), then restart it:

  FUSE_PROXY_ADMIN_URL=http://${ADMIN_LISTEN}
  FUSE_PROXY_ADMIN_TOKEN=${ADMIN_TOKEN:-<see $ENV_FILE>}

If the orchestrator runs on a different machine, point ADMIN_URL at this host
and move FUSE_PROXY_ADMIN_LISTEN off loopback -- but reach it over a private
network or a tunnel, never the public internet.

Firewall, on this host:
  udp ${TUNNEL_LISTEN}       open to every firecracker host (guests dial in)
  tcp ${PORTS}   open to clients (environment urls)
  tcp ${ADMIN_PORT}            orchestrator only
EOF
