# orchestrator setup QoL: design

source: [ORCHESTRATOR_SETUP_PLAN.md](../../../ORCHESTRATOR_SETUP_PLAN.md)

## scope

the plan has 11 items. items 5 (version stamping), 6 (goreleaser artifact), and
7 (systemd unit + install script) are already implemented (`fix: make CLI
version a stampable var`, `.goreleaser.yml`'s `orchestrator` build id,
`host-agent/fc-agent.sh install-orchestrator` + `orchestrator.service`).
verified by reading the current state of each, not by trusting the plan doc.
they are out of scope here.

in scope: p0 items 1-4, the `hostFromRecord` bug the plan calls out inside
item 2, p2 items 9-11, and item 8 (`fuse quickstart`). periodic/on-list host
token drift re-probing (mentioned as a "supporting fix" under item 2) is
deferred — it's a separate, larger feature (reconcile loop or hosts-list
latency impact) that doesn't block closing the original incident.

## 1. `GET /v1/version` (orchestrator, unauthenticated)

new handler alongside `Healthcheck.Liveness`/`Readiness` in
`internal/api/health.go`, mounted in `cmd/orchestrator/main.go` next to
`/health` and `/ready` (outside the auth/CIDR chain, same mux). returns:

```json
{"service": "fuse-orchestrator", "version": "0.4.0"}
```

version string threads in from `main.go`'s existing `version` var. also adds
a small middleware in `Handler.Router()` that sets `Server:
fuse-orchestrator/<version>` on every response, mirroring fc-agent's
`server_version = "fc-agent/0.1"`.

## 2. `fuse connect` probes before saving

in `cli/context_cmd.go`'s `newConnectCmd`, before `app.cfg.Add(...)`: a raw
HTTP GET (not through the SDK client — the URL/token pair isn't confirmed
yet) to `<url>/v1/version` with a short timeout, then one authenticated call
to confirm the token. hard-fails (unless `--no-verify`) on:

- connection error/timeout → `no orchestrator at %s (connection refused). is it running?`
- 200 but wrong/missing `service` field → `%s is not a fuse orchestrator (got Server: %s). the orchestrator default port is 8080; 8090 is the host agent.`
- version check passes, but the authenticated follow-up 401s → `orchestrator at %s rejected the token. this must be ORCH_AUTH_TOKEN, not the host-agent token.`

`--no-verify` flag skips both probes, for scripted/offline setup.

## 3. `fuse host register` probes the agent

two parts:

- CLI: `--token` becomes required for `fuse host register` (today defaults to
  `""` and is silently accepted — `cli/hosts_cmd.go:359`).
- server: `registerHost` already calls `CapacityProber.Capacity(ctx)` to
  source unset capacity fields. a probe **error** becomes fatal (unless
  `SkipVerify`), regardless of whether capacity was fully declared —
  today it's only checked when capacity fields are missing (handlers.go:694).
  this is a deliberate behavior change: the probe is now doubly-purposed as
  an auth check, not just a capacity source, so a declared-capacity
  registration with a bad token now also fails loudly instead of silently
  registering a host that will 401 on first VM create.

reuses the existing capacity round-trip, no second network call. to let
`internal/api` branch on the failure's HTTP status without importing
`internal/firecracker`/`internal/qemu` (breaking the existing
provider-interface decoupling), export a shared status-carrying error type:

```go
// internal/orchestrator/provider.go
type HTTPStatusError struct {
    Code int
    Body string
}
func (e *HTTPStatusError) Error() string { return fmt.Sprintf("http %d: %s", e.Code, e.Body) }
```

`internal/firecracker` and `internal/qemu` each currently define their own
private, byte-identical `httpStatusError` — both switch to constructing
`*orchestrator.HTTPStatusError` instead. `registerHost` then does:

```go
if _, err := prober.Capacity(ctx); err != nil && !req.SkipVerify {
    var statusErr *orchestrator.HTTPStatusError
    switch {
    case errors.As(err, &statusErr) && statusErr.Code == http.StatusUnauthorized:
        // "host agent at %s rejected the token. this must be its FC_AGENT_TOKEN, not the orchestrator token."
    default:
        // "no host agent at %s (connection refused). is fc-agent running on :8090?" or generic
    }
}
```

