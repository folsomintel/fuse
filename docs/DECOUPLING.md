# Fuse — surfd decoupling design (authoritative)

Fuse is a standalone Firecracker orchestrator for agents, extracted from the Surf
orchestrator. This document is the **authoritative design** for removing surfd-specific
coupling while preserving VM-lifecycle functionality. All refactor work must conform to it.

## Why

The orchestrator hardcodes **surfd** (Surf's in-guest daemon) into its Boot path and drain
logic. The single import `github.com/surf-dev/surf/packages/manifest-schema/api/surf/v1`
(used only in `surfd_client.go` for the gRPC `Down` call) is what ties Fuse to the Surf
monorepo and prevents it from building standalone (`go.mod` has a `replace` pointing at a
relative path that does not exist outside the monorepo).

**Goal:** surfd becomes *one configuration* of a generic, pluggable **guest-agent**
abstraction. OSS users bring their own in-guest daemon (or use a default), fetched from a
URL (e.g. GitHub releases). Fuse core imports nothing from Surf.

## The three coupling points → generic moves

1. **`Environment.StartSurfd(StartSurfdOpts{...})` → `Environment.StartAgent(AgentSpec{...})`**
   - `AgentSpec` is generic: `Files map[string][]byte` (arbitrary files to upload),
     `DownloadURL string` (fetch the agent binary, e.g. GitHub releases),
     `Command string` (how to launch), plus pass-through `AuthToken`, `Gateway`,
     `GatewayToken`, and `DrainCommand string`.
   - surfd is expressed as a **default AgentSpec profile** that reproduces today's behavior
     (manifest/secrets/tls/auth-token files + surfd launch command). Nothing surf-specific
     is hardcoded in the core.

2. **Boot's hardcoded `/surf/...` uploads → `AgentSpec.Files`**
   - Boot uploads whatever `AgentSpec.Files` specifies. The `/surf/manifest.json`,
     `/surf/secrets.json`, `/surf/tls/*`, `/surf/auth-token` paths live only inside the
     default surfd profile, not in Boot.

3. **`surfd_client.go` gRPC `Down` drain → configurable drain command via `Exec`**
   - Drain runs `AgentSpec.DrainCommand` inside the guest via the existing
     `Environment.Exec` surface (no gRPC, no proto). Empty command → skip graceful drain,
     stay `Draining`, let the caller `DELETE` (back-compat preserved).
   - **This removes the `manifest-schema/api/surf/v1` proto import**, after which the
     `require`/`replace` in `go.mod` are deleted and `go mod tidy` makes Fuse build standalone.

## Green-bar (definition of "same functionality")

These must stay GREEN (the loop's termination condition):
- **scheduler** (`TestFits_*`, scheduler_test.go)
- **state/persistence** (state_store, postgres, secrets crypto)
- **reconcile** (reconcile_test.go)
- **fleet lifecycle** (provision → running → drain → destroy; `TestCompleteTask*`,
  `TestDestroyVM*`, `TestListFleet`, `TestGetVM*`)
- **provider / Boot** (`TestBoot_*`, firecracker + daytona + stub provider tests)
- **drain** (the `Drain_*` / `EnvironmentAction_Drain_*` family — rewritten against the
  generic drain command instead of surfd gRPC)

surfd-proto-specific assertions (`TestBuildSurfdCommand`, gRPC `Down` mocking) are rewritten
against the generic hook or dropped **with a logged note** in this file's changelog.

## Hard constraints

- **Only edit `/Users/andrew/dev/misc/fuse`.** Never touch `surf-site/apps/orchestrator`
  (the source must keep working — this is a copy, not a move).
- **No `git init`, no module-path rename** yet. Keep `module github.com/surf-dev/surf/apps/orchestrator`.
- Do **not** cut Daytona, the gateway registration, or the provider abstraction. The
  simplicity target is **surfd specifically**.
- Daytona's `SurfdDownloadURL` generalizes to the `AgentSpec.DownloadURL` concept.

## Changelog (dropped/rewritten surfd assertions)
<!-- workflow appends notes here as it rewrites/drops surfd-specific tests -->

### 2026-05-29 — surfd decoupling round 1

- `Environment.StartSurfd(StartSurfdOpts)` → `Environment.StartAgent(AgentSpec)`.
  `StartSurfdOpts` replaced by the generic `AgentSpec` (Files / DownloadURL /
  Command / AuthToken / Gateway / GatewayToken / DrainCommand). surfd is now
  expressed entirely as the `SurfdAgentSpec` profile in the new
  `agent_profile.go`, the single home for all `/surf/*` path knowledge, the
  surfd launch command line, `DefaultSurfdDrainCommand`, and
  `DefaultSurfdManifest`.
- `surfd_client.go` **DELETED** (`SurfdInvoker`, `grpcSurfdInvoker`,
  `dialSurfdReal`, `waitForSurfdReady`, `bearerCreds`, `dialSurfdFunc`,
  `SetSurfdDialerForTest`, `drainSurfdTimeout`). The `go.mod` `require` +
  `replace` for `manifest-schema` were removed and `go mod tidy` run; the
  `surf/v1` proto import is gone and the (now unused) `google.golang.org/grpc`
  require was pruned. Fuse builds standalone.
- Boot no longer hardcodes `/surf/*` uploads: it uploads `AgentSpec.Files`
  (manifest/secrets/credentials, all populated by the profile) via a generic
  `uploadFiles` helper and applies the token side effect via
  `setTokenIfSupported`. `uploadCredentials` removed; `token_rotation.go` reuses
  `surfdCredentialFiles` + `setTokenIfSupported`. The empty-secret-map default
  ("{}") moved into `SurfdAgentSpec`.
- Drain now runs `AgentSpec.DrainCommand` (stored per-VM as `vm.drainCommand`,
  carried via `BootResult.DrainCommand`) inside the guest via
  `Environment.ExecStream` under the relocated/renamed `drainTimeout` (was
  `drainSurfdTimeout`). Empty command ⇒ skip + stay Draining (back-compat).
- DROPPED tests: `TestBuildSurfdCommand`,
  `TestBuildSurfdCommand_InsecureWhenNoToken` (daytona `buildSurfdCommand`
  deleted; the command line is assembled in core's `SurfdAgentSpec` and
  Daytona runs `AgentSpec.Command` verbatim).
- DROPPED test doubles: drain_test.go `stubInvoker` / `withStubInvoker` /
  `withDialError` / `deadlineCheckingInvoker`; api/handlers_test.go
  `stubInvoker` / `installStubDialer` — all hooked the deleted
  SurfdInvoker/dialer seam.
- REWRITTEN tests: drain lifecycle (`TestDrain_HappyPath`, `_NotFound`,
  `_NotRunningRejected`, `_ThenDestroyHappyPath`, `TestDestroyWithoutDrain_BackCompat`)
  and the api Drain action tests now assert the DrainCommand was Exec'd (or
  skipped) on the env the Fleet holds, instead of gRPC `Down` call counts.
  `TestDrain_SurfdDownError...` + `TestDrain_DialFailure...` merged into
  `TestDrain_DrainCommandErrorPreservesDrainingState`; added
  `TestDrain_EmptyDrainCommandSkips`; `TestDrain_RPCContextHasTimeout` →
  `TestDrain_ExecContextHasTimeout` (asserts a deadline within `drainTimeout`).
  Boot tests source upload paths from the profile constants
  (`surfManifestPath`/`surfSecretsPath`). firecracker `TestStub_start_surfd` →
  `TestStub_start_agent`.
- Daytona generalized: `StartSurfd`→`StartAgent` (runs `AgentSpec.Command`
  verbatim), `ensureSurfdBinary`→`ensureAgentBinary` (uses
  `AgentSpec.DownloadURL`, Config fallback), `Config.SurfdDownloadURL`→
  `DownloadURL`, `surfdSessionID`→`agentSessionID`, `surfdPort`→`agentPort`,
  `surfdStarted`→`agentStarted`. `ensureGuestDirs` now mkdirs each upload's
  parent dir with a **per-directory cache** (`createdDirs map[string]bool`)
  instead of the single `/surf`-prefix latch — correct under the
  non-deterministic `AgentSpec.Files` map iteration order (the old single bool
  + hardcoded `mkdir -p /surf/tls` would have missed `/surf/tls` if the cert
  file uploaded after a `/surf` file). server/main.go wires
  `AGENT_DOWNLOAD_URL` with `SURFD_DOWNLOAD_URL` as a back-compat alias.
- DELIBERATE FREEZE: the firecracker host-agent wire
  (`POST /v1/vm/{id}/start-surfd` with structured ManifestPath/SecretsPath/TLS
  fields) is kept frozen because the external host agent cannot be changed.
  Only the `Environment` method was renamed to `StartAgent`; it still sends the
  structured surfd paths, now from private package-local constants
  (`surfdManifestGuestPath`/`surfdSecretsGuestPath`/`surfdTLSCertGuestPath`/
  `surfdTLSKeyGuestPath`) that mirror the core profile by design. The exported
  `firecracker.TLSCertGuestPath`/`TLSKeyGuestPath` were made private (no other
  package referenced them).
- daytona `e2e_grpcweb_test.go` relabeled as a surfd-PROFILE transport e2e
  (build-tagged `e2e_grpcweb`, live-only, off the green-bar critical path); no
  behavioral change.
- Flagged-not-relocated couplings (commented in place, no behavior change):
  `api/resolver.go` `defaultManifest` now sources
  `orchestrator.DefaultSurfdManifest` (surfd-profile schema);
  `secrets/secrets.go` `manifestSecretExtractor`/`ExtractRequiredSecrets`/
  `ValidateSecrets` flagged as surfd-profile manifest-shaped validation.
- PRE-EXISTING (not introduced here, out of scope): `go test -race ./...`
  reports a data race in `TestLoad_provisionWithHosts` via
  `allocateOnHost`/`Schedule` (`fleet_hosts.go`, `scheduler.go`) — files
  untouched by this work and byte-identical to the source orchestrator.
  `go build ./...` and `go test ./...` (the green-bar) both pass.

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
green, zero `github.com/surf-dev/surf` imports outside the module, source
`apps/orchestrator` untouched (git-verified).

## Runtime-seam fixes (round 2)

The two runtime-seam gaps the code-level verification could not see have been addressed.

### Fix 1 — graceful drain (RESOLVED, live-proven)

surfd traps SIGTERM and tears down its service DAG before exit — the same teardown the gRPC
`Down` RPC drove. So the drain command is now `systemctl stop surfd 2>/dev/null || pkill
-TERM surfd` (no `|| true`): on the firecracker profile `systemctl stop` delivers SIGTERM,
waits for the clean exit, and the unit's `Restart=on-failure` does not bring it back; the
`pkill -TERM` fallback covers non-systemd guests. A genuine failure now propagates so Drain
keeps the VM `Draining` (original semantics). **No rootfs re-bake needed** — `systemctl` and
`surfd.service` are already in the baked image, and the SIGTERM path already exists in surfd.

The earlier `surfctl down || true` plan was abandoned: surfctl isn't baked, and `|| true`
silently no-op'd. The SIGTERM approach is simpler, needs no TLS/token/port, and is verifiable
on the current image.

### Fix 2 — generalized firecracker agent launch (CODE COMPLETE, fallback live-proven)

`fc-agent.py` gained an **additive `/start-agent`** action: a generalized `do_start_agent`
parameterizes the systemd-drop-in `ExecStart` by `binary_path`/`listen` and, when
`download_url` is set, fetches the agent binary into the guest before launch (idempotent
`test -x || curl … && chmod +x`). `/start-surfd` is now a thin wrapper over `do_start_agent`
with the surfd defaults, so its bytes are unchanged. The firecracker provider's `StartAgent`
posts `/start-agent` first (including `DownloadURL` from `spec` or `Config`) and **falls back
to `/start-surfd` on a 404** (via a typed `httpStatusError`); other errors propagate.
`server/main.go` wires `AGENT_DOWNLOAD_URL` (with `SURFD_DOWNLOAD_URL` alias) into the
firecracker provider and the per-host factory. This keeps the surfd structured launch
(not arbitrary `Command`, which stays Daytona's model) while adding the OSS win: fetch your
agent binary from a URL instead of re-baking.

### Proof status (honest split)

- **PROVEN (live, on `64.34.84.221`):** drain actually stops surfd (port open→closed across
  drain); provision still reaches `Running` via the `/start-agent`→404→`/start-surfd`
  fallback (the live host runs the old `fc-agent.py`); `go build` + `go vet` + `go test ./...`
  green; new firecracker tests cover `/start-agent` happy-path, spec-over-config DownloadURL
  precedence, 404 fallback (asserting the start-surfd body equals the frozen wire), and
  non-404 propagation.
- **PENDING host redeploy / re-bake:** the `/start-agent` endpoint itself and `DownloadURL`
  fetch run only on a host whose `fc-agent.py` is updated from `tools/` (the live host still
  serves the old one, so today they are reached only via the fallback path, i.e. not yet
  exercised end-to-end). Daytona's non-systemd drain (`pkill -TERM` branch) is reasoned, not
  live-tested.
