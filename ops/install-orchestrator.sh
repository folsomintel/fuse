#!/usr/bin/env bash
# install-orchestrator.sh - install and start the Fuse orchestrator (control
# plane) on a dedicated host, so bring-up is one script instead of
# clone-and-build.
#
# what it does:
#   1. installs the orchestrator binary to /usr/local/bin/orchestrator
#      (from ORCH_BIN_SRC, a local ./orchestrator, or the latest release)
#   2. writes /etc/fuse/orchestrator.env with a generated ORCH_AUTH_TOKEN and
#      TOKEN_ENCRYPTION_KEY (only if the file does not already exist)
#   3. installs the systemd unit and starts it
#   4. prints the exact `fuse connect` command to run next
#
# with FUSE_RESTATE_MODE=self it also runs restate-server next to the
# orchestrator, as a pinned docker image (restate-server.service), writes
# /etc/fuse/restate.env (every port on 127.0.0.1), and registers the
# orchestrator's handler endpoint with it after the orchestrator starts. mode
# self needs docker and DATABASE_URL. on an upgrade, restate invocations still
# pinned to the old deployment keep running on the old binary
# (fuse-orchestrator-drain.service) while the new one registers on the other
# handler port; see docs/runbooks/restate.mdx.
#
# default posture is a single-node, in-memory store with auth ON: it starts
# immediately and is safe to reach only from trusted networks. for a durable,
# production deploy set DATABASE_URL (Postgres) in the env file and add
# ORCH_REQUIRE_AUTH=true, then restart. for an orchestrator co-located with a
# firecracker host agent, use `host-agent/firecracker/scripts/fc-agent.sh install-orchestrator`
# instead, which wires FIRECRACKER_BASE_URL to the local agent.
#
# usage:
#   sudo ./install-orchestrator.sh
#   ORCH_BIN_SRC=/path/to/orchestrator sudo -E ./install-orchestrator.sh
#   FUSE_REPO=folsomintel/fuse VERSION=v0.4.0 sudo -E ./install-orchestrator.sh
#   FUSE_RESTATE_MODE=self DATABASE_URL=postgres://... sudo -E ./install-orchestrator.sh
#
# restate options (mode self):
#   FUSE_RESTATE_MODE              off (default) | self | cloud. read from the
#                                  env file when unset. only self installs
#                                  restate-server.
#   RESTATE_SNAPSHOT_DESTINATION   s3://bucket/prefix for partition snapshots
#                                  (credentials go in /etc/fuse/restate.env)
#   FUSE_RESTATE_FORCE_UPGRADE=1   replace a deployment that still has
#                                  invocations pinned to it when the old binary
#                                  cannot drain them
#
# restate options (mode cloud): no restate-server is installed; these are
# written to the env file. the first three are required unless the env file
# already has them. see docs/content/docs/cloud/restate-cloud.mdx.
#   FUSE_RESTATE_INGRESS_URL       https://<id>.env.<region>.restate.cloud:8080
#   FUSE_RESTATE_API_KEY           cloud api key (ingress + registration)
#   FUSE_RESTATE_IDENTITY_KEY      publickeyv1_... request signing key
#   FUSE_RESTATE_CONNECT           tunnel (default) | public
#   FUSE_RESTATE_DEPLOYMENT_URL    https url of the handler endpoint (public)
#   FUSE_RESTATE_ADMIN_URL, FUSE_RESTATE_ENVIRONMENT_ID, FUSE_RESTATE_REGION,
#   FUSE_RESTATE_TUNNEL_NAME, RESTATE_INPROC_AUTH_TOKEN_FILE   optional
set -euo pipefail

# FUSE_INSTALL_ROOT is for ops/test-install-fuse.sh only: every path below is
# placed under it.
P="${FUSE_INSTALL_ROOT:-}"
BIN=$P/usr/local/bin/orchestrator
ENV_DIR=$P/etc/fuse
ENV_FILE="$ENV_DIR/orchestrator.env"
UNIT_DIR=$P/etc/systemd/system
UNIT="$UNIT_DIR/fuse-orchestrator.service"
REPO="${FUSE_REPO:-folsomintel/fuse}"
LISTEN="${ORCH_LISTEN:-:8080}"

