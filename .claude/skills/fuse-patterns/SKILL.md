---
name: fuse-patterns
description: Review a fuse diff (current branch, or a PR number) for the bug classes that keep recurring in this repo - 500s that should be 4xx, openapi lagging api structs, sdk parity gaps, fc-agent fixes missing from qemu-agent, host-agent path containment, drifted shell helpers, missing timeouts, cli exiting 0 on failure, fusefile fields dropped before create, secrets reaching the guest or logs, vacuous e2e asserts. Use before opening a PR, when asked to "check fuse patterns", "review this for fuse bugs", or as the pull-request skill's self-review.
argument-hint: "[pr-number] [post]"
---

Review the diff against `origin/main` (or PR `$ARGUMENTS` if a number is given: `gh pr checkout <n>` first, or read it with `gh pr diff <n>`) for the patterns below. Each one has burned this repo more than once; the commit/PR refs are the evidence.

## 1. run the mechanical checks

```bash
.claude/skills/fuse-patterns/check.sh origin/main
```

FAIL lines are findings. WARN lines need a look: confirm or dismiss each one in the report, don't silently drop them. Some WARNs are standing debt on main (version.ts drift, fc tests not wired into ci); only report those if the diff touches that area.

## 2. read the diff for the judgment calls

`git diff origin/main...HEAD`. For every class the diff touches, do the check. Skip classes the diff doesn't touch.

| # | pattern | evidence | what to check |
|---|---|---|---|
| 1 | **500 where 4xx is right** | 84d7ab7, fa69107, #221, #74 | every new error path in `internal/orchestrator` that a caller can cause (bad input, wrong state, missing thing) must be a sentinel mapped in `classifyFleetError` (`internal/api/errors.go`) with a case in `error_sentinels_test.go`. a new status also goes in `internal/api/openapi.yaml` and `docs/content/reference/api/` |
| 2 | **contract not exercised end to end** | #220, #169, #221, #204 | when a host-agent response dict changes, the go decoder in `internal/firecracker` / `internal/qemu` reads the same key. e2e asserts decode exec stdout (base64) before comparing. a test that can't fail is a finding |
| 3 | **sdk parity** | #14, #55, 785f795, b9bc805 | see the `sdk-parity` skill. a fix to sdk behavior (timeouts, retries, 202 handling) lands in all four too, not just the type |
| 4 | **openapi lags `internal/api/types.go`** | #323 (`egress.secret` missing from openapi) | every json tag added/renamed in `internal/api/{types,exec,computer,interventions}.go` appears in the matching schema in `openapi.yaml`, with the same required/optional |
| 5 | **fc-only fix** | #214, 2c0732f, a0b736e, 94e6207, #296 | a fix in `host-agent/firecracker/fc-agent.py` - does `host-agent/qemu/qemu-agent.py` have the same code? if so, fix both or say why not in the PR. qemu `qmp.sock` from `vm_dir()` is a known latent SUN_LEN case |
| 6 | **host-agent path containment / CodeQL** | 89d3602, 0b72b8e, #117, a0b736e, #159 | any path built from request data goes through `vm_dir()` / the realpath guard, never the raw id. no request value echoed into a response header |
| 7 | **copied shell helpers drift** | #323 (`iptables -I -t nat`) | `sha256_of()` (6 copies) and `rule()` (fc-warp, fc-wireguard) changed in one copy means change all. table flag first in iptables |
| 8 | **missing timeouts / bounds** | b9bc805, 963f573, #119, ebe1413 | new http clients, agent calls, guest execs, request bodies and loops have a timeout or limit. long-lived handlers (sse, attach, stream) clear the write deadline like the existing ones. `reconcileOrphans` destroy is a known unbounded call |
| 9 | **cli says success on failure** | 1e3b900, 56c052c, 0ffae11 | in `cli/`: a stream EOF/error is not "ready"; empty list from a stale `active_host` is not success; presence of a secret key is not a non-empty secret. non-zero exit on every failure path |
| 10 | **fusefile field dropped before create** | #325 (`hitl.gates`/`exec` dropped by `fuse up`) | a new field in `internal/fusefile` types is copied in `cli/up_cmd.go` and `cli/build_cmd.go`, in `schema/fusefile-v1.json`, and in `docs/content/docs/concepts/fusefile.mdx` |
| 11 | **secrets leaking** | 25cbd9c, a0b736e, #323, #325 | a secret-bearing value never reaches the guest secrets map unless it is meant to, a log line, an sse event, an error string or an audit row unredacted |
| 12 | **main broken by merge/stack/release config** | 6af73ff, ad17ff2, 4ad3d7e, e6a02c9, #198 | pr base is `main` (or the stack parent on purpose); `gofmt -l .` empty; `go build ./...`; `.goreleaser.yaml` / workflow edits pass `goreleaser check` / parse |
| 13 | **checked-in generated files stale** | version.ts 0.21.0 vs 0.39.0 | regenerate anything the diff makes stale |
| 14 | **new test not run in ci** | test_fc_agent_{hotplug,pin,ready}.py | a new `host-agent/*/tests/test_*.py` is added to the explicit list in `.github/workflows/ci.yml` (`test_fc_agent_attach` is excluded on purpose) |

Also hold the repo rules: systemd units live in `ops/systemd/`; migrations in `internal/orchestrator/migrations` stay sequential; fork changes keep the three fork invariants (the fork gets its own credentials and a fused restart, it is allocated on a host like a create, the gpu guardrail holds).

## 3. report

Rank findings P1 (wrong behavior, security, broken main), P2 (drift that will bite: openapi, parity, docs, one-sided fix), P3 (cleanup). For each: `file:line`, the pattern number, the concrete failure ("POST /v1/environments with a held key returns 500 instead of 409"), and the fix. Verify each one against the code before reporting it; drop what doesn't hold. End with a count line: `Findings: 1 P1 · 2 P2 · 0 P3` (or `Findings: none`).

If `post` was passed, post the report as one PR comment whose body starts with `<!-- fuse-review -->` (`gh pr comment <n> --body-file <file>`), so babysit can tell review findings from replies.

Do not fix anything in this skill unless asked; it reports.
