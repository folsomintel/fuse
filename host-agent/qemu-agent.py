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

import base64
import hmac
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
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

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
QEMU_BIN = os.environ.get("QEMU_BIN", "/usr/bin/qemu-system-x86_64")
# Path listing bindable GPU groups, one per line ("<count> <kind> <pci_slot>...")
# as emitted by `qemu-vfio-bind.sh --list`. The agent reads it to pick free
# devices to attach at create time.
VFIO_INVENTORY = Path(os.environ.get("VFIO_INVENTORY", str(QEMU_DIR / "vfio-inventory.txt")))
# Port the in-guest agent listens on; per-VM host ports DNAT to this.
FUSED_PORT = int(os.environ.get("FUSED_PORT", "9550"))
HOST_PORT_BASE = int(os.environ.get("FUSE_HOST_PORT_BASE", "19650"))
PUBLIC_HOST = os.environ.get("PUBLIC_HOST") or (
    subprocess.run(["curl", "-fsS", "ifconfig.me"], capture_output=True, text=True).stdout.strip()
    or subprocess.run(["bash", "-lc", "hostname -I | awk '{print $1}'"], capture_output=True, text=True).stdout.strip()
)

if not TOKEN:
    print("QEMU_AGENT_TOKEN must be set", file=sys.stderr)
    sys.exit(1)

VMS_DIR.mkdir(parents=True, exist_ok=True)

_vm_locks: dict[str, threading.Lock] = {}
_locks_guard = threading.Lock()
_create_lock = threading.Lock()


# -- Small helpers ------------------------------------------------------------

def vm_lock(vm_id: str) -> threading.Lock:
    """Return the per-vm lock, creating it on first use."""
    with _locks_guard:
        if vm_id not in _vm_locks:
            _vm_locks[vm_id] = threading.Lock()
        return _vm_locks[vm_id]


def now_iso() -> str:
    """UTC timestamp in the same Z-suffixed ISO form fc-agent uses."""
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def sanitize_name(name: str) -> str:
    """Reduce an arbitrary name to a dns-safe vm id (lowercase, [a-z0-9-])."""
    s = re.sub(r"[^a-z0-9-]+", "-", name.lower()).strip("-")
    return s or "vm"


def run(cmd: list[str], check: bool = True, input_bytes: bytes | None = None) -> subprocess.CompletedProcess:
    """Run a command, capturing output (thin wrapper over subprocess.run)."""
    return subprocess.run(cmd, capture_output=True, check=check, input=input_bytes)


def sudo(cmd: list[str], check: bool = True) -> subprocess.CompletedProcess:
    """Run a command under non-interactive sudo."""
    return run(["sudo", "-n"] + cmd, check=check)


# -- VFIO / GPU inventory -----------------------------------------------------

