#!/usr/bin/env bash
# start/stop/env wrapper for the Surf-compatible Firecracker host agent.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORT="${FC_AGENT_PORT:-8090}"
ENV_FILE="$FC_DIR/.env"
LEGACY_ENV_FILE="$FC_DIR/.fc-agent.env"
PID_FILE="$FC_DIR/.fc-agent.pid"
LOG="$FC_DIR/fc-agent.log"

cmd="${1:-start}"
public_ip() { curl -fsS ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}'; }

ensure_token() {
  if [ ! -f "$ENV_FILE" ] && [ -f "$LEGACY_ENV_FILE" ]; then
    mv "$LEGACY_ENV_FILE" "$ENV_FILE"
  fi
  if [ ! -f "$ENV_FILE" ]; then
    (umask 077 && echo "FC_AGENT_TOKEN=$(openssl rand -hex 32)" > "$ENV_FILE")
  fi
  chmod 600 "$ENV_FILE"
}

case "$cmd" in
  start)
    if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
      echo "[fc-agent] already running (pid $(cat "$PID_FILE"))"
    else
      ensure_token
      # shellcheck disable=SC1090
      source "$ENV_FILE"
      FC_AGENT_TOKEN="$FC_AGENT_TOKEN" FC_AGENT_PORT="$PORT" FC_DIR="$FC_DIR" \
        nohup python3 "$FC_DIR/fc-agent.py" >"$LOG" 2>&1 &
      echo $! > "$PID_FILE"
      sleep 0.5
    fi
    sudo -n iptables -C INPUT -p tcp --dport "$PORT" -j ACCEPT 2>/dev/null \
      || sudo -n iptables -I INPUT -p tcp --dport "$PORT" -j ACCEPT
    "$0" env
    ;;
  stop)
    [ -f "$PID_FILE" ] && kill "$(cat "$PID_FILE")" 2>/dev/null || true
    rm -f "$PID_FILE"
    pkill -f fc-agent.py 2>/dev/null || true
    sudo -n iptables -D INPUT -p tcp --dport "$PORT" -j ACCEPT 2>/dev/null || true
    echo "[fc-agent] stopped"
    ;;
  restart)
    "$0" stop; "$0" start ;;
  log)
    tail -n 200 -f "$LOG" ;;
  install-service)
    ensure_token
    USER_NAME=$(id -un)
    UNIT=/etc/systemd/system/fc-agent.service
    sudo -n tee "$UNIT" >/dev/null <<EOF
[Unit]
Description=Surf Firecracker host agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$FC_DIR
EnvironmentFile=$ENV_FILE
Environment=FC_DIR=$FC_DIR
Environment=FC_AGENT_PORT=$PORT
ExecStart=/usr/bin/python3 $FC_DIR/fc-agent.py
Restart=on-failure
RestartSec=3
StandardOutput=append:$LOG
StandardError=append:$LOG

[Install]
WantedBy=multi-user.target
EOF
    sudo -n systemctl daemon-reload
    sudo -n systemctl enable --now fc-agent.service
    sudo -n systemctl status --no-pager fc-agent.service | head -15
    "$0" env
    ;;
  uninstall-service)
    sudo -n systemctl disable --now fc-agent.service 2>/dev/null || true
    sudo -n rm -f /etc/systemd/system/fc-agent.service
    sudo -n systemctl daemon-reload
    echo "[fc-agent] service removed"
    ;;
  install-updater)
    # Weekly surfd auto-update via systemd timer. Requires GH_TOKEN in the env file
    # next to .fc-agent.env so the updater can reach the private release.
    UPDATER_ENV="$FC_DIR/.fc-updater.env"
    if [ ! -f "$UPDATER_ENV" ]; then
      echo "Create $UPDATER_ENV with 'GH_TOKEN=...' before installing the timer." >&2
      exit 1
    fi
    sudo -n tee /etc/systemd/system/fc-update-surfd.service >/dev/null <<EOF
[Unit]
Description=Pull latest surfd and rebake rootfs
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
WorkingDirectory=$FC_DIR
EnvironmentFile=$UPDATER_ENV
ExecStart=$FC_DIR/fc-update-surfd.sh
EOF
    sudo -n tee /etc/systemd/system/fc-update-surfd.timer >/dev/null <<EOF
[Unit]
Description=Weekly surfd refresh

[Timer]
OnCalendar=Mon *-*-* 04:00:00
Persistent=true
RandomizedDelaySec=30min

[Install]
WantedBy=timers.target
EOF
    sudo -n systemctl daemon-reload
    sudo -n systemctl enable --now fc-update-surfd.timer
    sudo -n systemctl list-timers fc-update-surfd.timer --no-pager | head -5
    echo "[fc-agent] weekly updater installed (Mon 04:00 UTC, ±30min jitter)"
    ;;
  uninstall-updater)
    sudo -n systemctl disable --now fc-update-surfd.timer 2>/dev/null || true
    sudo -n rm -f /etc/systemd/system/fc-update-surfd.{service,timer}
    sudo -n systemctl daemon-reload
    echo "[fc-agent] updater removed"
    ;;
  env)
    ensure_token
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    IP=$(public_ip)
    echo
    echo "# ---- Surf orchestrator env ----"
    echo "FIRECRACKER_BASE_URL=http://${IP}:${PORT}"
    echo "FIRECRACKER_TOKEN=${FC_AGENT_TOKEN}"
    echo "# --------------------------------"
    ;;
  *)
    echo "usage: $0 {start|stop|restart|log|env|install-service|uninstall-service|install-updater|uninstall-updater}" >&2; exit 1 ;;
esac
