#!/usr/bin/env bash
# The host side of the wireguard egress backend.
#
#   fc-wireguard.sh up     <fp> <slot>  < wireproxy.conf   bring one bridge up (idempotent)
#   fc-wireguard.sh down   <fp> <slot>                     tear it down
#   fc-wireguard.sh attach <fp> <slot> <tap> <listen_ip> <port> <bridge_port>
#                                                           publish the bridge on one vm's tap
#   fc-wireguard.sh detach <fp> <slot> <tap> <listen_ip> <port> <bridge_port>
#                                                           remove that
#   fc-wireguard.sh status <fp> <slot>                     print the bridge's wireguard state
#
# One bridge per distinct wireguard config, named by the config's fingerprint
# <fp> and given a /30 by <slot>: fc-agent.py validates the config, allocates
# the slot and keeps the refcount, and calls this for everything that touches
# the host.
#
# A bridge is wireproxy (a userspace wireguard client that serves socks5 and
# http) running under fuse-wireguard@<fp>.service inside its own network
# namespace, fuse-wg-<fp>. The namespace reaches the internet through a veth
# pair and the host's nat, which is all the wireguard udp needs; the tunnel
# itself lives in wireproxy's process, so nothing in the namespace or on the
# host routes through it. A guest reaches the bridge through one dnat on its
# tap: <listen_ip>:<port> becomes 10.202.<slot>.2:<bridge_port>. That path
# leaves the tap on the veth, not the host's default interface, so the
# FORWARD drop that enforces proxy mode never sees it.
#
# The config, which holds the private key, arrives on stdin, never as an
# argument. It is written once, mode 600 in a root-only directory under /run
# (tmpfs, gone at reboot), and handed to the unit as a systemd credential.
set -euo pipefail

RUN_DIR=/run/fuse-wireguard
UNIT_TEMPLATE=fuse-wireguard@.service
WIREPROXY=/usr/local/bin/wireproxy
STARTUP_TIMEOUT="${WG_STARTUP_TIMEOUT:-30}"
# every bridge's /30 comes out of this /16. a guest is kept away from every
# bridge but its own.
BRIDGE_NET=10.202.0.0/16

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPS_SYSTEMD="$(cd "$HERE/../../ops/systemd" 2>/dev/null && pwd || true)"

log() { printf '[wireguard] %s\n' "$*"; }
die() { printf '[wireguard] %s\n' "$*" >&2; exit 1; }

host_iface() { ip -o route get 8.8.8.8 | awk '{print $5}'; }

# rule <add|del> <table-args...>: idempotent iptables add/remove on the host.
rule() {
  local mode="$1"; shift
  if iptables -C "$@" 2>/dev/null; then
    [ "$mode" = del ] && iptables -D "$@"
  else
    [ "$mode" = add ] && iptables -I "$@"
  fi
  return 0
}

# names <fp> <slot>: validates both and derives every name from them. both
# end up in interface names, unit names and paths, so nothing else is let in.
names() {
  FP="${1:?fingerprint}"; SLOT="${2:?slot}"
  [[ "$FP" =~ ^[0-9a-f]{12}$ ]] || die "invalid fingerprint: $FP"
  [[ "$SLOT" =~ ^[0-9]{1,3}$ ]] && [ "$SLOT" -le 255 ] || die "invalid slot: $SLOT"
  NS="fuse-wg-$FP"
  VETH_HOST="fwg${SLOT}h"
  VETH_NS="fwg${SLOT}n"
  VETH_HOST_IP="10.202.$SLOT.1"
  VETH_NS_IP="10.202.$SLOT.2"
  UNIT="fuse-wireguard@$FP.service"
  CONF_DIR="$RUN_DIR/$FP"
}

ensure_unit() {
  [ -x "$WIREPROXY" ] || die "wireproxy is not installed at $WIREPROXY; run fc-deps.sh with FUSE_WITH_WIREGUARD=1"
  [ -n "$OPS_SYSTEMD" ] && [ -f "$OPS_SYSTEMD/$UNIT_TEMPLATE" ] || die "unit not found: expected ops/systemd/$UNIT_TEMPLATE next to this checkout"
  if ! cmp -s "$OPS_SYSTEMD/$UNIT_TEMPLATE" "/etc/systemd/system/$UNIT_TEMPLATE"; then
    install -m0644 "$OPS_SYSTEMD/$UNIT_TEMPLATE" "/etc/systemd/system/$UNIT_TEMPLATE"
    systemctl daemon-reload
    log "installed $UNIT_TEMPLATE"
  fi
}

