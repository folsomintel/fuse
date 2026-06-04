# fc-agent

Surf-compatible host agent for Firecracker. Drives per-VM microVMs and speaks
the contract the Surf orchestrator's Firecracker provider client expects.

## Layout

```
fc-agent.py         # the agent — one firecracker process per VM, SSH for guest ops
fc-agent.sh         # start/stop/restart/log/env
fc-agent-test.sh    # end-to-end smoke test against the contract

fc-install.sh       # fetch firecracker binary, kernel, base rootfs, SSH key
fc-up.sh / fc-down.sh / fc-ssh.sh / fc-status.sh / fc-test.sh / fc-expose.sh
                    # manual helpers for a single VM (pre-agent; still useful for debugging)
```

Runtime-only (ignored in git):

```
vmlinux.bin         # guest kernel
rootfs.ext4         # base Firecracker CI rootfs
rootfs-surfd.ext4   # baked rootfs with surfd + systemd unit
ubuntu.id_rsa       # SSH key for root@<guest>
surfd               # binary (used only for baking the rootfs)
agent-state/        # per-VM metadata, rootfs copies, snapshots
.fc-agent.env       # bearer token (generated on first start)
```

## Bringing up a new host

```bash
git clone <this repo> ~/fc && cd ~/fc
./fc-install.sh                 # fetches firecracker binary + CI kernel/rootfs
# Drop a baked rootfs-surfd.ext4 next to rootfs.ext4
# (build instructions below, or fetch a prebuilt from your artifact store)
./fc-agent.sh start             # prints FIRECRACKER_BASE_URL + FIRECRACKER_TOKEN
./fc-agent-test.sh              # smoke test
```

Then feed the printed env vars into the Surf orchestrator:

```
FIRECRACKER_BASE_URL=http://<host>:8090
FIRECRACKER_TOKEN=<generated>
```

## Contract

All routes under `/v1/vm`, bearer auth (`Authorization: Bearer $TOKEN`), JSON in/out.

| Method | Path | Purpose |
| --- | --- | --- |
| POST   | `/v1/vm` | Create a microVM. Body: `{name,cpus,memory_mb,storage_gb,region}`. Returns `{vm_id,url}`. |
| GET    | `/v1/vm/{id}` | `{vm_id,url}` |
| GET    | `/v1/vm?prefix=` | `{vms:[{vm_id,url}]}` — prefix match on `name` |
| DELETE | `/v1/vm/{id}` | Tear down, free TAP + DNAT. |
| POST   | `/v1/vm/{id}/upload` | `{path, content_b64}` — writes into the guest (mkdir -p). |
| POST   | `/v1/vm/{id}/exec` | `{cmd:[...]}` — returns `{exit_code, stdout (b64), stderr (b64)}`. |
| POST   | `/v1/vm/{id}/start-surfd` | `{manifest_path, secrets_path, gateway?, extra_args?}` — writes a systemd drop-in and `systemctl start surfd`. |
| POST   | `/v1/vm/{id}/snapshot` | `{comment, include_ram}` — disk-only (`include_ram` ignored). |
| GET    | `/v1/vm/{id}/snapshots` | `{snapshots:[...]}` |
| POST   | `/v1/vm/{id}/restore` | `{snapshot_id, include_ram}` — stops fc, swaps rootfs, reboots VM. |

`url` is `<public_host>:<host_port>`, DNAT'd to the guest's `9550`. Host port =
`19550 + vm_index`. Public host is auto-detected; override with
`PUBLIC_HOST=...` in the env.

## Networking model

- One TAP per VM (`fcv<N>`), /30 subnet `10.200.<N>.0/30`, host `.1`, guest `.2`.
- Host iptables: MASQUERADE for egress, FORWARD accept for the TAP, per-VM
  PREROUTING DNAT `<public_host>:<port> -> <guest>:9550` for surfd.
- Cloud SG / external firewall must allow inbound TCP to the agent port (8090)
  and the DNAT range (19551–19799 at current defaults).

## Building the surfd rootfs (`rootfs-surfd.ext4`)

Built on top of the Firecracker CI Ubuntu 22.04 rootfs. Contents injected:

