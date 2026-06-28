#!/usr/bin/env bash
# Expose a guest TCP port to the outside world.
# Usage: ./fc-expose.sh <host_port> <guest_port>   (add rule)
#        ./fc-expose.sh -d <host_port> <guest_port> (remove rule)
set -euo pipefail
GUEST_IP=172.16.0.2
HOST_IFACE=$(ip -o route get 8.8.8.8 | awk '{print $5}')

OP=-A; FWD_OP=-A
if [ "${1:-}" = "-d" ]; then OP=-D; FWD_OP=-D; shift; fi

HOST_PORT="${1:?host port required}"
GUEST_PORT="${2:?guest port required}"

sudo iptables -t nat $OP PREROUTING -i "$HOST_IFACE" -p tcp --dport "$HOST_PORT" -j DNAT --to "${GUEST_IP}:${GUEST_PORT}"
sudo iptables $FWD_OP FORWARD -p tcp -d "$GUEST_IP" --dport "$GUEST_PORT" -j ACCEPT

HOST_PUB=$(curl -fsS ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}')
if [ "$OP" = "-A" ]; then
  echo "[expose] $HOST_PUB:$HOST_PORT -> $GUEST_IP:$GUEST_PORT"
else
  echo "[expose] removed forward for :$HOST_PORT"
fi
