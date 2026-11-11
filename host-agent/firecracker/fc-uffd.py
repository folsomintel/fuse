"""

host-agent/firecracker/fc-uffd.py: fill a live-migrated guests memory while it runs.

The new VM on the target resumes with an empty memory. Firecracker hands this
process a userfaultfd over a unix socket, and every page the guest touches
is fetched from the source in 2M chunks, checked against live.json, and copied in. Faults are served by a
pool of workers, so vcpus waiting on different chunks wait in parallel and vcpus waiting on the same chunk
share one fetch. A background thread pulls the rest,
and --status reports done once the source is no longer needed

all-zero chunks (holes in the image, which is what unplugged virtio-mem
blocks are) are never prefetched, only zero-filled if the guest touches them,
so a shrunk guest does not come back at full size on the host. a block the
guest unplugs after the resume arrives as UFFD_EVENT_REMOVE, and its next
touch is zero-filled rather than refilled with the snapshot's old bytes.
"""
import argparse
import ctypes 
import errno
import fcntl
import hashlib 
import http.client
import os 
import json
import queue
import select 
import socket 
import struct
import sys 
import time
import threading
from urllib.parse import urlparse

UFFD_EVENT_PAGEFAULT = 0x12 
UFFD_EVENT_REMOVE = 0x15
UFFDIO_COPY = 0xC028AA03
UFFD_MSG_SIZE = 32

GRANT_HEADER = "X-Fuse-Artifact-Grant"
FETCH_ATTEMPTS = 5 
FETCH_TIMEOUT = 30.0
STATUS_EVERY = 64
# threads serving page faults. each holds one fetch in flight, so this many
# vcpus can wait on different chunks at once.
FAULT_WORKERS = int(os.environ.get("FC_UFFD_WORKERS", "8"))

def log(msg: str) -> None:
    print(f"[fc-uffd] {msg}", file=sys.stderr, flush=True)

