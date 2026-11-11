---
name: greptile
description: Pull Greptile's review comments on a fuse PR, draft a fix for each one, show the diff locally and ask the user to approve each fix, then commit and push the approved ones. Never replies to, reacts to or resolves Greptile's threads. Use when asked to "address greptile", "incorporate greptile feedback", "handle the greptile review", or via /greptile.
argument-hint: "[pr-number]"
disable-model-invocation: true
---

Turn Greptile's review into local, user-approved commits. **Greptile gets no response.** Don't reply, react, resolve threads, re-request review or post a summary comment. The only thing that reaches GitHub is the push.

## 1. Find the PR and branch

- PR is `$ARGUMENTS` if given, otherwise `gh pr view --json number,headRefName,state`.
- The checked-out branch must be the PR's `headRefName`. If it isn't, stop and say which branch to check out. Don't switch branches yourself, since the user may have work in progress.
- `git fetch origin && git status -sb`. If the branch is behind its remote, `git pull --ff-only`. If it has diverged, stop and report.
- Record the files that are already dirty (`git status --porcelain`). Never restore, stash or commit them. If a fix needs to touch one of them, show that finding to the user but don't draft it.

## 2. Collect Greptile's comments

Greptile posts as `greptile-apps[bot]`. Pull all three surfaces:

```bash
gh api --paginate repos/folsomintel/fuse/pulls/<n>/comments \
  --jq '.[] | select(.user.login=="greptile-apps[bot]" and .in_reply_to_id==null) | {id, path, line, original_line, commit_id, body}'
gh api --paginate repos/folsomintel/fuse/pulls/<n>/reviews \
  --jq '.[] | select(.user.login=="greptile-apps[bot]") | {id, state, commit_id, body}'
gh api --paginate repos/folsomintel/fuse/issues/<n>/comments \
  --jq '.[] | select(.user.login=="greptile-apps[bot]") | {id, body}'
```

- **Inline comments** are the findings. A ` ```suggestion ` block is Greptile's proposed replacement for the commented line range.
- **Review and issue comment bodies** are usually a summary or confidence score. Pull findings out of them only where they name a concrete problem with a file. A body that is only an HTML comment marker (`<!-- greptile-auto-approve ... -->`) has nothing to act on.
- Skip ids already in the state file (see [State](#state)).
- `line` is null when the code moved since Greptile commented. Use `original_line` plus `commit_id` (`git show <commit_id>:<path>`) to find the spot in the current file. If you can't find it, the finding is **stale**.

If there's nothing new, say so and stop.

## 3. Draft a fix for each finding (nothing applied yet)

For each finding, one at a time:

1. Read the code around it, not just the commented line. Decide on a verdict:
   - **valid**: the problem is in the code. Draft the fix, and prefer Greptile's suggestion block when it's correct.
   - **disagree**: wrong, already handled, or it conflicts with repo conventions (CLAUDE.md, sdk parity, fuse-patterns). Write one line saying why.
   - **stale**: the code it points at is gone.
2. For a valid finding: make the edit, save the patch, then put the files back so the tree stays at the base:
   ```bash
   git diff -- <files> > "$CLAUDE_JOB_DIR/tmp/greptile-<id>.patch"   # or a mktemp dir outside a job
   git restore -- <files>
   ```
   Only restore files that weren't dirty in step 1.
3. If the fix changes an sdk's api surface, draft it in all four sdks with a test in each (the `sdk-parity` skill), as one patch.

## 4. Show and ask

First print a numbered list in plain text. Each entry gets the finding (`path:line` and Greptile's point in one line), the verdict, and for a valid finding the full patch in a ` ```diff ` block. The approval dialog may be the only thing on screen, so this list is the record.

Then ask with AskUserQuestion. Use one question per finding, at most 4 questions per call, and more calls if there are more findings:

- question: `#<k> <path>:<line>: <greptile's point, one line>?`
- valid: options `Apply (Recommended)` with the patch as `preview`, then `Skip`.
- disagree / stale: options `Skip (Recommended)` with the reason as its description, then `Apply anyway`. Draft the patch only if they pick it.
- An `Other` answer is a change request. Redo that patch, then ask again for that one finding.

Don't apply anything until all the answers are in.

## 5. Apply, check, commit, push

1. Apply the approved patches in order with `git apply "<patch>"`. If one no longer applies because an earlier patch touched the same lines, redo that edit on the current tree and show the user the new diff before going on.
2. Run the checks for the areas the applied patches touch, using the table in `.claude/skills/pull-request/SKILL.md` step 2. If a check fails, fix it, show the user the extra diff and ask once more. Don't push a red tree.
3. Commit only the files the patches touched (`git add <files>`, never `-A`). Use one commit per approved finding, conventional and lowercase, describing the fix itself: `fix: bound the restore exec with a timeout`. No mention of greptile and no trailers.
4. `git push`. If the remote moved, `git pull --rebase` and retry once. If it fails again, stop and report. Never force-push.
5. **Don't touch the PR on GitHub after pushing.** No reply, no reaction, no thread resolve, no comment.

## State

Keep `$(git rev-parse --absolute-git-dir)/greptile-state.json` as `{ "pr": <n>, "handledIds": [] }`. Reset it when `pr` differs. After step 5, add every finding the user answered (applied or skipped). Leave out a finding you couldn't draft (dirty file), so it comes back next run.

## Report

End with: the PR, the commits pushed (sha plus subject), the skipped findings with their reasons, and any finding left for later. If nothing was approved, nothing gets pushed. Say so.
