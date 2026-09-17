"""Tests for the host-agent side of the egress policy (epic #235, #239).

Pins the four things the epic calls load-bearing: proxy mode installs no
tap -> iface accept and does install a drop; direct mode's rule sequence is
byte-for-byte what it was before egress existed; the agent forward that
carries the orchestrator's connection to fused survives proxy mode; and a
backend's config map never reaches meta.json.

Stdlib-only, no VMs: sudo is captured, never run.
"""
import importlib.util
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock


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


def capture_sudo(present=()):
    """Returns (calls, fake). fake records every argv. A -C check or -D delete
    answers 0 (rule present) only for argv suffixes listed in `present`, and
    only once each, so a purge loop sees one stale rule and then nothing."""
    calls = []
    remaining = [list(p) for p in present]

    def fake(cmd, check=True):
        calls.append(list(cmd))
        probing = "-C" in cmd or "-D" in cmd
        for rule in remaining:
            if probing and cmd[-len(rule):] == rule:
                remaining.remove(rule)
                return subprocess.CompletedProcess(cmd, 0)
        return subprocess.CompletedProcess(cmd, 1)

    return calls, fake


# The pre-egress call sequence for a direct vm, verbatim. If this changes,
# direct mode is no longer byte-for-byte what every existing host runs.
DIRECT_SEQUENCE = [
    ["ip", "link", "del", "fcv3"],
    ["ip", "tuntap", "add", "fcv3", "mode", "tap"],
    ["ip", "addr", "add", "10.200.3.1/30", "dev", "fcv3"],
    ["ip", "link", "set", "fcv3", "up"],
    ["sysctl", "-w", "net.ipv4.ip_forward=1"],
    ["iptables", "-t", "nat", "-C", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"],
    ["iptables", "-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"],
    ["iptables", "-C", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"],
    ["iptables", "-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"],
    ["iptables", "-C", "FORWARD", "-i", "eth0", "-o", "fcv3", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
    ["iptables", "-I", "FORWARD", "-i", "eth0", "-o", "fcv3", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
]

OUTBOUND_ACCEPT = ["iptables", "-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"]
OUTBOUND_DROP = ["iptables", "-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"]
PURGE_ACCEPT = ["iptables", "-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"]
PURGE_DROP = ["iptables", "-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"]
RETURN_ACCEPT = DIRECT_SEQUENCE[-1]


class SetupTapTest(unittest.TestCase):
    def test_direct_mode_is_byte_for_byte_unchanged(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            result = fc_agent.setup_tap(3, "eth0")
        self.assertEqual(result, ("fcv3", "10.200.3.1", "10.200.3.2"))
        self.assertEqual(calls, DIRECT_SEQUENCE)

    def test_explicit_direct_equals_the_default(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.setup_tap(3, "eth0", "direct")
        self.assertEqual(calls, DIRECT_SEQUENCE)

    def test_proxy_mode_installs_a_drop_and_no_accept(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            result = fc_agent.setup_tap(3, "eth0", "proxy")
        self.assertEqual(result, ("fcv3", "10.200.3.1", "10.200.3.2"))
        self.assertIn(OUTBOUND_DROP, calls)
        self.assertNotIn(OUTBOUND_ACCEPT, calls)
        # the drop must not be a bare omission: with no default-deny on the
        # chain (issue #208) an absent accept enforces nothing.
        self.assertTrue(any(c[1:3] == ["-I", "FORWARD"] and c[-1] == "DROP" for c in calls))

    def test_proxy_mode_keeps_the_return_leg_above_the_drop(self):
        # both are -I'd, so the later insert sits higher in the chain. the
        # RELATED,ESTABLISHED accept must be inserted after the drop, or the
        # reply leg of the orchestrator's dnat'd connection to fused dies.
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.setup_tap(3, "eth0", "proxy")
        self.assertLess(calls.index(OUTBOUND_DROP), calls.index(RETURN_ACCEPT))

    def test_proxy_mode_purges_a_stale_accept_first(self):
        # iptables rules outlive the tap they name. a direct vm that held
        # index 3 earlier left its accept; it must go before the drop lands.
        calls, fake = capture_sudo(present=[PURGE_ACCEPT[1:]])
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.setup_tap(3, "eth0", "proxy")
        purges = [i for i, c in enumerate(calls) if c == PURGE_ACCEPT]
        self.assertEqual(len(purges), 2, "one delete that hit, one that found nothing")
        self.assertLess(purges[-1], calls.index(OUTBOUND_DROP))

    def test_proxy_mode_shares_everything_but_the_outbound_leg_with_direct(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.setup_tap(3, "eth0", "proxy")
        outbound = {"ACCEPT", "DROP"}
        stripped = [c for c in calls if not (c[1] in ("-C", "-I", "-D") and c[2] == "FORWARD" and c[4] == "fcv3" and c[-1] in outbound)]
        expected = [c for c in DIRECT_SEQUENCE if not (c[0] == "iptables" and c[2] == "FORWARD" and c[4] == "fcv3")]
        self.assertEqual(stripped, expected)


class AgentForwardTest(unittest.TestCase):
    def test_agent_forward_never_touches_the_tap_outbound_rule(self):
        # the control plane reaches fused through PREROUTING dnat plus a
        # destination-matched FORWARD accept; none of it is keyed on the tap
        # as an input interface, so proxy mode's drop cannot cut it off.
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.add_agent_forward(19553, "10.200.3.2", "eth0")
        self.assertEqual(calls, [
            ["iptables", "-t", "nat", "-I", "PREROUTING", "-i", "eth0", "-p", "tcp", "--dport", "19553", "-j", "DNAT", "--to-destination", "10.200.3.2:9550"],
            ["iptables", "-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", "tcp", "--dport", "19553", "-j", "DNAT", "--to-destination", "10.200.3.2:9550"],
            ["iptables", "-I", "FORWARD", "-p", "tcp", "-d", "10.200.3.2", "--dport", "9550", "-j", "ACCEPT"],
            ["iptables", "-I", "INPUT", "-p", "tcp", "--dport", "19553", "-j", "ACCEPT"],
        ])
        for c in calls:
            self.assertNotIn("fcv3", c)


class EgressModeTest(unittest.TestCase):
    def test_absent_is_direct(self):
        self.assertEqual(fc_agent.parse_egress_mode({}), "direct")
        self.assertEqual(fc_agent.parse_egress_mode({"egress": None}), "direct")
        self.assertEqual(fc_agent.parse_egress_mode({"egress": {}}), "direct")

    def test_proxy(self):
        self.assertEqual(fc_agent.parse_egress_mode({"egress": {"mode": "proxy"}}), "proxy")

    def test_unknown_mode_is_a_400_not_a_fallback(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.parse_egress_mode({"egress": {"mode": "tunnel"}})
        self.assertEqual(caught.exception.code, 400)
        self.assertIn("tunnel", caught.exception.msg)

    def test_create_vm_rejects_a_bad_mode_before_allocating_anything(self):
        with mock.patch.object(fc_agent, "sudo", side_effect=AssertionError("sudo must not run")), \
             mock.patch.object(fc_agent, "pick_index", side_effect=AssertionError("no index must be picked")):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.create_vm({"name": "x", "egress": {"mode": "tunnel"}})
        self.assertEqual(caught.exception.code, 400)


class GuestNetworkFixupTest(unittest.TestCase):
    def test_direct_is_the_pre_egress_command_verbatim(self):
        self.assertEqual(
            fc_agent.guest_network_fixup("10.200.3.1", "direct"),
            "ip route show default | grep -q . || ip route add default via "
            "10.200.3.1; grep -q 1.1.1.1 /etc/resolv.conf 2>/dev/null || "
            "echo nameserver 1.1.1.1 > /etc/resolv.conf",
        )

    def test_proxy_points_the_resolver_at_a_stub(self):
        cmd = fc_agent.guest_network_fixup("10.200.3.1", "proxy")
        self.assertIn("ip route add default via 10.200.3.1", cmd)
        self.assertIn("nameserver 127.0.0.1", cmd)
        self.assertNotIn("1.1.1.1", cmd)


class EgressWireTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.vms = Path(self.tmp.name) / "vms"
        self.vms.mkdir()
        patcher = mock.patch.object(fc_agent, "VMS_DIR", self.vms)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.provisioned = []
        self.destroyed = []

        def provision(meta, body):
            self.provisioned.append((meta["vm_id"], body))
            return {"url": "socks5h://10.200.3.1:1080", "protocol": "socks5"}

        def destroy(meta):
            self.destroyed.append(meta["vm_id"])

        providers = mock.patch.dict(fc_agent.EGRESS_PROVIDERS, {"fake": (provision, destroy)}, clear=True)
        providers.start()
        self.addCleanup(providers.stop)

    def write_meta(self, vm_id, **extra):
        (self.vms / vm_id).mkdir()
        meta = {"vm_id": vm_id, "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2", "tap": "fcv3", "index": 3}
        meta.update(extra)
        fc_agent.save_meta(meta)
        return meta

    def meta_text(self, vm_id):
        return (self.vms / vm_id / "meta.json").read_text()

    def body(self, **overrides):
        b = {"provider": "fake", "protocol": "socks5", "listen_ip": "10.200.3.1",
             "guest_ip": "10.200.3.2", "config": {"token": "SECRET-warp-license"}}
        b.update(overrides)
        return b

    def test_config_never_reaches_meta_json_or_the_response(self):
        self.write_meta("vm-a", egress_mode="proxy")
        rec = fc_agent.provision_egress("vm-a", self.body())
        self.assertEqual(rec, {"provider": "fake", "protocol": "socks5", "url": "socks5h://10.200.3.1:1080"})
        text = self.meta_text("vm-a")
        self.assertNotIn("SECRET-warp-license", text)
        self.assertNotIn("config", text)
        self.assertEqual(json.loads(text)["egress"], rec)
        # the backend did receive it; that is the only place it may go.
        self.assertEqual(self.provisioned[0][1]["config"], {"token": "SECRET-warp-license"})

    def test_unknown_provider_fails_rather_than_being_ignored(self):
        self.write_meta("vm-a", egress_mode="proxy")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.provision_egress("vm-a", self.body(provider="warp"))
        self.assertEqual(caught.exception.code, 400)
        self.assertIn("'warp'", caught.exception.msg)
        self.assertIn("fake", caught.exception.msg)
        self.assertNotIn("egress", json.loads(self.meta_text("vm-a")))

    def test_direct_vm_cannot_take_a_backend(self):
        self.write_meta("vm-a", egress_mode="direct")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.provision_egress("vm-a", self.body())
        self.assertEqual(caught.exception.code, 409)
        self.assertEqual(self.provisioned, [])

    def test_double_provision_is_a_409(self):
        self.write_meta("vm-a", egress_mode="proxy")
        fc_agent.provision_egress("vm-a", self.body())
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.provision_egress("vm-a", self.body())
        self.assertEqual(caught.exception.code, 409)
        self.assertEqual(len(self.provisioned), 1)

    def test_listen_ip_must_be_the_tap_address(self):
        self.write_meta("vm-a", egress_mode="proxy")
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.provision_egress("vm-a", self.body(listen_ip="0.0.0.0"))
        self.assertEqual(caught.exception.code, 400)
        self.assertEqual(self.provisioned, [])

    def test_unknown_vm_is_a_404(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.provision_egress("vm-missing", self.body())
        self.assertEqual(caught.exception.code, 404)

    def test_release_is_idempotent_and_clears_the_record(self):
        self.write_meta("vm-a", egress_mode="proxy")
        fc_agent.provision_egress("vm-a", self.body())
        meta = fc_agent.load_meta("vm-a")
        fc_agent.release_egress(meta)
        self.assertEqual(self.destroyed, ["vm-a"])
        self.assertNotIn("egress", json.loads(self.meta_text("vm-a")))
        fc_agent.release_egress(fc_agent.load_meta("vm-a"))
        self.assertEqual(self.destroyed, ["vm-a"], "second release must not call the backend again")
        # released, the vm can be provisioned again.
        fc_agent.provision_egress("vm-a", self.body())
        self.assertEqual(len(self.provisioned), 2)

    def test_release_survives_a_failing_or_missing_backend(self):
        self.write_meta("vm-a", egress_mode="proxy", egress={"provider": "gone", "protocol": "socks5", "url": "x"})
        fc_agent.release_egress(fc_agent.load_meta("vm-a"))
        self.assertNotIn("egress", json.loads(self.meta_text("vm-a")))

        def boom(meta):
            raise RuntimeError("backend exploded")

        with mock.patch.dict(fc_agent.EGRESS_PROVIDERS, {"fake": (lambda m, b: {"url": "x"}, boom)}):
            fc_agent.provision_egress("vm-a", self.body())
            fc_agent.release_egress(fc_agent.load_meta("vm-a"))
        self.assertNotIn("egress", json.loads(self.meta_text("vm-a")))


class DestroyVMTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.vms = Path(self.tmp.name) / "vms"
        self.vms.mkdir()
        for target, value in (("VMS_DIR", self.vms), ("SSH_CONTROL_DIR", Path(self.tmp.name))):
            p = mock.patch.object(fc_agent, target, value)
            p.start()
            self.addCleanup(p.stop)
        self.destroyed = []
        providers = mock.patch.dict(fc_agent.EGRESS_PROVIDERS, {"fake": (None, self.destroyed.append)}, clear=True)
        providers.start()
        self.addCleanup(providers.stop)

    def write_meta(self, **extra):
        (self.vms / "vm-a").mkdir()
        meta = {"vm_id": "vm-a", "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2", "tap": "fcv3", "index": 3}
        meta.update(extra)
        fc_agent.save_meta(meta)

    def destroy(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake), \
             mock.patch.object(fc_agent, "stop_firecracker"), \
             mock.patch.object(fc_agent, "host_iface", return_value="eth0"):
            fc_agent.destroy_vm("vm-a")
        return calls

    def test_proxy_vm_releases_its_backend_and_its_drop(self):
        self.write_meta(egress_mode="proxy", egress={"provider": "fake", "protocol": "socks5", "url": "x"})
        calls = self.destroy()
        self.assertEqual([m["vm_id"] for m in self.destroyed], ["vm-a"])
        self.assertIn(PURGE_DROP, calls)
        self.assertLess(calls.index(PURGE_DROP), calls.index(["ip", "link", "del", "fcv3"]))

    def test_direct_vm_touches_no_egress_rule(self):
        self.write_meta(egress_mode="direct")
        calls = self.destroy()
        self.assertEqual(self.destroyed, [])
        self.assertNotIn(PURGE_DROP, calls)
        self.assertNotIn(PURGE_ACCEPT, calls)

    def test_pre_egress_meta_behaves_as_direct(self):
        # a vm created before the field existed has no egress_mode at all.
        self.write_meta()
        calls = self.destroy()
        self.assertNotIn(PURGE_DROP, calls)


class WarpBackendTest(unittest.TestCase):
    """the cloudflare-warp backend: one warp per host, refcounted, and the
    credential travels to the script on stdin and nowhere else."""

    SECRET = "svc-secret-0123456789abcdef"

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        refs = Path(self.tmp.name) / "warp-refs"
        p = mock.patch.object(fc_agent, "WARP_REFS_DIR", refs)
        p.start()
        self.addCleanup(p.stop)
        self.runs = []
        self.sudos = []

        def fake_run(cmd, check=True, input_bytes=None):
            self.runs.append((list(cmd), input_bytes))
            return subprocess.CompletedProcess(cmd, 0, stdout=b"", stderr=b"")

        def fake_sudo(cmd, check=True):
            self.sudos.append(list(cmd))
            return subprocess.CompletedProcess(cmd, 0)

        for name, fake in (("run", fake_run), ("sudo", fake_sudo)):
            p = mock.patch.object(fc_agent, name, side_effect=fake)
            p.start()
            self.addCleanup(p.stop)

    def meta(self, vm_id, idx=3):
        return {"vm_id": vm_id, "tap": f"fcv{idx}", "host_ip": f"10.200.{idx}.1", "guest_ip": f"10.200.{idx}.2"}

    def body(self, **overrides):
        b = {"provider": "cloudflare-warp", "protocol": "socks5", "listen_ip": "10.200.3.1",
             "config": {"WARP_ORG": "acme", "WARP_CLIENT_ID": "client-0123456789", "WARP_CLIENT_SECRET": self.SECRET}}
        b.update(overrides)
        return b

    def script_calls(self, verb):
        return [c for c in self.runs if c[0][-1] == verb] + [(c, None) for c in self.sudos if verb in c]

    def test_first_vm_brings_warp_up_with_the_credential_on_stdin_only(self):
        ep = fc_agent.warp_provision(self.meta("vm-a"), self.body())
        self.assertEqual(ep, {"url": "socks5h://10.200.3.1:1080", "protocol": "socks5"})
        ups = [c for c in self.runs if "up" in c[0]]
        self.assertEqual(len(ups), 1)
        argv, stdin = ups[0]
        self.assertNotIn(self.SECRET, " ".join(argv))
        self.assertIn(f"WARP_CLIENT_SECRET={self.SECRET}\n", stdin.decode())
        self.assertIn("WARP_ORG=acme\n", stdin.decode())
        # no sudo call ever carries it either.
        for c in self.sudos:
            self.assertNotIn(self.SECRET, " ".join(c))
        self.assertIn(["bash", str(fc_agent.WARP_SCRIPT), "attach", "fcv3", "10.200.3.1", "1080"], self.sudos)
        self.assertEqual(fc_agent.warp_refs(), {"vm-a"})

    def test_second_vm_shares_the_running_warp(self):
        fc_agent.warp_provision(self.meta("vm-a", 3), self.body())
        self.runs.clear(); self.sudos.clear()
        ep = fc_agent.warp_provision(self.meta("vm-b", 4), self.body(protocol="http"))
        self.assertEqual(ep, {"url": "http://10.200.4.1:1080", "protocol": "http"})
        self.assertEqual([c for c in self.runs if "up" in c[0]], [], "warp brought up twice")
        self.assertIn(["bash", str(fc_agent.WARP_SCRIPT), "attach", "fcv4", "10.200.4.1", "1080"], self.sudos)
        self.assertEqual(fc_agent.warp_refs(), {"vm-a", "vm-b"})

    def test_last_vm_out_tears_warp_down(self):
        fc_agent.warp_provision(self.meta("vm-a", 3), self.body())
        fc_agent.warp_provision(self.meta("vm-b", 4), self.body())
        self.sudos.clear()
        fc_agent.warp_destroy(self.meta("vm-a", 3))
        self.assertIn(["bash", str(fc_agent.WARP_SCRIPT), "detach", "fcv3", "10.200.3.1", "1080"], self.sudos)
        self.assertNotIn(["bash", str(fc_agent.WARP_SCRIPT), "down"], self.sudos, "torn down with a vm still attached")
        self.sudos.clear()
        fc_agent.warp_destroy(self.meta("vm-b", 4))
        self.assertIn(["bash", str(fc_agent.WARP_SCRIPT), "down"], self.sudos)
        self.assertEqual(fc_agent.warp_refs(), set())
        # a second destroy is a no-op that still runs the idempotent detach.
        fc_agent.warp_destroy(self.meta("vm-b", 4))

    def test_a_backend_that_does_not_come_up_is_a_502_without_the_credential(self):
        def failing_run(cmd, check=True, input_bytes=None):
            return subprocess.CompletedProcess(cmd, 1, stdout=b"", stderr=f"[warp] refused: bad token {self.SECRET}\n".encode())

        with mock.patch.object(fc_agent, "run", side_effect=failing_run):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.warp_provision(self.meta("vm-a"), self.body())
        self.assertEqual(caught.exception.code, 502)
        self.assertNotIn(self.SECRET, caught.exception.msg)
        self.assertIn("[REDACTED]", caught.exception.msg)
        self.assertEqual(fc_agent.warp_refs(), set(), "a failed provision must hold no ref")

    def test_rejects_bad_protocol_missing_credential_and_unknown_keys(self):
        for body, want in (
            (self.body(protocol="udp"), 400),
            (self.body(config={"WARP_ORG": "acme"}), 400),
            (self.body(config={"WARP_LICENSE": "k", "EVIL": "x"}), 400),
        ):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.warp_provision(self.meta("vm-a"), body)
            self.assertEqual(caught.exception.code, want)
        self.assertEqual(self.runs, [], "a refused request must not touch the host")

    def test_registered_under_its_name(self):
        self.assertIn("cloudflare-warp", fc_agent.EGRESS_PROVIDERS)


class ExecEnvTest(unittest.TestCase):
    """exec'd commands and attach-with-command run as a non-login bash -c
    over ssh, which reads no profile, so the agent sources the egress hook
    ahead of them. a bare attach is a login shell and reads it itself."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.vms = Path(self.tmp.name) / "vms"
        self.vms.mkdir()
        patcher = mock.patch.object(fc_agent, "VMS_DIR", self.vms)
        patcher.start()
        self.addCleanup(patcher.stop)
        (self.vms / "vm-a").mkdir()
        fc_agent.save_meta({"vm_id": "vm-a", "guest_ip": "10.200.3.2"})

    def test_exec_sources_the_egress_hook_first(self):
        with mock.patch.object(fc_agent, "ssh_exec", return_value=(0, b"", b"")) as ssh:
            fc_agent.do_exec("vm-a", ["env"])
        remote = ssh.call_args.args[1]
        self.assertTrue(remote.startswith(fc_agent.EGRESS_ENV_PREFIX), remote)
        self.assertTrue(remote.endswith("env"), remote)
        # the guard keeps a guest with no hook file unchanged.
        self.assertIn("[ -r /etc/profile.d/fuse-egress.sh ] &&", remote)

    def test_exec_still_quotes_the_command(self):
        with mock.patch.object(fc_agent, "ssh_exec", return_value=(0, b"", b"")) as ssh:
            fc_agent.do_exec("vm-a", ["sh", "-c", "echo $HTTP_PROXY"])
        remote = ssh.call_args.args[1]
        self.assertTrue(remote.endswith("sh -c 'echo $HTTP_PROXY'"), remote)

    def test_attach_with_command_sources_the_hook(self):
        argv = fc_agent.attach_argv("10.200.3.2", ["bash"])
        self.assertEqual(argv[-2:], [fc_agent.EGRESS_ENV_PREFIX, "bash"])

    def test_bare_attach_is_a_plain_login_shell(self):
        argv = fc_agent.attach_argv("10.200.3.2", [])
        self.assertEqual(argv[-2:], ["-tt", "root@10.200.3.2"])
        self.assertNotIn(fc_agent.EGRESS_ENV_PREFIX, argv)


class VMPublicTest(unittest.TestCase):
    def test_reports_the_tap_ends_and_the_mode(self):
        pub = fc_agent.vm_public({"vm_id": "vm-a", "url": "u", "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2", "egress_mode": "proxy"})
        self.assertEqual(pub, {"vm_id": "vm-a", "url": "u", "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2", "egress_mode": "proxy"})

    def test_pre_egress_meta_reads_as_direct(self):
        pub = fc_agent.vm_public({"vm_id": "vm-a"})
        self.assertEqual(pub["egress_mode"], "direct")
        self.assertEqual(pub["host_ip"], "")


if __name__ == "__main__":
    unittest.main()
