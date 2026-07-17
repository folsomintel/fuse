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
        qemu_agent.MIG_INVENTORY = self.root / "mig-inventory.txt"

    def tearDown(self):
        self.temp_dir.cleanup()

    def create_without_hardware(self, request):
        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]),
            mock.patch.object(qemu_agent, "pick_mig_devices", return_value=[]),
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

    def test_pick_mig_devices_by_profile(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222\n"
            "2g.20gb a100 ccc33333-3333-3333-3333-333333333333\n"
        )

        uuids = qemu_agent.pick_mig_devices(2, "1g.10gb", "a100")

        self.assertEqual(
            uuids,
            [
                "aaa11111-1111-1111-1111-111111111111",
                "bbb22222-2222-2222-2222-222222222222",
            ],
        )

    def test_pick_mig_devices_skips_allocated(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222\n"
        )
        with mock.patch.object(
            qemu_agent,
            "allocated_mdev_uuids",
            return_value={"aaa11111-1111-1111-1111-111111111111"},
        ):
            uuids = qemu_agent.pick_mig_devices(1, "1g.10gb", None)

        self.assertEqual(uuids, ["bbb22222-2222-2222-2222-222222222222"])

    def test_pick_mig_devices_insufficient_raises_409(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with self.assertRaises(qemu_agent.HTTPError) as raised:
            qemu_agent.pick_mig_devices(2, "1g.10gb", "a100")

        self.assertEqual(raised.exception.code, 409)

    def test_create_with_gpu_profile_uses_mig_path(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_slots,
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
            meta = qemu_agent.create_vm(
                {
                    "name": "mig-vm",
                    "gpus": 1,
                    "gpu_kind": "a100",
                    "gpu_profile": "1G.10GB",
                }
            )

        pick_slots.assert_not_called()
        self.assertEqual(meta["gpu_profile"], "1g.10gb")
        self.assertEqual(meta["gpu_slots"], [])
        self.assertEqual(meta["gpu_mdevs"], ["aaa11111-1111-1111-1111-111111111111"])

    def test_start_qemu_emits_mdev_sysfsdev(self):
        meta = {
            "vm_id": "mig-vm",
            "memory_mb": 1024,
            "cpus": 2,
            "rootfs": "/tmp/rootfs.qcow2",
            "tap": "qv1",
            "mac": "06:00:ac:10:01:02",
            "guest_ip": "10.200.1.2",
            "host_ip": "10.200.1.1",
            "gpu_slots": [],
            "gpu_mdevs": ["aaa11111-1111-1111-1111-111111111111"],
        }
        captured = {}

        def fake_sudo(cmd, check=True):
            if isinstance(cmd, list) and cmd and cmd[0] == "/usr/bin/qemu-system-x86_64":
                captured["argv"] = list(cmd)
            return mock.Mock(returncode=0)

        vm_path = self.root / "vms" / "mig-vm"
        vm_path.mkdir(parents=True)
        (vm_path / "qemu.pid").write_text("12345")
        (vm_path / "qmp.sock").write_text("")

        with (
            mock.patch.object(qemu_agent, "sudo", side_effect=fake_sudo),
            mock.patch.object(qemu_agent, "vm_dir", return_value=vm_path),
            mock.patch.object(qemu_agent, "QEMU_BIN", "/usr/bin/qemu-system-x86_64"),
            mock.patch.object(qemu_agent, "OVMF_CODE", Path("/usr/share/OVMF/OVMF_CODE.fd")),
            mock.patch.object(qemu_agent, "KERNEL", self.root / "vmlinuz.bin"),
            mock.patch("time.sleep"),
        ):
            qemu_agent.start_qemu(meta)

        self.assertIn("argv", captured)
        self.assertIn(
            "vfio-pci,sysfsdev=/sys/bus/mdev/devices/aaa11111-1111-1111-1111-111111111111",
            captured["argv"],
        )


if __name__ == "__main__":
    unittest.main()
