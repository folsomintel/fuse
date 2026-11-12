#!/usr/bin/env bash
# Tests for ops/install-fuse.sh and the restate parts of
# ops/install-orchestrator.sh. No network: a fake `curl` on PATH serves a
# synthetic release, so the platform mapping and the fail-closed verification
# are exercised without touching GitHub.
#
#   ./ops/test-install-fuse.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$HERE/install-fuse.sh"

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL + 1)); }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- a fake release, served by a fake curl ----------------------------------
# RELEASE_DIR stands in for the release's asset list: whatever file the fake
# curl is asked for, it copies from here. A missing file is a 404.
RELEASE_DIR="$WORK/release"
mkdir -p "$RELEASE_DIR"

# A "fuse" binary that just prints a version, tarred the way goreleaser does.
printf '#!/bin/sh\necho "fuse version 9.9.9"\n' > "$WORK/fuse"
chmod +x "$WORK/fuse"

make_asset() {
  # make_asset <asset-name> - build the tarball and record its real checksum.
  tar -czf "$RELEASE_DIR/$1" -C "$WORK" fuse
}

sha_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# The fake curl: understands `-o <dest> <url>` and ignores the rest.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'SHIM'
#!/usr/bin/env bash
dest=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) dest="$2"; shift 2 ;;
    -H) shift 2 ;;
    -*) shift ;;
    *)  url="$1"; shift ;;
  esac
done
name="${url##*/}"
src="$FAKE_RELEASE_DIR/$name"
[ -f "$src" ] || exit 22          # curl's exit code for HTTP 4xx with -f
if [ -n "$dest" ]; then cp "$src" "$dest"; else cat "$src"; fi
SHIM
chmod +x "$WORK/bin/curl"

run_installer() {
  # run_installer <install-dir> - run with the fake curl first on PATH.
  PATH="$WORK/bin:$PATH" FAKE_RELEASE_DIR="$RELEASE_DIR" \
    VERSION=v9.9.9 FUSE_INSTALL_DIR="$1" FUSE_REPO=folsomintel/fuse \
    bash "$INSTALLER" >"$WORK/out.log" 2>&1
}

# The asset name the installer should ask for on this machine, mirroring
# .goreleaser.yaml's cli name_template.
case "$(uname -s)" in
  Darwin) OS="Darwin" ;;
  Linux)  OS="Linux" ;;
  *) echo "unsupported test OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH="x86_64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  i386|i686)     ARCH="i386" ;;
  *) echo "unsupported test arch $(uname -m)" >&2; exit 1 ;;
esac
ASSET="fuse-cli_${OS}_${ARCH}.tar.gz"

echo "install-fuse ($ASSET)"

# --- 1. happy path -----------------------------------------------------------
make_asset "$ASSET"
printf '%s  %s\n' "$(sha_of "$RELEASE_DIR/$ASSET")" "$ASSET" > "$RELEASE_DIR/checksums.txt"
DEST="$WORK/dest-ok"
if run_installer "$DEST" && [ -x "$DEST/fuse" ] && "$DEST/fuse" | grep -q "9.9.9"; then
  ok "a valid asset installs and runs"
else
  bad "a valid asset installs and runs"; cat "$WORK/out.log"
fi

# The installer must resolve the platform to the goreleaser asset name. If this
# mapping ever drifts the fake curl 404s and the install fails, so reaching
# here at all proves the name matched.
grep -q "$ASSET" "$WORK/out.log" && ok "requests the goreleaser asset name" \
  || bad "requests the goreleaser asset name"

# --- 2. tampered asset -------------------------------------------------------
# Same name, different bytes: the case HTTPS cannot catch.
printf '#!/bin/sh\necho pwned\n' > "$WORK/fuse"
chmod +x "$WORK/fuse"
make_asset "$ASSET"                       # rebuilt, checksums.txt now stale
DEST="$WORK/dest-tampered"
if run_installer "$DEST"; then
  bad "a tampered asset is refused"
else
  [ ! -e "$DEST/fuse" ] && ok "a tampered asset is refused, nothing installed" \
    || bad "a tampered asset is refused, nothing installed"
fi

