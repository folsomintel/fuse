#!/usr/bin/env bash
# end-to-end tests for hitl secrets, terminal takeover and signals (issue
# #312) against a real orchestrator and host.
#
#   FUSE_URL=http://<orchestrator>:8080 FUSE_TOKEN=<master token> \
#     FUSE_BIN=/tmp/fuse-main ./test/e2e/hitl-takeover.sh
#
# the cli must already be connected (with --master) to the same orchestrator,
# with a host selected, the same as run.sh. the agent side is plain curl so
# nothing here depends on an sdk build. the orchestrator needs
# TOKEN_ENCRYPTION_KEY, since secret requests are refused without it.
#
# two cases need access beyond the api and are skipped unless enabled:
#   PSQL_CMD   runs one sql statement read from stdin against the orchestrator
#              database, e.g. "ssh debian@host sudo -u postgres psql -tA fuse"
#   LOGS_CMD   prints the orchestrator's recent logs, e.g.
#              "ssh debian@host sudo journalctl -u fuse-orchestrator --since -15min"
#
# every environment is created under E2E_PREFIX and destroyed on exit.
set -uo pipefail

FUSE="${FUSE_BIN:-fuse}"
URL="${FUSE_URL:-}"
TOKEN="${FUSE_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
HERE="$(cd "$(dirname "$0")" && pwd)"
FF="$HERE/fusefiles/Fusefile.hitl-takeover"
FF_EXPIRY="$HERE/fusefiles/Fusefile.hitl-expiry"
SECRET_VALUE="e2e-secret-$$-$(date +%s)"

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
SSE_PID=""
cleanup() {
  head_ "cleanup"
  [ -n "$SSE_PID" ] && kill "$SSE_PID" 2>/dev/null
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

# code <method> <path> [json-body] prints only the http status.
code() {
  local method="$1" path="$2" body="${3:-}"
  curl -sS -o /dev/null -w '%{http_code}' -X "$method" "$URL$path" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" ${body:+-d "$body"}
}

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

absent() { # absent <name> <haystack>: the secret value must not be in it
  case "$2" in
    *"$SECRET_VALUE"*) bad "secret absent from $1" ;;
    *) ok "secret absent from $1" ;;
  esac
}

up() { # up <case> <fusefile>: id lands in UP_ID
  local task="$PREFIX-hitl-$1-$$"
  UP_ID="fuse-$task"
  ENVS+=("$UP_ID")
  if ! "$FUSE" up -f "$2" --task-id "$task" >"/tmp/$task.log" 2>&1; then
    bad "fuse up ($1), log: /tmp/$task.log"
    return 1
  fi
}

request() { # request <env> <json> prints the intervention id
  api POST "/v1/environments/$1?action=request" "$2" | field id
}

wait_for() { # wait_for <id> <max seconds> prints the final json
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

exec_json() { # exec_json <env> <shell> prints the exec response body
  api POST "/v1/environments/$1?action=exec" "$(python3 -c 'import json,sys; print(json.dumps({"shell": sys.argv[1]}))' "$2")"
}

# agent_exec <env> <shell> behaves like an sdk exec: on 423 it waits on the
# lock's intervention and retries. prints the final response body.
agent_exec() {
  local out status id
  for _ in $(seq 1 20); do
    out="$(curl -sS -w '\n%{http_code}' -X POST "$URL/v1/environments/$1?action=exec" \
      -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
      -d "$(python3 -c 'import json,sys; print(json.dumps({"shell": sys.argv[1]}))' "$2")")"
    status="${out##*$'\n'}"
    out="${out%$'\n'*}"
    if [ "$status" != "423" ]; then printf '%s' "$out"; return 0; fi
    id="$(printf '%s' "$out" | field error.details.intervention_id)"
    wait_for "$id" 300 >/dev/null
  done
}

