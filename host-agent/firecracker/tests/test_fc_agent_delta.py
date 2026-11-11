"""tests for diff live snapshots and delta pulls.

a diff snapshot holds only what changed since its parent: the dirty memory
pages and the rootfs blocks whose hashes moved, each as (offset, length)
records. a delta pull merges those onto a complete copy of the parent the
pulling host already holds, and the result has to be byte for byte what the
source had. firecracker writes the sparse diff image itself, so the snapshots
here are built by hand the same way snapshot_create lays them out; one process
plays both hosts. stdlib-only, no VMs.
"""
import hashlib
import importlib.util
import json
import os
import tempfile
import threading
import time
import unittest
from http.server import ThreadingHTTPServer
from pathlib import Path


_import_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location("fc_agent", Path(__file__).parent.parent / "fc-agent.py")
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

# the commit step shells out through sudo; the test store is ours already.
fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)

B = 4096
fc_agent.ROOTFS_BLOCK_BYTES = B
# mem chunks of 8 pages, so a 64 page image has a table worth merging.
fc_agent.HUGE_PAGE_BYTES = 8 * B


def grant_for(digest: str) -> str:
    expiry, nonce = str(int(time.time()) + 60), "ab" * 16
    return f"v1.{digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(digest, expiry, nonce)}"


def write_snapshot(sid: str, rootfs: bytes, mem: bytes, parent: str = "",
                   dirty: list[tuple[int, bytes]] | None = None, version: int = 1,
                   block_table_rootfs: bool = False) -> tuple[str, dict]:
    """lay a snapshot out the way snapshot_create does: a complete one, or a
    diff against parent holding only dirty pages and changed blocks. version
    2 gives a complete one a mem chunk table; block_table_rootfs gives the
    rootfs the digest a migrate's final pause records."""
    d = fc_agent.SNAPSHOTS_DIR / sid
    d.mkdir(parents=True)
    (d / "rootfs.ext4").write_bytes(rootfs)
    (d / "vmstate").write_bytes(b"vcpu state of " + sid.encode())
    digest, blocks = fc_agent.rootfs_digests(d / "rootfs.ext4")
    if block_table_rootfs:
        digest = fc_agent.block_table_digest(blocks)
    manifest = {"snapshot_id": sid, "index": 7, "rootfs_size": len(rootfs), "rootfs_blocks": blocks}
    if parent:
        parent_manifest = fc_agent.SNAPSHOTS_DIR / parent / "live.json"
        old = json.loads(parent_manifest.read_text())["rootfs_blocks"]
        manifest["parent_manifest"] = fc_agent.manifest_identity(parent_manifest)
        changed = [(i * B, min(B, len(rootfs) - i * B))
                   for i, h in enumerate(blocks) if i >= len(old) or old[i] != h]
        fc_agent.write_delta(d / "rootfs.ext4", changed, d / "rootfs.delta")
        # what firecracker leaves: a full-size image with only dirty pages in
        # it. the extents are given directly, since the test filesystem may
        # not report holes (see snapshot_fs_reports_holes).
        with open(d / "mem", "wb") as f:
            f.truncate(len(mem))
            for off, data in dirty or []:
                f.seek(off)
                f.write(data)
        fc_agent.write_delta(d / "mem", [(off, len(data)) for off, data in dirty or []], d / "mem.delta")
        (d / "mem").unlink()
        names = fc_agent.DELTA_FILES
    else:
        (d / "mem").write_bytes(mem)
        names = fc_agent.LIVE_FILES
        if version == 2:
            manifest.update(digest_version=2, mem_chunks=fc_agent.chunk_digests(d / "mem")[0])
    (d / "live.json").write_text(json.dumps(manifest))
    files = {n: fc_agent.file_digest(d / n) for n in names}
    if "mem_chunks" in manifest:
        files["mem"] = fc_agent.mem_table_digest(manifest["mem_chunks"])
    record = {"snapshot_id": sid, "digest": digest, "kind": "live", "files": files}
    if parent:
        record["parent"] = parent
    (d / "meta.json").write_text(json.dumps(record))
    return digest, files


