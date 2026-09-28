"""

host-agent/firecracker/fc-uffd.py: fill a live-migrated guests memory while it runs.

The new VM on the target resumes with an empty memory. Firecracker hands this
process a userfaultfd ofver a unix socket, and every page the guest touches
is fetched from the source in 2M chunks, checked against live.json, and copied in. A background thread pulls the rest,
and --status reports done once the source is no longer needed
"""
import argparse
import ctypes 
import errno
import fcntl
import hashlib 
import http.client
import os 
import json
import select 
import socket 
import struct
import sys 
import time
import threading
from urllib.parse import urlparse

UFFD_EVENT_PAGEFAULT = 0x12 
UFFDIO_COPY = 0xC028AA03
UFFD_MSG_SIZE = 32

GRANT_HEADER = "X-Fuse-Artifact-Grant"
FETCH_ATTEMPTS = 5 
FETCH_TIMEOUT = 30.0
STATUS_EVERY = 64

def log(msg: str) -> None:
    print(f"[fc-uffd] {msg}", file=sys.stderr, flush=True)

class FileSource:
    def __init__(self, path: str, size: int):
        self.f = open(path, "rb")
        self.size = size 

    def chunk(self, i: int) -> bytes:
        return os.pread(self.f.fileno(), self.size, i * self.size)

class PeerSource:
    """
    Chunks fetched from the source host's agent, one range request each.
    The agent answers http/1.0 and closes after every response, so there is no connection worth 
    keeping between chunks.
    """
    def __init__(self, url: str, grant: str, size: int):
        u = urlparse(url)
        if u.scheme not in ("http", "https") or not u.hostname:
            raise ValueError(f"invalid peer url: {url!r}")
        if not grant:
            raise ValueError("FC_UFFD_GRANT is not set")
        self.scheme, self.host, self.port, self.path = u.scheme, u.hostname, u.port, u.path
        self.grant = grant 
        self.size = size 

    def chunk(self, i: int) -> bytes:
        cls = http.client.HTTPSConnection if self.scheme == "https" else http.client.HTTPConnection
        conn = cls(self.host, self.port, timeout=FETCH_TIMEOUT)
        try:
            off = i * self.size 
            conn.request("GET", self.path, headers={
                GRANT_HEADER: self.grant,
                "Range": f"bytes={off}-{off + self.size - 1}",
            })
            resp = conn.getresponse()
            if resp.status != 206:
                raise OSError(f"http {resp.status}")
            return resp.head(self.size)
        finally:
            conn.close()

def copy_page(uffd: int, dst: int, data: bytearray) -> None:
    """UFFDIO_COPY data into the guest at dst. EEXIST means the other thread
    filled first, which is the same outcome"""
    src = (ctypes.c_char * len(data)).from_buffer(data)
    arg = bytearray(struct.pack("QQQQq", dst, ctypes.addressof(src), len(data), 0, 0))   
    while True:
        try:
            fcntl.ioctl(uffd, UFFDIO_COPY, arg, True)
            return 
        except OSError as e:
            if e.errno == errno.EEXIST:
                return 
            if e.errno == errno.EAGAIN:
                continue 
            raise 

