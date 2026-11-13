#!/usr/bin/env bash
# map a diff to the fuse subsystems it can affect and the go packages to test.
# usage: impact.sh [base-ref]   (default origin/main)
# prints:
#   AREA <name>  <why>        one line per affected subsystem
#   PKG  <import path>        go packages that import a changed package (plus the changed ones)
#   FILE <path>               the changed files

set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

BASE="${1:-origin/main}"
MB="$(git merge-base "$BASE" HEAD 2>/dev/null)" || { echo "cannot find merge-base with $BASE" >&2; exit 2; }
MOD="github.com/folsomintel/fuse"

CHANGED="$( { git diff --name-only "$MB"; git ls-files --others --exclude-standard; } | sort -u)"
[ -n "$CHANGED" ] || { echo "no changes against $BASE"; exit 0; }

# "name<tab>why" lines; first reason wins (bash 3.2 on macos has no associative arrays)
AREAS=""
add() { grep -q "^$1	" <<<"$AREAS" || AREAS="$AREAS$1	$2
"; }
has() { grep -Eq "$1" <<<"$CHANGED"; }
# true when the diff of the files matching $1 adds or removes a line matching $2
hunk() {
  local files
  files="$(grep -E "$1" <<<"$CHANGED")"
  [ -n "$files" ] || return 1
  # shellcheck disable=SC2086
  git diff -U0 "$MB" -- $files | grep -E '^[+-][^+-]' | grep -Eqi "$2"
}

O='^internal/orchestrator/'

# --- direct mapping from paths
has "${O}(scheduler|reconcile|fleet|fleet_hosts|provider|health|drain|events|pagination|state_store|state_store_postgres|token_rotation)\.go$|${O}migrations/|^internal/entspike/|^orchestrator/|^internal/metrics/" \
  && add orchestrator "scheduler, reconcile, state store, migrations or the orchestrator binary"
has "${O}(fork|snapshots|checkpoints|delta|migrate|artifacts|agent_profile)\.go$|^host-agent/firecracker/fc-uffd\.py$" \
  && add lifecycle "fork, snapshot, checkpoint, delta or migrate"
has "^internal/(egress|ingress|tunnel)/|^proxy/|^fused/|${O}(egress_guest|egress_health|ingress)\.go$|fc-(warp|wireguard|expose)\.sh$|^ops/install-proxy\.sh$|fuse-(proxy|warp|wireguard@)\.service$" \
  && add networking "egress, ingress, tunnel, proxy or host network scripts"
has "^internal/(hostwire|firecracker|qemu)/|^host-agent/(firecracker/fc-agent|qemu/qemu-agent)\.py$" \
  && add agent-wire "orchestrator <-> host agent contract"
has "^internal/hitl/|${O}(hitl_|gate|interventions|takeover_record|computer|desktop)|^cli/hitl_" \
  && add hitl "gates, interventions, access, takeover or computer"
has "^internal/durable/|^ops/restate/|restate-server\.service$|fuse-orchestrator-drain\.service$" \
  && add durable "restate wiring or restate-server install"
has "^internal/(api|apikeys|secrets)/" \
  && add api "http api, keys or secrets"
has "^cli/" && add cli "operator cli"
has "^internal/fusefile/|^schema/" && add fusefile "fusefile compiler or json schema"
has "^sdks/" && add sdks "one or more sdks"
has "^fused/|fc-bake|^host-agent/firecracker/fuse-(display|wm)-run$|^host-agent/shared/" \
  && add guest "guest agent or rootfs bake"
has "^ops/|^host-agent/(firecracker|qemu)/scripts/(fc-install|fc-deps|fc-update|fc-agent|qemu-install)\.sh$|^host-agent/local/|\.service$" \
  && add ops "installers, updaters, systemd units or local appliance setup"
has "^\.github/|^\.goreleaser\.yaml$|^go\.(mod|sum)$|^go\.work" && add release "ci, release config or go deps"
has "^docs/" && add docs "docs site"

# --- network changes hiding inside the host agents
hunk '^host-agent/(firecracker/fc-agent|qemu/qemu-agent)\.py$' 'iptables|nft|FORWARD|POSTROUTING|PREROUTING|\btap|netns|resolv|dns|egress|dnat|masquerade|ip (link|addr|route)' \
  && add networking "host agent diff touches iptables, taps, netns or dns"
hunk '^host-agent/(firecracker/fc-agent|qemu/qemu-agent)\.py$' 'snapshot|restore|fork|migrate|uffd|live|delta|checkpoint' \
  && add lifecycle "host agent diff touches snapshot, restore, fork or migrate code"

# --- contracts that pull in neighbours
has "^internal/api/(types|exec|computer|interventions)\.go$|^internal/api/openapi\.yaml$" \
  && { add sdks "api surface changed, all four sdks follow"; add cli "api surface changed, cli is a client"; }
has "^internal/fusefile/" && add cli "fusefile changed, cli up/build copy its fields"
has "^internal/hostwire/|${O}provider\.go$" && add agent-wire "provider interface or hostwire changed"
has "^host-agent/firecracker/fc-agent\.py$" && ! has "^host-agent/qemu/qemu-agent\.py$" \
  && add agent-wire "fc-agent changed without qemu-agent (check for a one-sided fix)"

# --- go reverse deps: every package that imports a changed package
CHANGED_PKGS="$(grep -E '\.go$' <<<"$CHANGED" | grep -v '^sdks/' | xargs -r -n1 dirname | sort -u | sed "s#^#$MOD/#; s#/\.\$##")"
PKGS=""
if [ -n "$CHANGED_PKGS" ]; then
  PKGS="$(go list -buildvcs=false -f '{{.ImportPath}} {{join .Deps " "}}' ./... 2>/dev/null | while read -r pkg deps; do
    for c in $CHANGED_PKGS; do
      if [ "$pkg" = "$c" ] || grep -qw -- "$c" <<<"$deps"; then echo "$pkg"; break; fi
    done
  done | sort -u)"
  # a reverse dep that is the orchestrator binary means the whole control plane ships the change
  [ -n "$PKGS" ] || echo "WARN go list found no packages; run go list ./... to see why" >&2
  grep -qx "$MOD/orchestrator" <<<"$PKGS" && add orchestrator "orchestrator binary imports a changed package"
  grep -qx "$MOD/fused" <<<"$PKGS" && add guest "fused (the guest agent) imports a changed package"
fi

echo "base: $BASE ($MB)"
for a in orchestrator lifecycle networking agent-wire hitl durable api cli fusefile sdks guest ops release docs; do
  why="$(grep "^$a	" <<<"$AREAS" | cut -f2)"
  [ -n "$why" ] && printf 'AREA %-13s %s\n' "$a" "$why"
done
for p in $PKGS; do echo "PKG  $p"; done
while read -r f; do echo "FILE $f"; done <<<"$CHANGED"
