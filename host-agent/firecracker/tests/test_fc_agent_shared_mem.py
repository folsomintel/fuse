"""tests for sharing a seed's memory image across the vms resumed from it.

a file-backed resume hardlinks the seed's mem into the vm dir, so n vms
resumed from one seed hold one inode (one image's worth of page cache and
disk) instead of n copies, and deleting the seed from the store leaves every
link standing. what the guests dirty lives in firecracker's private mappings,
which only a real host can show. stdlib-only, no VMs.
"""
import importlib.util
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


_import_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location("fc_agent", Path(__file__).parent.parent / "fc-agent.py")
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
sys.modules.pop("agentlib", None)
_spec.loader.exec_module(fc_agent)

IMAGE = 1 << 20


def write_seed(snapshot_id: str) -> Path:
    d = fc_agent.SNAPSHOTS_DIR / snapshot_id
    d.mkdir(parents=True, exist_ok=True)
    (d / "mem").write_bytes(b"m" * IMAGE)
    (d / "vmstate").write_bytes(b"v")
    (d / "manifest.json").write_text(json.dumps({}))
    (d / "meta.json").write_text(json.dumps({"snapshot_id": snapshot_id, "kind": "live"}))
    return d


def vm_meta(vm_id: str) -> dict:
    fc_agent.vm_dir(vm_id).mkdir(parents=True, exist_ok=True)
    return {"vm_id": vm_id, "sock": "/nonexistent.sock",
            "rootfs": str(fc_agent.vm_dir(vm_id) / "rootfs.ext4")}


class ResumeSharesMemTest(unittest.TestCase):
    def setUp(self):
        self.loads = []
        names = ("spawn_firecracker", "fc_api", "fc_vm_state", "hotplug_regrow", "sudo")
        self.saved = {n: getattr(fc_agent, n) for n in names}

        def fc_api(sock, method, path, body=None, timeout=5.0):
            if path == "/snapshot/load":
                self.loads.append(body)
            return 204, None

        def sudo(cmd, check=True):
            # snapshot_delete's rm -rf, without sudo.
            return fc_agent.run(cmd, check=check)

        fc_agent.spawn_firecracker = lambda meta: None
        fc_agent.fc_api = fc_api
        fc_agent.fc_vm_state = lambda sock, state: None
        fc_agent.hotplug_regrow = lambda meta: None
        fc_agent.sudo = sudo

    def tearDown(self):
        for name, fn in self.saved.items():
            setattr(fc_agent, name, fn)

    def resume(self, seed: Path, vm_id: str) -> dict:
        meta = vm_meta(vm_id)
        fc_agent.resume_firecracker(meta, {
            "rootfs_path": str(fc_agent.VMS_DIR / f"src-{vm_id}" / "rootfs.ext4"),
            "vmstate": seed / "vmstate",
            "mem": seed / "mem",
            "manifest": seed / "manifest.json",
            "huge_pages": False,
            "lazy": False,
        })
        return meta

    def test_n_vms_from_one_seed_hold_one_inode(self):
        n = 8
        seed = write_seed("snap-fanout")
        metas = [self.resume(seed, f"vm-fan-{i}") for i in range(n)]
        mems = [fc_agent.vm_dir(m["vm_id"]) / "mem" for m in metas]
        inodes = {os.stat(p).st_ino for p in mems} | {os.stat(seed / "mem").st_ino}
        self.assertEqual(len(inodes), 1)
        self.assertEqual(os.stat(seed / "mem").st_nlink, n + 1)
        self.assertTrue(all(m["mem_shared"] for m in metas))
        # one image's worth of blocks on disk, not n + 1.
        self.assertEqual(sum({os.stat(p).st_ino: os.stat(p).st_blocks for p in mems}.values()),
                         os.stat(seed / "mem").st_blocks)
        # every load points firecracker at its own vm's name for the image.
        self.assertEqual([b["mem_backend"]["backend_path"] for b in self.loads], [str(p) for p in mems])

    def test_deleting_the_seed_leaves_the_vms_their_image(self):
        seed = write_seed("snap-gc")
        metas = [self.resume(seed, f"vm-gc-{i}") for i in range(3)]
        fc_agent.snapshot_delete("snap-gc")
        self.assertFalse(seed.exists())
        for m in metas:
            mem = fc_agent.vm_dir(m["vm_id"]) / "mem"
            self.assertEqual(mem.read_bytes(), b"m" * IMAGE)
            self.assertEqual(os.stat(mem).st_nlink, 3)

    def test_a_retried_resume_still_links(self):
        seed = write_seed("snap-retry")
        stale = fc_agent.vm_dir("vm-retry") / "mem"
        stale.parent.mkdir(parents=True, exist_ok=True)
        stale.write_bytes(b"left by the last attempt")
        meta = self.resume(seed, "vm-retry")
        self.assertTrue(meta["mem_shared"])
        self.assertEqual(os.stat(stale).st_ino, os.stat(seed / "mem").st_ino)

    def test_another_filesystem_falls_back_to_a_copy(self):
        seed = write_seed("snap-xdev")
        with mock.patch.object(fc_agent.os, "link", side_effect=OSError(18, "Invalid cross-device link")):
            meta = self.resume(seed, "vm-xdev")
        mem = fc_agent.vm_dir("vm-xdev") / "mem"
        self.assertFalse(meta["mem_shared"])
        self.assertNotEqual(os.stat(mem).st_ino, os.stat(seed / "mem").st_ino)
        self.assertEqual(mem.read_bytes(), b"m" * IMAGE)

    def test_hugepage_vms_keep_their_memory_private(self):
        # a 2M guest is filled by fc-uffd into its own hugepages; nothing links.
        seed = write_seed("snap-huge")
        meta = vm_meta("vm-huge")
        backend = {"backend_type": "Uffd", "backend_path": "/x.uffd"}
        with mock.patch.object(fc_agent, "start_uffd", return_value=backend) as start, \
                mock.patch.object(fc_agent, "link_mem") as link:
            fc_agent.resume_firecracker(meta, {
                "rootfs_path": str(fc_agent.VMS_DIR / "src-huge" / "rootfs.ext4"),
                "vmstate": seed / "vmstate",
                "mem": seed / "mem",
                "manifest": seed / "manifest.json",
                "huge_pages": True,
                "lazy": False,
            })
        start.assert_called_once()
        link.assert_not_called()
        self.assertFalse((fc_agent.vm_dir("vm-huge") / "mem").exists())
        self.assertEqual(os.stat(seed / "mem").st_nlink, 1)


if __name__ == "__main__":
    unittest.main()