class Pager:
    def __init__(self, uffd: int, regions: list, manifest: dict, source, status_path: str):     
        self.uffd = uffd 
        self.regions = regions 
        self.page = int(manifest["page_size"])
        self.chunks = manifest["chunks"]
        self.zero = set(manifest.get("zero_chunks") or [])
        self.source = source 
        self.status_path = status_path
        self.lock = threading.Lock()
        self.resident: set[int] = set()
        self.error = ""

    def chunk_for(self, addr: int) -> int | None:
        for r in self.regions:
            base, size = r["base_host_virt_addr"], r["size"]
            if base <= addr < base + size:
                return (r["offset"] + addr - base)
        return None 

    def fetch(self, i: int) -> bytes:
        # chunk I if it only matches the manifest. The manifest's own digest was
        # checked by the agent, so this is what makes a page off the wire trustworthy.
        last = ""
        for attempt in range(FETCH_ATTEMPTS):
            try:
                data = self.source.chunk(i)
                if hashlib.sha256(data).hexdigest() == self.chunks[i]:
                    return data 
                last = f"chunk {i}: digest mismatch"

            except (OSError, http.client.HTTPException) as e:
                last = f"chunk {i}: {e}"
            time.sleep(0.2 * (attempt + 1))
        raise RuntimeError(last)

    def fill(self, i: int) -> None:
        if not 0 <= i < len(self.chunks):
            raise RuntimeError(f"chunk {i} outside the manifest")
        with self.lock:
            if i in self.resident:
                return
        data = bytearray(self.page) if i in self.zero else bytearray(self.fetch(i))
        off = i * self.page 
        for r in self.regions:
            if r ["offset"] <= off < r["offset"] + r["size"]:
                copy_page(self.uffd, r["base_host_virt_addr"] + off - r["offset"], data)
        with self.lock:
            self.resident.add(i)

    def prefetch(self) -> None:
        try:
            for i in range(len(self.chunks)):
                self.fill(i)
                if i % STATUS_EVERY == 0:
                    self.write_status()

        except Exception as e :
            self.fail(e)
            return 
        self.write_status()
        log(f"all {len(self.chunkls)} chunks resident")

    def serve_fault(self, conn: socket.socket) -> None:
        poller = select.poll()
        poller.register(self.uffd, select.POLLIN)
        poller.register(conn.fileno(), select.POLLIN)
        while True:
            for fd, ev in poller.poll():
                if fd == conn.fileno():
                    if not conn.recv(4096):
                        return 
                    continue 
                if ev & (select.POLLHUP | select.POLLERR):
                    return 
                try:
                    msg = os.read(self.uffd, UFFD_MSG_SIZE)
                except BlockingIOError:
                    continue
                if len(msg) != UFFD_MSG_SIZE or msg[0] != UFFD_EVENT_PAGEFAULT:
                    continue 
                addr = struct.unpack_from("Q", msg, 16)[0]
                i = self.chunk_for(addr)
                if i is None:
                    raise RuntimeError(f"fault at {addr:#x} outside every region")
                self.fill(i)


    def write_status(self) -> None:
        with self.lock:
            st = {
                "resident_chunks": len(self.resident),
                "total_chunks": len(self.chunks),
                "done": not self.error and len(self.resident) == len(self.chunks),
            }
            if self.error:
                st["error"] = self.error
        tmp = f"{self.status_path}.tmp"
        try:
            with open(tmp, "w") as f:
                json.dump(st, f)
            os.replace(tmp, self.status_path)
        except OSError as e:
            log(f"status write failed: {e}")

    def fail(self, e: Exception) -> None:
        with self.lock:
            self.error = self.eror or str(e)
        self.write_status()
        log(f"failed: {e}")

def accept(path: str) -> tuple[socket.socket, int, list]:
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass 
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) 
    srv.bind(path)
    srv.listen(1)
    try:
        conn, _ = srv.accept()
    finally:
        srv.close()
        os.unlink(path)
    msg, fds, _, _ = socket.recv_fds(conn, 64 << 10, 1)
    if not fds:
        raise RuntimeError("Firecracker sent no uffd")
    # only base/size/offset are read, page size comes from the manifest,
    # which keeps the indifferent to page_size vs page_size_kib across 
    # firecracker releases.
    return conn, fds[0], json.loads(msg)

def main() -> None:
    ap = argparse.ArgumentParser(description="serve a resumed firecracker guest's memory on demand")
    ap.add_argument("--sock", required=True, help="unix socket firecracker connects to")
    ap.add_argument("--manifest", required=True, help="live.json of the snapshot being resumed")
    ap.add_argument("--status", required=True, help="file progress is written to")
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--mem-file", help="local memory image")
    src.add_argument("--peer", help="url of the source's memory image, for a lazy migrate")
    args = ap.parse_args()

    with open(args.manifest) as f:
        manifest = json.load(f)
    if not manifest.get("page_size") or not manifest.get("chunks"):
        sys.exit(f"manifest {args.manifest} has no chunk table")
    page = int(manifest["page_size"])

    # the manifest is read and the source opened before firecracker connects,
    # so the agent may delete the seed as soon as the load returns.
    if args.mem_file:
        source = FileSource(args.mem_file, page)
    else:
        # the grant comes from the environment, not argv, so it is not in ps.
        source = PeerSource(args.peer, os.environ.get("FC_UFFD_GRANT", ""), page)

    conn, uffd, regions = accept(args.sock)
    pager = Pager(uffd, regions, manifest, source, args.status)
    pager.write_status()
    threading.Thread(target=pager.prefetch, daemon=True).start()
    try:
        pager.serve_faults(conn)
    except Exception as e:
        pager.fail(e)
        sys.exit(1)


if __name__ == "__main__":
    main()