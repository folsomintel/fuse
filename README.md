# Fuse

**A control plane for agents — deploy Firecracker microVMs anywhere, with one script.**

Fuse lets you stand up isolated microVMs on your own hosts and drive their full
lifecycle (provision → running → drain → destroy) through a REST API. It's the
orchestration layer agents need for sandboxed compute — scheduling, snapshots, host
management, and live event streams — built on Firecracker and entirely open source.

The hard part of Firecracker has always been getting it running: the host setup, the
in-guest agent, building a rootfs. Fuse collapses all of that into a single script — one
command brings up a host with the agent installed and a rootfs baked and ready.

## What you get out of the box

- **One-script Firecracker setup** — host, agent, and a baked rootfs, ready to boot.
- **Scheduling across hosts** — register hosts, and Fuse places microVMs for you.
- **Full VM lifecycle** — provision → running → drain → destroy, tracked and reconciled.
- **Snapshots** — capture a running microVM and restore from it.
- **Live event streams** — tail a microVM's events over SSE.
- **Durable state** — backed by Postgres (or in-memory for local runs).
- **Per-VM secrets** — credentials generated per microVM, tokens encrypted at rest.
- **REST API + Prometheus metrics** — drive everything over HTTP; observe it out of the box.

## Firecracker, made simple

Getting Firecracker production-ready is normally a slog: install the binary, fetch a
kernel, build a rootfs, customize it, wire up networking, expose a control port. Fuse ships
that whole toolchain as a set of idempotent scripts in [`tools/`](tools/), so a host goes
from bare to bootable in a few commands:

```bash
cd tools
./fc-install.sh          # firecracker binary + kernel + base rootfs
./fc-bake-rootfs.sh      # build your guest rootfs
./fc-agent.sh start      # start the host agent — prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN
```

