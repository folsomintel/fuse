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
_spec = importlib.util.spec_from_file_location("fc_agent", Path(__file__).with_name("fc-agent.py"))
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

# the commit step shells out through sudo; the test store is ours already.
fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)

B = 4096
fc_agent.ROOTFS_BLOCK_BYTES = B


def grant_for(digest: str) -> str:
    expiry, nonce = str(int(time.time()) + 60), "ab" * 16
    return f"v1.{digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(digest, expiry, nonce)}"


def write_snapshot(sid: str, rootfs: bytes, mem: bytes, parent: str = "",
                   dirty: list[tuple[int, bytes]] | None = None) -> tuple[str, dict]:
    """lay a snapshot out the way snapshot_create does: a complete one, or a
    diff against parent holding only dirty pages and changed blocks."""
    d = fc_agent.SNAPSHOTS_DIR / sid
    d.mkdir(parents=True)
    (d / "rootfs.ext4").write_bytes(rootfs)
    (d / "vmstate").write_bytes(b"vcpu state of " + sid.encode())
    digest, blocks = fc_agent.rootfs_digests(d / "rootfs.ext4")
    manifest = {"snapshot_id": sid, "index": 7, "rootfs_size": len(rootfs), "rootfs_blocks": blocks}
    if parent:
        parent_manifest = fc_agent.SNAPSHOTS_DIR / parent / "live.json"
        old = json.loads(parent_manifest.read_text())["rootfs_blocks"]
        manifest["parent_manifest"] = fc_agent.file_digest(parent_manifest)
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
    (d / "live.json").write_text(json.dumps(manifest))
    files = {n: fc_agent.file_digest(d / n) for n in names}
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

    def test_the_holes_probe_answers(self):
        # whatever this filesystem supports, the probe must answer rather than
        # raise, and must not leave its file behind.
        self.assertIsInstance(fc_agent.snapshot_fs_reports_holes(), bool)
        self.assertEqual(list(fc_agent.SNAPSHOTS_DIR.glob(".holes-probe-*")), [])


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
