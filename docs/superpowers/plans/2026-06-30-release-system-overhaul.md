# Release System Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the partial, ungated, drift-prone release path with a single release-please driven pipeline that publishes binaries, the operator CLI, and all three SDKs from one unified version, with no hardcoded version constants.

**Architecture:** One workflow on `push: main` runs release-please; when its release PR merges it tags `vX.Y.Z` and the gated `verify`, `binaries`, `publish-npm`, `publish-pypi`, and `tag-go-sdk` jobs publish each surface. Versions are derived (generated at build or read from package metadata), never hand-edited. The Go SDK becomes its own module released by a lockstep `sdks/go/vX.Y.Z` tag.

**Tech Stack:** GitHub Actions, release-please (googleapis/release-please-action@v4), GoReleaser v2, Go 1.26, npm + tsup, uv + hatchling, OIDC trusted publishing (npm + PyPI).

## Global Constraints

- Canonical repo: `github.com/folsomintel/fuse`. Module path, GoReleaser owner, and all package URLs already target it; the local git remote is the only mismatch.
- One unified version number across binaries, CLI, Go SDK, TS SDK, Python SDK.
- npm package: `@folsom/fuse`. PyPI distribution: `folsom-fuse`. Go SDK import: `github.com/folsomintel/fuse/sdks/go`.
- Node 24 everywhere (publish needs npm >= 11.5.1).
- No hardcoded version constants: TS generated from package.json, Python via `importlib.metadata`, Go SDK via `runtime/debug.ReadBuildInfo`, binaries + CLI via ldflags `-X main.version`.
- Comments lowercase. No emojis. No em dashes.
- Conventional commits (feat/fix/docs/refactor/chore/test/style), lowercase.

---

## Phase 1: Pipeline rework, org fix, surface fixes

### Task 1: release-please config + git remote fix

**Files:**

- Create: `release-please-config.json`
- Create: `.release-please-manifest.json`

- [ ] **Step 1: Fix the local git remote (no commit, local-only)**

Run: `git remote set-url origin https://github.com/folsomintel/fuse.git && git remote -v`
Expected: both fetch and push lines show `github.com/folsomintel/fuse.git`.

- [ ] **Step 2: Create the release-please manifest with the current version**

`.release-please-manifest.json`:

```json
{
  ".": "0.0.1"
}
```

- [ ] **Step 3: Create the release-please config (one unified version, updates both manifests)**

`release-please-config.json`:

```json
{
  "$schema": "https://raw.githubusercontent.com/googleapis/release-please/main/schemas/config.json",
  "separate-pull-requests": false,
  "include-component-in-tag": false,
  "packages": {
    ".": {
      "release-type": "simple",
      "package-name": "fuse",
      "extra-files": [
        {
          "type": "json",
          "path": "sdks/typescript/package.json",
          "jsonpath": "$.version"
        },
        {
          "type": "toml",
          "path": "sdks/python/pyproject.toml",
          "jsonpath": "$.project.version"
        }
      ]
    }
  }
}
```

Note: `release-type: simple` tracks the version in `.release-please-manifest.json` and writes a root `CHANGELOG.md`. `include-component-in-tag: false` makes the root tag `vX.Y.Z` (not `fuse-vX.Y.Z`). The `extra-files` typed updaters rewrite the npm and PyPI manifest versions to the same number. version.ts (TS), the Go SDK, and the CLI are derived in Phase 2, so they are not listed here.

- [ ] **Step 4: Validate the JSON is well-formed**

Run: `python3 -c "import json; json.load(open('release-please-config.json')); json.load(open('.release-please-manifest.json')); print('ok')"`
Expected: `ok`

- [ ] **Step 5: Commit**

```bash
git add release-please-config.json .release-please-manifest.json
git commit -m "build: add release-please config for unified versioning"
```

### Task 2: rewrite the release workflow

**Files:**

- Modify (full rewrite): `.github/workflows/release.yml`

**Interfaces:**

- Produces: job `release-please` with outputs `release_created`, `tag_name`, `version`. Later tasks add jobs that declare `needs: [release-please, verify]` and `if: needs.release-please.outputs.release_created == 'true'`.