ensure_netns() {
  if ! ip netns list | awk '{print $1}' | grep -qx "$NS"; then
    ip netns add "$NS"
  fi
  ip netns exec "$NS" ip link set lo up
  if ! ip link show "$VETH_HOST" >/dev/null 2>&1; then
    ip link add "$VETH_HOST" type veth peer name "$VETH_NS"
    ip link set "$VETH_NS" netns "$NS"
    ip addr add "$VETH_HOST_IP/30" dev "$VETH_HOST"
    ip link set "$VETH_HOST" up
    ip netns exec "$NS" ip addr add "$VETH_NS_IP/30" dev "$VETH_NS"
    ip netns exec "$NS" ip link set "$VETH_NS" up
    ip netns exec "$NS" ip route replace default via "$VETH_HOST_IP"
  fi
  local iface
  iface="$(host_iface)"
  sysctl -qw net.ipv4.ip_forward=1
  rule add -t nat POSTROUTING -s "$VETH_NS_IP" -o "$iface" -j MASQUERADE
  rule add FORWARD -i "$VETH_HOST" -o "$iface" -j ACCEPT
  rule add FORWARD -i "$iface" -o "$VETH_HOST" -m state --state RELATED,ESTABLISHED -j ACCEPT
}

# metrics prints wireproxy's /metrics (the wg uapi dump, keys redacted by
# wireproxy itself) from inside the namespace, without needing curl.
metrics() {
  ip netns exec "$NS" bash -c 'exec 3<>/dev/tcp/127.0.0.1/9080 && printf "GET /metrics HTTP/1.0\r\n\r\n" >&3 && cat <&3' 2>/dev/null || true
}

# the bridge is up when a peer has completed a handshake. a listening
# wireproxy with a dead tunnel would pass a port probe and carry nothing.
wait_handshake() {
  local deadline=$((SECONDS + STARTUP_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if metrics | grep -Eq '^last_handshake_time_sec=[1-9]'; then
      return 0
    fi
    sleep 1
  done
  return 1
}

cmd_up() {
  names "$@"
  ensure_unit
  ensure_netns
  install -d -m0700 "$CONF_DIR"
  local tmp="$CONF_DIR/wireproxy.conf.new"
  (umask 077; cat > "$tmp")
  [ -s "$tmp" ] || { rm -f "$tmp"; die "no config on stdin"; }
  if ! cmp -s "$tmp" "$CONF_DIR/wireproxy.conf" || ! systemctl is-active --quiet "$UNIT"; then
    mv -f "$tmp" "$CONF_DIR/wireproxy.conf"
    systemctl restart "$UNIT"
    log "started $UNIT"
  else
    rm -f "$tmp"
  fi
  if ! wait_handshake; then
    local why
    why="$(journalctl -u "$UNIT" -n 5 --no-pager -o cat 2>/dev/null | tr '\n' ' ' || true)"
    systemctl stop "$UNIT" >/dev/null 2>&1 || true
    die "no wireguard handshake within ${STARTUP_TIMEOUT}s; check the peer endpoint and keys. wireproxy said: ${why:-nothing}"
  fi
  log "bridge $FP up on $VETH_NS_IP"
}

cmd_down() {
  names "$@"
  systemctl stop "$UNIT" >/dev/null 2>&1 || true
  local iface
  iface="$(host_iface)"
  rule del -t nat POSTROUTING -s "$VETH_NS_IP" -o "$iface" -j MASQUERADE
  rule del FORWARD -i "$VETH_HOST" -o "$iface" -j ACCEPT
  rule del FORWARD -i "$iface" -o "$VETH_HOST" -m state --state RELATED,ESTABLISHED -j ACCEPT
  ip link del "$VETH_HOST" 2>/dev/null || true
  ip netns del "$NS" 2>/dev/null || true
  rm -rf "$CONF_DIR"
  log "bridge $FP down"
}

# vm_rules <add|del> <fp> <slot> <tap> <listen_ip> <port> <bridge_port>. the
# drop goes in first so the accepts land above it: the tap may reach its own
# bridge's one port and no other address in the bridge range.
vm_rules() {
  local mode="$1"; shift
  names "$1" "$2"
  local tap="${3:?tap}" listen_ip="${4:?listen ip}" port="${5:?port}" bridge_port="${6:?bridge port}"
  rule "$mode" FORWARD -i "$tap" -d "$BRIDGE_NET" -j DROP
  rule "$mode" FORWARD -i "$VETH_HOST" -o "$tap" -m state --state RELATED,ESTABLISHED -j ACCEPT
  rule "$mode" FORWARD -i "$tap" -o "$VETH_HOST" -d "$VETH_NS_IP" -p tcp --dport "$bridge_port" -j ACCEPT
  rule "$mode" -t nat PREROUTING -i "$tap" -d "$listen_ip" -p tcp --dport "$port" -j DNAT --to-destination "$VETH_NS_IP:$bridge_port"
}

case "${1:-}" in
  up)     shift; cmd_up "$@" ;;
  down)   shift; cmd_down "$@" ;;
  attach) shift; vm_rules add "$@"; log "attached $3 $4:$5" ;;
  detach) shift; vm_rules del "$@"; log "detached $3 $4:$5" ;;
  status) shift; names "$@"; systemctl is-active "$UNIT" || true; metrics | grep -E '^(public_key|endpoint|last_handshake_time_sec|rx_bytes|tx_bytes)=' || true ;;
  *) die "usage: fc-wireguard.sh up|down|attach|detach|status <fp> <slot> [...]" ;;
esac
