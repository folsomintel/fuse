"""tests for fc-uffd serving faults concurrently.

faults go to a pool of workers: vcpus waiting on different chunks wait in
parallel, vcpus waiting on the same chunk share one fetch, and each thread
keeps one keep-alive connection to the source agent. copy_page is stubbed
(the ioctl needs linux and a real guest) and a pipe stands in for the uffd.
stdlib-only, no VMs.
"""
import hashlib
import importlib.util
import json
import os
import socket
import struct
import tempfile
import threading
import time
import unittest
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock


_import_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"


def load(name: str, filename: str):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).parent.parent / filename)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


fc_agent = load("fc_agent", "fc-agent.py")
fc_uffd = load("fc_uffd", "fc-uffd.py")

PAGE = 4096
fc_agent.HUGE_PAGE_BYTES = PAGE
BASE = 0x10000000
CHUNKS = 4
# fc-uffd's retry backoff is patched out, which patches time.sleep for
# everyone; the tests' own waits keep the real one.
pause = time.sleep
MEM = b"".join(bytes([ord("a") + i]) * PAGE for i in range(CHUNKS))


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def fault(addr: int) -> bytes:
    # struct uffd_msg: event, then the pagefault's flags and address.
    return struct.pack("B7xQQQ", fc_uffd.UFFD_EVENT_PAGEFAULT, 0, addr, 0)


def remove(start: int, end: int) -> bytes:
    return struct.pack("B7xQQQ", fc_uffd.UFFD_EVENT_REMOVE, start, end, 0)


class GatedSource:
    """a source whose fetches block until released, counting each one."""

    def __init__(self):
        self.calls = []
        self.lock = threading.Lock()
        self.release = threading.Event()
        self.started = threading.Condition(self.lock)
        self.fail_first = 0

    def chunk(self, i: int) -> bytes:
        with self.lock:
            self.calls.append(i)
            self.started.notify_all()
            fail = self.fail_first > 0
            self.fail_first -= 1
        if not self.release.wait(10):
            raise OSError("never released")
        if fail:
            raise OSError("source down")
        return MEM[i * PAGE:(i + 1) * PAGE]

    def wait_calls(self, n: int) -> bool:
        with self.lock:
            return self.started.wait_for(lambda: len(self.calls) >= n, 5)


class Base(unittest.TestCase):
    def setUp(self):
        self.manifest = {"page_size": PAGE, "chunks": [sha(MEM[i:i + PAGE]) for i in range(0, len(MEM), PAGE)],
                         "zero_chunks": []}
        self.regions = [{"base_host_virt_addr": BASE, "size": len(MEM), "offset": 0}]
        self.status = Path(tempfile.mkdtemp()) / "uffd.json"
        self.copies = []
        self.copies_lock = threading.Lock()

        def copy(uffd, dst, data, removed=None):
            with self.copies_lock:
                self.copies.append((dst, bytes(data)))

        for patcher in (mock.patch.object(fc_uffd, "copy_page", copy),
                        mock.patch.object(fc_uffd.time, "sleep")):
            patcher.start()
            self.addCleanup(patcher.stop)

    def pager(self, source, uffd: int = -1) -> "fc_uffd.Pager":
        return fc_uffd.Pager(uffd, self.regions, self.manifest, source, str(self.status))

    def run_threads(self, targets) -> tuple[list, list]:
        errors = []

        def wrap(fn):
            try:
                fn()
            except Exception as e:
                errors.append(e)

        threads = [threading.Thread(target=wrap, args=(t,)) for t in targets]
        for t in threads:
            t.start()
        return threads, errors


