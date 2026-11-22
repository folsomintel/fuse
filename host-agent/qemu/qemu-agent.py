#!/usr/bin/env python3
"""Fuse QEMU host agent (GPU passthrough).

Speaks the same HTTP contract as fc-agent.py (POST /v1/vm, upload/exec,
start-agent, ...) so Fuse's `qemu` provider (internal/qemu) can drive it
unchanged. One QEMU process per VM; state persisted under
<QEMU_DIR>/agent-state/vms/<vm_id>/.

Difference from firecracker: VMs boot under QEMU/KVM with one or more GPUs
passed through via VFIO (-device vfio-pci). Because a passed-through GPU cannot
be checkpointed, snapshot/restore is intentionally unsupported and returns 501
(mirrors decision D4 in GPU_PLAN.md; the qemu Environment in internal/qemu is
deliberately not SnapshotCapable).

Auth: Authorization: Bearer $QEMU_AGENT_TOKEN
Transport for guest ops (upload/exec/start-agent): SSH to root@<guest_ip>.

NOTE: the HTTP router, auth, config, and error types are wired so the module
imports and serves; all lifecycle functions are implemented.
"""
from __future__ import annotations

import json
import os
import re
import shlex
import shutil
import socket
import subprocess
import sys
import threading
import time
import traceback
import uuid
from http.server import ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

# code shared with fc-agent.py lives in host-agent/shared/agentlib.py. a
# standalone install (fuse local) puts it next to this file instead.
_HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(_HERE), str(_HERE.parent / "shared")]
import agentlib  # noqa: E402
from agentlib import (  # noqa: E402
    FUSED_PORT,
    HTTPError,
    IPTABLES,
    _ssh_control_path,
    add_agent_forward,
    compose_up_command,
    create_spec_digest,
    del_agent_forward,
    do_attach,
    do_exec,
    do_upload,
    goarch,
    host_authority,
    host_iface,
    list_vms,
    load_meta,
    now_iso,
    parse_attach_spec,
    pid_alive,
    resolve_public_host,
    sanitize_name,
    save_meta,
    ssh_exec,
    sudo,
    teardown_tap,
    vm_dir,
    vm_lock,
)

# -- Configuration ------------------------------------------------------------

QEMU_DIR = Path(os.environ.get("QEMU_DIR", "/home/ubuntu/qemu"))
STATE_DIR = QEMU_DIR / "agent-state"
VMS_DIR = STATE_DIR / "vms"
KERNEL = QEMU_DIR / "vmlinuz.bin"
BASE_ROOTFS = Path(os.environ.get("BASE_ROOTFS", str(QEMU_DIR / "rootfs-cuda.qcow2")))
# Named rootfs images for bring-your-own-image (see internal/fusefile's
# ResourceSpec.Image): <IMAGES_DIR>/<name>.qcow2. An operator bakes and places a
# CUDA-capable rootfs there (e.g. with qemu-bake-cuda-rootfs.sh) before a
# Fusefile can reference it by name.
IMAGES_DIR = Path(os.environ.get("IMAGES_DIR", str(QEMU_DIR / "images")))
# OVMF/UEFI firmware QEMU boots the guest with (installed by qemu-install.sh).
OVMF_CODE = Path(os.environ.get("OVMF_CODE", "/usr/share/OVMF/OVMF_CODE.fd"))
SSH_KEY = QEMU_DIR / "ubuntu.id_rsa"
TOKEN = os.environ.get("QEMU_AGENT_TOKEN")
PORT = int(os.environ.get("QEMU_AGENT_PORT", "8091"))
# Request body ceilings. Ordinary control-plane calls are a few short fields;
# uploads carry a base64 file and get their own, larger, ceiling. Raise
# QEMU_AGENT_MAX_UPLOAD_BYTES on a host that needs to push bigger payloads into
# guests. Header and request-line sizes are already bounded by http.server
# (64 KiB per line, 100 headers).
MAX_BODY_BYTES = int(os.environ.get("QEMU_AGENT_MAX_BODY_BYTES", str(1 << 20)))       # 1 MiB
MAX_UPLOAD_BYTES = int(os.environ.get("QEMU_AGENT_MAX_UPLOAD_BYTES", str(64 << 20)))  # 64 MiB
# Socket timeout for an HTTP conversation. Bounds a client that opens a
# connection and then dribbles (or never finishes) its request. Attach clears
# it for its own socket, since an interactive session is idle by design.
REQUEST_TIMEOUT = float(os.environ.get("QEMU_AGENT_REQUEST_TIMEOUT", "60"))
QEMU_BIN = os.environ.get("QEMU_BIN", "/usr/bin/qemu-system-x86_64")
# Path listing bindable GPU groups, one per line ("<count> <kind> <pci_slot>...")
# as emitted by `qemu-vfio-bind.sh --list`. The agent reads it to pick free
# devices to attach at create time.
VFIO_INVENTORY = Path(os.environ.get("VFIO_INVENTORY", str(QEMU_DIR / "vfio-inventory.txt")))
# Path listing MIG instances exposed as mediated devices, one per line
# ("<profile> <kind> <mdev_uuid>", e.g. "1g.10gb a100 c73f1fa6-..."). The
# operator carves MIG GPU instances and creates the mdev devices (nvidia vGPU
# host driver) before listing them here; the agent picks free ones to attach
# via vfio-pci sysfsdev at create time (decision D5, fractional passthrough).
MIG_INVENTORY = Path(os.environ.get("MIG_INVENTORY", str(QEMU_DIR / "mig-inventory.txt")))
# MIG_LIFECYCLE_MANAGED=1 declares that this host's MIG GPU instances are
# managed by qemu-mig-setup.sh (it owns the layout config and re-applies it on
# reboot). when set, destroy_vm re-applies the layout after a VM holding MIG
# instances is torn down so freed instances are immediately re-carved and
# available again. when unset the operator hand-manages instances and the
# agent never touches the card layout on destroy (the instance is simply
# released from the in-memory allocation set).
MIG_LIFECYCLE_MANAGED = os.environ.get("MIG_LIFECYCLE_MANAGED", "") not in ("", "0", "false")
MIG_SETUP_SCRIPT = Path(os.environ.get("MIG_SETUP_SCRIPT", str(Path(__file__).parent / "scripts" / "qemu-mig-setup.sh")))
# Guest ports that must never be published: the agent's own management API
# (FUSED_PORT) and the guest SSH port (22). The fusefile parser enforces this
# first (internal/fusefile/parse.go), but we re-check here as defense-in-depth
# so a direct API caller cannot DNAT the control plane to the open internet.
RESERVED_GUEST_PORTS = frozenset({FUSED_PORT, 22})
HOST_PORT_BASE = int(os.environ.get("FUSE_HOST_PORT_BASE", "19650"))


PUBLIC_HOST = resolve_public_host()

if not TOKEN:
    print("QEMU_AGENT_TOKEN must be set", file=sys.stderr)
    sys.exit(1)

