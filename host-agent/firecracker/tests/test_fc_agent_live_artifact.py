"""tests for moving a live snapshot between hosts, and for resume_plan.

a live snapshot is three files beyond its rootfs (vmstate, mem, live.json), and
its rootfs is not standalone: it was copied from a paused guest without a sync.
so the pull has to be all or nothing, every file has to verify against a digest
the orchestrator supplied rather than one the peer claims, and the target has to
refuse a resume it cannot do correctly instead of trying it.

one process plays both hosts: the handler serves out of the same store the pull
commits into, under a different snapshot id. stdlib-only, no VMs.
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
_spec = importlib.util.spec_from_file_location(
    "fc_agent", Path(__file__).parent.parent / "fc-agent.py"
)
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

# the commit step shells out through sudo; the test store is ours already.
fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def grant_for(digest: str) -> str:
    expiry, nonce = str(int(time.time()) + 60), "ab" * 16
    return f"v1.{digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(digest, expiry, nonce)}"


def write_snapshot(snapshot_id: str, contents: dict, kind: str = "live") -> tuple[str, dict]:
    """write a snapshot dir and return (rootfs digest, memory-half digests)."""
    d = fc_agent.SNAPSHOTS_DIR / snapshot_id
    d.mkdir(parents=True)
    for name, data in contents.items():
        (d / name).write_bytes(data)
    digest = sha(contents["rootfs.ext4"])
    files = {n: sha(contents[n]) for n in fc_agent.LIVE_FILES if n in contents}
    (d / "meta.json").write_text(json.dumps(
        {"snapshot_id": snapshot_id, "digest": digest, "kind": kind, "files": files}
    ))
    return digest, files


class LiveArtifactPullTest(unittest.TestCase):
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
        self.contents = {
            "rootfs.ext4": f"rootfs {self.name}".encode(),
            "vmstate": b"vcpu state",
            "mem": b"guest memory " * 1000,
            "live.json": b'{"index": 7}',
        }

    def test_commits_every_file_of_a_live_snapshot(self):
        digest, files = write_snapshot(f"src-{self.name}", self.contents)
        rec = fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}", files)
        self.assertEqual(rec["kind"], "live")
        self.assertEqual(rec["files"], files)
        self.assertEqual(rec["bytes"], sum(len(v) for v in self.contents.values()))
        dest = fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}"
        for name, data in self.contents.items():
            self.assertEqual((dest / name).read_bytes(), data)
        self.assertEqual(fc_agent.snapshot_kind("", f"dst-{self.name}"), "live")

    def test_a_pull_without_files_is_still_a_disk_artifact(self):
        digest, _ = write_snapshot(f"src-{self.name}", self.contents)
        rec = fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}")
        self.assertEqual(rec["kind"], "disk")
        dest = fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}"
        self.assertEqual(sorted(p.name for p in dest.iterdir()), ["meta.json", "rootfs.ext4"])

    def test_one_bad_file_commits_nothing(self):
        digest, files = write_snapshot(f"src-{self.name}", self.contents)
        files["mem"] = sha(b"not the memory the orchestrator recorded")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}", files)
        self.assertEqual(caught.exception.code, 422)
        # the rootfs verified, and must still not be there on its own.
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}").exists())
        self.assertEqual(list(fc_agent.ARTIFACT_TMP_DIR.iterdir()), [])

    def test_rejects_a_partial_or_unknown_file_set(self):
        digest, files = write_snapshot(f"src-{self.name}", self.contents)
        for bad in ({"mem": files["mem"]}, {**files, "../meta.json": files["mem"]}):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}", bad)
            self.assertEqual(caught.exception.code, 400)

    def test_serves_only_the_closed_set_of_names_and_only_under_a_grant(self):
        digest, _ = write_snapshot(f"src-{self.name}", self.contents)
        conn = fc_agent._peer_connection(self.peer)
        try:
            for name, headers, want in [
                ("mem", {}, 403),
                ("meta.json", {fc_agent.ARTIFACT_GRANT_HEADER: grant_for(digest)}, 404),
                ("mem", {fc_agent.ARTIFACT_GRANT_HEADER: grant_for(digest)}, 200),
            ]:
                conn.request("GET", f"/v1/artifacts/{digest}/files/{name}", headers=headers)
                resp = conn.getresponse()
                body = resp.read()
                self.assertEqual(resp.status, want, name)
            self.assertEqual(body, self.contents["mem"])
        finally:
            conn.close()


def write_v2_snapshot(snapshot_id: str, rootfs: bytes, mem: bytes,
                      block_table_rootfs: bool = False) -> tuple[str, dict]:
    """a version 2 snapshot laid out the way snapshot_create writes one: mem's
    digest is a hash over its chunk table, and with block_table_rootfs the
    rootfs digest is the block table one a migrate's final pause records."""
    d = fc_agent.SNAPSHOTS_DIR / snapshot_id
    d.mkdir(parents=True)
    (d / "rootfs.ext4").write_bytes(rootfs)
    (d / "mem").write_bytes(mem)
    (d / "vmstate").write_bytes(b"vcpu state")
    rootfs_digest, blocks = fc_agent.rootfs_digests(d / "rootfs.ext4")
    chunks, _ = fc_agent.chunk_digests(d / "mem")
    manifest = {"index": 7, "rootfs_blocks": blocks,
                "digest_version": fc_agent.LIVE_DIGEST_VERSION, "mem_chunks": chunks}
    (d / "live.json").write_text(json.dumps(manifest))
    digest = fc_agent.block_table_digest(blocks) if block_table_rootfs else rootfs_digest
    files = {"vmstate": sha(b"vcpu state"), "mem": fc_agent.mem_table_digest(chunks),
             "live.json": fc_agent.file_digest(d / "live.json")}
    (d / "meta.json").write_text(json.dumps(
        {"snapshot_id": snapshot_id, "digest": digest, "kind": "live", "files": files}
    ))
    return digest, files


