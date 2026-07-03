#!/usr/bin/env python3
"""Fuse Firecracker host agent.

Speaks the contract documented in ~/fc/README.md (POST /v1/vm, upload/exec,
snapshots, ...). Fuse's `firecracker` provider talks to this agent. One
firecracker process per VM; state persisted under ~/fc/agent-state/vms/<vm_id>/.

Auth: Authorization: Bearer $FC_AGENT_TOKEN
Transport for guest ops (upload/exec/start-agent): SSH to root@<guest_ip>.
"""
from __future__ import annotations

import base64
import copy
import hmac
import http.client
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

FC_DIR = Path(os.environ.get("FC_DIR", "/home/ubuntu/fc"))
STATE_DIR = FC_DIR / "agent-state"
VMS_DIR = STATE_DIR / "vms"
KERNEL = FC_DIR / "vmlinux.bin"
BASE_ROOTFS = Path(os.environ.get("BASE_ROOTFS", str(FC_DIR / "rootfs-fused.ext4")))
# Named rootfs images for bring-your-own-image (see internal/fusefile's
# ResourceSpec.Image): <IMAGES_DIR>/<name>.ext4. There is no OCI pull here —
# an operator bakes and places a rootfs there (e.g. with fc-bake-rootfs.sh)
# before a Fusefile can reference it by name.
IMAGES_DIR = Path(os.environ.get("IMAGES_DIR", str(FC_DIR / "images")))
SSH_KEY = FC_DIR / "ubuntu.id_rsa"
TOKEN = os.environ.get("FC_AGENT_TOKEN")
PORT = int(os.environ.get("FC_AGENT_PORT", "8090"))
FC_BIN = os.environ.get("FC_BIN", "/usr/local/bin/firecracker")
# Port the in-guest agent listens on; per-VM host ports DNAT to this.
FUSED_PORT = int(os.environ.get("FUSED_PORT", "9550"))
HOST_PORT_BASE = int(os.environ.get("FUSE_HOST_PORT_BASE", "19550"))
PUBLIC_HOST = os.environ.get("PUBLIC_HOST") or (
    subprocess.run(["curl", "-fsS", "ifconfig.me"], capture_output=True, text=True).stdout.strip()
    or subprocess.run(["bash", "-lc", "hostname -I | awk '{print $1}'"], capture_output=True, text=True).stdout.strip()
)

if not TOKEN:
    print("FC_AGENT_TOKEN must be set", file=sys.stderr)
    sys.exit(1)

VMS_DIR.mkdir(parents=True, exist_ok=True)

_vm_locks: dict[str, threading.Lock] = {}
_locks_guard = threading.Lock()
_create_lock = threading.Lock()


def vm_lock(vm_id: str) -> threading.Lock:
    with _locks_guard:
        if vm_id not in _vm_locks:
            _vm_locks[vm_id] = threading.Lock()
        return _vm_locks[vm_id]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def sanitize_name(name: str) -> str:
    s = re.sub(r"[^a-z0-9-]+", "-", name.lower()).strip("-")
    return s or "vm"


def run(cmd: list[str], check: bool = True, input_bytes: bytes | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, check=check, input=input_bytes)


def sudo(cmd: list[str], check: bool = True) -> subprocess.CompletedProcess:
    return run(["sudo", "-n"] + cmd, check=check)


# -- Firecracker HTTP client over unix socket ---------------------------------

class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path: str):
        super().__init__("localhost")
        self._p = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._p)
        self.sock = s


