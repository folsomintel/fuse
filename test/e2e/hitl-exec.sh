#!/usr/bin/env bash
# end-to-end tests for hitl exec and lifecycle gates (issue #311) against a
# real orchestrator and host.
#
#   FUSE_URL=http://<orchestrator>:8080 FUSE_TOKEN=<master token> \
#     FUSE_BIN=/tmp/fuse-main ./test/e2e/hitl-exec.sh
#
# the cli must already be connected (with --master) to the same orchestrator,
# with a host selected, the same as run.sh. exec is master-token only, so the
# agent side is the master token; the approver is an invite pinned to alice,
# or the cli where a case exercises it.
#
# the no-destroy env cannot be destroyed through the api by design; its
# max_runtime (10m) reaps it. every other env and key is cleaned up on exit.
set -uo pipefail

FUSE="${FUSE_BIN:-fuse}"
URL="${FUSE_URL:-}"
TOKEN="${FUSE_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"

PASS=0; FAIL=0
FAILED_CASES=()
ok()    { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()   { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); FAILED_CASES+=("$1"); }
note()  { printf '       %s\n' "$1"; }
head_() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

if [ -z "$URL" ] || [ -z "$TOKEN" ]; then
  echo "FUSE_URL and FUSE_TOKEN (the master token) must both be set" >&2
  exit 2
fi

ENVS=()
KEYS=()
BG=()
cleanup() {
  head_ "cleanup"
  for p in ${BG[@]+"${BG[@]}"}; do kill "$p" 2>/dev/null; done
  for id in ${ENVS[@]+"${ENVS[@]}"}; do
    "$FUSE" environment destroy "$id" --yes >/dev/null 2>&1 && note "destroyed $id"
  done
  for k in ${KEYS[@]+"${KEYS[@]}"}; do
    "$FUSE" apikeys revoke "$k" --yes >/dev/null 2>&1
  done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# call <token> <method> <path> [json-body] - prints the body, then the http
# status on the last line.
call() {
  local tok="$1" method="$2" path="$3" body="${4:-}"
  local args=(-sS -X "$method" "$URL$path" -H "Authorization: Bearer $tok" -w '\n%{http_code}')
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  curl "${args[@]}"
}
status() { tail -n 1; }
body()   { sed '$d'; }

# field <dotted.path> reads one json field from stdin, empty when absent. list
# values print as json.
field() {
  python3 -c '
import json, sys
v = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v = v.get(k) if isinstance(v, dict) else None
print("" if v is None else (json.dumps(v) if isinstance(v, (list, dict)) else v))' "$1"
}

expect() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1"; note "expected: $2"; note "actual:   $3"; fi
}

# exec_argv <env> <argv...> - POST an exec, prints body + status.
exec_argv() {
  local env="$1"; shift
  call "$TOKEN" POST "/v1/environments/$env?action=exec" \
    "$(python3 -c 'import json,sys; print(json.dumps({"cmd": sys.argv[1:]}))' "$@")"
}

# settle <intervention id> - long-poll until settled, print the intervention.
settle() {
  local out
  for _ in $(seq 1 12); do
    out="$(call "$TOKEN" GET "/v1/interventions/$1?wait=30s" | body)"
    case "$(printf '%s' "$out" | field state)" in
      pending) continue ;;
      approved|expired)
        [ -n "$(printf '%s' "$out" | field result)" ] || [ "$(printf '%s' "$out" | field mode)" != "ask" ] || continue ;;
    esac
    break
  done
  printf '%s' "$out"
}

up() {
  local task="$PREFIX-hitl-exec-$1-$$"
  UP_ID="fuse-$task"
  [ "${3:-}" = "keep" ] || ENVS+=("$UP_ID")
  if ! "$FUSE" up -f "$HERE/fusefiles/$2" --task-id "$task" >"$WORK/$task.log" 2>&1; then
    bad "fuse up ($1), log: $WORK/$task.log"
    return 1
  fi
}

# ---------------------------------------------------------------------------
head_ "setup"
ENV=""; ALLOW=""; NODESTROY=""
up gates Fusefile.hitl-exec && ENV="$UP_ID"
up allow Fusefile.hitl-exec-allowlist && ALLOW="$UP_ID"
up nodestroy Fusefile.hitl-exec-nodestroy keep && NODESTROY="$UP_ID"
if [ -n "$ENV" ]; then
  read -r INV_ID INVITE < <("$FUSE" -o json hitl invite "$ENV" --scopes decide --ttl 30m --actor alice 2>/dev/null |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["id"], d["key"])') || true
  [ -n "${INVITE:-}" ] && { KEYS+=("$INV_ID"); ok "approver invite for alice"; } || bad "approver invite for alice"
fi