- [ ] **Step 1: Replace release.yml with the release-please pipeline**

`.github/workflows/release.yml`:

```yaml
name: Release

# release-please watches main. When its release PR merges, it tags vX.Y.Z and
# creates the GitHub release; the gated jobs below then publish each surface.
on:
  push:
    branches: [main]

permissions:
  contents: write
  pull-requests: write

jobs:
  release-please:
    runs-on: ubuntu-latest
    outputs:
      release_created: ${{ steps.rp.outputs.release_created }}
      tag_name: ${{ steps.rp.outputs.tag_name }}
      version: ${{ steps.rp.outputs.version }}
    steps:
      # a PAT (or the release-please app) is required so the release PR triggers
      # ci.yml; the default GITHUB_TOKEN would not trigger downstream workflows.
      - uses: googleapis/release-please-action@v4
        id: rp
        with:
          token: ${{ secrets.RELEASE_PLEASE_TOKEN }}

  # full gate, re-run on the exact release commit (race detector + all linters).
  verify:
    needs: release-please
    if: needs.release-please.outputs.release_created == 'true'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: test -z "$(gofmt -l .)"
      - run: go vet ./...
      - run: go build ./...
      - run: go test ./... -race -count=1
      - uses: golangci/golangci-lint-action@v7
        with:
          version: v2.12.2
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: check
      - name: TS SDK checks
        working-directory: sdks/typescript
        run: |
          npm ci
          npm run lint
          npm run test
          npm run build

  binaries:
    needs: [release-please, verify]
    if: needs.release-please.outputs.release_created == 'true'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

  publish-npm:
    name: Publish TS SDK to npm
    needs: [release-please, verify]
    if: needs.release-please.outputs.release_created == 'true'
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write # oidc: npm trusted publishing + provenance
    defaults:
      run:
        working-directory: sdks/typescript
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 24
          registry-url: https://registry.npmjs.org
      - run: npm install -g npm@latest
      - run: npm ci
      - run: npm run lint
      - run: npm run test
      - run: npm run build
      # no `npm version` step: package.json was bumped in the merged release PR.
      - run: npm publish
```

- [ ] **Step 2: Lint the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Verify the release-please action output names**

The action's exact output names can vary by version. Confirm `release_created`, `tag_name`, and `version` are the top-level outputs for a single-package manifest at `release-please-action@v4` (action README / a no-op dry run). If they differ (e.g. `releases_created`), update the `outputs:` block and all `if:` conditions to match.
Expected: output names confirmed and consistent across the file.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: replace tag-triggered release with gated release-please pipeline"
```

### Task 3: GoReleaser project rename + release mode + host script asset names

**Files:**

- Modify: `.goreleaser.yaml:5` (project_name), `.goreleaser.yaml:101-126` (changelog), `.goreleaser.yaml:128-132` (release)
- Modify: `host-agent/fc-update.sh:134`
- Modify: `host-agent/fc-agent.sh:179-180`

- [ ] **Step 1: Rename the GoReleaser project to fuse**

In `.goreleaser.yaml`, change line 5 from `project_name: orchestrator` to:

```yaml
project_name: fuse
```

- [ ] **Step 2: Let release-please own the changelog and the release body**

In `.goreleaser.yaml`, replace the entire `changelog:` block (lines 101-126) with:

```yaml
changelog:
  disable: true
```

And replace the `release:` block (lines 128-132) with:

```yaml
release:
  github:
    owner: folsomintel
    name: fuse
  # release-please already created the release + notes for this tag; attach
  # artifacts to it instead of creating a second release.
  mode: keep-existing
```

- [ ] **Step 3: Update the host updater tarball name**

In `host-agent/fc-update.sh` line 134, change:

```bash
    TARBALL="orchestrator_Linux_${ASSET_ARCH}.tar.gz"
```

to:

```bash
    TARBALL="fuse_Linux_${ASSET_ARCH}.tar.gz"
