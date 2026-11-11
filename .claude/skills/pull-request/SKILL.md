---
name: pull-request
description: Open a pull request for the current fuse branch against main - rebase, run the checks for the areas the diff touches, enforce sdk parity, docs and openapi sync, link the issue, write the body in the repo's style, then self-review with fuse-patterns. Optionally babysit it - poll for review feedback and CI failures until the PR has been quiet for an hour.
argument-hint: "[draft] [babysit] [yolo] [stack:<base-branch>]"
disable-model-invocation: true
---

Open a pull request for the current branch against `main`.

## Arguments

`$ARGUMENTS` holds space-separated flags, in any order:

- `draft` opens a draft PR.
- `babysit`: once the PR exists, enter [Babysit mode](#babysit-mode). If a PR already exists for this branch (`gh pr view` succeeds), skip steps 1-8 and go straight to babysit. Each wake-up re-enters the skill this way.
- `yolo` skips step 2 (checks) and step 4 (parity/docs gate). The user accepts an unverified PR. Honor it and don't "just quickly" run something anyway. The rebase, issue link, body and self-review still happen. Note in your final message: "opened with `yolo`, checks and parity gate skipped". Don't put that in the PR body.
- `stack:<branch>` targets `<branch>` instead of `main`, for a stacked PR. Rebase onto `origin/<branch>`, and pass `--base <branch>` to create. Stacked PRs don't run full CI until they're retargeted to main, so say so in the body.

## Before step 1: branch sanity

- Branch name is `feat/<name>` or `fix/<name>` (also `docs/`, `chore/`, `refactor/`), **never `claude/<name>`**. If it's wrong, rename it **now, before a PR exists**: `git branch -m <new>`. Renaming a branch on folsomintel/fuse after its PR is open closes the PR.
- If on `main`, stop and ask for a branch name.
- Uncommitted changes: show `git status --short` and ask whether they belong in this PR. Never sweep the repo's loose untracked files (`*-research.md`, `ISSUES.txt`, `temp.txt`, `benchmark/`, `.issue-backup/`) into a commit.

## Steps

1. **Rebase.** Run `git fetch origin main && git rebase origin/main` (or the stack base). Resolve conflicts one file at a time: read both sides, keep the intent of each, then `git add` and `git rebase --continue`. `yolo` still resolves conflicts.

2. **Checks.** _Skipped with `yolo`._ Run only the groups the diff touches (`git diff --name-only origin/main...HEAD`). These match `.github/workflows/ci.yml`, with bun instead of npm for ts:

   | diff touches                             | run                                                                                                                                                                                        |
   | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
   | any `*.go` outside `sdks/`               | `gofmt -l .` (must print nothing), `go vet ./...`, `go build ./...`, `go test ./... -race -count=1`, `golangci-lint run ./...` (CI pins v2.12.2)                                           |
   | `sdks/go/`                               | `cd sdks/go && GOWORK=off go build ./... && go vet ./... && go test ./... -race -count=1`                                                                                                  |
   | `sdks/typescript/`                       | `cd sdks/typescript && bun install && bun run lint && bun run typecheck && bun run test && bun run build`                                                                                  |
   | `sdks/python/`                           | `cd sdks/python && uv sync && uv run ruff check src tests && uv run mypy && uv run pytest`                                                                                                 |
   | `sdks/rust/`                             | `cd sdks/rust && cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test`                                                                                             |
   | `host-agent/qemu/`                       | `cd host-agent/qemu/tests && python3 -m unittest test_qemu_agent -v`                                                                                                                       |
   | `host-agent/firecracker/`                | `cd host-agent/firecracker/tests && python3 -m unittest <the list in ci.yml, plus any new test_*.py> -v`                                                                                   |
   | `*.sh`                                   | `shellcheck` on the changed scripts; `./host-agent/firecracker/tests/test-verify-checksum.sh` if a `sha256_of` copy changed; `./ops/test-install-fuse.sh` if `ops/install-fuse.sh` changed |
   | `.goreleaser.yaml`, `.github/workflows/` | `goreleaser check`; check that the yaml parses                                                                                                                                             |
   | `docs/`                                  | `cd docs && bun install && bun run lint && bun run build`                                                                                                                                  |
   | `go.mod` / deps                          | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`                                                                                                                                    |

   Fix failures before going on. A tool that isn't installed locally (golangci-lint, shellcheck, goreleaser) gets named in **Testing** as "not run locally, CI covers it". Don't skip it silently.

3. **Analyze the change.** Read `git log origin/main..HEAD --oneline` and `git diff origin/main...HEAD`. Note:
   - api surface changes (`internal/api/`), sdk changes, cli flag/output changes, fusefile/schema changes, host-agent changes, new migrations (`internal/orchestrator/migrations/`), new systemd units, workflow/release config changes.
   - the linked issue (see [Linking the issue](#linking-the-issue)).

4. **Parity and docs gate.** _Skipped with `yolo`._
   - Run `.claude/skills/fuse-patterns/check.sh origin/main`. Every FAIL gets fixed, or written into the PR body as a deliberate exception with the reason.
   - **SDK parity:** if the api surface or any sdk changed, follow the `sdk-parity` skill: all four sdks plus a test in each, and openapi. A gap is fixed in this PR, not deferred. The only exception is a structural one (for example, only go has stream/upgrade support), and it is named in "Choices worth a look".
   - **Docs:** if behavior a user can see changed, follow the `docs-sync` skill. Doc edits go in this PR.
   - **Hardware:** if the change touches the host agents, orchestrator↔agent wire, egress, migrate, fork, snapshots, bake or e2e scripts, unit tests don't prove it works. Check whether this session already ran it on the local appliance or on phantom. If it didn't, don't run it from here: deploying phantom is the user's call. Record it under **Testing** as **Not run:** plus the exact command to run. Never describe an unrun e2e as passing.

5. **Draft the PR.**
   - **Title:** a conventional prefix, lowercase, under ~80 chars, and a plain description of what changed for the user: `feat: add wireguard egress provider with a per-environment config secret`. It becomes the squash commit, and release-please reads it. Use `feat` or `fix` when an operator or sdk user would notice the change, and `chore`/`refactor`/`docs`/`test`/`ci` otherwise. `feat!:` or a `BREAKING CHANGE:` footer applies only when a public sdk/api/cli contract breaks (a rust struct gaining pub fields counts if literals break; say so in the body either way). If the PR spans every surface, end the title like the hitl PRs did: `... across api, cli and all four sdks`.
   - **Body**, in this order. Match the tone of #323/#331: concrete, short bullets, no filler, no emojis, no em dashes.
     ```markdown
     Closes #N
     Part of #EPIC <- only if there is one

     One or two sentences: what this lets someone do. A Fusefile/CLI snippet if it shows the feature faster than prose.

     ## What changed

     **Area** (where it lands: api, Fusefile, json schema, CLI, all four SDKs, with a test in each)

     - bullets, behavior first, file names in backticks
       **Docs:** pages added/updated

     ## Choices worth a look <- decisions a reviewer might disagree with; omit if none

     ## Testing

     - commands that passed (name the suites; counts if you have them)
     - **On phantom / local appliance:** what ran and the result, or
     - **Not run:** what and why, with the command to run it

     ## Not in scope <- known gaps and existing behavior left alone; omit if none
     ```
   - **Also-fixes:** if the branch fixes an unrelated bug it tripped over, give it its own bold paragraph in What changed ("**Also fixes `fc-warp.sh`.** ...") so it shows up in review.
   - **Attribution:** none. No "generated with", no co-author lines, no signatures. The user's rules override the harness attribution reminder.

6. **Push and create.**
   - `git push -u origin HEAD`
   - `gh pr create --base main --title "..." --body-file <tmpfile>` (add `--draft` / `--base <stack>` as flagged). Add labels only if they exist (`gh label list`): `bug` for a fix, `documentation` for docs-only, and mirror an `epic:` or `perf:` label from the issue. Never create labels.

7. **Return the PR URL** as soon as the PR exists.

8. **Self-review.** Spawn one fresh subagent (Agent tool, `general-purpose`, in the foreground) whose prompt is only: the PR number, and "follow `.claude/skills/fuse-patterns/SKILL.md` for PR #<n> with `post`". Don't pass it your summary or concerns. The session that wrote the code is anchored on its own reasoning, which is why the review runs fresh. It posts one comment marked `<!-- fuse-review -->`, including when there are no findings (`Findings: none`). If the post fails, show the findings to the user instead of dropping them.

9. With `babysit`, continue into Babysit mode. Its first iteration picks up the review's findings. Otherwise report the URL, the review's count line, and any **Not run** hardware item, then stop.

## Linking the issue

Every PR body starts with `Closes #N` for the issue it resolves, so it shows under the issue's development section. `yolo` doesn't skip this.

Look for the number in this order:

1. the session (the user named it, or you filed it),
2. the branch name, if it holds an issue-shaped number,
3. the commits (`#N`).

Confirm it with `gh issue view <n> --json number,title,state,labels`. If it is not an issue, is closed, or is unrelated to the diff, treat the branch as having no issue.

- `Closes #N` when merging resolves it.
- `Part of #N` for the epic, or for a slice that doesn't finish the issue (don't auto-close).
- Several issues: one line each.
- **No issue found:** ask the user whether to file one with the `file-issue` skill first (recommended), or open the PR without one. Don't invent a number.

