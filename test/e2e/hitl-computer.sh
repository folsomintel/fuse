#!/usr/bin/env bash
# end-to-end tests for hitl computer gates, deciders, review requests and
# desktop takeover (issue #313) against a real orchestrator and host with
# the desktop image baked.
#
#   FUSE_URL=http://<orchestrator>:8080 FUSE_TOKEN=<master token> \
#     FUSE_BIN=/tmp/fuse-main ./test/e2e/hitl-computer.sh
#
# the cli must already be connected (with --master) to the same orchestrator,
# with a host selected, the same as run.sh. the agent side is plain curl so
# nothing here depends on an sdk build.
#
# the script runs a decider on 127.0.0.1:$DECIDER_PORT (default 18913), so
# run it on the orchestrator's host, or set DECIDER_URL to where the
# orchestrator reaches it (the script still listens on $DECIDER_PORT).
#
# every environment is created under E2E_PREFIX and destroyed on exit.
set -uo pipefail

FUSE="${FUSE_BIN:-fuse}"
URL="${FUSE_URL:-}"
TOKEN="${FUSE_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
HERE="$(cd "$(dirname "$0")" && pwd)"
DECIDER_PORT="${DECIDER_PORT:-18913}"
DECIDER_URL="${DECIDER_URL:-http://127.0.0.1:$DECIDER_PORT}"
DECIDE_KEY="e2e-decide-$$-$(date +%s)"
WORK="$(mktemp -d)"
FF="$WORK/Fusefile"
sed -e "s|__DECIDER_URL__|$DECIDER_URL/decide|" -e "s|__SLOW_URL__|$DECIDER_URL/slow|" \
  "$HERE/fusefiles/Fusefile.hitl-computer" >"$FF"

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
DECIDER_PID=""
cleanup() {
  head_ "cleanup"
  [ -n "$DECIDER_PID" ] && kill "$DECIDER_PID" 2>/dev/null
  for id in ${ENVS[@]+"${ENVS[@]}"}; do
    "$FUSE" environment destroy "$id" --yes >/dev/null 2>&1 && note "destroyed $id"
  done
  rm -rf "$WORK"
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

# call <method> <path> [json-body] prints the body, then the status on its
# own last line.
call() {
  local method="$1" path="$2" body="${3:-}"
  curl -sS -w '\n%{http_code}' -X "$method" "$URL$path" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" ${body:+-d "$body"}
}
status() { tail -n 1; }
body()   { sed '$d'; }

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

wait_for() { # wait_for <id> <max seconds> prints the json once settled
  local id="$1" until=$(( $(date +%s) + $2 )) out state
  while [ "$(date +%s)" -lt "$until" ]; do
    out="$(api GET "/v1/interventions/$id?wait=20s" 2>/dev/null)"
    state="$(printf '%s' "$out" | field state 2>/dev/null)"
    if [ -n "$state" ] && [ "$state" != "pending" ]; then
      # an approved gated action is settled once its result is in.
      if [ "$state" != "approved" ] || [ -n "$(printf '%s' "$out" | field result)" ] ||
        [ "$(printf '%s' "$out" | field decision.decided_by.source)" = "decider" ]; then
        printf '%s' "$out"
        return 0
      fi
    fi
    [ -z "$state" ] && sleep 2
  done
  printf '%s' "$out"
}

exec_json() { # exec_json <env> <shell> prints the exec response body
  api POST "/v1/environments/$1?action=exec" "$(python3 -c 'import json,sys; print(json.dumps({"shell": sys.argv[1]}))' "$2")"
}

computer() { # computer <env> <json> prints body + status
  call POST "/v1/environments/$1/computer" "$2"
}

# ---------------------------------------------------------------------------
# the decider: /decide answers by the key combo, /slow never answers in
# time. every call is checked against its signature and logged.
python3 - "$DECIDER_PORT" "$DECIDE_KEY" "$WORK/decider.jsonl" <<'PY' &
import hashlib, hmac, json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port, key, log = int(sys.argv[1]), sys.argv[2].encode(), sys.argv[3]
verdicts = {"ctrl+a": ("allow", "harmless"), "ctrl+w": ("deny", "e2e: closing windows is not allowed")}
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        sig = dict(p.split("=", 1) for p in self.headers.get("Fuse-Signature", "").split(",") if "=" in p)
        want = hmac.new(key, sig.get("t", "").encode() + b"." + raw, hashlib.sha256).hexdigest()
        body = json.loads(raw)
        with open(log, "a") as f:
            f.write(json.dumps({"path": self.path, "signed": hmac.compare_digest(want, sig.get("v1", "")),
                                "text": (body.get("computer") or {}).get("text"),
                                "screenshot": len(body.get("screenshot") or "")}) + "\n")
        if self.path == "/slow":
            time.sleep(15)
        decision, reason = verdicts.get((body.get("computer") or {}).get("text"), ("ask", "e2e: a human should look"))
        out = json.dumps({"decision": decision, "reason": reason}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(out)
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
PY
DECIDER_PID=$!

ENV=""
TASK="$PREFIX-hitl-computer-$$"
ENV="fuse-$TASK"
ENVS+=("$ENV")
head_ "up a desktop environment with computer gates"
if ! "$FUSE" up -f "$FF" --task-id "$TASK" --secret "DECIDE_KEY=$DECIDE_KEY" >"$WORK/up.log" 2>&1; then
  bad "fuse up, log:"; note "$(cat "$WORK/up.log")"
  ENV=""
else
  ok "fuse up $ENV"
  # the display takes a moment after boot.
  for _ in $(seq 1 30); do
    [ "$(api GET "/v1/environments/$ENV/computer" | field up)" = "True" ] && break
    sleep 2
  done
fi

# ---------------------------------------------------------------------------
head_ "a decider allows one action, denies another and escalates a third"
if [ -n "$ENV" ]; then
  OUT="$(computer "$ENV" '{"action":"key","text":"ctrl+a"}')"
  expect "allowed key runs at once" "200" "$(printf '%s' "$OUT" | status)"
  [ -n "$(printf '%s' "$OUT" | body | field screenshot)" ] && ok "allowed key returns a screenshot" || bad "allowed key returns a screenshot"

  OUT="$(computer "$ENV" '{"action":"key","text":"ctrl+w"}')"
  expect "denied key is refused" "403" "$(printf '%s' "$OUT" | status)"
  expect "deny carries the decider's reason" "e2e: closing windows is not allowed" "$(printf '%s' "$OUT" | body | field error.message)"

  OUT="$(computer "$ENV" '{"action":"key","text":"F5"}')"
  expect "escalated key is held" "202" "$(printf '%s' "$OUT" | status)"
  EID="$(printf '%s' "$OUT" | body | field intervention_id)"
  expect "held key carries the decider's note" "e2e: a human should look" "$(printf '%s' "$OUT" | body | field intervention.payload.decider.reason)"
  api POST "/v1/interventions/$EID?action=reject" '{"reason":"e2e: not now"}' >/dev/null

  START=$(date +%s)
  OUT="$(computer "$ENV" '{"action":"right_click","coordinate":[640,400]}')"
  ELAPSED=$(( $(date +%s) - START ))
  expect "a decider that times out falls through to ask" "202" "$(printf '%s' "$OUT" | status)"
  [ -n "$(printf '%s' "$OUT" | body | field intervention.payload.decider.error)" ] && ok "the timeout is noted on the intervention" || bad "the timeout is noted on the intervention"
  [ "$ELAPSED" -lt 14 ] && ok "the slow decider cost ${ELAPSED}s, not its 15s" || bad "the slow decider cost ${ELAPSED}s"
  SID="$(printf '%s' "$OUT" | body | field intervention_id)"
  api POST "/v1/interventions/$SID?action=reject" '{"reason":"e2e"}' >/dev/null

  python3 - "$WORK/decider.jsonl" <<'PY' && ok "every decider call is signed and carries a screenshot" || bad "every decider call is signed and carries a screenshot"
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
assert len(rows) >= 4, rows
assert all(r["signed"] and r["screenshot"] > 0 for r in rows), rows
PY

  ROWS="$(api GET "/v1/interventions?env=$ENV&state=all" | python3 -c '
import json, sys
ins = json.load(sys.stdin)["interventions"]
print(sum(1 for i in ins if (i.get("decision") or {}).get("decided_by", {}).get("source") == "decider"))')"
  expect "the allow and the deny are recorded with source: decider" "2" "$ROWS"
fi

# ---------------------------------------------------------------------------
head_ "a gated type is approved with edited text, and the edit is what gets typed"
if [ -n "$ENV" ]; then
  # an xterm that writes its first line to a file, so what was typed can be
  # read back from the guest.
  exec_json "$ENV" 'rm -f /tmp/typed; DISPLAY=:1 setsid xterm -geometry 60x8+20+20 -e sh -c "read l; printf %s \"\$l\" > /tmp/typed" >/dev/null 2>&1 < /dev/null &' >/dev/null
  sleep 4
  OUT="$(computer "$ENV" '{"action":"left_click","coordinate":[120,60]}')"
  expect "an ungated click passes" "200" "$(printf '%s' "$OUT" | status)"

  computer "$ENV" '{"action":"type","text":"e2e-original\n"}' >"$WORK/type.out" &
  AGENT=$!
  sleep 2
  TID="$(api GET "/v1/interventions?env=$ENV&state=pending" | python3 -c '
import json, sys
s = [i for i in json.load(sys.stdin)["interventions"] if i["kind"] == "computer" and i["gate"] == "typing"]
print(s[0]["id"] if s else "")')"
  if [ -z "$TID" ]; then
    bad "type is held by the typing gate"
  else
    ok "type is held by the typing gate ($TID)"
    "$FUSE" hitl approve "$TID" --text $'e2e-edited\n' >/dev/null 2>&1 && ok "fuse hitl approve --text" || bad "fuse hitl approve --text"
    wait "$AGENT"
    expect "the agent's call got 202" "202" "$(status <"$WORK/type.out")"
    SETTLED="$(wait_for "$TID" 60)"
    [ -n "$(printf '%s' "$SETTLED" | field result.computer)" ] && ok "the result is on the intervention" || { bad "the result is on the intervention"; note "$SETTLED"; }
    expect "the decision records the edit" $'e2e-edited\n' "$(printf '%s' "$SETTLED" | field decision.edited_text)"
    sleep 1
    expect "the guest got the edited text" "e2e-edited" "$(exec_json "$ENV" 'cat /tmp/typed 2>/dev/null' | field stdout)"
  fi
fi

# ---------------------------------------------------------------------------
head_ "a review of a diff is rejected with comments that reach the agent"
if [ -n "$ENV" ]; then
  DIFF=$'diff --git a/app.py b/app.py\n--- a/app.py\n+++ b/app.py\n@@ -1 +1 @@\n-check_auth()\n+pass\n'
  RID="$(api POST "/v1/environments/$ENV?action=request" "$(python3 -c 'import json,sys; print(json.dumps({"kind":"review","content":sys.argv[1],"message":"e2e: dropping the auth check"}))' "$DIFF")" | field id)"
  wait_for "$RID" 120 >"$WORK/review.json" &
  WAITER=$!
  sleep 1
  SHOW="$("$FUSE" hitl show "$RID" 2>&1)"
  case "$SHOW" in *"+pass"*) ok "fuse hitl show prints the diff" ;; *) bad "fuse hitl show prints the diff"; note "$SHOW" ;; esac
  "$FUSE" hitl reject "$RID" --reason "e2e: keep check_auth" >/dev/null 2>&1 && ok "fuse hitl reject" || bad "fuse hitl reject"
  wait "$WAITER"
  expect "the agent gets the comments" "e2e: keep check_auth" "$(field decision.reason <"$WORK/review.json")"
  expect "the format was detected" "diff" "$(field payload.review.format <"$WORK/review.json")"

  exec_json "$ENV" 'printf "line one\n" > /tmp/review.txt' >/dev/null
  PID_="$(api POST "/v1/environments/$ENV?action=request" '{"kind":"review","path":"/tmp/review.txt"}')"
  expect "a path review reads the file" "line one" "$(printf '%s' "$PID_" | field payload.review.content | tr -d '\n')"
  api POST "/v1/interventions/$(printf '%s' "$PID_" | field id)?action=approve" '{"reason":"e2e: fine"}' >/dev/null