```

The extraction (`tar -xzf "$TGZ" -C "$EXDIR" orchestrator`) is unchanged: the binary inside is still named `orchestrator`, only the archive name changed.

- [ ] **Step 4: Update the host installer tarball name**

In `host-agent/fc-agent.sh`, update the two references (lines 179-180) from `orchestrator_Linux_${ASSET_ARCH}.tar.gz` to `fuse_Linux_${ASSET_ARCH}.tar.gz`. Leave the `tar -xzf ... orchestrator` extraction unchanged.

- [ ] **Step 5: Validate the GoReleaser config and a snapshot build**

Run: `goreleaser check && goreleaser build --snapshot --clean --single-target`
Expected: `check` passes; a snapshot build produces `dist/` binaries with no error. (If goreleaser is not installed: `go install github.com/goreleaser/goreleaser/v2@latest`.)

- [ ] **Step 6: Confirm the archive name template now yields fuse_***

Run: `grep -n "fuse_Linux" host-agent/fc-update.sh host-agent/fc-agent.sh`
Expected: both files reference `fuse_Linux_${ASSET_ARCH}.tar.gz`.

- [ ] **Step 7: Commit**

```bash
git add .goreleaser.yaml host-agent/fc-update.sh host-agent/fc-agent.sh
git commit -m "build: rename goreleaser project to fuse and align host asset names"
```

### Task 4: align CI node version

**Files:**

- Modify: `.github/workflows/ci.yml:103`

- [ ] **Step 1: Bump the sdk-ts job to node 24**

In `.github/workflows/ci.yml`, change line 103 from `node-version: 20` to:

```yaml
node-version: 24
```

- [ ] **Step 2: Validate the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: align TS SDK test node version to 24"
```

---

## Phase 2: Kill version drift

### Task 5: generate TS version.ts from package.json

**Files:**

- Create: `sdks/typescript/scripts/gen-version.mjs`
- Modify: `sdks/typescript/package.json:47-55` (scripts)
- Create: `sdks/typescript/test/version.test.ts`
- Modify: `sdks/typescript/src/version.ts` (becomes generated output, regenerated by prebuild)

**Interfaces:**

- Produces: `src/version.ts` exporting `VERSION` equal to `package.json` version, regenerated by the `prebuild` npm lifecycle hook before every `build`.

- [ ] **Step 1: Write the failing test**

`sdks/typescript/test/version.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { VERSION } from "../src/version.js";

describe("version", () => {
  it("matches package.json", () => {
    const pkg = JSON.parse(
      readFileSync(new URL("../package.json", import.meta.url), "utf8"),
    );
    expect(VERSION).toBe(pkg.version);
  });
});
```

- [ ] **Step 2: Make the test fail by desyncing version.ts**

Temporarily set `package.json` version to `0.0.2` (do not commit), then run:
Run: `cd sdks/typescript && npm run test -- version`
Expected: FAIL (`VERSION` is `0.0.1`, package.json is `0.0.2`). Revert package.json to `0.0.1` afterward.

- [ ] **Step 3: Write the generator**

`sdks/typescript/scripts/gen-version.mjs`:

```js
// regenerate src/version.ts from package.json so the runtime user-agent version
// always matches the published package version. runs as the prebuild hook.
import { readFileSync, writeFileSync } from "node:fs";

const pkg = JSON.parse(
  readFileSync(new URL("../package.json", import.meta.url), "utf8"),
);
const body = `// generated by scripts/gen-version.mjs. do not edit.
export const VERSION = ${JSON.stringify(pkg.version)};
`;
writeFileSync(new URL("../src/version.ts", import.meta.url), body);
```

- [ ] **Step 4: Wire the prebuild + pretest hooks**

In `sdks/typescript/package.json` scripts, add `prebuild` and `pretest` so `version.ts` is regenerated before build and before tests:

```json
  "scripts": {
    "gen:version": "node scripts/gen-version.mjs",
    "prebuild": "node scripts/gen-version.mjs",
    "pretest": "node scripts/gen-version.mjs",
    "build": "tsup",
    "typecheck": "tsc --noEmit",
    "test": "vitest run",
    "test:watch": "vitest",
    "lint": "prettier --check . && tsc --noEmit",
    "format": "prettier --write .",
    "prepublishOnly": "npm run build"
  },
```

- [ ] **Step 5: Regenerate and run the test**

Run: `cd sdks/typescript && npm run gen:version && npm run test -- version`
Expected: PASS. `src/version.ts` now carries the generated banner comment.