Copy those two printed values into Fuse's config and you're scheduling microVMs. Every
script is idempotent and safe to re-run. By default the rootfs ships with `fused`, our
reference in-guest agent — but the rootfs is yours to customize (more on the agent
[below](#the-in-guest-agent)).

## Concepts

### Environments

An **environment** is a single Firecracker microVM and the task running inside it. You
create one with a spec (CPU/memory), an optional manifest and secrets, and an optional
startup script; Fuse picks a host, boots the VM, uploads your files, starts the in-guest
agent, and tracks it through `provision → running → drain → destroy`. A background
reconcile loop catches orphans and stuck VMs.

| Method   | Path                                 | Purpose                              |
| -------- | ------------------------------------ | ------------------------------------ |
| `POST`   | `/v1/environments`                   | Create a microVM for a task          |
| `GET`    | `/v1/environments`                   | List environments                    |
| `GET`    | `/v1/environments/{id}`              | Get one environment                  |
| `POST`   | `/v1/environments/{id}?action=drain` | Gracefully drain (or `rotate-token`) |
| `DELETE` | `/v1/environments/{id}`              | Destroy the microVM                  |

### Hosts

A **host** is a machine running the Firecracker host agent. Register your hosts and Fuse
schedules microVMs across them. You can **cordon** a host to stop new placements (e.g.
before maintenance) and **uncordon** it to resume.

| Method   | Path                           | Purpose                |
| -------- | ------------------------------ | ---------------------- |
| `POST`   | `/v1/hosts`                    | Register a host        |
| `GET`    | `/v1/hosts`                    | List hosts             |
| `GET`    | `/v1/hosts/{id}`               | Get one host           |
| `POST`   | `/v1/hosts/{id}?action=cordon` | Cordon (or `uncordon`) |
| `DELETE` | `/v1/hosts/{id}`               | Remove a host          |

### Snapshots

A **snapshot** captures a running microVM's state so you can restore from it later — useful
for fast cold-starts, checkpointing long tasks, or branching from a known-good point.
Snapshots are persisted records and can carry a comment, retention, and metadata.

| Method   | Path                                | Purpose                           |
| -------- | ----------------------------------- | --------------------------------- |
| `POST`   | `/v1/environments/{id}/snapshots`   | Snapshot a running microVM        |
| `GET`    | `/v1/snapshots`                     | List snapshots                    |
| `GET`    | `/v1/snapshots/{id}`                | Get one snapshot                  |
| `POST`   | `/v1/snapshots/{id}?action=restore` | Restore a microVM from a snapshot |
| `DELETE` | `/v1/snapshots/{id}`                | Delete a snapshot                 |

### Logs & events

Each environment exposes a live **event stream** over Server-Sent Events — tail state
transitions and activity as they happen, no polling.

| Method | Path                           | Purpose                         |
| ------ | ------------------------------ | ------------------------------- |
| `GET`  | `/v1/environments/{id}/events` | Stream environment events (SSE) |

## Quickstart

```bash
# Build
go build -o bin/fuse ./server

# Run against a Firecracker host (see tools/ to bring one up)
export FIRECRACKER_BASE_URL=http://<host>:<port>
export FIRECRACKER_TOKEN=<token>
export TOKEN_ENCRYPTION_KEY=<hex-encoded 32-byte AES key>
./bin/fuse                       # listens on :8080 (ORCH_LISTEN)

# Or run with no host configured — an in-memory stub is used, handy for local dev
./bin/fuse
```

## Deploy

For anything past local dev, run the orchestrator as a service. The simplest topology
co-locates it on a Firecracker host, and the host toolchain installs it next to the agent:

```bash
cd tools
./fc-install.sh                     # firecracker + kernel + rootfs
./fc-agent.sh install-service       # host agent (systemd, :8090)
./fc-agent.sh install-orchestrator  # orchestrator (systemd, :8080), co-located
sudoedit /etc/default/orchestrator  # set DATABASE_URL (Postgres)
sudo systemctl start orchestrator
```

`install-orchestrator` prefills `/etc/default/orchestrator` with this host's
`FIRECRACKER_TOKEN` and generated auth/encryption keys (you supply only the Postgres
`DATABASE_URL`), installs the unit, and prints the `FUSE_BASE_URL` + `FUSE_TOKEN` for the
dashboard. Auth is required by default: the orchestrator refuses to boot until
`ORCH_AUTH_TOKEN` and `DATABASE_URL` are set, and the Postgres schema is created on first
start. Full walkthrough in
[`tools/README.md`](tools/README.md#co-locating-the-orchestrator-control-plane).

Topology is just one env var. Leave `FIRECRACKER_BASE_URL` on loopback to co-locate, or
point it at a remote agent (and register more hosts via `/v1/hosts`) to run the orchestrator
as a standalone control plane scheduling across many hosts.

## Dashboard

[**fuse-frontend**](../fuse-frontend) is the Fuse Control Plane: a Phoenix/LiveView console
for environments, hosts, snapshots, and a live event log, driving this orchestrator over its
REST API. Point it at your orchestrator with the two values `install-orchestrator` prints:

| Variable        | Value                                                           |
| --------------- | --------------------------------------------------------------- |
| `FUSE_BASE_URL` | orchestrator URL, e.g. `http://<host>:8080` (or your TLS proxy) |
| `FUSE_TOKEN`    | the orchestrator's `ORCH_AUTH_TOKEN` (same value)               |

```bash
cd ../fuse-frontend
FUSE_BASE_URL=http://<host>:8080 FUSE_TOKEN=<orch-auth-token> mix phx.server
# dashboard on http://localhost:4000
```

## The in-guest agent

Fuse doesn't hardcode a daemon. Boot drives a pluggable `AgentSpec` — files to upload, an
optional download URL for the agent binary, and the commands to launch and gracefully
drain it:

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

`fused` ships as the reference profile (`FusedAgentSpec` in `agent_profile.go`). To run
your own agent, supply a different `AgentSpec` and bake your binary into the rootfs — see
[`tools/FUSE.md`](tools/FUSE.md).

## Configuration

| Env var                                  | Flag                | Default               | Purpose                                                                       |
| ---------------------------------------- | ------------------- | --------------------- | ----------------------------------------------------------------------------- |
| `ORCH_LISTEN`                            | `--listen`          | `:8080`               | API listen address                                                            |
| `FIRECRACKER_BASE_URL`                   | `--firecracker-url` | _(empty → stub)_      | Firecracker host agent URL                                                    |
| `FIRECRACKER_TOKEN`                      |                     |                       | Bearer token for the host agent                                               |
| `DATABASE_URL`                           | `--database-url`    | _(empty → in-memory)_ | Postgres state store                                                          |
| `TOKEN_ENCRYPTION_KEY`                   |                     |                       | Hex-encoded 32-byte AES key for per-VM token encryption                       |
| `AGENT_DOWNLOAD_URL`                     |                     |                       | URL to fetch the guest agent binary at boot                                   |
| `ORCH_TLS_CERT` / `ORCH_TLS_KEY`         |                     |                       | Serve the API over TLS                                                        |
| `ORCH_REQUIRE_AUTH`                      |                     |                       | Fail closed: refuse to boot unless `ORCH_AUTH_TOKEN` + `DATABASE_URL` are set |
| `ORCH_AUTH_TOKEN` / `ORCH_ALLOWED_CIDRS` |                     |                       | API auth + IP allowlist                                                       |

## API

REST on [chi](https://github.com/go-chi/chi). The full surface is in the concept tables
above; `/metrics` exposes Prometheus metrics (unauthenticated). Spec:
[`api/openapi.yaml`](api/openapi.yaml).

## Use cases

> _Coming soon._ This section will collect real-world examples of how people run Fuse —
> from agent sandboxes to ephemeral CI runners and beyond. Using Fuse for something
> interesting? Open a PR and add it here.

## Layout

```
server/             entrypoint (main)
internal/core/      Boot, AgentSpec, FleetManager, scheduler, reconcile, snapshots, state
internal/core/agent_profile.go   fused reference profile (the only fused-aware Go file)
secrets/            per-VM credential generation + token encryption
firecracker/        Firecracker provider (talks to fc-agent)
api/                REST handlers
tools/              fc-agent host toolchain (bring up a Firecracker host)
```
