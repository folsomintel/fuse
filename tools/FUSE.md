# Fuse + fc-agent (host toolchain)

This directory is the **Firecracker host toolchain** bundled with Fuse. `fc-agent.py` is the
per-host agent that drives one Firecracker microVM per VM; Fuse's `firecracker` provider
talks to its HTTP API (`POST /v1/vm`, `GET /v1/vm`, `DELETE /v1/vm/{id}`,
`POST /v1/vm/{id}/start-agent`-class endpoints). See `README.md` for full setup.

## Requirement

Firecracker needs hardware virtualization — the host must expose `/dev/kvm` (bare-metal
Linux or a nested-virt-enabled instance). See [`README.md`](README.md) → *Requirements* for
the full prerequisites and a quick `/dev/kvm` check.

## Bring up a host

```bash
./fc-install.sh        # fetch firecracker binary + CI kernel/rootfs + SSH key
./fc-bake-rootfs.sh    # bake the guest rootfs (agent binary is baked in)
./fc-agent.sh start    # prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN for Fuse
./fc-agent-test.sh     # smoke test the contract
```

Point Fuse at the printed `FIRECRACKER_BASE_URL` / `FIRECRACKER_TOKEN`. Full setup,
the HTTP contract, and the networking model are in [`README.md`](README.md).

## The in-guest agent (fused vs. your own)

Fuse does not hardcode a specific in-guest daemon — it uploads a set of files into the
guest and launches a configurable command (`AgentSpec`; see `../docs/DECOUPLING.md`). The
**reference** in-guest agent is `fused`, baked into the rootfs by `fc-bake-rootfs.sh`.

`fc-bake-rootfs.sh` expects two inputs in this directory, which you supply (they are not
bundled): the `fused` binary (static `linux/amd64`) and a `fused.service` systemd unit that
supervises it inside the guest.

**To run your own in-guest agent instead of fused:** adapt `fc-bake-rootfs.sh` to bake your
binary + a supervisor unit into the rootfs, and have your agent consume the files Fuse
uploads (manifest/secrets/credentials) and accept the same start/stop entry points. The
agent is baked into the image, so re-bake whenever the binary changes. (Fuse's
`AgentSpec.DownloadURL` / the `/start-agent` `download_url` field can alternatively fetch the
binary from a URL at boot, but the default model here is bake-every-time.)
