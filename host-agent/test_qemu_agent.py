import importlib.util
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock


_import_root = tempfile.TemporaryDirectory()
os.environ["QEMU_DIR"] = _import_root.name
os.environ["QEMU_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location(
    "qemu_agent", Path(__file__).with_name("qemu-agent.py")
)
qemu_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(qemu_agent)


class QEMUAgentTest(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        qemu_agent.QEMU_DIR = self.root
        qemu_agent.STATE_DIR = self.root / "agent-state"
        qemu_agent.VMS_DIR = qemu_agent.STATE_DIR / "vms"
        qemu_agent.VMS_DIR.mkdir(parents=True)
        qemu_agent.BASE_ROOTFS = self.root / "rootfs-cuda.qcow2"
        qemu_agent.IMAGES_DIR = self.root / "images"
        qemu_agent.IMAGES_DIR.mkdir()
        qemu_agent.VFIO_INVENTORY = self.root / "vfio-inventory.txt"

    def tearDown(self):
        self.temp_dir.cleanup()

    def create_without_hardware(self, request):
        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]),
            mock.patch.object(
                qemu_agent,
                "setup_tap",
                return_value=("qv1", "10.200.1.1", "10.200.1.2"),
            ),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            return qemu_agent.create_vm(request)

    def test_create_uses_default_rootfs(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")

        meta = self.create_without_hardware({"name": "default-vm"})

        self.assertEqual(meta["image"], "")
        self.assertEqual(Path(meta["rootfs"]).read_bytes(), b"default")

    def test_create_uses_named_rootfs(self):
        named = qemu_agent.IMAGES_DIR / "cuda.qcow2"
        named.write_bytes(b"named")

        meta = self.create_without_hardware({"name": "named-vm", "image": "cuda"})

        self.assertEqual(meta["image"], "cuda")
        self.assertEqual(Path(meta["rootfs"]).read_bytes(), b"named")

    def test_create_rejects_missing_rootfs_before_network_setup(self):
        with mock.patch.object(qemu_agent, "setup_tap") as setup_tap:
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.create_vm({"name": "missing-vm"})

        self.assertEqual(raised.exception.code, 400)
        setup_tap.assert_not_called()

    def test_inventory_group_includes_companion_functions(self):
        qemu_agent.VFIO_INVENTORY.write_text(
            "1 a100 0000:17:00.0 0000:17:00.1\n"
        )

        slots = qemu_agent.pick_gpu_slots(1, "a100")

        self.assertEqual(slots, ["0000:17:00.0", "0000:17:00.1"])

    def test_inventory_group_is_not_split(self):
        qemu_agent.VFIO_INVENTORY.write_text(
            "2 a100 0000:17:00.0 0000:18:00.0\n"
        )

        with self.assertRaises(qemu_agent.HTTPError) as raised:
            qemu_agent.pick_gpu_slots(1, "a100")

        self.assertEqual(raised.exception.code, 409)

    def test_used_companion_function_reserves_whole_group(self):
        qemu_agent.VFIO_INVENTORY.write_text(
            "1 a100 0000:17:00.0 0000:17:00.1\n"
        )
        with mock.patch.object(
            qemu_agent, "allocated_pci_slots", return_value={"0000:17:00.1"}
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.pick_gpu_slots(1, "a100")

        self.assertEqual(raised.exception.code, 409)

    def test_no_gpu_capacity_does_not_leave_vm_state(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")

        with self.assertRaises(qemu_agent.HTTPError):
            qemu_agent.create_vm({"name": "no-capacity", "gpus": 1})

        self.assertFalse(qemu_agent.vm_dir("no-capacity").exists())


if __name__ == "__main__":
    unittest.main()
