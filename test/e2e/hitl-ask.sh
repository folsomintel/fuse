#!/usr/bin/env bash
# end-to-end tests for hitl interventions (issue #309): the ask loop against a
# real orchestrator and host.
#
#   FUSE_URL=http://<orchestrator>:8080 FUSE_TOKEN=<master token> \
#     FUSE_BIN=/tmp/fuse-main ./test/e2e/hitl-ask.sh
#
# the cli must already be connected (with --master) to the same orchestrator,
# with a host selected, the same as run.sh. the agent side is plain curl so
# nothing here depends on an sdk build.
#
# two cases need access beyond the api and are skipped unless enabled:
#   ORCH_RESTART_CMD  a command that restarts the orchestrator, e.g.
#                     "ssh debian@host sudo systemctl restart fuse-orchestrator"
#   PSQL_CMD          a command that runs one sql statement read from stdin
#                     against the orchestrator database, e.g.
#                     "ssh debian@host sudo -u postgres psql -tA fuse"
#
# every environment is created under E2E_PREFIX and destroyed on exit.
set -uo pipefail

FUSE="${FUSE_BIN:-fuse}"
URL="${FUSE_URL:-}"
TOKEN="${FUSE_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
HERE="$(cd "$(dirname "$0")" && pwd)"
FF="$HERE/fusefiles/Fusefile.hitl-ask"

PASS=0; FAIL=0; SKIP=0
FAILED_CASES=()
ok()    { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()   { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); FAILED_CASES+=("$1"); }
skip()  { printf '  \033[33mskip\033[0m %s\n' "$1"; SKIP=$((SKIP + 1)); }
note()  { printf '       %s\n' "$1"; }
head_() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

if [ -z "$URL" ] || [ -z "$TOKEN" ]; then
  echo "FUSE_URL and FUSE_TOKEN (the master token) must both be set" >&2
  exit 2
fi

ENVS=()
cleanup() {
  head_ "cleanup"
  for id in ${ENVS[@]+"${ENVS[@]}"}; do
    "$FUSE" environment destroy "$id" --yes >/dev/null 2>&1 && note "destroyed $id"
  done
}
trap cleanup EXIT INT TERM

api() { # api <method> <path> [json-body]
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -X "$method" "$URL$path" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" -d "$body"
  else
    curl -sS -X "$method" "$URL$path" -H "Authorization: Bearer $TOKEN"
  fi
}

# field <dotted.path> reads one json field from stdin, empty when absent.
field() {
  python3 -c '
import json, sys
v = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else v)' "$1"
}

expect() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; note "expected: $2"; note "actual:   $3"; fi
}

# up <case> - create an environment from the hitl Fusefile and record its id
# in UP_ID (not printed: a command substitution would lose the ENVS entry
# cleanup needs).
up() {
  local task="$PREFIX-hitl-$1-$$"
  UP_ID="fuse-$task"
  ENVS+=("$UP_ID")
  if ! "$FUSE" up -f "$FF" --task-id "$task" >"/tmp/$task.log" 2>&1; then
    bad "fuse up ($1), log: /tmp/$task.log"
    return 1
  fi
}

# ask <env> <question> [timeout_seconds] - raise an ask, print its id.
ask() {
  local body
  body="$(python3 -c 'import json,sys; b={"kind":"ask","question":sys.argv[1],"choices":["yes","no"]}
if sys.argv[2]: b["timeout_seconds"]=int(sys.argv[2])
print(json.dumps(b))' "$2" "${3:-}")"
  api POST "/v1/environments/$1?action=request" "$body" | field id
}

# wait_for <id> <max seconds> - long-poll like an sdk until terminal, print
# the final json. survives the orchestrator going away by retrying.
wait_for() {
  local id="$1" until=$(( $(date +%s) + $2 )) out state
  while [ "$(date +%s)" -lt "$until" ]; do
    out="$(api GET "/v1/interventions/$id?wait=20s" 2>/dev/null)"
    state="$(printf '%s' "$out" | field state 2>/dev/null)"
    if [ -n "$state" ] && [ "$state" != "pending" ]; then
      printf '%s' "$out"
      return 0
    fi
    [ -z "$state" ] && sleep 2
  done
  printf '%s' "$out"
}

