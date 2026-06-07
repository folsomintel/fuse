#!/usr/bin/env bash
# Self-host auto-update: pull the latest Fuse GitHub release and apply it on this
# Firecracker host. Idempotent — a no-op when already on the latest tag.
#
# What it does when a newer release exists:
#   1. git pull the checkout (updates fc-agent.py + the tools/ scripts)
#   2. download the release's `fused` binary for this arch
#   3. re-bake the guest rootfs so new microVMs run the new agent
#   4. restart the host agent
#   5. (optional) update + restart a co-located orchestrator service
#
# Designed to be run by the systemd timer installed via
# `./fc-agent.sh install-updater`, or by hand. Public repo — no token needed;
# set GH_TOKEN to avoid GitHub API rate limits.
#
# Env knobs:
#   FUSE_REPO          owner/name to pull releases from (default: andrewn6/fuse)
#   GH_TOKEN           optional GitHub token (rate limits / private forks)
#   FC_SKIP_REBAKE=1   update the binary but don't re-bake/restart (rare)
#   FUSE_ORCH_SERVICE  systemd unit of a co-located orchestrator to also update
#   FUSE_ORCH_BIN      path to that orchestrator binary (required with the above)
set -euo pipefail

FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${FUSE_REPO:-andrewn6/fuse}"
MARKER="$FC_DIR/.fc-version"

log()  { printf '\033[1;36m[update] %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m  ! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

curl_gh() {
  if [ -n "${GH_TOKEN:-}" ]; then
    curl -fsSL -H "Authorization: Bearer $GH_TOKEN" "$@"
  else
    curl -fsSL "$@"
  fi
}

# Arch as it appears in the goreleaser asset name (fused_Linux_<arch>).
case "$(uname -m)" in
  x86_64)  ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) die "unsupported arch $(uname -m)" ;;
esac

log "checking latest release of $REPO"
RELEASE_JSON="$(curl_gh "https://api.github.com/repos/$REPO/releases/latest")" \
  || die "could not reach GitHub releases API"
LATEST="$(printf '%s' "$RELEASE_JSON" | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 | sed -E 's/.*"([^"]+)"$/\1/')"
[ -n "$LATEST" ] || die "could not parse latest tag (rate-limited? set GH_TOKEN)"

# Installed version: the baked agent binary's --version, else the marker file.
INSTALLED=""
if [ -x "$FC_DIR/fused" ]; then
  INSTALLED="$("$FC_DIR/fused" --version 2>/dev/null || true)"
fi
[ -n "$INSTALLED" ] && [ -f "$MARKER" ] || INSTALLED="$(cat "$MARKER" 2>/dev/null || echo none)"

log "installed=$INSTALLED  latest=$LATEST"
if [ "$INSTALLED" = "$LATEST" ]; then
  ok "already up to date"
  exit 0
fi

# 1. Update the checkout (fc-agent.py + scripts), best-effort.
if git -C "$FC_DIR/.." rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  log "git pull"
  git -C "$FC_DIR/.." fetch --tags --quiet || warn "git fetch failed"
  git -C "$FC_DIR/.." pull --ff-only --quiet && ok "checkout updated" || warn "git pull --ff-only failed (local changes?) — continuing"
fi

# 2. Download the new fused binary for this arch.
ASSET="fused_Linux_${ASSET_ARCH}"
URL="https://github.com/$REPO/releases/download/$LATEST/$ASSET"
log "downloading $ASSET"
TMP="$(mktemp)"
curl_gh -o "$TMP" "$URL" || die "download failed: $URL"
chmod +x "$TMP"
"$TMP" --version >/dev/null 2>&1 || die "downloaded fused is not runnable on this host"
mv "$TMP" "$FC_DIR/fused"
ok "fused -> $("$FC_DIR/fused" --version)"

if [ "${FC_SKIP_REBAKE:-0}" = "1" ]; then
  warn "FC_SKIP_REBAKE=1 — not re-baking/restarting; new agent applies on next manual bake"
else
  # 3. Re-bake so new microVMs boot the new agent.
  log "re-baking rootfs"
  "$FC_DIR/fc-bake-rootfs.sh"
  ok "rootfs re-baked"

  # 4. Restart the host agent (re-attaches running VMs).
  log "restarting fc-agent"
  if systemctl is-enabled fc-agent.service >/dev/null 2>&1; then
    sudo -n systemctl restart fc-agent.service
  else
    "$FC_DIR/fc-agent.sh" restart
  fi
  ok "fc-agent restarted"
fi

# 5. Optional: update a co-located orchestrator service.
if [ -n "${FUSE_ORCH_SERVICE:-}" ]; then
  [ -n "${FUSE_ORCH_BIN:-}" ] || die "FUSE_ORCH_SERVICE set but FUSE_ORCH_BIN is not"
  log "updating orchestrator -> $FUSE_ORCH_BIN"
  TARBALL="orchestrator_Linux_${ASSET_ARCH}.tar.gz"
  TGZ="$(mktemp)"; EXDIR="$(mktemp -d)"
  curl_gh -o "$TGZ" "https://github.com/$REPO/releases/download/$LATEST/$TARBALL" || die "orchestrator download failed"
  tar -xzf "$TGZ" -C "$EXDIR" orchestrator
  sudo -n install -m 0755 "$EXDIR/orchestrator" "$FUSE_ORCH_BIN"
  rm -rf "$TGZ" "$EXDIR"
  sudo -n systemctl restart "$FUSE_ORCH_SERVICE"
  ok "orchestrator updated + restarted ($("$FUSE_ORCH_BIN" --version 2>/dev/null || echo '?'))"
fi

# 6. Record the applied version.
printf '%s\n' "$LATEST" > "$MARKER"
printf '\n\033[1;32m[update] updated %s -> %s\033[0m\n' "$INSTALLED" "$LATEST"
