# Fuse + fc-agent (host toolchain)

This directory is the **Firecracker host toolchain** bundled with Fuse. `fc-agent.py` is the
per-host agent that drives one Firecracker microVM per VM; Fuse's `firecracker` provider
talks to its HTTP API (`POST /v1/vm`, `GET /v1/vm`, `DELETE /v1/vm/{id}`,
`POST /v1/vm/{id}/start-agent`-class endpoints). See `README.md` for full setup.

## Bring up a host

```bash
./fc-install.sh        # fetch firecracker binary + CI kernel/rootfs + SSH key
./fc-agent.sh start    # prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN for Fuse
./fc-agent-test.sh     # smoke test the contract
```

Point Fuse at the printed `FIRECRACKER_BASE_URL` / `FIRECRACKER_TOKEN`.

## The in-guest agent (surfd vs. your own)

Fuse no longer hardcodes a specific in-guest daemon — it uploads a set of files into the
guest and launches a configurable command (`AgentSpec`; see `../DECOUPLING.md`). The
**reference** in-guest agent is `surfd`, baked into the rootfs by `fc-bake-rootfs.sh`.

These **surfd-runtime scripts were intentionally NOT bundled** here (they live in the Surf
repo and are surfd-specific):

- `fc-up-surfd.sh` — boot a single VM with surfd, the manual (pre-agent) way
- `fc-update-surfd.sh` — pull a new surfd binary (e.g. from GitHub releases) into a host
- `surfd.service` — the systemd unit that supervises surfd inside the guest

**To run your own in-guest agent instead of surfd:** adapt `fc-bake-rootfs.sh` to bake your
binary + a supervisor unit into the rootfs, and have your agent consume the files Fuse
uploads (manifest/secrets/credentials) and accept the same start/stop entry points. Fuse's
`AgentSpec.DownloadURL` lets the host fetch your agent binary from a URL (e.g. a GitHub
release) at boot, mirroring what `fc-update-surfd.sh` did for surfd.