# restate-server, pinned by digest. bump it together with the copy in
# ops/systemd/restate-server.service.
RESTATE_IMAGE=docker.io/restatedev/restate:1.8.1@sha256:fea6b4a893616fd8ec4594bd72fbf9967f9ab9242c8c75008de75edfbb58b2c7
RESTATE_ENV="$ENV_DIR/restate.env"
RESTATE_DEPLOY=$P/usr/local/lib/fuse/restate-deploy
DRAIN_DIR=$P/var/lib/fuse/restate-drain
# the two handler addresses an upgrade alternates between, so the old binary
# can keep its address while invocations pinned to it finish.
HANDLER_PORT_A=9080
HANDLER_PORT_B=9081

log() { echo "[install-orchestrator] $*"; }
die() { echo "[install-orchestrator] error: $*" >&2; exit 1; }

# env_value <key>: the key's last value in the orchestrator env file, or empty.
env_value() {
  [ -f "$ENV_FILE" ] || return 0
  grep -E "^$1=" "$ENV_FILE" | tail -n1 | cut -d= -f2- || true
}

# set_env_key <file> <key> <value>: replace the key's line, or append it.
set_env_key() {
  local tmp
  tmp=$(mktemp)
  grep -vE "^$2=" "$1" > "$tmp" || true
  echo "$2=$3" >> "$tmp"
  cat "$tmp" > "$1"
  rm -f "$tmp"
}

# url_for <bind-address>: the loopback url that reaches a bind address.
url_for() { echo "http://127.0.0.1:${1##*:}"; }

# >>> fuse release-asset checksum verification >>>
# This block is embedded, not sourced: ops/install-orchestrator.sh is
# documented as curl-able and run standalone on a fresh host, so a sibling
# library file is not guaranteed to exist on disk. Keep the copies in
# host-agent/firecracker/scripts/fc-update.sh, host-agent/firecracker/scripts/fc-agent.sh,
# host-agent/local/fuse-local-setup.sh and ops/install-orchestrator.sh
# byte-identical;
# host-agent/firecracker/tests/test-verify-checksum.sh asserts that and exercises the helper.

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

MODE="${FUSE_RESTATE_MODE:-$(env_value FUSE_RESTATE_MODE)}"
MODE="${MODE:-off}"
case "$MODE" in
  off|self|cloud) ;;
  *) die "FUSE_RESTATE_MODE must be off, self or cloud (got $MODE)" ;;
esac
# leaving self mode would strand whatever restate still holds for this
# orchestrator; the installer does not do that switch.
if [ "$(env_value FUSE_RESTATE_MODE)" = self ] && [ "$MODE" != self ]; then
  die "$ENV_FILE runs FUSE_RESTATE_MODE=self; switching it to $MODE would strand restate invocations. drain them and edit $ENV_FILE by hand"
fi
DB_URL="${DATABASE_URL:-$(env_value DATABASE_URL)}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# install_unit <name> <inline-fn>: install the tracked ops/systemd/ copy, or
# the inline one when this script runs curl'd standalone.
install_unit() {
  if [ -f "$SCRIPT_DIR/systemd/$1" ]; then
    install -m0644 "$SCRIPT_DIR/systemd/$1" "$UNIT_DIR/$1"
  else
    "$2" > "$UNIT_DIR/$1"
    chmod 0644 "$UNIT_DIR/$1"
  fi
  log "installed unit -> $UNIT_DIR/$1"
}

restate_server_unit() {
  cat <<EOF
[Unit]
Description=Restate server (durable execution for the Fuse orchestrator)
Documentation=https://github.com/$REPO
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=FUSE_RESTATE_IMAGE=$RESTATE_IMAGE
EnvironmentFile=/etc/fuse/restate.env
StateDirectory=restate
StateDirectoryMode=0700
ExecStartPre=-/usr/bin/docker rm -f fuse-restate
ExecStart=/usr/bin/docker run --rm --name fuse-restate \\
  --network host \\
  --env-file /etc/fuse/restate.env \\
  --volume /var/lib/restate:/restate-data \\
  --cap-drop ALL --security-opt no-new-privileges \\
  --read-only --tmpfs /tmp \\
  --log-opt max-size=10m --log-opt max-file=3 \\
  \${FUSE_RESTATE_IMAGE}
ExecStop=/usr/bin/docker stop --time 60 fuse-restate
TimeoutStopSec=90
Restart=on-failure
RestartSec=2

NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
EOF
}