class FileSource:
    def __init__(self, path: str, size: int):
        self.f = open(path, "rb")
        self.size = size 
        self.count = -(-os.fstat(self.f.fileno()).st_size // size)

    def chunk(self, i: int) -> bytes:
        return os.pread(self.f.fileno(), self.size, i * self.size)

    def holes(self) -> set[int]:
        """chunks that lie wholly in a hole of the image, for a file with no
        chunk table to name its zero chunks (a snapshot merged from a delta)."""
        fd, out, off = self.f.fileno(), set(), 0
        end = self.count * self.size
        while off < end:
            try:
                data = os.lseek(fd, off, os.SEEK_DATA)
            except OSError:
                data = end
            out.update(range(-(-off // self.size), min(data, end) // self.size))
            if data >= end:
                break
            off = os.lseek(fd, data, os.SEEK_HOLE)
        return out

class BadChunk(OSError):
    """the source answered, and the answer was not the chunk."""


class PeerSource:
    """
    Chunks fetched from the source host's agent, one range request each.
    The agent speaks http/1.1 keep-alive, so each thread that fetches (every
    fault worker, and the prefetcher) keeps one connection of its own and
    reuses it chunk after chunk.
    """
    def __init__(self, url: str, grant: str, size: int):
        u = urlparse(url)
        if u.scheme not in ("http", "https") or not u.hostname:
            raise ValueError(f"invalid peer url: {url!r}")
        if not grant:
            raise ValueError("FC_UFFD_GRANT is not set")
        # the query pins the source snapshot (see artifact_file in fc-agent).
        path = u.path + (f"?{u.query}" if u.query else "")
        self.scheme, self.host, self.port, self.path = u.scheme, u.hostname, u.port, path
        self.grant = grant 
        self.size = size 
        self.local = threading.local()

    def connect(self) -> http.client.HTTPConnection:
        cls = http.client.HTTPSConnection if self.scheme == "https" else http.client.HTTPConnection
        return cls(self.host, self.port, timeout=FETCH_TIMEOUT)

    def chunk(self, i: int) -> bytes:
        conn = getattr(self.local, "conn", None)
        if conn is not None:
            try:
                return self.request(conn, i)
            except BadChunk:
                raise
            except (OSError, http.client.HTTPException):
                # the agent drops a connection that sat idle past its request
                # timeout. that is not the source failing, so one fresh
                # connection gets the chunk before fetch's retries count it.
                pass
        self.local.conn = self.connect()
        return self.request(self.local.conn, i)

    def request(self, conn: http.client.HTTPConnection, i: int) -> bytes:
        try:
            off = i * self.size 
            conn.request("GET", self.path, headers={
                GRANT_HEADER: self.grant,
                "Range": f"bytes={off}-{off + self.size - 1}",
            })
            resp = conn.getresponse()
            if resp.status != 206:
                raise BadChunk(f"http {resp.status}")
            # one byte over the chunk is enough to tell a wrong length.
            data = resp.read(self.size + 1)
            if len(data) != self.size:
                raise BadChunk(f"chunk of {len(data)} bytes, not {self.size}")
            return data
        except BaseException:
            # whatever is left of a failed exchange is not reusable.
            conn.close()
            self.local.conn = None
            raise

def copy_page(uffd: int, dst: int, data: bytearray, removed=lambda: False) -> None:
    """UFFDIO_COPY data into the guest at dst. EEXIST means the other thread
    filled first, which is the same outcome. EAGAIN means an event such as a
    remove is waiting to be read; if it removed this chunk, the bytes in hand
    are stale and zeros go in instead."""
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
                if removed():
                    data[:] = bytes(len(data))
                time.sleep(0.001)
                continue 
            raise 

class Pager:
    def __init__(self, uffd: int, regions: list, manifest: dict, source, status_path: str):     
        self.uffd = uffd 
        self.regions = regions 
        self.page = int(manifest["page_size"])
        # no chunk table means a local file the agent already verified (a
        # snapshot merged from a delta): nothing to check its chunks against.
        self.chunks = manifest.get("chunks")
        self.total = len(self.chunks) if self.chunks else source.count
        self.zero = set(manifest.get("zero_chunks") or []) if self.chunks else source.holes()
        self.source = source 
        self.status_path = status_path
        self.lock = threading.Lock()
        self.resident: set[int] = set()
        # chunks the guest gave back since the resume. the host dropped their
        # pages, so they read as zeros from here on.
        self.removed: set[int] = set()
        # chunk -> set once the thread filling it is done. a second fault on
        # the chunk waits on it instead of fetching the chunk again.
        self.inflight: dict[int, threading.Event] = {}
        self.error = ""
        # the first error a fault worker hit, which ends serve_faults.
        self.fault_error: Exception | None = None

    def chunk_for(self, addr: int) -> int | None:
        for r in self.regions:
            base, size = r["base_host_virt_addr"], r["size"]
            if base <= addr < base + size:
                return (r["offset"] + addr - base) // self.page
        return None 

    def fetch(self, i: int) -> bytes:
        # chunk I if it only matches the manifest. The manifest's own digest was
        # checked by the agent, so this is what makes a page off the wire trustworthy.
        last = ""
        for attempt in range(FETCH_ATTEMPTS):
            try:
                data = self.source.chunk(i)
                if not self.chunks or hashlib.sha256(data).hexdigest() == self.chunks[i]:
                    return data 
                last = f"chunk {i}: digest mismatch"

            except (OSError, http.client.HTTPException) as e:
                last = f"chunk {i}: {e}"
            time.sleep(0.2 * (attempt + 1))
        raise RuntimeError(last)

    def fill(self, i: int) -> None:
        """make chunk i resident. one thread at a time fills a chunk; the
        others wait for it, and fill it themselves only if it failed."""
        if not 0 <= i < self.total:
            raise RuntimeError(f"chunk {i} outside the manifest")
        while True:
            with self.lock:
                if i in self.resident:
                    return
                done = self.inflight.get(i)
                if done is None:
                    self.inflight[i] = threading.Event()
                    break
            done.wait()
        try:
            self.copy_in(i)
        finally:
            with self.lock:
                self.inflight.pop(i).set()

    def copy_in(self, i: int) -> None:
        with self.lock:
            blank = i in self.zero or i in self.removed
        data = bytearray(self.page) if blank else bytearray(self.fetch(i))
        removed = lambda: self.is_removed(i)
        # a remove can land while the chunk was being fetched.
        if not blank and removed():
            data = bytearray(self.page)
        off = i * self.page 
        for r in self.regions:
            if r ["offset"] <= off < r["offset"] + r["size"]:
                copy_page(self.uffd, r["base_host_virt_addr"] + off - r["offset"], data, removed)
        with self.lock:
            self.resident.add(i)

    def is_removed(self, i: int) -> bool:
        with self.lock:
            return i in self.removed

    def remove(self, start: int, end: int) -> None:
        """the guest gave [start, end) back. a later touch of any chunk in it
        has to read zeros, never the snapshot's old bytes, and a fill already
        in flight has to see it (copy_page's EAGAIN check)."""
        addr = start
        while addr < end:
            i = self.chunk_for(addr)
            if i is not None:
                with self.lock:
                    self.removed.add(i)
                    self.resident.discard(i)
            addr = (addr // self.page + 1) * self.page

    def settled(self) -> int:
        """chunks that no longer need the source: copied in, or zeros."""
        return len(self.resident | self.zero | self.removed)

    def prefetch(self) -> None:
        try:
            for i in range(self.total):
                # holes stay unpopulated until the guest touches them.
                with self.lock:
                    hole = i in self.zero or i in self.removed
                if not hole:
                    self.fill(i)
                if i % STATUS_EVERY == 0:
                    self.write_status()

        except Exception as e :
            self.fail(e)
            return 
        self.write_status()
        log(f"all {self.total} chunks settled")

    def serve_faults(self, conn: socket.socket, workers: int = FAULT_WORKERS) -> None:
        """read uffd events and hand each fault to a pool of workers, so a
        vcpu waiting on the network does not hold up the others. removes are
        applied here, in order with the faults around them."""
        faults: queue.Queue = queue.Queue()
        for _ in range(max(1, workers)):
            threading.Thread(target=self.fault_worker, args=(faults,), daemon=True).start()
        poller = select.poll()
        poller.register(self.uffd, select.POLLIN)
        poller.register(conn.fileno(), select.POLLIN)
        while True:
            # the timeout is only so a failed worker is noticed.
            for fd, ev in poller.poll(200):
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
                if len(msg) != UFFD_MSG_SIZE:
                    continue
                if msg[0] == UFFD_EVENT_REMOVE:
                    start, end = struct.unpack_from("QQ", msg, 8)
                    self.remove(start, end)
                    continue
                if msg[0] != UFFD_EVENT_PAGEFAULT:
                    continue 
                addr = struct.unpack_from("Q", msg, 16)[0]
                i = self.chunk_for(addr)
                if i is None:
                    raise RuntimeError(f"fault at {addr:#x} outside every region")
                faults.put(i)
            with self.lock:
                err = self.fault_error
            if err:
                raise err

    def fault_worker(self, faults: queue.Queue) -> None:
        while True:
            i = faults.get()
            try:
                self.fill(i)
            except Exception as e:
                with self.lock:
                    self.fault_error = self.fault_error or e
                return

    def write_status(self) -> None:
        settled = self.settled()
        with self.lock:
            st = {
                "resident_chunks": settled,
                "total_chunks": self.total,
                "done": not self.error and settled == self.total,
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
            self.error = self.error or str(e)
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
    # a peer's chunks are only trusted against the chunk table.
    if not manifest.get("page_size") or (args.peer and not manifest.get("chunks")):
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