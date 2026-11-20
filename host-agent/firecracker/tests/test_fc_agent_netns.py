"""tests for the per-vm network namespace (#419).

every vm gets a namespace with the same guest network inside, a veth to the
host and a 16-bit slot that names both. these pin the slot pool (no 249 cap,
lowest free slot reused), that setup and teardown leave nothing behind on the
host (#211), that the egress rules follow the veth, and that firecracker is
launched where its tap is.

stdlib-only, no VMs: sudo goes to FakeHost, a small model of the links,
namespaces and iptables chains it would have changed.
"""
import importlib.util
import json
import os
import shutil
import subprocess
import sys
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
sys.modules.pop("agentlib", None)
_spec.loader.exec_module(fc_agent)


class FakeHost:
    """the state sudo would change: links as (namespace, name) with "" for the
    host, veth peers, namespaces, and iptables rules per (namespace, table),
    top of chain first. good enough to say what is left after a teardown."""

    def __init__(self):
        self.links: set[tuple[str, str]] = set()
        self.peers: dict[tuple[str, str], tuple[str, str]] = {}
        self.netns: set[str] = set()
        self.rules: dict[tuple[str, str], list[list[str]]] = {}

    def __call__(self, cmd, check=True):
        rc = self.apply("", list(cmd))
        if check and rc != 0:
            raise subprocess.CalledProcessError(rc, cmd)
        return subprocess.CompletedProcess(cmd, rc)

    def apply(self, ns: str, cmd: list[str]) -> int:
        if cmd[:2] == ["rm", "-rf"]:
            shutil.rmtree(cmd[2], ignore_errors=True)
            return 0
        if cmd[:3] == ["ip", "netns", "exec"]:
            return self.apply(cmd[3], cmd[4:]) if cmd[3] in self.netns else 1
        if cmd[:3] == ["ip", "netns", "add"]:
            if cmd[3] in self.netns:
                return 1
            self.netns.add(cmd[3])
            return 0
        if cmd[:3] == ["ip", "netns", "del"]:
            if cmd[3] not in self.netns:
                return 1
            self.netns.discard(cmd[3])
            for link in [lk for lk in self.links if lk[0] == cmd[3]]:
                self.del_link(link)
            self.rules = {k: v for k, v in self.rules.items() if k[0] != cmd[3]}
            return 0
        if cmd[:3] == ["ip", "link", "add"]:
            # ip link add <a> type veth peer name <b> netns <ns>
            a, b = (ns, cmd[3]), (cmd[10], cmd[8])
            self.links |= {a, b}
            self.peers[a], self.peers[b] = b, a
            return 0
        if cmd[:3] == ["ip", "link", "del"]:
            if (ns, cmd[3]) not in self.links:
                return 1
            self.del_link((ns, cmd[3]))
            return 0
        if cmd[:3] == ["ip", "tuntap", "add"]:
            self.links.add((ns, cmd[3]))
            return 0
        if cmd[0] == "iptables":
            table = "filter"
            rest = cmd[1:]
            if rest[0] == "-t":
                table, rest = rest[1], rest[2:]
            verb, rule = rest[0], rest[1:]
            chain = self.rules.setdefault((ns, table), [])
            if verb == "-C":
                return 0 if rule in chain else 1
            if verb == "-I":
                chain.insert(0, rule)
            elif verb == "-A":
                chain.append(rule)
            elif verb == "-D":
                if rule not in chain:
                    return 1
                chain.remove(rule)
            return 0
        return 0

    def del_link(self, link: tuple[str, str]) -> None:
        self.links.discard(link)
        peer = self.peers.pop(link, None)
        if peer:
            self.peers.pop(peer, None)
            self.links.discard(peer)

    def host_rules(self) -> list[list[str]]:
        return [r for (ns, _), chain in self.rules.items() if ns == "" for r in chain]


class FakeHostCase(unittest.TestCase):
    def setUp(self):
        self.host = FakeHost()
        for mod in (fc_agent, fc_agent.agentlib):
            p = mock.patch.object(mod, "sudo", side_effect=self.host)
            p.start()
            self.addCleanup(p.stop)

    def mentions(self, dev: str) -> list[list[str]]:
        return [r for r in self.host.host_rules() if dev in r]


