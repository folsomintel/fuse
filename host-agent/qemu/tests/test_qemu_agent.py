import http.client
import importlib.util
import io
import json
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
os.environ["QEMU_DIR"] = _import_root.name
os.environ["QEMU_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location(
    "qemu_agent", Path(__file__).parent.parent / "qemu-agent.py"
)
qemu_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
# a fresh agentlib per load, so each agent copy keeps the paths it was
# configured with instead of sharing the last one loaded.
sys.modules.pop("agentlib", None)
_spec.loader.exec_module(qemu_agent)


class QEMUAgentTest(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        qemu_agent.QEMU_DIR = self.root
        qemu_agent.STATE_DIR = self.root / "agent-state"
        qemu_agent.VMS_DIR = qemu_agent.STATE_DIR / "vms"
        qemu_agent.VMS_DIR.mkdir(parents=True)
        qemu_agent.agentlib.STATE_DIR = qemu_agent.STATE_DIR
        qemu_agent.agentlib.VMS_DIR = qemu_agent.VMS_DIR
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

    def assert_image_rejected(self, image):
        """create_vm must 400 on image before it allocates anything."""
        with (
            mock.patch.object(qemu_agent, "setup_tap") as setup_tap,
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_gpu_slots,
            mock.patch.object(qemu_agent, "add_agent_forward") as add_agent_forward,
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.create_vm({"name": "escape-vm", "image": image})

        self.assertEqual(raised.exception.code, 400, image)
        setup_tap.assert_not_called()
        pick_gpu_slots.assert_not_called()
        add_agent_forward.assert_not_called()
        self.assertFalse((qemu_agent.VMS_DIR / "escape-vm").exists())

    def test_create_rejects_traversal_image_names(self):
        victim = qemu_agent.VMS_DIR / "victim"
        victim.mkdir(parents=True)
        (victim / "rootfs.qcow2").write_bytes(b"victim-secrets")

        for image in (
            "../agent-state/vms/victim/rootfs",
            "../../etc/shadow",
            "sub/../../escape",
            "./../escape",
        ):
            self.assert_image_rejected(image)

    def test_create_rejects_absolute_image_names(self):
        outside = self.root / "outside.qcow2"
        outside.write_bytes(b"outside")

        self.assert_image_rejected(str(self.root / "outside"))
        self.assert_image_rejected("/etc/passwd")

    def test_create_rejects_symlink_escaping_images_dir(self):
        outside = self.root / "outside.qcow2"
        outside.write_bytes(b"outside")
        (qemu_agent.IMAGES_DIR / "linked.qcow2").symlink_to(outside)

        self.assert_image_rejected("linked")

    def test_create_rejects_symlinked_parent_escaping_images_dir(self):
        elsewhere = self.root / "elsewhere"
        elsewhere.mkdir()
        (elsewhere / "cuda.qcow2").write_bytes(b"outside")
        (qemu_agent.IMAGES_DIR / "nested").symlink_to(elsewhere)

        self.assert_image_rejected("nested/cuda")

    def test_create_rejects_control_characters_in_image_name(self):
        self.assert_image_rejected("cuda\x00/../../etc/shadow")
        self.assert_image_rejected("   ")

    def test_create_allows_nested_image_inside_images_dir(self):
        nested = qemu_agent.IMAGES_DIR / "gpu"
        nested.mkdir()
        (nested / "cuda.qcow2").write_bytes(b"nested")

        meta = self.create_without_hardware({"name": "nested-vm", "image": "gpu/cuda"})

        self.assertEqual(Path(meta["rootfs"]).read_bytes(), b"nested")

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

    def test_host_capacity_reports_mig_instance_uuids(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111 GPU-parent-1\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222 GPU-parent-1\n"
            "2g.20gb a100 ccc33333-3333-3333-3333-333333333333 GPU-parent-2\n"
        )

        cap = qemu_agent.host_capacity()

        instances = cap["mig_instances"]
        self.assertEqual(len(instances), 3)
        first = instances[0]
        self.assertEqual(first["profile"], "1g.10gb")
        self.assertEqual(first["kind"], "a100")
        self.assertEqual(first["uuid"], "aaa11111-1111-1111-1111-111111111111")
        self.assertEqual(first["parent_gpu_uuid"], "GPU-parent-1")
        # the count map is derived as a back-compat summary of the instances.
        self.assertEqual(cap["mig_profiles"], {"1g.10gb": 2, "2g.20gb": 1})

    def test_host_capacity_omits_mig_when_no_inventory(self):
        # no mig-inventory.txt: neither mig_instances nor mig_profiles appear,
        # so a cpu-only or vfio-only host reports no MIG capacity.
        cap = qemu_agent.host_capacity()
        self.assertNotIn("mig_instances", cap)
        self.assertNotIn("mig_profiles", cap)

    def test_read_mig_inventory_accepts_optional_parent_uuid(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111 GPU-parent-1\n"
            "2g.20gb a100 bbb22222-2222-2222-2222-222222222222\n"
        )

        devs = qemu_agent.read_mig_inventory()

        self.assertEqual(devs[0]["parent_gpu_uuid"], "GPU-parent-1")
        self.assertEqual(devs[1]["parent_gpu_uuid"], "")

    def test_claim_mig_devices_binds_requested_uuids(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222\n"
        )

        uuids = qemu_agent.claim_mig_devices(
            ["bbb22222-2222-2222-2222-222222222222"], "1g.10gb", "a100"
        )

        self.assertEqual(uuids, ["bbb22222-2222-2222-2222-222222222222"])

    def test_claim_mig_devices_rejects_wrong_profile(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "2g.20gb a100 ccc33333-3333-3333-3333-333333333333\n"
        )

        with self.assertRaises(qemu_agent.HTTPError) as raised:
            qemu_agent.claim_mig_devices(
                ["ccc33333-3333-3333-3333-333333333333"], "1g.10gb", None
            )

        self.assertEqual(raised.exception.code, 409)

    def test_claim_mig_devices_rejects_already_allocated(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )
        with mock.patch.object(
            qemu_agent,
            "allocated_mdev_uuids",
            return_value={"aaa11111-1111-1111-1111-111111111111"},
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.claim_mig_devices(
                    ["aaa11111-1111-1111-1111-111111111111"], "1g.10gb", None
                )

        self.assertEqual(raised.exception.code, 409)

    def test_create_binds_requested_mig_instance_uuids(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111 GPU-parent-1\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222 GPU-parent-1\n"
        )

        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_slots,
            mock.patch.object(qemu_agent, "pick_mig_devices") as pick_mig,
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
                    "gpu_profile": "1g.10gb",
                    "mig_instance_uuids": ["bbb22222-2222-2222-2222-222222222222"],
                }
            )

        # the orchestrator-chosen uuid was bound, and neither local picker ran.
        pick_slots.assert_not_called()
        pick_mig.assert_not_called()
        self.assertEqual(
            meta["gpu_mdevs"], ["bbb22222-2222-2222-2222-222222222222"]
        )

    def test_destroy_reapplies_mig_layout_when_lifecycle_managed(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with (
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
                {"name": "mig-vm", "gpus": 1, "gpu_profile": "1g.10gb"}
            )

        with (
            mock.patch.object(qemu_agent, "stop_qemu"),
            mock.patch.object(qemu_agent, "del_agent_forward"),
            mock.patch.object(qemu_agent, "teardown_tap"),
            mock.patch.object(qemu_agent, "MIG_LIFECYCLE_MANAGED", True),
            mock.patch.object(qemu_agent, "MIG_SETUP_SCRIPT", self.root / "mig-setup.sh"),
            mock.patch.object(qemu_agent.Path, "exists", return_value=True),
            mock.patch("subprocess.run") as run,
            mock.patch.object(qemu_agent, "sudo"),
        ):
            qemu_agent.destroy_vm(meta["vm_id"])

        # the lifecycle hook re-ran the setup script exactly once.
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[0], [str(self.root / "mig-setup.sh")])

    def test_destroy_skips_mig_reapply_when_not_lifecycle_managed(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with (
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
                {"name": "mig-vm", "gpus": 1, "gpu_profile": "1g.10gb"}
            )

        with (
            mock.patch.object(qemu_agent, "stop_qemu"),
            mock.patch.object(qemu_agent, "del_agent_forward"),
            mock.patch.object(qemu_agent, "teardown_tap"),
            mock.patch.object(qemu_agent, "MIG_LIFECYCLE_MANAGED", False),
            mock.patch("subprocess.run") as run,
            mock.patch.object(qemu_agent, "sudo"),
        ):
            qemu_agent.destroy_vm(meta["vm_id"])

        run.assert_not_called()

    def test_qemu_mig_setup_script_skips_without_nvidia_smi(self):
        # qemu-mig-setup.sh is an operator script run on a real MIG host, so we
        # do not exercise it against live hardware here. this test just guards
        # the contract that --list degrades (rather than crashing) when
        # nvidia-smi is absent, mirroring the skip-on-absent-hardware pattern.
        import shutil as _shutil

        if _shutil.which("nvidia-smi") is not None:
            self.skipTest("nvidia-smi present on this host; skip the absent-path test")

        import subprocess

        script = Path(qemu_agent.__file__).parent / "scripts" / "qemu-mig-setup.sh"
        proc = subprocess.run(
            [str(script), "--list"],
            capture_output=True, text=True, timeout=10,
            env={"QEMU_DIR": str(self.root), "PATH": "/usr/bin:/bin"},
        )
        # the script dies with a clear message rather than producing partial
        # output, so a missing nvidia-smi is loud, not silent.
        self.assertNotEqual(proc.returncode, 0)

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


class PublicHostTest(unittest.TestCase):
    """Pins host:port composition (issue #126): a dual-stack host resolved to
    its IPv6 address and the agent emitted "2607:5300:203:535a:::19554"."""

    def test_ipv4(self):
        self.assertEqual(qemu_agent.host_authority("203.0.113.7", 19654), "203.0.113.7:19654")

    def test_ipv6_is_bracketed(self):
        self.assertEqual(
            qemu_agent.host_authority("2607:5300:203:535a::", 19654),
            "[2607:5300:203:535a::]:19654",
        )

    def test_hostname(self):
        self.assertEqual(
            qemu_agent.host_authority("host.example.com", 8091), "host.example.com:8091"
        )

    def test_override_accepts_ip_and_hostname(self):
        for value in ("198.51.100.4", "2607:5300:203:535a::", "gpu1.example.com"):
            with mock.patch.dict(os.environ, {"PUBLIC_HOST": value}):
                self.assertEqual(qemu_agent.resolve_public_host(), value)

    def test_override_rejects_garbage(self):
        with mock.patch.dict(os.environ, {"PUBLIC_HOST": "http://2607:5300:203:535a:::8091"}):
            with self.assertRaises(SystemExit):
                qemu_agent.resolve_public_host()

    def test_probe_prefers_ipv4(self):
        def fake_probe(cmd):
            return "203.0.113.7" if "-4" in cmd else "2607:5300:203:535a::"

        with mock.patch.dict(os.environ, {"PUBLIC_HOST": ""}):
            with mock.patch.object(qemu_agent.agentlib, "_probe", side_effect=fake_probe):
                self.assertEqual(qemu_agent.resolve_public_host(), "203.0.113.7")

    def test_probe_exhausted_fails_loudly(self):
        with mock.patch.dict(os.environ, {"PUBLIC_HOST": ""}):
            with mock.patch.object(qemu_agent.agentlib, "_probe", return_value=""):
                with self.assertRaises(SystemExit):
                    qemu_agent.resolve_public_host()


class ComposeUpCommandTest(unittest.TestCase):
    """The guest runs podman, not docker.

    Compose defaults to /var/run/docker.sock, which no guest has, so an
    unqualified `docker-compose up` fails to reach any runtime. That failure is
    not contained to the service: compose runs ahead of fused under `set -e`, so
    the whole create returns a 500 and no environment starts.
    """

    def test_points_at_podmans_socket(self):
        cmd = qemu_agent.compose_up_command()
        self.assertIn("DOCKER_HOST=unix:///run/podman/podman.sock", cmd)

    def test_never_relies_on_the_docker_socket_default(self):
        # The regression this pins is an *absent* env var, so asserting the
        # docker socket is unmentioned is what actually catches a revert.
        self.assertNotIn("/var/run/docker.sock", qemu_agent.compose_up_command())

    def test_sets_the_variable_for_the_compose_command(self):
        # A prefix assignment, not a bare export earlier in the script: the
        # variable has to survive into the compose invocation itself.
        cmd = qemu_agent.compose_up_command()
        self.assertIn(
            "DOCKER_HOST=unix:///run/podman/podman.sock "
            "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d",
            cmd,
        )

    def test_is_guarded_on_the_compose_file(self):
        # An environment with no services declares no compose.yaml, and must not
        # have its boot fail on a missing file.
        self.assertTrue(
            qemu_agent.compose_up_command().startswith("if [ -f /fuse/compose.yaml ]; then")
        )

class UploadRemoteCommandTest(unittest.TestCase):
    """/fuse holds credentials in cleartext and must not be world readable.

    The upload wire carries no mode, so everything the orchestrator puts in
    /fuse used to land at the session default of 0644: the resolved secrets in
    `env`, the guest agent's `auth-token`, the TLS key. Any unprivileged process
    in the guest could read them.
    """

    RESERVED = [
        "/fuse/env",
        "/fuse/auth-token",
        "/fuse/secrets.json",
        "/fuse/manifest.json",
        "/fuse/compose.yaml",
        "/fuse/tls/key.pem",
    ]

    def test_reserved_files_are_root_only(self):
        for path in self.RESERVED:
            with self.subTest(path=path):
                cmd = qemu_agent.agentlib.upload_remote_command(path)
                self.assertIn(f"chmod 0600 {path}", cmd)

    def test_reserved_files_are_created_restricted(self):
        # chmod alone would leave the content world readable between `cat` and
        # the chmod, so the mode is also set at creation.
        for path in self.RESERVED:
            with self.subTest(path=path):
                self.assertTrue(
                    qemu_agent.agentlib.upload_remote_command(path).startswith("umask 0077 &&")
                )

    def test_reserved_directory_is_locked_down(self):
        # A pre-existing /fuse keeps its own mode through umask, so it is
        # chmod'd explicitly. A nested path locks both it and its parent.
        self.assertIn("chmod 0700 /fuse", qemu_agent.agentlib.upload_remote_command("/fuse/env"))
        nested = qemu_agent.agentlib.upload_remote_command("/fuse/tls/key.pem")
        self.assertIn("/fuse", nested)
        self.assertIn("/fuse/tls", nested)

    def test_ordinary_paths_are_unchanged(self):
        # Caller files keep the previous behaviour and the default mode: a
        # workload's own files are not the agent's to restrict.
        cmd = qemu_agent.agentlib.upload_remote_command("/workspace/app/main.py")
        self.assertEqual(cmd, "mkdir -p /workspace/app && cat > /workspace/app/main.py")

    def test_a_path_merely_prefixed_fuse_is_not_reserved(self):
        # /fused and /fuse-backup share the prefix but are not the directory.
        for path in ["/fused/x", "/fuse-backup/x"]:
            with self.subTest(path=path):
                self.assertNotIn("chmod", qemu_agent.agentlib.upload_remote_command(path))

    def test_traversal_cannot_aim_the_chmod_outside_fuse(self):
        # The dangerous shape: a reserved-looking path that normalizes out of
        # /fuse would otherwise chmod 0700 a directory like / or /etc.
        cmd = qemu_agent.agentlib.upload_remote_command("/fuse/../etc/passwd")
        self.assertNotIn("chmod", cmd)
        self.assertIn("cat > /etc/passwd", cmd)


class EgressTest(unittest.TestCase):
    """The qemu backend accepts the egress wire and refuses anything but
    direct (epic #235, #239). A refusal is honest where a silent direct would
    leave the control plane believing the vm is proxied."""

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp_dir.cleanup)
        self.root = Path(self.temp_dir.name)
        for name, value in (
            ("QEMU_DIR", self.root),
            ("STATE_DIR", self.root / "agent-state"),
            ("VMS_DIR", self.root / "agent-state" / "vms"),
            ("BASE_ROOTFS", self.root / "rootfs-cuda.qcow2"),
            ("IMAGES_DIR", self.root / "images"),
            ("VFIO_INVENTORY", self.root / "vfio-inventory.txt"),
            ("MIG_INVENTORY", self.root / "mig-inventory.txt"),
        ):
            p = mock.patch.object(qemu_agent, name, value)
            p.start()
            self.addCleanup(p.stop)
            if name in ("STATE_DIR", "VMS_DIR"):
                p = mock.patch.object(qemu_agent.agentlib, name, value)
                p.start()
                self.addCleanup(p.stop)
        qemu_agent.VMS_DIR.mkdir(parents=True)
        qemu_agent.IMAGES_DIR.mkdir()
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")

    def create(self, request):
        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]),
            mock.patch.object(qemu_agent, "host_iface", return_value="eth0"),
            mock.patch.object(qemu_agent, "setup_tap", return_value=("qv1", "10.200.1.1", "10.200.1.2")),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            return qemu_agent.create_vm(request)

    def test_absent_and_explicit_direct_both_create(self):
        self.create({"name": "plain"})
        self.create({"name": "direct", "egress": {"mode": "direct"}})
        self.assertTrue(qemu_agent.vm_dir("plain").exists())
        self.assertTrue(qemu_agent.vm_dir("direct").exists())

    def test_proxy_mode_is_refused_before_anything_is_allocated(self):
        with (
            mock.patch.object(qemu_agent, "setup_tap") as setup_tap,
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_gpu_slots,
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.create_vm({"name": "proxied", "egress": {"mode": "proxy", "provider": "mock"}})
        self.assertEqual(raised.exception.code, 400)
        self.assertIn("'proxy'", raised.exception.msg)
        setup_tap.assert_not_called()
        pick_gpu_slots.assert_not_called()
        self.assertFalse(qemu_agent.vm_dir("proxied").exists())

    def route(self, method, path, body=None):
        """Drives Handler._route without a socket: auth passes, the body is
        canned, and the two response writers are captured."""
        handler = qemu_agent.Handler.__new__(qemu_agent.Handler)
        handler.path = path
        handler._auth = lambda: True
        handler._read_json = lambda *a, **k: body or {}
        handler._json = mock.Mock(return_value=None)
        handler._text = mock.Mock(return_value=None)
        handler._route(method)
        return handler

    def test_egress_wire_provision_is_a_501(self):
        self.create({"name": "plain"})
        handler = self.route("POST", "/v1/vm/plain/egress", {"provider": "cloudflare-warp"})
        code, msg = handler._text.call_args.args
        self.assertEqual(code, 501)
        self.assertIn("direct", msg)
        handler._json.assert_not_called()

    def test_egress_wire_release_is_a_no_op(self):
        self.create({"name": "plain"})
        handler = self.route("DELETE", "/v1/vm/plain/egress")
        self.assertEqual(handler._text.call_args.args, (204, ""))

    def test_egress_wire_release_of_unknown_vm_is_a_404(self):
        handler = self.route("DELETE", "/v1/vm/missing/egress")
        self.assertEqual(handler._text.call_args.args[0], 404)

    def test_exec_sources_the_egress_hook_first(self):
        self.create({"name": "plain"})
        with mock.patch.object(qemu_agent.agentlib, "ssh_exec", return_value=(0, b"", b"")) as ssh:
            qemu_agent.do_exec("plain", ["env"])
        remote = ssh.call_args.args[1]
        self.assertTrue(remote.startswith(qemu_agent.agentlib.EGRESS_ENV_PREFIX), remote)
        self.assertTrue(remote.endswith("env"), remote)

    def test_attach_with_command_sources_the_hook_and_bare_attach_does_not(self):
        with_cmd = qemu_agent.agentlib.attach_argv("10.200.1.2", ["bash"])
        self.assertEqual(with_cmd[-2:], [qemu_agent.agentlib.EGRESS_ENV_PREFIX, "bash"])
        bare = qemu_agent.agentlib.attach_argv("10.200.1.2", [])
        self.assertEqual(bare[-2:], ["-tt", "root@10.200.1.2"])

    def test_vm_public_reports_tap_ends_and_direct(self):
        pub = qemu_agent.vm_public({"vm_id": "vm-a", "url": "u", "host_ip": "10.200.1.1", "guest_ip": "10.200.1.2"})
        self.assertEqual(pub["egress_mode"], "direct")
        self.assertEqual((pub["host_ip"], pub["guest_ip"]), ("10.200.1.1", "10.200.1.2"))



class RequestIDLogTest(unittest.TestCase):
    """the agent logs the orchestrator's X-Request-ID on every request."""

    def setUp(self):
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), qemu_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()

    def get(self, path: str, headers: dict) -> str:
        log = io.StringIO()
        with mock.patch("sys.stderr", log):
            c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=5)
            c.request("GET", path, headers={"Authorization": "Bearer test-token", **headers})
            c.getresponse().read()
            c.close()
            # the access line is written after the response; wait for it.
            for _ in range(50):
                if "HTTP" in log.getvalue():
                    break
                threading.Event().wait(0.02)
        return log.getvalue()

    def test_logs_request_id(self):
        self.assertIn("request_id=req_0123abcd", self.get("/healthz", {"X-Request-ID": "req_0123abcd"}))

    def test_drops_malformed_id(self):
        out = self.get("/healthz", {"X-Request-ID": "bad id\x1b[31m"})
        self.assertNotIn("request_id=", out)

    def test_failure_line_carries_id(self):
        with mock.patch.object(qemu_agent, "host_capacity", side_effect=RuntimeError("boom")):
            out = self.get("/v1/capacity", {"X-Request-ID": "req_fail1"})
        self.assertIn("GET /v1/capacity failed request_id=req_fail1", out)

