"""tests for parallel boots (#79).

a create holds _create_lock only while it picks a slot and saves the meta
that reserves it, then boots with it released, under its own vm lock. these
pin that other vms create and destroy while one boots, that parallel creates
never share a slot, that a failed create frees its slot, that a destroy or a
retry of the same vm waits for its create, and that a restart undoes a create
it interrupted.

stdlib-only, no VMs: every host and guest call is mocked.
"""
import http.client
import importlib.util
import os
import shutil
import subprocess
import sys
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
_spec = importlib.util.spec_from_file_location(
    "fc_agent", Path(__file__).parent.parent / "fc-agent.py"
)
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
sys.modules.pop("agentlib", None)
_spec.loader.exec_module(fc_agent)

AUTH = {"Authorization": "Bearer test-token"}
WAIT = 5.0


def fake_sudo(cmd, check=True):
    # the only sudo call whose effect a test looks at is destroy's rm -rf.
    if cmd[:2] == ["rm", "-rf"]:
        shutil.rmtree(cmd[2], ignore_errors=True)
    return subprocess.CompletedProcess(cmd, 0)


class Gate:
    """start_firecracker for a test: vms named in hold block until released,
    the rest return at once. entered[vm_id] is set once that vm is booting."""

    def __init__(self, *hold):
        self.hold = set(hold)
        self.entered = {}
        self.release = threading.Event()
        self.lock = threading.Lock()

    def started(self, vm_id):
        with self.lock:
            return self.entered.setdefault(vm_id, threading.Event())

    def __call__(self, meta):
        self.started(meta["vm_id"]).set()
        if meta["vm_id"] in self.hold:
            assert self.release.wait(WAIT), "boot was never released"


class BootTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.vms = root / "vms"
        self.vms.mkdir()
        rootfs = root / "rootfs.ext4"
        rootfs.write_bytes(b"rootfs")
        self.gate = Gate()
        self.teardown_net = mock.Mock()
        self.setup_tap = mock.Mock()
        patches = [
            mock.patch.object(fc_agent, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent, "BASE_ROOTFS", rootfs),
            mock.patch.object(fc_agent, "sudo", side_effect=fake_sudo),
            mock.patch.object(fc_agent, "host_iface", return_value="eth0"),
            mock.patch.object(fc_agent, "setup_tap", self.setup_tap),
            mock.patch.object(fc_agent, "teardown_net", self.teardown_net),
            mock.patch.object(fc_agent, "add_agent_forward"),
            mock.patch.object(fc_agent, "del_agent_forward"),
            mock.patch.object(fc_agent, "stop_firecracker"),
            mock.patch.object(fc_agent, "start_firecracker", side_effect=lambda m: self.gate(m)),
            mock.patch.object(fc_agent, "wait_for_ssh", return_value=True),
            mock.patch.object(fc_agent, "ssh_exec", return_value=(0, b"", b"")),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self.tmp.cleanup)
        # a gate left closed by a failing test must not hang the next one.
        self.addCleanup(lambda: self.gate.release.set())

    def create_in_thread(self, name):
        out = {}

        def run():
            try:
                out["meta"] = fc_agent.create_vm({"name": name})
            except Exception as e:
                out["error"] = e

        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t, out

    def booting(self, name):
        self.assertTrue(self.gate.started(name).wait(WAIT), f"{name} never started booting")


class ParallelBootTest(BootTestCase):
    def test_another_vm_creates_while_one_is_booting(self):
        self.gate.hold = {"a"}
        ta, out_a = self.create_in_thread("a")
        self.booting("a")
        tb, out_b = self.create_in_thread("b")
        tb.join(WAIT)
        self.assertFalse(tb.is_alive(), "b waited for a's boot")
        self.assertIn("meta", out_b, out_b.get("error"))
        self.gate.release.set()
        ta.join(WAIT)
        self.assertNotEqual(out_a["meta"]["index"], out_b["meta"]["index"])

    def test_create_lock_is_free_while_a_vm_boots(self):
        self.gate.hold = {"a"}
        ta, _ = self.create_in_thread("a")
        self.booting("a")
        self.assertTrue(fc_agent._create_lock.acquire(blocking=False), "boot holds _create_lock")
        fc_agent._create_lock.release()
        self.gate.release.set()
        ta.join(WAIT)

    def test_parallel_creates_never_share_a_slot(self):
        n = 8
        # every create has to be booting at once to pass the barrier, so
        # this also fails if creates still boot one at a time.
        barrier = threading.Barrier(n, timeout=WAIT)
        self.gate = Gate()
        with mock.patch.object(fc_agent, "start_firecracker", side_effect=lambda m: barrier.wait()):
            threads = [self.create_in_thread(f"vm-{i}") for i in range(n)]
            for t, _ in threads:
                t.join(WAIT * 2)
        errors = [out.get("error") for _, out in threads if "error" in out]
        self.assertEqual(errors, [])
        indices = [out["meta"]["index"] for _, out in threads]
        self.assertEqual(sorted(indices), list(range(1, n + 1)))

    def test_a_finished_create_drops_the_creating_flag(self):
        meta = fc_agent.create_vm({"name": "a"})
        self.assertNotIn("creating", meta)
        self.assertNotIn("creating", fc_agent.load_meta("a"))
        self.assertTrue(meta["ssh_ready"])


