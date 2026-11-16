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
        with mock.patch.object(fc_agent.urllib.request, "urlopen", return_value=resp):
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
        for p in self.patches:
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
        self.assertEqual(set(rec), {"digest", "ref", "manifest_digest", "name", "state", "error"})
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