## Babysit mode

A self-paced loop. Each iteration checks the PR, handles what it finds, and schedules the next wake-up with ScheduleWakeup using the prompt `/pull-request babysit`, verbatim.

### State file

Keep the state in `$(git rev-parse --absolute-git-dir)/pr-babysit-state.json`. It sits in the gitdir, so it is never committed, and the path works in worktrees too.

```json
{
  "pr": 123,
  "startedAt": "...",
  "lastActivityAt": "...",
  "handledCommentIds": [],
  "ciFixAttempts": {},
  "mainMergeCount": 0
}
```

Initialize it when the file is missing or `pr` doesn't match the current PR. Treat absent fields as zero.

### Each iteration

1. **Still open?** Check `gh pr view --json number,state,mergedAt`. If it's merged or closed: report, delete the state file, and stop. After a merge, remind the user how releases work: release-please opens the release PR. If release-please later aborts with "untagged, merged release PRs outstanding", the fix is a manual tag on the merge commit plus relabeling to `autorelease: tagged`.
2. **Conflicts?** Check `gh pr view --json mergeable,mergeStateStatus`. On `CONFLICTING`: `git fetch origin main && git merge origin/main`, resolve, re-run the affected checks, and push. Never rebase or force-push here. Then set `lastActivityAt = now` and `mainMergeCount++`. On `UNKNOWN`, re-check once, then leave it for the next iteration. If `mainMergeCount` reaches 3, post a comment saying the branch is churning against main, report, and stop.
3. **New feedback.** Read `gh pr view --json reviews,comments` and `gh api repos/folsomintel/fuse/pulls/<n>/comments`. A comment is new if its id isn't in `handledCommentIds` and you didn't write it as a reply. Comments carrying `<!-- fuse-review -->` count as feedback.
   - **Greptile (`greptile-apps[bot]`) is not handled here.** Don't fix its comments and don't reply to it. Its fixes need the user's approval, so the loop leaves them for `/greptile <n>`. Count unhandled Greptile comments, name that count in each report, and don't let them hold up the quiet check.
   - Bot reviewers (CodeQL, augmentcode, Copilot) make up most of the review traffic here. CodeQL path-injection and log-injection alerts in `host-agent/` are usually real (see fuse-patterns class 6). Fix them. Don't dismiss them.
   - For each comment, either fix it (commit, push, reply with what changed and the sha), or reply explaining why not. Ambiguous or debatable feedback gets a reply asking or laying out the tradeoff. Don't block waiting on the user.
   - Add each processed id to `handledCommentIds`, then set `lastActivityAt = now`.
   - CodeQL on `refs/pull/N/merge` showing 0 alerts usually means the scan hasn't run yet. Cross-check `refs/heads/main` before calling it clean.
