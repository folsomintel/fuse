# Release System Overhaul

Date: 2026-06-30
Branch: claude/release-system
Status: approved design, pending implementation plan

## Problem

The fuse monorepo ships a control-plane binary set plus three SDKs (Go, TypeScript,
Python) and an operator CLI, but the release path is partial, ungated, and drifts.

Current state (verified against the repo):

- One human action drives releases: pushing a `v*` git tag fires
  `.github/workflows/release.yml`, which runs two independent, non-gated jobs:
  GoReleaser (binaries) and `publish-ts` (npm). `ci.yml` gates `main` but never publishes.
- The Go SDK rides the root module implicitly via the Go proxy (no `go.mod` of its own).
- The Python SDK `folsom-fuse` and the operator `fuse` CLI are not built, tested,
  or published by any automation.

Concrete defects found:

1. Org mismatch (blocker). git remote is `github.com/andrewn6/fuse`, but `go.mod`,
   GoReleaser, `host-agent/fc-update.sh`, and every SDK URL target `folsomintel/fuse`.
   A tag pushed to andrewn6 never runs Actions on folsomintel.
2. TS User-Agent permanently stale. The build step runs before the version stamp and
   `src/version.ts` is never rewritten, so every build bakes `fuse-ts/0.0.1`.
3. No gating between release jobs. npm can publish while GoReleaser fails (or vice versa).
4. Python SDK has zero publish automation and zero CI; its sdist references a
   nonexistent `LICENSE`; README says `uv add folsom-fuse` but it is not on PyPI.
5. Operator `fuse` CLI is not built by GoReleaser and its version is an un-stampable `const`.
6. Go SDK has no module boundary, so consumers inherit pgx/bubbletea/chi/prometheus/cobra.
7. Version `0.0.1` is hardcoded in 5+ places; only npm is stamped, so all others drift.
8. Loose tag filter (`v*` and `v*.*.*`), dead `dbcheck` ldflags stamp, GoReleaser
   `project_name: orchestrator` (archives named `orchestrator_*`), node 20 (CI) vs 24
   (publish), `-race` on CI but not on the release path.

## Decisions

Settled during brainstorming:

- Scope: fix correctness bugs, fill coverage gaps, kill version drift, and rethink tooling.
- Canonical home: `folsomintel/fuse`. The local git remote is corrected to match.
- Versioning: one unified version number across binaries, all three SDKs, and the CLI.
- Release driver: release-please (conventional-commit driven, single version, release PR).
- Go SDK: own `go.mod`, released by a `sdks/go/vX.Y.Z` tag kept in lockstep with the
  unified version.
- CLI distribution: raw release binaries plus a Homebrew tap.
- `dbcheck`: gets a real `--version` flag so its ldflags stamp becomes live.
- Sequencing: one spec, one phased implementation plan; the Go SDK module split lands
  last because it is the most invasive change.

## Architecture

A single workflow `.github/workflows/release.yml`, `on: push: branches: [main]`,
replaces the tag-triggered model.

Jobs:

- `release-please` runs the release-please action. On ordinary commits it maintains the
  release PR. When that PR merges it creates tag `vX.Y.Z` plus a GitHub Release with
  generated notes, and exposes outputs `release_created` and `version`.
- `verify` (the gate) runs the full check set on the release commit: `go build`,
  `go test -race`, `go vet`, `gofmt`, golangci-lint, TS lint/typecheck/test/build, and
  Python ruff/mypy/pytest, plus `goreleaser check`.
- `binaries`, `publish-npm`, `publish-pypi`, `tag-go-sdk` each declare
  `needs: [release-please, verify]` and `if: needs.release-please.outputs.release_created`,
  so they fire only on an actual release and only after the gate is green.

`ci.yml` stays as the pull-request gate and gains the currently missing Python and
Go-SDK jobs so both are linted/tested on every PR.

This single-workflow shape is the idiomatic release-please setup: it folds "what creates
the tag", "gating", and "no half-published release" into one place. Cross-registry
atomicity (npm + PyPI + GitHub + Go proxy) is not achievable, so a failed publish job is
re-runnable on its own rather than pretended atomic.

## Version: one source of truth, derived everywhere

`.release-please-manifest.json` is the single source of truth. release-please propagates
it into committed files when it opens the release PR; runtime values are derived from
those committed files or from the tag, so no version constant is ever hand-edited.

| Surface          | Committed by release-please | Runtime value derived from                      |
| ---------------- | --------------------------- | ----------------------------------------------- |
| npm @folsom/fuse | package.json version        | version.ts generated from package.json at build |
| PyPI folsom-fuse | pyproject.toml version      | importlib.metadata.version("folsom-fuse")       |
| Go SDK           | tag only                    | runtime/debug.ReadBuildInfo(), fallback "dev"   |
| binaries + CLI   | tag only                    | ldflags -X main.version                         |

Effects:

- TS: `src/version.ts` is generated from `package.json` at build time, so the stale
  User-Agent bug is removed at the root. The `npm version` step in the workflow is
  deleted because `package.json` already carries the right version from the merged PR.
- Python: `_core.py` reads `importlib.metadata.version("folsom-fuse")`; the hardcoded
  `VERSION` constant is removed. pyproject keeps a static version that release-please owns.