fi

# ---------------------------------------------------------------------------
head_ "during a desktop takeover agent clicks get 423, and the recording is linked"
if [ -n "$ENV" ]; then
  OUT="$(call POST "/v1/environments/$ENV?action=takeover" '{"via":"desktop","reason":"e2e: solve the captcha"}')"
  expect "desktop takeover starts" "200" "$(printf '%s' "$OUT" | status)"
  HID="$(printf '%s' "$OUT" | body | field id)"
  expect "agent click gets 423" "423" "$(computer "$ENV" '{"action":"left_click","coordinate":[10,10]}' | status)"
  expect "the plain desktop stream gets 423" "423" "$(curl -sS -o /dev/null -w '%{http_code}' "$URL/v1/environments/$ENV/computer/stream" \
    -H "Authorization: Bearer $TOKEN" -H "Upgrade: fuse-vnc/1" -H "Connection: Upgrade")"

  # the human's view: speak just enough rfb to click, type and hold the
  # session open long enough for two screenshots.
  python3 - "$URL" "$TOKEN" "$HID" <<'PY' >"$WORK/vnc.log" 2>&1 && ok "the holder drives the desktop over rfb" || { bad "the holder drives the desktop over rfb"; note "$(cat "$WORK/vnc.log")"; }
import socket, struct, sys, time
from urllib.parse import urlparse
u = urlparse(sys.argv[1]); token, hid = sys.argv[2], sys.argv[3]
s = socket.create_connection((u.hostname, u.port or 80), timeout=20)
s.sendall((f"GET /v1/interventions/{hid}/desktop HTTP/1.1\r\nHost: {u.netloc}\r\n"
           f"Authorization: Bearer {token}\r\nUpgrade: fuse-vnc/1\r\nConnection: Upgrade\r\n\r\n").encode())