class SlotTest(unittest.TestCase):
    def test_addresses_carry_the_slots_two_bytes(self):
        self.assertEqual(fc_agent.slot_addrs(1), ("10.208.0.1", "10.209.0.1"))
        self.assertEqual(fc_agent.slot_addrs(300), ("10.208.1.44", "10.209.1.44"))
        self.assertEqual(fc_agent.slot_addrs(0x1234), ("10.208.18.52", "10.209.18.52"))
        self.assertEqual((fc_agent.tap_name(300), fc_agent.netns_name(300)), ("fcv300", "fcns300"))

    def test_every_slot_name_fits_an_interface_name(self):
        self.assertLessEqual(len(fc_agent.tap_name(fc_agent.SLOT_MAX)), 15)

    def test_the_agent_port_of_the_last_slot_is_a_port(self):
        # below the ephemeral range expose ports are drawn from, so an agent
        # port and an expose dnat can never be handed the same number.
        start = fc_agent._ephemeral_start()
        self.assertLess(fc_agent.HOST_PORT_BASE + fc_agent.SLOT_MAX, start)
        if start == 32768:
            self.assertEqual(fc_agent.SLOT_MAX, 13217)

    def test_a_legacy_guest_network_names_its_own_slot(self):
        self.assertEqual(fc_agent.legacy_slot(fc_agent.legacy_guest_net(5)), 5)
        self.assertEqual(fc_agent.legacy_slot(fc_agent.GUEST_NET), 0)
        self.assertEqual(fc_agent.legacy_slot({**fc_agent.legacy_guest_net(5), "mac": "06:00:AC:10:0c:02"}), 0)


class PickIndexTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        p = mock.patch.object(fc_agent, "VMS_DIR", Path(self.tmp.name))
        p.start()
        self.addCleanup(p.stop)

    def hold(self, idx):
        d = Path(self.tmp.name) / f"vm-{idx}"
        d.mkdir()
        (d / "meta.json").write_text(json.dumps({"index": idx}))
        return d

    def test_past_the_old_249_cap(self):
        for i in range(1, 301):
            self.hold(i)
        self.assertEqual(fc_agent.pick_index(), 301)

    def test_a_freed_slot_is_reused_lowest_first(self):
        held = [self.hold(i) for i in range(1, 6)]
        (held[2] / "meta.json").unlink()
        self.assertEqual(fc_agent.pick_index(), 3)

    def test_a_full_host_is_a_503(self):
        self.hold(1)
        self.hold(2)
        with mock.patch.object(fc_agent, "SLOT_MAX", 2):
            with self.assertRaises(fc_agent.HTTPError) as caught:
                fc_agent.pick_index()
        self.assertEqual(caught.exception.code, 503)


