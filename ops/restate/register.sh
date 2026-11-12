#!/usr/bin/env bash
# register.sh: register the orchestrator's restate handler endpoint with
# restate-server, so restate knows the Environment and Host objects and can
# invoke them. run it after the orchestrator starts with FUSE_RESTATE_MODE=self,
# and again after every orchestrator upgrade (registration is how restate
# picks up handler changes).
#
#   RESTATE_ADMIN_URL   restate admin api (default http://127.0.0.1:9070)
#   ORCH_RESTATE_URL    handler endpoint as restate-server reaches it
#                       (default http://127.0.0.1:9080)
#   WAIT_SECONDS        how long to wait for both to come up (default 30)
set -euo pipefail

ADMIN="${RESTATE_ADMIN_URL:-http://127.0.0.1:9070}"
ENDPOINT="${ORCH_RESTATE_URL:-http://127.0.0.1:9080}"
WAIT="${WAIT_SECONDS:-30}"

# wait_for <url> <what> [curl flags...]: poll until the url answers, or fail
# after WAIT.
wait_for() {
  local url="$1" what="$2" i=0
  shift 2
  until curl -fsS -o /dev/null --max-time 2 "$@" "$url" 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -ge "$WAIT" ]; then
      echo "register: $what at $url did not answer within ${WAIT}s" >&2
      return 1
    fi
    sleep 1
  done
}

wait_for "$ADMIN/health" "restate admin"
# the handler endpoint speaks h2c only, so curl has to start in http/2.
wait_for "$ENDPOINT/health" "orchestrator handler endpoint" --http2-prior-knowledge

# force re-registers an endpoint already known at this uri, which is what an
# upgrade on the same address needs.
out=$(curl -fsS --max-time 30 -X POST "$ADMIN/deployments" \
  -H 'content-type: application/json' \
  -d "{\"uri\":\"$ENDPOINT\",\"force\":true}") || {
  echo "register: restate admin refused $ENDPOINT" >&2
  exit 1
}
case "$out" in
  *'"Environment"'*'"Host"'*|*'"Host"'*'"Environment"'*) ;;
  *) echo "register: registered, but the response lacks the Environment/Host services: $out" >&2; exit 1 ;;
esac
echo "register: $ENDPOINT registered with $ADMIN"
