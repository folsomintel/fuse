"""tests for per-vm epoch fencing (#374).

a vm id is reused when an environment is destroyed and created again under
the same task id, so a request the orchestrator sent for one incarnation can
reach the next. X-Fuse-Epoch names the incarnation a mutating request is
for; the agent refuses one that does not match the vm it finds, and a create
for an incarnation older than the newest it has made. these drive the real
create path and handler with every host and guest call mocked.
"""
import http.client
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
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


def fake_sudo(cmd, check=True):
    if cmd[:2] == ["rm", "-rf"]:
        shutil.rmtree(cmd[2], ignore_errors=True)
    return subprocess.CompletedProcess(cmd, 0)


class EpochTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.vms = root / "vms"
        self.vms.mkdir()
        self.state = root / "state"
        self.state.mkdir()
        rootfs = root / "rootfs.ext4"
        rootfs.write_bytes(b"rootfs")
        self.boot_fail = False

        def start(meta):
            if self.boot_fail:
                raise RuntimeError("boot failed")

        patches = [
            mock.patch.object(fc_agent, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "STATE_DIR", self.state),
            mock.patch.object(fc_agent, "BASE_ROOTFS", rootfs),
            mock.patch.object(fc_agent, "sudo", side_effect=fake_sudo),
            mock.patch.object(fc_agent, "host_iface", return_value="eth0"),
            mock.patch.object(fc_agent, "setup_tap"),
            mock.patch.object(fc_agent, "teardown_net"),
            mock.patch.object(fc_agent, "add_agent_forward"),
            mock.patch.object(fc_agent, "del_agent_forward"),
            mock.patch.object(fc_agent, "stop_firecracker"),
            mock.patch.object(fc_agent, "start_firecracker", side_effect=start),
            mock.patch.object(fc_agent, "wait_for_ssh", return_value=True),
            mock.patch.object(fc_agent, "ssh_exec", return_value=(0, b"", b"")),
            mock.patch.object(fc_agent.agentlib, "ssh_exec", return_value=(0, b"ran", b"")),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)
        self.addCleanup(self.tmp.cleanup)

    def call(self, method, path, body=None, epoch=None):
        headers = dict(AUTH)
        if epoch is not None:
            headers["X-Fuse-Epoch"] = str(epoch)
        data = json.dumps(body).encode() if body is not None else None
        if data is not None:
            headers["Content-Type"] = "application/json"
        c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=10)
        try:
            c.request(method, path, body=data, headers=headers)
            r = c.getresponse()
            return r.status, r.read()
        finally:
            c.close()

    def create(self, epoch=None, name="x"):
        return self.call("POST", "/v1/vm", {"name": name}, epoch=epoch)

    def exists(self, name="x"):
        return fc_agent.load_meta(name) is not None

    def test_newer_create_does_not_adopt_an_older_incarnation(self):
        # the same spec under the same name used to hand the new incarnation
        # the old one's vm.
        self.assertEqual(self.create(epoch=1)[0], 200)
        code, body = self.create(epoch=2)
        self.assertEqual(code, 409, body)
        self.assertEqual(fc_agent.load_meta("x")["epoch"], 1)

    def test_same_epoch_create_is_the_same_vm(self):
        self.assertEqual(self.create(epoch=1)[0], 200)
        code, body = self.create(epoch=1)
        self.assertEqual(code, 200, body)
        self.assertEqual(json.loads(body)["epoch"], 1)

    def test_request_for_another_incarnation_is_refused(self):
        self.assertEqual(self.create(epoch=2)[0], 200)
        for epoch in (1, 3):
            code, _ = self.call("POST", "/v1/vm/x/exec", {"cmd": ["true"]}, epoch=epoch)
            self.assertEqual(code, 412)
            code, _ = self.call("DELETE", "/v1/vm/x", epoch=epoch)
            self.assertEqual(code, 412)
        self.assertTrue(self.exists())
        self.assertEqual(self.call("POST", "/v1/vm/x/exec", {"cmd": ["true"]}, epoch=2)[0], 200)
        self.assertEqual(self.call("DELETE", "/v1/vm/x", epoch=2)[0], 204)
        self.assertFalse(self.exists())

    def test_stale_create_cannot_bring_back_a_destroyed_incarnation(self):
        self.assertEqual(self.create(epoch=2)[0], 200)
        self.assertEqual(self.call("DELETE", "/v1/vm/x", epoch=2)[0], 204)
        self.assertEqual(self.create(epoch=2)[0], 412)
        self.assertEqual(self.create(epoch=1)[0], 412)
        self.assertFalse(self.exists())
        self.assertEqual(self.create(epoch=3)[0], 200)

    def test_destroyed_epoch_is_kept_on_disk(self):
        self.assertEqual(self.create(epoch=5)[0], 200)
        self.assertEqual(self.call("DELETE", "/v1/vm/x", epoch=5)[0], 204)
        # a restarted agent reads the same record.
        self.assertEqual(fc_agent.agentlib.epoch_seen("x"), 5)

    def test_failed_create_can_be_retried_with_its_epoch(self):
        self.boot_fail = True
        self.assertEqual(self.create(epoch=4)[0], 500)
        self.assertFalse(self.exists())
        self.boot_fail = False
        self.assertEqual(self.create(epoch=4)[0], 200)

    def test_stale_fork_target_is_refused(self):
        self.assertEqual(self.create(epoch=1, name="src")[0], 200)
        self.assertEqual(self.create(epoch=7, name="child")[0], 200)
        self.assertEqual(self.call("DELETE", "/v1/vm/child", epoch=7)[0], 204)
        snap = Path(self.tmp.name) / "snap.ext4"
        snap.write_bytes(b"snap")
        with mock.patch.object(fc_agent, "snapshot_rootfs", return_value=snap):
            code, _ = self.call("POST", "/v1/vm/src/fork", {"name": "child", "snapshot_id": "s1"}, epoch=7)
            self.assertEqual(code, 412)
            code, body = self.call("POST", "/v1/vm/src/fork", {"name": "child", "snapshot_id": "s1"}, epoch=8)
            self.assertEqual(code, 200, body)
        self.assertEqual(fc_agent.load_meta("child")["epoch"], 8)

    def test_unfenced_requests_behave_as_before(self):
        self.assertEqual(self.create()[0], 200)
        self.assertEqual(self.create()[0], 200)
        self.assertEqual(self.call("POST", "/v1/vm/x/exec", {"cmd": ["true"]})[0], 200)
        # a vm made before fencing has no epoch, so a fenced request is let through.
        self.assertEqual(self.call("POST", "/v1/vm/x/exec", {"cmd": ["true"]}, epoch=9)[0], 200)
        self.assertEqual(self.call("DELETE", "/v1/vm/x")[0], 204)

    def test_malformed_epoch_is_rejected(self):
        for bad in ("0", "-1", "abc", "1.5"):
            self.assertEqual(self.create(epoch=bad)[0], 400)
        self.assertFalse(self.exists())

    def test_listing_reports_each_vm_epoch(self):
        self.assertEqual(self.create(epoch=3)[0], 200)
        code, body = self.call("GET", "/v1/vm")
        self.assertEqual(code, 200)
        self.assertEqual(json.loads(body)["vms"][0]["epoch"], 3)


if __name__ == "__main__":
    unittest.main()