VMS_DIR.mkdir(parents=True, exist_ok=True)
SSH_CONTROL_DIR = STATE_DIR / "ssh-control"
SSH_CONTROL_DIR.mkdir(parents=True, exist_ok=True)

_create_lock = threading.Lock()


# -- Small helpers ------------------------------------------------------------


def resolve_image_path(image: str) -> Path:
    """Resolve a caller-supplied image name to a path inside IMAGES_DIR.

    image names the request into a filesystem path, so resolve it and confirm
    it stays under IMAGES_DIR; "../agent-state/vms/<victim>/rootfs" would else
    reach another vm's disk and get copied into the caller's guest. realpath
    resolves symlinks too, so a link planted inside IMAGES_DIR that points out
    of it is rejected as well. Raises HTTPError(400) on anything outside.
    """
    if not isinstance(image, str) or not image.strip():
        raise HTTPError(400, f"invalid base image name: {image!r}")
    # NUL and other control bytes never name a real image and would either
    # raise deep inside os.path or truncate the path at the C boundary.
    if any(ord(c) < 0x20 or c == "\x7f" for c in image):
        raise HTTPError(400, f"invalid base image name: {image!r}")
    if os.path.isabs(image) or image.startswith("~"):
        raise HTTPError(400, f"base image name must be relative to IMAGES_DIR: {image!r}")

    images_root = os.path.realpath(IMAGES_DIR)
    resolved = os.path.realpath(os.path.join(images_root, f"{image}.qcow2"))
    if not resolved.startswith(images_root.rstrip(os.sep) + os.sep):
        raise HTTPError(400, f"invalid base image name: {image!r}")
    return Path(resolved)


# -- VFIO / GPU inventory -----------------------------------------------------

def _parse_vfio_metadata(meta: str, kind: str, slots: list[str]) -> list[dict]:
    """Parse the optional " | key=val;..." suffix into per-device dicts.

    qemu-vfio-bind.sh emits one metadata blob per line, describing the group's
    primary GPU (bus_id, uuid, model, memory_mb, mig_mode). Returns a
    single-element devices list shaped like probe_gpu_devices()'s dicts, or []
    when the blob is empty. Unknown keys are ignored; missing keys degrade to
    sensible defaults.

    Note: a single blob per line means a rare multi-GPU IOMMU group (e.g. an
    NVLink pair sharing one group) contributes one device entry even though
    group["count"] is > 1. Per-GPU device entries for such groups are deferred
    to the device-aware model (issue #38).
    """
    fields: dict[str, str] = {}
    for pair in meta.split(";"):
        pair = pair.strip()
        if not pair or "=" not in pair:
            continue
        key, _, val = pair.partition("=")
        fields[key.strip()] = val.strip()
    if not fields:
        return []
    try:
        memory_mb = int(fields.get("memory_mb", "") or 0)
    except ValueError:
        memory_mb = 0
    mig_mode = fields.get("mig_mode", "")
    # prefer the gpu's own bus id from the blob; fall back to the group's first
    # slot for old-format blobs that predate the bus_id key (the first slot may
    # be a bridge, so this is only a last resort).
    bus_id = fields.get("bus_id", "") or (slots[0] if slots else "")
    return [{
        "index": 0,
        "uuid": fields.get("uuid", ""),
        "model": fields.get("model", ""),
        "pci_bus_id": bus_id,
        "memory_mb": memory_mb,
        "driver_version": fields.get("driver_version", ""),
        "compute_cap": fields.get("compute_cap", ""),
        "mig_capable": bool(mig_mode) and mig_mode != "N/A",
        "mig_mode": mig_mode,
    }]


