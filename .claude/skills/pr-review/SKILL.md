---
name: pr-review
description: Blast-radius review of a fuse PR or branch. Maps the diff to every subsystem it can affect (orchestrator, lifecycle, networking, agent wire, hitl, durable, api, cli, fusefile, sdks, guest, ops, release), runs each affected area's tests including reverse go deps and the hostless lifecycle e2e, has a fresh reviewer per area check that area's invariants still hold, and ends with a per-area pass/fail matrix and a merge verdict. Use when asked to "review this PR", "make sure this doesn't break anything", "check what this touches", "is this safe to merge", or via /pr-review.
argument-hint: "[pr-number] [hardware] [post]"
---

Prove that a diff doesn't break the parts of fuse it reaches. `fuse-patterns` looks for known bug classes. This skill asks a different question: which subsystems can this change damage, and does each one still pass and still hold its invariants?

**This skill reports. It doesn't fix, commit or push.** The only GitHub write is the optional `post` comment.

## Arguments

Space-separated, in any order:

- `<n>`: review PR `n`. Without it, review the current branch against `origin/main`.
- `hardware`: also run the hardware tier on the **local appliance** (step 5). Never phantom.
- `post`: post the report as one PR comment.

## 1. Get the code

- **Current branch:** `git fetch origin main`. The base is `origin/main`, or the PR's base for a stacked PR (`gh pr view --json baseRefName`).
- **PR number:** don't check it out over the user's tree. Use a throwaway worktree:
  ```bash
  W="$CLAUDE_JOB_DIR/tmp/pr-<n>"   # or a mktemp -d outside a job
  git fetch origin pull/<n>/head:pr-review-<n> <base>
  git worktree add --detach "$W" pr-review-<n>
  ```
  Run everything below from `$W`. Remove it at the end (`git worktree remove "$W" && git branch -D pr-review-<n>`).

## 2. Map the blast radius

```bash
.claude/skills/pr-review/impact.sh origin/<base>
```

It prints `AREA` lines (affected subsystems and why), `PKG` lines (changed go packages plus every package that imports them) and `FILE` lines. It catches path matches, network or lifecycle edits buried in the host agents, contract pull-ins (api to sdks and cli, fusefile to cli, fc-agent without qemu-agent) and go reverse deps.

Then read `git diff origin/<base>...HEAD` and add any area the script missed. Example: a `fleet.go` change that alters what reconcile sees is `orchestrator` even if only `exec.go` changed. Note each addition with its reason. Under-scoping is the failure mode here, so when unsure, include the area.

## 3. Run the gates (tier 1 and 2, always local)

Run only the affected areas, but run all of each. Record pass/fail per command with counts. A tool that isn't installed gets **not run (CI covers it)**, never a silent skip.