drain_unit() {
  cat <<EOF
[Unit]
Description=Fuse orchestrator, previous version draining restate invocations
Documentation=https://github.com/$REPO
After=restate-server.service
Wants=restate-server.service

[Service]
Type=simple
EnvironmentFile=/etc/fuse/orchestrator.env
EnvironmentFile=/var/lib/fuse/restate-drain/drain.env
ExecStart=/usr/local/lib/fuse/restate-deploy drain /var/lib/fuse/restate-drain/orchestrator
Restart=on-failure
RestartSec=5

NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
EOF
}

# write_restate_deploy: the helper the installer and the drain unit use to
# talk to the restate admin api. curl and grep only, no jq.
write_restate_deploy() {
  mkdir -p "$(dirname "$RESTATE_DEPLOY")"
  cat > "$RESTATE_DEPLOY" <<'HELPER'
#!/usr/bin/env bash
# restate-deploy: register, inspect and retire the fuse orchestrator's restate
# deployments. written by ops/install-orchestrator.sh; edits are overwritten.
#
#   restate-deploy register <uri> [force]  register a handler endpoint
#   restate-deploy id <uri>                its deployment id, empty if none
#   restate-deploy inflight <uri>          invocations pinned to it, not done
#   restate-deploy retire <uri>            delete its deployment once nothing is
#                                          pinned to it (exit 1 while some are)
#   restate-deploy drain <old-binary>      run the old binary until
#                                          RESTATE_DRAIN_URI retires, which
#                                          waits for a deployment at
#                                          RESTATE_DRAIN_AFTER_URI
#
#   RESTATE_ADMIN_URL    default http://127.0.0.1:9070
#   WAIT_SECONDS         how long register waits for both ends (default 60)
#   DRAIN_POLL_SECONDS   default 60
set -euo pipefail

ADMIN="${RESTATE_ADMIN_URL:-http://127.0.0.1:9070}"
WAIT="${WAIT_SECONDS:-60}"

die() { echo "restate-deploy: $*" >&2; exit 1; }

# restate stores endpoints with a trailing slash.
norm() { case "$1" in */) echo "$1" ;; *) echo "$1/" ;; esac; }

# query <sql>: run sql against the admin api, print the json rows.
query() {
  curl -fsS --max-time 10 -X POST "$ADMIN/query" \
    -H 'content-type: application/json' -H 'accept: application/json' \
    -d "{\"query\":\"$1\"}"
}

deployment_id() {
  local out
  out=$(query "SELECT id FROM sys_deployment WHERE endpoint = '$(norm "$1")'") || return 1
  echo "$out" | grep -oE '"id":"[^"]+"' | head -n1 | cut -d'"' -f4 || true
}

inflight() {
  local out n
  out=$(query "SELECT count(*) AS n FROM sys_invocation i JOIN sys_deployment d ON i.pinned_deployment_id = d.id WHERE d.endpoint = '$(norm "$1")' AND i.status != 'completed'") || return 1
  n=$(echo "$out" | grep -oE '"n":[0-9]+' | cut -d: -f2 || true)
  [ -n "$n" ] || return 1
  echo "$n"
}

# wait_for <url> <what> [curl flags...]: poll until the url answers.
wait_for() {
  local url="$1" what="$2" i=0
  shift 2
  until curl -fsS -o /dev/null --max-time 2 "$@" "$url" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt "$WAIT" ] || die "$what at $url did not answer within ${WAIT}s"
    sleep 1
  done
}

register() {
  local uri="$1" force=false out
  [ "${2:-}" = force ] && force=true
  wait_for "$ADMIN/health" "restate admin"
  # the handler endpoint speaks h2c only.
  wait_for "$(norm "$uri")health" "orchestrator handler endpoint" --http2-prior-knowledge
  out=$(curl -fsS --max-time 30 -X POST "$ADMIN/deployments" \
    -H 'content-type: application/json' \
    -d "{\"uri\":\"$uri\",\"force\":$force}") || die "restate admin refused $uri"
  case "$out" in
    *'"Environment"'*) ;;
    *) die "registered $uri, but it serves no Environment object: $out" ;;
  esac
  echo "restate-deploy: registered $uri"
}