# ---------------------------------------------------------------------------
head_ "a secret lands in the guest and nowhere else"
ENV=""
if up secret "$FF"; then
  ENV="$UP_ID"
  # watch the sse stream for the whole case; the value must never appear.
  curl -sS -N "$URL/v1/environments/$ENV/events" -H "Authorization: Bearer $TOKEN" >"/tmp/$ENV.sse" 2>/dev/null &
  SSE_PID=$!

  CODE="$(code POST "/v1/environments/$ENV?action=request" '{"kind":"secret","target":"AWS_SECRET_ACCESS_KEY"}')"
  expect "undeclared target is refused" "400" "$CODE"

  ID="$(request "$ENV" '{"kind":"secret","target":"E2E_TOKEN","message":"please paste the deploy token"}')"
  wait_for "$ID" 120 >"/tmp/$ID.json" &
  WAITER=$!
  sleep 2
  SHOW="$("$FUSE" hitl show "$ID" 2>&1)"
  case "$SHOW" in *"e2e token for the deploy bot"*) ok "human view shows the fusefile label" ;; *) bad "human view shows the fusefile label"; note "$SHOW" ;; esac
  printf '%s\n' "$SECRET_VALUE" | "$FUSE" hitl secret "$ID" >"/tmp/$ID.cli" 2>&1 && ok "fuse hitl secret" || { bad "fuse hitl secret"; note "$(cat "/tmp/$ID.cli")"; }
  wait "$WAITER"
  expect "agent gets done" "done" "$(field decision.answer <"/tmp/$ID.json")"
  absent "cli output" "$(cat "/tmp/$ID.cli")"
  expect "later exec sees the env var" "$SECRET_VALUE" "$(exec_json "$ENV" 'printf %s "$E2E_TOKEN"' | field stdout)"

  FID="$(request "$ENV" '{"kind":"secret","target":"/root/.e2e-netrc"}')"
  printf '%s\n' "$SECRET_VALUE" | "$FUSE" hitl secret "$FID" >/dev/null 2>&1
  expect "file target holds the value" "$SECRET_VALUE" "$(exec_json "$ENV" 'cat /root/.e2e-netrc' | field stdout)"
  expect "file target is 0600" "600" "$(exec_json "$ENV" 'stat -c %a /root/.e2e-netrc' | field stdout | tr -d '\n')"

  absent "the intervention row (api)" "$(api GET "/v1/interventions/$ID")"
  absent "the audit log" "$(api GET "/v1/hitl/audit?env=$ENV")"
  case "$(api GET "/v1/hitl/audit?env=$ENV")" in
    *secret_answered*) ok "audit records secret_answered" ;; *) bad "audit records secret_answered" ;;
  esac
  sleep 1
  kill "$SSE_PID" 2>/dev/null; SSE_PID=""
  absent "the sse stream" "$(cat "/tmp/$ENV.sse")"
  if [ -n "${PSQL_CMD:-}" ]; then
    ROW="$(printf "SELECT encode(payload,'escape') || encode(coalesce(result,''::bytea),'escape') FROM orchestrator_interventions WHERE intervention_id IN ('%s','%s');\n" "$ID" "$FID" | sh -c "$PSQL_CMD")"
    absent "postgres" "$ROW"
  else
    skip "set PSQL_CMD to check postgres"
  fi
  if [ -n "${LOGS_CMD:-}" ]; then
    absent "orchestrator logs" "$(sh -c "$LOGS_CMD" 2>&1)"
  else
    skip "set LOGS_CMD to check the orchestrator logs"
  fi
  rm -f "/tmp/$ID.json" "/tmp/$ID.cli" "/tmp/$ENV.sse"
fi

# ---------------------------------------------------------------------------
head_ "takeover locks agent exec until release, and the note reaches the agent"
if [ -n "$ENV" ]; then
  TID="$(request "$ENV" '{"kind":"takeover","via":"terminal","reason":"e2e: please log in for me"}')"
  wait_for "$TID" 300 >"/tmp/$TID.json" &
  REQUESTER=$!
  "$FUSE" hitl takeover "$ENV" --no-shell >/dev/null 2>&1 && ok "fuse hitl takeover claims the request" || bad "fuse hitl takeover claims the request"
  expect "agent exec gets 423" "423" "$(code POST "/v1/environments/$ENV?action=exec" '{"shell":"true"}')"

  agent_exec "$ENV" 'echo resumed' >"/tmp/$TID.exec" &
  AGENT=$!
  sleep 3
  kill -0 "$AGENT" 2>/dev/null && ok "agent exec is waiting" || bad "agent exec is waiting"

  # a recorded shell session: drive the cli in a pty.
  python3 - "$FUSE" "$ENV" <<'PY' >"/tmp/$TID.shell" 2>&1
import os, pty, sys, time, select
fuse, env = sys.argv[1], sys.argv[2]
pid, fd = pty.fork()
if pid == 0:
    # the guest computes 42, so the marker only shows up as command output,
    # not as the echoed keystrokes.
    os.execvp(fuse, [fuse, "hitl", "takeover", env])
out = b""
deadline = time.time() + 60
sent = False
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 1)
    if r:
        try:
            chunk = os.read(fd, 4096)
        except OSError:
            break
        if not chunk:
            break
        out += chunk
    if not sent and b"recorded" in out:
        time.sleep(1)
        os.write(fd, b"echo e2e-shell-marker-$((40+2))\nexit\n")
        sent = True
sys.stdout.write(out.decode(errors="replace"))
PY
  SHELL_OUT="$(cat "/tmp/$TID.shell")"
  case "$SHELL_OUT" in *"this session is recorded"*) ok "shell opens with the recording banner" ;; *) bad "shell opens with the recording banner"; note "$SHELL_OUT" ;; esac
  case "$SHELL_OUT" in *"e2e-shell-marker-42"*) ok "shell runs commands" ;; *) bad "shell runs commands" ;; esac

  "$FUSE" hitl release "$ENV" --note "e2e: logged in" >/dev/null 2>&1 && ok "fuse hitl release" || bad "fuse hitl release"
  wait "$REQUESTER"
  expect "agent gets the note" "e2e: logged in" "$(field decision.answer <"/tmp/$TID.json")"
  wait "$AGENT"
  expect "waiting exec completes after release" "resumed" "$(field stdout <"/tmp/$TID.exec" | tr -d '\n')"

  REC="$(api GET "/v1/hitl/audit?env=$ENV" | python3 -c '