class FailedCreateTest(BootTestCase):
    def test_a_failed_boot_frees_its_slot(self):
        with mock.patch.object(fc_agent, "start_firecracker", side_effect=RuntimeError("boom")):
            with self.assertRaises(RuntimeError):
                fc_agent.create_vm({"name": "a"})
        self.assertFalse(fc_agent.vm_dir("a").exists())
        self.teardown_net.assert_called_once_with(1, "eth0")
        self.assertEqual(fc_agent.create_vm({"name": "b"})["index"], 1)

    def test_a_failed_network_setup_frees_its_slot(self):
        self.setup_tap.side_effect = subprocess.CalledProcessError(1, ["ip"])
        with self.assertRaises(subprocess.CalledProcessError):
            fc_agent.create_vm({"name": "a"})
        self.assertFalse(fc_agent.vm_dir("a").exists())
        self.assertNotIn(1, fc_agent.used_indices())

    def test_a_full_host_leaves_no_dir_behind(self):
        with mock.patch.object(fc_agent, "SLOT_MAX", 0):
            with self.assertRaises(fc_agent.HTTPError) as e:
                fc_agent.create_vm({"name": "a"})
        self.assertEqual(e.exception.code, 503)
        self.assertFalse(fc_agent.vm_dir("a").exists())


class SameVMTest(BootTestCase):
    def test_a_retry_during_boot_waits_and_gets_the_finished_vm(self):
        self.gate.hold = {"a"}
        first, _ = self.create_in_thread("a")
        self.booting("a")
        retry, out = self.create_in_thread("a")
        retry.join(0.3)
        self.assertTrue(retry.is_alive(), "the retry did not wait for the create")
        self.gate.release.set()
        first.join(WAIT)
        retry.join(WAIT)
        self.assertNotIn("creating", out["meta"])
        self.assertTrue(out["meta"]["ssh_ready"])

    def test_create_gives_up_on_a_held_vm_lock_with_a_409(self):
        # a fork holds its source's lock while it takes the new vm's, so the
        # wait is bounded instead of a deadlock between two crossed forks.
        lock = fc_agent.vm_lock("a")
        lock.acquire()
        self.addCleanup(lock.release)
        with mock.patch.object(fc_agent, "CREATE_VM_LOCK_WAIT", 0.1):
            with self.assertRaises(fc_agent.HTTPError) as e:
                fc_agent.create_vm({"name": "a"})
        self.assertEqual(e.exception.code, 409)
        self.assertFalse(fc_agent.vm_dir("a").exists())

    def test_a_fork_onto_its_own_source_is_refused_at_once(self):
        fc_agent.create_vm({"name": "a"})
        lock = fc_agent.vm_lock("a")
        with lock, mock.patch.object(fc_agent, "snapshot_rootfs", return_value=fc_agent.BASE_ROOTFS), \
             mock.patch.object(fc_agent, "CREATE_VM_LOCK_WAIT", WAIT):
            started = time.monotonic()
            with self.assertRaises(fc_agent.HTTPError) as e:
                fc_agent.fork_vm("a", {"name": "a", "snapshot_id": "s1"})
        self.assertEqual(e.exception.code, 409)
        self.assertLess(time.monotonic() - started, 1.0, "the fork waited out the lock timeout")


class DestroyLockTest(BootTestCase):
    def setUp(self):
        super().setUp()
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)

    def delete(self, vm_id):
        c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=WAIT * 2)
        try:
            c.request("DELETE", f"/v1/vm/{vm_id}", headers=AUTH)
            return c.getresponse().status
        finally:
            c.close()

    def test_destroy_of_one_vm_does_not_wait_for_another_vms_create(self):
        fc_agent.create_vm({"name": "c"})
        self.gate.hold = {"a"}
        ta, _ = self.create_in_thread("a")
        self.booting("a")
        out = {}
        td = threading.Thread(target=lambda: out.update(status=self.delete("c")), daemon=True)
        td.start()
        td.join(WAIT)
        self.assertFalse(td.is_alive(), "destroy of c waited for a's create")
        self.assertEqual(out["status"], 204)
        self.assertFalse(fc_agent.vm_dir("c").exists())
        self.gate.release.set()
        ta.join(WAIT)

    def test_destroy_racing_a_create_of_the_same_vm_waits_for_it(self):
        self.gate.hold = {"a"}
        ta, out_a = self.create_in_thread("a")
        self.booting("a")
        out = {}
        td = threading.Thread(target=lambda: out.update(status=self.delete("a")), daemon=True)
        td.start()
        td.join(0.3)
        self.assertTrue(td.is_alive(), "destroy ran in the middle of a's create")
        self.assertTrue(fc_agent.vm_dir("a").exists())
        self.gate.release.set()
        ta.join(WAIT)
        td.join(WAIT)
        self.assertIn("meta", out_a, out_a.get("error"))
        self.assertEqual(out["status"], 204)
        self.assertFalse(fc_agent.vm_dir("a").exists())
        self.assertNotIn(out_a["meta"]["index"], fc_agent.used_indices())