class SharedFetchTest(Base):
    def test_vcpus_faulting_on_one_chunk_share_one_fetch(self):
        source = GatedSource()
        p = self.pager(source)
        threads, errors = self.run_threads([lambda: p.fill(2)] * 4)
        self.assertTrue(source.wait_calls(1))
        # the other three are waiting on the first fetch, not making their own.
        pause(0.1)
        self.assertEqual(source.calls, [2])
        source.release.set()
        for t in threads:
            t.join(5)
        self.assertEqual(errors, [])
        self.assertEqual(source.calls, [2])
        self.assertEqual(self.copies, [(BASE + 2 * PAGE, MEM[2 * PAGE:3 * PAGE])])
        self.assertIn(2, p.resident)
        self.assertEqual(p.inflight, {})

    def test_different_chunks_are_fetched_at_once(self):
        source = GatedSource()
        p = self.pager(source)
        threads, errors = self.run_threads([lambda i=i: p.fill(i) for i in range(CHUNKS)])
        # every fetch starts before any finishes: serial fills would stall
        # here with one call made.
        self.assertTrue(source.wait_calls(CHUNKS), source.calls)
        source.release.set()
        for t in threads:
            t.join(5)
        self.assertEqual(errors, [])
        self.assertEqual(sorted(source.calls), list(range(CHUNKS)))
        self.assertEqual(p.resident, set(range(CHUNKS)))

    def test_a_waiter_fetches_the_chunk_itself_when_the_first_fetch_fails(self):
        source = GatedSource()
        # every attempt of the first fill fails, then the source comes back.
        source.fail_first = fc_uffd.FETCH_ATTEMPTS
        p = self.pager(source)
        first, first_errors = self.run_threads([lambda: p.fill(1)])
        self.assertTrue(source.wait_calls(1))
        second, second_errors = self.run_threads([lambda: p.fill(1)])
        source.release.set()
        for t in first + second:
            t.join(5)
        self.assertEqual(len(first_errors), 1)
        self.assertEqual(second_errors, [])
        self.assertEqual(len(source.calls), fc_uffd.FETCH_ATTEMPTS + 1)
        self.assertIn(1, p.resident)

    def test_a_remove_during_a_fill_leaves_the_chunk_to_its_next_fault(self):
        source = GatedSource()
        p = self.pager(source)
        threads, errors = self.run_threads([lambda: p.fill(0)])
        self.assertTrue(source.wait_calls(1))
        p.remove(BASE, BASE + PAGE)
        source.release.set()
        for t in threads:
            t.join(5)
        self.assertEqual(errors, [])
        # zeros went in, and the chunk is not claimed resident: had it been,
        # the next fault on it would be dropped and that vcpu never woken.
        self.assertEqual(self.copies, [(BASE, bytes(PAGE))])
        self.assertNotIn(0, p.resident)
        p.fill(0)
        self.assertEqual(self.copies[-1], (BASE, bytes(PAGE)))


class ServeFaultsTest(Base):
    def start(self, source, workers: int = 4):
        r, w = os.pipe()
        self.addCleanup(os.close, w)
        self.addCleanup(os.close, r)
        ours, theirs = socket.socketpair()
        self.addCleanup(ours.close)
        p = self.pager(source, uffd=r)
        result = {}

        def serve():
            try:
                p.serve_faults(theirs, workers)
            except Exception as e:
                result["error"] = e
            finally:
                theirs.close()

        t = threading.Thread(target=serve, daemon=True)
        t.start()
        return p, w, ours, t, result

    def wait(self, cond) -> bool:
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if cond():
                return True
            pause(0.01)
        return False

    def test_faults_on_different_chunks_are_served_in_parallel(self):
        source = GatedSource()
        p, w, conn, t, result = self.start(source)
        for i in range(CHUNKS):
            os.write(w, fault(BASE + i * PAGE + 8))
        self.assertTrue(source.wait_calls(CHUNKS), source.calls)
        source.release.set()
        self.assertTrue(self.wait(lambda: p.resident == set(range(CHUNKS))))
        conn.close()
        t.join(5)
        self.assertFalse(t.is_alive())
        self.assertEqual(result, {})

    def test_repeat_faults_on_one_chunk_cost_one_fetch(self):
        source = GatedSource()
        p, w, conn, t, result = self.start(source)
        for _ in range(4):
            os.write(w, fault(BASE + PAGE + 100))
        self.assertTrue(source.wait_calls(1))
        source.release.set()
        self.assertTrue(self.wait(lambda: 1 in p.resident))
        conn.close()
        t.join(5)
        self.assertEqual(source.calls, [1])
        self.assertEqual(len(self.copies), 1)

    def test_repeat_faults_do_not_hold_up_another_chunk(self):
        source = GatedSource()
        p, w, conn, t, result = self.start(source, workers=2)
        for _ in range(4):
            os.write(w, fault(BASE + PAGE))
        os.write(w, fault(BASE + 2 * PAGE))
        # with every repeat taking a worker, both workers would sit on chunk 1
        # and chunk 2 would wait behind them.
        self.assertTrue(source.wait_calls(2), source.calls)
        source.release.set()
        self.assertTrue(self.wait(lambda: p.resident == {1, 2}))
        conn.close()
        t.join(5)
        self.assertEqual(sorted(source.calls), [1, 2])

    def test_a_repeat_fault_after_a_remove_mid_fill_gets_its_own_fill(self):
        source = GatedSource()
        p, w, conn, t, result = self.start(source)
        os.write(w, fault(BASE))
        self.assertTrue(source.wait_calls(1))
        # the guest gives the chunk back and touches it again while the first
        # fill is still fetching: that touch is folded into the first fault,
        # and still has to be served.
        os.write(w, remove(BASE, BASE + PAGE))
        os.write(w, fault(BASE))
        self.assertTrue(self.wait(lambda: p.pending.get(0) is True))
        source.release.set()
        self.assertTrue(self.wait(lambda: 0 in p.resident))
        conn.close()
        t.join(5)
        self.assertEqual(self.copies, [(BASE, bytes(PAGE)), (BASE, bytes(PAGE))])
        self.assertEqual(p.pending, {})

    def test_a_remove_reaches_the_pager_in_order(self):
        source = GatedSource()
        source.release.set()
        p, w, conn, t, result = self.start(source)
        os.write(w, fault(BASE))
        self.assertTrue(self.wait(lambda: 0 in p.resident))
        os.write(w, remove(BASE, BASE + PAGE))
        os.write(w, fault(BASE))
        self.assertTrue(self.wait(lambda: len(self.copies) == 2))
        conn.close()
        t.join(5)
        self.assertEqual(self.copies[1], (BASE, bytes(PAGE)))
        self.assertEqual(source.calls, [0])

    def test_a_worker_that_fails_ends_serving(self):
        source = GatedSource()
        source.fail_first = fc_uffd.FETCH_ATTEMPTS
        source.release.set()
        p, w, conn, t, result = self.start(source)
        os.write(w, fault(BASE))
        t.join(5)
        self.assertFalse(t.is_alive())
        self.assertIn("source down", str(result.get("error")))


class PeerKeepAliveTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.peer = f"http://127.0.0.1:{cls.server.server_address[1]}"
        d = fc_agent.SNAPSHOTS_DIR / "src"
        d.mkdir(parents=True)
        (d / "rootfs.ext4").write_bytes(b"rootfs src")
        (d / "mem").write_bytes(MEM)
        cls.digest = fc_agent.file_digest(d / "rootfs.ext4")
        (d / "meta.json").write_text(json.dumps({"snapshot_id": "src", "digest": cls.digest, "kind": "live"}))

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def source(self) -> "fc_uffd.PeerSource":
        expiry, nonce = str(int(time.time()) + 60), "ab" * 16
        grant = f"v1.{self.digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(self.digest, expiry, nonce)}"
        src = fc_uffd.PeerSource(f"{self.peer}/v1/artifacts/{self.digest}/files/mem?snapshot=src", grant, PAGE)
        self.connects = []
        real = src.connect

        def counted():
            conn = real()
            self.connects.append(threading.get_ident())
            self.addCleanup(conn.close)
            return conn

        src.connect = counted
        return src

    def test_one_connection_serves_every_chunk(self):
        src = self.source()
        for i in (3, 1, 0, 2):
            self.assertEqual(src.chunk(i), MEM[i * PAGE:(i + 1) * PAGE])
        self.assertEqual(len(self.connects), 1)

    def test_each_thread_keeps_its_own_connection(self):
        src = self.source()
        out = {}

        def fetch(n):
            out[n] = [src.chunk(i) for i in range(CHUNKS)]

        threads = [threading.Thread(target=fetch, args=(n,)) for n in range(3)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(10)
        self.assertEqual([b"".join(out[n]) for n in range(3)], [MEM] * 3)
        self.assertEqual(len(self.connects), 3)
        self.assertEqual(len(set(self.connects)), 3)

    def test_a_connection_the_agent_dropped_is_replaced_without_a_retry(self):
        src = self.source()
        src.chunk(0)
        # what the agent does to a connection idle past its request timeout.
        src.local.conn.sock.shutdown(socket.SHUT_RDWR)
        self.assertEqual(src.chunk(1), MEM[PAGE:2 * PAGE])
        self.assertEqual(len(self.connects), 2)

    def test_a_stalled_source_is_not_retried_on_a_new_connection(self):
        src = self.source()

        class Stalled:
            def request(self, *a, **k):
                raise TimeoutError("timed out")

            def close(self):
                pass

        src.local.conn = Stalled()
        with self.assertRaises(TimeoutError):
            src.chunk(0)
        self.assertEqual(self.connects, [])

    def test_a_refused_chunk_is_not_retried_on_a_new_connection(self):
        src = self.source()
        src.grant = "v1.nope"
        with self.assertRaises(fc_uffd.BadChunk):
            src.chunk(0)
        self.assertEqual(len(self.connects), 1)
        with self.assertRaises(fc_uffd.BadChunk):
            src.chunk(0)
        # the failed exchange dropped the connection, so this made a second.
        self.assertEqual(len(self.connects), 2)


if __name__ == "__main__":
    unittest.main()