- [ ] **Step 6: Commit**

```bash
git add sdks/typescript/scripts/gen-version.mjs sdks/typescript/package.json sdks/typescript/src/version.ts sdks/typescript/test/version.test.ts
git commit -m "fix: generate TS SDK version from package.json to kill user-agent drift"
```

### Task 6: derive Python version from package metadata

**Files:**

- Modify: `sdks/python/src/fuse/_core.py:8-9`
- Create: `sdks/python/tests/test_version.py`

- [ ] **Step 1: Write the failing test**

`sdks/python/tests/test_version.py`:

```python
from importlib.metadata import version

import fuse


def test_version_matches_installed_metadata() -> None:
    assert fuse.VERSION == version("folsom-fuse")
    assert fuse.VERSION != "0.0.1" or version("folsom-fuse") == "0.0.1"
```

- [ ] **Step 2: Run it against the current hardcoded constant**

Run: `cd sdks/python && uv sync && uv run pytest tests/test_version.py -v`
Expected: PASS only by coincidence today (both are `0.0.1`). The real fix is removing the hardcode so it cannot drift; proceed regardless.

- [ ] **Step 3: Replace the hardcoded VERSION with metadata lookup**

In `sdks/python/src/fuse/_core.py`, replace lines 8-9:

```python
# sdk version, reported in the default user agent.
VERSION = "0.0.1"
```

with:

```python
# sdk version, read from installed package metadata so it never drifts from
# pyproject. falls back when running from an uninstalled source tree.
from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _pkg_version

try:
    VERSION = _pkg_version("folsom-fuse")
except PackageNotFoundError:
    VERSION = "0+unknown"
```

Keep the existing `from __future__ import annotations` as the first line; place these imports with the other top-of-file imports if the linter (isort via ruff) requires it. Run `uv run ruff check --fix src` to auto-sort.

- [ ] **Step 4: Run the test**

Run: `cd sdks/python && uv run ruff check src && uv run pytest tests/test_version.py -v`
Expected: PASS, lint clean.

- [ ] **Step 5: Commit**

```bash
git add sdks/python/src/fuse/_core.py sdks/python/tests/test_version.py
git commit -m "fix: derive Python SDK version from package metadata"
```

### Task 7: derive Go SDK version from build info

**Files:**

- Modify: `sdks/go/fuse.go:10-11` and `sdks/go/fuse.go:70`
- Create: `sdks/go/version_test.go`

**Interfaces:**

- Produces: `func sdkVersion() string` in package `fuse`, returning the module version of `github.com/folsomintel/fuse/sdks/go` from build info, or `"dev"` when unavailable. The default user agent becomes `"fuse-go/" + sdkVersion()`.

- [ ] **Step 1: Write the failing test**

`sdks/go/version_test.go`:

```go
package fuse

import "testing"

// under `go test` the module is the main module with version "(devel)", so
// sdkVersion falls back to "dev". this pins the fallback contract.
func TestSDKVersionFallback(t *testing.T) {
	if got := sdkVersion(); got != "dev" {
		t.Fatalf("sdkVersion() = %q, want %q in test context", got, "dev")
	}
}
```

- [ ] **Step 2: Run it (fails: sdkVersion undefined)**

Run: `cd sdks/go && go test ./... -run TestSDKVersion`
Expected: FAIL, `undefined: sdkVersion`.

- [ ] **Step 3: Replace the const with a build-info lookup**

In `sdks/go/fuse.go`, replace lines 10-11:

```go
// Version is the SDK version, reported in the default user agent.
const Version = "0.0.1"
```

with:

```go
// sdkVersion reports the module version of this SDK as recorded in the consuming
// binary's build info. it is empty/"(devel)" in local builds and tests, where it
// falls back to "dev". stamped automatically by the go proxy for tagged installs.
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	const modPath = "github.com/folsomintel/fuse/sdks/go"
	if info.Main.Path == modPath && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	for _, d := range info.Deps {
		if d.Path == modPath && d.Version != "" && d.Version != "(devel)" {
			return strings.TrimPrefix(d.Version, "v")
		}
	}
	return "dev"
}
```

