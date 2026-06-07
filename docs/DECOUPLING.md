# Fuse — guest-agent decoupling design (authoritative)

Fuse is a standalone **Firecracker orchestrator for agents**, extracted from the Surf
orchestrator. It runs Firecracker microVMs on your own bare-metal hosts — there is **no
third-party sandbox service**. This document is the authoritative design for keeping the
Go core daemon-agnostic while preserving VM-lifecycle functionality. All refactor work
must conform to it.

## Why

The orchestrator originally hardcoded **surfd** (Surf's in-guest daemon — since renamed to
the `fused` reference agent) into its Boot path and drain logic, and tied Fuse to the Surf
monorepo via the `github.com/surf-dev/surf/packages/manifest-schema/api/surf/v1` proto import
(used only for the gRPC `Down` call). That coupling has been removed.

**Goal:** the reference agent (`fused`) is *one configuration* of a generic, pluggable
**guest-agent** abstraction. Operators bring their own in-guest daemon (or use the `fused`
reference profile), fetched from a URL (e.g. GitHub releases). Fuse core imports nothing
from Surf.

## The three coupling points → generic moves

1. **`Environment.StartSurfd(StartSurfdOpts{...})` → `Environment.StartAgent(AgentSpec{...})`**
   - `AgentSpec` is generic: `Files map[string][]byte` (arbitrary files to upload),
     `DownloadURL string` (fetch the agent binary, e.g. GitHub releases),
     `Command string` (how to launch), plus pass-through `AuthToken`, `Gateway`,
     `GatewayToken`, and `DrainCommand string`.
   - the reference agent is expressed as a **default AgentSpec profile** (`FusedAgentSpec` in
     `internal/core/agent_profile.go`) that reproduces today's behavior
     (manifest/secrets/tls/auth-token files + the `fused` launch command). Nothing
     fuse-specific is hardcoded in the core.

2. **Boot's hardcoded `/fuse/...` uploads → `AgentSpec.Files`**
   - Boot uploads whatever `AgentSpec.Files` specifies. The `/fuse/manifest.json`,
     `/fuse/secrets.json`, `/fuse/tls/*`, `/fuse/auth-token` paths live only inside the
     `fused` profile, not in Boot.

3. **`surfd_client.go` gRPC `Down` drain → configurable drain command via `Exec`**
   - Drain runs `AgentSpec.DrainCommand` inside the guest via the existing
     `Environment.Exec` surface (no gRPC, no proto). Empty command → skip graceful drain,
     stay `Draining`, let the caller `DELETE` (back-compat preserved).
   - This removed the `manifest-schema/api/surf/v1` proto import; the `require`/`replace`
     in `go.mod` are gone and Fuse builds standalone.

## Provider

Fuse ships a **single provider: firecracker**. It drives a per-host `fc-agent`
(`tools/fc-agent.py`) and falls back to an in-memory stub when `FIRECRACKER_BASE_URL` is
unset (the dev default). Selected via `FUSE_PROVIDER` (`firecracker`).

The firecracker host-agent wire is deliberately frozen (the external host agent cannot be
changed): `StartAgent` posts the generalized `POST /v1/vm/{id}/start-agent` (which adds
`DownloadURL` support so you can fetch your agent binary instead of re-baking the rootfs)
and **falls back to the frozen `/start-surfd` wire on a 404** (the route name is kept for
back-compat; its payload now carries the `/fuse/*` paths). The structured guest paths it
sends come from private package-local constants
(`fusedManifestGuestPath`/`fusedSecretsGuestPath`/`fusedTLSCertGuestPath`/
`fusedTLSKeyGuestPath`) that mirror the core profile by design. The host agent owns the
launch mechanism, so it IGNORES `AgentSpec.Command` and reads the structured fields off the
wire.

## Green-bar (definition of "same functionality")

