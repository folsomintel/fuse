---
name: file-issue
description: File a GitHub issue (or an epic with sub-issues) on folsomintel/fuse in the repo's short format - `area: what to do` title, optional one-line context, `## files` and `## do`. Use when asked to "file an issue", "open a ticket", "make issues for this", or to break work into an epic.
argument-hint: "<what the issue is about>"
---

Format (from CLAUDE.md). No background essays, competitor comparisons, or long acceptance sections.

```markdown
Part of #N            <- optional one line: "Part of #N", "Depends on #N", or one sentence of context

## files
- internal/orchestrator/migrate.go: pickStandby
- sdks/{go,typescript,python,rust}: MigrateRequest

## do
- pick the standby by free capacity instead of first-registered
- 409 when no host fits, mapped in classifyFleetError
- test in each sdk
```

- **Title:** `area: what to do`, all lowercase, imperative. For example `migrate: pick the checkpoint standby by capacity`. The area is the subsystem (`api`, `cli`, `sdk`, `fusefile`, `fc-agent`, `qemu-agent`, `egress`, `hitl`, `docs`, `ops`, `migrate`, `fork`, ...).
- **`## files`:** paths plus the function or type to touch, never line numbers. Look them up in the code first. Don't guess paths.
- **`## do`:** bullets of the work. If the work touches an sdk's api surface, include "all four sdks, with a test in each". If it changes something user-visible, include the docs page.
- **Epic:** the body is one line of context plus a `- [ ] #N` checklist of sub-issues. File the sub-issues first, then the epic, then edit each sub-issue's first line to `Part of #<epic>`.
- **Labels:** `bug` or `enhancement` when obvious, `epic:` for epics, and an existing area label if one fits. Never create labels.

Steps:
1. Check for a duplicate: `gh issue list --search "<keywords>" --state all --limit 20`.
2. Draft it and show the user the title and body before filing. Filing an issue is visible to others, so confirm unless the user already said to file it.
3. File it: `gh issue create --title "..." --body-file <tmpfile> [--label ...]`.
4. Return the issue URL(s).