retire() {
  local uri="$1" id n
  id=$(deployment_id "$uri") || return 1
  [ -n "$id" ] || return 0
  n=$(inflight "$uri") || return 1
  if [ "$n" != 0 ]; then
    echo "restate-deploy: $n invocations still pinned to $uri ($id)" >&2
    return 1
  fi
  # restate only supports forced deletion; nothing is pinned to it any more.
  curl -fsS --max-time 30 -o /dev/null -X DELETE "$ADMIN/deployments/$id?force=true" || return 1
  echo "restate-deploy: retired $id ($uri)"
}

id() { deployment_id "$1"; }

drain() {
  local bin="$1" uri="${RESTATE_DRAIN_URI:?RESTATE_DRAIN_URI is not set}"
  local after="${RESTATE_DRAIN_AFTER_URI:?RESTATE_DRAIN_AFTER_URI is not set}" pid rc=0 next
  "$bin" &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    sleep "${DRAIN_POLL_SECONDS:-60}"
    # until the new version registers, the old deployment is the only one
    # restate can send work to, so it must stay even when idle.
    next=$(deployment_id "$after") || continue
    [ -n "$next" ] || continue
    if retire "$uri"; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" || true
      systemctl disable fuse-orchestrator-drain.service >/dev/null 2>&1 || true
      exit 0
    fi
  done
  wait "$pid" || rc=$?
  die "old orchestrator exited ($rc) before $uri drained"
}

cmd="${1:-}"
shift || true
case "$cmd" in
  register|id|inflight|retire|drain) "$cmd" "$@" ;;
  *) die "usage: restate-deploy register|id|inflight|retire|drain ..." ;;
esac
HELPER
  chmod 0755 "$RESTATE_DEPLOY"
}

# restate_up: write restate.env, install the units, pull and verify the image,
# start restate-server and wait for its admin api.
restate_up() {
  mkdir -p "$ENV_DIR"
  if [ ! -f "$RESTATE_ENV" ]; then
    (umask 077 && cat > "$RESTATE_ENV" <<EOF
# /etc/fuse/restate.env - restate-server config, passed into its container.
# Edit, then: sudo systemctl restart restate-server
# Every port stays on 127.0.0.1: the admin api has no auth, and ingress
# invokes handlers directly. Keep them off the network.
RESTATE_NODE_NAME=fuse-restate
RESTATE_BASE_DIR=/restate-data
RESTATE_LISTEN_MODE=tcp
RESTATE_INGRESS__BIND_ADDRESS=127.0.0.1:18080
RESTATE_ADMIN__BIND_ADDRESS=127.0.0.1:9070
RESTATE_BIND_ADDRESS=127.0.0.1:5122

# Keep step history of completed invocations (the default 0 drops it).
RESTATE_DEFAULT_JOURNAL_RETENTION=7d
RESTATE_DEFAULT_WORKFLOW_COMPLETION_RETENTION=7d

# Partition snapshots to object storage (s3 only, no shared filesystems):
# RESTATE_WORKER__SNAPSHOTS__DESTINATION=s3://bucket/prefix
# RESTATE_WORKER__SNAPSHOTS__SNAPSHOT_INTERVAL_NUM_RECORDS=10000
# AWS_ACCESS_KEY_ID=
# AWS_SECRET_ACCESS_KEY=
# AWS_REGION=
EOF
    )
    chmod 600 "$RESTATE_ENV"
    log "wrote $RESTATE_ENV"
  else
    log "$RESTATE_ENV exists - leaving its settings untouched"
  fi
  if [ -n "${RESTATE_SNAPSHOT_DESTINATION:-}" ]; then
    set_env_key "$RESTATE_ENV" RESTATE_WORKER__SNAPSHOTS__DESTINATION "$RESTATE_SNAPSHOT_DESTINATION"
    grep -qE '^RESTATE_WORKER__SNAPSHOTS__SNAPSHOT_INTERVAL_NUM_RECORDS=' "$RESTATE_ENV" \
      || set_env_key "$RESTATE_ENV" RESTATE_WORKER__SNAPSHOTS__SNAPSHOT_INTERVAL_NUM_RECORDS 10000
    log "snapshots -> $RESTATE_SNAPSHOT_DESTINATION"
  fi
  if grep -E '^RESTATE_[A-Z_]*BIND_ADDRESS=' "$RESTATE_ENV" | grep -vqE '=(127\.0\.0\.1|\[::1\]):'; then
    log "WARNING: $RESTATE_ENV binds a restate port beyond loopback; the admin api has no auth"
  fi

  mkdir -p "$UNIT_DIR"
  install_unit restate-server.service restate_server_unit
  install_unit fuse-orchestrator-drain.service drain_unit
  write_restate_deploy

  log "pulling $RESTATE_IMAGE"
  docker pull -q "$RESTATE_IMAGE" >/dev/null || die "docker pull $RESTATE_IMAGE failed"
  # pulling by digest already makes docker verify the content; check the
  # result anyway so a misbehaving registry mirror fails loudly.
  case " $(docker image inspect --format '{{range .RepoDigests}}{{.}} {{end}}' "$RESTATE_IMAGE" 2>/dev/null)" in
    *"@${RESTATE_IMAGE##*@}"*) log "image digest verified: ${RESTATE_IMAGE##*@}" ;;
    *) die "pulled $RESTATE_IMAGE but its digest does not match; refusing to run it" ;;
  esac

  systemctl daemon-reload
  systemctl enable restate-server.service
  systemctl restart restate-server.service
  local admin i=0
  admin=$(url_for "$(grep -E '^RESTATE_ADMIN__BIND_ADDRESS=' "$RESTATE_ENV" | tail -n1 | cut -d= -f2-)")
  until curl -fsS -o /dev/null --max-time 2 "$admin/health" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || die "restate-server did not answer on $admin within 60s (journalctl -u restate-server)"
    sleep 1
  done
  export RESTATE_ADMIN_URL="$admin"
  log "restate-server up, admin on $admin"
}

