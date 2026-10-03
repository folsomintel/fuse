"""tests for virtio-mem memory hotplug.

a vm booted with FC_MEM_HOTPLUG gets fixed boot memory plus a hotplug region
the agent plugs and unplugs: fully at boot, down to the guest's use plus
headroom in the background loop, down to a tight margin before a migration
snapshot, and back to full after any resume. the unplugged blocks are holes
in that snapshot's memory image, which mem.sparse leaves out on the wire.
firecracker is faked at the fc_api seam; stdlib-only, no VMs.
"""
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
_spec = importlib.util.spec_from_file_location("fc_agent", Path(__file__).with_name("fc-agent.py"))
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

# the commit step shells out through sudo; the test store is ours already.
fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)

MIB = 1 << 20


def grant_for(digest: str) -> str:
    expiry, nonce = str(int(time.time()) + 60), "ab" * 16
    return f"v1.{digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(digest, expiry, nonce)}"


class FakeFirecracker:
    """the hotplug device, the balloon stats and the boot calls, as fc_api
    sees them. the guest plugs or unplugs to the request at once unless
    stuck is set, which models a guest that cannot free its blocks."""

    def __init__(self, total=0, plugged=0, avail_mib=None, stuck=False):
        self.total, self.plugged, self.requested = total, plugged, plugged
        self.avail_mib, self.stuck = avail_mib, stuck
        self.calls = []

    def __call__(self, sock, method, path, body=None, timeout=5.0):
        self.calls.append((method, path, body))
        if path == "/hotplug/memory" and method == "GET":
            if not self.total:
                return 400, b"no hotplug device"
            return 200, json.dumps({"total_size_mib": self.total, "block_size_mib": 2,
                                    "slot_size_mib": 128, "plugged_size_mib": self.plugged,
                                    "requested_size_mib": self.requested}).encode()
        if path == "/hotplug/memory" and method == "PATCH":
            self.requested = body["requested_size_mib"]
            if not self.stuck:
                self.plugged = self.requested
            return 204, b""
        if path == "/balloon/statistics":
            if self.avail_mib is None:
                return 400, b"statistics not enabled"
            return 200, json.dumps({"available_memory": self.avail_mib * MIB}).encode()
        return 204, b""

    def requests(self):
        return [b["requested_size_mib"] for m, p, b in self.calls if m == "PATCH" and p == "/hotplug/memory"]


class MemSplitTest(unittest.TestCase):
    def test_off_keeps_all_memory_at_boot(self):
        with mock.patch.object(fc_agent, "MEM_HOTPLUG", False):
            self.assertEqual(fc_agent.mem_split(4096), (4096, 0))

    def test_on_splits_into_boot_and_whole_slots(self):
        with mock.patch.object(fc_agent, "MEM_HOTPLUG", True):
            self.assertEqual(fc_agent.mem_split(4096), (512, 3584))
            # boot grows with size, for the region's struct pages.
            self.assertEqual(fc_agent.mem_split(65536), (4096, 61440))
            # the remainder that is not a whole slot stays boot memory.
            self.assertEqual(fc_agent.mem_split(1000), (1000 - 384, 384))

    def test_too_small_for_a_slot_gets_no_region(self):
        with mock.patch.object(fc_agent, "MEM_HOTPLUG", True):
            self.assertEqual(fc_agent.mem_split(600), (600, 0))

    def test_the_halves_always_sum_to_the_size(self):
        with mock.patch.object(fc_agent, "MEM_HOTPLUG", True):
            for size in (128, 512, 513, 640, 2048, 3000, 8192, 100000):
                boot, hotplug = fc_agent.mem_split(size)
                self.assertEqual(boot + hotplug, size)
                self.assertEqual(hotplug % fc_agent.MEM_SLOT_MIB, 0)


