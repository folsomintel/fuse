"""Tests for oci image conversion (POST /v1/images, GET /v1/images/{digest}).

Stdlib-only, no VMs and no network: the registry is mocked at _registry_get,
and the bake script is a stub shell script that writes (or fails to write)
IMAGES_DIR/<name>.ext4 the way fc-bake-distro-rootfs.sh does.
"""
import hashlib
import http.client
import importlib.util
import json
import os
import tempfile
import threading
import time
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
_spec.loader.exec_module(fc_agent)

AUTH = {"Authorization": "Bearer test-token"}
AMD64 = "sha256:" + "a" * 64
ARM64 = "sha256:" + "b" * 64


class ParseRefTest(unittest.TestCase):
    def test_docker_hub_normalisation(self):
        cases = {
            "ubuntu": ("registry-1.docker.io", "library/ubuntu", "latest", "docker.io/library/ubuntu"),
            "ubuntu:24.04": ("registry-1.docker.io", "library/ubuntu", "24.04", "docker.io/library/ubuntu"),
            "org/app:v1": ("registry-1.docker.io", "org/app", "v1", "docker.io/org/app"),
            "ghcr.io/org/app:v1": ("ghcr.io", "org/app", "v1", "ghcr.io/org/app"),
            "localhost:5000/app": ("localhost:5000", "app", "latest", "localhost:5000/app"),
            "fedora:40@" + AMD64: ("registry-1.docker.io", "library/fedora", AMD64, "docker.io/library/fedora"),
        }
        for ref, want in cases.items():
            self.assertEqual(fc_agent.parse_image_ref(ref), want, ref)

    def test_rejects_unsafe_refs(self):
        for ref in ("-rf", "ubuntu;id", "../etc", "", "a" * 256, 5):
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.parse_image_ref(ref)
            self.assertEqual(cm.exception.code, 400, ref)


def index_body():
    return json.dumps({"mediaType": fc_agent._INDEX_TYPES[0], "manifests": [
        {"digest": AMD64, "platform": {"os": "linux", "architecture": "amd64"}},
        {"digest": ARM64, "platform": {"os": "linux", "architecture": "arm64", "variant": "v8"}},
    ]}).encode()


class ResolveManifestTest(unittest.TestCase):
    def test_index_picks_the_host_arch(self):
        with mock.patch.object(fc_agent, "_registry_get", return_value=(200, {}, index_body())):
            self.assertEqual(fc_agent.resolve_manifest_digest("ubuntu:24.04", "arm64"),
                             ("docker.io/library/ubuntu", ARM64))
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.resolve_manifest_digest("ubuntu:24.04", "riscv64")
        self.assertEqual(cm.exception.code, 400)

    def test_anonymous_token_flow(self):
        calls = []
        challenge = 'Bearer realm="https://auth.example/token",service="reg",scope="repository:library/ubuntu:pull"'

        def get(url, accept, token=""):
            calls.append((url, token))
            if url.startswith("https://auth.example/token"):
                return 200, {}, b'{"token":"t0k"}'
            if not token:
                return 401, {"www-authenticate": challenge}, b""
            return 200, {}, index_body()

        with mock.patch.object(fc_agent, "_registry_get", side_effect=get):
            _, digest = fc_agent.resolve_manifest_digest("ubuntu", "amd64")
        self.assertEqual(digest, AMD64)
        self.assertIn("scope=repository%3Alibrary%2Fubuntu%3Apull", calls[1][0])
        self.assertEqual(calls[2][1], "t0k")

    def test_registry_get_keys_headers_in_lowercase(self):
        # a registry or proxy may send any case; the challenge must still be
        # found, or a public image reads as a private one.
        msg = http.client.HTTPMessage()
        msg["www-authenticate"] = 'Bearer realm="https://auth.example/token"'
        msg["Docker-Content-Digest"] = AMD64
        resp = mock.MagicMock(status=200, headers=msg)
        resp.read.return_value = b"{}"
        resp.__enter__.return_value = resp
        with mock.patch.object(fc_agent._REGISTRY_OPENER, "open", return_value=resp):
            code, headers, _ = fc_agent._registry_get("https://registry.example/v2/", "application/json")
        self.assertEqual(code, 200)
        self.assertEqual(headers["www-authenticate"], 'Bearer realm="https://auth.example/token"')
        self.assertEqual(headers["docker-content-digest"], AMD64)

    def test_token_realm_with_a_query(self):
        calls = []

        def get(url, accept, token=""):
            calls.append(url)
            return 200, {}, b'{"access_token":"t0k"}'

        with mock.patch.object(fc_agent, "_registry_get", side_effect=get):
            token = fc_agent._registry_token('Bearer realm="https://auth.example/token?a=1",service="reg"')
        self.assertEqual(token, "t0k")
        self.assertEqual(calls[0], "https://auth.example/token?a=1&service=reg")

    def test_private_and_missing_are_4xx(self):
        with mock.patch.object(fc_agent, "_registry_get", return_value=(401, {"www-authenticate": "Basic"}, b"")):
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.resolve_manifest_digest("ghcr.io/org/secret", "amd64")
        self.assertEqual(cm.exception.code, 400)
        with mock.patch.object(fc_agent, "_registry_get", return_value=(404, {}, b"")):
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.resolve_manifest_digest("ubuntu:nope", "amd64")
        self.assertEqual(cm.exception.code, 404)

    def test_single_manifest_checks_the_config_arch(self):
        manifest = json.dumps({"config": {"digest": "sha256:" + "c" * 64}}).encode()

        def get(url, accept, token=""):
            if "/blobs/" in url:
                return 200, {}, b'{"architecture":"arm64"}'
            return 200, {"docker-content-digest": AMD64}, manifest

        with mock.patch.object(fc_agent, "_registry_get", side_effect=get):
            self.assertEqual(fc_agent.resolve_manifest_digest("org/app", "arm64")[1], AMD64)
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.resolve_manifest_digest("org/app", "amd64")
        self.assertEqual(cm.exception.code, 400)


