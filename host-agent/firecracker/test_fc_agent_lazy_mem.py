"""tests for lazy memory: the agent side of a lazy migrate, and fc-uffd.

a lazy resume leaves mem on the source. the source serves it one aligned chunk
at a time under the artifact grant, the target commits every file but mem, and
fc-uffd pages chunks in, each checked against the chunk table in live.json.

the userfaultfd ioctl needs linux and a real firecracker, so copy_page is
stubbed; everything around it (layout mapping, verification, the range
server, the lazy pull, resume_plan, the status route) runs here for real.
the chunk size is shrunk so the fixtures stay small. stdlib-only, no VMs.
"""
import hashlib
import http.client
import importlib.util
import json
import os
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
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


fc_agent = load("fc_agent", "fc-agent.py")
fc_uffd = load("fc_uffd", "fc-uffd.py")

# the commit step shells out through sudo; the test store is ours already.
fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)

PAGE = 4096
fc_agent.HUGE_PAGE_BYTES = PAGE


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def grant_for(digest: str) -> str:
    expiry, nonce = str(int(time.time()) + 60), "ab" * 16
    return f"v1.{digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(digest, expiry, nonce)}"


def memory_image() -> bytes:
    # chunk 1 is all zero, the rest are distinct.
    return b"a" * PAGE + bytes(PAGE) + b"c" * PAGE + b"d" * PAGE


def write_live_snapshot(snapshot_id: str, mem: bytes, chunks: bool = True) -> tuple[str, dict]:
    """write a live snapshot the way snapshot_create does for a 2M vm and
    return (rootfs digest, memory-half digests)."""
    d = fc_agent.SNAPSHOTS_DIR / snapshot_id
    d.mkdir(parents=True)
    (d / "rootfs.ext4").write_bytes(f"rootfs {snapshot_id}".encode())
    (d / "vmstate").write_bytes(b"vcpu state")
    (d / "mem").write_bytes(mem)
    manifest = {"index": 7, "rootfs_path": "/nowhere/rootfs.ext4", "host": fc_agent.host_fingerprint()}
    if chunks:
        table, zeros = fc_agent.chunk_digests(d / "mem")
        manifest.update(page_size=PAGE, chunks=table, zero_chunks=zeros)
    (d / "live.json").write_text(json.dumps(manifest))
    digest = fc_agent.file_digest(d / "rootfs.ext4")
    files = {n: fc_agent.file_digest(d / n) for n in fc_agent.LIVE_FILES}
    (d / "meta.json").write_text(json.dumps(
        {"snapshot_id": snapshot_id, "digest": digest, "kind": "live", "files": files}
    ))
    return digest, files


class AgentServer:
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.peer = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")

    def get(self, path: str, headers: dict) -> tuple[int, dict, bytes]:
        conn = fc_agent._peer_connection(self.peer)
        try:
            conn.request("GET", path, headers=headers)
            resp = conn.getresponse()
            return resp.status, dict(resp.getheaders()), resp.read()
        finally:
            conn.close()


class ChunkTableTest(unittest.TestCase):
    def test_digests_every_chunk_and_flags_the_zero_ones(self):
        path = Path(_import_root.name) / "chunk-table-mem"
        path.write_bytes(memory_image())
        table, zeros = fc_agent.chunk_digests(path)
        mem = memory_image()
        self.assertEqual(table, [sha(mem[i:i + PAGE]) for i in range(0, len(mem), PAGE)])
        self.assertEqual(zeros, [1])


class MemRangeTest(AgentServer, unittest.TestCase):
    def test_serves_one_aligned_chunk_of_mem(self):
        digest, _ = write_live_snapshot(f"src-{self.name}", memory_image())
        status, headers, body = self.get(
            f"/v1/artifacts/{digest}/files/mem",
            {fc_agent.ARTIFACT_GRANT_HEADER: grant_for(digest), "Range": f"bytes={2 * PAGE}-{3 * PAGE - 1}"},
        )
        self.assertEqual(status, 206)
        self.assertEqual(body, b"c" * PAGE)
        self.assertEqual(headers["Content-Range"], f"bytes {2 * PAGE}-{3 * PAGE - 1}/{4 * PAGE}")

    def test_refuses_every_other_range(self):
        digest, _ = write_live_snapshot(f"src-{self.name}", memory_image())
        grant = {fc_agent.ARTIFACT_GRANT_HEADER: grant_for(digest)}
        for name, rng in [
            ("mem", "bytes=1-4096"),                          # unaligned
            ("mem", f"bytes=0-{2 * PAGE - 1}"),               # two chunks
            ("mem", f"bytes={4 * PAGE}-{5 * PAGE - 1}"),      # past the end
            ("mem", "bytes=0-"),                              # open ended
            ("vmstate", f"bytes=0-{PAGE - 1}"),               # not mem
        ]:
            status, _, _ = self.get(f"/v1/artifacts/{digest}/files/{name}", {**grant, "Range": rng})
            self.assertEqual(status, 416, f"{name} {rng}")

    def test_a_range_still_needs_the_grant(self):
        digest, _ = write_live_snapshot(f"src-{self.name}", memory_image())
        status, _, _ = self.get(f"/v1/artifacts/{digest}/files/mem", {"Range": f"bytes=0-{PAGE - 1}"})
        self.assertEqual(status, 403)