# plan_restate_upgrade: decide which handler address the new binary takes.
# runs after the new binary is staged, so the old one is stopped or drained
# only for the swap itself. sets HANDLER_LISTEN, REGISTER_FORCE and DRAIN.
#   - nothing registered yet: take the current address.
#   - the old binary supports drain-only mode: always blue/green. it keeps its
#     address (DRAIN_DIR) and the old deployment, the new binary takes the
#     other handler port, and the old deployment retires only after the new
#     one registered and nothing is pinned to it. no work can start on code
#     that is about to change.
#   - otherwise: stop the old orchestrator first, so nothing new starts on it,
#     then count what is pinned to it. replace it in place when that is zero
#     (or FUSE_RESTATE_FORCE_UPGRADE=1); start it again and refuse if not.
plan_restate_upgrade() {
  local cur cur_url id n host port other
  cur="$(env_value ORCH_RESTATE_LISTEN)"
  cur="${cur:-127.0.0.1:$HANDLER_PORT_A}"
  cur_url="$(url_for "$cur")"
  HANDLER_LISTEN="$cur"
  REGISTER_FORCE=force
  DRAIN=""
  [ -x "$BIN" ] || return 0
  id=$("$RESTATE_DEPLOY" id "$cur_url") || die "could not look up the restate deployment at $cur_url"
  [ -n "$id" ] || return 0

  if ! grep -aq ORCH_RESTATE_DRAIN_ONLY "$BIN"; then
    systemctl stop fuse-orchestrator.service
    n=$("$RESTATE_DEPLOY" inflight "$cur_url") || {
      systemctl start fuse-orchestrator.service
      die "could not count restate invocations on $cur_url; the old orchestrator was started again"
    }
    [ "$n" = 0 ] && return 0
    if [ "${FUSE_RESTATE_FORCE_UPGRADE:-}" != 1 ]; then
      systemctl start fuse-orchestrator.service
      die "$n restate invocations are pinned to the running orchestrator ($cur_url) and this binary cannot stay up to finish them (no ORCH_RESTATE_DRAIN_ONLY support). the old orchestrator was started again. wait for them, or set FUSE_RESTATE_FORCE_UPGRADE=1 to replace it anyway (they then replay on the new code)"
    fi
    log "FUSE_RESTATE_FORCE_UPGRADE=1: replacing a deployment with $n invocations pinned to it"
    return 0
  fi

  host="${cur%:*}"
  port="${cur##*:}"
  other=$HANDLER_PORT_A
  [ "$port" = "$HANDLER_PORT_A" ] && other=$HANDLER_PORT_B
  # the other address may still hold the deployment an earlier upgrade left
  # draining; it must be empty before the new binary takes it.
  "$RESTATE_DEPLOY" retire "$(url_for "$host:$other")" \
    || die "an earlier upgrade is still draining $(url_for "$host:$other"); wait for fuse-orchestrator-drain to finish (journalctl -u fuse-orchestrator-drain)"
  systemctl stop fuse-orchestrator-drain.service 2>/dev/null || true

  mkdir -p "$DRAIN_DIR"
  install -m0755 "$BIN" "$DRAIN_DIR/orchestrator"
  (umask 077 && cat > "$DRAIN_DIR/drain.env" <<EOF
# written by install-orchestrator.sh for fuse-orchestrator-drain.service
ORCH_RESTATE_LISTEN=$cur
ORCH_RESTATE_DRAIN_ONLY=true
RESTATE_DRAIN_URI=$cur_url
RESTATE_DRAIN_AFTER_URI=$(url_for "$host:$other")
RESTATE_ADMIN_URL=$RESTATE_ADMIN_URL
EOF
  )
  HANDLER_LISTEN="$host:$other"
  REGISTER_FORCE=""
  DRAIN=1
  log "blue/green: the old binary keeps serving $cur_url until what is pinned to it finishes, the new one takes $(url_for "$HANDLER_LISTEN")"
}

