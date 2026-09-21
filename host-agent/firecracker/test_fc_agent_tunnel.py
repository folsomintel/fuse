"""tests for the tunnel sidecar's unit and the path it is pointed at.

start_tunnel writes a unit file and a shell line into the guest from a path in
the request, so the path is held to one shape. the unit itself has two
properties worth pinning: it runs the agent binary in its sidecar role, and it
always restarts, because a published guest with no sidecar is unreachable.

stdlib-only, no VMs: importing fc-agent.py only needs FC_DIR/FC_AGENT_TOKEN.
"""
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path


_import_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location(
    "fc_agent", Path(__file__).with_name("fc-agent.py")
)
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)


class TunnelUnitTest(unittest.TestCase):
    def test_runs_the_agent_binary_as_the_sidecar_and_always_restarts(self):
        unit = fc_agent.tunnel_unit("/usr/local/bin/fused", "/fuse/tunnel.json")
        self.assertIn("ExecStart=/usr/local/bin/fused tunnel -config /fuse/tunnel.json\n", unit)
        self.assertIn("Restart=always\n", unit)
        self.assertIn("WantedBy=multi-user.target\n", unit)


class StartTunnelTest(unittest.TestCase):
    def setUp(self):
        self.vm_id = "vm-" + self.id().rsplit(".", 1)[1].replace("_", "-")
        d = fc_agent.VMS_DIR / self.vm_id
        d.mkdir(parents=True)
        (d / "meta.json").write_text(json.dumps({"vm_id": self.vm_id, "guest_ip": "10.200.9.2"}))
        self.ran = []
        self.rc = 0
        fc_agent.ssh_exec = self.fake_ssh

    def fake_ssh(self, guest_ip, remote, stdin=None, timeout=60.0):
        self.ran.append((guest_ip, remote))
        return self.rc, b"", b"fused at /usr/local/bin/fused predates the tunnel sidecar"

    def test_writes_enables_and_restarts_the_unit(self):
        fc_agent.start_tunnel(self.vm_id, "/fuse/tunnel.json")
        (guest_ip, remote), = self.ran
        self.assertEqual(guest_ip, "10.200.9.2")
        self.assertIn("/etc/systemd/system/fuse-tunnel.service", remote)
        self.assertIn("systemctl enable fuse-tunnel", remote)
        # restart, not start: a forked disk already runs the source's sidecar.
        self.assertIn("systemctl restart fuse-tunnel", remote)
        self.assertIn("tunnel -h", remote)

    def test_rejects_a_path_that_is_not_a_plain_file_under_fuse(self):
        for path in ["/etc/passwd", "/fuse/../etc/passwd", "/fuse/a b", "/fuse/x;reboot", "/fuse/", "fuse/tunnel.json"]:
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.start_tunnel(self.vm_id, path)
            self.assertEqual(caught.exception.code, 400, path)
        self.assertEqual(self.ran, [])

    def test_a_guest_that_cannot_run_the_sidecar_fails_the_start(self):
        self.rc = 65
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.start_tunnel(self.vm_id, "/fuse/tunnel.json")
        self.assertEqual(caught.exception.code, 500)
        self.assertIn("predates the tunnel sidecar", caught.exception.msg)

    def test_unknown_vm_is_404(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.start_tunnel("vm-nope", "/fuse/tunnel.json")
        self.assertEqual(caught.exception.code, 404)


if __name__ == "__main__":
    unittest.main()
