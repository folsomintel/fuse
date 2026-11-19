"""tests for the clock fields a snapshot load sends.

a guest resumed from a memory image wakes up with the clock it was frozen
with. on an x86_64 host with linux 5.16 or newer the load asks kvm to move
kvmclock forward (clock_realtime); anywhere else the field would be refused,
so it is left out and resync_guest_clock is the only correction. stdlib-only,
no VMs.
"""
import importlib.util
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


def uname(machine: str, release: str):
    return os.uname_result(("Linux", "host", release, "#1", machine))


class LoadClockFieldsTest(unittest.TestCase):
    def test_x86_on_a_new_kernel_advances_the_clock(self):
        with mock.patch("os.uname", return_value=uname("x86_64", "6.1.0-28-amd64")):
            self.assertEqual(fc_agent.load_clock_fields(), {"clock_realtime": True})

    def test_x86_on_5_16_exactly_advances_the_clock(self):
        with mock.patch("os.uname", return_value=uname("x86_64", "5.16.0")):
            self.assertEqual(fc_agent.load_clock_fields(), {"clock_realtime": True})

    def test_x86_on_an_old_kernel_sends_nothing(self):
        with mock.patch("os.uname", return_value=uname("x86_64", "5.15.0-91-generic")):
            self.assertEqual(fc_agent.load_clock_fields(), {})

    def test_arm_sends_nothing(self):
        with mock.patch("os.uname", return_value=uname("aarch64", "6.8.0")):
            self.assertEqual(fc_agent.load_clock_fields(), {})


class ResumeSendsClockFieldsTest(unittest.TestCase):
    """resume_firecracker carries the clock fields on its /snapshot/load."""

    def setUp(self):
        self.calls = []
        names = ("spawn_firecracker", "copy_rootfs", "fc_api", "fc_vm_state", "hotplug_regrow")
        self.saved = {n: getattr(fc_agent, n) for n in names}

        def fc_api(sock, method, path, body=None, timeout=5.0):
            self.calls.append((method, path, body))
            return 204, None

        fc_agent.spawn_firecracker = lambda meta: None
        fc_agent.copy_rootfs = lambda src, dst: None
        fc_agent.fc_api = fc_api
        fc_agent.fc_vm_state = lambda sock, state: None
        fc_agent.hotplug_regrow = lambda meta: None

    def tearDown(self):
        for name, fn in self.saved.items():
            setattr(fc_agent, name, fn)

    def test_the_load_asks_for_the_clock_to_move(self):
        store = Path(_import_root.name) / "snapshots" / "seed-1"
        meta = {"vm_id": "vm-new", "sock": "/nonexistent.sock",
                "rootfs": str(fc_agent.VMS_DIR / "vm-new" / "rootfs.ext4")}
        resume = {
            "rootfs_path": str(fc_agent.VMS_DIR / "vm-src" / "rootfs.ext4"),
            "vmstate": store / "vmstate",
            "mem": store / "mem",
            "manifest": store / "manifest.json",
            "huge_pages": False,
            "lazy": False,
        }
        fc_agent.VMS_DIR.mkdir(parents=True, exist_ok=True)
        with mock.patch("os.uname", return_value=uname("x86_64", "6.1.0")):
            fc_agent.resume_firecracker(meta, resume)
        loads = [body for method, path, body in self.calls if path == "/snapshot/load"]
        self.assertEqual(len(loads), 1)
        self.assertIs(loads[0].get("clock_realtime"), True)
        self.assertEqual(meta["resumed_from"], "seed-1")


if __name__ == "__main__":
    unittest.main()
