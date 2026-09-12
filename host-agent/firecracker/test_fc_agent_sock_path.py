"""Tests for sock_path's containment check.

sock_path is the second vm_id-derived path in the agent: vm_dir re-checks that
its result stays under VMS_DIR, but sock_path builds off the raw id instead of
off vm_dir's contained result, so a traversal in vm_id used to reach the
filesystem through the api socket path. These pin that it no longer can, and
that the sun_path truncation still produces a contained, collision-free name.

Stdlib-only, no VMs: importing fc-agent.py only needs FC_DIR/FC_AGENT_TOKEN.
"""
import importlib.util
import os
import tempfile
import unittest
from pathlib import Path


_import_root = tempfile.TemporaryDirectory()
_sock_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
os.environ["FC_SOCK_DIR"] = _sock_root.name
_spec = importlib.util.spec_from_file_location(
    "fc_agent", Path(__file__).with_name("fc-agent.py")
)
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

SOCK_ROOT = os.path.realpath(_sock_root.name)


class SockPathContainmentTest(unittest.TestCase):
    def test_keeps_an_ordinary_id_under_the_socket_dir(self):
        p = fc_agent.sock_path("vm-abc123")
        self.assertEqual(str(p), os.path.join(SOCK_ROOT, "vm-abc123.sock"))

    def test_rejects_a_traversal_out_of_the_socket_dir(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.sock_path("../../etc/fuse")
        self.assertEqual(caught.exception.code, 400)

    def test_rejects_an_absolute_id(self):
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.sock_path("/etc/fuse")

    def test_rejects_a_sibling_dir_sharing_the_prefix(self):
        # startswith without the trailing separator would let this through.
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.sock_path(f"../{os.path.basename(SOCK_ROOT)}-evil/x")

    def test_truncated_long_ids_stay_contained_and_distinct(self):
        long_a = "vm-" + "a" * 120
        long_b = "vm-" + "a" * 119 + "b"
        pa, pb = fc_agent.sock_path(long_a), fc_agent.sock_path(long_b)
        self.assertNotEqual(pa, pb)
        for p in (pa, pb):
            self.assertTrue(str(p).startswith(SOCK_ROOT + os.sep))
            # 24 id chars + "-" + 12 hex + ".sock"; the root's own length is
            # the operator's problem, but the derived name has to stay bounded.
            self.assertLessEqual(len(p.name), 42)


if __name__ == "__main__":
    unittest.main()
