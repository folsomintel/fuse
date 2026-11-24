"""Code fc-agent.py and qemu-agent.py share.

Both agents speak the same HTTP contract and drive their guests the same way
over SSH, so everything here is backend-neutral. The few values that differ
per backend (state dir, SSH key, bearer token, env var names, log prefix) come
in through configure(), which each agent calls once at import, before it
serves a request.
"""

from __future__ import annotations

import base64
import fcntl
import hashlib
import hmac
import ipaddress
import json
import os
import platform
import pty
import re
import selectors
import shlex
import signal
import struct
import subprocess
import sys
import termios
import threading
import time
import uuid
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler
from pathlib import Path

# set by configure(). each agent owns these and hands them over at import.
NAME = "agent"
STATE_DIR: Path = None
VMS_DIR: Path = None
SSH_CONTROL_DIR: Path = None
SSH_BASE: list[str] = []
TOKEN: str | None = None
MAX_BODY_BYTES = 1 << 20
IDEMPOTENCY_TTL = 24 * 3600.0


def configure(*, name: str, state_dir: Path, vms_dir: Path, ssh_control_dir: Path,
              ssh_base: list[str], token: str | None, max_body_bytes: int,
              idempotency_ttl: float) -> None:
    """Hand this module the calling agent's backend-specific values."""
    global NAME, STATE_DIR, VMS_DIR, SSH_CONTROL_DIR, SSH_BASE, TOKEN, MAX_BODY_BYTES, IDEMPOTENCY_TTL
    NAME = name
    STATE_DIR = state_dir
    VMS_DIR = vms_dir
    SSH_CONTROL_DIR = ssh_control_dir
    SSH_BASE = ssh_base
    TOKEN = token
    MAX_BODY_BYTES = max_body_bytes
    IDEMPOTENCY_TTL = idempotency_ttl


# Port the in-guest agent listens on; per-VM host ports DNAT to this.
FUSED_PORT = int(os.environ.get("FUSED_PORT", "9550"))


# Public host resolution. The agent hands the orchestrator a bare "host:port"
# authority for each VM, so a malformed host silently produces environments
# nobody can reach. Prefer IPv4 (consumers of the url treat it as an opaque
# host:port), bracket IPv6 when that is all the host has, and fail loudly on a
# value that is neither an IP nor a plausible hostname.
_HOSTNAME_RE = re.compile(
    r"^(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
    r"(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$"
)


def host_authority(host: str, port: int | str) -> str:
    """Compose a host:port authority, bracketing host when it is IPv6."""
    try:
        if isinstance(ipaddress.ip_address(host), ipaddress.IPv6Address):
            return f"[{host}]:{port}"
    except ValueError:
        pass
    return f"{host}:{port}"


def _probe(cmd: list[str]) -> str:
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=10).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return ""


def probe_public_host() -> str:
    """Best-effort public address of this host, IPv4 first, IPv6 as fallback."""
    for cmd in (
        ["curl", "-4", "-fsS", "ifconfig.me"],
        ["bash", "-lc", "hostname -I | tr ' ' '\\n' | awk '/:/ {next} NF {print; exit}'"],
        ["curl", "-6", "-fsS", "ifconfig.me"],
        ["bash", "-lc", "hostname -I | awk '{print $1}'"],
    ):
        out = _probe(cmd)
        if not out:
            continue
        try:
            ipaddress.ip_address(out)
        except ValueError:
            continue
        return out
    return ""


def resolve_public_host() -> str:
    """PUBLIC_HOST if set (IP or hostname), else a probed IP. Never garbage."""
    override = os.environ.get("PUBLIC_HOST", "").strip()
    if override:
        try:
            ipaddress.ip_address(override)
            return override
        except ValueError:
            pass
        if _HOSTNAME_RE.match(override):
            return override
        raise SystemExit(
            f"PUBLIC_HOST is neither an IP address nor a hostname: {override!r}"
        )
    host = probe_public_host()
    if not host:
        raise SystemExit(
            "could not resolve a public IP for this host; "
            "set PUBLIC_HOST=<ip> in the agent env"
        )
    return host


_vm_locks: dict[str, threading.Lock] = {}
_locks_guard = threading.Lock()


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


# every iptables call waits for the xtables lock instead of failing when a
# parallel create holds it. bounded so a stuck holder cannot hang a create.
IPTABLES = ["iptables", "-w", "30"]