| area         | tier 1: unit / static                                                                                                                                                                                        | tier 2: local integration, no host                                                                                                         |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------ |
| any go       | `gofmt -l .` (empty), `go vet ./...`, `go build ./...`, `go test -race -count=1 <every PKG line>`                                                                                                            |                                                                                                                                            |
| orchestrator | above, plus `golangci-lint run ./...`                                                                                                                                                                        | `FUSE_E2E_PORT=18180 ./host-agent/firecracker/tests/fc-e2e.sh` (stub provider: create, get, list, snapshot, restore, drain, destroy; ~20s) |
| lifecycle    | `go test -race -count=1 ./internal/orchestrator/ -run 'Fork\|Snapshot\|Checkpoint\|Delta\|Migrate\|Restore'`; fc tests `test_fc_agent_{delta,lazy_mem,live_artifact}` + `test_fc_uffd_workers`               | `fc-e2e.sh` as above                                                                                                                       |
| networking   | `go test -race -count=1 ./internal/egress/... ./internal/ingress/... ./internal/tunnel/... ./proxy/... ./fused/...`; fc tests `test_fc_agent_{egress,tunnel,public_host}`; `shellcheck` on changed `fc-*.sh` | `fc-e2e.sh` if the orchestrator egress/ingress code changed                                                                                |
| agent-wire   | `go test -race -count=1 ./internal/hostwire/... ./internal/firecracker/... ./internal/qemu/...`; all fc tests in the ci.yml list; `python3 -m unittest test_qemu_agent -v`                                   | `fc-e2e.sh`                                                                                                                                |
| hitl         | `go test -race -count=1 ./internal/hitl/... ./internal/orchestrator/ -run 'Hitl\|Gate\|Intervention\|Takeover\|Access\|Audit\|Decider'`; `go test ./cli/ -run 'Hitl'`                                        |                                                                                                                                            |
| durable      | `go test -race -count=1 ./internal/durable/...`                                                                                                                                                              | `./test/e2e/restate.sh` if `restate-server` is on PATH or docker is up (`RESTATE_COMPOSE=1`); otherwise **not run**                        |
| api          | `go test -race -count=1 ./internal/api/...`; `.claude/skills/fuse-patterns/check.sh origin/<base>`                                                                                                           | `fc-e2e.sh`                                                                                                                                |
| cli          | `go test -race -count=1 ./cli/...`                                                                                                                                                                           |                                                                                                                                            |
| fusefile     | `go test -race -count=1 ./internal/fusefile/... ./cli/...`                                                                                                                                                   |                                                                                                                                            |
| sdks         | the per-sdk commands in `.claude/skills/pull-request/SKILL.md` step 2, for **all four**, not just the changed one                                                                                            |                                                                                                                                            |
| guest        | `go test -race -count=1 ./fused/...`; `shellcheck` on changed bake scripts                                                                                                                                   |                                                                                                                                            |
| ops          | `shellcheck` on changed scripts; `./ops/test-install-fuse.sh`; `./host-agent/firecracker/tests/test-verify-checksum.sh`                                                                                      |                                                                                                                                            |
| release      | `goreleaser check`; yaml parses; `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` if deps moved                                                                                                       |                                                                                                                                            |
| docs         | `cd docs && bun install && bun run lint && bun run build`                                                                                                                                                    |                                                                                                                                            |

Notes:

- fc tests run from `host-agent/firecracker/tests` with `python3 -m unittest <names> -v`. `test_fc_agent_attach` needs a real pty and is excluded from CI on purpose. Skip it too.
- `fc-e2e.sh` defaults to port 18080, which is restate's ingress port. Keep `FUSE_E2E_PORT=18180`. It writes `bin/fuse` (gitignored) and logs to `/tmp/fuse-e2e.log`. Read the log when it fails.
- A failure that also fails on `origin/<base>` is pre-existing. Check that before blaming the PR (re-run the one failing test in a base worktree). Report it as **pre-existing**, not as a PR failure.

## 4. Invariant review (one fresh reviewer per area)

Tests only catch what someone thought to test. For each affected area that has invariants below, spawn one `general-purpose` subagent. Send them all in a single message so they run in parallel, at most 6. If more than 6 areas are affected, merge the smallest ones. Give each one only:

- the PR number or the base ref and worktree path,
- its area's invariant list, copied from below,
- this instruction: _"For each invariant, read the diff and the surrounding code and answer **holds**, **broken** or **unsure**, with `file:line` evidence and, when it's broken, the concrete failure (input, then the wrong result). Also flag any behavior in this area that changes for existing callers who didn't opt in. Report only. Don't edit files."_

Don't pass along your own opinion of the diff. A fresh reviewer is the point.

**orchestrator**