buf = b""
while b"\r\n\r\n" not in buf:
    buf += s.recv(1)
assert b" 101 " in buf.split(b"\r\n")[0], buf
def recv(n):
    out = b""
    while len(out) < n:
        c = s.recv(n - len(out))
        assert c, "stream closed"
        out += c
    return out
recv(12)
s.sendall(b"RFB 003.008\n")
types = recv(recv(1)[0])
assert 1 in types, f"server wants auth: {list(types)}"
s.sendall(b"\x01")
assert struct.unpack(">I", recv(4))[0] == 0, "security failed"
s.sendall(b"\x01")
recv(20); recv(struct.unpack(">I", recv(4))[0])
s.sendall(struct.pack(">BBHH", 5, 1, 300, 200) + struct.pack(">BBHH", 5, 0, 300, 200))
for c in b"hunter2":
    s.sendall(struct.pack(">BBxxI", 4, 1, c) + struct.pack(">BBxxI", 4, 0, c))
s.sendall(struct.pack(">BBxxI", 4, 1, 0xff0d) + struct.pack(">BBxxI", 4, 0, 0xff0d))
time.sleep(13)
s.close()
PY
  sleep 2
  expect "fuse hitl release" "200" "$(call POST "/v1/environments/$ENV?action=release" '{"note":"e2e: done"}' | status)"
  expect "agent clicks again after release" "200" "$(computer "$ENV" '{"action":"left_click","coordinate":[10,10]}' | status)"

  REC="$(api GET "/v1/hitl/audit?env=$ENV" | python3 -c '
