# Fuse

A standalone **Firecracker orchestrator for agents**. Fuse is the control plane that
schedules microVMs across hosts, manages their lifecycle (provision → running → drain →
destroy), persists state in Postgres, and exposes a REST API. Its Go core is
daemon-agnostic: Boot drives a configurable **in-guest agent** via `AgentSpec` rather than
hardcoding a specific daemon.

> **Generalization status:** the core is daemon-agnostic. The **firecracker** provider boots
> the `surfd` reference profile over the host-agent wire, but now prefers an additive
> `/start-agent` endpoint that supports `DownloadURL` (fetch your agent binary from a URL,
> e.g. a GitHub release — no rootfs re-bake) and falls back to `/start-surfd` on older hosts.
> The **daytona** provider runs an arbitrary `AgentSpec.Command`. Graceful drain stops surfd
> via SIGTERM (`systemctl stop surfd`), which runs surfd's real DAG teardown — no extra guest
> tooling. The `/start-agent` + `DownloadURL` path needs a host running the updated
> `tools/fc-agent.py`. See [`DECOUPLING.md`](DECOUPLING.md) → *Runtime-seam fixes*.

Fuse was extracted from the Surf orchestrator and decoupled from `surfd` (Surf's in-guest
daemon). See [`DECOUPLING.md`](DECOUPLING.md) for the design and what changed.

## What it does

1. Receives `POST /v1/environments` with a task ID + spec.
2. The scheduler picks a host (or uses an in-memory stub when no hosts/Firecracker are
   configured).
3. The provider creates a Firecracker VM and uploads the agent's files
   (manifest/secrets/credentials) into the guest.
4. Fuse launches the configured in-guest agent and tracks the VM through its lifecycle.
5. A reconcile loop detects orphans, stuck tasks, and registration drift.

## The pluggable agent (`AgentSpec`)

Instead of hardcoding an in-guest daemon, Boot drives an `AgentSpec`:

```go
type AgentSpec struct {
    Files        map[string][]byte // arbitrary files uploaded into the guest
    DownloadURL  string            // optional: fetch the agent binary (e.g. a GitHub release)
    Command      string            // how to launch the daemon
    DrainCommand string            // graceful-stop command, run via Exec on drain
    AuthToken    string
    Gateway      string
    GatewayToken string
}
```

`surfd` is provided as the reference profile (`SurfdAgentSpec` in `agent_profile.go`) — the
single home for all `/surf/*` path knowledge and the surfd launch line. To run your own
agent, supply a different `AgentSpec` (and bake your binary into the rootfs — see
[`tools/FUSE.md`](tools/FUSE.md)).

## Providers

- **firecracker** (default) — drives a per-host `fc-agent` (see [`tools/`](tools/)). Falls
  back to an in-memory stub when `FIRECRACKER_BASE_URL` is unset.
- **daytona** — Daytona-backed sandboxes.

Selected via `SURF_PROVIDER` (`firecracker` | `daytona`).

## Quickstart

```bash
# Build
go build -o bin/fuse ./server

# Run against a Firecracker host (see tools/ to bring one up)
export FIRECRACKER_BASE_URL=http://<host>:<port>
export FIRECRACKER_TOKEN=<token>
export TOKEN_ENCRYPTION_KEY=<hex-encoded 32-byte AES key>
./bin/fuse                       # listens on :8080 (ORCH_LISTEN)

# Or run with no host configured — the in-memory stub provider is used
./bin/fuse
```

Provision a host toolchain with the bundled scripts:

```bash
cd tools
./fc-install.sh && ./fc-agent.sh start   # prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN
```

## Configuration

| Env var | Flag | Default | Purpose |
|---|---|---|---|
| `ORCH_LISTEN` | `--listen` | `:8080` | API listen address |
| `FIRECRACKER_BASE_URL` | `--firecracker-url` | _(empty → stub)_ | Firecracker host agent URL |
| `FIRECRACKER_TOKEN` | | | Bearer token for the host agent |
| `DATABASE_URL` | `--database-url` | _(empty → in-memory)_ | Postgres state store |
| `TOKEN_ENCRYPTION_KEY` | | | Hex-encoded 32-byte AES key for per-VM token encryption |
| `SURF_PROVIDER` | | `firecracker` | Provider: `firecracker` \| `daytona` |
| `AGENT_DOWNLOAD_URL` | | | Daytona: URL to fetch the agent binary (`SURFD_DOWNLOAD_URL` alias) |
| `ORCH_TLS_CERT` / `ORCH_TLS_KEY` | | | Serve the API over TLS |
| `ORCH_AUTH_TOKEN` / `ORCH_ALLOWED_CIDRS` | | | API auth + IP allowlist |

## API

REST on chi. Key endpoints: `POST/GET /v1/environments`, `POST /v1/hosts`, and
`/metrics` (Prometheus, unauthenticated). See [`api/openapi.yaml`](api/openapi.yaml).

## Layout

```
server/         entrypoint (main)
provider.go     Provider/Environment interfaces, Boot, AgentSpec
agent_profile.go  surfd reference profile (the only surfd-aware Go file)
fleet.go        FleetManager — lifecycle + tracking
scheduler.go    host scheduling
reconcile.go    reconcile loop
state_store*.go state (in-memory + Postgres)
secrets/        per-VM credential generation + token encryption
firecracker/    Firecracker provider (talks to fc-agent)
daytona/        Daytona provider
api/            REST handlers
tools/          fc-agent host toolchain (bring up a Firecracker host)
```