def read_vfio_inventory() -> list[dict]:
    """Parse VFIO_INVENTORY into a list of bindable GPU groups.

    Each line is "<count> <kind> <pci_slot> [<pci_slot> ...] [| key=val;...]" as
    emitted by qemu-vfio-bind.sh --list. Returns dicts like
    {"count": 1, "kind": "a100", "slots": ["0000:17:00.0"]}, plus a "devices"
    list when the optional per-device metadata suffix is present. Old-format
    lines (no " | " suffix) parse exactly as before, without a "devices" key.
    """
    if not VFIO_INVENTORY.exists():
        return []
    groups: list[dict] = []
    for lineno, raw in enumerate(VFIO_INVENTORY.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        # split off the optional metadata suffix before the count/kind/slots part
        head, sep, meta = line.partition("|")
        parts = head.split()
        if len(parts) < 3:
            raise HTTPError(500, f"bad vfio inventory at {VFIO_INVENTORY}:{lineno}: {raw!r}")
        count_s, kind, slots = parts[0], parts[1], parts[2:]
        try:
            count = int(count_s)
        except ValueError:
            raise HTTPError(500, f"bad gpu count at {VFIO_INVENTORY}:{lineno}: {count_s!r}")
        if count <= 0 or len(slots) < count:
            raise HTTPError(500,
                f"bad vfio inventory group at {VFIO_INVENTORY}:{lineno}: {raw!r}")
        group = {"count": count, "kind": kind.lower(), "slots": slots}
        if sep:
            devices = _parse_vfio_metadata(meta, kind.lower(), slots)
            if devices:
                group["devices"] = devices
        groups.append(group)
    return groups


def allocated_pci_slots() -> set[str]:
    """Return the set of pci slots already attached to a running vm.

    Scans every vm meta.json under VMS_DIR so device allocation never
    double-assigns a GPU across concurrent VMs.
    """
    used: set[str] = set()
    for meta in list_vms():
        used.update(meta.get("gpu_slots", []))
    return used

def pick_gpu_slots(count: int, kind: str | None) -> list[str]:
    """Choose complete free iommu groups representing `count` gpus.

    Empty/None kind matches any group. Raises HTTPError(409) when fewer than
    `count` matching devices are free. Whole-device allocation only (no MIG).
    """
    if count <= 0:
        return [] 
    
    want = (kind or "").lower()
    used = allocated_pci_slots() 

    selected: list[str] = []
    selected_count = 0
    available_count = 0
    for group in read_vfio_inventory():
        if want and group["kind"] != want:
            continue 
        if any(slot in used for slot in group["slots"]):
            continue
        available_count += group["count"]
        if selected_count + group["count"] > count:
            continue
        selected.extend(group["slots"])
        selected_count += group["count"]
        if selected_count == count:
            return selected

    if selected_count != count:
        kind_desc = f"kind {want!r}" if want else "any kind"
        raise HTTPError(409, 
            f"insufficient free gpus: requested {count} of {kind_desc}, "
            f"{available_count} available in complete iommu groups")

    return selected


# -- MIG / mdev inventory (fractional passthrough, decision D5) ---------------

def read_mig_inventory() -> list[dict]:
    """Parse MIG_INVENTORY into a list of allocatable MIG instances.

    Each line is "<profile> <kind> <mdev_uuid> [<parent_gpu_uuid>]" (one MIG
    instance per line). The optional 4th field is the parent GPU uuid, written
    by qemu-mig-setup.sh so the orchestrator can bind a specific instance to a
    specific VM and report which card it came off. Returns dicts like
    {"profile": "1g.10gb", "kind": "a100", "uuid": "...", "parent_gpu_uuid": "..."}.
    """
    if not MIG_INVENTORY.exists():
        return []
    devices: list[dict] = []
    for lineno, raw in enumerate(MIG_INVENTORY.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split()
        if len(parts) not in (3, 4):
            raise HTTPError(500, f"bad mig inventory at {MIG_INVENTORY}:{lineno}: {raw!r}")
        profile, kind, mdev_uuid = parts[0], parts[1], parts[2]
        parent = parts[3] if len(parts) == 4 else ""
        devices.append({
            "profile": profile.lower(),
            "kind": kind.lower(),
            "uuid": mdev_uuid,
            "parent_gpu_uuid": parent,
        })
    return devices


def allocated_mdev_uuids() -> set[str]:
    """Return the set of mdev uuids already attached to a running vm.

    Scans every vm meta.json under VMS_DIR so MIG allocation never
    double-assigns an instance across concurrent VMs.
    """
    used: set[str] = set()
    for meta in list_vms():
        used.update(meta.get("gpu_mdevs", []))
    return used


def pick_mig_devices(count: int, profile: str, kind: str | None) -> list[str]:
    """Choose `count` free MIG instances of `profile` (optionally by kind).

    Returns their mdev uuids. Empty/None kind matches any kind. Raises
    HTTPError(409) when fewer than `count` matching instances are free.
    """
    if count <= 0:
        return []

    want_profile = profile.lower()
    want_kind = (kind or "").lower()
    used = allocated_mdev_uuids()

    selected: list[str] = []
    for device in read_mig_inventory():
        if device["profile"] != want_profile:
            continue
        if want_kind and device["kind"] != want_kind:
            continue
        if device["uuid"] in used:
            continue
        selected.append(device["uuid"])
        if len(selected) == count:
            return selected

    kind_desc = f"kind {want_kind!r}" if want_kind else "any kind"
    raise HTTPError(409,
        f"insufficient free mig instances: requested {count} x {want_profile} of "
        f"{kind_desc}, {len(selected)} free")


def claim_mig_devices(uuids: list[str], profile: str, kind: str | None) -> list[str]:
    """Validate and claim a specific set of MIG instance uuids.

    The orchestrator's per-instance scheduler picks concrete uuids and sends
    them in the create request so the control plane knows which instance went
    to which VM. this enforces the contract the local pick_mig_devices path
    also upholds: every requested uuid must be a real instance in the inventory,
    of the requested profile, optionally of the requested kind, and currently
    free (not attached to another VM). raises HTTPError(409) on any mismatch.
    """
    if not uuids:
        return []

    want_profile = profile.lower()
    want_kind = (kind or "").lower()
    used = allocated_mdev_uuids()

    by_uuid = {d["uuid"]: d for d in read_mig_inventory()}
    selected: list[str] = []
    for u in uuids:
        dev = by_uuid.get(u)
        if dev is None:
            raise HTTPError(409, f"mig instance {u} not in host inventory")
        if dev["profile"] != want_profile:
            raise HTTPError(409,
                f"mig instance {u} is profile {dev['profile']!r}, not {want_profile!r}")
        if want_kind and dev["kind"] != want_kind:
            raise HTTPError(409,
                f"mig instance {u} is kind {dev['kind']!r}, not {want_kind!r}")
        if u in used:
            raise HTTPError(409, f"mig instance {u} already attached to another vm")
        if u in selected:
            raise HTTPError(409, f"mig instance {u} requested more than once")
        selected.append(u)
    return selected


def _normalize_pci_bus_id(bus_id: str) -> str:
    """Reduce nvidia-smi's extended pci id to lspci's domain:bus:dev.func form.

    nvidia-smi reports "00000000:17:00.0" (8-digit domain); lspci and the vfio
    inventory use "0000:17:00.0" (4-digit domain). Trim to the low 16 bits so
    ids match across sources.
    """
    bus_id = bus_id.strip().lower()
    head, sep, rest = bus_id.partition(":")
    if sep and len(head) > 4:
        head = head[-4:]
        return f"{head}:{rest}"
    return bus_id


def _probe_gpu_devices_nvidia_smi() -> list[dict]:
    """Best-effort per-gpu probe via nvidia-smi CSV. Returns [] on any failure.

    nvidia-smi is blind once devices are bound to vfio-pci, and may be absent
    entirely, so this is only a fallback when the inventory has no per-device
    metadata. Never raises.
    """
    fields = [
        "index", "uuid", "name", "pci.bus_id", "memory.total",
        "driver_version", "compute_cap", "mig.mode.current", "mig.mode.pending",
    ]
    try:
        cp = subprocess.run(
            ["nvidia-smi", f"--query-gpu={','.join(fields)}",
             "--format=csv,noheader,nounits"],
            capture_output=True, text=True, check=True,
        )
    except (FileNotFoundError, subprocess.SubprocessError):
        return []

    devices: list[dict] = []
    for line in cp.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        cols = [c.strip() for c in line.split(",")]
        if len(cols) < len(fields):
            continue
        index, uuid_s, model, bus_id, mem_s, driver, cap, mig_cur, _mig_pend = cols[:9]
        try:
            memory_mb = int(mem_s)
        except ValueError:
            memory_mb = 0
        try:
            idx = int(index)
        except ValueError:
            idx = 0
        devices.append({
            "index": idx,
            "uuid": uuid_s,
            "model": model,
            "pci_bus_id": _normalize_pci_bus_id(bus_id),
            "memory_mb": memory_mb,
            "driver_version": driver,
            "compute_cap": cap,
            "mig_capable": mig_cur != "N/A",
            "mig_mode": mig_cur,
        })
    return devices


def probe_gpu_devices() -> list[dict]:
    """Return a list of per-gpu dicts describing this host's GPUs.

    Primary source is VFIO_INVENTORY (read_vfio_inventory), the post-bind truth
    written pre-bind by qemu-vfio-bind.sh. When the inventory carries no
    per-device metadata (older format, or before #34's enrichment lands), fall
    back to a live nvidia-smi probe, which only works while devices are still on
    the nvidia driver. Never raises.
    """
    devices: list[dict] = []
    for group in read_vfio_inventory():
        devices.extend(group.get("devices", []))
    if devices:
        return devices
    return _probe_gpu_devices_nvidia_smi()


# -- Networking ---------------------------------------------------------------
# Mirrors fc-agent.py's tap + DNAT model: one tap per vm, a host port DNATed to
# the guest's fused port, plus optional expose forwards.

def pick_index() -> int:
    """Pick the lowest unused per-vm index (drives tap name, ip, host port)."""
    used = set()
    for d in VMS_DIR.iterdir() if VMS_DIR.exists() else []:
        meta_p = d / "meta.json"
        if meta_p.exists():
            try:
                used.add(json.loads(meta_p.read_text())["index"])
            except Exception:
                pass

    for i in range(1, 250):
        if i not in used:
            return i
    raise HTTPError(503, "no free vm index: this host already holds 249 vms")


def tap_name(idx: int) -> str:
    """Deterministic tap device name for a vm index."""
    return f"qv{idx}"


def tap_addrs(idx: int) -> tuple[str, str]:
    """(host_ip, guest_ip): the two ends of a vm index's tap."""
    return f"10.200.{idx}.1", f"10.200.{idx}.2"


def setup_tap(idx: int, iface: str) -> tuple[str, str, str]:
    """Create and configure the tap for a vm index; return (tap, host_ip, guest_ip)."""
    tap = tap_name(idx)
    host_ip, guest_ip = tap_addrs(idx)
    sudo(["ip", "link", "del", tap], check=False)
    sudo(["ip", "tuntap", "add", tap, "mode", "tap"])
    sudo(["ip", "addr", "add", f"{host_ip}/30", "dev", tap])
    sudo(["ip", "link", "set", tap, "up"])
    sudo(["sysctl", "-w", "net.ipv4.ip_forward=1"])
    for chk, add in [
        (IPTABLES + ["-t", "nat", "-C", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"],
         IPTABLES + ["-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"]),
        (IPTABLES + ["-C", "FORWARD", "-i", tap, "-o", iface, "-j", "ACCEPT"],
         IPTABLES + ["-I", "FORWARD", "-i", tap, "-o", iface, "-j", "ACCEPT"]),
        (IPTABLES + ["-C", "FORWARD", "-i", iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
         IPTABLES + ["-I", "FORWARD", "-i", iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"]),
    ]:
        if sudo(chk, check=False).returncode != 0:
            sudo(add)
    return tap, host_ip, guest_ip


# Transports an expose entry can name. Kept in step with fusefile's
# validProtocols and the firecracker agent's EXPOSE_PROTOCOLS; the parser is
# the first gate, this is the re-check for a direct API caller.
EXPOSE_PROTOCOLS = {"tcp", "udp"}


def _free_host_port(protocol: str = "tcp") -> int:
    """Bind to port 0, read it back, then close. Small race is acceptable.

    The probe socket's type matches the protocol being published: the tcp and
    udp port spaces are independent, so probing the wrong one can hand back a
    port already bound on the transport that actually matters."""
    kind = socket.SOCK_DGRAM if protocol == "udp" else socket.SOCK_STREAM
    with socket.socket(socket.AF_INET, kind) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def add_expose_forward(host_port: int, guest_ip: str, guest_port: int,
                       protocol: str = "tcp") -> None:
    """DNAT host_port -> guest_ip:guest_port for a published (exposed) guest port.

    protocol is "tcp" or "udp", defaulting to tcp so a caller that predates
    the field installs exactly the rules it always did. Rules are
    per-transport: a tcp rule forwards no udp traffic."""
    iface = host_iface()
    rules = [
        IPTABLES + ["-t", "nat", "-I", "PREROUTING", "-i", iface, "-p", protocol,
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        IPTABLES + ["-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", protocol,
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        IPTABLES + ["-I", "FORWARD", "-p", protocol, "-d", guest_ip,
         "--dport", str(guest_port), "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def del_expose_forward(host_port: int, guest_ip: str, guest_port: int,
                       protocol: str = "tcp") -> None:
    """Remove an expose DNAT rule (idempotent)."""
    iface = host_iface()
    rules = [
        IPTABLES + ["-t", "nat", "-D", "PREROUTING", "-i", iface, "-p", protocol,
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        IPTABLES + ["-t", "nat", "-D", "OUTPUT", "-o", "lo", "-p", protocol,
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        IPTABLES + ["-D", "FORWARD", "-p", protocol, "-d", guest_ip,
         "--dport", str(guest_port), "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


# -- Guest transport (SSH) ----------------------------------------------------

SSH_BASE = [
    "ssh", "-i", str(SSH_KEY),
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "LogLevel=ERROR",
    "-o", "ConnectTimeout=5",
    "-o", "BatchMode=yes",
    # Reuse one multiplexed connection per guest instead of paying a fresh
    # TCP+SSH handshake on every call (readiness poll, uploads, exec, agent
    # start easily add up to 8-10 calls per create). Short-lived on purpose:
    # ControlPersist expires the master on its own, so a stale socket left
    # by a destroyed VM is harmless (a new VM at the same guest ip just
    # starts a fresh master once the old one has timed out).
    "-o", "ControlMaster=auto",
    "-o", "ControlPersist=60s",
]


def wait_for_ssh(guest_ip: str, timeout: float = 30.0) -> bool:
    """Block until the guest accepts SSH or timeout elapses; return readiness."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, _, _ = ssh_exec(guest_ip, "true", timeout=4.0)
        if rc == 0:
            return True
        time.sleep(0.3)
    return False


# -- QEMU process lifecycle ---------------------------------------------------

def start_qemu(meta: dict) -> None:
    """Launch the QEMU process for a vm and wait until it is reachable.

    Builds the qemu-system-x86_64 argv from meta: kvm acceleration, OVMF
    firmware, the per-vm rootfs drive, a tap netdev, vcpu/memory sizing, and one
    "-device vfio-pci,host=<slot>" per assigned whole GPU in meta["gpu_slots"]
    plus one "-device vfio-pci,sysfsdev=/sys/bus/mdev/devices/<uuid>" per
    assigned MIG instance in meta["gpu_mdevs"] (decision D5). Detach with
    setsid; record the pid and monitor socket in meta.
    """
    d = vm_dir(meta["vm_id"])
    sock = d / "qmp.sock" 
    log = d / "qemu.log"
    sudo(["rm", "-f", str(sock)], check=False)

    argv = [
        QEMU_BIN,
        "-accel", "kvm",
        "-cpu", "host",
        "-m", str(meta["memory_mb"]),
        "-smp", str(meta["cpus"]),
        "-drive", f"if=pflash,format=raw,readonly=on,file={OVMF_CODE}",
        "-drive", f"file={meta['rootfs']},format=qcow2,if=virtio",
        "-netdev", f"tap,id=net0,ifname={meta['tap']},script=no,downscript=no",
        "-device", f"virtio-net-pci,netdev=net0,mac={meta['mac']}",
        "-kernel", str(KERNEL),
        "-append", (
            "root=/dev/vda console=ttyS0 rw "
            f"ip={meta['guest_ip']}::{meta['host_ip']}:255.255.255.252::eth0:off"
        ),
        "-qmp", f"unix:{sock},server,nowait",
        "-display", "none",
        "-daemonize",
        "-pidfile", str(d / "qemu.pid"),
    ] 

    for slot in meta.get("gpu_slots", []):
        argv += ["-device", f"vfio-pci,host={slot}"]

    for mdev_uuid in meta.get("gpu_mdevs", []):
        argv += ["-device", f"vfio-pci,sysfsdev=/sys/bus/mdev/devices/{mdev_uuid}"]

    sudo(argv, check=True)

    # The pidfile/socket usually appear in well under 50ms, so poll fast
    # instead of eating a fixed startup delay before the first check.
    pidfile = d / "qemu.pid"
    for _ in range(100):
        if pidfile.exists() and sock.exists():
            break
        time.sleep(0.02)
    if not pidfile.exists():
        raise RuntimeError(f"qemu failed to start (see {log})")
         
    meta["pid"] = int(pidfile.read_text().strip())
    meta["sock"] = str(sock)
    sudo(["chmod" ,"666", str(sock)], check=False)


def stop_qemu(meta: dict) -> None:
    """Terminate a vm's QEMU process and clean up its runtime sockets (idempotent)."""
    pid = meta.get("pid")
    if pid and pid_alive(pid):
        sudo(["kill","-9", str(pid)], check=False)
    sock = meta.get("sock")
    if sock:
        sudo(["rm", "-f", sock], check=False)
    pidfile = vm_dir(meta["vm_id"]) / "qemu.pid"
    sudo(["rm", "-f", str(pidfile)], check=False)

# -- VM lifecycle (HTTP surface) ---------------------------------------------

def create_vm(req: dict) -> dict:
    """creates and boots a vm. the vm's own lock is held throughout, so a
    retry or a destroy of the same name waits for this create. _create_lock
    is held only while reserve_vm picks the index and gpus and writes the
    meta that reserves them, so the rootfs copy, the boot and the ssh wait of
    different vms run in parallel."""
    name = req.get("name") or f"vm-{uuid.uuid4().hex[:8]}"
    vm_id = sanitize_name(name)
    with vm_lock(vm_id):
        with _create_lock:
            meta, source_rootfs = reserve_vm(req, name, vm_id)
        if not meta.get("creating"):
            # the same create sent again: the vm it already made.
            return meta
        boot_vm(meta, source_rootfs)
        return meta


def reserve_vm(req: dict, name: str, vm_id: str) -> tuple[dict, Path]:
    """Validate a create request and reserve its index and gpus.

    req carries name, cpus, memory_mb, storage_gb, region, image, and the GPU
    fields the qemu provider forwards: gpus (int), gpu_kind (str), and
    gpu_profile (str, MIG profile like "1g.10gb"). Resolves the base rootfs
    (BASE_ROOTFS or IMAGES_DIR/<image>.qcow2), picks GPU devices (with
    gpu_profile set, `gpus` free MIG instances of that profile via
    pick_mig_devices (fractional, D5); otherwise `gpus` free whole VFIO
    devices via pick_gpu_slots), then a per-vm index and host port. boot_vm
    builds the tap, copies the rootfs and starts qemu. Returns the vm meta, already saved with "creating" set, and the rootfs
    to copy. callers hold _create_lock: the index and the gpu picks read
    every saved meta, so they are taken from the moment it is saved.
    """
    # the same create sent again (a retry that lost its response) gets the vm
    # it made, before any allocation that vm itself would now fail. a
    # different spec under the name is still the 409 below.
    existing = load_meta(vm_id)
    if existing and existing.get("create_spec") == create_spec_digest(req):
        return existing, BASE_ROOTFS
    # this backend is one host with no one to test the proxy-mode netfilter
    # work against, so it refuses anything but direct egress. a refusal is
    # honest where a silent direct would leave the control plane believing
    # the vm is proxied. checked before any allocation, like the image.
    egress_mode = (req.get("egress") or {}).get("mode") or "direct"
    if egress_mode != "direct":
        raise HTTPError(400, f"egress mode {egress_mode!r} is not supported on the qemu backend; only direct egress is")
    
    # Resolve the source rootfs before any allocation, so a rejected or unknown
    # image fails fast with no vm dir, tap, forward, or gpu claim to roll back.
    image = req.get("image") or ""
    source_rootfs = resolve_image_path(image) if image else BASE_ROOTFS

    if not source_rootfs.exists():
        raise HTTPError(400, f"base image {image or 'default'!r} not found at {source_rootfs}; bake and place a rootfs there before use")

    gpu_count = int(req.get("gpus", "0"))
    gpu_kind = req.get("gpu_kind") or ""
    gpu_profile = (req.get("gpu_profile") or "").lower()
    # When the orchestrator's per-instance scheduler has picked concrete MIG
    # instance uuids, it sends them here and we bind exactly those (validating
    # they are free and match the profile/kind). When it has only a profile +
    # count (legacy / hand-managed hosts), fall back to picking locally. this
    # keeps pick_mig_devices' contract intact for back-compat.
    requested_mig_uuids = req.get("mig_instance_uuids") or []
    if gpu_profile:
        gpu_slots: list[str] = []
        if requested_mig_uuids:
            gpu_mdevs = claim_mig_devices(list(requested_mig_uuids), gpu_profile, gpu_kind or None)
        else:
            gpu_mdevs = pick_mig_devices(gpu_count, gpu_profile, gpu_kind or None)
    else:
        gpu_slots = pick_gpu_slots(gpu_count, gpu_kind or None)
        gpu_mdevs = []

    d = vm_dir(vm_id)
    if d.exists():
        raise HTTPError(409, f"vm {vm_id} already exists")
    # picked before the dir exists, so a full host leaves no empty dir
    # behind to 409 the name.
    idx = pick_index()
    host_ip, guest_ip = tap_addrs(idx)
    mac = f"06:00:AC:10:{idx:02x}:02"
    host_port = HOST_PORT_BASE + idx
    meta = {
        "vm_id": vm_id,
        "name": name,
        "index": idx,
        "cpus": int(req.get("cpus", 1)),
        "memory_mb": int(req.get("memory_mb", 1024)),
        "storage_gb": int(req.get("storage_gb", 0)),
        "region": req.get("region", ""),
        "image": image,
        "tap": tap_name(idx),
        "host_ip": host_ip,
        "guest_ip": guest_ip,
        "mac": mac,
        "rootfs": str(d / "rootfs.qcow2"),
        "host_port": host_port,
        "gpus": gpu_count,
        "gpu_kind": gpu_kind,
        "gpu_profile": gpu_profile,
        "gpu_slots": gpu_slots,
        "gpu_mdevs": gpu_mdevs,
        "url": host_authority(PUBLIC_HOST, host_port),
        "created_at": now_iso(),
        "create_spec": create_spec_digest(req),
        # set until boot_vm finishes. an agent restarted mid-create finds it
        # and undoes the create, so the index and gpus are not held forever.
        "creating": True,
    }
    try:
        d.mkdir(parents=True)
        save_meta(meta)
    except Exception:
        shutil.rmtree(d, ignore_errors=True)
        raise
    return meta, source_rootfs


def boot_vm(meta: dict, source_rootfs: Path) -> None:
    """builds the tap, copies the rootfs and boots a vm reserve_vm reserved.
    runs without _create_lock, under the vm's own lock. any failure undoes
    the whole create, which frees the index and the gpus."""
    host_ip, guest_ip = meta["host_ip"], meta["guest_ip"]
    try:
        iface = host_iface()
        setup_tap(meta["index"], iface)
        add_agent_forward(meta["host_port"], guest_ip, iface)
        # Guard against a stale multiplexed connection from a previous VM that
        # held this same guest ip (indices, and so ips, get reused).
        Path(_ssh_control_path(guest_ip)).unlink(missing_ok=True)

        rootfs = Path(meta["rootfs"])
        # Reflink when the filesystem supports copy-on-write (near-instant);
        # falls back to a full byte copy otherwise.
        sudo(["cp", "--reflink=auto", str(source_rootfs), str(rootfs)], check=False)
        if not rootfs.exists():
            shutil.copyfile(source_rootfs, rootfs)
        sudo(["chmod", "666", str(rootfs)], check=False)

        start_qemu(meta)
        save_meta(meta)
        if not wait_for_ssh(guest_ip, timeout=60.0):
            meta["ssh_ready"] = False
        else:
            meta["ssh_ready"] = True
            ssh_exec(
                guest_ip,
                "ip route show default | grep -q . || ip route add default via "
                f"{host_ip}; grep -q 1.1.1.1 /etc/resolv.conf 2>/dev/null || "
                "echo nameserver 1.1.1.1 > /etc/resolv.conf",
            )
        meta.pop("creating")
        save_meta(meta)
    except Exception:
        undo_create(meta)
        raise


def undo_create(meta: dict) -> None:
    """removes what a create built for meta and then its dir, which frees
    the index and the gpus. every step tolerates a missing piece, so this is
    safe at any point after reserve_vm, including on an agent restarted
    mid-create."""
    stop_qemu(meta)
    del_agent_forward(meta["host_port"], meta["guest_ip"])
    teardown_tap(meta["tap"])
    shutil.rmtree(vm_dir(meta["vm_id"]), ignore_errors=True)

def reapply_mig_layout() -> None:
    """Re-apply the persisted MIG layout (qemu-mig-setup.sh) so freed instances
    are re-carved and immediately available again.

    Only does anything when the host is declared lifecycle-managed
    (MIG_LIFECYCLE_MANAGED=1) and the setup script exists. Best-effort: a
    failure is logged but never raised, so a VM destroy always succeeds even
    when the card layout re-apply hiccups. the operator can re-run the script
    by hand to recover.
    """
    if not MIG_LIFECYCLE_MANAGED or not MIG_SETUP_SCRIPT.exists():
        return
    try:
        subprocess.run([str(MIG_SETUP_SCRIPT)], check=False,
                        capture_output=True, text=True, timeout=60.0)
    except (subprocess.SubprocessError, OSError):
        # destroy must not fail because the layout re-apply did.
        pass


def destroy_vm(vm_id: str) -> None:
    """Stop a vm, release its GPU/tap/DNAT allocations, and delete its state dir."""
    meta = load_meta(vm_id)
    if not meta:
        # already gone: a retried destroy succeeds rather than 404ing.
        return
    held_mdevs = bool(meta.get("gpu_mdevs"))
    stop_qemu(meta)
    if "host_port" in meta:
        del_agent_forward(meta["host_port"], meta["guest_ip"])
    for endpoint in meta.get("expose_endpoints", []):
        # endpoints written before the protocol field existed are tcp.
        del_expose_forward(endpoint["host_port"], meta["guest_ip"],
                           endpoint["port"], endpoint.get("protocol", "tcp"))
    teardown_tap(meta["tap"])
    Path(_ssh_control_path(meta["guest_ip"])).unlink(missing_ok=True)
    sudo(["rm", "-rf", str(vm_dir(vm_id))], check=False)
    # On a lifecycle-managed host, a VM that held MIG instances just freed
    # them; re-apply the layout so the card re-carves the same instances and
    # they are available for the next allocation without operator action.
    if held_mdevs:
        reapply_mig_layout()
    

# -- Snapshots (unsupported for GPU passthrough) ------------------------------
# VFIO passthrough cannot be checkpointed, so these always fail with 501. The
# qemu provider's Environment is deliberately not SnapshotCapable, so the
# orchestrator never calls these in practice; the endpoints exist only to give
# a clear, explicit answer if something does (decision D4).

def snapshot_create(vm_id: str, comment: str) -> dict:
    """Snapshot is unsupported for GPU passthrough vms. Always raises 501."""
    raise HTTPError(501, "snapshots are not supported for gpu passthrough vms")


def snapshot_list(vm_id: str) -> list[dict]:
    """Snapshot is unsupported for GPU passthrough vms. Always raises 501."""
    raise HTTPError(501, "snapshots are not supported for gpu passthrough vms")


def snapshot_restore(vm_id: str, snapshot_id: str) -> None:
    """Restore is unsupported for GPU passthrough vms. Always raises 501."""
    raise HTTPError(501, "snapshots are not supported for gpu passthrough vms")


# -- Upload / Exec / start-agent ----------------------------------------------


def do_start_agent(vm_id: str, manifest_path: str, secrets_path: str,
                   gateway: str | None = None, extra_args: str | None = None,
                   tls_cert_path: str | None = None, tls_key_path: str | None = None,
                   auth_token: str | None = None, download_url: str | None = None,
                   binary_path: str = "/usr/local/bin/fused",
                   listen: str = "0.0.0.0:9550",
                   expose: list[dict] | None = None) -> list[dict]:
    """Launch the in-guest fused agent, mirroring fc-agent's start-agent.

    Optionally fetches the fused binary via download_url (idempotent), wires the
    manifest/secrets/TLS paths and gateway/auth-token, sets up any expose
    forwards, and starts fused listening on `listen`. Returns the list of
    published endpoints (empty when nothing was exposed).
    """
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    if download_url:
        fetch = (
            f"test -x {shlex.quote(binary_path)} || "
            f"(curl -fsSL {shlex.quote(download_url)} -o {shlex.quote(binary_path)} "
            f"&& chmod +x {shlex.quote(binary_path)})"
        )
        rc, out, err = ssh_exec(meta["guest_ip"], fetch, timeout=120.0)
        if rc != 0:
            detail = err.decode(errors="replace").strip()
            stdout = out.decode(errors="replace").strip()
            if stdout:
                detail = f"{detail} | stdout: {stdout}" if detail else stdout
            raise HTTPError(500, f"start-agent download failed: {detail or 'unknown error (rc={rc})'}")
    extras = []
    if gateway:
        extras += ["--gateway", gateway]
    extras += ["--vm-id", meta["vm_id"]]
    if tls_cert_path:
        extras += ["--tls-cert", tls_cert_path]
    if tls_key_path:
        extras += ["--tls-key", tls_key_path]
    if auth_token:
        extras += ["--auth-token-file", "/fuse/auth-token"]
    if extra_args:
        extras.append(extra_args)
    extras_str = " ".join(shlex.quote(e) if " " not in e else e for e in extras)
    unit = (
        "[Unit]\n"
        "Description=Fuse in-guest agent (fused)\n"
        "After=network-online.target\n"
        "Wants=network-online.target\n\n"
        "[Service]\n"
        "Type=simple\n"
        "EnvironmentFile=-/etc/default/fused\n"
        f"ExecStart={binary_path} --listen {listen} "
        f"--manifest {shlex.quote(manifest_path)} "
        f"--secrets {shlex.quote(secrets_path)} $FUSED_EXTRA_ARGS\n"
        "Restart=on-failure\n"
        "RestartSec=2\n\n"
        "[Install]\n"
        "WantedBy=multi-user.target\n"
    )
    compose_up = compose_up_command()
    remote = (
        "export LC_ALL=C; set -e; "
        f"test -x {shlex.quote(binary_path)} || {{ echo 'agent binary not found at {binary_path}' >&2; exit 127; }}; "
        f"{compose_up}"
        f"printf '%s\\n' 'FUSED_EXTRA_ARGS={extras_str}' > /etc/default/fused; "
        f"cat > /etc/systemd/system/fused.service <<'EOF'\n{unit}EOF\n"
        "systemctl daemon-reload; "
        "systemctl enable fused >/dev/null 2>&1 || true; "
        "systemctl restart fused; "
        "sleep 0.3; systemctl is-active fused"
    )
    rc, out, err = ssh_exec(meta["guest_ip"], remote, timeout=60.0)
    if rc != 0:
        detail = err.decode(errors="replace").strip()
        stdout = out.decode(errors="replace").strip()
        if stdout:
            detail = f"{detail} | stdout: {stdout}" if detail else stdout
        raise HTTPError(500, f"agent start failed: {detail or 'unknown error (rc={rc})'}")

    # Defense-in-depth: the fusefile parser (internal/fusefile/parse.go) rejects
    # the agent's own management port and SSH before they ever reach here, but
    # we re-check on the host so a buggy or direct API caller can never DNAT
    # the control plane to the open internet.
    endpoints: list[dict] = []
    if expose:
        for entry in expose:
            guest_port = int(entry["port"])
            if guest_port in RESERVED_GUEST_PORTS:
                raise HTTPError(
                    500,
                    f"refusing to expose reserved guest port {guest_port} "
                    f"(agent management port {FUSED_PORT} or SSH 22); "
                    f"this is blocked at fusefile parse time as well",
                )
            # an omitted protocol is tcp: that is what every expose entry
            # written before the field existed meant, and the orchestrator
            # normalizes it before sending, so this covers a direct caller.
            protocol = str(entry.get("protocol") or "tcp").lower()
            if protocol not in EXPOSE_PROTOCOLS:
                raise HTTPError(
                    500,
                    f"unsupported expose protocol {protocol!r} "
                    f"(want one of {sorted(EXPOSE_PROTOCOLS)})",
                )
            host_port = _free_host_port(protocol)
            add_expose_forward(host_port, meta["guest_ip"], guest_port, protocol)
            endpoints.append({"as": entry.get("as", ""), "url": host_authority(PUBLIC_HOST, host_port), "port": guest_port, "host_port": host_port, "protocol": protocol})
        meta["expose_endpoints"] = endpoints
        save_meta(meta)
    return endpoints


# -- HTTP server --------------------------------------------------------------


def vm_public(meta: dict) -> dict:
    """Project a vm meta dict to the public response shape. host_ip and
    guest_ip are the two ends of the vm's tap, carried for parity with the
    firecracker agent; egress_mode is always direct on this backend."""
    return {
        "vm_id": meta["vm_id"],
        "url": meta.get("url", ""),
        "host_ip": meta.get("host_ip", ""),
        "guest_ip": meta.get("guest_ip", ""),
        "egress_mode": "direct",
    }


def host_capacity() -> dict:
    """Real hardware capacity of this host: cpu count, total ram, and free
    disk on the filesystem backing QEMU_DIR (where rootfs images and vm
    state live). Fuse's orchestrator probes this at registration time
    instead of trusting operator-declared --cpus/--ram-mb/--storage-gb
    flags. GPUs are probed too: count and kind come from VFIO_INVENTORY
    (matching pick_gpu_slots), and gpu_devices carries per-device detail.

    MIG inventory is reported per-instance (mig_instances) when
    MIG_INVENTORY lists carved instances, so the orchestrator can bind a
    specific instance to a specific VM instead of just a profile count
    (issue #41). the legacy mig_profiles count map is derived from the
    per-instance list as a back-compat summary.
    """
    cpus = os.cpu_count() or 1
    ram_mb = 0
    try:
        with open("/proc/meminfo") as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    ram_mb = int(line.split()[1]) // 1024
                    break
    except OSError:
        pass
    free_bytes = shutil.disk_usage(QEMU_DIR).free

    groups = read_vfio_inventory()
    gpus = sum(g["count"] for g in groups)
    kinds = {g["kind"] for g in groups}
    gpu_kind = next(iter(kinds)) if len(kinds) == 1 else ""

    mig_instances = read_mig_inventory()
    mig_profiles: dict[str, int] = {}
    for inst in mig_instances:
        mig_profiles[inst["profile"]] = mig_profiles.get(inst["profile"], 0) + 1

    cap = {
        "cpus": cpus,
        "ram_mb": ram_mb,
        "storage_gb": free_bytes // (1024 ** 3),
        "arch": goarch(),
        "gpus": gpus,
        "gpu_kind": gpu_kind,
        "gpu_devices": probe_gpu_devices(),
    }
    if mig_instances:
        cap["mig_instances"] = mig_instances
    if mig_profiles:
        cap["mig_profiles"] = mig_profiles
    return cap


IDEMPOTENCY_TTL = float(os.environ.get("QEMU_AGENT_IDEMPOTENCY_TTL", str(24 * 3600)))

agentlib.configure(
    name="qemu-agent",
    state_dir=STATE_DIR,
    vms_dir=VMS_DIR,
    ssh_control_dir=SSH_CONTROL_DIR,
    ssh_base=SSH_BASE,
    token=TOKEN,
    max_body_bytes=MAX_BODY_BYTES,
    idempotency_ttl=IDEMPOTENCY_TTL,
)


class Handler(agentlib.BaseHandler):
    server_version = "qemu-agent/0.1"
    # socketserver applies this to the connection, so a client that opens a
    # socket and then stalls mid-request is dropped instead of holding a
    # thread. do_attach clears it for its own socket.
    timeout = REQUEST_TIMEOUT


    def _dispatch(self, method: str):
        if not self._auth():
            return self._text(401, "unauthorized")
        path = self.path.split("?", 1)[0]
        query = {}
        if "?" in self.path:
            for kv in self.path.split("?", 1)[1].split("&"):
                if "=" in kv:
                    k, v = kv.split("=", 1)
                    query[k] = v

        try:
            # a keyed repeat replays the first answer instead of running again.
            replay = self._idem_claim(method, path)
            if replay is not None:
                return self._replay(replay)

            # /v1/vm collection
            if path == "/v1/vm" and method == "POST":
                req = self._read_json()
                meta = create_vm(req)
                return self._json(200, vm_public(meta))
            if path == "/v1/vm" and method == "GET":
                prefix = query.get("prefix", "")
                vms = [vm_public(m) for m in list_vms() if m["name"].startswith(prefix)]
                return self._json(200, {"vms": vms})
            # /v1/vm/{id}
            m = re.fullmatch(r"/v1/vm/([^/]+)", path)
            if m:
                vm_id = sanitize_name(m.group(1))
                if method == "GET":
                    meta = load_meta(vm_id)
                    if not meta:
                        raise HTTPError(404, "vm not found")
                    return self._json(200, vm_public(meta))
                if method == "DELETE":
                    # create_vm holds the vm's own lock for the whole create,
                    # so a delete of this vm waits for its create to finish
                    # (or fail and roll back) but never for another vm's.
                    # destroy frees the index and gpus last, by removing the
                    # vm dir, so it needs no _create_lock.
                    with vm_lock(vm_id):
                        destroy_vm(vm_id)
                    return self._text(204, "")
            # /v1/vm/{id}/<action>
            m = re.fullmatch(r"/v1/vm/([^/]+)/([a-z-]+)", path)
            if m:
                vm_id = sanitize_name(m.group(1))
                action = m.group(2)

                # Attach is handled outside vm_lock and outside the HTTP
                # response machinery: it hijacks the socket for the lifetime of
                # an interactive session, so holding the lock would wedge every
                # other operation on this VM until the human logs out.
                if action == "attach" and method == "GET":
                    self.close_connection = True
                    spec = parse_attach_spec(parse_qs(urlparse(self.path).query, keep_blank_values=True))
                    do_attach(self, vm_id, spec)
                    return None

                with vm_lock(vm_id):
                    if action == "upload" and method == "POST":
                        # Uploads carry a base64 file, so they get the larger
                        # ceiling rather than the control-plane one.
                        body = self._read_json(MAX_UPLOAD_BYTES)
                        do_upload(vm_id, body["path"], body["content_b64"])
                        return self._json(200, {"ok": True})
                    if action == "exec" and method == "POST":
                        body = self._read_json()
                        return self._json(
                            200,
                            do_exec(vm_id, body["cmd"], int(body.get("timeout_ms", 0) or 0)),
                        )
                    if action == "start-agent" and method == "POST":
                        body = self._read_json()
                        endpoints = do_start_agent(
                            vm_id,
                            body["manifest_path"],
                            body["secrets_path"],
                            gateway=body.get("gateway"),
                            extra_args=body.get("extra_args"),
                            tls_cert_path=body.get("tls_cert_path"),
                            tls_key_path=body.get("tls_key_path"),
                            auth_token=body.get("auth_token"),
                            download_url=body.get("download_url"),
                            binary_path=body.get("binary_path") or "/usr/local/bin/fused",
                            listen=body.get("listen") or "0.0.0.0:9550",
                            expose=body.get("expose"),
                        )
                        return self._json(200, {"ok": True, "endpoints": endpoints})
                    # Snapshots always 501 for gpu passthrough vms (D4).
                    if action == "snapshot" and method == "POST":
                        body = self._read_json()
                        rec = snapshot_create(vm_id, body.get("comment", ""))
                        return self._json(200, {"snapshot_id": rec["snapshot_id"]})
                    if action == "snapshots" and method == "GET":
                        return self._json(200, {"snapshots": snapshot_list(vm_id)})
                    if action == "restore" and method == "POST":
                        body = self._read_json()
                        snapshot_restore(vm_id, body["snapshot_id"])
                        return self._json(200, {"ok": True})
                    # the egress wire exists here so the answer is explicit:
                    # no backend can be brought up on this host, and there is
                    # never anything to release. create_vm already refuses
                    # proxy mode, so a release is a no-op by construction.
                    if action == "egress" and method == "POST":
                        self._read_json()
                        raise HTTPError(501, "egress backends are not supported on the qemu backend; only direct egress is")
                    if action == "egress" and method == "DELETE":
                        if not load_meta(vm_id):
                            raise HTTPError(404, "vm not found")
                        return self._text(204, "")
            # oci images are converted to ext4 on firecracker hosts only; this
            # backend boots qcow2 images and has no conversion. the
            # orchestrator never sends one here, so this is the defensive half.
            if path == "/v1/images" or path.startswith("/v1/images/"):
                if method == "POST":
                    self._read_json()
                raise HTTPError(501, "oci images are not supported on qemu hosts; convert them on a firecracker host")
            # Capacity
            if path == "/v1/capacity" and method == "GET":
                return self._json(200, host_capacity())
            # Health
            if path in ("/", "/healthz") and method == "GET":
                return self._json(200, {"ok": True, "app_name": "qemu-agent"})
            return self._text(404, "not found")
        except HTTPError as e:
            return self._text(e.code, e.msg)
        except KeyError as e:
            return self._text(400, f"missing field: {e}")
        except Exception as e:
            sys.stderr.write(f"[qemu-agent] {method} {path} failed request_id={self._request_id() or '-'}\n")
            traceback.print_exc(file=sys.stderr)
            return self._text(500, f"{e}")


    def log_message(self, fmt, *args):
        rid = self._request_id()
        suffix = f" request_id={rid}" if rid else ""
        sys.stderr.write("[qemu-agent] " + fmt % args + suffix + "\n")


def reattach_vms() -> None:
    """On agent startup, re-launch any vms whose QEMU process is gone.

    Triggered by host reboots (taps destroyed, pids gone) and agent crashes.
    VMs with a live pid are left alone. Re-acquires each vm's GPU/tap/DNAT
    allocations before restart.
    """
    if not VMS_DIR.exists():
        return
    for d in sorted(VMS_DIR.iterdir()):
        meta = load_meta(d.name)
        if not meta:
            continue
        if meta.get("creating"):
            # the agent stopped mid-create, so nobody got this vm back.
            # undo it like a failed create, which frees its index and gpus.
            print(f"[qemu-agent] reattach: undoing unfinished create of {meta['vm_id']}", flush=True)
            try:
                undo_create(meta)
            except Exception as e:
                print(f"[qemu-agent] reattach: undo of {meta['vm_id']} FAILED: {e}", flush=True)
            continue
        pid = meta.get("pid")
        sock = meta.get("sock")
        if pid and pid_alive(pid) and sock and Path(sock).exists():
            print(f"[qemu-agent] reattach: {meta['vm_id']} still running (pid {pid})", flush=True)
            continue
        print(f"[qemu-agent] reattach: relaunching {meta['vm_id']}", flush=True)
        try:
            teardown_tap(meta["tap"])
            iface = host_iface()
            tap, host_ip, guest_ip = setup_tap(meta["index"], iface)
            meta["tap"], meta["host_ip"], meta["guest_ip"] = tap, host_ip, guest_ip
            if "host_port" in meta:
                del_agent_forward(meta["host_port"], guest_ip)
                add_agent_forward(meta["host_port"], guest_ip, iface)
            start_qemu(meta)
            save_meta(meta)
        except Exception as e:
            print(f"[qemu-agent] reattach FAILED for {meta['vm_id']}: {e}", flush=True)
            traceback.print_exc(file=sys.stderr)


def main():
    reattach_vms()
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"[qemu-agent] listening :{PORT}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