if [ "$MODE" = self ]; then
  # postgres stays the read model; the in-memory store forgets every vm on
  # restart while restate remembers the invocations about them.
  [ -n "$DB_URL" ] || die "FUSE_RESTATE_MODE=self needs DATABASE_URL (postgres); set it in the environment or in $ENV_FILE"
  command -v docker >/dev/null 2>&1 || die "FUSE_RESTATE_MODE=self runs restate-server in docker; install docker first (https://docs.docker.com/engine/install/debian/)"
  docker info >/dev/null 2>&1 || die "docker is installed but its daemon is not reachable (systemctl start docker)"
  command -v curl >/dev/null 2>&1 || die "curl is required for FUSE_RESTATE_MODE=self"
  case "${RESTATE_SNAPSHOT_DESTINATION:-s3://}" in
    s3://*) ;;
    *) die "RESTATE_SNAPSHOT_DESTINATION must be an s3:// url (got $RESTATE_SNAPSHOT_DESTINATION)" ;;
  esac
fi

# the env keys mode cloud writes. the orchestrator validates them at startup;
# the installer only checks that the required ones are there.
CLOUD_KEYS="FUSE_RESTATE_INGRESS_URL FUSE_RESTATE_API_KEY FUSE_RESTATE_IDENTITY_KEY FUSE_RESTATE_CONNECT FUSE_RESTATE_DEPLOYMENT_URL FUSE_RESTATE_ADMIN_URL FUSE_RESTATE_ENVIRONMENT_ID FUSE_RESTATE_REGION FUSE_RESTATE_TUNNEL_NAME RESTATE_INPROC_AUTH_TOKEN_FILE"
if [ "$MODE" = cloud ]; then
  for k in FUSE_RESTATE_INGRESS_URL FUSE_RESTATE_API_KEY FUSE_RESTATE_IDENTITY_KEY; do
    [ -n "${!k:-}" ] || [ -n "$(env_value "$k")" ] \
      || die "FUSE_RESTATE_MODE=cloud needs $k; set it in the environment or in $ENV_FILE"
  done
  if [ "${FUSE_RESTATE_CONNECT:-$(env_value FUSE_RESTATE_CONNECT)}" = public ]; then
    [ -n "${FUSE_RESTATE_DEPLOYMENT_URL:-$(env_value FUSE_RESTATE_DEPLOYMENT_URL)}" ] \
      || die "FUSE_RESTATE_CONNECT=public needs FUSE_RESTATE_DEPLOYMENT_URL (the https url restate cloud reaches the handler endpoint at)"
  fi
fi

# 1. resolve the binary and stage it next to the installed one. it replaces
#    the old binary only after the restate upgrade plan, so the old one is
#    stopped or drained for the swap alone, not for the download.
mkdir -p "$(dirname "$BIN")"
STAGED="$(dirname "$BIN")/.orchestrator.new"
case "$(uname -m)" in
  x86_64)        ASSET_ARCH="x86_64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) die "unsupported arch $(uname -m); set ORCH_BIN_SRC=/path/to/orchestrator" ;;