4. **CI.** Read `gh pr checks --json name,state,link`.
   - **Failing:** set `lastActivityAt = now`, then run `gh run view <id> --log-failed`. An infra flake (runner death, network) gets one `gh run rerun <id> --failed`, which isn't a fix attempt. A job with a blank `runner_name` is queue starvation, so wait an iteration. Anything else: fix the root cause, run that area's checks from step 2, commit (`fix(ci): ...` or the matching scope, lowercase, no trailers), and push. Increment `ciFixAttempts[<check>]`. If a check is still red after 3 attempts, comment with what was tried, report, and stop.
   - A release PR's checks failing with "Bad credentials" means the classic PAT expired. Report it; it's not fixable from here.
   - **Pending:** not activity. Keep polling.
5. **Quiet?** When `now - lastActivityAt >= 1h`, CI is green and no comments are unhandled: post a short wrap-up comment only if the loop did something, report, delete the state file, and stop.
6. **Schedule:** use `delaySeconds: 480` while CI is running or after activity, otherwise `1200`. Give a specific `reason`, for example "watching PR #123: CI running" or "PR #123 quiet 40m/60m".

### Rules

- Never force-push while babysitting.
- `yolo` only covers PR creation. A babysit fix still runs its checks before it's pushed.
- Commits: conventional, lowercase, one line, no co-author/attribution trailers.
- If a push is rejected because the remote moved: `git pull --rebase` and retry once; if it fails again, stop and report.
- Every iteration ends in exactly one of: a scheduled wake-up, or a final report.