Add `"runtime/debug"` and `"strings"` to the import block (lines 3-8).

- [ ] **Step 4: Update the user-agent default**

In `sdks/go/fuse.go` line 70, change:

```go
		userAgent: "fuse-go/" + Version,
```

to:

```go
		userAgent: "fuse-go/" + sdkVersion(),
```

- [ ] **Step 5: Run the test**

Run: `cd sdks/go && go vet ./... && go test ./... -run TestSDKVersion -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sdks/go/fuse.go sdks/go/version_test.go
git commit -m "fix: derive Go SDK version from build info"
```

### Task 8: make the CLI version stampable

**Files:**

- Modify: `cli/root.go:12-13`, `cli/root.go:48-58` (set root.Version)

- [ ] **Step 1: Convert the const to a stampable var**

In `cli/root.go`, replace lines 12-13:

```go
// version is the cli version, also sent as the user agent.
const version = "0.0.1"
```

with:

```go
// version is the cli version, also sent as the user agent. stamped at release
// time via -ldflags "-X main.version=...".
var version = "dev"
```

- [ ] **Step 2: Expose it via `fuse --version`**

In `cli/root.go`, add `Version: version,` to the `&cobra.Command{...}` literal in `newRootCmd` (around line 49-58), so fang renders `--version`:

```go
	root := &cobra.Command{
		Use:     "fuse",
		Version: version,
		Short:   "Manage Fuse hosts, environments, snapshots, and API keys",
```

- [ ] **Step 3: Build and check the flag**

Run: `go build -o /tmp/fuse-cli ./cli && /tmp/fuse-cli --version`
Expected: prints `fuse version dev` (or similar). With ldflags it would print the tag.

- [ ] **Step 4: Commit**

```bash
git add cli/root.go
git commit -m "fix: make CLI version a stampable var"
```

---

## Phase 3: Coverage (Python publish + CI, CLI release, brew, dbcheck)

### Task 9: Python packaging hygiene

**Files:**

- Create: `sdks/python/LICENSE`
- Delete: `sdks/python/main.py`
- Modify: `sdks/python/.python-version`

- [ ] **Step 1: Add the SDK LICENSE the sdist references**

Run: `cp LICENSE sdks/python/LICENSE`
Expected: `sdks/python/LICENSE` exists (matches the `include = [..., "LICENSE"]` in pyproject).

- [ ] **Step 2: Remove the stray uv-init stub**

Run: `git rm sdks/python/main.py`
Expected: file deleted.

- [ ] **Step 3: Reconcile the pinned python version**

Set `sdks/python/.python-version` to a version inside the supported range and classifiers:

```
3.13
```

(The classifiers list 3.9-3.13 and `requires-python = ">=3.9"`; pinning 3.13 removes the 3.14 mismatch. Do not widen `requires-python`.)

- [ ] **Step 4: Verify the build now ships the license**

Run: `cd sdks/python && uv build && python3 -c "import tarfile,glob; t=tarfile.open(glob.glob('dist/*.tar.gz')[0]); names=t.getnames(); assert any(n.endswith('/LICENSE') for n in names), names; print('license present')"`
Expected: `license present`.

- [ ] **Step 5: Commit**

```bash
git add sdks/python/LICENSE sdks/python/.python-version
git rm --cached sdks/python/main.py 2>/dev/null; git add -A sdks/python
git commit -m "build: add Python SDK license, drop init stub, pin python 3.13"
```

### Task 10: Python CI job

**Files:**

- Modify: `.github/workflows/ci.yml` (append a job after `sdk-ts`)

- [ ] **Step 1: Add the sdk-py job**

Append to `.github/workflows/ci.yml`:

```yaml
# Python SDK (sdks/python): lint, typecheck, test via uv. Independent of the
# Go and TS jobs.
sdk-py:
  name: Python SDK
  runs-on: ubuntu-latest
  defaults:
    run:
      working-directory: sdks/python
  steps:
    - uses: actions/checkout@v4
    - name: Set up uv
      uses: astral-sh/setup-uv@v5
      with:
        python-version: "3.13"
    - run: uv sync
    - name: Lint
      run: uv run ruff check src tests
    - name: Typecheck
      run: uv run mypy
    - name: Test
      run: uv run pytest
```