import json, sys
rows = [e for e in json.load(sys.stdin)["entries"] if e["event"] == "takeover_session"]
print(rows[-1].get("recording_id", "") if rows else "")')"
  if [ -z "$REC" ]; then
    bad "takeover_session audit row links a recording"
  else
    ok "takeover_session audit row links a recording ($REC)"
    api GET "/v1/hitl/recordings/$REC" >"$WORK/rec.json"
    python3 - "$WORK/rec.json" <<'PY' && ok "the recording holds the clicks, keys and screenshots, not the typed text" || { bad "the recording holds the clicks, keys and screenshots, not the typed text"; note "$(head -c 400 "$WORK/rec.json")"; }
import json, sys
raw = open(sys.argv[1]).read()
r = json.loads(raw)
assert r["kind"] == "desktop", r["kind"]
types = [e["type"] for e in r["events"]]
assert "pointer" in types and "text" in types and any(e.get("key") == "Return" for e in r["events"]), types
assert sum(e.get("chars", 0) for e in r["events"]) == 7, r["events"]
assert len(r["screenshots"]) >= 2, len(r["screenshots"])
assert "hunter2" not in raw
PY
  fi
fi

printf '\n%d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
for c in ${FAILED_CASES[@]+"${FAILED_CASES[@]}"}; do printf '  failed: %s\n' "$c"; done
[ "$FAIL" -eq 0 ]