# ---------------------------------------------------------------------------
head_ "ask answered from the cli reaches the waiting caller"
ENV=""
if up answer; then
  ENV="$UP_ID"
  ID="$(ask "$ENV" "e2e: deploy?")"
  if [ -z "$ID" ]; then
    bad "raise an ask"
  else
    ok "raise an ask ($ID)"
    wait_for "$ID" 60 >"/tmp/$ID.json" &
    WAITER=$!
    sleep 2
    "$FUSE" hitl list --env "$ENV" | grep -q "$ID" && ok "fuse hitl list shows it" || bad "fuse hitl list shows it"
    "$FUSE" hitl answer "$ID" yes >/dev/null 2>&1 && ok "fuse hitl answer" || bad "fuse hitl answer"
    wait "$WAITER"
    expect "waiter sees approved" "approved" "$(field state <"/tmp/$ID.json")"
    expect "waiter gets the answer" "yes" "$(field decision.answer <"/tmp/$ID.json")"
    CODE="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$URL/v1/interventions/$ID?action=reject" \
      -H "Authorization: Bearer $TOKEN")"
    expect "deciding twice is 409" "409" "$CODE"
    rm -f "/tmp/$ID.json"
  fi
fi

# ---------------------------------------------------------------------------
head_ "an ask nobody answers expires with on_timeout applied"
if [ -n "$ENV" ]; then
  ID="$(ask "$ENV" "e2e: nobody will answer" 30)"
  OUT="$(wait_for "$ID" 90)"
  expect "state is expired" "expired" "$(printf '%s' "$OUT" | field state)"
  expect "on_timeout reject applied" "reject" "$(printf '%s' "$OUT" | field decision.outcome)"
  expect "decided by system" "system" "$(printf '%s' "$OUT" | field decision.decided_by.id)"
fi

# ---------------------------------------------------------------------------
head_ "destroying the env cancels a pending ask"
if up destroy; then
  ENV2="$UP_ID"
  ID="$(ask "$ENV2" "e2e: about to be destroyed")"
  wait_for "$ID" 60 >"/tmp/$ID.json" &
  WAITER=$!
  sleep 2
  "$FUSE" environment destroy "$ENV2" --yes >/dev/null 2>&1
  wait "$WAITER"
  expect "waiter sees cancelled" "cancelled" "$(field state <"/tmp/$ID.json")"
  expect "cancel is a reject" "reject" "$(field decision.outcome <"/tmp/$ID.json")"
  rm -f "/tmp/$ID.json"
fi

# ---------------------------------------------------------------------------
head_ "a pending ask keeps an idle env alive"
if [ -n "$ENV" ]; then
  ID="$(ask "$ENV" "e2e: hold the env open")"
  # idle_timeout is 1m and the reaper needs two strikes on a 30s loop; 150s
  # is comfortably past the point it would have reaped.
  note "waiting 150s past the 1m idle timeout"
  sleep 150
  STATE="$("$FUSE" -o json environment get "$ENV" 2>/dev/null | field state)"
  expect "env still running" "running" "$STATE"
  "$FUSE" hitl reject "$ID" --reason "e2e done" >/dev/null 2>&1
fi

# ---------------------------------------------------------------------------
head_ "an orchestrator restart loses nothing"
if [ -z "${ORCH_RESTART_CMD:-}" ]; then
  skip "set ORCH_RESTART_CMD to run this case"
elif [ -n "$ENV" ]; then
  ID="$(ask "$ENV" "e2e: survive a restart")"
  wait_for "$ID" 120 >"/tmp/$ID.json" &
  WAITER=$!
  sh -c "$ORCH_RESTART_CMD" >/dev/null 2>&1
  for _ in $(seq 1 30); do api GET "/v1/interventions/$ID" >/dev/null 2>&1 && break; sleep 2; done
  expect "still pending after restart" "pending" "$(api GET "/v1/interventions/$ID" | field state)"
  "$FUSE" hitl answer "$ID" no >/dev/null 2>&1
  wait "$WAITER"
  expect "waiter resumes and gets the answer" "no" "$(field decision.answer <"/tmp/$ID.json")"
  rm -f "/tmp/$ID.json"
fi

# ---------------------------------------------------------------------------
head_ "the payload is ciphertext in postgres"
if [ -z "${PSQL_CMD:-}" ]; then
  skip "set PSQL_CMD to run this case"
elif [ -n "$ENV" ]; then
  ID="$(ask "$ENV" "e2e-plaintext-marker")"
  ROW="$(printf "SELECT sealed, encode(payload, 'escape') FROM orchestrator_interventions WHERE intervention_id = '%s';\n" "$ID" | sh -c "$PSQL_CMD")"
  case "$ROW" in
    t\|*) ok "row is marked sealed" ;;
    *) bad "row is marked sealed"; note "row: $ROW" ;;
  esac
  case "$ROW" in
    *e2e-plaintext-marker*) bad "question is not stored in the clear" ;;
    *) ok "question is not stored in the clear" ;;
  esac
  "$FUSE" hitl reject "$ID" --reason "e2e done" >/dev/null 2>&1
fi

printf '\n%d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
for c in ${FAILED_CASES[@]+"${FAILED_CASES[@]}"}; do printf '  failed: %s\n' "$c"; done
[ "$FAIL" -eq 0 ]
