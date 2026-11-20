#!/usr/bin/env bash
# agent-level e2e: vms resumed from one live snapshot do not share identity.
#
# like live-snapshot-agent.sh this talks to fc-agent directly, not through
# the orchestrator, and needs a host on the 6.1 guest kernel (fc-install.sh's
# default). 5.10 only reseeds from vmgenid in kernels from 5.18 on, so a host
# still on the 5.10 vmlinux.bin can fail the urandom case.
#
#   FC_AGENT_URL=http://<host>:9000 FC_AGENT_TOKEN=<token> ./test/e2e/resume-unique-agent.sh
#
# each vm has its own network namespace with the same guest network inside
# (#419), so both clones run at once, each in its own slot, and both carry the
# source's mac and ip inside their namespace. the source is still destroyed
# after the snapshot: a resume needs the drive path it recorded to be free.
#
# fused runs in the source with a token file, so each clone wakes up serving
# the source's token; the case then writes the clone's own token and signals
# fused the way token rotation does.
set -uo pipefail

FC_URL="${FC_AGENT_URL:-}"
FC_TOKEN="${FC_AGENT_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
SRC="${PREFIX}-uniq-src-$$"
# how long the seed sits before the first resume. without it the clones wake
# up seconds after the snapshot and the clock check cannot fail.
AGE="${E2E_SNAPSHOT_AGE:-90}"
VMS=()
SNAP=""

# keep in step with fusedReloadCommand in internal/orchestrator/agent_profile.go.
RELOAD='rm -f /run/fused-reload
pid=$(systemctl show -p MainPID --value fused) && [ "${pid:-0}" -gt 0 ] && kill -HUP "$pid" || exit 1
i=0
while [ ! -e /run/fused-reload ] && [ "$i" -lt 20 ]; do sleep 0.1; i=$((i + 1)); done
if [ -e /run/fused-reload ]; then
  grep -qx ok /run/fused-reload && exit 0
  cat /run/fused-reload >&2
  exit 1
fi
if systemctl is-active -q fused; then echo "fused did not report the reload" >&2; exit 1; fi
systemctl restart fused && sleep 0.5 && systemctl is-active -q fused'

PASS=0; FAIL=0
FAILED_CASES=()

ok()    { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()   { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); FAILED_CASES+=("$1"); }
note()  { printf '       %s\n' "$1"; }
head_() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

if [ -z "$FC_URL" ] || [ -z "$FC_TOKEN" ]; then
  echo "FC_AGENT_URL and FC_AGENT_TOKEN must both be set" >&2
  exit 2
fi

api() { # api <method> <path> [json-body]
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -X "$method" "$FC_URL$path" \
      -H "Authorization: Bearer $FC_TOKEN" \
      -H "Content-Type: application/json" -d "$body"
  else
    curl -sS -X "$method" "$FC_URL$path" -H "Authorization: Bearer $FC_TOKEN"
  fi
}

jqr() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"; }

# guest_sh <vm> <shell line>: runs it in the guest and echoes decoded stdout.
guest_sh() {
  api POST "/v1/vm/$1/exec" \
    "$(python3 -c 'import json,sys; print(json.dumps({"cmd":["sh","-c",sys.argv[1]],"timeout_ms":30000}))' "$2")" \
    | python3 -c 'import base64,json,sys; d=json.load(sys.stdin); sys.stdout.write(base64.b64decode(d.get("stdout","")).decode(errors="replace"))'
}

# fused_code <agent url> <token>: the status /v1/info answers with that token.
fused_code() {
  curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$1/v1/info" -H "Authorization: Bearer $2"
}

cleanup() {
  head_ "cleanup"
  for v in "$SRC" "${VMS[@]}"; do
    [ -n "$v" ] && api DELETE "/v1/vm/$v" >/dev/null 2>&1 && note "destroyed $v"
  done
  [ -n "$SNAP" ] && api DELETE "/v1/snapshots/$SNAP" >/dev/null 2>&1 && note "deleted $SNAP"
}
trap cleanup EXIT INT TERM

head_ "source $SRC with fused on a token file"
CREATE=$(api POST /v1/vm "{\"name\":\"$SRC\",\"cpus\":1,\"memory_mb\":512}")
SRC_URL=$(echo "$CREATE" | jqr url)
if [ -z "$(echo "$CREATE" | jqr vm_id)" ]; then
  echo "create failed: $CREATE" >&2
  exit 1
fi
guest_sh "$SRC" 'mkdir -p /fuse && echo "{}" > /fuse/manifest.json && echo "{}" > /fuse/secrets.json && echo source-token > /fuse/auth-token' >/dev/null
api POST "/v1/vm/$SRC/start-agent" \
  '{"manifest_path":"/fuse/manifest.json","secrets_path":"/fuse/secrets.json","auth_token":"source-token"}' >/dev/null
sleep 1
[ "$(fused_code "$SRC_URL" source-token)" = "200" ] \
  && ok "source fused serves its token" \
  || bad "source fused does not answer with its token at $SRC_URL"