`RegisterHostRequest` (both `internal/api/types.go` and `sdks/go/types.go`)
gains `SkipVerify bool json:"skip_verify,omitempty"`. CLI's `--no-verify`
sets it, matching connect's flag name.

## 4. `hostFromRecord` silent-garbage-token bug

`internal/orchestrator/fleet_hosts.go:331-355`: when `TOKEN_ENCRYPTION_KEY`
is wrong/unset and a record has ciphertext, it currently falls back to
treating the raw ciphertext bytes as a plaintext bearer token — a silent
garbage token that only surfaces as a 401 on the next VM create, same failure
family as item 2.

fix: `hostFromRecord` returns `(Host, error)`. `recoverState` (fleet.go:1093,
phase 0 host rehydration loop at line ~1107) logs the error via `fm.logger`
and skips that host (does not add it to `fm.hosts`/`fm.hostProviders`)
instead of failing the whole recovery or silently continuing with a garbage
token. this only changes phase-0 host loading — it does not touch the
VM-recovery ordering phases after it (VMStateProvisioning demotion,
Allocated recompute) which are independently ordering-sensitive.

## 5. 404 route disambiguation

add a chi catch-all (`r.NotFound` in `Handler.Router()`) returning a
structured JSON 404 with a distinct error code (`route_not_found`) instead of
whatever chi's default produces. CLI's `friendly()` (`cli/render.go:73`)
special-cases that code: `%s does not expose the fuse API (is this the
orchestrator?)`, instead of the generic "not found" branch.

## 6. `fuse hosts` aliases `fuse host`'s subcommands

`newHostsCmd()` (`cli/hosts_cmd.go:15`) adds `register`, `get`, `cordon`,
`uncordon`, `remove`, `metrics` as subcommands, reusing the same constructors
`newHostCmd()` already calls (`newHostRegisterCmd()`,
`newHostGetCmd()`, etc.) — same `*cobra.Command` builders attached to both
parents. `fuse hosts register ...` now works exactly like `fuse host register
...`.

## 7. `fuse quickstart`

new `cli/quickstart_cmd.go`, registered in `root.go`. interactive-only
(errors with a clear message if `!isInteractive()`). flow:

1. prompt for orchestrator URL + token, run the same probe-before-save logic
   as `fuse connect` (factored into a shared helper so both commands call it)
2. on success, prompt for host agent URL + token, call `fuse host register`
   equivalent (reusing `newHostRegisterCmd`'s RunE body factored similarly),
   with the server-side probe from item 3 protecting it
3. select the new host as active

## 8. docs

- `docs/content/docs/reference/auth.mdx`: add the token glossary table (item
  10) — orchestrator bearer / host agent token / api key, who sets it, who
  uses it, which flag/env. this page is already the auth reference, no new
  page needed.
- `docs/content/docs/cloud/orchestrator-deploy.mdx` and
  `docs/content/docs/cloud/firecracker-host-setup.mdx`: one-line callout
  stating default ports (8080 orchestrator, 8090 fc-agent) if not already
  present (item 11).
- `docs/content/docs/reference/cli/contexts/connect.mdx`: document the new
  probe behavior and `--no-verify`.
- `docs/content/docs/reference/cli/hosts/register.mdx`: document the
  required `--token`, the new auth probe, and `--no-verify`.
- new `docs/content/docs/reference/cli/quickstart.mdx` for item 7.
- `docs/content/docs/reference/cli/hosts/index.mdx`: note that `fuse hosts`
  now aliases all of `fuse host`'s subcommands.

## testing

- unit tests for `hostFromRecord`'s new error path (decryption failure,
  unset key with ciphertext present) and `recoverState`'s skip-on-error
  behavior.
- unit tests for `registerHost`'s new fatal-on-probe-error behavior,
  including the `SkipVerify` bypass and the 401-vs-unreachable message
  branch.
- CLI tests (existing `cli/cli_test.go` pattern) for `connect --no-verify`,
  the new hard-fail messages against a stub HTTP server, and `fuse hosts
  register` aliasing.
- `GET /v1/version` handler test (unauthenticated, correct shape).
- 404 catch-all test asserting the structured `route_not_found` body.