# ---------------------------------------------------------------------------
head_ "git push --force is held, approved with an edit, and the edit is what runs"
if [ -n "$ENV" ]; then
  OUT="$(exec_argv "$ENV" git push --force origin main)"
  expect "202 held" "202" "$(printf '%s' "$OUT" | status)"
  ID="$(printf '%s' "$OUT" | body | field intervention_id)"
  expect "held by force-push" "force-push" "$(printf '%s' "$OUT" | body | field intervention.gate)"
  OUT="$(call "$TOKEN" GET "/v1/interventions/$ID" | body)"
  expect "still pending" "pending" "$(printf '%s' "$OUT" | field state)"

  OUT="$(call "$INVITE" POST "/v1/interventions/$ID?action=approve" '{"edited_argv":["rm","-rf","/"]}')"
  expect "an edit that hits a deny gate is 403" "403" "$(printf '%s' "$OUT" | status)"
  expect "and stays pending" "pending" "$(call "$TOKEN" GET "/v1/interventions/$ID" | body | field state)"

  OUT="$(call "$INVITE" POST "/v1/interventions/$ID?action=approve" '{"edited_argv":["sh","-c","echo edited > /tmp/hitl-edited"]}')"
  expect "approve with edit" "200" "$(printf '%s' "$OUT" | status)"
  expect "decided by alice" "alice" "$(printf '%s' "$OUT" | body | field decision.decided_by.id)"
  OUT="$(settle "$ID")"
  expect "edited argv recorded" '["sh", "-c", "echo edited > /tmp/hitl-edited"]' "$(printf '%s' "$OUT" | field decision.edited_argv)"
  expect "edited exec exited 0" "0" "$(printf '%s' "$OUT" | field result.exec.exit_code)"
  OUT="$(exec_argv "$ENV" cat /tmp/hitl-edited)"
  expect "the guest ran the edit" "edited" "$(printf '%s' "$OUT" | body | field stdout | tr -d '\n')"
fi

# ---------------------------------------------------------------------------
head_ "wrappers do not hide the program"
if [ -n "$ENV" ]; then
  for argv in "env GIT_TRACE=1 git push --force" "sudo git push --force" "sudo -u root nice -n 5 git push --force"; do
    # shellcheck disable=SC2086
    OUT="$(exec_argv "$ENV" $argv)"
    expect "$argv is held" "202" "$(printf '%s' "$OUT" | status)"
    ID="$(printf '%s' "$OUT" | body | field intervention_id)"
    [ -n "$ID" ] && call "$INVITE" POST "/v1/interventions/$ID?action=reject" '{"reason":"e2e"}' >/dev/null
  done
fi

# ---------------------------------------------------------------------------
head_ "the cli waits on a held exec and prints the intervention"
if [ -n "$ENV" ]; then
  "$FUSE" environment exec "$ENV" -- git push --force >"$WORK/cli.out" 2>"$WORK/cli.err" &
  CLI_PID=$!; BG+=("$CLI_PID")
  ID=""
  for _ in $(seq 1 30); do
    ID="$(grep -o 'intervention int_[0-9a-f]*' "$WORK/cli.err" 2>/dev/null | head -n 1 | cut -d' ' -f2)"
    [ -n "$ID" ] && break
    sleep 1
  done
  [ -n "$ID" ] && ok "cli printed waiting on intervention $ID" || bad "cli printed waiting on an intervention"
  if [ -n "$ID" ]; then
    call "$INVITE" POST "/v1/interventions/$ID?action=reject" '{"reason":"e2e says no"}' >/dev/null
    wait "$CLI_PID"; RC=$?
    [ "$RC" -ne 0 ] && ok "cli exits non-zero on reject" || bad "cli exits non-zero on reject"
    grep -q "e2e says no" "$WORK/cli.err" && ok "cli shows the reason" || { bad "cli shows the reason"; note "$(cat "$WORK/cli.err")"; }
  fi
fi

# ---------------------------------------------------------------------------
head_ "a notify gate lets the exec through and leaves a terminal record"
if [ -n "$ENV" ]; then
  OUT="$(call "$TOKEN" POST "/v1/environments/$ENV?action=exec" '{"shell":"echo notified"}')"
  expect "200 straight through" "200" "$(printf '%s' "$OUT" | status)"
  expect "it ran" "notified" "$(printf '%s' "$OUT" | body | field stdout | tr -d '\n')"
  OUT="$(call "$TOKEN" GET "/v1/interventions?env=$ENV&state=approved" | body)"
  NOTE="$(printf '%s' "$OUT" | python3 -c '
import json, sys
for i in json.load(sys.stdin)["interventions"]:
    if i.get("gate") == "watch-shell" and i.get("mode") == "notify":
        print(i["id"])')"
  NOTE="$(printf '%s' "$NOTE" | tail -n 1)"
  [ -n "$NOTE" ] && ok "terminal notify intervention $NOTE" || bad "terminal notify intervention"
  ROWS="$(call "$TOKEN" GET "/v1/hitl/audit?env=$ENV&limit=200" | body | python3 -c '
import json, sys
print(" ".join(e["event"] for e in json.load(sys.stdin)["entries"] if e.get("intervention_id") == sys.argv[1]))' "$NOTE")"
  expect "audit rows raised + decided" "raised decided" "$ROWS"
fi