UPTIME_AT_SNAP=$(guest_sh "$SRC" 'cut -d. -f1 /proc/uptime' | tr -d '\r\n')
SNAP_JSON=$(api POST "/v1/vm/$SRC/snapshot" '{"comment":"e2e resume unique","live":true}')
SNAP=$(echo "$SNAP_JSON" | jqr snapshot_id)
[ "$(echo "$SNAP_JSON" | jqr kind)" = "live" ] && ok "live snapshot $SNAP" || { bad "live snapshot failed: $SNAP_JSON"; exit 1; }
api DELETE "/v1/vm/$SRC" >/dev/null && SRC=""
note "letting the seed age ${AGE}s"
sleep "$AGE"

RANDS=(); MACS=(); URLS=()
for i in 1 2; do
  VM="${PREFIX}-uniq-$i-$$"
  head_ "clone $i: $VM"
  CREATE=$(api POST /v1/vm "{\"name\":\"$VM\",\"seed_snapshot\":\"$SNAP\",\"resume\":true}")
  URL=$(echo "$CREATE" | jqr url)
  if [ -z "$(echo "$CREATE" | jqr vm_id)" ]; then
    bad "resume $i failed: $CREATE"
    continue
  fi
  VMS+=("$VM")
  URLS+=("$URL")

  # the driver being bound is the decisive part: without it the new
  # generation id firecracker writes on the load reaches nothing.
  BOUND=$(guest_sh "$VM" 'ls /sys/bus/acpi/drivers/vmgenid /sys/bus/platform/drivers/vmgenid 2>/dev/null | grep -v -x -e bind -e unbind -e uevent -e module -e new_id -e remove_id -e "" -e "/sys/.*:"' | tr -d '\r')
  [ -n "$BOUND" ] && ok "vmgenid driver bound ($BOUND)" || bad "no vmgenid device bound in the guest (5.10 kernel?)"

  RANDS+=("$(guest_sh "$VM" 'head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n"')")
  MACS+=("$(guest_sh "$VM" 'cat /sys/class/net/eth0/address' | tr -d '\r\n')")

  [ "$(fused_code "$URL" source-token)" = "200" ] \
    && ok "clone wakes up serving the source's token" \
    || bad "clone fused is not the resumed source agent"
  guest_sh "$VM" "echo clone-$i-token > /fuse/auth-token && $RELOAD" >/dev/null
  NEW=$(fused_code "$URL" "clone-$i-token")
  OLD=$(fused_code "$URL" source-token)
  [ "$NEW" = "200" ] && [ "$OLD" = "401" ] \
    && ok "SIGHUP moved fused to the clone's token (new 200, old 401)" \
    || bad "after reload: new token $NEW, source token $OLD, want 200 and 401"

  HOST_EPOCH=$(date -u +%s)
  GUEST_EPOCH=$(guest_sh "$VM" 'date -u +%s' | tr -d '\r\n')
  DRIFT=$(( HOST_EPOCH > ${GUEST_EPOCH:-0} ? HOST_EPOCH - ${GUEST_EPOCH:-0} : ${GUEST_EPOCH:-0} - HOST_EPOCH ))
  [ "$DRIFT" -lt 30 ] && ok "clock within ${DRIFT}s of host after a ${AGE}s-old seed" || bad "clock ${DRIFT}s off the host"

  # clock_realtime moves kvmclock, and so uptime, forward by the seed's age.
  # without it (arm, host kernel < 5.16, or a non-tsc host when the snapshot
  # was taken) uptime continues from the snapshot and only the wall clock is
  # set over ssh, so this is reported rather than asserted.
  if [ "$i" = 1 ]; then
    UPTIME_NOW=$(guest_sh "$VM" 'cut -d. -f1 /proc/uptime' | tr -d '\r\n')
    if [ $(( ${UPTIME_NOW:-0} - ${UPTIME_AT_SNAP:-0} )) -ge "$AGE" ]; then
      ok "uptime advanced by the seed's age (${UPTIME_AT_SNAP}s -> ${UPTIME_NOW}s), clock_realtime took effect"
    else
      note "uptime ${UPTIME_AT_SNAP}s -> ${UPTIME_NOW}s: clock_realtime not used on this host, resync_guest_clock alone set the clock"
    fi
  fi

done

head_ "the clones differ"
if [ "${#RANDS[@]}" -eq 2 ] && [ -n "${RANDS[0]}" ] && [ "${RANDS[0]}" != "${RANDS[1]}" ]; then
  ok "/dev/urandom output differs between clones"
else
  bad "/dev/urandom output matched or was not read: ${RANDS[*]:-none}"
fi
if [ "${#URLS[@]}" -eq 2 ] && [ "${URLS[0]}" != "${URLS[1]}" ]; then
  ok "both clones run at once on their own agent ports (${URLS[*]})"
  for j in 0 1; do
    [ "$(fused_code "${URLS[$j]}" "clone-$((j + 1))-token")" = "200" ] \
      && ok "clone $((j + 1)) still answers with the other clone running" \
      || bad "clone $((j + 1)) stopped answering once the other clone was resumed"
  done
else
  bad "the two clones did not both come up on distinct urls: ${URLS[*]:-none}"
fi
note "macs: ${MACS[*]:-none} (the same by design: each sits in its own namespace)"

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  for c in "${FAILED_CASES[@]}"; do printf '  \033[31m-\033[0m %s\n' "$c"; done
  exit 1
fi
