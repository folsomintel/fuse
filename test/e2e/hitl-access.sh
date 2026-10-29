#!/usr/bin/env bash
# end-to-end tests for hitl access (issue #310): scoped keys, actors, the
# audit log and webhooks, against a real orchestrator and host.
#
#   FUSE_URL=http://<orchestrator>:8080 FUSE_TOKEN=<master token> \
#     FUSE_BIN=/tmp/fuse-main ./test/e2e/hitl-access.sh
#
# the cli must already be connected (with --master) to the same orchestrator,
# with a host selected, the same as run.sh. agent and approver calls are
# plain curl so nothing here depends on an sdk build.
#
# the webhook case starts a receiver on this machine and needs the
# orchestrator to reach it:
#   HOOK_PORT  port the receiver listens on (default 18089)
#   HOOK_URL   url the orchestrator posts to (default http://127.0.0.1:$HOOK_PORT/),
#              so run on the orchestrator host or point this at this machine
#
# the audit tamper case needs database access and is skipped unless set:
#   PSQL_CMD   a command that runs one sql statement read from stdin against
#              the orchestrator database, e.g.
#              "ssh debian@host sudo -u postgres psql -tA fuse"
#
# every environment and key is created under E2E_PREFIX and cleaned up on exit.
set -uo pipefail

FUSE="${FUSE_BIN:-fuse}"
URL="${FUSE_URL:-}"
TOKEN="${FUSE_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
HOOK_PORT="${HOOK_PORT:-18089}"
HOOK_URL="${HOOK_URL:-http://127.0.0.1:$HOOK_PORT/}"
HOOK_SECRET="e2e-hook-secret-$$"
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
FF="$WORK/Fusefile"
sed "s|__HOOK_URL__|$HOOK_URL|" "$HERE/fusefiles/Fusefile.hitl-access" >"$FF"

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
KEYS=()
HOOK_PID=""
cleanup() {
  head_ "cleanup"
  for id in ${ENVS[@]+"${ENVS[@]}"}; do
    "$FUSE" environment destroy "$id" --yes >/dev/null 2>&1 && note "destroyed $id"
  done
  for k in ${KEYS[@]+"${KEYS[@]}"}; do
    "$FUSE" apikeys revoke "$k" --yes >/dev/null 2>&1
  done
  [ -n "$HOOK_PID" ] && kill "$HOOK_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# call <token> <actor or ""> <method> <path> [json-body] - prints the body,
# then the http status on the last line.
call() {
  local tok="$1" actor="$2" method="$3" path="$4" body="${5:-}"
  local args=(-sS -X "$method" "$URL$path" -H "Authorization: Bearer $tok" -w '\n%{http_code}')
  [ -n "$actor" ] && args+=(-H "Fuse-Actor: $actor")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  curl "${args[@]}"
}
status() { tail -n 1; }
body()   { sed '$d'; }

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

# mint <fuse apikeys/hitl args...> - mint a key with the cli, print "id key".
mint() {
  local out
  out="$("$FUSE" -o json "$@" 2>/dev/null)" || return 1
  local id key
  id="$(printf '%s' "$out" | field id)"
  key="$(printf '%s' "$out" | field key)"
  KEYS+=("$id")
  printf '%s %s' "$id" "$key"
}

# ask <token> <actor or ""> <env> <question> - raise an ask, print its id.
ask() {
  call "$1" "$2" POST "/v1/environments/$3?action=request" \
    "$(python3 -c 'import json,sys; print(json.dumps({"kind":"ask","question":sys.argv[1]}))' "$4")" | body | field id
}

up() {
  local task="$PREFIX-hitl-access-$1-$$"
  UP_ID="fuse-$task"
  ENVS+=("$UP_ID")
  if ! "$FUSE" up -f "$FF" --task-id "$task" --secret "HITL_HOOK_SECRET=$HOOK_SECRET" >"$WORK/$task.log" 2>&1; then
    bad "fuse up ($1), log: $WORK/$task.log"
    return 1
  fi
}

# the webhook receiver appends one json line per delivery: signature + body.
python3 - "$HOOK_PORT" "$WORK/hooks.jsonl" <<'PY' &
import http.server, json, sys
port, path = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0))).decode()
        with open(path, "a") as f:
            f.write(json.dumps({"sig": self.headers.get("Fuse-Signature", ""), "body": body}) + "\n")
        self.send_response(204); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", port), H).serve_forever()