class DeltaFormatTest(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())

    def test_write_then_apply_reproduces_the_ranges(self):
        src = self.dir / "src"
        src.write_bytes(bytes(range(256)) * 64)
        target = self.dir / "target"
        target.write_bytes(b"\0" * src.stat().st_size)
        fc_agent.write_delta(src, [(0, 10), (5000, 3000)], self.dir / "delta")
        fc_agent.apply_delta(self.dir / "delta", target, target.stat().st_size)
        want = bytearray(target.stat().st_size)
        data = src.read_bytes()
        want[0:10] = data[0:10]
        want[5000:8000] = data[5000:8000]
        self.assertEqual(target.read_bytes(), bytes(want))

    def test_a_record_past_the_image_is_refused(self):
        src = self.dir / "src"
        src.write_bytes(b"x" * 8192)
        fc_agent.write_delta(src, [(4096, 4096)], self.dir / "delta")
        target = self.dir / "target"
        target.write_bytes(b"\0" * 4096)
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.apply_delta(self.dir / "delta", target, 4096)
        self.assertEqual(caught.exception.code, 422)

    def test_a_truncated_delta_is_refused(self):
        src = self.dir / "src"
        src.write_bytes(b"x" * 4096)
        fc_agent.write_delta(src, [(0, 4096)], self.dir / "delta")
        raw = (self.dir / "delta").read_bytes()
        (self.dir / "delta").write_bytes(raw[:-1])
        target = self.dir / "target"
        target.write_bytes(b"\0" * 4096)
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.apply_delta(self.dir / "delta", target, 4096)

    def test_rootfs_digests_matches_a_whole_file_hash(self):
        path = self.dir / "rootfs"
        path.write_bytes(b"a" * B + b"b" * 100)
        digest, blocks = fc_agent.rootfs_digests(path)
        self.assertEqual(digest, fc_agent.file_digest(path))
        self.assertEqual(blocks, [hashlib.sha256(b"a" * B).hexdigest(), hashlib.sha256(b"b" * 100).hexdigest()])

    def test_the_reflink_probe_answers(self):
        self.assertIsInstance(fc_agent.snapshot_store_reflinks(), bool)
        self.assertEqual(list(fc_agent.SNAPSHOTS_DIR.glob(".reflink-probe-*")), [])

    def test_the_holes_probe_answers(self):
        # whatever this filesystem supports, the probe must answer rather than
        # raise, and must not leave its file behind.
        self.assertIsInstance(fc_agent.snapshot_fs_reports_holes(), bool)
        self.assertEqual(list(fc_agent.SNAPSHOTS_DIR.glob(".holes-probe-*")), [])


SHARED = fc_agent.FIEMAP_EXTENT_SHARED


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