These must stay GREEN (the loop's termination condition):
- **scheduler** (`TestFits_*`, scheduler_test.go)
- **state/persistence** (state_store, postgres, secrets crypto)
- **reconcile** (reconcile_test.go)
- **fleet lifecycle** (provision → running → drain → destroy; `TestCompleteTask*`,
  `TestDestroyVM*`, `TestListFleet`, `TestGetVM*`)
- **provider / Boot** (`TestBoot_*`, firecracker + stub provider tests)
- **drain** (the `Drain_*` / `EnvironmentAction_Drain_*` family — written against the
  generic drain command instead of surfd gRPC)

## Hard constraints

- **Module path** is `module github.com/andrewn6/fuse` (renamed from
  `github.com/surf-dev/surf/apps/orchestrator`; keep it stable now).
- Do **not** cut the gateway registration or the provider abstraction. The
  `AgentSpec`/`StartAgent` seam is the OSS extension point and must stay generic — the
  simplicity target is the **reference agent (`fused`) specifically**, not the abstraction
  around it.

## Changelog

<!-- newest first -->

### 2026-06-07 — releases, self-host auto-update, green CI/CD, full e2e

- **Versioned releases (GoReleaser).** `server` and `cmd/fused` now carry a
  `-X main.version` stamp and a `--version` flag. `.goreleaser.yaml` publishes the
  control-plane tarball (orchestrator + dbcheck) and `fused` as a raw, directly
  downloadable asset (`fused_Linux_x86_64`) for `AGENT_DOWNLOAD_URL` and the updater.
  Fixed a pre-existing invalid `archives.files.default` field that failed `goreleaser check`.
- **Self-host auto-update.** `tools/fc-update.sh` polls the latest `andrewn6/fuse` GitHub
  release, compares `fused --version` to the latest tag, and on a newer release: `git pull`s
  the checkout, downloads the new `fused`, re-bakes the rootfs, and restarts the agent
  (optionally updating a co-located orchestrator via `FUSE_ORCH_SERVICE`/`FUSE_ORCH_BIN`).
  `./fc-agent.sh install-updater` wires it as a weekly systemd timer — public repo, no token
  required (`GH_TOKEN` optional for rate limits).
- **CI/CD green.** Fixed the long-standing data race (`activeHosts` handed out shared
  `*Host` pointers that `Schedule` read while `allocateOnHost` wrote — now snapshots copies),
  so `go test -race ./...` passes. Added `.golangci.yml`, made the lint job **blocking**
  (pinned `golangci-lint v2.12.2`, repo is clean), and added a `goreleaser check` CI job so a
  CD-config regression fails on PRs. Fixed all prod lint findings (unchecked `Close`, unused
  `nextID`/`subscriberCount`, a De Morgan simplification).
- **Full e2e (`e2e/deploy_test.go`).** Now covers the whole API surface — environment
  lifecycle, **hosts** register/list/get/cordon/uncordon/remove, **rotate-token**, and the
  **SSE events** stream — in addition to the deploy lifecycle. Hermetic against the stub by
  default; set `FUSE_E2E_REMOTE=1` (reads `FIRECRACKER_BASE_URL`/`FIRECRACKER_TOKEN` from
  `.env`) or `FUSE_E2E_FIRECRACKER_URL`/`_TOKEN` to run the same suite against a real host.

### 2026-06-07 — reference `fused` agent + full deploy test

The repo had no buildable in-guest agent after the rename (the old `surfd` lived in another
repo and its fetch alias was dropped), so `fc-bake-rootfs.sh` couldn't complete and no real
deploy was possible. Added a minimal reference agent and an end-to-end deploy test.

- **`cmd/fused`** — the reference in-guest agent: reads the uploaded `/fuse/manifest.json` +
  `/fuse/secrets.json`, binds `--listen` (default `:9550`), serves unauthenticated `/health`
  and bearer-protected `/v1/info`, supports `--tls-cert/--tls-key/--auth-token-file/--gateway/
  --vm-id/--insecure`, and exits 0 on SIGTERM (so the `systemctl stop fused` drain is clean
  and `Restart=on-failure` does not revive it). Unit-tested (`cmd/fused/main_test.go`).
- **`tools/fc-build-agent.sh`** builds `tools/fused` (static `linux/amd64`) from `cmd/fused`;
  **`tools/fused.service`** is the systemd unit (its `ExecStart` is overridden by the host
  fc-agent drop-in on start-agent). `fc-bake-rootfs.sh` now bakes these.
- **`.goreleaser.yaml`** ships `fused` (linux amd64/arm64) so it can be fetched at boot via
  `AGENT_DOWNLOAD_URL` / the `/start-agent` `download_url` field (no rebake).
- **End-to-end deploy tests** (`e2e/deploy_test.go` + `tools/fc-e2e.sh`): assemble the full
  stack like `server/main.go` and walk the lifecycle over HTTP (probes → create → get → list
  → snapshot → restore → drain → destroy → 404). They run against the in-memory stub by
  default (hermetic); set `FUSE_E2E_FIRECRACKER_URL`/`_TOKEN` (Go) or `FUSE_E2E_REMOTE=1`
  (shell, reads `.env`) to target a real host. NOTE: the stub provider's `Exec`/`ExecStream`
  were made no-ops so the stub server can run the full lifecycle locally; the drain step is
  therefore *simulated* under the stub — real guest-exec/drain semantics are covered by the
  `TestDrain_*` unit tests, and the real path requires a host running the new agent.

### 2026-06-07 — rename the reference agent `surfd` → `fused`

De-Surf the OSS product: the reference in-guest daemon is now `fused`. This was a
system-wide rename across the Go core, the firecracker provider, the `tools/` host
toolchain, and the docs.

- Go core (`agent_profile.go`): `SurfdAgentSpec`→`FusedAgentSpec`,
  `DefaultSurfdDrainCommand`→`DefaultFusedDrainCommand`,
  `DefaultSurfdManifest`→`DefaultFusedManifest`, `buildSurfdCommand`→`buildFusedCommand`,
  `surfdCredentialFiles`→`fusedCredentialFiles`, the `surf*Path` consts →`fuse*Path`,
  `SurfSecretsPath`→`FuseSecretsPath`, `surfAgentBinaryPath` value →`/usr/local/bin/fused`,
  drain command →`systemctl stop fused 2>/dev/null || pkill -TERM fused`.
- Guest path convention `/surf/*` → `/fuse/*` (manifest/secrets/tls/auth-token), kept in
  sync between the core profile and the firecracker frozen-wire consts
  (`surfdManifestGuestPath`→`fusedManifestGuestPath`, etc.).
- Baked image: `rootfs-surfd.ext4`→`rootfs-fused.ext4`, guest binary
  `/usr/local/bin/surfd`→`/usr/local/bin/fused`, unit `surfd.service`→`fused.service`,
  `/etc/default/surfd`→`/etc/default/fused`, `SURFD_EXTRA_ARGS`→`FUSED_EXTRA_ARGS`.
  `fc-bake-rootfs.sh` now expects operator-supplied `fused` + `fused.service` inputs.
- **FROZEN, deliberately NOT renamed:** the `/start-surfd` HTTP route (and the
  `startSurfdRequest` struct / `do_start_surfd` handler that model it). The Go provider still
  posts `/start-agent` first and falls back to `/start-surfd` on a 404; the route name is a
  back-compat alias whose payload now carries the `/fuse/*` paths. On the **new** host the
  `/start-surfd` alias launches `fused`. (An already-deployed **old** host running the old
  `fc-agent.py` + old `rootfs-surfd.ext4` is not path-compatible with the new `/fuse/*`
  uploads — for OSS that's a non-issue since hosts bake fresh and run the new agent.)
- Build/vet/`go test ./...` green after the rename.
- Follow-up (same day) — completed the rest of the de-Surf so the OSS product carries no
  Surf branding outside genuine historical references and the frozen `/start-surfd` route:
  - **Go module path** `github.com/surf-dev/surf/apps/orchestrator` → `github.com/andrewn6/fuse`
    (every import across 23 files + `go.mod`).
  - **VM-name prefix** default `surf-` → `fuse-` (`ORCH_VM_PREFIX`, plus test fixtures and the
    `cmd/dbcheck` example).
  - **API title/branding** "Surf Orchestrator" → "Fuse Orchestrator" (`server/main.go` `@title`
    + comments; `api/docs/swagger.*` regenerate from the annotation).
  - **Env vars**: `SURF_PROVIDER` → `FUSE_PROVIDER`, `SURF_CONTAINER_BIN` → `FUSE_CONTAINER_BIN`;
    **dropped** the `SURFD_DOWNLOAD_URL` back-compat alias (use `AGENT_DOWNLOAD_URL`).

### 2026-06-05 — remove the Daytona provider (bare-metal only)

Fuse is now Firecracker-only on our own bare-metal hosts — no third-party sandbox service.
The Daytona provider has been removed.

- **DELETED** the `daytona/` package (`client.go`, `daytona.go`, plus
  `*_test.go`, `integration_test.go`, `e2e_grpcweb_test.go`).
- `providers/factory.go`: dropped the `Daytona` `Kind`, the `daytona` import, the
  `Config.Daytona` field, and the `case Daytona` construction branch.
- `server/main.go`: dropped the `daytona` import and the `FUSE_PROVIDER=daytona`
  selection branch (and its `DAYTONA_API_KEY`/`DAYTONA_BASE_URL` reads). The valid-provider
  error message is now `(valid: firecracker)`.
- `internal/core/agent_profile.go`: renamed the guest binary path constant from
  `/home/daytona/surfd` to `/usr/local/bin/surfd`; reworded the `Command` and drain
  comments that referenced Daytona (the firecracker host agent still ignores `Command`).
- `tools/fc-agent.py`: dropped the comment cross-reference to Daytona's
  `ensureAgentBinary`.
- README updated: removed the daytona provider bullet, the `FUSE_PROVIDER` daytona option,
  the layout entry, and re-framed the `AGENT_DOWNLOAD_URL` row as a generic guest-agent
  download (it was always wired into the firecracker provider, not just Daytona).
- The generic `AgentSpec.Command` / `buildSurfdCommand` seam is **kept**: it is Fuse's own
  pluggable-agent extension point, not a third-party integration.
- Also fixed a pre-existing build break introduced by the "move core packages" refactor:
  `internal/core` embeds `migrations/*.sql`, but the directory still sat at the repo root.
  Moved it to `internal/core/migrations/` so `go:embed` resolves. `go build ./...`,
  `go vet ./...`, and `go test ./...` all green afterward.

### 2026-05-29 — independent verification

Adversarial behavioral-equivalence verification (baseline `/tmp/fuse-baseline` vs. new
fuse, 8 dimensions + completeness critic): **8/8 dimensions "preserved", 0 blockers, 0
regressions, 0 uncertain.** All 36 findings are `note`-severity and non-behavioral:
- `SurfdAgentSpec` discards a `json.Marshal` error that cannot fail for `map[string]string`
  (bytes identical to the original `"{}"` default).
- `uploadFiles` / token rotation iterate a Go map (non-deterministic order) instead of the
  old fixed sequence; final guest filesystem is byte-identical (distinct paths, no
  inter-file deps, all uploads complete before the agent reads).
- Production error strings reworded (`upload %s: %w`); no test/caller asserts on them.

Independently re-confirmed: standalone `go build` + `go vet` clean, full `go test -count=1`
green, zero `github.com/surf-dev/surf` imports outside the module.

### 2026-05-29 — surfd decoupling round 1

- `Environment.StartSurfd(StartSurfdOpts)` → `Environment.StartAgent(AgentSpec)`.
  `StartSurfdOpts` replaced by the generic `AgentSpec` (Files / DownloadURL /
  Command / AuthToken / Gateway / GatewayToken / DrainCommand). surfd is now
  expressed entirely as the `SurfdAgentSpec` profile in `agent_profile.go`, the single
  home for all `/surf/*` path knowledge, the surfd launch command line,
  `DefaultSurfdDrainCommand`, and `DefaultSurfdManifest`.
- `surfd_client.go` **DELETED** (`SurfdInvoker`, `grpcSurfdInvoker`, `dialSurfdReal`,
  `waitForSurfdReady`, `bearerCreds`, `dialSurfdFunc`, `SetSurfdDialerForTest`,
  `drainSurfdTimeout`). The `go.mod` `require` + `replace` for `manifest-schema` were
  removed and `go mod tidy` run; the `surf/v1` proto import is gone and the (now unused)
  `google.golang.org/grpc` require was pruned. Fuse builds standalone.
- Boot no longer hardcodes `/surf/*` uploads: it uploads `AgentSpec.Files`
  (manifest/secrets/credentials, all populated by the profile) via a generic `uploadFiles`
  helper and applies the token side effect via `setTokenIfSupported`. `uploadCredentials`
  removed; `token_rotation.go` reuses `surfdCredentialFiles` + `setTokenIfSupported`. The
  empty-secret-map default ("{}") moved into `SurfdAgentSpec`.
- Drain now runs `AgentSpec.DrainCommand` (stored per-VM as `vm.drainCommand`, carried via
  `BootResult.DrainCommand`) inside the guest via `Environment.ExecStream` under the
  relocated/renamed `drainTimeout` (was `drainSurfdTimeout`). Empty command ⇒ skip + stay
  Draining (back-compat).
- DELIBERATE FREEZE: the firecracker host-agent wire (`POST /v1/vm/{id}/start-surfd` with
  structured ManifestPath/SecretsPath/TLS fields) is kept frozen because the external host
  agent cannot be changed. Only the `Environment` method was renamed to `StartAgent`; it
  still sends the structured surfd paths, now from private package-local constants that
  mirror the core profile by design.

## Runtime-seam fixes

### Graceful drain (RESOLVED, live-proven)

surfd traps SIGTERM and tears down its service DAG before exit — the same teardown the gRPC
`Down` RPC drove. So the drain command is `systemctl stop surfd 2>/dev/null || pkill -TERM
surfd` (no `|| true`): on the firecracker profile `systemctl stop` delivers SIGTERM, waits
for the clean exit, and the unit's `Restart=on-failure` does not bring it back; the `pkill
-TERM` fallback covers non-systemd guests. A genuine failure propagates so Drain keeps the
VM `Draining` (original semantics). **No rootfs re-bake needed** — `systemctl` and
`surfd.service` are already in the baked image, and the SIGTERM path already exists in
surfd.

### Generalized firecracker agent launch (CODE COMPLETE, fallback live-proven)

`fc-agent.py` gained an **additive `/start-agent`** action: a generalized `do_start_agent`
parameterizes the systemd-drop-in `ExecStart` by `binary_path`/`listen` and, when
`download_url` is set, fetches the agent binary into the guest before launch (idempotent
`test -x || curl … && chmod +x`). `/start-surfd` is now a thin wrapper over `do_start_agent`
with the surfd defaults, so its bytes are unchanged. The firecracker provider's `StartAgent`
posts `/start-agent` first (including `DownloadURL` from `spec` or `Config`) and **falls back
to `/start-surfd` on a 404** (via a typed `httpStatusError`); other errors propagate.
`server/main.go` wires `AGENT_DOWNLOAD_URL` into the
firecracker provider and the per-host factory. This keeps the surfd structured launch while
adding the OSS win: fetch your agent binary from a URL instead of re-baking.