- Go SDK: `fuse.go` derives its version from `runtime/debug.ReadBuildInfo()` (the proxy
  fills in the exact module version for `go get ...@vX.Y.Z`), falling back to `"dev"`
  locally; the `const Version` is removed.
- Binaries and CLI: stamped via ldflags from the tag, as the binaries already are.

## Release jobs

- `binaries`: GoReleaser with `project_name: fuse`. Builds orchestrator, dbcheck, fused,
  and the `fuse` CLI. Uploads to the release that release-please already created
  (`release.mode: append`, `changelog.disable: true` so release-please owns the notes).
  Pushes a Homebrew formula to the tap.
- `publish-npm`: build + `npm publish` for `@folsom/fuse` via OIDC trusted publishing.
  No `npm version` step.
- `publish-pypi`: `uv build` + publish `folsom-fuse` via PyPI trusted publishing (OIDC).
- `tag-go-sdk`: create and push `sdks/go/vX.Y.Z` at the release commit, then warm the
  proxy with `go list -m github.com/folsomintel/fuse/sdks/go@vX.Y.Z`.

## Go SDK module split

The one structurally invasive change. Lands last in the plan.

- New `sdks/go/go.mod` (`module github.com/folsomintel/fuse/sdks/go`, stdlib-only, so an
  empty require block). This sheds pgx/bubbletea/chi/prometheus/cobra from SDK consumers.
- The in-repo CLI imports this module. A root `go.work` listing `.` and `sdks/go` makes
  local builds and GoReleaser use the in-tree SDK, so the released CLI embeds the local
  code and there is no chicken-and-egg between the CLI build and the SDK tag.
- Consumers receive the SDK through the lockstep `sdks/go/vX.Y.Z` tag (the `tag-go-sdk`
  job). The unified version number is preserved; only the tag ref name differs.
- CI builds and tests both modules.

If the split proves messier than its worth during implementation, every other section
works unchanged with the SDK left in the root module. The split is in scope by decision.

## GoReleaser changes

- `project_name: orchestrator` becomes `fuse`, so archives are `fuse_*`. The asset-name
  consumers in `host-agent/fc-update.sh` and `host-agent/fc-agent.sh` are updated to match.
- Add a `fuse` CLI build for `./cli`; convert `cli/root.go` `const version` to
  `var version = "dev"` so ldflags can stamp it.
- Add a `brews:` block targeting `folsomintel/homebrew-fuse`
  (`brew install folsomintel/fuse/fuse`).
- `dbcheck`: add `var version` and a `--version` flag to `cmd/dbcheck/main.go` so the
  existing `-X main.version` stamp becomes live, consistent with orchestrator and fused.
- `release.mode: append` and `changelog.disable: true`.

Note: `host-agent/fc-update.sh` resolves the control-plane tarball as
`orchestrator_Linux_<arch>.tar.gz`. Renaming `project_name` to `fuse` changes that asset
name to `fuse_Linux_<arch>.tar.gz`, so the updater and `fc-agent.sh` references must be
updated in the same change to avoid breaking host self-update.

## Org fix and hygiene

- Remote: `git remote set-url origin https://github.com/folsomintel/fuse.git`. Everything
  else already targets folsomintel; this is the one local mismatch.
- Tag handling: strict semver via release-please; the loose `v*` filter is gone.
- Align node to 24 in both CI and publish (publish needs npm >= 11.5.1).
- Add `sdks/python/LICENSE` (the sdist include list references a file that does not exist).
- Delete the stray `sdks/python/main.py` `uv init` stub.
- Reconcile `sdks/python/.python-version` (3.14) with `requires-python` and classifiers.

## External prerequisites (out-of-band, not code)

These cannot be done from the repo and gate the first green release:

1. `folsomintel/fuse` repo exists and is the push target (the remote fix assumes this).
2. npm trusted publisher for `@folsom/fuse` pointed at `folsomintel/fuse` and the new
   workflow filename.
3. PyPI project `folsom-fuse` plus a trusted publisher (OIDC) pointed at `folsomintel/fuse`
   and the workflow (and environment if one is used).
4. Homebrew tap repo `folsomintel/homebrew-fuse` plus a `HOMEBREW_TAP_TOKEN` secret
   (PAT or deploy key with write access to the tap).
5. release-please token: release PRs opened with the default `GITHUB_TOKEN` do not trigger
   CI. Use a PAT (or the release-please GitHub App) so the release PR runs `ci.yml`.

## Phasing

The implementation plan orders the work so a working, gated release exists before the
riskiest change:

1. Pipeline rework: the single release-please workflow, `verify` gate, strict tagging,
   org remote fix, and the existing-surface fixes (project_name, node alignment, `-race`).
2. Drift kill: derive versions everywhere (generated `version.ts`, importlib.metadata,
   ReadBuildInfo, CLI `var`); delete hardcoded constants.
3. Coverage: Python publish + CI jobs, `sdks/python/LICENSE`, remove `main.py`,
   `.python-version` reconcile; ship the `fuse` CLI via GoReleaser; add the Homebrew tap;
   `dbcheck --version`.
4. Go SDK module split (last): `sdks/go/go.mod`, root `go.work`, `tag-go-sdk` job,
   both-module CI.

## Out of scope

- Per-component independent version numbers (rejected in favor of one unified version).
- Signing or notarization of binaries.
- Container image publishing.
- Changing the existing conventional-commit convention (already in use; release-please
  keys off it directly).