PY
HOOK_PID=$!

# ---------------------------------------------------------------------------
head_ "setup"
read -r AGENT_ID AGENT < <(mint apikeys create --label "$PREFIX-agent-$$") || true
[ -n "${AGENT:-}" ] && ok "agent full-access key ($AGENT_ID)" || bad "agent full-access key"
ENV=""; ENV2=""
up a && ENV="$UP_ID"
up b && ENV2="$UP_ID"

# ---------------------------------------------------------------------------
head_ "the agent's full-access key cannot approve its own ask"
if [ -n "$ENV" ]; then
  ID="$(ask "$AGENT" "" "$ENV" "e2e: approve yourself?")"
  OUT="$(call "$AGENT" "" POST "/v1/interventions/$ID?action=approve" '{"answer":"yes"}')"
  expect "403" "403" "$(printf '%s' "$OUT" | status)"
  expect "reason missing_scope" "missing_scope" "$(printf '%s' "$OUT" | body | field error.details.reason)"
  ASK_A="$ID"
fi

# ---------------------------------------------------------------------------
head_ "an invite pinned to alice decides on its env and nowhere else"
if [ -n "$ENV" ] && [ -n "$ENV2" ]; then
  read -r INV_ID INVITE < <(mint hitl invite "$ENV" --scopes decide --ttl 30m --actor alice) || true
  OUT="$(call "$INVITE" "" POST "/v1/interventions/$ASK_A?action=approve" '{"answer":"yes"}')"
  expect "invite decides" "200" "$(printf '%s' "$OUT" | status)"
  expect "recorded as alice" "alice" "$(printf '%s' "$OUT" | body | field decision.decided_by.id)"
  expect "source pinned" "pinned" "$(printf '%s' "$OUT" | body | field decision.decided_by.source)"
  ID_B="$(ask "$AGENT" "" "$ENV2" "e2e: other env")"
  expect "404 on another env's intervention" "404" "$(call "$INVITE" "" GET "/v1/interventions/$ID_B" | status)"
  expect "404 on another env" "404" "$(call "$INVITE" "" GET "/v1/environments/$ENV2" | status)"
  expect "403 off the hitl routes" "403" "$(call "$INVITE" "" GET "/v1/environments/$ENV" | status)"
  expect "pinned key naming bob is 403" "403" "$(call "$INVITE" "bob" GET "/v1/interventions" | status)"
fi

# ---------------------------------------------------------------------------
head_ "act-as records the asserted actor; a key without it may not name one"
if [ -n "$ENV2" ]; then
  read -r ACTAS_ID ACTAS < <(mint apikeys create --label "$PREFIX-app-$$" --scopes decide,act-as --ttl 30m) || true
  OUT="$(call "$ACTAS" "carol" POST "/v1/interventions/$ID_B?action=reject" '{"reason":"e2e"}')"
  expect "act-as decides" "200" "$(printf '%s' "$OUT" | status)"
  expect "recorded as carol" "carol" "$(printf '%s' "$OUT" | body | field decision.decided_by.id)"
  expect "source asserted" "asserted" "$(printf '%s' "$OUT" | body | field decision.decided_by.source)"
  expect "asserting key recorded" "$ACTAS_ID" "$(printf '%s' "$OUT" | body | field decision.decided_by.key_id)"
  OUT="$(call "$AGENT" "carol" GET "/v1/interventions")"
  expect "agent key naming an actor is 403" "403" "$(printf '%s' "$OUT" | status)"
  expect "reason actor_not_allowed" "actor_not_allowed" "$(printf '%s' "$OUT" | body | field error.details.reason)"
fi

# ---------------------------------------------------------------------------
head_ "an actor cannot decide what it raised, even with another key"
if [ -n "$ENV2" ]; then
  ID="$(ask "$TOKEN" "dave" "$ENV2" "e2e: raised by dave")"
  OUT="$(call "$ACTAS" "dave" POST "/v1/interventions/$ID?action=approve" '{"answer":"yes"}')"
  expect "403" "403" "$(printf '%s' "$OUT" | status)"
  expect "reason self_approval" "self_approval" "$(printf '%s' "$OUT" | body | field error.details.reason)"
  call "$ACTAS" "erin" POST "/v1/interventions/$ID?action=reject" '{"reason":"e2e"}' >/dev/null