- [ ] **Step 2: Validate the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Run the same checks locally to confirm they pass**

Run: `cd sdks/python && uv sync && uv run ruff check src tests && uv run mypy && uv run pytest`
Expected: all green (tests include `test_version.py` from Task 6).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add Python SDK lint/typecheck/test job"
```

### Task 11: Python publish job

**Files:**

- Modify: `.github/workflows/release.yml` (append a job)

- [ ] **Step 1: Add the publish-pypi job**

Append to `.github/workflows/release.yml`:

```yaml
publish-pypi:
  name: Publish Python SDK to PyPI
  needs: [release-please, verify]
  if: needs.release-please.outputs.release_created == 'true'
  runs-on: ubuntu-latest
  permissions:
    contents: read
    id-token: write # oidc: pypi trusted publishing
  defaults:
    run:
      working-directory: sdks/python
  steps:
    - uses: actions/checkout@v4
    - uses: astral-sh/setup-uv@v5
      with:
        python-version: "3.13"
    # pyproject version was bumped in the merged release PR (extra-files).
    - run: uv build
    - run: uv publish
```

Note: `uv publish` uses PyPI trusted publishing (OIDC) when run in Actions with `id-token: write` and no token configured. The PyPI trusted publisher must be configured out-of-band (see prerequisites).

- [ ] **Step 2: Validate the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Confirm a build artifact is produced locally**

Run: `cd sdks/python && uv build && ls dist`
Expected: a `.whl` and a `.tar.gz` named `folsom_fuse-0.0.1*`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: publish Python SDK to PyPI on release"
```

### Task 12: dbcheck --version

**Files:**

- Modify: `cmd/dbcheck/main.go:26-51`

- [ ] **Step 1: Add a stampable version var and flag**

In `cmd/dbcheck/main.go`, after the `package main` line (line 26) and its doc comment, add:

```go
// version is stamped at release time via -ldflags "-X main.version=...".
var version = "dev"
```

Then inside `main()` (after line 41-42 flag declarations), add:

```go
	showVersion := flag.Bool("version", false, "print version and exit")
```

And immediately after `flag.Parse()` (line 47), add:

```go
	if *showVersion {
		fmt.Println(version)
		return
	}
```

- [ ] **Step 2: Build and check the flag**

Run: `go build -o /tmp/dbcheck ./cmd/dbcheck && /tmp/dbcheck --version`
Expected: prints `dev`. The existing `-X main.version` ldflags in `.goreleaser.yaml:42` now has a live target.

- [ ] **Step 3: Commit**

```bash
git add cmd/dbcheck/main.go
git commit -m "feat: add dbcheck --version flag"
```

### Task 13: ship the CLI via GoReleaser + Homebrew tap

**Files:**

- Modify: `.goreleaser.yaml` (add a build, an archive, a brews block)
- Modify: `.github/workflows/release.yml` (binaries job env)

**Interfaces:**

- Consumes: `cli/root.go` `var version` from Task 8 (ldflags target `main.version`).

- [ ] **Step 1: Add the CLI build**

In `.goreleaser.yaml`, add a fourth build under `builds:` (after the `fused` build, before `archives:`):

```yaml
- id: fuse-cli
  main: ./cli
  binary: fuse
  env:
    - CGO_ENABLED=0
  goos:
    - linux
    - darwin
  goarch:
    - amd64
    - arm64
  ldflags:
    - -s -w -X main.version={{.Version}}
```

- [ ] **Step 2: Add a CLI archive**

In `.goreleaser.yaml` under `archives:`, add a third archive (after the `fused` archive):

```yaml
- id: cli
  ids:
    - fuse-cli
  formats:
    - tar.gz
  name_template: >-
    fuse-cli_
    {{- title .Os }}_
    {{- if eq .Arch "amd64" }}x86_64
    {{- else if eq .Arch "386" }}i386
    {{- else }}{{ .Arch }}{{ end }}
```

- [ ] **Step 3: Add the Homebrew tap block**

In `.goreleaser.yaml`, add a top-level `brews:` block (after `checksum:`):