def teardown_tap(tap: str) -> None:
    sudo(["ip", "link", "del", tap], check=False)


def host_iface() -> str:
    return subprocess.check_output(
        "ip -o route get 8.8.8.8 | awk '{print $5}'", shell=True
    ).decode().strip()


def add_agent_forward(host_port: int, guest_ip: str, iface: str) -> None:
    """DNAT <host>:host_port -> guest_ip:FUSED_PORT, plus FORWARD allow."""
    rules = [
        IPTABLES + ["-t", "nat", "-I", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        # Also accept traffic arriving via lo (so 127.0.0.1:<host_port> works).
        IPTABLES + ["-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        IPTABLES + ["-I", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(FUSED_PORT), "-j", "ACCEPT"],
        IPTABLES + ["-I", "INPUT", "-p", "tcp", "--dport", str(host_port),
         "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def del_agent_forward(host_port: int, guest_ip: str) -> None:
    iface = host_iface()
    rules = [
        IPTABLES + ["-t", "nat", "-D", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        IPTABLES + ["-t", "nat", "-D", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        IPTABLES + ["-D", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(FUSED_PORT), "-j", "ACCEPT"],
        IPTABLES + ["-D", "INPUT", "-p", "tcp", "--dport", str(host_port),
         "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


# sourced ahead of every exec'd command and every attach that names a
# command. both run as a non-login `bash -c` over ssh, which reads neither
# /etc/profile.d nor /fuse/env, so the proxy variables the orchestrator
# writes to /etc/profile.d/fuse-egress.sh would otherwise reach the startup
# script and a login shell but not `fuse exec`. the guard keeps a guest
# booted by an older orchestrator, which wrote no such file, unchanged.
EGRESS_ENV_PREFIX = "[ -r /etc/profile.d/fuse-egress.sh ] && . /etc/profile.d/fuse-egress.sh; "


def _ssh_control_path(guest_ip: str) -> str:
    return str(SSH_CONTROL_DIR / f"{guest_ip}.sock")


def ssh_exec(guest_ip: str, remote_cmd: str, stdin: bytes | None = None, timeout: float = 60.0) -> tuple[int, bytes, bytes]:
    cmd = SSH_BASE + ["-o", f"ControlPath={_ssh_control_path(guest_ip)}", f"root@{guest_ip}", remote_cmd]
    try:
        cp = subprocess.run(cmd, input=stdin, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        return 124, b"", f"timeout: {e}".encode()
    return cp.returncode, cp.stdout, cp.stderr


def vm_dir(vm_id: str) -> Path:
    # vm_id is expected to already be sanitize_name()'d by every caller, but
    # this is the one choke point all vm filesystem paths pass through, so it
    # re-checks: resolve and confirm the result stays under VMS_DIR rather
    # than trusting callers not to pass something like "../../etc" through.
    vms_root = os.path.realpath(str(VMS_DIR))
    resolved = os.path.realpath(os.path.join(vms_root, vm_id))
    if not resolved.startswith(vms_root + os.sep):
        raise HTTPError(400, f"invalid vm id: {vm_id!r}")
    return Path(resolved)


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


def create_spec_digest(req: dict) -> str:
    """the digest a create is remembered by, so the same create sent again
    can be told from a different one under the same name."""
    return hashlib.sha256(json.dumps(req, sort_keys=True).encode()).hexdigest()


# Everything under this directory belongs to the guest agent rather than the
# caller: the manifest, the resolved secrets, the rendered env file, the auth
# token and the TLS key all land here. Both the API and the Fusefile compiler
# already refuse a caller file addressed into it.
RESERVED_GUEST_DIR = "/fuse"


def is_reserved_guest_path(path: str) -> bool:
    """Whether an already-normalized path lives under the reserved directory."""
    return path == RESERVED_GUEST_DIR or path.startswith(RESERVED_GUEST_DIR + "/")


def upload_remote_command(path: str) -> str:
    """Shell that writes one uploaded file, taking its content on stdin.

    Files under the reserved directory hold credentials in cleartext: the
    resolved secrets in `env`, the guest agent's own `auth-token`, the TLS key.
    The upload wire carries no mode, so every one of them used to land at the
    session's default 0644 and any unprivileged process in the guest could read
    them. They are written root-only here instead.

    The path is normalized first so a `..` segment cannot make an ordinary
    destination look reserved and turn the chmod loose on a directory outside
    /fuse. Callers cannot reach that today, since the API rejects both `..` and
    reserved paths, so this is the second line of defence rather than the only
    one.
    """
    path = os.path.normpath(path)
    parent = str(Path(path).parent)
    if not is_reserved_guest_path(path):
        return f"mkdir -p {shlex.quote(parent)} && cat > {shlex.quote(path)}"
    # The umask covers the file and any directory mkdir creates. The explicit
    # chmods cover directories that already exist, which is the common case:
    # /fuse is created by whichever upload happens to land first, and a baked
    # rootfs may ship it world readable.
    targets = " ".join(shlex.quote(d) for d in sorted({RESERVED_GUEST_DIR, parent}))
    return (
        f"umask 0077 && mkdir -p {shlex.quote(parent)} && "
        f"chmod 0700 {targets} && "
        f"cat > {shlex.quote(path)} && chmod 0600 {shlex.quote(path)}"
    )


def do_upload(vm_id: str, path: str, content_b64: str) -> None:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    try:
        data = base64.b64decode(content_b64)
    except Exception as e:
        raise HTTPError(400, f"bad base64: {e}")
    remote = upload_remote_command(path)
    rc, out, err = ssh_exec(meta["guest_ip"], remote, stdin=data, timeout=60.0)
    if rc != 0:
        raise HTTPError(500, f"upload failed: {err.decode(errors='replace')}")


def do_exec(vm_id: str, cmd: list[str], timeout_ms: int = 0) -> dict:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    if not isinstance(cmd, list) or not cmd:
        raise HTTPError(400, "cmd must be non-empty array")
    # The caller's bound wins, capped at our own ceiling so a client cannot ask
    # us to hold an SSH session open indefinitely.
    timeout = EXEC_TIMEOUT_MAX
    if timeout_ms and timeout_ms > 0:
        timeout = min(timeout_ms / 1000.0, EXEC_TIMEOUT_MAX)
    remote = EGRESS_ENV_PREFIX + " ".join(shlex.quote(c) for c in cmd)
    rc, out, err = ssh_exec(meta["guest_ip"], remote, timeout=timeout)
    return {
        "exit_code": rc,
        "stdout": base64.b64encode(out).decode(),
        "stderr": base64.b64encode(err).decode(),
    }


# The guest has no docker. The rootfs bakes in docker's compose v2 binary as a
# compose *provider* and drives podman through podman.socket (fc-bake-rootfs.sh
# enables the socket for exactly this reason). Compose still defaults to
# /var/run/docker.sock, which does not exist in the guest, so DOCKER_HOST has to
# name podman's socket explicitly. Without it compose cannot reach a runtime and
# fails the whole create, not just the service: `docker compose up` runs before
# fused starts, under `set -e`.
PODMAN_SOCKET = "/run/podman/podman.sock"


def compose_up_command() -> str:
    """Shell that starts the compose project, for a guest that declared services.

    Guarded on the file's presence: an environment with no `services` in its
    Fusefile has no compose.yaml and this is a no-op.
    """
    return (
        "if [ -f /fuse/compose.yaml ]; then "
        f"DOCKER_HOST=unix://{PODMAN_SOCKET} "
        "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d; "
        "fi; "
    )


class HTTPError(Exception):
    def __init__(self, code: int, msg: str):
        self.code = code
        self.msg = msg


EXEC_TIMEOUT_MAX = 600.0  # ceiling on any single guest command


# -- Attach (interactive pty) -------------------------------------------------
#
# The orchestrator relays a fuse-attach/1 stream between the CLI and this
# endpoint without interpreting it, so the frame codec below is one of only two
# implementations that matter (the other is the Go client). See docs/attach.md.
#
# Frames are [type:1][reserved:3][length:4 big-endian][payload:length].

ATTACH_PROTO = "fuse-attach/1"

FRAME_STDIN = 0
FRAME_STDOUT = 1
FRAME_STDERR = 2
FRAME_RESIZE = 3
FRAME_EXIT = 4

FRAME_HEADER = 8
MAX_FRAME_PAYLOAD = 1 << 20  # a bogus length must not let a peer allocate GBs


def encode_frame(ftype: int, payload: bytes) -> bytes:
    return bytes([ftype, 0, 0, 0]) + len(payload).to_bytes(4, "big") + payload


class FrameDecoder:
    """Incremental decoder: feed it socket reads, get whole frames back.

    Incremental because a TCP read has no relationship to a frame boundary --
    one read can carry half a frame or three of them.
    """

    def __init__(self) -> None:
        self.buf = bytearray()

    def feed(self, data: bytes):
        self.buf += data
        while True:
            if len(self.buf) < FRAME_HEADER:
                return
            length = int.from_bytes(self.buf[4:8], "big")
            if length > MAX_FRAME_PAYLOAD:
                raise ValueError(f"attach frame too large: {length}")
            if len(self.buf) < FRAME_HEADER + length:
                return
            ftype = self.buf[0]
            payload = bytes(self.buf[FRAME_HEADER:FRAME_HEADER + length])
            del self.buf[:FRAME_HEADER + length]
            yield ftype, payload


def set_winsize(fd: int, rows: int, cols: int) -> None:
    """Resize the pty. The kernel sends SIGWINCH to the pty's foreground
    process group, which is how ssh learns to tell the guest its new size."""
    if rows <= 0 or cols <= 0:
        return
    # A winsize field is 16 bits. A relayed resize frame carrying rows/cols
    # above 65535 would otherwise make struct.pack raise struct.error (not an
    # OSError), which the pty relay does not catch, wedging the session. Clamp
    # rather than raise: a client asking for an absurd size wants the maximum.
    rows = min(rows, 0xFFFF)
    cols = min(cols, 0xFFFF)
    packed = struct.pack("HHHH", rows, cols, 0, 0)
    try:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, packed)
    except OSError:
        pass


def parse_attach_spec(query: dict) -> dict:
    """Read the attach spec out of the request query. cmd repeats to preserve
    argv boundaries, so a command containing `ces survives the round trip."""

    def _int(name: str) -> int:
        try:
            return int(query.get(name, ["0"])[0])
        except (ValueError, IndexError):
            return 0

    tty = query.get("tty", [""])[0] in ("1", "true")
    return {
        "cmd": query.get("cmd", []),
        "tty": tty,
        "rows": _int("rows"),
        "cols": _int("cols"),
    }


def drain_buffered(rfile, sock) -> bytes:
    """Return bytes the client sent immediately after the request head.

    They are already inside rfile's buffer, where select() on the socket will
    never see them -- so without this the first keystrokes of a fast client
    would be silently swallowed.
    """
    sock.setblocking(False)
    try:
        peeked = rfile.peek(0)
        if peeked:
            return rfile.read(len(peeked))
        return b""
    except (BlockingIOError, OSError):
        return b""
    finally:
        sock.setblocking(True)


def attach_argv(guest_ip: str, cmd: list[str]) -> list[str]:
    """Build the ssh argv for an attach session.

    -tt forces a pty on the far side even though ssh's own stdin is already
    one; without it a command given to ssh runs without a terminal. An empty
    cmd means the guest's login shell, which reads /etc/profile.d itself; a
    named command runs non-login and gets the egress variables sourced ahead
    of it, the same as do_exec.
    """
    if cmd:
        return SSH_BASE + ["-tt", f"root@{guest_ip}", EGRESS_ENV_PREFIX] + list(cmd)
    return SSH_BASE + ["-tt", f"root@{guest_ip}"]


def do_attach(handler, vm_id: str, spec: dict) -> None:
    """Relay a fuse-attach/1 stream between the client socket and a pty running
    ssh into the guest.

    This owns the socket: it writes the 101 itself and the connection is not an
    HTTP conversation afterwards. It must therefore be called outside the
    per-VM lock -- an interactive session lasts as long as a human keeps it
    open, and holding the lock would block every other operation on that VM for
    the duration.
    """
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    if not spec["tty"]:
        raise HTTPError(400, "attach requires tty=1; use /exec for non-interactive commands")

    argv = attach_argv(meta["guest_ip"], spec["cmd"])

    conn = handler.connection
    # An interactive session is idle whenever nobody is typing, so the
    # request-level socket timeout must not apply to it. The selector loop
    # below is what bounds this connection instead.
    conn.settimeout(None)
    pending = drain_buffered(handler.rfile, conn)

    handler.wfile.write(
        b"HTTP/1.1 101 Switching Protocols\r\n"
        b"Upgrade: " + ATTACH_PROTO.encode() + b"\r\n"
        b"Connection: Upgrade\r\n\r\n"
    )
    handler.wfile.flush()

    # pty.fork rather than subprocess: the child needs the pty as its
    # controlling terminal in a new session, which is what makes SIGWINCH
    # delivery (and therefore window resizing) work at all.
    pid, master_fd = pty.fork()
    if pid == 0:
        try:
            # SSH_BASE[0] ("ssh"), not argv[0]: argv is built by concatenating
            # SSH_BASE with the caller-supplied cmd, and indexing into that
            # concatenation reads as an attacker-controlled exec path to static
            # analysis even though position 0 is always the literal from
            # SSH_BASE. Naming the constant directly keeps the executable
            # unambiguous.
            os.execvp(SSH_BASE[0], argv)
        except Exception:
            pass
        os._exit(127)

    set_winsize(master_fd, spec["rows"], spec["cols"])

    decoder = FrameDecoder()
    exit_code = 0
    client_gone = False
    try:
        if pending:
            _pump_client_frames(decoder, pending, master_fd)

        sel = selectors.DefaultSelector()
        sel.register(conn, selectors.EVENT_READ, "sock")
        sel.register(master_fd, selectors.EVENT_READ, "pty")

        while True:
            done = False
            for key, _ in sel.select(timeout=30.0):
                if key.data == "sock":
                    data = conn.recv(65536)
                    if not data:
                        done = True
                        client_gone = True
                        break
                    _pump_client_frames(decoder, data, master_fd)
                else:
                    try:
                        out = os.read(master_fd, 65536)
                    except OSError:
                        out = b""  # EIO on Linux once the child is gone
                    if not out:
                        done = True  # guest process exited
                        break
                    conn.sendall(encode_frame(FRAME_STDOUT, out))
            if done:
                break
    except Exception:
        # Any escape from the relay loop means we are tearing down, and the
        # child must be killed, not waited on: _reap(kill=False) would block in
        # waitpid forever on a still-live ssh -tt process.
        client_gone = True
    finally:
        exit_code = _reap(pid, kill=client_gone)
        try:
            os.close(master_fd)
        except OSError:
            pass

    # Best effort: if the client is the side that went away there is nobody
    # left to tell, and that is not a failure.
    try:
        conn.sendall(encode_frame(FRAME_EXIT, json.dumps({"exit_code": exit_code}).encode()))
    except OSError:
        pass


def _pump_client_frames(decoder: FrameDecoder, data: bytes, master_fd: int) -> None:
    for ftype, payload in decoder.feed(data):
        if ftype == FRAME_STDIN:
            os.write(master_fd, payload)
        elif ftype == FRAME_RESIZE:
            try:
                size = json.loads(payload)
                set_winsize(master_fd, int(size.get("rows", 0)), int(size.get("cols", 0)))
            except (ValueError, TypeError):
                pass
        # stdout/stderr/exit frames are server-to-client; ignore them inbound.


def _reap(pid: int, kill: bool) -> int:
    """Wait for the ssh child and turn its wait status into an exit code.

    Only kill when the client is the side that left. On a normal exit the pty
    reports EOF a hair before the child is reaped, so an unconditional SIGKILL
    would race a process that was already exiting cleanly and report 137 in
    place of its real status.
    """
    if kill:
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            pass
    try:
        _, status = os.waitpid(pid, 0)
    except OSError:
        return 0
    if os.WIFEXITED(status):
        return os.WEXITSTATUS(status)
    if os.WIFSIGNALED(status):
        return 128 + os.WTERMSIG(status)
    return 0


def goarch() -> str:
    """This host's cpu architecture in goarch vocabulary ("amd64", "arm64"),
    which is what the orchestrator's scheduler matches specs against. An
    unrecognized machine string is passed through lowercased so registration
    can name it in an error instead of silently mis-scheduling.
    """
    machine = platform.machine().lower()
    return {"x86_64": "amd64", "aarch64": "arm64"}.get(machine, machine)


# X-Request-ID values the agent will echo into its log. this is
# telemetry.RequestIDPattern in internal/telemetry/telemetry.go; a go test fails
# if the two differ.
REQUEST_ID_RE = re.compile(r"[A-Za-z0-9_-]{1,128}")


# idempotency. a mutating request carrying Idempotency-Key runs once per key:
# a repeat gets the first response back, and a repeat that arrives while the
# first is still running waits for it. answers below 500 are kept on disk for
# IDEMPOTENCY_TTL seconds, so a retry after a lost response or an agent restart
# still replays; a 5xx is not kept, so the retry runs again. the key is scoped
# to one method and path: reusing it for another request is a 422.
IDEMPOTENCY_HEADER = "Idempotency-Key"
IDEMPOTENCY_KEY_RE = re.compile(r"[A-Za-z0-9_.:/-]{1,256}")
IDEMPOTENCY_SWEEP_INTERVAL = 3600.0
_idem_lock = threading.Lock()
_idem_inflight: dict[str, threading.Event] = {}
_idem_last_sweep = 0.0
# answers whose disk write failed. kept for this process's lifetime, so a
# waiting or later repeat still replays instead of running the request again.
_idem_unsaved: dict[str, dict] = {}


def idem_dir() -> Path:
    return STATE_DIR / "idempotency"


def _idem_path(key: str) -> Path:
    return idem_dir() / (hashlib.sha256(key.encode()).hexdigest() + ".json")


def idem_load(key: str) -> dict | None:
    """the stored response for key, or None when there is none or it expired."""
    p = _idem_path(key)
    rec = _idem_unsaved.get(key)
    if rec is None:
        try:
            rec = json.loads(p.read_text())
        except (OSError, ValueError):
            return None
    if time.time() - float(rec.get("at", 0)) > IDEMPOTENCY_TTL:
        _idem_unsaved.pop(key, None)
        p.unlink(missing_ok=True)
        return None
    return rec


def idem_save(key: str, request: str, code: int, ctype: str, body: bytes) -> None:
    """store the answer for key. a failed disk write keeps it in memory and
    re-raises, so the caller can log it and repeats in this process replay."""
    rec = {"at": time.time(), "request": request, "code": code, "ctype": ctype,
           "body": base64.b64encode(body).decode()}
    try:
        # records hold full responses, exec output included, so only the
        # agent's user may read them, whatever the umask.
        d = idem_dir()
        d.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(d, 0o700)
        p = _idem_path(key)
        # a fresh file renamed into place, so a reader never sees a partial record.
        tmp = p.with_name(f"{p.name}.{uuid.uuid4().hex}.tmp")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(json.dumps(rec))
        os.replace(tmp, p)
    except OSError:
        with _idem_lock:
            _idem_unsaved[key] = rec
        raise
    idem_sweep()


def idem_sweep(now: float | None = None) -> None:
    """delete expired records, at most once per IDEMPOTENCY_SWEEP_INTERVAL."""
    global _idem_last_sweep
    now = time.time() if now is None else now
    with _idem_lock:
        if now - _idem_last_sweep < IDEMPOTENCY_SWEEP_INTERVAL:
            return
        _idem_last_sweep = now
    try:
        entries = list(idem_dir().iterdir())
    except OSError:
        return
    for p in entries:
        try:
            if now - p.stat().st_mtime > IDEMPOTENCY_TTL:
                p.unlink(missing_ok=True)
        except OSError:
            pass


def idem_begin(key: str, request: str) -> dict | None:
    """claim key for request. returns the stored response to replay, or None
    when the caller now owns the key and must run the request, then call
    idem_end. a repeat of an in-flight key waits for it to finish."""
    while True:
        with _idem_lock:
            ev = _idem_inflight.get(key)
            if ev is None:
                rec = idem_load(key)
                if rec is None:
                    _idem_inflight[key] = threading.Event()
                    return None
                if rec.get("request") != request:
                    raise HTTPError(422, f"{IDEMPOTENCY_HEADER} was already used for {rec.get('request')}")
                return rec
        ev.wait()


def idem_end(key: str) -> None:
    with _idem_lock:
        ev = _idem_inflight.pop(key, None)
    if ev is not None:
        ev.set()


# epoch fencing. a vm id is reused when an environment is destroyed and
# created again under the same task id, so a request sent for one
# incarnation can reach the next: one queued behind the vm lock after its
# caller gave up, or a retry from a step that outlived its vm. a mutating
# request may carry X-Fuse-Epoch, the incarnation it is for. one for a vm
# whose saved epoch differs is refused with 412, and so is a create for an
# epoch no newer than the newest this host has made under the id, which is
# kept on disk past the vm's destroy. a request without the header, or a vm
# made before fencing, is not checked.
EPOCH_HEADER = "X-Fuse-Epoch"
EPOCH_RE = re.compile(r"[1-9][0-9]{0,17}")
# how long the newest epoch of a destroyed vm is kept. a request delayed
# longer than this is not one the fence has to stop.
EPOCH_TOMBSTONE_TTL = 30 * 24 * 3600.0
EPOCH_SWEEP_INTERVAL = 3600.0
_epoch_lock = threading.Lock()
_epoch_last_sweep = 0.0


def epoch_dir() -> Path:
    return STATE_DIR / "epochs"


def epoch_seen(vm_id: str) -> int:
    """the newest epoch a create under vm_id has succeeded with, or 0."""
    try:
        return int((epoch_dir() / vm_id).read_text())
    except (OSError, ValueError):
        return 0


def epoch_record(vm_id: str, epoch: int) -> None:
    """keep epoch as vm_id's newest, unless a newer one is kept already."""
    with _epoch_lock:
        if epoch <= epoch_seen(vm_id):
            return
        d = epoch_dir()
        d.mkdir(parents=True, exist_ok=True)
        p = d / vm_id
        # a fresh file renamed into place, so a reader never sees a partial one.
        tmp = p.with_name(f"{vm_id}.{uuid.uuid4().hex}.tmp")
        tmp.write_text(str(epoch))
        os.replace(tmp, p)
    epoch_sweep()


def epoch_sweep(now: float | None = None) -> None:
    """drop the records of vms long destroyed, at most once per interval."""
    global _epoch_last_sweep
    now = time.time() if now is None else now
    with _epoch_lock:
        if now - _epoch_last_sweep < EPOCH_SWEEP_INTERVAL:
            return
        _epoch_last_sweep = now
    try:
        entries = list(epoch_dir().iterdir())
    except OSError:
        return
    for p in entries:
        try:
            if not (VMS_DIR / p.name).exists() and now - p.stat().st_mtime > EPOCH_TOMBSTONE_TTL:
                p.unlink(missing_ok=True)
        except OSError:
            pass


def epoch_fence_create(vm_id: str, existing: dict | None, epoch: int) -> None:
    """refuse a create for epoch unless it is the vm already there, or a new
    incarnation of an id this host has no vm under. callers hold the vm lock."""
    if not epoch:
        return
    if existing:
        have = int(existing.get("epoch") or 0)
        if have == epoch:
            return
        if have > epoch:
            raise HTTPError(412, f"vm {vm_id} is epoch {have}; a create for epoch {epoch} is stale")
        raise HTTPError(409, f"vm {vm_id} is held by an older incarnation (epoch {have or 'unset'}); destroy it first")
    seen = epoch_seen(vm_id)
    if epoch <= seen:
        raise HTTPError(412, f"vm {vm_id} was already created at epoch {seen}; a create for epoch {epoch} is stale")


def epoch_fence(vm_id: str, epoch: int) -> None:
    """refuse a request for epoch on a vm of another incarnation. a missing
    vm, or one made before fencing, is left to the handler. callers hold the
    vm lock, so the vm cannot change incarnation before the request runs."""
    if not epoch:
        return
    have = int((load_meta(vm_id) or {}).get("epoch") or 0)
    if have and have != epoch:
        raise HTTPError(412, f"vm {vm_id} is epoch {have}; this request is for epoch {epoch}")


class BaseHandler(BaseHTTPRequestHandler):
    """Request plumbing both agents share: auth, bounded JSON bodies,
    responses, idempotent replay and request ids. Each agent's Handler
    subclasses this and implements _dispatch(method) with its own routes."""

    def _auth(self) -> bool:
        return hmac.compare_digest(
            self.headers.get("Authorization", ""), f"Bearer {TOKEN}"
        )

    def _json(self, code: int, obj) -> None:
        body = json.dumps(obj).encode()
        self._remember(code, "application/json", body)
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _text(self, code: int, msg: str) -> None:
        if code == 204:
            # a 204 has no body and no Content-Length.
            self._remember(code, "", b"")
            self.send_response(code)
            self.end_headers()
            return
        body = msg.encode()
        self._remember(code, "text/plain", body)
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self, limit: int | None = None) -> dict:
        """Read and parse a bounded JSON request body.

        The orchestrator is the only intended caller, but this agent listens on
        a host port, so the body has to be bounded before it is read: a large
        Content-Length would otherwise be believed and allocated.

        Chunked bodies are rejected rather than read. Nothing Fuse sends uses
        them, and the previous code silently treated a chunked body as empty --
        which would also be a way past the limit.
        """
        if limit is None:
            limit = MAX_BODY_BYTES
        if "chunked" in self.headers.get("Transfer-Encoding", "").lower():
            raise HTTPError(411, "chunked request bodies are not supported; send Content-Length")

        try:
            n = int(self.headers.get("Content-Length", "0") or 0)
        except ValueError:
            raise HTTPError(400, "invalid Content-Length")
        if n < 0:
            raise HTTPError(400, "invalid Content-Length")
        if n > limit:
            raise HTTPError(413, f"request body of {n} bytes exceeds the {limit} byte limit")

        raw = self.rfile.read(n) if n else b"{}"
        # A short read means the client declared more than it sent. Parsing the
        # truncated remainder would be guessing at what it meant.
        if len(raw) < n:
            raise HTTPError(400, "request body shorter than Content-Length")
        self._body_read = True
        try:
            return json.loads(raw or b"{}")
        except Exception as e:
            raise HTTPError(400, f"bad JSON: {e}")

    def _idem_claim(self, method: str, path: str) -> dict | None:
        """take the request's Idempotency-Key, if it is a mutating request
        that carries one. returns a stored response to replay, or None to run
        the request; a run's answer is stored by _remember."""
        headers = getattr(self, "headers", None)
        key = headers.get(IDEMPOTENCY_HEADER, "") if headers is not None else ""
        if not key or method not in ("POST", "PUT", "DELETE"):
            return None
        if not IDEMPOTENCY_KEY_RE.fullmatch(key):
            raise HTTPError(400, f"invalid {IDEMPOTENCY_HEADER}")
        request = f"{method} {path}"
        rec = idem_begin(key, request)
        if rec is None:
            self._idem = (key, request)
        return rec

    def _epoch(self) -> int:
        """the request's X-Fuse-Epoch, or 0 when it carries none."""
        headers = getattr(self, "headers", None)
        raw = headers.get(EPOCH_HEADER, "") if headers is not None else ""
        if not raw:
            return 0
        if not EPOCH_RE.fullmatch(raw):
            raise HTTPError(400, f"invalid {EPOCH_HEADER}")
        return int(raw)

    def _fence(self, vm_id: str, method: str) -> None:
        """epoch_fence for a mutating request. reads are never fenced."""
        if method in ("POST", "PUT", "DELETE"):
            epoch_fence(vm_id, self._epoch())

    def _remember(self, code: int, ctype: str, body: bytes) -> None:
        # stored before the response is written, so a client that drops the
        # connection mid-response still gets this answer on its retry.
        idem = getattr(self, "_idem", None)
        if idem is None or code >= 500:
            return
        try:
            idem_save(idem[0], idem[1], code, ctype, body)
        except OSError as e:
            # the request already ran; failing its answer now would only
            # invite the retry this store exists to absorb.
            sys.stderr.write(f"[{NAME}] idempotency record not saved to disk, kept in memory: {e}\n")

    def _replay(self, rec: dict) -> None:
        code = int(rec["code"])
        body = base64.b64decode(rec.get("body", ""))
        self.send_response(code)
        self.send_header("Idempotent-Replayed", "true")
        if code != 204:
            self.send_header("Content-Type", rec.get("ctype") or "text/plain")
            self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if code != 204:
            self.wfile.write(body)

    def _route(self, method: str):
        self._idem = None
        try:
            return self._dispatch(method)
        finally:
            if self._idem is not None:
                idem_end(self._idem[0])
                self._idem = None

    def do_GET(self):    self._route("GET")

    def do_POST(self):   self._route("POST")

    def do_PUT(self):    self._route("PUT")

    def do_DELETE(self): self._route("DELETE")

    def _request_id(self) -> str:
        """The orchestrator's X-Request-ID, or "" when absent or malformed.

        Only the charset the orchestrator itself accepts is logged, so a
        caller cannot write arbitrary text into the agent log.
        """
        headers = getattr(self, "headers", None)
        rid = headers.get("X-Request-ID", "") if headers is not None else ""
        return rid if REQUEST_ID_RE.fullmatch(rid) else ""