class RestartTest(BootTestCase):
    def write_meta(self, vm_id, index, **extra):
        fc_agent.vm_dir(vm_id).mkdir(parents=True)
        meta = {"vm_id": vm_id, "name": vm_id, "index": index, "host_port": 19550 + index,
                "guest_ip": f"10.209.0.{index}", "netns": f"fcns{index}"}
        meta.update(extra)
        fc_agent.save_meta(meta)
        return meta

    def test_restart_undoes_an_unfinished_create(self):
        self.write_meta("a", 5, creating=True)
        with mock.patch.object(fc_agent, "rebuild_net", side_effect=AssertionError("relaunched")):
            fc_agent.reattach_vms()
        self.assertFalse(fc_agent.vm_dir("a").exists())
        self.teardown_net.assert_called_once_with(5, "eth0")
        self.assertNotIn(5, fc_agent.used_indices())

    def test_restart_still_relaunches_a_finished_vm(self):
        self.write_meta("a", 5)
        with mock.patch.object(fc_agent, "rebuild_net") as rebuild:
            fc_agent.reattach_vms()
        rebuild.assert_called_once()
        self.assertTrue(fc_agent.vm_dir("a").exists())
        self.teardown_net.assert_not_called()


class ParallelResumeTest(BootTestCase):
    """every resume of one snapshot links the source's drive path for its
    load, so two at once take turns on it instead of colliding."""

    def setUp(self):
        super().setUp()
        self.store = Path(self.tmp.name) / "seed-1"
        self.store.mkdir()
        (self.store / "manifest.json").write_text("{}")
        self.loading = threading.Event()
        self.finish = threading.Event()
        self.addCleanup(self.finish.set)
        self.loads = []

        def fc_api(sock, method, path, body=None, timeout=5.0):
            if path == "/snapshot/load":
                self.loads.append(sock)
                # the symlink is in place for as long as the load runs.
                assert (self.vms / "vm-src" / "rootfs.ext4").is_symlink()
                if len(self.loads) == 1:
                    self.loading.set()
                    assert self.finish.wait(WAIT), "load was never released"
            return 204, None

        for name, fn in [("spawn_firecracker", lambda m: m.update(sock=f"/{m['vm_id']}.sock")),
                         ("fc_api", fc_api), ("fc_vm_state", lambda sock, state: None),
                         ("hotplug_regrow", lambda m: None), ("link_mem", lambda src, dst: False)]:
            p = mock.patch.object(fc_agent, name, side_effect=fn)
            p.start()
            self.addCleanup(p.stop)

    def resume(self, vm_id):
        meta = {"vm_id": vm_id, "rootfs": str(self.vms / vm_id / "rootfs.ext4")}
        fc_agent.resume_firecracker(meta, {
            "rootfs_path": str(self.vms / "vm-src" / "rootfs.ext4"),
            "vmstate": self.store / "vmstate", "mem": self.store / "mem",
            "manifest": self.store / "manifest.json", "huge_pages": False, "lazy": False,
        })

    def test_two_resumes_of_one_snapshot_both_load(self):
        errors = []

        def run(vm_id):
            try:
                self.resume(vm_id)
            except Exception as e:
                errors.append(e)

        ta = threading.Thread(target=run, args=("vm-a",), daemon=True)
        ta.start()
        self.assertTrue(self.loading.wait(WAIT))
        tb = threading.Thread(target=run, args=("vm-b",), daemon=True)
        tb.start()
        tb.join(0.2)
        self.assertTrue(tb.is_alive(), "the second resume did not wait for the first's load")
        self.finish.set()
        ta.join(WAIT)
        tb.join(WAIT)
        self.assertEqual(errors, [])
        self.assertEqual(len(self.loads), 2)
        self.assertFalse((self.vms / "vm-src").exists())

    def test_a_resume_onto_its_sources_name_is_refused(self):
        with self.assertRaises(fc_agent.HTTPError) as cm:
            self.resume("vm-src")
        self.assertEqual(cm.exception.code, 409)
        self.assertEqual(self.loads, [])


if __name__ == "__main__":
    unittest.main()