# --- 3. missing checksums.txt ------------------------------------------------
printf '#!/bin/sh\necho "fuse version 9.9.9"\n' > "$WORK/fuse"
chmod +x "$WORK/fuse"
make_asset "$ASSET"
rm -f "$RELEASE_DIR/checksums.txt"
DEST="$WORK/dest-nosums"
if run_installer "$DEST"; then
  bad "a release with no checksums.txt is refused"
else
  [ ! -e "$DEST/fuse" ] && ok "a release with no checksums.txt is refused" \
    || bad "a release with no checksums.txt is refused"
fi

# --- 4. no entry for this asset ----------------------------------------------
printf '%s  %s\n' "$(sha_of "$RELEASE_DIR/$ASSET")" "some-other-asset.tar.gz" \
  > "$RELEASE_DIR/checksums.txt"
DEST="$WORK/dest-noentry"
if run_installer "$DEST"; then
  bad "a checksums.txt without this asset is refused"
else
  [ ! -e "$DEST/fuse" ] && ok "a checksums.txt without this asset is refused" \
    || bad "a checksums.txt without this asset is refused"
fi

# --- install-orchestrator.sh, FUSE_RESTATE_MODE=self --------------------------
# Runs the real installer under FUSE_INSTALL_ROOT with shims for id, systemctl
# and docker, and a curl that plays the restate admin api. FAKE_INFLIGHT_<port>
# and FAKE_DEPLOYED_<port> say what restate holds for each handler address.
echo
echo "install-orchestrator (restate)"

ORCH_INSTALLER="$HERE/install-orchestrator.sh"
IMAGE=$(grep -E '^RESTATE_IMAGE=' "$ORCH_INSTALLER" | cut -d= -f2-)
OBIN="$WORK/obin"
mkdir -p "$OBIN"
printf '#!/bin/sh\n[ "$1" = -u ] && echo 0 || /usr/bin/id "$@"\n' > "$OBIN/id"
printf '#!/bin/sh\necho 10.0.0.5\n' > "$OBIN/hostname"
printf '#!/bin/sh\necho "systemctl $*" >> "$FAKE_LOG"\n[ "$1" = is-active ] && exit 3\nexit 0\n' > "$OBIN/systemctl"
cat > "$OBIN/docker" <<'SHIM'
#!/usr/bin/env bash
echo "docker $*" >> "$FAKE_LOG"
case "$1 ${2:-}" in
  "image inspect") echo "restatedev/restate@${FAKE_DIGEST} " ;;
esac
exit 0
SHIM
cat > "$OBIN/curl" <<'SHIM'
#!/usr/bin/env bash
method=GET; body=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    -d) body="$2"; shift 2 ;;
    -o|-H|--max-time|-w) shift 2 ;;
    -*) shift ;;
    *)  url="$1"; shift ;;
  esac
done
port=$(echo "$body" | grep -oE ':908[0-9]/' | tr -d :/)
case "$method $url" in
  *ifconfig.me*) exit 6 ;;
  *"/health") exit 0 ;;
  "POST "*/query)
    case "$body" in
      *sys_invocation*) echo "count $port" >> "$FAKE_LOG"; v="FAKE_INFLIGHT_$port"; echo "{\"rows\":[{\"n\":${!v:-0}}]}" ;;
      *sys_deployment*) v="FAKE_DEPLOYED_$port"
        if [ "${!v:-}" = 1 ]; then echo "{\"rows\":[{\"id\":\"dp_$port\"}]}"; else echo '{"rows":[]}'; fi ;;
    esac ;;
  "POST "*/deployments)
    echo "register $body" >> "$FAKE_LOG"
    echo '{"id":"dp_new","services":[{"name":"Environment"},{"name":"Host"}]}' ;;
  "DELETE "*) echo "delete $url" >> "$FAKE_LOG" ;;
  *) exit 22 ;;