def fc_api(sock_path: str, method: str, path: str, body: dict | None = None, timeout: float = 5.0) -> tuple[int, bytes]:
    # Uses sudo-owned socket; simplest is curl to avoid permission issues.
    data = json.dumps(body) if body is not None else None
    args = ["sudo", "-n", "curl", "-sS", "--unix-socket", sock_path,
            "-X", method, f"http://localhost{path}",
            "-H", "Content-Type: application/json",
            "-w", "\n__HTTP_CODE__%{http_code}"]
    if data is not None:
        args += ["-d", data]
    try:
        cp = subprocess.run(args, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return 599, b"timeout"
    out = cp.stdout
    marker = b"\n__HTTP_CODE__"
    idx = out.rfind(marker)
    if idx < 0:
        return 599, out + cp.stderr
    code = int(out[idx + len(marker):].strip() or b"0")
    return code, out[:idx]


# -- Networking ---------------------------------------------------------------

def pick_index() -> int:
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
    raise RuntimeError("no free VM index")


def tap_name(idx: int) -> str:
    return f"fcv{idx}"


def setup_tap(idx: int) -> tuple[str, str, str]:
    tap = tap_name(idx)
    host_ip = f"10.200.{idx}.1"
    guest_ip = f"10.200.{idx}.2"
    sudo(["ip", "link", "del", tap], check=False)
    sudo(["ip", "tuntap", "add", tap, "mode", "tap"])
    sudo(["ip", "addr", "add", f"{host_ip}/30", "dev", tap])
    sudo(["ip", "link", "set", tap, "up"])
    host_iface = subprocess.check_output(
        "ip -o route get 8.8.8.8 | awk '{print $5}'", shell=True
    ).decode().strip()
    sudo(["sysctl", "-w", "net.ipv4.ip_forward=1"])
    # NAT rules (idempotent)
    for chk, add in [
        (["iptables", "-t", "nat", "-C", "POSTROUTING", "-o", host_iface, "-j", "MASQUERADE"],
         ["iptables", "-t", "nat", "-A", "POSTROUTING", "-o", host_iface, "-j", "MASQUERADE"]),
        (["iptables", "-C", "FORWARD", "-i", tap, "-o", host_iface, "-j", "ACCEPT"],
         ["iptables", "-I", "FORWARD", "-i", tap, "-o", host_iface, "-j", "ACCEPT"]),
        (["iptables", "-C", "FORWARD", "-i", host_iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
         ["iptables", "-I", "FORWARD", "-i", host_iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"]),
    ]:
        if sudo(chk, check=False).returncode != 0:
            sudo(add)
    return tap, host_ip, guest_ip


def teardown_tap(tap: str) -> None:
    sudo(["ip", "link", "del", tap], check=False)


def host_iface() -> str:
    return subprocess.check_output(
        "ip -o route get 8.8.8.8 | awk '{print $5}'", shell=True
    ).decode().strip()


def add_agent_forward(host_port: int, guest_ip: str) -> None:
    """DNAT <host>:host_port -> guest_ip:FUSED_PORT, plus FORWARD allow."""
    iface = host_iface()
    rules = [
        ["iptables", "-t", "nat", "-I", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        # Also accept traffic arriving via lo (so 127.0.0.1:<host_port> works).
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
    """Picks a free host port by binding to port 0 and reading it back, then
    closing the socket. There is an inherent (small) race between this and
    the DNAT rule being installed; acceptable for this host agent's level of
    simplicity, matching fc-expose.sh's own lack of allocation/conflict
    detection."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("", 0))
        return s.getsockname()[1]


def add_expose_forward(host_port: int, guest_ip: str, guest_port: int) -> None:
    """Publishes <host_port> -> guest_ip:guest_port via fc-expose.sh."""
    subprocess.run(
        ["bash", str(FC_DIR / "fc-expose.sh"), str(host_port), guest_ip, str(guest_port)],
        check=True,
    )


def del_expose_forward(host_port: int, guest_ip: str, guest_port: int) -> None:
    """Removes a forward previously installed by add_expose_forward. Best
    effort (mirrors del_agent_forward): a vm being destroyed should not fail
    to tear down over a stale firewall rule."""
    subprocess.run(
        ["bash", str(FC_DIR / "fc-expose.sh"), "-d", str(host_port), guest_ip, str(guest_port)],
        check=False,
    )


# -- SSH ----------------------------------------------------------------------

SSH_BASE = [
    "ssh", "-i", str(SSH_KEY),
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "LogLevel=ERROR",
    "-o", "ConnectTimeout=5",
    "-o", "BatchMode=yes",
]


def ssh_exec(guest_ip: str, remote_cmd: str, stdin: bytes | None = None, timeout: float = 60.0) -> tuple[int, bytes, bytes]:
    cmd = SSH_BASE + [f"root@{guest_ip}", remote_cmd]
    try:
        cp = subprocess.run(cmd, input=stdin, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        return 124, b"", f"timeout: {e}".encode()
    return cp.returncode, cp.stdout, cp.stderr


def wait_for_ssh(guest_ip: str, timeout: float = 30.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, _, _ = ssh_exec(guest_ip, "true", timeout=4.0)
        if rc == 0:
            return True
        time.sleep(1)
    return False


# -- VM lifecycle -------------------------------------------------------------

def vm_dir(vm_id: str) -> Path:
    return VMS_DIR / vm_id


def load_meta(vm_id: str) -> dict | None:
    p = vm_dir(vm_id) / "meta.json"
    if not p.exists():
        return None
    try:
        return json.loads(p.read_text())
    except Exception:
        return None


def save_meta(meta: dict) -> None:
    p = vm_dir(meta["vm_id"]) / "meta.json"
    tmp = p.with_suffix(".tmp")
    tmp.write_text(json.dumps(meta, indent=2))
    tmp.replace(p)


def list_vms() -> list[dict]:
    return [m for d in sorted(VMS_DIR.iterdir()) if (m := load_meta(d.name))]


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except Exception:
        return False


def start_firecracker(meta: dict) -> None:
    d = vm_dir(meta["vm_id"])
    sock = d / "fc.sock"
    log = d / "fc.log"
    sudo(["rm", "-f", str(sock)], check=False)
    # Launch firecracker under sudo, detach with setsid; capture pid from shell.
    cmd = (
        f"sudo -n setsid {shlex.quote(FC_BIN)} --api-sock {shlex.quote(str(sock))} "
        f">{shlex.quote(str(log))} 2>&1 & echo $!"
    )
    pid_out = subprocess.check_output(["bash", "-lc", cmd]).decode().strip()
    # The shell pid isn't the firecracker pid; discover via ss/lsof on the sock.
    time.sleep(0.3)
    for _ in range(20):
        if sock.exists():
            break
        time.sleep(0.2)
    if not sock.exists():
        raise RuntimeError(f"firecracker failed to open {sock} (see {log})")
    pgrep = subprocess.run(
        ["pgrep", "-f", f"firecracker --api-sock {sock}"],
        capture_output=True, text=True,
    )
    real_pid = int(pgrep.stdout.strip().split("\n")[0]) if pgrep.stdout.strip() else int(pid_out)
    meta["pid"] = real_pid
    meta["sock"] = str(sock)
    # Make sock readable by our user so we could inspect; curl uses sudo anyway.
    sudo(["chmod", "666", str(sock)], check=False)

    # Configure boot, drive, net, machine-config, start.
    boot_args = (
        "console=ttyS0 reboot=k panic=1 pci=off "
        f"ip={meta['guest_ip']}::{meta['host_ip']}:255.255.255.252::eth0:off"
    )
    steps = [
        ("/boot-source", {"kernel_image_path": str(KERNEL), "boot_args": boot_args}),
        ("/drives/rootfs", {"drive_id": "rootfs", "path_on_host": meta["rootfs"],
                             "is_root_device": True, "is_read_only": False}),
        ("/network-interfaces/eth0", {"iface_id": "eth0", "host_dev_name": meta["tap"],
                                        "guest_mac": meta["mac"]}),
        ("/machine-config", {"vcpu_count": meta["cpus"], "mem_size_mib": meta["memory_mb"]}),
        ("/actions", {"action_type": "InstanceStart"}),
    ]
    for path, body in steps:
        code, resp = fc_api(str(sock), "PUT", path, body)
        if code >= 300:
            raise RuntimeError(f"firecracker API {path} -> {code}: {resp!r}")


def stop_firecracker(meta: dict) -> None:
    pid = meta.get("pid")
    if pid and pid_alive(pid):
        sudo(["kill", "-9", str(pid)], check=False)
    sock = meta.get("sock")
    if sock:
        sudo(["rm", "-f", sock], check=False)


def create_vm(req: dict) -> dict:
    name = req.get("name") or f"vm-{uuid.uuid4().hex[:8]}"
    vm_id = sanitize_name(name)

    # Resolve the source rootfs before any allocation, so an unknown named
    # image fails fast with no vm dir/tap/forward left to roll back. Unset
    # (the common case today) is byte-for-byte the existing BASE_ROOTFS path.
    image = req.get("image") or ""
    source_rootfs = BASE_ROOTFS
    if image:
        source_rootfs = IMAGES_DIR / f"{image}.ext4"
        if not source_rootfs.exists():
            raise HTTPError(400, f"base image {image!r} not found at {source_rootfs}; bake and place a rootfs there before use")

    d = vm_dir(vm_id)
    if d.exists():
        raise HTTPError(409, f"vm {vm_id} already exists")
    d.mkdir(parents=True)
    (d / "snapshots").mkdir()

    idx = pick_index()
    tap, host_ip, guest_ip = setup_tap(idx)
    mac = f"06:00:AC:10:{idx:02x}:02"
    host_port = HOST_PORT_BASE + idx
    add_agent_forward(host_port, guest_ip)

    rootfs = d / "rootfs.ext4"
    # Per-VM rootfs copy (so writes don't affect other VMs).
    shutil.copyfile(source_rootfs, rootfs)
    sudo(["chmod", "666", str(rootfs)], check=False)

    meta = {
        "vm_id": vm_id,
        "name": name,
        "index": idx,
        "cpus": int(req.get("cpus", 1)),
        "memory_mb": int(req.get("memory_mb", 512)),
        "storage_gb": int(req.get("storage_gb", 0)),
        "region": req.get("region", ""),
        "tap": tap,
        "host_ip": host_ip,
        "guest_ip": guest_ip,
        "mac": mac,
        "rootfs": str(rootfs),
        "host_port": host_port,
        "url": f"{PUBLIC_HOST}:{host_port}",
        "created_at": now_iso(),
        "snapshots": [],
    }
    save_meta(meta)
    try:
        start_firecracker(meta)
        save_meta(meta)
        if not wait_for_ssh(guest_ip, timeout=30.0):
            # Non-fatal: VM booted but SSH didn't come up in time. Still report created.
            meta["ssh_ready"] = False
        else:
            meta["ssh_ready"] = True
            # Fix up guest networking: add default route + DNS if missing.
            ssh_exec(guest_ip, (
                "ip route show default | grep -q . || ip route add default via "
                f"{host_ip}; grep -q 1.1.1.1 /etc/resolv.conf 2>/dev/null || "
                "echo nameserver 1.1.1.1 > /etc/resolv.conf"
            ))
        save_meta(meta)
    except Exception:
        # Roll back on failure.
        stop_firecracker(meta)
        del_agent_forward(host_port, guest_ip)
        teardown_tap(tap)
        shutil.rmtree(d, ignore_errors=True)
        raise
    return meta


def destroy_vm(vm_id: str) -> None:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    stop_firecracker(meta)
    if "host_port" in meta:
        del_agent_forward(meta["host_port"], meta["guest_ip"])
    for endpoint in meta.get("expose_endpoints", []):
        del_expose_forward(endpoint["host_port"], meta["guest_ip"], endpoint["port"])
    teardown_tap(meta["tap"])
    sudo(["rm", "-rf", str(vm_dir(vm_id))], check=False)


# -- Snapshots (disk-only) ----------------------------------------------------
# TODO: Implement Firecracker memory snapshots (Pause + CreateSnapshot with
# snapshot_type=Full) to capture RAM + vCPU state. Current disk-only snapshots
# still require a full cold boot on restore (~5-15s). Memory snapshots would
# enable sub-second (<200ms) resume, eliminating cold start latency.

def snapshot_create(vm_id: str, comment: str) -> dict:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    snap_id = f"snap-{int(time.time())}-{uuid.uuid4().hex[:6]}"
    snap_dir = vm_dir(vm_id) / "snapshots" / snap_id
    snap_dir.mkdir(parents=True)
    # Quiesce guest FS then copy.
    ssh_exec(meta["guest_ip"], "sync; sync", timeout=10.0)
    snap_rootfs = snap_dir / "rootfs.ext4"
    sudo(["cp", "--reflink=auto", meta["rootfs"], str(snap_rootfs)], check=False)
    if not snap_rootfs.exists():
        shutil.copyfile(meta["rootfs"], snap_rootfs)
    record = {"snapshot_id": snap_id, "comment": comment, "created_at": now_iso()}
    (snap_dir / "meta.json").write_text(json.dumps(record))
    meta.setdefault("snapshots", []).append(record)
    save_meta(meta)
    return record


def snapshot_list(vm_id: str) -> list[dict]:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    return meta.get("snapshots", [])


def snapshot_restore(vm_id: str, snapshot_id: str) -> None:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    snap_rootfs = vm_dir(vm_id) / "snapshots" / snapshot_id / "rootfs.ext4"
    if not snap_rootfs.exists():
        raise HTTPError(404, "snapshot not found")
    # Stop firecracker, swap rootfs, recreate the TAP (the dead fc process
    # may still be holding it briefly), restart with same config.
    stop_firecracker(meta)
    time.sleep(0.3)
    if "host_port" in meta:
        del_agent_forward(meta["host_port"], meta["guest_ip"])
    teardown_tap(meta["tap"])
    tap, host_ip, guest_ip = setup_tap(meta["index"])
    meta["tap"], meta["host_ip"], meta["guest_ip"] = tap, host_ip, guest_ip
    if "host_port" in meta:
        add_agent_forward(meta["host_port"], guest_ip)
    sudo(["cp", str(snap_rootfs), meta["rootfs"]])
    sudo(["chmod", "666", meta["rootfs"]], check=False)
    start_firecracker(meta)
    save_meta(meta)
    wait_for_ssh(meta["guest_ip"], timeout=30.0)


# -- Upload / Exec / start-agent ---------------------------------------------

def do_upload(vm_id: str, path: str, content_b64: str) -> None:
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
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    # Optionally fetch the agent binary into the guest first (skip rootfs
    # re-bake). Idempotent: only download when the target isn't already an
    # executable.
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
    # Write a SELF-CONTAINED systemd unit (not a drop-in). A drop-in extends a
    # baked-in base fused.service; writing the whole unit here means the agent
    # also works on a plain (unbaked) rootfs as long as `fused` is present —
    # combined with download_url, that's a no-bake deploy.
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
    # Bring up any declared services before starting the main task. Guarded on
    # the compose file's presence, so environments with no `services` in their
    # Fusefile (including every existing caller of this function, and the
    # FROZEN /start-surfd path which never passes one) see no change at all.
    # `docker-compose` here is the baked podman compose provider (see
    # fc-bake-rootfs.sh) — the guest has no docker CLI.
    compose_up = (
        "if [ -f /fuse/compose.yaml ]; then "
        "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d; "
        "fi; "
    )
    remote = (
        "export LC_ALL=C; set -e; "
        # Check the absolute binary path, not `command -v fused`: a
        # non-interactive SSH shell's PATH may exclude /usr/local/bin even when
        # the binary is baked in, which would wrongly report it missing.
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

    # Publish any declared ingress ports now that the agent (and any compose
    # services) are up. Guarded on `expose` being non-empty, so callers with
    # no ports to publish (including the FROZEN /start-surfd path, which
    # never passes expose) see no change at all. Persisted onto meta so
    # destroy_vm can remove the same rules on teardown.
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


def do_start_surfd(vm_id: str, manifest_path: str, secrets_path: str,
                   gateway: str | None = None, extra_args: str | None = None,
                   tls_cert_path: str | None = None, tls_key_path: str | None = None,
                   auth_token: str | None = None) -> None:
    # Thin wrapper preserving the FROZEN /start-surfd behavior byte-for-byte:
    # fused defaults, no download. The generic launch lives in do_start_agent.
    do_start_agent(
        vm_id, manifest_path, secrets_path,
        gateway=gateway, extra_args=extra_args,
        tls_cert_path=tls_cert_path, tls_key_path=tls_key_path,
        auth_token=auth_token, download_url=None,
        binary_path="/usr/local/bin/fused", listen="0.0.0.0:9550",
    )


# -- HTTP layer ---------------------------------------------------------------

class HTTPError(Exception):
    def __init__(self, code: int, msg: str):
        self.code = code
        self.msg = msg


def vm_public(meta: dict) -> dict:
    return {"vm_id": meta["vm_id"], "url": meta.get("url", "")}


class Handler(BaseHTTPRequestHandler):
    server_version = "fc-agent/0.1"

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
                    if action == "start-surfd" and method == "POST":
                        body = self._read_json()
                        do_start_surfd(
                            vm_id,
                            body["manifest_path"],
                            body["secrets_path"],
                            gateway=body.get("gateway"),
                            extra_args=body.get("extra_args"),
                            tls_cert_path=body.get("tls_cert_path"),
                            tls_key_path=body.get("tls_key_path"),
                            auth_token=body.get("auth_token"),
                        )
                        return self._json(200, {"ok": True})
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
            # Health
            if path in ("/", "/healthz") and method == "GET":
                return self._json(200, {"ok": True, "app_name": "fc-agent"})
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
        sys.stderr.write("[fc-agent] " + fmt % args + "\n")


def reattach_vms() -> None:
    """On agent startup, re-launch any VMs whose firecracker process is gone.

    Triggered by host reboots (TAPs destroyed, pids gone) and agent crashes.
    VMs with a live pid are left alone.
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
            print(f"[fc-agent] reattach: {meta['vm_id']} still running (pid {pid})", flush=True)
            continue
        print(f"[fc-agent] reattach: relaunching {meta['vm_id']}", flush=True)
        try:
            # Recreate TAP + DNAT using the stored index/ports.
            teardown_tap(meta["tap"])
            tap, host_ip, guest_ip = setup_tap(meta["index"])
            meta["tap"], meta["host_ip"], meta["guest_ip"] = tap, host_ip, guest_ip
            if "host_port" in meta:
                del_agent_forward(meta["host_port"], guest_ip)
                add_agent_forward(meta["host_port"], guest_ip)
            start_firecracker(meta)
            save_meta(meta)
        except Exception as e:
            print(f"[fc-agent] reattach FAILED for {meta['vm_id']}: {e}", flush=True)
            traceback.print_exc(file=sys.stderr)


def main():
    reattach_vms()
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"[fc-agent] listening :{PORT}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