class HotplugLoopTest(unittest.TestCase):
    def tick(self, fc, meta=None):
        with mock.patch.object(fc_agent, "fc_api", fc):
            fc_agent.hotplug_tick(meta or {"sock": "s", "hotplug_mib": fc.total})

    def test_target_is_use_plus_headroom_rounded_to_a_block(self):
        fc = FakeFirecracker(total=3584, plugged=3584, avail_mib=3001)
        with mock.patch.object(fc_agent, "fc_api", fc):
            _, target = fc_agent.hotplug_target("s", 1024)
        # 583 in use of the region, plus 1024, up to a 2 MiB block.
        self.assertEqual(target, 1608)

    def test_target_is_clamped_to_the_region(self):
        with mock.patch.object(fc_agent, "fc_api", FakeFirecracker(total=1024, plugged=1024, avail_mib=0)):
            self.assertEqual(fc_agent.hotplug_target("s", 1024)[1], 1024)
        with mock.patch.object(fc_agent, "fc_api", FakeFirecracker(total=1024, plugged=0, avail_mib=4000)):
            self.assertEqual(fc_agent.hotplug_target("s", 1024)[1], 0)

    def test_no_stats_or_no_device_means_no_target(self):
        with mock.patch.object(fc_agent, "fc_api", FakeFirecracker(total=1024, plugged=1024)):
            self.assertIsNone(fc_agent.hotplug_target("s", 1024))
        with mock.patch.object(fc_agent, "fc_api", FakeFirecracker(avail_mib=100)):
            self.assertIsNone(fc_agent.hotplug_target("s", 1024))

    def test_grows_as_soon_as_headroom_runs_short(self):
        fc = FakeFirecracker(total=4096, plugged=1024, avail_mib=100)
        self.tick(fc)
        self.assertEqual(fc.requests(), [1024 - 100 + fc_agent.MEM_HEADROOM_MIB])

    def test_shrinks_only_once_well_over_headroom(self):
        # just over the headroom, inside the slack: left alone.
        fc = FakeFirecracker(total=4096, plugged=2048, avail_mib=fc_agent.MEM_HEADROOM_MIB + 100)
        self.tick(fc)
        self.assertEqual(fc.requests(), [])
        # far over it: unplugged down to use plus headroom.
        fc = FakeFirecracker(total=4096, plugged=4096, avail_mib=3500)
        self.tick(fc)
        self.assertEqual(fc.requests(), [4096 - 3500 + fc_agent.MEM_HEADROOM_MIB])

    def test_skips_paused_vms_and_vms_without_a_region(self):
        fc = FakeFirecracker(total=4096, plugged=1024, avail_mib=0)
        self.tick(fc, {"sock": "s", "hotplug_mib": 4096, "paused": True})
        self.tick(fc, {"sock": "s", "hotplug_mib": 0})
        self.assertEqual(fc.calls, [])


class ShrinkRegrowTest(unittest.TestCase):
    def test_shrink_unplugs_to_the_snapshot_margin_and_waits(self):
        fc = FakeFirecracker(total=4096, plugged=4096, avail_mib=3500)
        with mock.patch.object(fc_agent, "fc_api", fc):
            fc_agent.hotplug_shrink({"vm_id": "v", "sock": "s"})
        want = 4096 - 3500 + fc_agent.MEM_SNAPSHOT_HEADROOM_MIB
        self.assertEqual(fc.requests(), [want])
        self.assertEqual(fc.plugged, want)

    def test_shrink_gives_up_on_a_guest_that_cannot_unplug(self):
        fc = FakeFirecracker(total=4096, plugged=4096, avail_mib=3500, stuck=True)
        with mock.patch.object(fc_agent, "fc_api", fc), \
                mock.patch.object(fc_agent, "MEM_UNPLUG_TIMEOUT", 0.2):
            fc_agent.hotplug_shrink({"vm_id": "v", "sock": "s"})
        # best effort: it returned, and the snapshot goes ahead at this size.
        self.assertEqual(fc.plugged, 4096)

    def test_regrow_plugs_the_whole_region_and_records_it(self):
        fc = FakeFirecracker(total=4096, plugged=700)
        meta = {"sock": "s"}
        with mock.patch.object(fc_agent, "fc_api", fc):
            fc_agent.hotplug_regrow(meta)
        self.assertEqual(meta["hotplug_mib"], 4096)
        self.assertEqual(fc.requests(), [4096])

    def test_regrow_of_a_vm_without_a_region_does_nothing(self):
        fc = FakeFirecracker()
        meta = {"sock": "s", "hotplug_mib": 4096}
        with mock.patch.object(fc_agent, "fc_api", fc):
            fc_agent.hotplug_regrow(meta)
        self.assertEqual(meta["hotplug_mib"], 0)
        self.assertEqual(fc.requests(), [])


class BootTest(unittest.TestCase):
    def boot(self, hotplug: bool, memory_mb: int = 4096):
        fc = FakeFirecracker()
        meta = {"sock": "s", "cpus": 1, "memory_mb": memory_mb, "guest_ip": "172.16.0.2",
                "host_ip": "172.16.0.1", "rootfs": "/r", "tap": "t", "mac": "m"}
        with mock.patch.object(fc_agent, "fc_api", fc), \
                mock.patch.object(fc_agent, "MEM_HOTPLUG", hotplug):
            fc_agent.boot_firecracker(meta)
        return fc, meta

    def test_hotplug_vm_boots_with_the_device_and_plugs_it_all(self):
        fc, meta = self.boot(True)
        paths = [p for m, p, _ in fc.calls if m == "PUT"]
        bodies = {p: b for m, p, b in fc.calls if m == "PUT"}
        self.assertEqual(bodies["/machine-config"]["mem_size_mib"], 512)
        self.assertEqual(bodies["/hotplug/memory"]["total_size_mib"], 3584)
        self.assertEqual(bodies["/balloon"]["amount_mib"], 0, "the stats balloon must never inflate")
        self.assertIn("memhp_default_state=online_movable", bodies["/boot-source"]["boot_args"])
        # every device is configured before the guest starts.
        self.assertEqual(paths[-1], "/actions")
        self.assertEqual(fc.requests(), [3584])
        self.assertEqual(meta["hotplug_mib"], 3584)

    def test_hotplug_off_boots_exactly_as_before(self):
        fc, meta = self.boot(False)
        bodies = {p: b for m, p, b in fc.calls if m == "PUT"}
        self.assertEqual(bodies["/machine-config"]["mem_size_mib"], 4096)
        self.assertNotIn("/hotplug/memory", bodies)
        self.assertNotIn("/balloon", bodies)
        self.assertNotIn("memhp_default_state", bodies["/boot-source"]["boot_args"])
        self.assertEqual(fc.requests(), [])
        self.assertEqual(meta["hotplug_mib"], 0)