class ChunkedDigestPullTest(unittest.TestCase):
    """a version 2 snapshot's mem, and a final-pause rootfs, are checked chunk
    by chunk against the tables in live.json rather than as one sha256."""

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
        # three 2M chunks, the last one short.
        self.mem = bytes(range(256)) * ((5 << 20) // 256)
        self.rootfs = f"rootfs {self.name}".encode()

    def pull(self, digest: str, files: dict) -> dict:
        return fc_agent.pull_artifact(digest, self.peer, grant_for(digest), f"dst-{self.name}", files,
                                      source_snapshot_id=f"src-{self.name}")

    def assertNothingCommitted(self, caught, fragment: str):
        self.assertEqual(caught.exception.code, 422)
        self.assertIn(fragment, caught.exception.msg)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}").exists())
        self.assertEqual(list(fc_agent.ARTIFACT_TMP_DIR.iterdir()), [])

    def test_commits_a_version_2_snapshot(self):
        digest, files = write_v2_snapshot(f"src-{self.name}", self.rootfs, self.mem)
        self.assertNotEqual(files["mem"], sha(self.mem), "a version 2 mem digest is not the file sha256")
        rec = self.pull(digest, files)
        self.assertEqual(rec["files"], files)
        self.assertEqual((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}" / "mem").read_bytes(), self.mem)

    def test_a_wrong_chunk_fails_at_that_chunk(self):
        digest, files = write_v2_snapshot(f"src-{self.name}", self.rootfs, self.mem)
        src = fc_agent.SNAPSHOTS_DIR / f"src-{self.name}" / "mem"
        with open(src, "r+b") as f:
            f.seek((2 << 20) + 5)
            f.write(b"!")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            self.pull(digest, files)
        self.assertNothingCommitted(caught, "chunk 1")

    def test_a_table_that_is_not_the_recorded_digest_is_refused(self):
        digest, files = write_v2_snapshot(f"src-{self.name}", self.rootfs, self.mem)
        files["mem"] = sha(self.mem)
        with self.assertRaises(fc_agent.HTTPError) as caught:
            self.pull(digest, files)
        self.assertNothingCommitted(caught, "chunk table")

    def test_an_unknown_digest_version_is_refused(self):
        digest, files = write_v2_snapshot(f"src-{self.name}", self.rootfs, self.mem)
        manifest = fc_agent.SNAPSHOTS_DIR / f"src-{self.name}" / "live.json"
        manifest.write_text(json.dumps({**json.loads(manifest.read_text()), "digest_version": 3}))
        files["live.json"] = fc_agent.file_digest(manifest)
        with self.assertRaises(fc_agent.HTTPError) as caught:
            self.pull(digest, files)
        self.assertNothingCommitted(caught, "digest_version 3")

    def test_a_block_table_rootfs_digest_is_checked_block_by_block(self):
        digest, files = write_v2_snapshot(f"src-{self.name}", self.rootfs, self.mem, block_table_rootfs=True)
        self.assertNotEqual(digest, sha(self.rootfs))
        rec = self.pull(digest, files)
        self.assertEqual(rec["digest"], digest)
        self.assertEqual((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}" / "rootfs.ext4").read_bytes(), self.rootfs)

    def test_a_wrong_rootfs_block_commits_nothing(self):
        digest, files = write_v2_snapshot(f"src-{self.name}", self.rootfs, self.mem, block_table_rootfs=True)
        (fc_agent.SNAPSHOTS_DIR / f"src-{self.name}" / "rootfs.ext4").write_bytes(b"x" * len(self.rootfs))
        with self.assertRaises(fc_agent.HTTPError) as caught:
            self.pull(digest, files)
        self.assertNothingCommitted(caught, "chunk 0")


class ChunkTableCheckTest(unittest.TestCase):
    def test_chunks_split_across_reads_still_match(self):
        data = b"a" * 10 + b"b" * 10 + b"c" * 3
        check = fc_agent.ChunkTableCheck(10, [sha(b"a" * 10), sha(b"b" * 10), sha(b"c" * 3)])
        for piece in (data[:7], data[7:15], data[15:]):
            check.update(piece)
        check.finish()

    def test_a_missing_or_extra_chunk_is_refused(self):
        check = fc_agent.ChunkTableCheck(10, [sha(b"a" * 10), sha(b"b" * 10)])
        check.update(b"a" * 10)
        with self.assertRaises(fc_agent.HTTPError):
            check.finish()
        check = fc_agent.ChunkTableCheck(10, [sha(b"a" * 10)])
        with self.assertRaises(fc_agent.HTTPError):
            check.update(b"a" * 20)


class ResumePlanTest(unittest.TestCase):
    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")
        self.vms_root = os.path.realpath(str(fc_agent.VMS_DIR))

    def snapshot(self, **manifest) -> str:
        manifest = {
            "index": 7,
            "rootfs_path": os.path.join(self.vms_root, f"gone-{self.name}", "rootfs.ext4"),
            "host": fc_agent.host_fingerprint(),
            **manifest,
        }
        write_snapshot(self.name, {
            "rootfs.ext4": b"rootfs", "vmstate": b"v", "mem": b"m",
            "live.json": json.dumps(manifest).encode(),
        })
        return self.name

    def assertRefused(self, snapshot_id: str, fragment: str):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.resume_plan(snapshot_id)
        self.assertEqual(caught.exception.code, 409)
        self.assertIn(fragment, caught.exception.msg)

    def test_accepts_a_snapshot_this_host_can_resume(self):
        plan = fc_agent.resume_plan(self.snapshot())
        self.assertEqual(plan["index"], 7)
        self.assertEqual(plan["mem"].name, "mem")

    def test_refuses_a_disk_snapshot(self):
        write_snapshot(self.name, {"rootfs.ext4": b"rootfs"}, kind="disk")
        self.assertRefused(self.name, "no memory image")

    def test_refuses_another_hosts_cpu_or_firecracker(self):
        self.assertRefused(self.snapshot(host={"arch": "amd64", "cpu": "other", "firecracker": "v0"}),
                           "cannot resume on")

    def test_refuses_a_taken_network_slot(self):
        d = fc_agent.VMS_DIR / f"holder-{self.name}"
        d.mkdir()
        (d / "meta.json").write_text(json.dumps({"index": 9}))
        self.assertRefused(self.snapshot(index=9), "network slot 9 is in use")

    def test_refuses_a_drive_path_outside_the_vm_dir_shape(self):
        for i, path in enumerate([
            "/etc/rootfs.ext4",
            os.path.join(self.vms_root, "..", "rootfs.ext4"),
            os.path.join(self.vms_root, "a", "b", "rootfs.ext4"),
            os.path.join(self.vms_root, "gone", "shadow"),
        ]):
            self.name = f"{self.name}-{i}"
            self.assertRefused(self.snapshot(rootfs_path=path), "drive path")

    def test_refuses_a_drive_path_that_is_already_a_vm_here(self):
        (fc_agent.VMS_DIR / "taken").mkdir()
        self.assertRefused(
            self.snapshot(rootfs_path=os.path.join(self.vms_root, "taken", "rootfs.ext4")),
            "drive path")


if __name__ == "__main__":
    unittest.main()