import json, sys
rows = [e for e in json.load(sys.stdin)["entries"] if e["event"] == "takeover_session"]
print(rows[-1].get("recording_id", "") if rows else "")')"
  if [ -z "$REC" ]; then
    bad "takeover_session audit row links a recording"
  else
    ok "takeover_session audit row links a recording ($REC)"
    CAST="$(api GET "/v1/hitl/recordings/$REC")"
    expect "cast is asciinema v2" "2" "$(printf '%s' "$CAST" | head -n 1 | field version)"
    case "$CAST" in *e2e-shell-marker-42*) ok "cast replays the session" ;; *) bad "cast replays the session" ;; esac
    if [ -n "${PSQL_CMD:-}" ]; then
      RAW="$(printf "SELECT sealed, encode(data,'escape') FROM orchestrator_hitl_recordings WHERE id = '%s';\n" "$REC" | sh -c "$PSQL_CMD")"
      case "$RAW" in t\|*) ok "cast is sealed in postgres" ;; *) bad "cast is sealed in postgres" ;; esac
      case "$RAW" in *e2e-shell-marker*) bad "cast is not readable without the orchestrator key" ;; *) ok "cast is not readable without the orchestrator key" ;; esac
    else
      skip "set PSQL_CMD to check the cast is sealed"
    fi
  fi
  rm -f /tmp/"$TID".*
fi

# ---------------------------------------------------------------------------
head_ "three failing execs raise a stall that pauses the agent until acked"
if [ -n "$ENV" ]; then
  for _ in 1 2 3; do exec_json "$ENV" 'exit 1' >/dev/null; done
  SID="$(api GET "/v1/interventions?env=$ENV&state=pending" | python3 -c '
import json, sys
s = [i for i in json.load(sys.stdin)["interventions"] if i["kind"] == "stall"]
print(s[0]["id"] if s else "")')"
  if [ -z "$SID" ]; then
    bad "stall raised after 3 failures"
  else
    ok "stall raised after 3 failures ($SID)"
    expect "agent exec is paused" "423" "$(code POST "/v1/environments/$ENV?action=exec" '{"shell":"true"}')"
    expect "a human acks it" "200" "$(code POST "/v1/interventions/$SID?action=approve" '{"reason":"e2e: looked, carry on"}')"
    expect "agent runs again" "200" "$(code POST "/v1/environments/$ENV?action=exec" '{"shell":"true"}')"
  fi
fi

# ---------------------------------------------------------------------------
head_ "an approved expiry extends the env"
if up extend "$FF_EXPIRY"; then
  ENV2="$UP_ID"
  sleep 35 # one reconcile tick raises the expiry
  EID="$(api GET "/v1/interventions?env=$ENV2&state=pending" | python3 -c '
import json, sys
s = [i for i in json.load(sys.stdin)["interventions"] if i["kind"] == "expiry"]
print(s[0]["id"] if s else "")')"
  if [ -z "$EID" ]; then
    bad "expiry raised ahead of the idle reap"
  else
    ok "expiry raised ahead of the idle reap ($EID)"
    expect "approve with a 5m extension" "200" "$(code POST "/v1/interventions/$EID?action=approve" '{"extend_seconds":300}')"
    note "waiting 150s past the 1m idle timeout"
    sleep 150
    expect "env still running" "running" "$("$FUSE" -o json environment get "$ENV2" 2>/dev/null | field state)"
  fi
fi

# ---------------------------------------------------------------------------
head_ "an unanswered expiry lets the reap go ahead on schedule"
if up reap "$FF_EXPIRY"; then
  ENV3="$UP_ID"
  note "waiting 180s with nobody answering"
  sleep 180
  STATE="$("$FUSE" -o json environment get "$ENV3" 2>/dev/null | field state)"
  case "$STATE" in
    ""|destroying|destroyed) ok "env reaped ($STATE)" ;;
    *) bad "env reaped"; note "state: $STATE" ;;
  esac
  EXP="$(api GET "/v1/interventions?env=$ENV3&state=expired" | python3 -c '
import json, sys
print(sum(1 for i in json.load(sys.stdin)["interventions"] if i["kind"] == "expiry"))')"
  expect "its expiry expired" "1" "$EXP"
fi

printf '\n%d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
for c in ${FAILED_CASES[@]+"${FAILED_CASES[@]}"}; do printf '  failed: %s\n' "$c"; done
[ "$FAIL" -eq 0 ]
