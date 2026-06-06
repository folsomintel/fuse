# Fuse — surfd decoupling design (authoritative)

Fuse is a standalone **Firecracker orchestrator for agents**, extracted from the Surf
orchestrator. It runs Firecracker microVMs on your own bare-metal hosts — there is **no
third-party sandbox service**. This document is the authoritative design for keeping the
Go core daemon-agnostic while preserving VM-lifecycle functionality. All refactor work
must conform to it.

## Why

The orchestrator originally hardcoded **surfd** (Surf's in-guest daemon) into its Boot path
and drain logic, and tied Fuse to the Surf monorepo via the
`github.com/surf-dev/surf/packages/manifest-schema/api/surf/v1` proto import (used only for
the gRPC `Down` call). That coupling has been removed.

**Goal:** surfd is *one configuration* of a generic, pluggable **guest-agent** abstraction.
Operators bring their own in-guest daemon (or use the surfd reference profile), fetched from
a URL (e.g. GitHub releases). Fuse core imports nothing from Surf.

## The three coupling points → generic moves

1. **`Environment.StartSurfd(StartSurfdOpts{...})` → `Environment.StartAgent(AgentSpec{...})`**
   - `AgentSpec` is generic: `Files map[string][]byte` (arbitrary files to upload),
     `DownloadURL string` (fetch the agent binary, e.g. GitHub releases),
     `Command string` (how to launch), plus pass-through `AuthToken`, `Gateway`,
     `GatewayToken`, and `DrainCommand string`.
   - surfd is expressed as a **default AgentSpec profile** (`SurfdAgentSpec` in
     `internal/core/agent_profile.go`) that reproduces today's behavior
     (manifest/secrets/tls/auth-token files + surfd launch command). Nothing surf-specific
     is hardcoded in the core.

2. **Boot's hardcoded `/surf/...` uploads → `AgentSpec.Files`**
   - Boot uploads whatever `AgentSpec.Files` specifies. The `/surf/manifest.json`,
     `/surf/secrets.json`, `/surf/tls/*`, `/surf/auth-token` paths live only inside the
     surfd profile, not in Boot.

3. **`surfd_client.go` gRPC `Down` drain → configurable drain command via `Exec`**
   - Drain runs `AgentSpec.DrainCommand` inside the guest via the existing
     `Environment.Exec` surface (no gRPC, no proto). Empty command → skip graceful drain,
     stay `Draining`, let the caller `DELETE` (back-compat preserved).
   - This removed the `manifest-schema/api/surf/v1` proto import; the `require`/`replace`
     in `go.mod` are gone and Fuse builds standalone.

## Provider

Fuse ships a **single provider: firecracker**. It drives a per-host `fc-agent`
(`tools/fc-agent.py`) and falls back to an in-memory stub when `FIRECRACKER_BASE_URL` is
unset (the dev default). Selected via `SURF_PROVIDER` (`firecracker`).

The firecracker host-agent wire is deliberately frozen (the external host agent cannot be
changed): `StartAgent` posts the generalized `POST /v1/vm/{id}/start-agent` (which adds
`DownloadURL` support so you can fetch your agent binary instead of re-baking the rootfs)
and **falls back to the frozen `/start-surfd` wire on a 404**. The structured surfd paths
it sends come from private package-local constants
(`surfdManifestGuestPath`/`surfdSecretsGuestPath`/`surfdTLSCertGuestPath`/
`surfdTLSKeyGuestPath`) that mirror the core profile by design. The host agent owns the
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

- **No module-path rename** yet. Keep `module github.com/surf-dev/surf/apps/orchestrator`.
- Do **not** cut the gateway registration or the provider abstraction. The
  `AgentSpec`/`StartAgent` seam is the OSS extension point and must stay generic — the
  simplicity target is **surfd specifically**, not the abstraction around it.

## Changelog

<!-- newest first -->

### 2026-06-05 — remove the Daytona provider (bare-metal only)

Fuse is now Firecracker-only on our own bare-metal hosts — no third-party sandbox service.
The Daytona provider has been removed.

- **DELETED** the `daytona/` package (`client.go`, `daytona.go`, plus
  `*_test.go`, `integration_test.go`, `e2e_grpcweb_test.go`).
- `providers/factory.go`: dropped the `Daytona` `Kind`, the `daytona` import, the
  `Config.Daytona` field, and the `case Daytona` construction branch.
- `server/main.go`: dropped the `daytona` import and the `SURF_PROVIDER=daytona`
  selection branch (and its `DAYTONA_API_KEY`/`DAYTONA_BASE_URL` reads). The valid-provider
  error message is now `(valid: firecracker)`.
- `internal/core/agent_profile.go`: renamed the guest binary path constant from
  `/home/daytona/surfd` to `/usr/local/bin/surfd`; reworded the `Command` and drain
  comments that referenced Daytona (the firecracker host agent still ignores `Command`).
- `tools/fc-agent.py`: dropped the comment cross-reference to Daytona's
  `ensureAgentBinary`.
- README updated: removed the daytona provider bullet, the `SURF_PROVIDER` daytona option,
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
`server/main.go` wires `AGENT_DOWNLOAD_URL` (with `SURFD_DOWNLOAD_URL` alias) into the
firecracker provider and the per-host factory. This keeps the surfd structured launch while
adding the OSS win: fetch your agent binary from a URL instead of re-baking.
