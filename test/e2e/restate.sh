#!/usr/bin/env bash
# restate wiring check: an orchestrator running with FUSE_RESTATE_MODE=self is
# registered with restate-server, both virtual objects answer Ping through
# ingress, /ready reports restate, and a restate outage degrades /ready
# instead of failing it.
#
# run it on the machine the orchestrator runs on (restate binds loopback):
#
#   RESTATE_COMPOSE=1 ./test/e2e/restate.sh    # start restate from
#                                              # ops/restate/compose.yaml and
#                                              # run the outage case too
#   ./test/e2e/restate.sh                      # restate already running
#
# against restate cloud (FUSE_RESTATE_MODE=cloud), from anywhere: the
# orchestrator registers itself, so this only waits for that, pings through
# cloud ingress and, for FUSE_RESTATE_CONNECT=public, checks that an unsigned
# request to the handler endpoint is refused:
#
#   RESTATE_CLOUD=1 RESTATE_API_KEY=key_... \
#   RESTATE_INGRESS_URL=https://<id>.env.<region>.restate.cloud:8080 \
#   ORCH_URL=https://orch.example.com \
#   ORCH_RESTATE_PUBLIC_URL=https://restate.orch.example.com \  # public only
#     ./test/e2e/restate.sh
#
#   ORCH_URL             orchestrator api (default http://127.0.0.1:8080)
#   RESTATE_ADMIN_URL    default http://127.0.0.1:9070
#   RESTATE_INGRESS_URL  default http://127.0.0.1:18080
#   ORCH_RESTATE_URL     handler endpoint (default http://127.0.0.1:9080)
#   RESTATE_API_KEY      bearer token for cloud ingress and admin
#   ORCH_RESTATE_PUBLIC_URL  the public handler url (cloud, connect public)
#   RESTATE_STOP_CMD / RESTATE_START_CMD
#                        stop and start restate-server for the outage case;
#                        set by RESTATE_COMPOSE=1, skipped when unset
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
COMPOSE="$ROOT/ops/restate/compose.yaml"

ORCH="${ORCH_URL:-http://127.0.0.1:8080}"
ADMIN="${RESTATE_ADMIN_URL:-http://127.0.0.1:9070}"
INGRESS="${RESTATE_INGRESS_URL:-http://127.0.0.1:18080}"
ENDPOINT="${ORCH_RESTATE_URL:-http://127.0.0.1:9080}"
CLOUD="${RESTATE_CLOUD:-}"
AUTH=()
if [ "$CLOUD" = 1 ]; then
  : "${RESTATE_API_KEY:?RESTATE_CLOUD=1 needs RESTATE_API_KEY}"
  : "${RESTATE_INGRESS_URL:?RESTATE_CLOUD=1 needs RESTATE_INGRESS_URL}"
  AUTH=(-H "Authorization: Bearer $RESTATE_API_KEY")
fi

if [ "${RESTATE_COMPOSE:-}" = 1 ]; then
  RESTATE_STOP_CMD="${RESTATE_STOP_CMD:-docker compose -f $COMPOSE stop restate}"
  RESTATE_START_CMD="${RESTATE_START_CMD:-docker compose -f $COMPOSE start restate}"
fi

PASS=0; FAIL=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); }
note() { printf '       %s\n' "$1"; }

# expect <name> <haystack> <needle>: pass when the needle is in the haystack.
expect() {
  case "$2" in
    *"$3"*) ok "$1" ;;
    *) bad "$1"; note "want: $3"; note "got:  $2" ;;
  esac
}

# ready_restate <want>: poll /ready until checks.restate is <want> ("ok") or,
# for any other want, until it is something other than ok. prints the body.
ready_restate() {
  local want="$1" body="" _
  for _ in $(seq 1 30); do
    body=$(curl -sS --max-time 5 "$ORCH/ready" 2>/dev/null || true)
    case "$want:$body" in
      ok:*'"restate":"ok"'*) break ;;
      down:*'"restate":"'*) case "$body" in *'"restate":"ok"'*) ;; *) break ;; esac ;;
    esac
    sleep 1
  done
  printf '%s' "$body"
}

printf '\n\033[1m== restate\033[0m\n'

if [ "${RESTATE_COMPOSE:-}" = 1 ]; then
  if docker compose -f "$COMPOSE" up -d >/dev/null 2>&1; then
    ok "restate-server started from ops/restate/compose.yaml"
  else
    bad "docker compose up"
    exit 1
  fi
fi

if [ "$CLOUD" = 1 ]; then
  # the orchestrator registers itself once the tunnel (or its listener) is
  # up, so the first successful ping is the registration check.
  out=""
  for _ in $(seq 1 60); do
    out=$(curl -sS --max-time 10 ${AUTH[@]+"${AUTH[@]}"} -X POST "$INGRESS/Environment/e2e-restate-ping/Ping" 2>/dev/null || true)
    case "$out" in *'"object":"Environment"'*) break ;; esac
    sleep 2
  done
  expect "orchestrator registered itself with restate cloud" "$out" '"object":"Environment"'
else
  if RESTATE_ADMIN_URL="$ADMIN" ORCH_RESTATE_URL="$ENDPOINT" "$ROOT/ops/restate/register.sh" >/dev/null; then
    ok "deployment registered"
  else
    bad "deployment registered"
    exit 1
  fi
  deps=$(curl -sS --max-time 10 "$ADMIN/deployments" || true)
  expect "deployments list shows the orchestrator endpoint" "$deps" "\"$ENDPOINT/\""
fi

for obj in Environment Host; do
  out=$(curl -sS --max-time 10 ${AUTH[@]+"${AUTH[@]}"} -X POST "$INGRESS/$obj/e2e-restate-ping/Ping" || true)
  expect "$obj/e2e-restate-ping/Ping through ingress" "$out" \
    "{\"object\":\"$obj\",\"key\":\"e2e-restate-ping\",\"known\":false}"
done

if [ "$CLOUD" = 1 ] && [ -n "${ORCH_RESTATE_PUBLIC_URL:-}" ]; then
  # restate signs every call; anyone else reaching the public url gets a 401.
  for path in discover invoke/Host/Ping; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST \
      -H 'content-type: application/vnd.restate.invocation.v5' "$ORCH_RESTATE_PUBLIC_URL/$path")
    expect "unsigned /$path on the public handler url is refused" "$code" "401"
  done
fi

body=$(ready_restate ok)
expect "/ready reports restate ok" "$body" '"restate":"ok"'
expect "/ready status ready" "$body" '"status":"ready"'

if [ -n "${RESTATE_STOP_CMD:-}" ] && [ -n "${RESTATE_START_CMD:-}" ]; then
  if eval "$RESTATE_STOP_CMD" >/dev/null 2>&1; then
    body=$(ready_restate down)
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$ORCH/ready")
    expect "/ready is degraded with restate down" "$body" '"status":"degraded"'
    expect "/ready stays 200 with restate down" "$code" "200"
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$ORCH/health")
    expect "orchestrator still alive with restate down" "$code" "200"
    eval "$RESTATE_START_CMD" >/dev/null 2>&1 || bad "restart restate-server"
    body=$(ready_restate ok)
    expect "/ready recovers once restate is back" "$body" '"restate":"ok"'
  else
    bad "stop restate-server ($RESTATE_STOP_CMD)"
  fi
else
  note "skip: outage case (set RESTATE_COMPOSE=1 or RESTATE_STOP_CMD/RESTATE_START_CMD)"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
