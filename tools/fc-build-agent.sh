#!/usr/bin/env bash
# Build the reference in-guest agent (fused) for baking into the rootfs.
#
# Produces a static linux/amd64 binary at tools/fused (the input fc-bake-rootfs.sh
# expects). The systemd unit tools/fused.service is committed alongside it.
#
# Run this on any machine with Go before ./fc-bake-rootfs.sh. To bake a different
# agent instead, drop your own `fused` binary + `fused.service` here and skip this.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$REPO_ROOT/tools/fused"

echo "[build-agent] building reference agent -> $OUT (linux/amd64, static)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$REPO_ROOT" build \
  -ldflags='-s -w' -o "$OUT" ./cmd/fused

chmod 0755 "$OUT"
echo "[build-agent] done: $(cd "$REPO_ROOT/tools" && ls -lh fused | awk '{print $5}') static binary"
echo "[build-agent] next: ./fc-bake-rootfs.sh"