fi

# ---------------------------------------------------------------------------
head_ "an expired invite is rejected"
if [ -n "$ENV" ]; then
  read -r _ SHORT < <(mint hitl invite "$ENV" --scopes read --ttl 10s) || true
  expect "works before expiry" "200" "$(call "$SHORT" "" GET "/v1/interventions" | status)"
  sleep 12
  expect "401 after expiry" "401" "$(call "$SHORT" "" GET "/v1/interventions" | status)"
fi

# ---------------------------------------------------------------------------
head_ "webhooks are signed and carry no content"
sleep 3
if [ ! -s "$WORK/hooks.jsonl" ]; then
  bad "receiver got deliveries (is $HOOK_URL reachable from the orchestrator?)"
else
  python3 - "$WORK/hooks.jsonl" "$HOOK_SECRET" <<'PY' && ok "every delivery verifies and is content-free" || bad "every delivery verifies and is content-free"
import hashlib, hmac, json, sys
lines = [json.loads(l) for l in open(sys.argv[1])]
secret = sys.argv[2].encode()
allowed = {"event", "intervention_id", "env_id", "gate", "on", "deadline", "url"}
events = set()
for d in lines:
    parts = dict(p.split("=", 1) for p in d["sig"].split(","))
    want = hmac.new(secret, (parts["t"] + "." + d["body"]).encode(), hashlib.sha256).hexdigest()
    assert hmac.compare_digest(want, parts["v1"]), "bad signature"
    body = json.loads(d["body"])
    assert set(body) <= allowed, f"unexpected fields {set(body) - allowed}"
    assert "e2e:" not in d["body"], "payload leaked"
    events.add(body["event"])
print("       events seen: " + ", ".join(sorted(events)), file=sys.stderr)
assert {"raised", "decided", "invite_minted"} <= events, events
PY
fi

# ---------------------------------------------------------------------------
head_ "the audit log verifies, and a hand edit breaks it at that row"
"$FUSE" hitl audit --verify >/dev/null 2>&1 && ok "fuse hitl audit --verify passes" || bad "fuse hitl audit --verify passes"
if [ -z "${PSQL_CMD:-}" ]; then
  skip "set PSQL_CMD to run the tamper case"
else
  SEQ="$(printf "SELECT max(seq) - 1 FROM orchestrator_hitl_audit;\n" | sh -c "$PSQL_CMD" | tr -d '[:space:]')"
  ORIG="$(printf "SELECT actor_id FROM orchestrator_hitl_audit WHERE seq = %s;\n" "$SEQ" | sh -c "$PSQL_CMD" | tr -d '[:space:]')"
  printf "UPDATE orchestrator_hitl_audit SET actor_id = 'mallory' WHERE seq = %s;\n" "$SEQ" | sh -c "$PSQL_CMD" >/dev/null
  OUT="$("$FUSE" hitl audit --verify 2>&1)"
  case "$OUT" in
    *"seq $SEQ"*) ok "verify fails at seq $SEQ" ;;
    *) bad "verify fails at seq $SEQ"; note "$OUT" ;;
  esac
  printf "UPDATE orchestrator_hitl_audit SET actor_id = '%s' WHERE seq = %s;\n" "$ORIG" "$SEQ" | sh -c "$PSQL_CMD" >/dev/null
  "$FUSE" hitl audit --verify >/dev/null 2>&1 && ok "restored row verifies again" || bad "restored row verifies again"
fi

# ---------------------------------------------------------------------------
head_ "destroying an env revokes its invites"
if [ -n "$ENV" ]; then
  expect "invite works while the env lives" "200" "$(call "$INVITE" "" GET "/v1/interventions" | status)"
  "$FUSE" environment destroy "$ENV" --yes >/dev/null 2>&1
  expect "401 once the env is gone" "401" "$(call "$INVITE" "" GET "/v1/interventions" | status)"
fi

printf '\n%d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
for c in ${FAILED_CASES[@]+"${FAILED_CASES[@]}"}; do printf '  failed: %s\n' "$c"; done
[ "$FAIL" -eq 0 ]