```yaml
brews:
  - name: fuse
    ids:
      - cli
    repository:
      owner: folsomintel
      name: homebrew-fuse
      token: "{{ .Env.HOMEBREW_TAP_TOKEN }}"
    directory: Formula
    homepage: "https://github.com/folsomintel/fuse"
    description: "Operator CLI for the Fuse microVM control plane"
    license: "MIT"
    install: |
      bin.install "fuse"
    test: |
      system "#{bin}/fuse", "--version"
```

- [ ] **Step 4: Pass the tap token to the binaries job**

In `.github/workflows/release.yml`, add `HOMEBREW_TAP_TOKEN` to the GoReleaser step env in the `binaries` job:

```yaml
env:
  GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  HOMEBREW_TAP_TOKEN: ${{ secrets.HOMEBREW_TAP_TOKEN }}
```

- [ ] **Step 5: Validate config + snapshot build of the CLI**

Run: `goreleaser check && goreleaser build --snapshot --clean --id fuse-cli --single-target`
Expected: `check` passes; a `fuse` binary is built. (`brews` is not exercised by snapshot, which is expected.)

- [ ] **Step 6: Commit**

```bash
git add .goreleaser.yaml .github/workflows/release.yml
git commit -m "feat: release the operator fuse CLI via goreleaser and a homebrew tap"
```

---

## Phase 4: Go SDK module split (most invasive, lands last)

### Task 14: give sdks/go its own module + root workspace

**Files:**

- Create: `sdks/go/go.mod`
- Create: `go.work`
- Modify: `go.mod` (no version files needed; the directory leaving the root module is automatic once it has its own go.mod)

**Interfaces:**

- Produces: module `github.com/folsomintel/fuse/sdks/go` (stdlib-only). The root module keeps importing it; the `go.work` makes local builds resolve the in-tree copy.

- [ ] **Step 1: Determine the SDK's minimum Go version**

The SDK imports only stdlib. Use a conservative floor so consumers are not forced to 1.26.
Run: `cd sdks/go && go list -deps . 2>/dev/null | grep -v '^github.com/folsomintel' | grep '\.' || echo "stdlib only"`
Expected: `stdlib only` (no third-party deps).

- [ ] **Step 2: Create the SDK module file**

`sdks/go/go.mod`:

```
module github.com/folsomintel/fuse/sdks/go

go 1.23
```

- [ ] **Step 3: Create the workspace so the CLI builds against the local SDK**

`go.work`:

```
go 1.26.1

use (
	.
	./sdks/go
)
```

- [ ] **Step 4: Tidy both modules and build everything**

Run: `go work sync && go build ./... && (cd sdks/go && go build ./... && go vet ./...)`
Expected: both modules build; the CLI (root module) still resolves `github.com/folsomintel/fuse/sdks/go` via the workspace.

- [ ] **Step 5: Run all tests in both modules**

Run: `go test ./... -count=1 && (cd sdks/go && go test ./... -count=1)`
Expected: PASS in both.

- [ ] **Step 6: Confirm the SDK module is dependency-free**

Run: `cd sdks/go && test ! -s go.sum && echo "no external deps"`
Expected: `no external deps` (an empty or absent go.sum means stdlib-only).

- [ ] **Step 7: Commit**

```bash
git add sdks/go/go.mod go.work go.mod go.sum
git commit -m "refactor: split sdks/go into its own module with a go.work workspace"
```

### Task 15: CI builds and tests the SDK module standalone

**Files:**

- Modify: `.github/workflows/ci.yml` (append a job); `.github/workflows/release.yml` verify job (add SDK module build)

- [ ] **Step 1: Add the sdk-go job to ci.yml**

Append to `.github/workflows/ci.yml`:

```yaml
# Go SDK as a standalone module: builds with GOWORK=off so the workspace does
# not mask a missing/extra dependency a real consumer would hit.
sdk-go:
  name: Go SDK
  runs-on: ubuntu-latest
  env:
    GOWORK: "off"
  defaults:
    run:
      working-directory: sdks/go
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version-file: sdks/go/go.mod
    - run: go build ./...
    - run: go vet ./...
    - run: go test ./... -race -count=1
```