esac

if [ -n "${ORCH_BIN_SRC:-}" ] && [ -x "$ORCH_BIN_SRC" ]; then
  install -m0755 "$ORCH_BIN_SRC" "$STAGED"
  log "staged binary from $ORCH_BIN_SRC"
elif [ -x "./orchestrator" ]; then
  install -m0755 "./orchestrator" "$STAGED"
  log "staged binary from ./orchestrator"
else
  command -v curl >/dev/null 2>&1 || die "curl is required to download a release (or set ORCH_BIN_SRC)"
  TAG="${VERSION:-}"
  if [ -z "$TAG" ]; then
    TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 \
      | sed -E 's/.*"([^"]+)"$/\1/')
    [ -n "$TAG" ] || die "could not resolve latest release for $REPO; set VERSION or ORCH_BIN_SRC"
  fi
  ASSET="fuse_Linux_${ASSET_ARCH}.tar.gz"
  TGZ=$(mktemp); EXDIR=$(mktemp -d); SUMS=$(mktemp)
  trap 'rm -rf "$TGZ" "$EXDIR" "$SUMS"' EXIT
  log "downloading $ASSET ($TAG)"
  curl -fsSL -o "$TGZ" "https://github.com/$REPO/releases/download/$TAG/$ASSET" \
    || die "download failed for $TAG ($ASSET)"
  # Checksums for this exact tag, then verify before extracting or installing.
  curl -fsSL -o "$SUMS" "https://github.com/$REPO/releases/download/$TAG/checksums.txt" \
    || die "release $TAG publishes no checksums.txt - refusing to install unverified assets"
  verify_asset "$TGZ" "$ASSET" "$SUMS" || die "checksum verification failed for $ASSET ($TAG)"
  log "sha256 verified: $ASSET"
  tar -xzf "$TGZ" -C "$EXDIR" orchestrator
  install -m0755 "$EXDIR/orchestrator" "$STAGED"
  log "staged binary from release $TAG"
fi

if [ "$MODE" = self ]; then
  restate_up
  plan_restate_upgrade
fi
mv -f "$STAGED" "$BIN"
log "binary -> $BIN ($("$BIN" --version 2>/dev/null || echo '?'))"

# 2. write the env file, generating secrets, only if it does not exist so a
#    re-run never clobbers an operator's edits or rotates their tokens.
mkdir -p "$ENV_DIR"
if [ ! -f "$ENV_FILE" ]; then
  AUTH_TOKEN=$(openssl rand -hex 32)
  ENC_KEY=$(openssl rand -hex 32)
  (umask 077 && cat > "$ENV_FILE" <<EOF
# /etc/fuse/orchestrator.env - Fuse orchestrator config. Edit, then:
#   sudo systemctl restart fuse-orchestrator
ORCH_LISTEN=$LISTEN

# Auth: any non-empty ORCH_AUTH_TOKEN turns auth on. This is the token you
# pass to \`fuse connect --token\`. Keep it secret.
ORCH_AUTH_TOKEN=$AUTH_TOKEN

# Encrypts per-host agent tokens at rest (used once DATABASE_URL is set).
TOKEN_ENCRYPTION_KEY=$ENC_KEY

# State: empty = in-memory (does NOT survive a restart). For production set a
# Postgres URL and uncomment ORCH_REQUIRE_AUTH to fail closed on misconfig.
DATABASE_URL=$DB_URL
# ORCH_REQUIRE_AUTH=true

# Optional: restrict which source networks may reach the API.
# ORCH_ALLOWED_CIDRS=10.0.0.0/8,100.64.0.0/10
EOF
  )
  chmod 600 "$ENV_FILE"
  log "wrote $ENV_FILE (auth token + encryption key generated)"
else
  # shellcheck disable=SC1090
  AUTH_TOKEN=$(grep -E '^ORCH_AUTH_TOKEN=' "$ENV_FILE" | head -n1 | cut -d= -f2- || true)
  log "$ENV_FILE exists - leaving it untouched"
fi