class ShrinkRequestTest(unittest.TestCase):
    def test_shrink_memory_is_a_live_snapshot_option(self):
        fc_agent.vm_dir("shrink-disk").mkdir(parents=True, exist_ok=True)
        fc_agent.save_meta({"vm_id": "shrink-disk", "sock": "s"})
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.snapshot_create("shrink-disk", "", live=False, shrink_memory=True)
        self.assertEqual(caught.exception.code, 400)


def write_sparse_snapshot(sid: str, rootfs: bytes, mem: bytes, extents: list[tuple[int, int]]) -> tuple[str, dict]:
    """lay out a shrunk live snapshot the way snapshot_create does: mem with
    holes where blocks were unplugged, and mem.sparse holding only its data.
    the extents are given directly, since the test filesystem may not report
    holes (see snapshot_fs_reports_holes)."""
    d = fc_agent.SNAPSHOTS_DIR / sid
    d.mkdir(parents=True)
    (d / "rootfs.ext4").write_bytes(rootfs)
    (d / "vmstate").write_bytes(b"vcpu state of " + sid.encode())
    (d / "mem").write_bytes(mem)
    fc_agent.write_delta(d / "mem", extents, d / fc_agent.SPARSE_MEM)
    digest = fc_agent.file_digest(d / "rootfs.ext4")
    (d / "live.json").write_text(json.dumps({"snapshot_id": sid, "index": 7, "mem_size": len(mem)}))
    names = fc_agent.LIVE_FILES + (fc_agent.SPARSE_MEM,)
    files = {n: fc_agent.file_digest(d / n) for n in names}
    (d / "meta.json").write_text(json.dumps({"snapshot_id": sid, "digest": digest, "kind": "live", "files": files}))
    return digest, files


class SparsePullTest(unittest.TestCase):
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
        # 64 pages of which only two runs hold data; the rest was unplugged.
        mem = bytearray(64 * 4096)
        mem[0:8192] = b"k" * 8192
        mem[40 * 4096:41 * 4096] = b"u" * 4096
        self.mem = bytes(mem)
        self.src = f"src-{self.name}"
        self.digest, self.files = write_sparse_snapshot(
            self.src, b"R" * 4096, self.mem, [(0, 8192), (40 * 4096, 4096)])

    def pull(self, files=None):
        return fc_agent.pull_artifact(self.digest, self.peer, grant_for(self.digest), f"dst-{self.name}",
                                      files or self.files, source_snapshot_id=self.src)

    def test_rebuilds_mem_byte_for_byte_from_only_its_data(self):
        rec = self.pull()
        dst = fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}"
        self.assertEqual((dst / "mem").read_bytes(), self.mem)
        self.assertFalse((dst / fc_agent.SPARSE_MEM).exists(), "the wire copy was committed")
        # a full record with mem's own digest, so it can move on to a third host.
        self.assertEqual(set(rec["files"]), set(fc_agent.LIVE_FILES))
        self.assertEqual(rec["files"]["mem"], self.files["mem"])
        # only the data crossed the wire, not the holes.
        self.assertLess(rec["bytes"], len(self.mem) // 4)

    def test_a_rebuild_that_does_not_match_mem_commits_nothing(self):
        files = dict(self.files, mem="0" * 64)
        with self.assertRaises(fc_agent.HTTPError) as caught:
            self.pull(files)
        self.assertEqual(caught.exception.code, 422)
        self.assertFalse((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}").exists())

    def test_a_plain_live_pull_still_moves_the_whole_image(self):
        files = {n: self.files[n] for n in fc_agent.LIVE_FILES}
        rec = self.pull(files)
        self.assertEqual((fc_agent.SNAPSHOTS_DIR / f"dst-{self.name}" / "mem").read_bytes(), self.mem)
        self.assertGreater(rec["bytes"], len(self.mem))

    def test_refuses_an_unknown_extra_file(self):
        files = dict(self.files, other="0" * 64)
        with self.assertRaises(fc_agent.HTTPError) as caught:
            self.pull(files)
        self.assertEqual(caught.exception.code, 400)


if __name__ == "__main__":
    unittest.main()