- [ ] **Step 2: Validate the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Reproduce the standalone build locally**

Run: `cd sdks/go && GOWORK=off go build ./... && GOWORK=off go test ./... -count=1`
Expected: PASS without the workspace (proves no hidden root-module dependency).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: build and test the Go SDK as a standalone module"
```

### Task 16: tag the Go SDK module on release

**Files:**

- Modify: `.github/workflows/release.yml` (append a job)

- [ ] **Step 1: Add the tag-go-sdk job**

Append to `.github/workflows/release.yml`:

```yaml
tag-go-sdk:
  name: Tag Go SDK module
  needs: [release-please, verify]
  if: needs.release-please.outputs.release_created == 'true'
  runs-on: ubuntu-latest
  permissions:
    contents: write
  steps:
    - uses: actions/checkout@v4
      with:
        fetch-depth: 0
    # nested go module needs a path-prefixed tag for the proxy. keep it in
    # lockstep with the unified version (release-please already pushed vX.Y.Z).
    - name: Push lockstep module tag
      env:
        VERSION: ${{ needs.release-please.outputs.version }}
      run: |
        git config user.name "github-actions[bot]"
        git config user.email "github-actions[bot]@users.noreply.github.com"
        git tag "sdks/go/v${VERSION}" "v${VERSION}"
        git push origin "sdks/go/v${VERSION}"
    - uses: actions/setup-go@v5
      with:
        go-version-file: sdks/go/go.mod
    - name: Warm the module proxy
      env:
        VERSION: ${{ needs.release-please.outputs.version }}
        GOPROXY: proxy.golang.org
      run: go list -m "github.com/folsomintel/fuse/sdks/go@v${VERSION}" || true
```

Note: `tag_name` from release-please is `vX.Y.Z`; `version` is `X.Y.Z`. The module tag must be `sdks/go/vX.Y.Z`, hence `v${VERSION}`.

- [ ] **Step 2: Validate the workflow YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('ok')"`
Expected: `ok`

- [ ] **Step 3: Final whole-repo verification**

Run: `go build ./... && go test ./... -count=1 && (cd sdks/go && GOWORK=off go test ./... -count=1) && (cd sdks/typescript && npm ci && npm run build && npm run test) && (cd sdks/python && uv sync && uv run pytest) && goreleaser check`
Expected: every command green.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: tag the Go SDK module in lockstep on release"
```

---

## External prerequisites (out-of-band, gate the first green release)

These cannot be done from the repo. The code lands without them; the first real release stays red until they exist.

1. `folsomintel/fuse` repository exists and is the push target for `origin` (Task 1 Step 1 assumes this).
2. **npm** trusted publisher for `@folsom/fuse` -> repo `folsomintel/fuse`, workflow `.github/workflows/release.yml`, job `publish-npm`.
3. **PyPI** project `folsom-fuse` + a trusted publisher (OIDC) -> repo `folsomintel/fuse`, workflow `.github/workflows/release.yml`, job `publish-pypi`.
4. **Homebrew tap** repo `folsomintel/homebrew-fuse` + a `HOMEBREW_TAP_TOKEN` secret (PAT or fine-grained token with write to the tap).
5. **release-please token**: a `RELEASE_PLEASE_TOKEN` secret (PAT or the release-please GitHub App) so the release PR triggers `ci.yml`. The default `GITHUB_TOKEN` would not.

## Self-review notes

- Spec coverage: every spec section maps to a task. Pipeline/gating -> Task 2; version derivation -> Tasks 5-8; Python coverage -> Tasks 9-11; CLI + brew + dbcheck -> Tasks 12-13; Go module split -> Tasks 14-16; org/hygiene -> Tasks 1, 3, 4, 9.
- Symbol consistency: `sdkVersion()` (Go, Task 7) and `VERSION` (TS Task 5, Python Task 6) are referenced consistently; release.yml job/output names (`release_created`, `version`, jobs `release-please`/`verify`/`binaries`/`publish-npm`/`publish-pypi`/`tag-go-sdk`) are stable across Tasks 2, 11, 13, 16.
- Known external dependency: release-please output names (Task 2 Step 3) must be confirmed against the action version before the workflow can be trusted.