esac
SHIM
chmod +x "$OBIN"/*

printf '#!/bin/sh\necho "orchestrator v2"\n' > "$WORK/orch-new"
# an old binary that knows drain-only mode carries the env var name.
printf '#!/bin/sh\n# ORCH_RESTATE_DRAIN_ONLY\necho "orchestrator v1"\n' > "$WORK/orch-old-drain"
printf '#!/bin/sh\necho "orchestrator v1"\n' > "$WORK/orch-old"
chmod +x "$WORK"/orch-*

# run_orch <root> [VAR=value ...] - run install-orchestrator.sh into <root>.
run_orch() {
  local root="$1"; shift
  mkdir -p "$root"
  : > "$root.log"
  env PATH="$OBIN:$PATH" FAKE_LOG="$root.log" FAKE_DIGEST="${IMAGE##*@}" \
    FUSE_INSTALL_ROOT="$root" ORCH_BIN_SRC="$WORK/orch-new" "$@" \
    bash "${ORCH_SCRIPT:-$ORCH_INSTALLER}" >"$root.out" 2>&1
}

# seed_upgrade <root> <old-binary> - an existing mode-self install on :9080.
seed_upgrade() {
  mkdir -p "$1/usr/local/bin" "$1/etc/fuse"
  install -m0755 "$2" "$1/usr/local/bin/orchestrator"
  printf 'ORCH_AUTH_TOKEN=t\nDATABASE_URL=postgres://db\nFUSE_RESTATE_MODE=self\nORCH_RESTATE_LISTEN=127.0.0.1:9080\n' \
    > "$1/etc/fuse/orchestrator.env"
}

check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# 1. mode self without a database is refused before anything is installed.
R="$WORK/o-nodb"
if run_orch "$R" FUSE_RESTATE_MODE=self DATABASE_URL=; then
  bad "mode self without DATABASE_URL is refused"
else
  check "mode self without DATABASE_URL is refused, nothing installed" \
    '[ ! -e "$R/usr/local/bin/orchestrator" ] && [ ! -e "$R/etc/fuse/restate.env" ] && grep -q DATABASE_URL "$R.out"'
fi

# 2. fresh install.
R="$WORK/o-fresh"
if run_orch "$R" FUSE_RESTATE_MODE=self DATABASE_URL=postgres://db; then
  ok "fresh mode self install succeeds"
else
  bad "fresh mode self install succeeds"; cat "$R.out"
fi
# shellcheck disable=SC2034 # read inside check strings
E="$R/etc/fuse/restate.env"
check "restate.env binds ingress, admin and node ports to 127.0.0.1" \
  '[ "$(grep -cE "^RESTATE_([A-Z]+__)?BIND_ADDRESS=127\.0\.0\.1:" "$E")" = 3 ]'
check "restate.env keeps journals and workflow completions for 7d" \
  'grep -qx RESTATE_DEFAULT_JOURNAL_RETENTION=7d "$E" && grep -qx RESTATE_DEFAULT_WORKFLOW_COMPLETION_RETENTION=7d "$E"'
check "restate.env is private" '[ "$(stat -c %a "$E" 2>/dev/null || stat -f %Lp "$E")" = 600 ]'
check "installs restate-server.service pinned to the installer's image" \
  'grep -qx "Environment=FUSE_RESTATE_IMAGE=$IMAGE" "$R/etc/systemd/system/restate-server.service"'
check "pulls the pinned image by digest" 'grep -qx "docker pull -q $IMAGE" "$R.log"'
check "starts restate-server before the orchestrator" \
  '[ "$(grep -nE "restart (restate-server|fuse-orchestrator)" "$R.log" | cut -d: -f2 | tr "\n" " ")" = "systemctl restart restate-server.service systemctl restart fuse-orchestrator.service " ]'
check "orchestrator unit wants restate-server" \
  'grep -q "^Wants=.*restate-server.service" "$R/etc/systemd/system/fuse-orchestrator.service"'
# shellcheck disable=SC2034 # read inside check strings
O="$R/etc/fuse/orchestrator.env"
check "orchestrator.env gets mode self, the ingress url, the handler address and the database" \
  'grep -qx FUSE_RESTATE_MODE=self "$O" && grep -qx FUSE_RESTATE_INGRESS_URL=http://127.0.0.1:18080 "$O" && grep -qx ORCH_RESTATE_LISTEN=127.0.0.1:9080 "$O" && grep -qx DATABASE_URL=postgres://db "$O"'
check "registers the handler endpoint in place" \
  'grep -qF "register {\"uri\":\"http://127.0.0.1:9080\",\"force\":true}" "$R.log"'
check "no drain unit on a fresh install" '! grep -q "fuse-orchestrator-drain" "$R.log"'

# 3. snapshots: s3 is written, anything else is refused.
R="$WORK/o-snap"
check "a snapshot install succeeds" 'run_orch "$R" FUSE_RESTATE_MODE=self DATABASE_URL=postgres://db RESTATE_SNAPSHOT_DESTINATION=s3://bkt/fuse'
check "an s3 snapshot destination lands in restate.env" \
  'grep -qx RESTATE_WORKER__SNAPSHOTS__DESTINATION=s3://bkt/fuse "$R/etc/fuse/restate.env"'
R="$WORK/o-snap-fs"
if run_orch "$R" FUSE_RESTATE_MODE=self DATABASE_URL=postgres://db RESTATE_SNAPSHOT_DESTINATION=/mnt/nfs/restate; then
  bad "a filesystem snapshot destination is refused"
else
  ok "a filesystem snapshot destination is refused"
fi

# 4. an image that does not hash to the pin is never started.
R="$WORK/o-digest"
if run_orch "$R" FUSE_RESTATE_MODE=self DATABASE_URL=postgres://db FAKE_DIGEST=sha256:0000; then
  bad "an image with the wrong digest is refused"
else
  check "an image with the wrong digest is refused, restate-server not started" \
    '! grep -q "restart restate-server" "$R.log"'
fi

# 5. upgrade with nothing pinned to the old deployment: re-register in place.
R="$WORK/o-up-idle"
seed_upgrade "$R" "$WORK/orch-old"
check "an idle upgrade succeeds" 'run_orch "$R" FAKE_DEPLOYED_9080=1'
check "an idle upgrade re-registers in place, no drain" \
  'grep -qF "register {\"uri\":\"http://127.0.0.1:9080\",\"force\":true}" "$R.log" && ! grep -q "enable --now fuse-orchestrator-drain" "$R.log"'
check "an upgrade swaps the binary" '"$R/usr/local/bin/orchestrator" | grep -q v2'
check "the old orchestrator is stopped before its invocations are counted" \
  '[ "$(grep -nE "stop fuse-orchestrator.service|count 9080" "$R.log" | cut -d: -f2 | tr "\n" " ")" = "systemctl stop fuse-orchestrator.service count 9080 " ]'

# 6. upgrade with invocations pinned to the old deployment.
R="$WORK/o-up-busy"
seed_upgrade "$R" "$WORK/orch-old-drain"
check "a busy upgrade succeeds" 'run_orch "$R" FAKE_DEPLOYED_9080=1 FAKE_INFLIGHT_9080=2'
check "a busy upgrade keeps the old binary for the drain unit" \
  'cmp -s "$WORK/orch-old-drain" "$R/var/lib/fuse/restate-drain/orchestrator"'
check "the drain unit serves the old address in drain-only mode" \
  'grep -qx ORCH_RESTATE_LISTEN=127.0.0.1:9080 "$R/var/lib/fuse/restate-drain/drain.env" && grep -qx ORCH_RESTATE_DRAIN_ONLY=true "$R/var/lib/fuse/restate-drain/drain.env"'
check "the drain unit starts before the new orchestrator" \
  '[ "$(grep -nE "enable --now fuse-orchestrator-drain|restart fuse-orchestrator" "$R.log" | cut -d: -f2 | tr "\n" " ")" = "systemctl enable --now fuse-orchestrator-drain.service systemctl restart fuse-orchestrator.service " ]'
check "the new orchestrator moves to the other handler port" \
  'grep -qx ORCH_RESTATE_LISTEN=127.0.0.1:9081 "$R/etc/fuse/orchestrator.env"'
check "the new deployment registers beside the old one, not forced" \
  'grep -qF "register {\"uri\":\"http://127.0.0.1:9081\",\"force\":false}" "$R.log"'
check "the old deployment retires only after the new address registers" \
  'grep -qx RESTATE_DRAIN_AFTER_URI=http://127.0.0.1:9081 "$R/var/lib/fuse/restate-drain/drain.env"'

# 6b. a drain-capable old binary goes blue/green even when idle.
R="$WORK/o-up-bg-idle"
seed_upgrade "$R" "$WORK/orch-old-drain"
run_orch "$R" FAKE_DEPLOYED_9080=1
check "a drain-capable idle upgrade still goes blue/green" \
  'grep -qx ORCH_RESTATE_LISTEN=127.0.0.1:9081 "$R/etc/fuse/orchestrator.env" && grep -q "enable --now fuse-orchestrator-drain" "$R.log"'

# 7. a busy upgrade whose old binary cannot drain is refused untouched...
R="$WORK/o-up-nodrain"
seed_upgrade "$R" "$WORK/orch-old"
if run_orch "$R" FAKE_DEPLOYED_9080=1 FAKE_INFLIGHT_9080=2; then
  bad "a busy upgrade without drain support is refused"
else
  check "a busy upgrade without drain support is refused, old binary kept" \
    '"$R/usr/local/bin/orchestrator" | grep -q v1'
  check "the refused upgrade starts the old orchestrator again" \
    'grep -q "systemctl start fuse-orchestrator.service" "$R.log"'
fi
# ...unless forced.
R="$WORK/o-up-forced"
seed_upgrade "$R" "$WORK/orch-old"
run_orch "$R" FAKE_DEPLOYED_9080=1 FAKE_INFLIGHT_9080=2 FUSE_RESTATE_FORCE_UPGRADE=1
check "FUSE_RESTATE_FORCE_UPGRADE=1 replaces it in place" \
  'grep -qF "register {\"uri\":\"http://127.0.0.1:9080\",\"force\":true}" "$R.log"'

# 8. a second busy upgrade while the first still drains the other port.
R="$WORK/o-up-twice"
seed_upgrade "$R" "$WORK/orch-old-drain"
if run_orch "$R" FAKE_DEPLOYED_9080=1 FAKE_INFLIGHT_9080=2 FAKE_DEPLOYED_9081=1 FAKE_INFLIGHT_9081=1; then
  bad "an upgrade while an earlier one still drains is refused"
else
  check "an upgrade while an earlier one still drains is refused" 'grep -q "still draining" "$R.out"'
fi

# 9. an existing self install is not switched out of self, and an explicit
#    mode is saved.
R="$WORK/o-leave-self"
seed_upgrade "$R" "$WORK/orch-old"
if run_orch "$R" FUSE_RESTATE_MODE=off FAKE_DEPLOYED_9080=1; then
  bad "switching a self install to off is refused"
else
  check "switching a self install to off is refused, old binary kept" \
    '"$R/usr/local/bin/orchestrator" | grep -q v1 && grep -qx FUSE_RESTATE_MODE=self "$R/etc/fuse/orchestrator.env"'
fi
R="$WORK/o-cloud"
check "a fresh cloud install succeeds" 'run_orch "$R" FUSE_RESTATE_MODE=cloud'
check "a fresh cloud install saves the mode and runs no restate-server" \
  'grep -qx FUSE_RESTATE_MODE=cloud "$R/etc/fuse/orchestrator.env" && ! grep -q "^docker" "$R.log"'

# 10. mode off stays restate-free.
R="$WORK/o-off"
check "mode off installs" 'run_orch "$R"'
check "mode off installs no restate pieces and calls no docker" \
  '[ -x "$R/usr/local/bin/orchestrator" ] && [ ! -e "$R/etc/fuse/restate.env" ] && ! grep -q "^docker" "$R.log" && ! grep -q FUSE_RESTATE "$R/etc/fuse/orchestrator.env"'

# 11. curl'd standalone, the inline units match the tracked ones.
mkdir -p "$WORK/standalone"
cp "$ORCH_INSTALLER" "$WORK/standalone/"
R="$WORK/o-inline"
ORCH_SCRIPT="$WORK/standalone/install-orchestrator.sh" run_orch "$R" FUSE_RESTATE_MODE=self DATABASE_URL=postgres://db
for u in restate-server.service fuse-orchestrator-drain.service; do
  strip() { grep -vE '^(#|Documentation=|$)' "$1" | sed -e 's/[[:space:]]*\\$//' -e 's/^[[:space:]]*//'; }
  if diff <(strip "$HERE/systemd/$u") <(strip "$R/etc/systemd/system/$u") >/dev/null; then
    ok "inline $u matches ops/systemd/$u"
  else
    bad "inline $u matches ops/systemd/$u"; diff <(strip "$HERE/systemd/$u") <(strip "$R/etc/systemd/system/$u")
  fi
done

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