class IdempotencyTest(unittest.TestCase):
    """a keyed mutating request runs once: a repeat and a concurrent repeat
    get the first answer, an expired key or a 5xx runs again."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state = mock.patch.object(qemu_agent.agentlib, "STATE_DIR", Path(self.tmp.name))
        self.state.start()
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), qemu_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()
        self.state.stop()
        self.tmp.cleanup()

    def call(self, method, path, body=None, key=None):
        headers = {"Authorization": "Bearer test-token"}
        if key:
            headers["Idempotency-Key"] = key
        data = json.dumps(body).encode() if body is not None else None
        c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=10)
        try:
            c.request(method, path, body=data, headers=headers)
            r = c.getresponse()
            return r.status, r.read(), r.getheader("Idempotent-Replayed")
        finally:
            c.close()

    def test_oci_images_are_refused(self):
        code, body, _ = self.call("POST", "/v1/images", {"ref": "ubuntu:24.04"})
        self.assertEqual(code, 501)
        self.assertIn(b"not supported on qemu hosts", body)
        self.assertEqual(self.call("GET", "/v1/images/" + "a" * 64)[0], 501)

    def test_repeat_create_makes_one_vm(self):
        with mock.patch.object(qemu_agent, "create_vm", return_value={"vm_id": "a", "url": "u"}) as create:
            first = self.call("POST", "/v1/vm", {"name": "a"}, key="inv_1:create.boot/1")
            second = self.call("POST", "/v1/vm", {"name": "a"}, key="inv_1:create.boot/1")
        self.assertEqual(create.call_count, 1)
        self.assertEqual(second[:2], first[:2])
        self.assertEqual(second[2], "true")

    def test_concurrent_repeat_waits_for_the_first(self):
        started, release = threading.Event(), threading.Event()

        def slow_create(req, epoch=0):
            started.set()
            release.wait(5)
            return {"vm_id": "a", "url": "u"}

        results = []
        with mock.patch.object(qemu_agent, "create_vm", side_effect=slow_create) as create:
            t1 = threading.Thread(target=lambda: results.append(self.call("POST", "/v1/vm", {"name": "a"}, key="k1")))
            t1.start()
            self.assertTrue(started.wait(5))
            t2 = threading.Thread(target=lambda: results.append(self.call("POST", "/v1/vm", {"name": "a"}, key="k1")))
            t2.start()
            time.sleep(0.2)
            self.assertEqual(results, [])
            release.set()
            t1.join(5)
            t2.join(5)
        self.assertEqual(create.call_count, 1)
        self.assertEqual(len(results), 2)
        self.assertEqual(results[0][:2], results[1][:2])

    def test_failed_save_still_replays(self):
        # a full disk after the command ran: the repeat gets the first answer
        # from memory instead of running the command again.
        with mock.patch.object(qemu_agent, "create_vm", return_value={"vm_id": "a", "url": "u"}) as create, \
                mock.patch.dict(qemu_agent.agentlib._idem_unsaved, clear=True), \
                mock.patch.object(qemu_agent.os, "replace", side_effect=OSError(28, "no space left on device")):
            first = self.call("POST", "/v1/vm", {"name": "a"}, key="k-full")
            second = self.call("POST", "/v1/vm", {"name": "a"}, key="k-full")
        self.assertEqual(create.call_count, 1)
        self.assertEqual(second[:2], first[:2])
        self.assertEqual(second[2], "true")

    def test_records_are_private(self):
        old = os.umask(0o022)
        try:
            with mock.patch.object(qemu_agent, "create_vm", return_value={"vm_id": "a", "url": "u"}):
                self.call("POST", "/v1/vm", {"name": "a"}, key="k-private")
        finally:
            os.umask(old)
        d = qemu_agent.agentlib.idem_dir()
        self.assertEqual(d.stat().st_mode & 0o777, 0o700)
        records = list(d.iterdir())
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0].stat().st_mode & 0o777, 0o600)

    def test_expired_key_runs_again(self):
        with mock.patch.object(qemu_agent, "create_vm", return_value={"vm_id": "a", "url": "u"}) as create:
            self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            with mock.patch.object(qemu_agent.agentlib, "IDEMPOTENCY_TTL", 0.0):
                time.sleep(0.01)
                self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
        self.assertEqual(create.call_count, 2)

    def test_server_error_is_not_stored(self):
        with mock.patch.object(qemu_agent, "create_vm", side_effect=[RuntimeError("boom"), {"vm_id": "a", "url": ""}]) as create, \
                mock.patch("sys.stderr", io.StringIO()):
            first, _, _ = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            second, _, _ = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
        self.assertEqual((first, second), (500, 200))
        self.assertEqual(create.call_count, 2)

    def test_repeat_destroy_replays(self):
        with mock.patch.object(qemu_agent, "destroy_vm") as destroy:
            first = self.call("DELETE", "/v1/vm/a", key="k1")
            second = self.call("DELETE", "/v1/vm/a", key="k1")
        self.assertEqual(destroy.call_count, 1)
        self.assertEqual(first[0], 204)
        self.assertEqual(second[:2], first[:2])

    def test_key_reused_for_another_request_is_a_422(self):
        with mock.patch.object(qemu_agent, "create_vm", return_value={"vm_id": "a", "url": "u"}), \
                mock.patch.object(qemu_agent, "destroy_vm") as destroy:
            self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            code, _, _ = self.call("DELETE", "/v1/vm/a", key="k1")
        self.assertEqual(code, 422)
        destroy.assert_not_called()


class NaturalIdempotencyTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.vms = mock.patch.object(qemu_agent, "VMS_DIR", Path(self.tmp.name))
        self.vms.start()
        self.lib_vms = mock.patch.object(qemu_agent.agentlib, "VMS_DIR", Path(self.tmp.name))
        self.lib_vms.start()

    def tearDown(self):
        self.lib_vms.stop()
        self.vms.stop()
        self.tmp.cleanup()

    def test_destroy_of_missing_vm_succeeds(self):
        self.assertIsNone(qemu_agent.destroy_vm("nope"))

    def test_create_with_same_spec_returns_existing_vm(self):
        # a gpu create: its own vm holds the devices, so a repeat must return
        # before picking them again.
        req = {"name": "a", "gpus": 1}
        meta = {"vm_id": "a", "name": "a", "create_spec": qemu_agent.create_spec_digest(req)}
        qemu_agent.vm_dir("a").mkdir(parents=True)
        qemu_agent.save_meta(meta)
        with mock.patch.object(qemu_agent, "pick_gpu_slots") as pick:
            self.assertEqual(qemu_agent.create_vm(dict(req)), meta)
        pick.assert_not_called()

    def test_create_with_other_spec_is_a_conflict(self):
        meta = {"vm_id": "a", "name": "a", "create_spec": qemu_agent.create_spec_digest({"name": "a"})}
        qemu_agent.vm_dir("a").mkdir(parents=True)
        qemu_agent.save_meta(meta)
        rootfs = Path(self.tmp.name) / "rootfs.qcow2"
        rootfs.write_bytes(b"")
        with mock.patch.object(qemu_agent, "BASE_ROOTFS", rootfs), \
                mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]):
            with self.assertRaises(qemu_agent.HTTPError) as e:
                qemu_agent.create_vm({"name": "a", "cpus": 4})
        self.assertEqual(e.exception.code, 409)


def fake_sudo(cmd, check=True):
    # the only sudo call whose effect a test looks at is destroy's rm -rf.
    if cmd[:2] == ["rm", "-rf"]:
        shutil.rmtree(cmd[2], ignore_errors=True)
    return subprocess.CompletedProcess(cmd, 0)


# setUp mocks setup_tap out of create; the -w test calls the real one.
REAL_SETUP_TAP = qemu_agent.setup_tap


class BootConcurrencyTest(unittest.TestCase):
    """parallel boots (#79): a create holds _create_lock only while it picks
    an index and gpus and saves the meta that reserves them, then boots under
    its own vm lock. no hardware: every host and guest call is mocked."""

    WAIT = 5.0

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp_dir.cleanup)
        root = Path(self.temp_dir.name)
        vms = root / "agent-state" / "vms"
        vms.mkdir(parents=True)
        rootfs = root / "rootfs-cuda.qcow2"
        rootfs.write_bytes(b"rootfs")
        self.hold = set()
        self.booting = {}
        self.release = threading.Event()
        self.addCleanup(self.release.set)
        self.teardown_tap = mock.Mock()
        patches = [
            mock.patch.object(qemu_agent, "VMS_DIR", vms),
            mock.patch.object(qemu_agent.agentlib, "VMS_DIR", vms),
            mock.patch.object(qemu_agent, "BASE_ROOTFS", rootfs),
            mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]),
            mock.patch.object(qemu_agent, "pick_mig_devices", return_value=[]),
            mock.patch.object(qemu_agent, "sudo", side_effect=fake_sudo),
            mock.patch.object(qemu_agent, "host_iface", return_value="eth0"),
            mock.patch.object(qemu_agent, "setup_tap"),
            mock.patch.object(qemu_agent, "teardown_tap", self.teardown_tap),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "del_agent_forward"),
            mock.patch.object(qemu_agent, "stop_qemu"),
            mock.patch.object(qemu_agent, "start_qemu", side_effect=self.start_qemu),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def start_qemu(self, meta):
        self.booting.setdefault(meta["vm_id"], threading.Event()).set()
        if meta["vm_id"] in self.hold:
            assert self.release.wait(self.WAIT), "boot was never released"

    def wait_booting(self, vm_id):
        deadline = time.monotonic() + self.WAIT
        while vm_id not in self.booting and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertIn(vm_id, self.booting, f"{vm_id} never started booting")

    def create_in_thread(self, name, **req):
        out = {}

        def run():
            try:
                out["meta"] = qemu_agent.create_vm({"name": name, **req})
            except Exception as e:
                out["error"] = e

        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t, out

    def test_pick_index_skips_the_index_of_a_saved_vm(self):
        # it used to index the meta text before parsing it, swallow the
        # TypeError and hand every vm index 1.
        qemu_agent.vm_dir("a").mkdir(parents=True)
        qemu_agent.save_meta({"vm_id": "a", "index": 1})
        self.assertEqual(qemu_agent.pick_index(), 2)

    def test_a_full_host_is_a_503_and_leaves_no_dir(self):
        with mock.patch.object(qemu_agent, "pick_index",
                               side_effect=qemu_agent.HTTPError(503, "no free vm index")):
            with self.assertRaises(qemu_agent.HTTPError) as e:
                qemu_agent.create_vm({"name": "a"})
        self.assertEqual(e.exception.code, 503)
        self.assertFalse(qemu_agent.vm_dir("a").exists())

    def test_another_vm_creates_while_one_is_booting(self):
        self.hold = {"a"}
        ta, out_a = self.create_in_thread("a")
        self.wait_booting("a")
        # a's meta already reserves its index and gpus while it boots.
        reserved = qemu_agent.load_meta("a")
        self.assertTrue(reserved["creating"])
        self.assertIn("gpu_slots", reserved)
        tb, out_b = self.create_in_thread("b")
        tb.join(self.WAIT)
        self.assertFalse(tb.is_alive(), "b waited for a's boot")
        self.assertIn("meta", out_b, out_b.get("error"))
        self.release.set()
        ta.join(self.WAIT)
        self.assertEqual({out_a["meta"]["index"], out_b["meta"]["index"]}, {1, 2})
        self.assertNotIn("creating", qemu_agent.load_meta("a"))

    def test_parallel_creates_never_share_an_index(self):
        n = 6
        barrier = threading.Barrier(n, timeout=self.WAIT)
        with mock.patch.object(qemu_agent, "start_qemu", side_effect=lambda m: barrier.wait()):
            threads = [self.create_in_thread(f"vm-{i}") for i in range(n)]
            for t, _ in threads:
                t.join(self.WAIT * 2)
        self.assertEqual([o["error"] for _, o in threads if "error" in o], [])
        self.assertEqual(sorted(o["meta"]["index"] for _, o in threads), list(range(1, n + 1)))

    def test_a_failed_boot_frees_its_index(self):
        with mock.patch.object(qemu_agent, "start_qemu", side_effect=RuntimeError("boom")):
            with self.assertRaises(RuntimeError):
                qemu_agent.create_vm({"name": "a"})
        self.assertFalse(qemu_agent.vm_dir("a").exists())
        self.teardown_tap.assert_called_once_with("qv1")
        self.assertEqual(qemu_agent.create_vm({"name": "b"})["index"], 1)

    def test_destroy_waits_for_its_own_create_but_not_another_vms(self):
        srv = ThreadingHTTPServer(("127.0.0.1", 0), qemu_agent.Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)

        def delete(vm_id, out):
            c = http.client.HTTPConnection("127.0.0.1", srv.server_port, timeout=self.WAIT * 2)
            try:
                c.request("DELETE", f"/v1/vm/{vm_id}", headers={"Authorization": "Bearer test-token"})
                out["status"] = c.getresponse().status
            finally:
                c.close()

        qemu_agent.create_vm({"name": "c"})
        self.hold = {"a"}
        ta, _ = self.create_in_thread("a")
        self.wait_booting("a")
        other, same = {}, {}
        t_other = threading.Thread(target=delete, args=("c", other), daemon=True)
        t_same = threading.Thread(target=delete, args=("a", same), daemon=True)
        t_other.start()
        t_same.start()
        t_other.join(self.WAIT)
        self.assertEqual(other.get("status"), 204, "destroy of c waited for a's create")
        self.assertTrue(t_same.is_alive(), "destroy of a ran in the middle of its create")
        self.release.set()
        ta.join(self.WAIT)
        t_same.join(self.WAIT)
        self.assertEqual(same.get("status"), 204)
        self.assertFalse(qemu_agent.vm_dir("a").exists())

    def test_restart_undoes_an_unfinished_create(self):
        qemu_agent.vm_dir("a").mkdir(parents=True)
        qemu_agent.save_meta({"vm_id": "a", "index": 4, "tap": "qv4", "host_port": 19554,
                              "guest_ip": "10.200.4.2", "creating": True})
        with mock.patch.object(qemu_agent, "start_qemu", side_effect=AssertionError("relaunched")):
            qemu_agent.reattach_vms()
        self.assertFalse(qemu_agent.vm_dir("a").exists())
        self.teardown_tap.assert_called_once_with("qv4")

    def test_restart_kills_a_qemu_whose_pid_was_never_saved(self):
        qemu_agent.vm_dir("a").mkdir(parents=True)
        (qemu_agent.vm_dir("a") / "qemu.pid").write_text("4242\n")
        qemu_agent.save_meta({"vm_id": "a", "index": 4, "tap": "qv4", "host_port": 19554,
                              "guest_ip": "10.200.4.2", "creating": True})
        with mock.patch.object(qemu_agent, "start_qemu", side_effect=AssertionError("relaunched")):
            qemu_agent.reattach_vms()
        self.assertEqual(qemu_agent.stop_qemu.call_args.args[0]["pid"], 4242)
        self.assertFalse(qemu_agent.vm_dir("a").exists())

    def test_every_iptables_call_waits_for_the_lock(self):
        calls = []

        def record(cmd, check=True):
            calls.append(list(cmd))
            return subprocess.CompletedProcess(cmd, 1)

        with mock.patch.object(qemu_agent, "sudo", side_effect=record):
            REAL_SETUP_TAP(3, "eth0")
            qemu_agent.add_expose_forward(30000, "10.200.3.2", 8080)
            qemu_agent.del_expose_forward(30000, "10.200.3.2", 8080)
        ipt = [c for c in calls if "iptables" in c]
        self.assertTrue(ipt)
        for c in ipt:
            self.assertEqual(c[:3], ["iptables", "-w", "30"], c)


class EpochTest(unittest.TestCase):
    """per-vm epoch fencing (#374), as in test_fc_agent_epoch: a create for
    a newer incarnation never adopts an older one's vm, a stale create
    cannot bring back a destroyed incarnation, and a mutating request for
    another incarnation is refused."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        state = root / "agent-state"
        (state / "vms").mkdir(parents=True)
        (root / "rootfs.qcow2").write_bytes(b"default")
        patches = [
            mock.patch.object(qemu_agent, "STATE_DIR", state),
            mock.patch.object(qemu_agent, "VMS_DIR", state / "vms"),
            mock.patch.object(qemu_agent.agentlib, "STATE_DIR", state),
            mock.patch.object(qemu_agent.agentlib, "VMS_DIR", state / "vms"),
            mock.patch.object(qemu_agent, "BASE_ROOTFS", root / "rootfs.qcow2"),
            mock.patch.object(qemu_agent, "VFIO_INVENTORY", root / "vfio-inventory.txt"),
            mock.patch.object(qemu_agent, "MIG_INVENTORY", root / "mig-inventory.txt"),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), qemu_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)

    def create(self, epoch):
        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]),
            mock.patch.object(qemu_agent, "pick_mig_devices", return_value=[]),
            mock.patch.object(qemu_agent, "setup_tap", return_value=("qv1", "10.200.1.1", "10.200.1.2")),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            return qemu_agent.create_vm({"name": "x"}, epoch=epoch)

    def call(self, method, path, body=None, epoch=None):
        headers = {"Authorization": "Bearer test-token"}
        if epoch is not None:
            headers["X-Fuse-Epoch"] = str(epoch)
        data = json.dumps(body).encode() if body is not None else None
        c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=10)
        try:
            c.request(method, path, body=data, headers=headers)
            r = c.getresponse()
            return r.status, r.read()
        finally:
            c.close()

    def destroy(self, epoch):
        with (
            mock.patch.object(qemu_agent, "stop_qemu"),
            mock.patch.object(qemu_agent, "del_agent_forward"),
            mock.patch.object(qemu_agent, "teardown_tap"),
            mock.patch.object(qemu_agent, "sudo", side_effect=lambda cmd, check=True: shutil.rmtree(cmd[2], ignore_errors=True)),
        ):
            return self.call("DELETE", "/v1/vm/x", epoch=epoch)[0]

    def test_newer_create_does_not_adopt_an_older_incarnation(self):
        self.assertEqual(self.create(1)["epoch"], 1)
        self.assertEqual(self.create(1)["epoch"], 1)
        with self.assertRaises(qemu_agent.HTTPError) as e:
            self.create(2)
        self.assertEqual(e.exception.code, 409)

    def test_stale_create_cannot_bring_back_a_destroyed_incarnation(self):
        self.create(2)
        self.assertEqual(self.destroy(2), 204)
        for epoch in (1, 2):
            with self.assertRaises(qemu_agent.HTTPError) as e:
                self.create(epoch)
            self.assertEqual(e.exception.code, 412)
        self.assertEqual(self.create(3)["epoch"], 3)

    def test_request_for_another_incarnation_is_refused(self):
        self.create(2)
        with mock.patch.object(qemu_agent.agentlib, "ssh_exec", return_value=(0, b"", b"")):
            self.assertEqual(self.call("POST", "/v1/vm/x/exec", {"cmd": ["true"]}, epoch=1)[0], 412)
            self.assertEqual(self.call("POST", "/v1/vm/x/exec", {"cmd": ["true"]}, epoch=2)[0], 200)
        self.assertEqual(self.destroy(3), 412)
        self.assertIsNotNone(qemu_agent.load_meta("x"))
        self.assertEqual(self.destroy(2), 204)
        self.assertIsNone(qemu_agent.load_meta("x"))

    def test_listing_reports_each_vm_epoch(self):
        self.create(4)
        code, body = self.call("GET", "/v1/vm")
        self.assertEqual(code, 200)
        self.assertEqual(json.loads(body)["vms"][0]["epoch"], 4)


if __name__ == "__main__":
    unittest.main()
