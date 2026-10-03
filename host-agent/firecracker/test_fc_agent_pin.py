"""tests for the pinned firecracker release.

FIRECRACKER_VERSION in fc-agent.py is the one value the installers read, so
these check that every installer reads it the same way, that the pin is new
enough for what the agent relies on, and that the agent reports what the host
actually runs next to it. stdlib-only, no VMs.
"""
import importlib.util
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock


_import_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
HERE = Path(__file__).parent
_spec = importlib.util.spec_from_file_location("fc_agent", HERE / "fc-agent.py")
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

# the expression every installer reads the pin with.
PIN_SED = r's/^FIRECRACKER_VERSION = "\(v[0-9][0-9.]*\)"$/\1/p'
INSTALLERS = [
    HERE / "fc-install.sh",
    HERE / "fc-update.sh",
    HERE.parent / "local" / "fuse-local-setup.sh",
]


def version(tag: str) -> tuple[int, ...]:
    return tuple(int(p) for p in tag.lstrip("v").split("."))


class PinTest(unittest.TestCase):
    def test_the_pin_is_an_exact_release_tag(self):
        self.assertRegex(fc_agent.FIRECRACKER_VERSION, r"^v\d+\.\d+\.\d+$")

    def test_the_pin_has_the_virtio_mem_restore_fixes(self):
        # #6174 and #6176 shipped in v1.16.2 and v1.17.0; memory hotplug
        # restores are unsafe before them.
        self.assertGreaterEqual(version(fc_agent.FIRECRACKER_VERSION), (1, 16, 2))

    def test_every_installer_reads_the_pin_the_same_way(self):
        for script in INSTALLERS:
            self.assertIn(PIN_SED, script.read_text(), f"{script.name} does not read FIRECRACKER_VERSION")
            self.assertNotRegex(script.read_text(), r"firecracker-microvm/firecracker/releases/latest",
                                f"{script.name} still installs whatever firecracker is latest")

    def test_the_sed_expression_extracts_exactly_the_pin(self):
        out = subprocess.run(["sed", "-n", PIN_SED, str(HERE / "fc-agent.py")],
                             capture_output=True, text=True, check=True).stdout
        self.assertEqual(out.strip(), fc_agent.FIRECRACKER_VERSION)


class ReportTest(unittest.TestCase):
    def fake_firecracker(self, line: str) -> str:
        path = Path(tempfile.mkdtemp()) / "firecracker"
        path.write_text(f"#!/bin/sh\necho '{line}'\necho 'Supported snapshot data format versions: 8.0.0'\n")
        path.chmod(path.stat().st_mode | stat.S_IEXEC)
        return str(path)

    def test_capacity_reports_the_running_and_the_pinned_release(self):
        with mock.patch.object(fc_agent, "FC_BIN", self.fake_firecracker("Firecracker v1.16.1")):
            cap = fc_agent.host_capacity()
        self.assertEqual(cap["firecracker"], "v1.16.1")
        self.assertEqual(cap["firecracker_pinned"], fc_agent.FIRECRACKER_VERSION)

    def test_a_missing_binary_reports_empty_rather_than_failing(self):
        with mock.patch.object(fc_agent, "FC_BIN", "/nonexistent/firecracker"):
            cap = fc_agent.host_capacity()
        self.assertEqual(cap["firecracker"], "")

    def test_the_fingerprint_keeps_the_full_version_line(self):
        # resume_plan compares fingerprints recorded in existing snapshots, so
        # their shape must not change with this refactor.
        with mock.patch.object(fc_agent, "FC_BIN", self.fake_firecracker("Firecracker v1.17.0")):
            self.assertEqual(fc_agent.host_fingerprint()["firecracker"], "Firecracker v1.17.0")


if __name__ == "__main__":
    unittest.main()