def read_vfio_inventory() -> list[dict]:
    """Parse VFIO_INVENTORY into a list of bindable GPU groups.

    Each line is "<count> <kind> <pci_slot> [<pci_slot> ...]" as emitted by
    qemu-vfio-bind.sh --list. Returns dicts like
    {"count": 1, "kind": "a100", "slots": ["0000:17:00.0"]}.
    """
    if not VFIO_INVENTORY.exists():
        return []
    groups: list[dict] = []
    for lineno, raw in enumerate(VFIO_INVENTORY.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue 
        parts = line.split()
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
        groups.append({"count": count, "kind": kind.lower(), "slots": slots})
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
                used.add(json.loads(meta_p.read_text()["index"]))
            except Exception:
                pass 

    for i in range(1, 250):
        if i not in used:
            return i 
    raise RuntimeError("no free VM index")


def tap_name(idx: int) -> str:
    """Deterministic tap device name for a vm index."""
    return f"qv{idx}"


def setup_tap(idx: int) -> tuple[str, str, str]:
    """Create and configure the tap for a vm index; return (tap, host_ip, guest_ip)."""
    tap = tap_name(idx)
    host_ip = f"10.200.{idx}.1"
    guest_ip = f"10.200.{idx}.2"
    sudo(["ip", "link", "del", tap], check=False)
    sudo(["ip", "tuntap", "add", tap, "mode", "tap"])
    sudo(["ip", "addr", "add", f"{host_ip}/30", "dev", tap])
    sudo(["ip", "link", "set", tap, "up"])
    iface = host_iface()
    sudo(["sysctl", "-w", "net.ipv4.ip_forward=1"])
    for chk, add in [
        (["iptables", "-t", "nat", "-C", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"],
         ["iptables", "-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"]),
        (["iptables", "-C", "FORWARD", "-i", tap, "-o", iface, "-j", "ACCEPT"],
         ["iptables", "-I", "FORWARD", "-i", tap, "-o", iface, "-j", "ACCEPT"]),
        (["iptables", "-C", "FORWARD", "-i", iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
         ["iptables", "-I", "FORWARD", "-i", iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"]),
    ]:
        if sudo(chk, check=False).returncode != 0:
            sudo(add)
    return tap, host_ip, guest_ip


def teardown_tap(tap: str) -> None:
    """Delete a tap device (idempotent)."""
    sudo(["ip", "link", "del", tap], check=False)


def host_iface() -> str:
    """Return the host's default-route interface (for DNAT/masquerade rules)."""
    return subprocess.check_output(
        "ip -o route get 8.8.8.8 | awk '{print $5}'", shell=True
    ).decode().strip()


def add_agent_forward(host_port: int, guest_ip: str) -> None:
    """DNAT host_port -> guest_ip:FUSED_PORT so the orchestrator can reach fused."""
    iface = host_iface()
    rules = [
        ["iptables", "-t", "nat", "-I", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-I", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(FUSED_PORT), "-j", "ACCEPT"],
        ["iptables", "-I", "INPUT", "-p", "tcp", "--dport", str(host_port),
         "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def del_agent_forward(host_port: int, guest_ip: str) -> None:
    """Remove the DNAT rule added by add_agent_forward (idempotent)."""
    iface = host_iface()
    rules = [
        ["iptables", "-t", "nat", "-D", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-t", "nat", "-D", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-D", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(FUSED_PORT), "-j", "ACCEPT"],
        ["iptables", "-D", "INPUT", "-p", "tcp", "--dport", str(host_port),
         "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def _free_host_port() -> int:
    """Bind to port 0, read it back, then close. Small race is acceptable."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("", 0))
        return s.getsockname()[1]


def add_expose_forward(host_port: int, guest_ip: str, guest_port: int) -> None:
    """DNAT host_port -> guest_ip:guest_port for a published (exposed) guest port."""
    iface = host_iface()
    rules = [
        ["iptables", "-t", "nat", "-I", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        ["iptables", "-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        ["iptables", "-I", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(guest_port), "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def del_expose_forward(host_port: int, guest_ip: str, guest_port: int) -> None:
    """Remove an expose DNAT rule (idempotent)."""
    iface = host_iface()
    rules = [
        ["iptables", "-t", "nat", "-D", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        ["iptables", "-t", "nat", "-D", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{guest_port}"],
        ["iptables", "-D", "FORWARD", "-p", "tcp", "-d", guest_ip,
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
]


def ssh_exec(guest_ip: str, remote_cmd: str, stdin: bytes | None = None, timeout: float = 60.0) -> tuple[int, bytes, bytes]:
    """Run remote_cmd in the guest over SSH; return (rc, stdout, stderr)."""
    cmd = SSH_BASE + [f"root@{guest_ip}", remote_cmd]
    try:
        cp = subprocess.run(cmd, input=stdin, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        return 124, b"", f"timeout: {e}".encode()
    return cp.returncode, cp.stdout, cp.stderr


def wait_for_ssh(guest_ip: str, timeout: float = 30.0) -> bool:
    """Block until the guest accepts SSH or timeout elapses; return readiness."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, _, _ = ssh_exec(guest_ip, "true", timeout=4.0)
        if rc == 0:
            return True
        time.sleep(1)
    return False


# -- State persistence --------------------------------------------------------

def vm_dir(vm_id: str) -> Path:
    """Filesystem directory holding a vm's meta.json, rootfs, and logs."""
    return VMS_DIR / vm_id


def load_meta(vm_id: str) -> dict | None:
    """Load a vm's meta.json, or None if the vm is unknown."""
    p = vm_dir(vm_id) / "meta.json"
    if not p.exists():
        return None
    try:
        return json.loads(p.read_text())
    except Exception:
        return None


def save_meta(meta: dict) -> None:
    """Persist a vm's meta dict to its meta.json atomically."""
    p = vm_dir(meta["vm_id"]) / "meta.json"
    tmp = p.with_suffix(".tmp")
    tmp.write_text(json.dumps(meta, indent=2))
    tmp.replace(p)


def list_vms() -> list[dict]:
    """Return the meta dict for every known vm."""
    return [m for d in sorted(VMS_DIR.iterdir()) if (m := load_meta(d.name))]


def pid_alive(pid: int) -> bool:
    """True if a process with pid is currently running."""
    try:
        os.kill(pid, 0)
        return True
    except Exception:
        return False


# -- QEMU process lifecycle ---------------------------------------------------

def start_qemu(meta: dict) -> None:
    """Launch the QEMU process for a vm and wait until it is reachable.

    Builds the qemu-system-x86_64 argv from meta: kvm acceleration, OVMF
    firmware, the per-vm rootfs drive, a tap netdev, vcpu/memory sizing, and one
    "-device vfio-pci,host=<slot>" per assigned GPU in meta["gpu_slots"]. Detach
    with setsid; record the pid and monitor socket in meta.
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

    sudo(argv, check=True)

    time.sleep(0.3)
    pidfile = d / "qemu.pid"
    for _ in range(20):
        if pidfile.exists() and sock.exists():
            break 
        time.sleep(0.2)
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
    """Create and boot a vm from a create request.

    req carries name, cpus, memory_mb, storage_gb, region, image, and the GPU
    fields the qemu provider forwards: gpus (int) and gpu_kind (str). Resolves
    the base rootfs (BASE_ROOTFS or IMAGES_DIR/<image>.qcow2), allocates a
    per-vm index/tap/host-port, picks `gpus` free VFIO devices matching
    gpu_kind via pick_gpu_slots, copies a per-vm rootfs, then start_qemu. Rolls
    back all allocations on any failure. Returns the vm meta dict.
    """
    name = req.get("name") or f"vm-{uuid.uuid4().hex[:8]}"
    vm_id = sanitize_name(name) 
    
    image = req.get("image") or ""
    source_rootfs = BASE_ROOTFS 
    if image:
        source_rootfs = IMAGES_DIR / f"{image}.qcow2"

    if not source_rootfs.exists():
        raise HTTPError(400, f"base image {image or 'default'!r} not found at {source_rootfs}; bake and place a rootfs there before use")

    gpu_count = int(req.get("gpus", "0"))
    gpu_kind = req.get("gpu_kind") or ""
    gpu_slots = pick_gpu_slots(gpu_count, gpu_kind or None)

    d = vm_dir(vm_id)
    if d.exists():
        raise HTTPError(409, f"vm {vm_id} already exists")
    d.mkdir(parents=True)

    idx = pick_index()
    tap, host_ip, guest_ip = setup_tap(idx)
    mac = f"06:00:AC:10:{idx:02x}:02"
    host_port = HOST_PORT_BASE + idx
    add_agent_forward(host_port, guest_ip)

    rootfs = d / "rootfs.qcow2"
    shutil.copyfile(source_rootfs, rootfs)
    sudo(["chmod", "666", str(rootfs)], check=False)

    meta = {
        "vm_id": vm_id,
        "name": name,
        "index": idx,
        "cpus": int(req.get("cpus", 1)),
        "memory_mb": int(req.get("memory_mb", 1024)),
        "storage_gb": int(req.get("storage_gb", 0)),
        "region": req.get("region", ""),
        "image": image,
        "tap": tap,
        "host_ip": host_ip,
        "guest_ip": guest_ip,
        "mac": mac,
        "rootfs": str(rootfs),
        "host_port": host_port,
        "gpus": gpu_count,
        "gpu_kind": gpu_kind,
        "gpu_slots": gpu_slots,
        "url": f"{PUBLIC_HOST}:{host_port}",
        "created_at": now_iso(),
    }
    save_meta(meta)
    try:
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
            save_meta(meta)
    except Exception:
        stop_qemu(meta)
        del_agent_forward(host_port, guest_ip)
        teardown_tap(tap)
        shutil.rmtree(d, ignore_errors=True)
        raise
    return meta

def destroy_vm(vm_id: str) -> None:
    """Stop a vm, release its GPU/tap/DNAT allocations, and delete its state dir."""
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    stop_qemu(meta)
    if "host_port" in meta:
        del_agent_forward(meta["host_port"], meta["guest_ip"])
    for endpoint in meta.get("expose_endpoints", []):
        del_expose_forward(endpoint["host_port"], meta["guest_ip"], endpoint["port"])
    teardown_tap(meta["tap"])
    sudo(["rm", "-rf", str(vm_dir(vm_id))], check=False)
    


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

def do_upload(vm_id: str, path: str, content_b64: str) -> None:
    """Write base64 content to a path inside the guest (over SSH)."""
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    try:
        data = base64.b64decode(content_b64)
    except Exception as e:
        raise HTTPError(400, f"bad base64: {e}")
    remote = (
        f"mkdir -p {shlex.quote(str(Path(path).parent))} && cat > {shlex.quote(path)}"
    )
    rc, out, err = ssh_exec(meta["guest_ip"], remote, stdin=data, timeout=60.0)
    if rc != 0:
        raise HTTPError(500, f"upload failed: {err.decode(errors='replace')}")


def do_exec(vm_id: str, cmd: list[str]) -> dict:
    """Run a command in the guest; return {exit_code, stdout_b64, stderr_b64}."""
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    if not isinstance(cmd, list) or not cmd:
        raise HTTPError(400, "cmd must be non-empty array")
    remote = " ".join(shlex.quote(c) for c in cmd)
    rc, out, err = ssh_exec(meta["guest_ip"], remote, timeout=600.0)
    return {
        "exit_code": rc,
        "stdout": base64.b64encode(out).decode(),
        "stderr": base64.b64encode(err).decode(),
    }


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
    compose_up = (
        "if [ -f /fuse/compose.yaml ]; then "
        "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d; "
        "fi; "
    )
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

    endpoints: list[dict] = []
    if expose:
        for entry in expose:
            guest_port = int(entry["port"])
            host_port = _free_host_port()
            add_expose_forward(host_port, meta["guest_ip"], guest_port)
            endpoints.append({"as": entry.get("as", ""), "url": f"{PUBLIC_HOST}:{host_port}", "port": guest_port, "host_port": host_port})
        meta["expose_endpoints"] = endpoints
        save_meta(meta)
    return endpoints


# -- HTTP server --------------------------------------------------------------

class HTTPError(Exception):
    """Carries an HTTP status code and message for the router to render."""
    def __init__(self, code: int, msg: str):
        self.code = code
        self.msg = msg


def vm_public(meta: dict) -> dict:
    """Project a vm meta dict to the public {vm_id, url} response shape."""
    return {"vm_id": meta["vm_id"], "url": meta.get("url", "")}


def host_capacity() -> dict:
    """Real hardware capacity of this host: cpu count, total ram, and free
    disk on the filesystem backing QEMU_DIR (where rootfs images and vm
    state live). Fuse's orchestrator probes this at registration time
    instead of trusting operator-declared --cpus/--ram-mb/--storage-gb
    flags. GPU count/kind are not probed here (see VFIO_INVENTORY);
    capacity.gpus stays operator-declared.
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
    return {"cpus": cpus, "ram_mb": ram_mb, "storage_gb": free_bytes // (1024 ** 3)}


class Handler(BaseHTTPRequestHandler):
    server_version = "qemu-agent/0.1"

    def _auth(self) -> bool:
        return hmac.compare_digest(
            self.headers.get("Authorization", ""), f"Bearer {TOKEN}"
        )

    def _json(self, code: int, obj) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _text(self, code: int, msg: str) -> None:
        body = msg.encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self) -> dict:
        n = int(self.headers.get("Content-Length", "0") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            return json.loads(raw or b"{}")
        except Exception as e:
            raise HTTPError(400, f"bad JSON: {e}")

    def _route(self, method: str):
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
            # /v1/vm collection
            if path == "/v1/vm" and method == "POST":
                req = self._read_json()
                with _create_lock:
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
                    destroy_vm(vm_id)
                    return self._text(204, "")
            # /v1/vm/{id}/<action>
            m = re.fullmatch(r"/v1/vm/([^/]+)/([a-z-]+)", path)
            if m:
                vm_id = sanitize_name(m.group(1))
                action = m.group(2)
                with vm_lock(vm_id):
                    if action == "upload" and method == "POST":
                        body = self._read_json()
                        do_upload(vm_id, body["path"], body["content_b64"])
                        return self._json(200, {"ok": True})
                    if action == "exec" and method == "POST":
                        body = self._read_json()
                        return self._json(200, do_exec(vm_id, body["cmd"]))
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
            traceback.print_exc(file=sys.stderr)
            return self._text(500, f"{e}")

    def do_GET(self):    self._route("GET")
    def do_POST(self):   self._route("POST")
    def do_PUT(self):    self._route("PUT")
    def do_DELETE(self): self._route("DELETE")

    def log_message(self, fmt, *args):
        sys.stderr.write("[qemu-agent] " + fmt % args + "\n")


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
        pid = meta.get("pid")
        sock = meta.get("sock")
        if pid and pid_alive(pid) and sock and Path(sock).exists():
            print(f"[qemu-agent] reattach: {meta['vm_id']} still running (pid {pid})", flush=True)
            continue
        print(f"[qemu-agent] reattach: relaunching {meta['vm_id']}", flush=True)
        try:
            teardown_tap(meta["tap"])
            tap, host_ip, guest_ip = setup_tap(meta["index"])
            meta["tap"], meta["host_ip"], meta["guest_ip"] = tap, host_ip, guest_ip
            if "host_port" in meta:
                del_agent_forward(meta["host_port"], guest_ip)
                add_agent_forward(meta["host_port"], guest_ip)
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