- migrations in `internal/orchestrator/migrations` are sequential, and a shipped migration is never edited. A schema change regenerates ent with `GOWORK=off GOFLAGS=-mod=mod go run entgo.io/ent/cmd/ent generate --feature sql/upsert,sql/versioned-migration ./internal/entspike/ent/schema`. Without those features it silently drops OnConflictColumns/NamedDiff.
- `recoverState()` still rehydrates hosts and vms from postgres after a restart, and new persisted fields round-trip through both state stores (memory and postgres).
- every create and fork allocates host capacity, and every destroy deallocates exactly once. Counters clamp at 0, so a missed allocation turns into permanent overcommit.
- every provider or agent call made from reconcile has its own timeout. One stuck vm must not stall the whole cycle (the orphan-destroy stall, #357).
- a vm in `draining` or `destroying` still reaches a terminal state.

**lifecycle**

- a fork gets new credentials **and** a `fused` restart (`StartAgent`). Uploading credential files alone does nothing to a running fused.
- a fork is allocated on the source's host and never goes through `Schedule`.
- the gpu guardrail fires before any provider-forkability type assertion.
- a live snapshot's rootfs is not standalone. Fork and seed refuse live snapshots, and the fork live guard in `fork.go` stays.
- `/snapshot/load` uses `track_dirty_pages` (the old `enable_diff_snapshots` name breaks every live restore). Hugepage memory resumes through fc-uffd, never the File backend.
- snapshot and checkpoint delete still frees host disk. Live snapshot pulls stay pinned by `?snapshot=`.
- a failed migrate leaves the source running.

**networking**

- `egress.mode: direct` create is byte-for-byte unchanged: no new rules, no purges.
- proxy mode keeps an explicit `FORWARD -i <tap> -o <iface> -j DROP` below a RELATED,ESTABLISHED accept, purges any stale ACCEPT on its tap, and DNS fails closed.
- every rule a create adds, destroy removes. Stale `FORWARD -i fcvN ACCEPT` leaks are a known issue (#211), so don't add another.
- the iptables table flag comes first (`iptables -t nat -I ...`). `rule()` stays identical in `fc-warp.sh` and `fc-wireguard.sh`.
- provider credentials travel by env, then stdin, then a 0600 file. They never appear in argv, `meta.json`, the guest, logs or error strings.
- ingress and tunnel never echo a request value into a response header, and open tunnel streams survive a token rotation (#356).
- ports stay put: orchestrator 8080, fc-agent 8090, restate ingress 18080, admin 9070, handler 9080/9081, all restate binds on loopback.

**agent-wire**

- every key the agent returns is read under the same name by the go decoder in `internal/firecracker` / `internal/qemu`, and the reverse holds for requests.
- changes are additive. An orchestrator from this PR still talks to an agent from the last release, and the reverse. Hosts update on their own timer, so a new required field breaks a fleet mid-upgrade.
- a fix in `fc-agent.py` that applies to `qemu-agent.py` is in both, or the PR says why not.
- any path built from request data goes through `vm_dir()` / the realpath guard. Unix socket paths stay under the 107-byte SUN_LEN.
- guest execs pass `timeout_ms`. Nothing holds `vm_lock` for longer than its caller waits.

**hitl**

- a gate can't be bypassed by a path the diff adds: every new action on a gated resource goes through the gate check.
- audit rows are written for every decision and stay tamper-evident.
- secret values never reach logs, sse events, error strings or audit rows unredacted.
- an intervention pending across an orchestrator restart is still pending afterwards.

**durable**

- with `FUSE_RESTATE_MODE=off`, the orchestrator behaves exactly as before restate existed.
- restate down means `/ready` returns 200 "degraded", never 503.
- the restate version stays in step across `ops/restate/compose.yaml`, `ops/systemd/restate-server.service`, `RESTATE_IMAGE` in `ops/install-orchestrator.sh` and `RESTATE_VERSION` in `host-agent/local/fuse-local-setup.sh`.
- an upgrade with pinned invocations is refused unless `FUSE_RESTATE_FORCE_UPGRADE=1`, until drain-only mode exists.

**api / cli / fusefile / sdks**

- a caller-caused error is a sentinel mapped in `classifyFleetError` and returns 4xx, not 500.
- openapi matches the go structs. All four sdks plus a test in each move with any surface change (the `sdk-parity` skill).
- the cli exits non-zero on every failure path. Empty output from a stale `active_host` is not success.
- a new fusefile field is copied in `cli/up_cmd.go` and `cli/build_cmd.go`, the json schema and the fusefile concept page.
- no existing field changes meaning or default for a client that doesn't send it.

**guest / ops / release**

- fused still starts from the base bake, and a rebake can't use a stale `/tmp/fcbake-work` bundle.
- the checksum helper copies stay byte-identical. systemd units live in `ops/systemd/`. `install-updater` paths match the post-#328 layout.
- `gofmt` stays clean, `goreleaser check` passes, and nothing in a workflow change can break main's release.

## 5. Hardware tier (only reported unless `hardware` is passed)

Areas `orchestrator`, `lifecycle`, `networking`, `agent-wire`, `hitl`, `guest` and `durable` aren't proven by unit tests. For each one, write the exact command into the report. With `hardware`, run them on the local appliance, branch-built so it exercises this PR and not a release:

```bash
go build -buildvcs=false -o /tmp/fuse-pr ./cli                       # embeds this branch's fc-agent.py
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -o /tmp/orch-pr ./orchestrator   # arm64 on apple silicon
FUSE_LOCAL_ORCH_BIN=/tmp/orch-pr /tmp/fuse-pr local up
```

| area                     | hardware command (`FUSE_BIN=/tmp/fuse-pr`)                                                                                                      |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| orchestrator, agent-wire | `./test/e2e/run.sh baseline negative` and `FC_AGENT_URL=... ./host-agent/firecracker/tests/fc-agent-test.sh`                                    |
| lifecycle                | `./test/e2e/run.sh cache` and `./test/e2e/live-snapshot-agent.sh`; migrate needs two hosts, so it's always **not run**, say so                  |
| networking               | `./test/e2e/run.sh egress health` (the appliance sets `ORCH_EGRESS_MOCK=true`); warp and wireguard need a real provider, so they're **not run** |
| hitl                     | the `test/e2e/hitl-*.sh` scripts matching the changed files (ask, access, exec, takeover, computer)                                             |
| durable                  | `./test/e2e/restate.sh` inside the appliance                                                                                                    |
| guest                    | `./test/e2e/run.sh baseline files env`                                                                                                          |
| qemu / gpu               | no qemu or gpu host exists: always **not run**                                                                                                  |

Rules:

- **Never phantom.** Deploying to the live box is the user's call. Give them the command and leave it.
- If `fuse local up` fails, report the failure and mark the tier **not run**. Don't debug the appliance from inside a review.
- Run `/tmp/fuse-pr local down` at the end only if this run brought it up.
- An unrun hardware item is **not run**, never "expected to pass".

## 6. Report

Lead with the verdict, then the matrix:

```
Verdict: blocked (2 failing, 1 broken invariant) | needs hardware run | safe to merge

| area        | why affected                  | tier 1      | tier 2      | invariants        | hardware |
|-------------|-------------------------------|-------------|-------------|-------------------|----------|
| networking  | fc-agent diff touches iptables | pass 41/41  | pass        | 1 broken, 6 holds | not run  |
```

- **safe to merge**: every affected area passes tiers 1 and 2, no invariant is broken, and no area needs hardware, or the hardware tier passed.
- **needs hardware run**: the local tiers are clean, but an area needs hardware that wasn't run. List the exact commands.
- **blocked**: any failing gate (not counting pre-existing ones) or any **broken** invariant.

Below the matrix, list the details:

1. failures, with the command and the relevant output lines,
2. broken invariants, with `file:line`, the concrete failure and the fix,
3. **unsure** invariants, with what would settle each one,
4. pre-existing failures (also failing on base),
5. areas added by hand in step 2, with the reason,
6. not-run items, with their commands.

Verify every broken invariant against the code yourself before reporting it. Drop the ones that don't hold up. Subagent reports aren't visible to the user, so restate anything that matters.

With `post`: post the report as one comment whose body starts with `<!-- fuse-pr-review -->` (`gh pr comment <n> --body-file <file>`). Babysit mode treats it as feedback, like `<!-- fuse-review -->`.