- `/usr/local/bin/surfd` — static Go binary, linux/amd64
- `/usr/local/bin/podman` + crun/runc/conmon/netavark/pasta/fuse-overlayfs —
  [`mgoltzsche/podman-static`](https://github.com/mgoltzsche/podman-static) v5.8.1
- `iptables` + libxtables + `/usr/lib/x86_64-linux-gnu/xtables/*` extracted from
  Ubuntu 22.04 (`iptables` deb). `/usr/sbin/iptables` etc. re-symlinked to
  `xtables-legacy-multi` because the kernel has no nftables.
- `/etc/ssl/certs/ca-certificates.crt` (copied from host)
- `/etc/systemd/system/surfd.service` with drop-in slot; `start-surfd` writes
  a drop-in with `--manifest/--secrets/--gateway/--vm-id` and `systemctl start`.
- `/etc/containers/storage.conf` — native kernel overlay (kernel has
  `CONFIG_OVERLAY_FS=y` but no `CONFIG_FUSE_FS`, so fuse-overlayfs is unused).
- `/etc/containers/containers.conf`:
  ```toml
  [containers]
  netns = "host"
  [network]
  firewall_driver = "none"
  ```
  **Why host netns**: the Firecracker CI kernel (`vmlinux-5.10.223`) ships
  without `CONFIG_NETFILTER_XT_MATCH_COMMENT`, `CONFIG_FUSE_FS`, or
  `CONFIG_NF_TABLES`. Netavark unconditionally emits `-m comment` rules and
  fails. Forcing new containers into the host's network namespace sidesteps
  netavark entirely. Isolation is provided by the microVM itself. If you need
  per-container network isolation inside one VM, build a custom kernel that
  enables the missing netfilter matches.
- `/var/tmp`, `/var/lib/containers`, `/run/containers` — pre-created (the CI
  rootfs was missing `/var/tmp`, which breaks image pulls).

**Known limitations of this rootfs**:

- `apt` is unusable (the CI rootfs has an empty `/var/lib/dpkg/status`).
  Customize via the mounted ext4 from the host, not from inside the guest.
- `podman run` without `--network=host` falls back to host netns anyway because
  of the config. Bridged per-container networks are not supported.
- Kernel lacks fuse and nftables (see above).

## Re-baking

Script-less for now — the current rootfs was built interactively. Rough recipe:

```bash
cp rootfs.ext4 rootfs-surfd.ext4
sudo truncate -s 4G rootfs-surfd.ext4
sudo e2fsck -f -y rootfs-surfd.ext4 && sudo resize2fs rootfs-surfd.ext4
sudo mount -o loop rootfs-surfd.ext4 /tmp/fcroot

# surfd + systemd unit + /surf + CA bundle + container dirs
sudo cp surfd /tmp/fcroot/usr/local/bin/surfd && sudo chmod 755 $_
sudo cp surfd.service /tmp/fcroot/etc/systemd/system/
sudo ln -sf /etc/systemd/system/surfd.service \
  /tmp/fcroot/etc/systemd/system/multi-user.target.wants/surfd.service
sudo mkdir -p /tmp/fcroot/surf /tmp/fcroot/var/tmp \
  /tmp/fcroot/var/lib/containers /tmp/fcroot/run/containers
sudo chmod 1777 /tmp/fcroot/var/tmp
sudo cp /etc/ssl/certs/ca-certificates.crt /tmp/fcroot/etc/ssl/certs/

# podman-static
curl -fsSL -o /tmp/podman.tgz \
  https://github.com/mgoltzsche/podman-static/releases/download/v5.8.1/podman-linux-amd64.tar.gz
sudo tar -xzf /tmp/podman.tgz -C /tmp/fcroot --strip-components=1

# iptables bundle — extracted from ubuntu:22.04 via podman on host
# (see history in this README / fc-agent session for the exact tar recipe)
sudo tar -xf iptables-full.tar -C /tmp/fcroot
for n in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
  sudo ln -sf xtables-legacy-multi /tmp/fcroot/usr/sbin/$n
done

# containers.conf + storage.conf (see above)
sudo tee /tmp/fcroot/etc/containers/containers.conf ...
sudo tee /tmp/fcroot/etc/containers/storage.conf ...

sudo umount /tmp/fcroot
```

## Operating

```bash
./fc-agent.sh start               # launch agent on :8090, print env
./fc-agent.sh stop                 # stop
./fc-agent.sh restart              # stop+start; re-attaches to running VMs
./fc-agent.sh log                  # tail agent log
./fc-agent.sh env                  # print env keys for an already-running agent
./fc-agent-test.sh                 # contract smoke test

# systemd integration (optional, for long-lived hosts)
./fc-agent.sh install-service      # enable fc-agent.service (survives reboot)
./fc-agent.sh uninstall-service
./fc-agent.sh install-updater      # weekly `fc-update-surfd.sh` via systemd timer
./fc-agent.sh uninstall-updater
```

## Re-attach on restart

On startup the agent walks `agent-state/vms/` and for each VM:

- pid alive + socket present → reuse (no-op)
- pid dead / socket gone → recreate the TAP, re-add the DNAT rule, relaunch
  firecracker with the same config (same vm_id, guest IP, URL)

That means you can `systemctl restart fc-agent` without losing VMs, and
host reboots transparently bring everything back (as long as the systemd unit
is installed).

## Bringing up a fresh host

```bash
git clone <repo> ~/fc && cd ~/fc
./fc-install.sh                    # firecracker binary + CI kernel + base rootfs
# provide the surfd binary (any of):
#   - GH_TOKEN=... ./fc-update-surfd.sh          (pulls from surf-systems/surf)
#   - gh release download 0.0 -R surf-systems/surf -p surfd -p surfd.sha256
./fc-bake-rootfs.sh                # builds rootfs-surfd.ext4
./fc-agent.sh install-service      # enable + start the agent
./fc-agent.sh install-updater      # weekly surfd refresh (needs .fc-updater.env)
./fc-agent-test.sh                 # smoke test
```

Open these at the cloud-provider firewall:

- `8090/tcp` — agent HTTP
- `19551–19799/tcp` — per-VM surfd DNAT range

## Weekly surfd updates

`fc-update-surfd.sh` checks `surf-systems/surf`'s latest release, verifies the
`surfd.sha256`, rebakes `rootfs-surfd.ext4` if the binary changed, then bounces
the agent. Runs idempotent — no-op if already at latest.

Install the timer with `./fc-agent.sh install-updater`. Required file:

```bash
echo 'GH_TOKEN=ghp_xxxxxx' > ~/fc/.fc-updater.env
chmod 600 ~/fc/.fc-updater.env
```

Fires Mondays at 04:00 UTC with 30-minute jitter. `Persistent=true` catches up
on missed runs if the host was down.

State lives under `agent-state/vms/<vm_id>/` — safe to `rm -rf` if the agent is
stopped and you want a clean slate.