class RegistryDestinationTest(unittest.TestCase):
    """The registry, its redirects and its token realm all come from the
    caller's ref, so none of them may reach a non-public address."""

    def test_refuses_non_public_addresses(self):
        for ip in ("127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "100.64.0.1",
                   "169.254.169.254", "0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1",
                   "224.0.0.1"):
            fam = fc_agent.socket.AF_INET6 if ":" in ip else fc_agent.socket.AF_INET
            info = [(fam, fc_agent.socket.SOCK_STREAM, 6, "", (ip, 443))]
            with mock.patch.object(fc_agent.socket, "getaddrinfo", return_value=info), \
                    mock.patch.object(fc_agent.socket, "socket") as sock:
                with self.assertRaises(fc_agent.HTTPError, msg=ip) as cm:
                    fc_agent._public_create_connection(("registry.example", 443), 5)
            self.assertEqual(cm.exception.code, 400, ip)
            sock.assert_not_called()

    def test_one_private_answer_refuses_the_name(self):
        info = [(fc_agent.socket.AF_INET, fc_agent.socket.SOCK_STREAM, 6, "", ("8.8.8.8", 443)),
                (fc_agent.socket.AF_INET, fc_agent.socket.SOCK_STREAM, 6, "", ("10.1.2.3", 443))]
        with mock.patch.object(fc_agent.socket, "getaddrinfo", return_value=info), \
                mock.patch.object(fc_agent.socket, "socket") as sock:
            with self.assertRaises(fc_agent.HTTPError):
                fc_agent._public_create_connection(("registry.example", 443), 5)
        sock.assert_not_called()

    def test_connects_to_the_checked_address(self):
        info = [(fc_agent.socket.AF_INET, fc_agent.socket.SOCK_STREAM, 6, "", ("8.8.8.8", 443))]
        with mock.patch.object(fc_agent.socket, "getaddrinfo", return_value=info), \
                mock.patch.object(fc_agent.socket, "socket") as sock:
            fc_agent._public_create_connection(("registry.example", 443), 5)
        sock.return_value.connect.assert_called_once_with(("8.8.8.8", 443))

    def test_get_refuses_loopback_and_plain_http(self):
        # the token realm goes through _registry_get too, so this covers it.
        for url in ("https://127.0.0.1/token", "https://[::1]:5000/v2/", "http://registry.example/v2/"):
            with self.assertRaises(fc_agent.HTTPError, msg=url) as cm:
                fc_agent._registry_get(url, "application/json")
            self.assertEqual(cm.exception.code, 400, url)

    def test_redirects_only_to_https(self):
        handler = fc_agent._HTTPSOnlyRedirect()
        req = fc_agent.urllib.request.Request("https://registry.example/v2/x/blobs/y")
        with self.assertRaises(fc_agent.HTTPError) as cm:
            handler.redirect_request(req, None, 302, "Found", {}, "http://169.254.169.254/latest/")
        self.assertEqual(cm.exception.code, 400)
        self.assertIsNotNone(handler.redirect_request(req, None, 307, "Temporary", {}, "https://cdn.example/blob"))

    def test_the_opener_uses_the_checked_connection(self):
        # every https connection, the first and each redirect, goes through
        # the address check.
        handlers = [h for h in fc_agent._REGISTRY_OPENER.handlers
                    if isinstance(h, fc_agent.urllib.request.HTTPSHandler)]
        self.assertEqual([type(h) for h in handlers], [fc_agent._PublicHTTPSHandler])
        conn = fc_agent._PublicHTTPSConnection("registry.example")
        self.assertIs(conn._create_connection, fc_agent._public_create_connection)