# mode self: point the orchestrator at restate. these keys are the only edits
# made to an existing env file.
if [ "$MODE" = self ]; then
  [ -n "$(env_value DATABASE_URL)" ] || set_env_key "$ENV_FILE" DATABASE_URL "$DB_URL"
  set_env_key "$ENV_FILE" FUSE_RESTATE_MODE self
  set_env_key "$ENV_FILE" FUSE_RESTATE_INGRESS_URL \
    "$(url_for "$(grep -E '^RESTATE_INGRESS__BIND_ADDRESS=' "$RESTATE_ENV" | tail -n1 | cut -d= -f2-)")"
  set_env_key "$ENV_FILE" ORCH_RESTATE_LISTEN "$HANDLER_LISTEN"
  log "set FUSE_RESTATE_MODE=self in $ENV_FILE (handler endpoint $HANDLER_LISTEN)"
elif [ "$MODE" = cloud ]; then
  # only the keys given to this run are written, so a re-run keeps the rest.
  set_env_key "$ENV_FILE" FUSE_RESTATE_MODE cloud
  for k in $CLOUD_KEYS; do
    [ -z "${!k:-}" ] || set_env_key "$ENV_FILE" "$k" "${!k}"
  done
  connect="$(env_value FUSE_RESTATE_CONNECT)"
  log "set FUSE_RESTATE_MODE=cloud in $ENV_FILE (connect ${connect:-tunnel}); the orchestrator registers itself with restate cloud"
elif [ -n "${FUSE_RESTATE_MODE:-}" ]; then
  set_env_key "$ENV_FILE" FUSE_RESTATE_MODE "$MODE"
  log "set FUSE_RESTATE_MODE=$MODE in $ENV_FILE"
fi

# 3. install the systemd unit (prefer the tracked ops/systemd/ copy, fall back
#    to an inline definition so a curl'd standalone script still works).
mkdir -p "$UNIT_DIR"
if [ -f "$SCRIPT_DIR/systemd/fuse-orchestrator.service" ]; then
  install -m0644 "$SCRIPT_DIR/systemd/fuse-orchestrator.service" "$UNIT"
else
  cat > "$UNIT" <<EOF
[Unit]
Description=Fuse orchestrator (control plane)
Documentation=https://github.com/$REPO
After=network-online.target restate-server.service
Wants=network-online.target restate-server.service

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=on-failure
RestartSec=2
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
fi
log "installed unit -> $UNIT"

systemctl daemon-reload
if [ -n "${DRAIN:-}" ]; then
  # the old binary must let go of its handler address before its drain-only
  # copy can take it.
  systemctl stop fuse-orchestrator.service
  systemctl enable --now fuse-orchestrator-drain.service
  log "fuse-orchestrator-drain serving the old deployment until its invocations finish"
fi
systemctl enable fuse-orchestrator.service
# restart, not just start: a re-run is how an upgrade lands.
systemctl restart fuse-orchestrator.service
sleep 1
systemctl --no-pager --lines=0 status fuse-orchestrator.service || true

# register the new deployment once its handler endpoint is up. in place
# (forced) when nothing is pinned to the old one, as a new deployment beside
# it otherwise.
if [ "$MODE" = self ]; then
  "$RESTATE_DEPLOY" register "$(url_for "$HANDLER_LISTEN")" $REGISTER_FORCE \
    || die "restate registration failed; the orchestrator may lack FUSE_RESTATE_MODE=self support (journalctl -u fuse-orchestrator)"
fi

# 4. print the connect command so the operator never has to guess which token
#    goes where.
# prefer IPv4: the printed URL is a plain host:port authority, and an
# unbracketed IPv6 address would make it malformed. bracket v6 when that is all
# this host has.
IP=$(curl -4 -fsS ifconfig.me 2>/dev/null || true)
[ -z "$IP" ] && IP=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '/:/ {next} NF {print; exit}')
[ -z "$IP" ] && IP=$(curl -6 -fsS ifconfig.me 2>/dev/null || true)
[ -z "$IP" ] && IP="<this-host>"
case "$IP" in *:*) IP="[$IP]" ;; esac
PORT="${LISTEN##*:}"
echo
log "orchestrator installed and listening on $LISTEN"
echo
echo "  fuse connect http://${IP}:${PORT} --token ${AUTH_TOKEN:-<see $ENV_FILE>} --master"
echo
