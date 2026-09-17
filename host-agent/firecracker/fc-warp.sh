#!/usr/bin/env bash
# The host side of the cloudflare warp egress backend.
#
#   fc-warp.sh install                       install the unit, retire the stock warp-svc
#   fc-warp.sh up      < KEY=value lines     bring the per-host warp up (idempotent)
#   fc-warp.sh down                          tear it down
#   fc-warp.sh attach <tap> <listen_ip> <port>   publish the proxy on one vm's tap
#   fc-warp.sh detach <tap> <listen_ip> <port>   remove that
#   fc-warp.sh status                        print warp-cli's status
#
# One warp-svc per host, in its own network namespace, shared by every
# proxy-mode vm on the host. The namespace is the load-bearing part: warp-svc
# is a system-wide daemon that installs its own routing, and a fuse host also
# carries the orchestrator's control-plane traffic. Inside fuse-warp it can
# route whatever it likes.
#
# warp's proxy listener binds loopback inside the namespace and nothing else,
# so a guest reaches it through a veth pair plus two DNATs: the host rewrites
# <listen_ip>:<port> on the vm's tap to the namespace end of the veth, and the
# namespace rewrites that to its own loopback. The proxy path leaves the tap on
# the veth, not the host's default interface, so the FORWARD drop that enforces
# proxy mode never sees it.
#
# The credential arrives on stdin as KEY=value lines, never as an argument and
# never as an environment variable of a sudo command, both of which show in
# ps. It is written to warp's own state dir (mdm.xml, mode 600) or handed to
# warp-cli once, and appears nowhere else on the host.
#
# Not yet exercised on a production host: every step here follows cloudflare's
# documented headless enrolment and proxy mode, but a host with a real
# registration is what proves it.
set -euo pipefail

NS=fuse-warp
VETH_HOST=fwarp0
VETH_NS=fwarp1
VETH_HOST_IP=10.201.0.1
VETH_NS_IP=10.201.0.2
UNIT=fuse-warp.service
WARP_STATE=/var/lib/cloudflare-warp
STARTUP_TIMEOUT="${WARP_STARTUP_TIMEOUT:-60}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPS_SYSTEMD="$(cd "$HERE/../../ops/systemd" 2>/dev/null && pwd || true)"

log() { printf '[warp] %s\n' "$*"; }
die() { printf '[warp] %s\n' "$*" >&2; exit 1; }

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

ns_exec() { ip netns exec "$NS" "$@"; }

warp_cli() { ns_exec warp-cli --accept-tos "$@"; }

ensure_netns() {
  if ! ip netns list | grep -qx "$NS"; then
    ip netns add "$NS"
    log "created netns $NS"
  fi
  ns_exec ip link set lo up
  if ! ip link show "$VETH_HOST" >/dev/null 2>&1; then
    ip link add "$VETH_HOST" type veth peer name "$VETH_NS"
    ip link set "$VETH_NS" netns "$NS"
    ip addr add "$VETH_HOST_IP/30" dev "$VETH_HOST"
    ip link set "$VETH_HOST" up
    ns_exec ip addr add "$VETH_NS_IP/30" dev "$VETH_NS"
    ns_exec ip link set "$VETH_NS" up
    ns_exec ip route replace default via "$VETH_HOST_IP"
    log "created veth $VETH_HOST <-> $VETH_NS"
  fi
  # the namespace has no resolver of its own; warp-svc needs one to reach
  # cloudflare. the unit bind-mounts this over /etc/resolv.conf for warp-svc.
  mkdir -p "/etc/netns/$NS"
  printf 'nameserver 1.1.1.1\nnameserver 1.0.0.1\n' > "/etc/netns/$NS/resolv.conf"
  local iface
  iface="$(host_iface)"
  sysctl -qw net.ipv4.ip_forward=1
  # the namespace reaches the internet through the host: masquerade its
  # veth address out the default interface and allow the forward both ways.
  rule add -t nat POSTROUTING -s "$VETH_NS_IP" -o "$iface" -j MASQUERADE
  rule add FORWARD -i "$VETH_HOST" -o "$iface" -j ACCEPT
  rule add FORWARD -i "$iface" -o "$VETH_HOST" -m state --state RELATED,ESTABLISHED -j ACCEPT
}

# the proxy listens on loopback inside the namespace; route_localnet plus a
# dnat lets traffic addressed to the veth end reach it.
ensure_ns_dnat() {
  local port="$1"
  ns_exec sysctl -qw net.ipv4.conf.all.route_localnet=1
  if ! ns_exec iptables -t nat -C PREROUTING -d "$VETH_NS_IP" -p tcp --dport "$port" -j DNAT --to-destination "127.0.0.1:$port" 2>/dev/null; then
    ns_exec iptables -t nat -I PREROUTING -d "$VETH_NS_IP" -p tcp --dport "$port" -j DNAT --to-destination "127.0.0.1:$port"
  fi
}

# read KEY=value lines from stdin into shell variables. only the keys the
# backend knows are accepted, so a hostile line cannot become an arbitrary
# assignment.
read_config() {
  WARP_ORG=""; WARP_CLIENT_ID=""; WARP_CLIENT_SECRET=""; WARP_LICENSE=""; WARP_PROXY_PORT=40000
  local line key value
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    key="${line%%=*}"; value="${line#*=}"
    case "$key" in
      WARP_ORG|WARP_CLIENT_ID|WARP_CLIENT_SECRET|WARP_LICENSE|WARP_PROXY_PORT) printf -v "$key" '%s' "$value" ;;
      *) die "unknown config key: $key" ;;
    esac
  done
}