# ---------------------------------------------------------------------------
head_ "a grant: env approval suppresses the second identical prompt"
if [ -n "$ENV" ]; then
  OUT="$(exec_argv "$ENV" echo deploy)"
  expect "first echo deploy is held" "202" "$(printf '%s' "$OUT" | status)"
  ID="$(printf '%s' "$OUT" | body | field intervention_id)"
  "$FUSE" hitl approve "$ID" --grant env >/dev/null 2>&1 && ok "cli approve --grant env" || bad "cli approve --grant env"
  expect "grant recorded" "env" "$(settle "$ID" | field decision.grant)"
  OUT="$(exec_argv "$ENV" echo deploy)"
  expect "the repeat runs without a prompt" "200" "$(printf '%s' "$OUT" | status)"
  expect "and prints" "deploy" "$(printf '%s' "$OUT" | body | field stdout | tr -d '\n')"
  OUT="$(exec_argv "$ENV" echo deploy prod)"
  expect "a different argv still prompts" "202" "$(printf '%s' "$OUT" | status)"
  ID="$(printf '%s' "$OUT" | body | field intervention_id)"
  [ -n "$ID" ] && call "$INVITE" POST "/v1/interventions/$ID?action=reject" '{"reason":"e2e"}' >/dev/null
  EVENTS="$(call "$TOKEN" GET "/v1/hitl/audit?env=$ENV&limit=200" | body | python3 -c '
import json, sys
print(" ".join(sorted({e["event"] for e in json.load(sys.stdin)["entries"] if e["event"].startswith("grant_")})))')"
  expect "grant rows audited" "grant_created grant_used" "$EVENTS"
fi

# ---------------------------------------------------------------------------
head_ "a held exec does not block an unrelated snapshot of the same vm"
if [ -n "$ENV" ]; then
  OUT="$(exec_argv "$ENV" git push --force)"
  HELD="$(printf '%s' "$OUT" | body | field intervention_id)"
  START=$(date +%s)
  OUT="$(call "$TOKEN" POST "/v1/environments/$ENV/snapshots" '{"comment":"e2e hitl-exec"}')"
  expect "snapshot 201 while the exec is held" "201" "$(printf '%s' "$OUT" | status)"
  note "snapshot took $(( $(date +%s) - START ))s"
  SNAP="$(printf '%s' "$OUT" | body | field id)"
  expect "the exec is still pending" "pending" "$(call "$TOKEN" GET "/v1/interventions/$HELD" | body | field state)"
  call "$INVITE" POST "/v1/interventions/$HELD?action=reject" '{"reason":"e2e"}' >/dev/null
  [ -n "$SNAP" ] && "$FUSE" snapshot delete "$SNAP" --yes >/dev/null 2>&1
fi

# ---------------------------------------------------------------------------
head_ "allowlist mode holds python -c when python is not allowed"
if [ -n "$ALLOW" ]; then
  expect "ls is allowed" "200" "$(exec_argv "$ALLOW" ls / | status)"
  OUT="$(exec_argv "$ALLOW" python3 -c 'print(1)')"
  expect "python3 -c is held" "202" "$(printf '%s' "$OUT" | status)"
  expect "by the allowlist" "exec.allowlist" "$(printf '%s' "$OUT" | body | field intervention.gate)"
  ID="$(printf '%s' "$OUT" | body | field intervention_id)"
  [ -n "$ID" ] && call "$TOKEN" POST "/v1/interventions/$ID?action=reject" '{"reason":"e2e"}' >/dev/null
  OUT="$(call "$TOKEN" POST "/v1/environments/$ALLOW?action=exec" '{"shell":"ls"}')"
  expect "an opaque shell is held too" "202" "$(printf '%s' "$OUT" | status)"
  ID="$(printf '%s' "$OUT" | body | field intervention_id)"
  [ -n "$ID" ] && call "$TOKEN" POST "/v1/interventions/$ID?action=reject" '{"reason":"e2e"}' >/dev/null
fi

# ---------------------------------------------------------------------------
head_ "a deny gate on destroy returns the gate's reason and the env survives"
if [ -n "$NODESTROY" ]; then
  OUT="$(call "$TOKEN" DELETE "/v1/environments/$NODESTROY")"
  expect "403" "403" "$(printf '%s' "$OUT" | status)"
  expect "reason hitl_denied" "hitl_denied" "$(printf '%s' "$OUT" | body | field error.details.reason)"
  expect "the gate's reason" "e2e keeps this environment" "$(printf '%s' "$OUT" | body | field error.message)"
  expect "env still there" "200" "$(call "$TOKEN" GET "/v1/environments/$NODESTROY" | status)"
  OUT="$("$FUSE" environment destroy "$NODESTROY" --yes 2>&1)"
  case "$OUT" in
    *"denied by hitl gate no-destroy"*) ok "cli shows the deny" ;;
    *) bad "cli shows the deny"; note "$OUT" ;;
  esac
  note "$NODESTROY is reaped by its 10m max_runtime"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
for c in ${FAILED_CASES[@]+"${FAILED_CASES[@]}"}; do printf '  failed: %s\n' "$c"; done
[ "$FAIL" -eq 0 ]