class ExtentDiffTest(unittest.TestCase):
    """the final pause hashes only blocks whose extents moved since parent.
    extent maps are injected, since the test filesystem may not reflink."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.real_map = fc_agent.file_extent_map
        self.maps: dict[str, list] = {}
        fc_agent.file_extent_map = lambda p: self.maps[str(p)]

    def tearDown(self):
        fc_agent.file_extent_map = self.real_map

    def files(self, parent: bytes, child: bytes) -> tuple[Path, Path]:
        p, c = self.dir / "parent", self.dir / "child"
        p.write_bytes(parent)
        c.write_bytes(child)
        return p, c

    def test_a_shared_block_is_skipped_and_a_rewritten_one_is_sent(self):
        parent = b"a" * B + b"b" * B + b"c" * B
        # block 0 differs on disk here but its extent says it is shared, so
        # keeping parent's hash proves it was never read.
        child = b"x" * B + b"B" * B + b"c" * B
        p, c = self.files(parent, child)
        old = [sha(b"a" * B), sha(b"b" * B), sha(b"c" * B)]
        self.maps[str(p)] = [(0, 1 << 20, B, SHARED), (B, (1 << 20) + B, 2 * B, 0)]
        # blocks 1 and 2 were copied on write to new extents; 2 got the same bytes back.
        self.maps[str(c)] = [(0, 1 << 20, B, SHARED), (B, 9 << 20, 2 * B, fc_agent.FIEMAP_EXTENT_LAST)]
        blocks = fc_agent.rootfs_blocks_by_extents(c, p, old)
        self.assertEqual(blocks, [old[0], sha(b"B" * B), old[2]])
        self.assertEqual(fc_agent.changed_block_ranges(blocks, old, len(child)), [(B, B)])

    def test_runs_split_differently_still_compare_equal(self):
        p, c = self.files(b"a" * 2 * B, b"a" * 2 * B)
        old = [sha(b"a" * B)] * 2
        self.maps[str(p)] = [(0, 1 << 20, 2 * B, SHARED)]
        self.maps[str(c)] = [(0, 1 << 20, B // 2, SHARED), (B // 2, (1 << 20) + B // 2, B + B // 2, SHARED)]
        self.assertEqual(fc_agent.block_extent_maps(self.maps[str(p)], 2 * B),
                         fc_agent.block_extent_maps(self.maps[str(c)], 2 * B))
        self.assertEqual(fc_agent.rootfs_blocks_by_extents(c, p, old), old)

    def test_an_unsure_extent_is_always_hashed(self):
        p, c = self.files(b"a" * B, b"z" * B)
        flags = 0x4 | SHARED  # delayed allocation: the physical offset means nothing yet
        self.maps[str(p)] = [(0, 1 << 20, B, flags)]
        self.maps[str(c)] = [(0, 1 << 20, B, flags)]
        self.assertEqual(fc_agent.rootfs_blocks_by_extents(c, p, [sha(b"a" * B)]), [sha(b"z" * B)])

    def test_a_clone_not_flagged_shared_is_hashed(self):
        # a filesystem that clones without saying so is not trusted with the skip.
        p, c = self.files(b"a" * B, b"z" * B)
        self.maps[str(p)] = [(0, 1 << 20, B, 0)]
        self.maps[str(c)] = [(0, 1 << 20, B, 0)]
        self.assertEqual(fc_agent.rootfs_blocks_by_extents(c, p, [sha(b"a" * B)]), [sha(b"z" * B)])

    def test_holes_in_both_keep_parents_hash(self):
        p, c = self.files(bytes(B) + b"a" * B, bytes(B) + b"a" * B)
        old = [sha(bytes(B)), sha(b"a" * B)]
        self.maps[str(p)] = [(B, 1 << 20, B, SHARED)]
        self.maps[str(c)] = [(B, 1 << 20, B, SHARED)]
        self.assertEqual(fc_agent.rootfs_blocks_by_extents(c, p, old), old)

    def test_falls_back_without_fiemap_or_on_a_resized_disk(self):
        p, c = self.files(b"a" * B, b"a" * B)

        def no_fiemap(path):
            raise OSError(95, "Operation not supported")
        fc_agent.file_extent_map = no_fiemap
        self.assertIsNone(fc_agent.rootfs_blocks_by_extents(c, p, [sha(b"a" * B)]))
        c.write_bytes(b"a" * 2 * B)
        self.assertIsNone(fc_agent.rootfs_blocks_by_extents(c, p, [sha(b"a" * B)]))

    def test_the_full_pass_records_the_file_digest_once_unpaused(self):
        snap = self.dir / "snap"
        snap.mkdir()
        (snap / "rootfs.ext4").write_bytes(b"a" * B + b"b" * B)
        (snap / "meta.json").write_text(json.dumps({"snapshot_id": "snap", "digest": "0" * 64}))
        blocks = [sha(b"a" * B), sha(b"b" * B)]
        # no such vm: destroyed after the target resumed.
        fc_agent.finish_rootfs_digest(f"gone-{os.urandom(4).hex()}", snap, blocks)
        record = json.loads((snap / "meta.json").read_text())
        self.assertEqual(record["rootfs_digest"], fc_agent.file_digest(snap / "rootfs.ext4"))
        self.assertEqual(record["digest"], "0" * 64, "the wire identity must not change after the move")

    def test_a_restart_picks_up_a_pending_full_pass(self):
        snap = fc_agent.SNAPSHOTS_DIR / f"snap-pending-{os.urandom(4).hex()}"
        snap.mkdir(parents=True)
        (snap / "rootfs.ext4").write_bytes(b"a" * B)
        (snap / "live.json").write_text(json.dumps({"rootfs_blocks": [sha(b"a" * B)]}))
        (snap / "meta.json").write_text(json.dumps({"snapshot_id": snap.name, "digest": "0" * 64,
                                                    "origin_vm_id": "gone", "rootfs_digest_pending": True}))
        fc_agent.resume_pending_rootfs_digests()
        for _ in range(100):
            record = json.loads((snap / "meta.json").read_text())
            if "rootfs_digest" in record:
                break
            time.sleep(0.02)
        self.assertEqual(record.get("rootfs_digest"), fc_agent.file_digest(snap / "rootfs.ext4"))
        self.assertNotIn("rootfs_digest_pending", record)


class RealReflinkTest(unittest.TestCase):
    """the same thing against the real FIEMAP, where the test dir reflinks."""

    def test_a_cow_rewrite_moves_only_that_blocks_extent(self):
        d = Path(tempfile.mkdtemp())
        p, c = d / "parent", d / "child"
        p.write_bytes(b"a" * B + b"b" * B + b"c" * B)
        if fc_agent.run(["cp", "--reflink=always", str(p), str(c)], check=False).returncode:
            self.skipTest("test dir does not reflink (not xfs or btrfs)")
        with open(c, "r+b") as f:
            f.seek(B)
            f.write(b"B" * B)
        old = [sha(b"a" * B), sha(b"b" * B), sha(b"c" * B)]
        blocks = fc_agent.rootfs_blocks_by_extents(c, p, old)
        self.assertEqual(blocks, [old[0], sha(b"B" * B), old[2]])
        mine = fc_agent.block_extent_maps(fc_agent.file_extent_map(c), 3 * B)
        theirs = fc_agent.block_extent_maps(fc_agent.file_extent_map(p), 3 * B)
        skippable = [a is not None and a == b for a, b in zip(mine, theirs)]
        self.assertEqual(skippable, [True, False, True])


class DeltaPullTest(unittest.TestCase):
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
        self.rootfs0 = b"A" * B + b"B" * B + b"C" * B
        self.mem0 = b"m" * (64 * B)
        self.parent = f"parent-{self.name}"
        self.base = f"base-{self.name}"
        d0, f0 = write_snapshot(self.parent, self.rootfs0, self.mem0)
        fc_agent.pull_artifact(d0, self.peer, grant_for(d0), self.base, f0, source_snapshot_id=self.parent)

    def child(self, sid: str):
        rootfs1 = b"A" * B + b"X" * B + b"C" * B + b"D" * 100   # block 1 changed, file grew
        dirty = [(5 * B, b"z" * B), (40 * B, b"y" * (2 * B))]
        mem1 = bytearray(self.mem0)
        for off, data in dirty:
            mem1[off:off + len(data)] = data
        digest, files = write_snapshot(sid, rootfs1, self.mem0, parent=self.parent, dirty=dirty)
        return digest, files, rootfs1, bytes(mem1)

    def test_merges_onto_the_base_byte_for_byte(self):
        digest, files, rootfs1, mem1 = self.child(f"child-{self.name}")
        rec = fc_agent.pull_artifact_delta(digest, self.peer, grant_for(digest), f"merged-{self.name}",
                                           files, self.base, f"child-{self.name}")
        merged = fc_agent.SNAPSHOTS_DIR / f"merged-{self.name}"
        self.assertEqual((merged / "rootfs.ext4").read_bytes(), rootfs1)
        self.assertEqual((merged / "mem").read_bytes(), mem1)
        self.assertEqual(rec["kind"], "live")
        self.assertNotIn("mem", rec["files"], "a merge is not re-hashed, so it must not claim a mem digest")
        # only the changed rootfs block and the dirty pages crossed the wire.
        child_dir = fc_agent.SNAPSHOTS_DIR / f"child-{self.name}"
        self.assertLess((child_dir / "mem.delta").stat().st_size, len(self.mem0) // 10)
        self.assertLess((child_dir / "rootfs.delta").stat().st_size, len(rootfs1) // 2)
        # the base is untouched unless asked.
        self.assertTrue((fc_agent.SNAPSHOTS_DIR / self.base / "mem").exists())

    def test_drop_base_removes_the_base_after_the_merge(self):
        digest, files, _, _ = self.child(f"child-{self.name}")
        fc_agent.pull_artifact_delta(digest, self.peer, grant_for(digest), f"merged-{self.name}",
                                     files, self.base, f"child-{self.name}", drop_base=True)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / self.base).exists())
        self.assertTrue((fc_agent.SNAPSHOTS_DIR / f"merged-{self.name}" / "mem").exists())

    def test_refuses_a_delta_taken_against_another_parent(self):
        other = f"other-{self.name}"
        d2, f2 = write_snapshot(other, b"Q" * B, self.mem0)
        fc_agent.pull_artifact(d2, self.peer, grant_for(d2), f"{other}-base", f2, source_snapshot_id=other)
        digest, files, _, _ = self.child(f"child-{self.name}")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.pull_artifact_delta(digest, self.peer, grant_for(digest), f"merged-{self.name}",
                                         files, f"{other}-base", f"child-{self.name}", drop_base=True)
        self.assertEqual(caught.exception.code, 409)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / f"merged-{self.name}").exists())
        self.assertTrue((fc_agent.SNAPSHOTS_DIR / f"{other}-base").exists(), "a refused merge dropped its base")

    def test_a_bad_delta_file_commits_nothing(self):
        digest, files, _, _ = self.child(f"child-{self.name}")
        files["mem.delta"] = hashlib.sha256(b"not what was recorded").hexdigest()
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.pull_artifact_delta(digest, self.peer, grant_for(digest), f"merged-{self.name}",
                                         files, self.base, f"child-{self.name}", drop_base=True)
        self.assertEqual(caught.exception.code, 422)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / f"merged-{self.name}").exists())
        self.assertTrue((fc_agent.SNAPSHOTS_DIR / self.base).exists())

    def test_refuses_a_base_that_is_itself_a_diff(self):
        digest, files, _, _ = self.child(f"child-{self.name}")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.pull_artifact_delta(digest, self.peer, grant_for(digest), f"merged-{self.name}",
                                         files, f"child-{self.name}", f"child-{self.name}")
        self.assertEqual(caught.exception.code, 409)

    def test_a_snapshot_pin_keeps_two_same_digest_snapshots_apart(self):
        # an idle guest's two checkpoints share a rootfs digest; the pin is
        # what keeps one's vmstate from being served next to the other's mem.
        twin = f"twin-{self.name}"
        d_twin, _ = write_snapshot(twin, self.rootfs0, b"n" * (64 * B))
        d_parent = json.loads((fc_agent.SNAPSHOTS_DIR / self.parent / "meta.json").read_text())["digest"]
        self.assertEqual(d_twin, d_parent)
        path, _ = fc_agent.artifact_file(d_twin, "vmstate", twin)
        self.assertEqual(path.parent.name, twin)
        path, _ = fc_agent.artifact_file(d_twin, "vmstate", self.parent)
        self.assertEqual(path.parent.name, self.parent)


class ThreeHostTest(unittest.TestCase):
    """a vm checkpointed from host a to host b, then moved on to host c, with
    nothing but diffs crossing to b after the first full copy. one store plays
    all three hosts, each copy under its own id."""

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
        self.rootfs = b"A" * B + b"B" * B + b"C" * B
        self.mem = bytes(range(256)) * (64 * B // 256)

    def sid(self, label: str) -> str:
        return f"{label}-{self.name}"

    def diff_on_a(self, label: str, parent: str, block: bytes, dirty: list[tuple[int, bytes]],
                  block_table_rootfs: bool = False) -> tuple[str, dict]:
        """the next diff on host a: block 1 of the rootfs and the dirty pages change."""
        self.rootfs = self.rootfs[:B] + block + self.rootfs[2 * B:]
        mem = bytearray(self.mem)
        for off, data in dirty:
            mem[off:off + len(data)] = data
        self.mem = bytes(mem)
        return write_snapshot(self.sid(label), self.rootfs, self.mem, parent=self.sid(parent),
                              dirty=dirty, block_table_rootfs=block_table_rootfs)

    def merge_on(self, label: str, child: str, digest: str, files: dict, base: str) -> dict:
        return fc_agent.pull_artifact_delta(digest, self.peer, grant_for(digest), self.sid(label),
                                            files, self.sid(base), self.sid(child), drop_base=True)

    def assertSameVM(self, label: str):
        d = fc_agent.SNAPSHOTS_DIR / self.sid(label)
        self.assertEqual((d / "rootfs.ext4").read_bytes(), self.rootfs)
        self.assertEqual((d / "mem").read_bytes(), self.mem)

    def run_chain(self, block_table_rootfs: bool) -> None:
        # a -> b: one full copy, then only diffs.
        d0, f0 = write_snapshot(self.sid("a0"), self.rootfs, self.mem, version=2)
        fc_agent.pull_artifact(d0, self.peer, grant_for(d0), self.sid("b0"), f0,
                               source_snapshot_id=self.sid("a0"))
        # a dirty page inside a chunk, and a run across a chunk boundary.
        d1, f1 = self.diff_on_a("a1", "a0", b"X" * B, [(3 * B, b"z" * B), (15 * B, b"y" * (2 * B))],
                                block_table_rootfs)
        b1 = self.merge_on("b1", "a1", d1, f1, "b0")
        d2, f2 = self.diff_on_a("a2", "a1", b"Y" * B, [(40 * B, b"w" * B)], block_table_rootfs)
        b2 = self.merge_on("b2", "a2", d2, f2, "b1")
        self.assertSameVM("b2")

        # the merged copy's digests are its own, worked out from the base's
        # table and the touched chunks only, and agree with a full read.
        mem_b2 = fc_agent.SNAPSHOTS_DIR / self.sid("b2") / "mem"
        self.assertEqual(set(b1["files"]), set(fc_agent.LIVE_FILES))
        self.assertEqual(b2["files"]["mem"], fc_agent.mem_table_digest(fc_agent.chunk_digests(mem_b2)[0]))
        self.assertEqual(b2["files"]["live.json"],
                         fc_agent.file_digest(fc_agent.SNAPSHOTS_DIR / self.sid("b2") / "live.json"))

        # b -> c: a full move of the merged copy, the record's own digests
        # standing in for the orchestrator's.
        rec = fc_agent.pull_artifact(b2["digest"], self.peer, grant_for(b2["digest"]), self.sid("c2"),
                                     b2["files"], source_snapshot_id=self.sid("b2"))
        self.assertEqual(rec["kind"], "live")
        self.assertSameVM("c2")

        # the copy on c is still the parent the vm's next diff on a names.
        d3, f3 = self.diff_on_a("a3", "a2", b"Z" * B, [(60 * B, b"v" * B)], block_table_rootfs)
        self.merge_on("c3", "a3", d3, f3, "c2")
        self.assertSameVM("c3")

    def test_a_merged_copy_moves_on_to_a_third_host(self):
        self.run_chain(block_table_rootfs=False)

    def test_a_final_pause_rootfs_digest_moves_on_too(self):
        self.run_chain(block_table_rootfs=True)

    def test_a_diff_taken_after_resuming_from_a_merged_copy_merges_onto_its_moved_copy(self):
        d0, f0 = write_snapshot(self.sid("a0"), self.rootfs, self.mem, version=2)
        fc_agent.pull_artifact(d0, self.peer, grant_for(d0), self.sid("b0"), f0,
                               source_snapshot_id=self.sid("a0"))
        d1, f1 = self.diff_on_a("a1", "a0", b"X" * B, [(3 * B, b"z" * B)])
        b1 = self.merge_on("b1", "a1", d1, f1, "b0")
        fc_agent.pull_artifact(b1["digest"], self.peer, grant_for(b1["digest"]), self.sid("c1"),
                               b1["files"], source_snapshot_id=self.sid("b1"))
        # the vm resumed on b from b1 names b1 as its diff's parent, and the
        # copy on c answers to the same identity even though b rewrote live.json.
        b_manifest = fc_agent.SNAPSHOTS_DIR / self.sid("b1") / "live.json"
        c_manifest = fc_agent.SNAPSHOTS_DIR / self.sid("c1") / "live.json"
        self.assertNotEqual(fc_agent.file_digest(b_manifest), f1["live.json"])
        self.assertEqual(fc_agent.manifest_identity(b_manifest), f1["live.json"])
        self.assertEqual(fc_agent.manifest_identity(c_manifest), f1["live.json"])

    def test_a_version_1_base_still_merges_but_records_no_mem_digest(self):
        d0, f0 = write_snapshot(self.sid("a0"), self.rootfs, self.mem)
        fc_agent.pull_artifact(d0, self.peer, grant_for(d0), self.sid("b0"), f0,
                               source_snapshot_id=self.sid("a0"))
        d1, f1 = self.diff_on_a("a1", "a0", b"X" * B, [(3 * B, b"z" * B)])
        b1 = self.merge_on("b1", "a1", d1, f1, "b0")
        self.assertSameVM("b1")
        self.assertNotIn("mem", b1["files"])
        self.assertEqual(b1["files"]["live.json"], f1["live.json"], "a version 1 merge must not rewrite live.json")


class DiffRefusalTest(unittest.TestCase):
    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")
        write_snapshot(f"p-{self.name}", b"A" * B, b"m" * B)
        write_snapshot(f"c-{self.name}", b"A" * B, b"m" * B, parent=f"p-{self.name}", dirty=[(0, b"z" * B)])

    def test_a_diff_cannot_be_resumed_on_its_own(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.resume_plan(f"c-{self.name}")
        self.assertEqual(caught.exception.code, 409)
        self.assertIn("diff", caught.exception.msg)

    def test_a_diff_cannot_be_restored_on_its_own(self):
        vm_id = f"vm-{self.name}"
        d = fc_agent.VMS_DIR / vm_id
        d.mkdir(parents=True)
        (d / "meta.json").write_text(json.dumps({"vm_id": vm_id}))
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.snapshot_restore(vm_id, f"c-{self.name}")
        self.assertEqual(caught.exception.code, 409)

    def test_a_diff_needs_the_vms_own_last_live_snapshot_as_parent(self):
        vm_id = f"vm2-{self.name}"
        d = fc_agent.VMS_DIR / vm_id
        d.mkdir(parents=True)
        (d / "meta.json").write_text(json.dumps({"vm_id": vm_id, "sock": "/nonexistent.sock",
                                                 "chain_head": "somebody-else"}))
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.snapshot_create(vm_id, "", live=True, diff=True, parent=f"p-{self.name}")
        self.assertEqual(caught.exception.code, 409)
        self.assertEqual(list(fc_agent.SNAPSHOTS_DIR.glob("snap-*")), [], "a refused diff left a dir behind")


class FullLiveSnapshotTest(unittest.TestCase):
    """snapshot_create against a stand-in firecracker that writes the files."""

    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")
        self.vm_id = f"vm-{self.name}"
        d = fc_agent.VMS_DIR / self.vm_id
        d.mkdir(parents=True)
        (d / "rootfs.ext4").write_bytes(b"r" * (2 * B))
        (d / "meta.json").write_text(json.dumps({"vm_id": self.vm_id, "sock": str(d / "fc.sock"),
                                                 "rootfs": str(d / "rootfs.ext4"), "index": 3}))
        self.mem = bytes(range(256)) * ((3 << 20) // 256)
        self.saved = fc_agent.fc_api, fc_agent.fc_vm_state

        def fc_api(sock, method, path, body=None, timeout=5.0):
            Path(body["snapshot_path"]).write_bytes(b"vcpu state")
            Path(body["mem_file_path"]).write_bytes(self.mem)
            return 204, b""
        fc_agent.fc_api = fc_api
        fc_agent.fc_vm_state = lambda sock, state: None

    def tearDown(self):
        fc_agent.fc_api, fc_agent.fc_vm_state = self.saved

    def test_records_a_version_2_mem_digest(self):
        rec = fc_agent.snapshot_create(self.vm_id, "", live=True)
        snap = fc_agent.SNAPSHOTS_DIR / rec["snapshot_id"]
        manifest = json.loads((snap / "live.json").read_text())
        self.assertEqual(manifest["digest_version"], fc_agent.LIVE_DIGEST_VERSION)
        self.assertEqual(manifest["mem_chunks"], fc_agent.chunk_digests(snap / "mem")[0])
        self.assertNotIn("chunks", manifest, "the fc-uffd table is only for a 2M vm")
        self.assertEqual(rec["files"]["mem"], fc_agent.mem_table_digest(manifest["mem_chunks"]))
        self.assertEqual(rec["files"]["live.json"], fc_agent.file_digest(snap / "live.json"))


class SnapshotDeleteTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.peer = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def delete(self, path: str) -> int:
        conn = fc_agent._peer_connection(self.peer)
        try:
            conn.request("DELETE", path, headers={"Authorization": "Bearer test-token"})
            resp = conn.getresponse()
            resp.read()
            return resp.status
        finally:
            conn.close()

    def test_deletes_a_free_standing_copy(self):
        write_snapshot("free-copy", b"A" * B, b"m" * B)
        self.assertEqual(self.delete("/v1/snapshots/free-copy"), 204)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / "free-copy").exists())
        self.assertEqual(self.delete("/v1/snapshots/free-copy"), 404)

    def test_deletes_a_vms_snapshot_and_its_list_entry(self):
        write_snapshot("owned", b"A" * B, b"m" * B)
        d = fc_agent.VMS_DIR / "owner"
        d.mkdir(parents=True)
        (d / "meta.json").write_text(json.dumps({"vm_id": "owner", "snapshots": [{"snapshot_id": "owned"}]}))
        self.assertEqual(self.delete("/v1/vm/owner/snapshots/owned"), 204)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / "owned").exists())
        self.assertEqual(fc_agent.load_meta("owner")["snapshots"], [])

    def test_refuses_a_traversal_shaped_id(self):
        self.assertIn(self.delete("/v1/snapshots/..%2F..%2Fetc"), (400, 404))
        self.assertTrue(fc_agent.SNAPSHOTS_DIR.exists())


if __name__ == "__main__":
    unittest.main()