write_mdm() {
  # zero trust headless enrolment: warp-svc reads mdm.xml on start and
  # enrols with the service token, no browser involved.
  mkdir -p "$WARP_STATE"
  umask 077
  cat > "$WARP_STATE/mdm.xml" <<EOF
<dict>
  <key>organization</key><string>${WARP_ORG}</string>
  <key>auth_client_id</key><string>${WARP_CLIENT_ID}</string>
  <key>auth_client_secret</key><string>${WARP_CLIENT_SECRET}</string>
  <key>service_mode</key><string>proxy</string>
  <key>proxy_port</key><integer>${WARP_PROXY_PORT}</integer>
  <key>auto_connect</key><integer>1</integer>
  <key>onboarding</key><false/>
</dict>
EOF
  chmod 600 "$WARP_STATE/mdm.xml"
}

wait_connected() {
  local deadline=$((SECONDS + STARTUP_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if warp_cli status 2>/dev/null | grep -qi "connected"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

cmd_install() {
  [ -n "$OPS_SYSTEMD" ] && [ -f "$OPS_SYSTEMD/$UNIT" ] || die "unit not found: expected ops/systemd/$UNIT next to this checkout"
  command -v warp-svc >/dev/null 2>&1 || die "warp-svc is not installed; run fc-deps.sh first"
  # the stock unit would run a second warp-svc on the same state dir, in
  # the host's own namespace, which is exactly what fuse-warp exists to avoid.
  systemctl disable --now warp-svc.service >/dev/null 2>&1 || true
  install -m0644 "$OPS_SYSTEMD/$UNIT" "/etc/systemd/system/$UNIT"
  systemctl daemon-reload
  log "installed $UNIT (not started; the agent starts it on first use)"
}

cmd_up() {
  read_config
  [ -n "$WARP_LICENSE" ] || [ -n "$WARP_CLIENT_SECRET" ] || die "no credential: need WARP_LICENSE or WARP_ORG/WARP_CLIENT_ID/WARP_CLIENT_SECRET"
  ensure_netns
  if [ -n "$WARP_CLIENT_SECRET" ]; then
    write_mdm
  fi
  if ! systemctl is-active --quiet "$UNIT"; then
    systemctl start "$UNIT"
    log "started $UNIT"
  fi
  # give the daemon its socket before the first cli call.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    warp_cli status >/dev/null 2>&1 && break
    sleep 1
  done
  if [ -n "$WARP_LICENSE" ]; then
    # consumer warp+: register once, then apply the key. registration is
    # idempotent from our side; a second `new` on an enrolled host errors
    # and is ignored.
    warp_cli registration new >/dev/null 2>&1 || true
    warp_cli registration license "$WARP_LICENSE" >/dev/null
  fi
  warp_cli mode proxy >/dev/null
  warp_cli proxy port "$WARP_PROXY_PORT" >/dev/null
  warp_cli connect >/dev/null 2>&1 || true
  if ! wait_connected; then
    die "warp did not connect within ${STARTUP_TIMEOUT}s; a backend that is not up is a provisioning failure, not a slow create"
  fi
  ensure_ns_dnat "$WARP_PROXY_PORT"
  printf '%s\n' "$WARP_PROXY_PORT" > "/run/fuse-warp.port"
  log "warp up in $NS on 127.0.0.1:$WARP_PROXY_PORT (via $VETH_NS_IP)"
}

cmd_down() {
  warp_cli disconnect >/dev/null 2>&1 || true
  systemctl stop "$UNIT" >/dev/null 2>&1 || true
  local iface
  iface="$(host_iface)"
  rule del -t nat POSTROUTING -s "$VETH_NS_IP" -o "$iface" -j MASQUERADE
  rule del FORWARD -i "$VETH_HOST" -o "$iface" -j ACCEPT
  rule del FORWARD -i "$iface" -o "$VETH_HOST" -m state --state RELATED,ESTABLISHED -j ACCEPT
  ip link del "$VETH_HOST" 2>/dev/null || true
  ip netns del "$NS" 2>/dev/null || true
  rm -f /run/fuse-warp.port
  log "warp down"
}

# attach/detach: publish the namespace's proxy on one vm's tap address.
vm_rules() {
  local mode="$1" tap="$2" listen_ip="$3" port="$4" ns_port
  ns_port="$(cat /run/fuse-warp.port 2>/dev/null || echo 40000)"
  rule "$mode" -t nat PREROUTING -i "$tap" -d "$listen_ip" -p tcp --dport "$port" -j DNAT --to-destination "$VETH_NS_IP:$ns_port"
  rule "$mode" FORWARD -i "$tap" -o "$VETH_HOST" -j ACCEPT
  rule "$mode" FORWARD -i "$VETH_HOST" -o "$tap" -m state --state RELATED,ESTABLISHED -j ACCEPT
}

case "${1:-}" in
  install) cmd_install ;;
  up)      cmd_up ;;
  down)    cmd_down ;;
  attach)  vm_rules add "${2:?tap}" "${3:?listen ip}" "${4:?port}"; log "attached $2 $3:$4" ;;
  detach)  vm_rules del "${2:?tap}" "${3:?listen ip}" "${4:?port}"; log "detached $2 $3:$4" ;;
  status)  warp_cli status ;;
  *) die "usage: fc-warp.sh install|up|down|attach|detach|status" ;;
esac