class SetupTeardownTest(FakeHostCase):
    def test_setup_builds_the_namespace_and_teardown_leaves_nothing(self):
        for mode in ("direct", "proxy"):
            with self.subTest(mode=mode):
                fc_agent.setup_tap(3, "eth0", mode)
                self.assertEqual(self.host.netns, {"fcns3"})
                self.assertEqual(self.host.links, {("", "fcv3"), ("fcns3", "veth0"), ("fcns3", "tap0")})
                self.assertTrue(self.mentions("fcv3"))
                fc_agent.teardown_net(3, "eth0")
                self.assertEqual(self.host.netns, set())
                self.assertEqual(self.host.links, set())
                self.assertEqual(self.mentions("fcv3"), [])
                self.assertEqual(self.host.rules.get(("fcns3", "nat")), None)

    def test_the_namespace_nats_between_the_veth_and_the_guest(self):
        fc_agent.setup_tap(3, "eth0")
        nat = self.host.rules[("fcns3", "nat")]
        self.assertIn(["PREROUTING", "-i", "veth0", "-d", "10.209.0.3", "-j", "DNAT", "--to-destination", "172.16.0.2"], nat)
        self.assertIn(["POSTROUTING", "-o", "veth0", "-s", "172.16.0.2", "-j", "SNAT", "--to-source", "10.209.0.3"], nat)

    def test_two_slots_share_the_guest_network_and_nothing_else(self):
        fc_agent.setup_tap(3, "eth0")
        fc_agent.setup_tap(4, "eth0")
        self.assertIn(("", "fcv4"), self.host.links)
        self.assertEqual(self.host.netns, {"fcns3", "fcns4"})
        fc_agent.teardown_net(3, "eth0")
        self.assertEqual(self.mentions("fcv3"), [])
        self.assertTrue(self.mentions("fcv4"))

    def test_proxy_after_direct_on_one_slot(self):
        # a direct vm that leaked its accept (#211) must not hand the next
        # vm on the slot direct egress.
        self.host.rules[("", "filter")] = [["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"]]
        fc_agent.setup_tap(3, "eth0", "proxy")
        forward = [r for r in self.host.host_rules() if r[0] == "FORWARD" and r[2] == "fcv3"]
        self.assertNotIn(["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "ACCEPT"], forward)
        drop = forward.index(["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"])
        reply = forward.index(["FORWARD", "-i", "fcv3", "-o", "eth0", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"])
        self.assertLess(reply, drop, "the reply accept must sit above the drop")

    def test_setup_over_a_stale_slot_starts_clean(self):
        fc_agent.setup_tap(3, "eth0", "proxy")
        fc_agent.setup_tap(3, "eth0", "proxy")
        self.assertEqual(self.host.netns, {"fcns3"})
        # -C before -I: a rerun adds no second copy of any host rule.
        rules = self.mentions("fcv3")
        self.assertEqual(len(rules), len({tuple(r) for r in rules}))

    def test_a_legacy_guest_network_goes_inside_the_namespace(self):
        fc_agent.setup_tap(7, "eth0", guest_net=fc_agent.legacy_guest_net(7))
        # the tap keeps its old name, but in the namespace, and the veth
        # takes that name on the host.
        self.assertIn(("fcns7", "fcv7"), self.host.links)
        self.assertEqual(self.host.peers[("", "fcv7")], ("fcns7", "veth0"))
        self.assertIn(["PREROUTING", "-i", "veth0", "-d", "10.209.0.7", "-j", "DNAT", "--to-destination", "10.200.7.2"],
                      self.host.rules[("fcns7", "nat")])


class DestroyTest(FakeHostCase):
    def setUp(self):
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        for target, value in (("VMS_DIR", Path(self.tmp.name)), ("SSH_CONTROL_DIR", Path(self.tmp.name))):
            for mod in (fc_agent, fc_agent.agentlib):
                p = mock.patch.object(mod, target, value)
                p.start()
                self.addCleanup(p.stop)
        for mod in (fc_agent, fc_agent.agentlib):
            p = mock.patch.object(mod, "host_iface", return_value="eth0")
            p.start()
            self.addCleanup(p.stop)
        p = mock.patch.object(fc_agent.subprocess, "run")
        p.start()
        self.addCleanup(p.stop)

    def test_create_then_destroy_leaves_no_rule_netns_or_veth(self):
        rootfs = Path(self.tmp.name) / "base.ext4"
        rootfs.write_bytes(b"")
        for mode in ("direct", "proxy"):
            with self.subTest(mode=mode), \
                    mock.patch.object(fc_agent, "BASE_ROOTFS", rootfs), \
                    mock.patch.object(fc_agent, "start_firecracker"), \
                    mock.patch.object(fc_agent, "stop_firecracker"), \
                    mock.patch.object(fc_agent, "wait_for_ssh", return_value=False):
                meta = fc_agent.create_vm({"name": f"vm-{mode}", "egress": {"mode": mode}})
                self.assertEqual((meta["netns"], meta["tap"]), ("fcns1", "fcv1"))
                self.assertEqual((meta["host_ip"], meta["guest_ip"]), ("10.208.0.1", "10.209.0.1"))
                self.assertEqual(meta["guest_net"], fc_agent.GUEST_NET)
                self.assertTrue([r for r in self.host.host_rules() if "10.209.0.1:9550" in r])
                fc_agent.destroy_vm(meta["vm_id"])
                self.assertEqual(self.host.netns, set())
                self.assertEqual(self.host.links, set())
                self.assertEqual(self.mentions("fcv1"), [])
                self.assertEqual([r for r in self.host.host_rules() if any("10.209.0.1" in x for x in r)], [])

    def test_a_failed_create_rolls_its_slot_back(self):
        rootfs = Path(self.tmp.name) / "base.ext4"
        rootfs.write_bytes(b"")
        with mock.patch.object(fc_agent, "BASE_ROOTFS", rootfs), \
                mock.patch.object(fc_agent, "start_firecracker", side_effect=RuntimeError("boom")), \
                mock.patch.object(fc_agent, "stop_firecracker"):
            with self.assertRaises(RuntimeError):
                fc_agent.create_vm({"name": "vm-x", "egress": {"mode": "proxy"}})
        self.assertEqual(self.host.netns, set())
        self.assertEqual(self.host.links, set())
        self.assertEqual(self.mentions("fcv1"), [])


class SweepTest(FakeHostCase):
    def test_removes_only_namespaces_no_vm_holds(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        (Path(tmp.name) / "vm-4").mkdir()
        (Path(tmp.name) / "vm-4" / "meta.json").write_text(json.dumps({"index": 4}))
        for idx in (3, 4):
            fc_agent.setup_tap(idx, "eth0")
        self.host.netns.add("fuse-warp")
        listing = b"fcns3 (id: 0)\nfcns4 (id: 1)\nfuse-warp\n"
        with mock.patch.object(fc_agent, "VMS_DIR", Path(tmp.name)), \
                mock.patch.object(fc_agent, "host_iface", return_value="eth0"), \
                mock.patch.object(fc_agent, "run", return_value=subprocess.CompletedProcess([], 0, stdout=listing)):
            fc_agent.sweep_orphan_netns()
        self.assertEqual(self.host.netns, {"fcns4", "fuse-warp"})
        self.assertEqual(self.mentions("fcv3"), [])


class RebuildTest(FakeHostCase):
    def setUp(self):
        super().setUp()
        self.calls = []
        for name in ("del_agent_forward", "add_agent_forward", "del_expose_forward", "add_expose_forward"):
            p = mock.patch.object(fc_agent, name, side_effect=lambda *a, n=name: self.calls.append((n,) + a))
            p.start()
            self.addCleanup(p.stop)
        p = mock.patch.object(fc_agent, "host_iface", return_value="eth0")
        p.start()
        self.addCleanup(p.stop)
        self.released = []
        p = mock.patch.object(fc_agent, "release_egress", side_effect=lambda m: self.released.append(m["vm_id"]))
        p.start()
        self.addCleanup(p.stop)

    def test_a_vm_from_before_namespaces_keeps_its_old_layout(self):
        # its guest, the proxy url written into it and the orchestrator all
        # know 10.200.3.x, so moving it would cut its egress silently.
        meta = {"vm_id": "old", "index": 3, "tap": "fcv3", "host_ip": "10.200.3.1", "guest_ip": "10.200.3.2",
                "mac": "06:00:AC:10:03:02", "host_port": 19553, "egress_mode": "proxy",
                "egress": {"provider": "fake"}}
        before = dict(meta)
        fc_agent.rebuild_net(meta)
        self.assertEqual(meta, before)
        self.assertEqual(self.host.netns, set())
        self.assertEqual(self.host.links, {("", "fcv3")})
        self.assertIn(["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"], self.host.host_rules())
        self.assertEqual(self.calls, [
            ("del_agent_forward", 19553, "10.200.3.2"),
            ("add_agent_forward", 19553, "10.200.3.2", "eth0"),
        ])
        self.assertEqual(self.released, [])

    def test_a_namespaced_vm_keeps_its_addresses_and_backend(self):
        meta = {"vm_id": "new", "index": 3, "tap": "fcv3", "host_ip": "10.208.0.3", "guest_ip": "10.209.0.3",
                "netns": "fcns3", "guest_net": dict(fc_agent.GUEST_NET), "egress_mode": "proxy",
                "egress": {"provider": "fake"}, "host_port": 19553}
        before = dict(meta)
        fc_agent.rebuild_net(meta)
        self.assertEqual(meta, before)
        self.assertEqual(self.host.netns, {"fcns3"})
        self.assertEqual(self.calls[-1], ("add_agent_forward", 19553, "10.209.0.3", "eth0"))
        self.assertEqual(self.released, [])

    def test_a_stale_established_accept_cannot_sit_below_the_drop(self):
        # a crash mid-teardown can leave the reply accept behind. setup must
        # purge it, or -C would skip it and the drop would land above it.
        reply = ["FORWARD", "-i", "fcv3", "-o", "eth0", "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"]
        self.host.rules[("", "filter")] = [["FORWARD", "-i", "x", "-j", "ACCEPT"], reply]
        fc_agent.setup_tap(3, "eth0", "proxy")
        forward = self.host.rules[("", "filter")]
        self.assertEqual(forward.count(reply), 1)
        self.assertLess(forward.index(reply), forward.index(["FORWARD", "-i", "fcv3", "-o", "eth0", "-j", "DROP"]))


class LaunchTest(unittest.TestCase):
    def meta(self, **extra):
        meta = {"vm_id": "vm-a", "sock": "s", "cpus": 1, "memory_mb": 512, "rootfs": "/r",
                "index": 3, "tap": "fcv3", "netns": "fcns3", "guest_net": dict(fc_agent.GUEST_NET)}
        meta.update(extra)
        return meta

    def test_firecracker_runs_inside_the_vms_namespace(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        sock = Path(tmp.name) / "vm-a.sock"
        sock.touch()
        with mock.patch.object(fc_agent, "VMS_DIR", Path(tmp.name)), \
                mock.patch.object(fc_agent.agentlib, "VMS_DIR", Path(tmp.name)), \
                mock.patch.object(fc_agent, "sock_path", return_value=sock), \
                mock.patch.object(fc_agent, "sudo"), \
                mock.patch.object(fc_agent.subprocess, "check_output", return_value=b"42\n") as spawn, \
                mock.patch.object(fc_agent.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, stdout="")):
            fc_agent.spawn_firecracker(self.meta())
        cmd = spawn.call_args[0][0][-1]
        self.assertIn("sudo -n ip netns exec fcns3 setsid ", cmd)

    def boot_bodies(self, meta):
        calls = []
        with mock.patch.object(fc_agent, "fc_api", side_effect=lambda s, m, p, b=None, **k: calls.append((p, b)) or (204, b"")), \
                mock.patch.object(fc_agent, "MEM_HOTPLUG", False):
            fc_agent.boot_firecracker(meta)
        return dict(calls)

    def test_boot_uses_the_guest_network_not_the_host_addresses(self):
        bodies = self.boot_bodies(self.meta(host_ip="10.208.0.3", guest_ip="10.209.0.3"))
        self.assertIn("ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off", bodies["/boot-source"]["boot_args"])
        self.assertEqual(bodies["/network-interfaces/eth0"]["host_dev_name"], "tap0")
        self.assertEqual(bodies["/network-interfaces/eth0"]["guest_mac"], "06:00:AC:10:00:02")

    def test_a_vm_from_before_namespaces_boots_its_old_network(self):
        meta = self.meta()
        del meta["guest_net"], meta["netns"]
        bodies = self.boot_bodies(meta)
        self.assertIn("ip=10.200.3.2::10.200.3.1:", bodies["/boot-source"]["boot_args"])
        self.assertEqual(bodies["/network-interfaces/eth0"]["host_dev_name"], "fcv3")


class VMPublicTest(unittest.TestCase):
    def test_reports_the_addresses_the_guest_sees(self):
        # the orchestrator puts these in the guest's NO_PROXY: the guest's
        # own address is 172.16.0.2, not the veth address the host uses.
        pub = fc_agent.vm_public({"vm_id": "vm-a", "host_ip": "10.208.0.3", "guest_ip": "10.209.0.3",
                                  "guest_net": dict(fc_agent.GUEST_NET)})
        self.assertEqual((pub["host_ip"], pub["guest_ip"]), ("10.208.0.3", "172.16.0.2"))


class EgressFollowsTheVethTest(unittest.TestCase):
    def test_warp_and_wireguard_attach_to_the_veth_and_its_host_address(self):
        meta = {"vm_id": "vm-a", "tap": "fcv300", "host_ip": "10.208.1.44", "guest_ip": "10.209.1.44"}
        sudos = []
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        with mock.patch.object(fc_agent, "WARP_REFS_DIR", Path(tmp.name) / "warp"), \
                mock.patch.object(fc_agent, "run", return_value=subprocess.CompletedProcess([], 0, stdout=b"", stderr=b"")), \
                mock.patch.object(fc_agent, "sudo", side_effect=lambda c, check=True: sudos.append(c)):
            ep = fc_agent.warp_provision(meta, {"protocol": "socks5", "config": {"WARP_LICENSE": "lic-0123456789"}})
        self.assertEqual(ep["url"], "socks5h://10.208.1.44:1080")
        self.assertIn(["bash", str(fc_agent.WARP_SCRIPT), "attach", "fcv300", "10.208.1.44", "1080"], sudos)


if __name__ == "__main__":
    unittest.main()