class ResolveImagePathTest(unittest.TestCase):
    def test_stays_under_images_dir(self):
        for bad in ("../../etc/shadow", "/etc/shadow", "~root", "a\x00b", ""):
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.resolve_image_path(bad)
            self.assertEqual(cm.exception.code, 400, bad)
        self.assertEqual(fc_agent.resolve_image_path("oci-abc").name, "oci-abc.ext4")


BAKE_OK = """#!/bin/sh
set -e
echo "$1" > "$IMAGES_DIR/$2.ext4.src"
printf 'rootfs' > "$IMAGES_DIR/$2.ext4"
"""
BAKE_FAIL = """#!/bin/sh
echo "pulling $1" >&2
echo "fc-bake-distro-rootfs: unsupported distro (no apt-get or dnf in the image)" >&2
echo "cleanup" >&2
exit 1
"""
BAKE_HANG = """#!/bin/sh
printf 'part' > "$IMAGES_DIR/.$2.ext4.tmp"
sleep 30
"""


class ConvertBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        (root / "images").mkdir()
        (root / "fused").write_bytes(b"fused-binary")
        self.script = root / "bake.sh"
        self.patches = [
            mock.patch.object(fc_agent, "IMAGES_DIR", root / "images"),
            mock.patch.object(fc_agent, "IMAGE_STATE_DIR", root / "state" / "images"),
            mock.patch.object(fc_agent, "FUSED_BIN", root / "fused"),
            mock.patch.object(fc_agent, "DISTRO_BAKE_SCRIPT", self.script),
            mock.patch.object(fc_agent, "goarch", return_value="amd64"),
            mock.patch.object(fc_agent, "resolve_manifest_digest",
                              return_value=("docker.io/library/ubuntu", AMD64)),
        ]
        for p in self.patches:
            p.start()
        self.bake(BAKE_OK)

    def tearDown(self):
        for p in reversed(self.patches):
            p.stop()
        self.tmp.cleanup()

    def bake(self, body):
        self.script.write_text(body)
        self.script.chmod(0o755)

    def wait(self, digest, timeout=10.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            rec = fc_agent.image_record_load(digest)
            if rec and rec["state"] != "converting":
                return rec
            time.sleep(0.02)
        self.fail("conversion did not finish")


class ConvertTest(ConvertBase):
    def test_converts_from_the_pinned_digest(self):
        rec = fc_agent.image_convert_start("ubuntu:24.04")
        want = hashlib.sha256(b"fuse-oci-image/v1\n" + AMD64.encode() + b"\n"
                              + hashlib.sha256(b"fused-binary").hexdigest().encode() + b"\n").hexdigest()
        self.assertEqual(rec["digest"], want)
        self.assertEqual(rec["state"], "converting")
        self.assertEqual(rec["name"], "oci-" + want[:32])
        done = self.wait(rec["digest"])
        self.assertEqual(done["state"], "ready", done)
        self.assertEqual(done["size_bytes"], len("rootfs"))
        src = (fc_agent.IMAGES_DIR / f"{rec['name']}.ext4.src").read_text().strip()
        self.assertEqual(src, "docker.io/library/ubuntu@" + AMD64)

        # a repeat answers with the ready record and bakes nothing.
        self.bake(BAKE_FAIL)
        again = fc_agent.image_convert_start("ubuntu:24.04")
        self.assertEqual(again["state"], "ready")

    def test_a_new_fused_is_a_new_image(self):
        first = fc_agent.image_convert_start("ubuntu:24.04")["digest"]
        self.wait(first)
        fc_agent.FUSED_BIN.write_bytes(b"fused-binary-v2")
        self.assertNotEqual(fc_agent.image_convert_start("ubuntu:24.04")["digest"], first)

    def test_failure_surfaces_the_script_reason_and_retries(self):
        self.bake(BAKE_FAIL)
        rec = self.wait(fc_agent.image_convert_start("busybox")["digest"])
        self.assertEqual(rec["state"], "failed")
        self.assertEqual(rec["error"], "fc-bake-distro-rootfs: unsupported distro (no apt-get or dnf in the image)")
        self.bake(BAKE_OK)
        retry = fc_agent.image_convert_start("busybox")
        self.assertEqual(retry["state"], "converting")
        self.assertEqual(self.wait(retry["digest"])["state"], "ready")

    def test_timeout_kills_the_bake(self):
        self.bake(BAKE_HANG)
        with mock.patch.object(fc_agent, "IMAGE_CONVERT_TIMEOUT", 0.3):
            rec = self.wait(fc_agent.image_convert_start("ubuntu")["digest"])
        self.assertEqual(rec["state"], "failed")
        self.assertIn("timed out", rec["error"])
        # the kill skips the script's trap, so the agent removes the partial.
        self.assertFalse((fc_agent.IMAGES_DIR / f".{rec['name']}.ext4.tmp").exists())

    def test_a_second_convert_while_converting_does_not_bake_twice(self):
        self.bake(BAKE_HANG)
        started = []
        real = threading.Thread

        def thread(*a, **kw):
            started.append(kw.get("target"))
            return real(*a, **kw)

        with mock.patch.object(fc_agent, "IMAGE_CONVERT_TIMEOUT", 0.5), \
                mock.patch.object(fc_agent.threading, "Thread", side_effect=thread):
            first = fc_agent.image_convert_start("ubuntu")
            second = fc_agent.image_convert_start("ubuntu")
            self.assertEqual(first["digest"], second["digest"])
            self.assertEqual(len(started), 1)
            self.wait(first["digest"])

    def test_sweep_fails_an_interrupted_conversion(self):
        rec = {"digest": "e" * 64, "ref": "ubuntu", "manifest_digest": AMD64, "name": "oci-e", "state": "converting"}
        fc_agent.image_record_save(rec)
        fc_agent.image_sweep_interrupted()
        swept = fc_agent.image_record_load("e" * 64)
        self.assertEqual(swept["state"], "failed")
        self.assertIn("restarted", swept["error"])

    def test_missing_fused_is_503(self):
        fc_agent.FUSED_BIN.unlink()
        with self.assertRaises(fc_agent.HTTPError) as cm:
            fc_agent.image_convert_start("ubuntu")
        self.assertEqual(cm.exception.code, 503)


BAKE_64K = """#!/bin/sh
set -e
head -c 65536 /dev/urandom > "$IMAGES_DIR/$2.ext4"
"""
KIB = 1024


class EvictionTest(ConvertBase):
    """lru eviction under FC_IMAGE_CACHE_GB. each bake allocates 64K and the
    estimate (FC_ROOTFS_SIZE) is 128K, so a 200K cap holds two ready images
    and has to evict one before a third convert starts."""

    def setUp(self):
        super().setUp()
        root = Path(self.tmp.name)
        (root / "vms").mkdir()
        (root / "snapshots").mkdir()
        self.bake(BAKE_64K)
        for p in (
            mock.patch.object(fc_agent, "VMS_DIR", root / "vms"),
            # list_vms lives in agentlib and reads its own copy.
            mock.patch.object(fc_agent.agentlib, "VMS_DIR", root / "vms"),
            mock.patch.object(fc_agent, "SNAPSHOTS_DIR", root / "snapshots"),
            mock.patch.object(fc_agent, "IMAGE_CACHE_BYTES", 200 * KIB),
            mock.patch.dict(os.environ, {"FC_ROOTFS_SIZE": "128K"}),
            # a distinct manifest per ref, so each ref is its own image.
            mock.patch.object(fc_agent, "resolve_manifest_digest", side_effect=lambda ref, arch: (
                "docker.io/library/" + ref, "sha256:" + hashlib.sha256(ref.encode()).hexdigest())),
        ):
            p.start()
            self.patches.append(p)
        # a baked catalogue image: no record, so never counted or evicted.
        (fc_agent.IMAGES_DIR / "base.ext4").write_bytes(os.urandom(256 * KIB))

    def convert(self, ref):
        rec = self.wait(fc_agent.image_convert_start(ref)["digest"])
        self.assertEqual(rec["state"], "ready", rec)
        return rec

    def test_estimate_parses_the_bake_size(self):
        self.assertEqual(fc_agent.image_convert_estimate(), 128 * KIB)
        self.assertEqual(fc_agent.parse_size("4G"), 4 << 30)
        # every unit truncate takes, so a valid size never slips under the cap.
        self.assertEqual(fc_agent.parse_size("8GiB"), 8 << 30)
        self.assertEqual(fc_agent.parse_size("8GB"), 8 * 10**9)
        self.assertEqual(fc_agent.parse_size("2t"), 2 << 40)
        self.assertEqual(fc_agent.parse_size("4096"), 4096)
        with mock.patch.dict(os.environ, {"FC_ROOTFS_SIZE": ""}):
            self.assertEqual(fc_agent.image_convert_estimate(), 4 << 30)

    def test_an_unreadable_bake_size_refuses_the_convert(self):
        with mock.patch.dict(os.environ, {"FC_ROOTFS_SIZE": "lots"}):
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.image_convert_start("alpine")
        self.assertEqual(cm.exception.code, 503)
        self.assertEqual(fc_agent.image_records(), [])

    def test_evicts_the_least_recently_used(self):
        a = self.convert("alpine")
        b = self.convert("busybox")
        # a create from a makes b the least recently used.
        fc_agent.image_touch(a["name"])
        c = self.convert("caddy")
        self.assertIsNone(fc_agent.image_record_load(b["digest"]))
        self.assertFalse(fc_agent.resolve_image_path(b["name"]).exists())
        for kept in (a, c):
            self.assertEqual(fc_agent.image_record_load(kept["digest"])["state"], "ready")
            self.assertTrue(fc_agent.resolve_image_path(kept["name"]).exists())
        self.assertTrue((fc_agent.IMAGES_DIR / "base.ext4").exists())
        # converting b again bakes it back and evicts a, now the oldest.
        self.convert("busybox")
        self.assertIsNone(fc_agent.image_record_load(a["digest"]))

    def test_referenced_images_are_kept_and_full_is_507(self):
        a = self.convert("alpine")
        b = self.convert("busybox")
        vm = fc_agent.VMS_DIR / "vm-1"
        vm.mkdir()
        (vm / "meta.json").write_text(json.dumps({"vm_id": "vm-1", "name": "vm-1", "image": a["name"]}))
        snap = fc_agent.SNAPSHOTS_DIR / "snap-1"
        snap.mkdir()
        (snap / "meta.json").write_text(json.dumps({"snapshot_id": "snap-1", "image": b["name"]}))
        with self.assertRaises(fc_agent.HTTPError) as cm:
            fc_agent.image_convert_start("caddy")
        self.assertEqual(cm.exception.code, 507)
        self.assertIn("FC_IMAGE_CACHE_GB", cm.exception.msg)
        for kept in (a, b):
            self.assertEqual(fc_agent.image_record_load(kept["digest"])["state"], "ready")
        # once the snapshot is gone, b is evictable again.
        (snap / "meta.json").unlink()
        self.convert("caddy")
        self.assertIsNone(fc_agent.image_record_load(b["digest"]))
        self.assertIsNotNone(fc_agent.image_record_load(a["digest"]))

    def test_a_converting_image_counts_and_is_never_evicted(self):
        self.bake(BAKE_HANG)
        with mock.patch.object(fc_agent, "IMAGE_CONVERT_TIMEOUT", 0.5):
            first = fc_agent.image_convert_start("alpine")
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.image_convert_start("busybox")
            self.assertEqual(cm.exception.code, 507)
            self.assertEqual(fc_agent.image_record_load(first["digest"])["state"], "converting")
            # a repeat of the converting image is not a new conversion.
            self.assertEqual(fc_agent.image_convert_start("alpine")["digest"], first["digest"])
            self.wait(first["digest"])

    def test_create_records_and_touches_the_image(self):
        a = self.convert("alpine")
        before = fc_agent.image_record_load(a["digest"])["last_used"]
        snap = fc_agent.SNAPSHOTS_DIR / "snap-3"
        snap.mkdir()
        (snap / "rootfs.ext4").write_bytes(b"rootfs")
        (snap / "meta.json").write_text(json.dumps({"snapshot_id": "snap-3", "image": a["name"]}))
        with mock.patch.object(fc_agent, "pick_index", side_effect=[1, 2]), \
                mock.patch.object(fc_agent, "host_iface", return_value="eth0"), \
                mock.patch.object(fc_agent, "setup_tap", return_value=("tap1", "10.0.0.1", "10.0.0.2")), \
                mock.patch.object(fc_agent, "add_agent_forward"), \
                mock.patch.object(fc_agent, "sudo"), \
                mock.patch.object(fc_agent, "start_firecracker"), \
                mock.patch.object(fc_agent, "wait_for_ssh", return_value=False):
            vm = fc_agent.create_vm({"name": "from-image", "image": a["name"]})
            # a fork or seed from a snapshot inherits the image it records.
            forked = fc_agent.create_vm({"name": "from-snap"}, source_rootfs=snap / "rootfs.ext4")
        self.assertEqual(vm["image"], a["name"])
        self.assertEqual(forked["image"], a["name"])
        self.assertGreater(fc_agent.image_record_load(a["digest"])["last_used"], before)
        self.assertEqual(fc_agent.image_references(), {a["name"]})

    def test_create_from_an_evicted_image_is_404(self):
        a = self.convert("alpine")
        fc_agent.image_evict(fc_agent.image_record_load(a["digest"]))
        with self.assertRaises(fc_agent.HTTPError) as cm:
            fc_agent.create_vm({"name": "late", "image": a["name"]})
        self.assertEqual(cm.exception.code, 404)
        self.assertFalse(fc_agent.vm_dir("late").exists())
        # a baked catalogue name that is missing is still the operator's 400.
        with self.assertRaises(fc_agent.HTTPError) as cm:
            fc_agent.create_vm({"name": "late", "image": "nope"})
        self.assertEqual(cm.exception.code, 400)

    def test_snapshot_base_image(self):
        snap = fc_agent.SNAPSHOTS_DIR / "snap-2"
        snap.mkdir()
        self.assertEqual(fc_agent.snapshot_base_image(snap / "rootfs.ext4"), "")
        (snap / "meta.json").write_text(json.dumps({"image": "oci-abc"}))
        self.assertEqual(fc_agent.snapshot_base_image(snap / "rootfs.ext4"), "oci-abc")


class RoutesTest(ConvertBase):
    """The same store reached over http, the way the orchestrator does."""

    def setUp(self):
        super().setUp()
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()
        super().tearDown()

    def call(self, method, path, body=None):
        headers = dict(AUTH)
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

    def test_post_then_get(self):
        code, body = self.call("POST", "/v1/images", {"ref": "ubuntu:24.04"})
        self.assertEqual(code, 202, body)
        rec = json.loads(body)
        self.assertEqual(set(rec), {"digest", "ref", "manifest_digest", "name", "state", "error", "created_at"})
        self.assertTrue(rec["created_at"])
        self.wait(rec["digest"])
        code, body = self.call("GET", f"/v1/images/{rec['digest']}")
        self.assertEqual(code, 200)
        self.assertEqual(json.loads(body)["state"], "ready")

    def test_get_unknown_and_malformed(self):
        self.assertEqual(self.call("GET", "/v1/images/" + "f" * 64)[0], 404)
        self.assertEqual(self.call("GET", "/v1/images/..%2Fetc")[0], 400)

    def test_bad_ref_is_400(self):
        for ref in (7, "-rf", "ubuntu;id"):
            self.assertEqual(self.call("POST", "/v1/images", {"ref": ref})[0], 400, ref)
        self.assertEqual(self.call("POST", "/v1/images", {})[0], 400)

if __name__ == "__main__":
    unittest.main()