class LazyPullTest(AgentServer, unittest.TestCase):
    def test_commits_everything_but_mem_and_remembers_the_peer(self):
        digest, files = write_live_snapshot(f"src-{self.name}", memory_image())
        grant = grant_for(digest)
        rec = fc_agent.pull_artifact(digest, self.peer, grant, f"dst-{self.name}", files, lazy=True)
        self.assertEqual(rec["kind"], "live")
        self.assertEqual(rec["lazy"], {"peer_url": self.peer, "digest": digest, "grant": grant,
                                       "source_snapshot_id": ""})
        dest = fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}"
        self.assertEqual(sorted(p.name for p in dest.iterdir()),
                         ["live.json", "meta.json", "rootfs.ext4", "vmstate"])

    def test_refuses_a_snapshot_with_no_chunk_table(self):
        digest, files = write_live_snapshot(f"src-{self.name}", memory_image(), chunks=False)
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}", files, lazy=True)
        self.assertEqual(caught.exception.code, 409)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}").exists())

    def test_lazy_needs_the_live_files(self):
        digest, _ = write_live_snapshot(f"src-{self.name}", memory_image())
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}", {}, lazy=True)
        self.assertEqual(caught.exception.code, 400)


class LazyResumePlanTest(unittest.TestCase):
    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")
        vms_root = os.path.realpath(str(fc_agent.VMS_DIR))
        self.rootfs_path = os.path.join(vms_root, f"gone-{self.name}", "rootfs.ext4")

    def seed(self, chunks: bool, lazy: bool, page_size: bool = False) -> str:
        d = fc_agent.SNAPSHOTS_DIR / self.name
        d.mkdir(parents=True)
        (d / "rootfs.ext4").write_bytes(b"rootfs")
        (d / "vmstate").write_bytes(b"v")
        manifest = {"index": 7, "rootfs_path": self.rootfs_path, "host": fc_agent.host_fingerprint()}
        if chunks:
            manifest.update(page_size=PAGE, chunks=[sha(b"x")], zero_chunks=[])
        elif page_size:
            manifest.update(page_size=PAGE)
        (d / "live.json").write_text(json.dumps(manifest))
        meta = {"snapshot_id": self.name, "digest": sha(b"rootfs"), "kind": "live"}
        if lazy:
            meta["lazy"] = {"peer_url": "http://peer", "digest": sha(b"rootfs"), "grant": "g"}
        else:
            (d / "mem").write_bytes(b"m")
        (d / "meta.json").write_text(json.dumps(meta))
        return self.name

    def test_a_lazy_seed_resumes_through_uffd_with_no_local_mem(self):
        plan = fc_agent.resume_plan(self.seed(chunks=True, lazy=True))
        self.assertTrue(plan["huge_pages"])
        self.assertIsNone(plan["mem"])
        self.assertEqual(plan["lazy"]["peer_url"], "http://peer")

    def test_a_local_2m_seed_resumes_through_uffd_from_its_own_mem(self):
        plan = fc_agent.resume_plan(self.seed(chunks=True, lazy=False))
        self.assertTrue(plan["huge_pages"])
        self.assertIsNone(plan["lazy"])
        self.assertEqual(plan["mem"].name, "mem")

    def test_a_2m_seed_merged_from_a_diff_still_resumes_through_uffd(self):
        # a merged copy carries the diff's manifest: page_size, no chunk table.
        plan = fc_agent.resume_plan(self.seed(chunks=False, lazy=False, page_size=True))
        self.assertTrue(plan["huge_pages"])
        self.assertEqual(plan["mem"].name, "mem")

    def test_a_4k_seed_resumes_from_a_file(self):
        self.assertFalse(fc_agent.resume_plan(self.seed(chunks=False, lazy=False))["huge_pages"])

    def test_refuses_a_lazy_seed_with_only_a_page_size(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.resume_plan(self.seed(chunks=False, lazy=True, page_size=True))
        self.assertEqual(caught.exception.code, 409)

    def test_refuses_a_lazy_seed_with_no_chunk_table(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.resume_plan(self.seed(chunks=False, lazy=True))
        self.assertEqual(caught.exception.code, 409)
        self.assertIn("2M pages", caught.exception.msg)


class MemoryStatusRouteTest(AgentServer, unittest.TestCase):
    def vm(self, **extra) -> str:
        vm_id = f"vm-{self.name}"
        d = fc_agent.VMS_DIR / vm_id
        d.mkdir(parents=True)
        (d / "meta.json").write_text(json.dumps({"vm_id": vm_id, **extra}))
        return vm_id

    def memory(self, vm_id: str) -> tuple[int, dict]:
        status, _, body = self.get(f"/v1/vm/{vm_id}/memory", {"Authorization": "Bearer test-token"})
        return status, json.loads(body) if status == 200 else {}

    def test_a_vm_with_no_handler_has_all_of_its_memory(self):
        self.assertEqual(self.memory(self.vm()), (200, {"done": True}))

    def test_reports_the_handlers_progress(self):
        status_file = Path(_import_root.name) / f"{self.name}.json"
        status_file.write_text(json.dumps({"resident_chunks": 3, "total_chunks": 4, "done": False}))
        code, body = self.memory(self.vm(uffd_status=str(status_file)))
        self.assertEqual(code, 200)
        self.assertEqual(body["resident_chunks"], 3)
        self.assertFalse(body["done"])

    def test_unknown_vm_is_404(self):
        self.assertEqual(self.memory("vm-nobody")[0], 404)


class FakeSource:
    def __init__(self, mem: bytes, corrupt: set[int] = frozenset()):
        self.mem, self.corrupt, self.calls = mem, corrupt, []

    def chunk(self, i: int) -> bytes:
        self.calls.append(i)
        data = self.mem[i * PAGE:(i + 1) * PAGE]
        return b"x" * PAGE if i in self.corrupt else data


class PagerTest(unittest.TestCase):
    def setUp(self):
        mem = memory_image()
        self.mem = mem
        self.manifest = {
            "page_size": PAGE,
            "chunks": [sha(mem[i:i + PAGE]) for i in range(0, len(mem), PAGE)],
            "zero_chunks": [1],
        }
        # two regions, the second mapped at a different host address than
        # its file offset would suggest, as firecracker lays out guest memory.
        self.regions = [
            {"base_host_virt_addr": 0x10000000, "size": 2 * PAGE, "offset": 0},
            {"base_host_virt_addr": 0x90000000, "size": 2 * PAGE, "offset": 2 * PAGE},
        ]
        self.status = Path(tempfile.mkdtemp()) / "uffd.json"
        self.copies = []
        patcher = mock.patch.object(fc_uffd, "copy_page",
                                    lambda uffd, dst, data, removed=None: self.copies.append((dst, bytes(data))))
        patcher.start()
        self.addCleanup(patcher.stop)

    def pager(self, source) -> "fc_uffd.Pager":
        return fc_uffd.Pager(-1, self.regions, self.manifest, source, str(self.status))

    def test_maps_a_fault_to_its_chunk_in_either_region(self):
        p = self.pager(FakeSource(self.mem))
        self.assertEqual(p.chunk_for(0x10000000 + 5), 0)
        self.assertEqual(p.chunk_for(0x10000000 + PAGE), 1)
        self.assertEqual(p.chunk_for(0x90000000 + PAGE + 7), 3)
        self.assertIsNone(p.chunk_for(0x50000000))

    def test_fill_copies_the_verified_chunk_to_its_guest_address(self):
        p = self.pager(FakeSource(self.mem))
        p.fill(3)
        self.assertEqual(self.copies, [(0x90000000 + PAGE, b"d" * PAGE)])
        p.fill(3)
        self.assertEqual(len(self.copies), 1, "a resident chunk was copied twice")

    def test_a_zero_chunk_never_touches_the_source(self):
        source = FakeSource(self.mem)
        self.pager(source).fill(1)
        self.assertEqual(source.calls, [])
        self.assertEqual(self.copies, [(0x10000000 + PAGE, bytes(PAGE))])

    def test_a_chunk_that_never_verifies_is_never_copied(self):
        p = self.pager(FakeSource(self.mem, corrupt={2}))
        with mock.patch.object(fc_uffd.time, "sleep"):
            with self.assertRaises(RuntimeError) as caught:
                p.fill(2)
        self.assertIn("digest mismatch", str(caught.exception))
        self.assertEqual(self.copies, [])

    def test_prefetch_fills_everything_but_holes_and_reports_done(self):
        p = self.pager(FakeSource(self.mem))
        p.prefetch()
        # chunk 1 is a hole: it needs no source, so it is done without being
        # populated, and stays empty on the host until the guest touches it.
        self.assertEqual(sorted(dst for dst, _ in self.copies),
                         [0x10000000, 0x90000000, 0x90000000 + PAGE])
        st = json.loads(self.status.read_text())
        self.assertEqual(st, {"resident_chunks": 4, "total_chunks": 4, "done": True})

    def test_a_removed_chunk_is_zero_filled_on_its_next_touch(self):
        source = FakeSource(self.mem)
        p = self.pager(source)
        p.fill(3)
        p.remove(0x90000000 + PAGE, 0x90000000 + 2 * PAGE)
        self.assertNotIn(3, p.resident)
        p.fill(3)
        self.assertEqual(self.copies[-1], (0x90000000 + PAGE, bytes(PAGE)))
        self.assertEqual(source.calls, [3], "a removed chunk was fetched again")

    def test_prefetch_skips_a_removed_chunk_and_still_finishes(self):
        p = self.pager(FakeSource(self.mem))
        p.remove(0x90000000, 0x90000000 + PAGE)
        p.prefetch()
        self.assertNotIn(0x90000000, [dst for dst, _ in self.copies])
        self.assertTrue(json.loads(self.status.read_text())["done"])

    def test_a_remove_spanning_regions_marks_every_chunk(self):
        p = self.pager(FakeSource(self.mem))
        p.remove(0x10000000 + PAGE, 0x10000000 + 2 * PAGE)
        p.remove(0x90000000 + 5, 0x90000000 + 2 * PAGE)
        self.assertEqual(p.removed, {1, 2, 3})

    def test_prefetch_reports_a_failure_and_stops(self):
        p = self.pager(FakeSource(self.mem, corrupt={2}))
        with mock.patch.object(fc_uffd.time, "sleep"):
            p.prefetch()
        st = json.loads(self.status.read_text())
        self.assertFalse(st["done"])
        self.assertIn("chunk 2", st["error"])


class FileWithoutChunkTableTest(unittest.TestCase):
    """a snapshot merged from a delta has no chunk table: fc-uffd serves its
    local, already verified memory file without one."""

    def setUp(self):
        self.copies = []
        patcher = mock.patch.object(fc_uffd, "copy_page",
                                    lambda uffd, dst, data, removed=None: self.copies.append((dst, bytes(data))))
        patcher.start()
        self.addCleanup(patcher.stop)
        self.path = Path(tempfile.mkdtemp()) / "mem"
        self.regions = [{"base_host_virt_addr": 0x10000000, "size": 4 * PAGE, "offset": 0}]

    def pager(self) -> "fc_uffd.Pager":
        source = fc_uffd.FileSource(str(self.path), PAGE)
        status = self.path.with_name("uffd.json")
        return fc_uffd.Pager(-1, self.regions, {"page_size": PAGE}, source, str(status))

    def test_serves_every_chunk_of_the_file_unverified(self):
        self.path.write_bytes(memory_image())
        p = self.pager()
        p.prefetch()
        self.assertEqual(p.total, 4)
        copied = dict(self.copies)
        self.assertEqual(copied[0x10000000 + 3 * PAGE], b"d" * PAGE)
        self.assertTrue(json.loads(self.path.with_name("uffd.json").read_text())["done"])

    def test_holes_in_the_file_are_its_zero_chunks(self):
        with open(self.path, "wb") as f:
            f.truncate(4 * PAGE)
            for i in (0, 2, 3):
                f.seek(i * PAGE)
                f.write(b"z" * PAGE)
        with open(self.path, "rb") as f:
            if os.lseek(f.fileno(), 0, os.SEEK_HOLE) >= 4 * PAGE:
                self.skipTest("this filesystem does not report holes")
        p = self.pager()
        self.assertEqual(p.zero, {1})
        p.prefetch()
        self.assertNotIn(0x10000000 + PAGE, dict(self.copies))


class PeerSourceTest(AgentServer, unittest.TestCase):
    """fc-uffd's fetch against the real range server, end to end."""

    def test_fetches_each_chunk_from_the_source_agent(self):
        mem = memory_image()
        digest, _ = write_live_snapshot(f"src-{self.name}", mem)
        source = fc_uffd.PeerSource(f"{self.peer}/v1/artifacts/{digest}/files/mem", grant_for(digest), PAGE)
        for i in range(4):
            self.assertEqual(source.chunk(i), mem[i * PAGE:(i + 1) * PAGE])

    def test_a_bad_grant_is_an_error_not_a_page(self):
        digest, _ = write_live_snapshot(f"src-{self.name}", memory_image())
        source = fc_uffd.PeerSource(f"{self.peer}/v1/artifacts/{digest}/files/mem", "v1.nope", PAGE)
        with self.assertRaises((OSError, http.client.HTTPException)):
            source.chunk(0)


if __name__ == "__main__":
    unittest.main()
