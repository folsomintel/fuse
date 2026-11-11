"""tests for the agent's http/1.1 keep-alive and the artifact lookup cache.

under keep-alive every response has to say where it ends, and a request body
the handler did not read must end the connection rather than be parsed as the
next request. artifact_file caches where it found a digest, and a cached
entry is never served once the store no longer backs it.
stdlib-only, no VMs.
"""
import hashlib
import http.client
import importlib.util
import json
import os
import socket
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


def load(name: str, filename: str):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).parent.parent / filename)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


fc_agent = load("fc_agent", "fc-agent.py")
fc_agent.sudo = lambda cmd, check=True: fc_agent.run(cmd, check=check)

PAGE = 4096
fc_agent.HUGE_PAGE_BYTES = PAGE
AUTH = {"Authorization": "Bearer test-token"}


def grant_for(digest: str) -> str:
    expiry, nonce = str(int(time.time()) + 60), "ab" * 16
    return f"v1.{digest}.{expiry}.{nonce}.{fc_agent.artifact_grant_mac(digest, expiry, nonce)}"


def write_snapshot(snapshot_id: str, mem: bytes) -> str:
    d = fc_agent.SNAPSHOTS_DIR / snapshot_id
    d.mkdir(parents=True)
    (d / "rootfs.ext4").write_bytes(f"rootfs {snapshot_id}".encode())
    (d / "mem").write_bytes(mem)
    digest = fc_agent.file_digest(d / "rootfs.ext4")
    (d / "meta.json").write_text(json.dumps({"snapshot_id": snapshot_id, "digest": digest, "kind": "live"}))
    return digest


class AgentServer:
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.port = cls.server.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")
        fc_agent.forget_artifacts()

    def conn(self) -> http.client.HTTPConnection:
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        self.addCleanup(c.close)
        return c

    def call(self, c, method: str, path: str, body: bytes | None = None, headers: dict | None = None):
        c.request(method, path, body=body, headers=headers or {})
        resp = c.getresponse()
        return resp, resp.read()

    def open_sock(self, c: http.client.HTTPConnection) -> socket.socket:
        """the connection's socket, which http.client drops once a response
        says the connection ends."""
        self.assertIsNotNone(c.sock, "the server ended the connection")
        return c.sock

    def raw(self, request: bytes) -> bytes:
        """send requests on a raw socket and read until the server closes or
        goes quiet. self.closed says which."""
        s = socket.create_connection(("127.0.0.1", self.port), timeout=2)
        self.addCleanup(s.close)
        s.sendall(request)
        out, self.closed = b"", False
        try:
            while True:
                b = s.recv(65536)
                if not b:
                    self.closed = True
                    break
                out += b
        except socket.timeout:
            pass
        return out


class KeepAliveTest(AgentServer, unittest.TestCase):
    def test_serves_many_requests_on_one_connection(self):
        c = self.conn()
        resp, _ = self.call(c, "GET", "/healthz", headers=AUTH)
        sock = self.open_sock(c)
        for path in ("/healthz", "/nowhere", "/v1/capacity"):
            resp, _ = self.call(c, "GET", path, headers=AUTH)
            self.assertNotEqual(resp.getheader("Connection"), "close")
            self.assertIs(c.sock, sock, path)

    def test_every_response_has_a_length(self):
        c = self.conn()
        for method, path, headers in [
            ("GET", "/healthz", AUTH),
            ("GET", "/nowhere", AUTH),
            ("GET", "/v1/vm", {}),
            ("GET", "/v1/vm/vm-nobody", AUTH),
            ("DELETE", "/v1/snapshots/snap-nobody", AUTH),
            ("GET", "/v1/artifacts/" + "0" * 64, {}),
        ]:
            resp, body = self.call(c, method, path, headers=headers)
            self.assertEqual(resp.getheader("Content-Length"), str(len(body)), path)

    def test_a_204_has_no_body_and_keeps_the_connection(self):
        write_snapshot(f"snap-{self.name}", b"m" * PAGE)
        c = self.conn()
        resp, body = self.call(c, "DELETE", f"/v1/snapshots/snap-{self.name}", headers=AUTH)
        self.assertEqual(resp.status, 204)
        self.assertIsNone(resp.getheader("Content-Length"))
        self.assertEqual(body, b"")
        sock = self.open_sock(c)
        resp, _ = self.call(c, "GET", "/healthz", headers=AUTH)
        self.assertEqual(resp.status, 200)
        self.assertIs(c.sock, sock)

    def test_a_raw_204_ends_at_its_headers(self):
        write_snapshot(f"snap-{self.name}", b"m" * PAGE)
        out = self.raw(
            f"DELETE /v1/snapshots/snap-{self.name} HTTP/1.1\r\nHost: x\r\n"
            f"Authorization: Bearer test-token\r\n\r\n"
            "GET /healthz HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer test-token\r\nConnection: close\r\n\r\n".encode()
        )
        # the second response follows the first directly, so nothing of a body
        # sat between them.
        self.assertTrue(out.startswith(b"HTTP/1.1 204"), out)
        self.assertIn(b"\r\n\r\nHTTP/1.1 200", out)
        self.assertTrue(self.closed)

    def test_an_unread_body_closes_the_connection(self):
        body = json.dumps({"cmd": ["true"]}).encode()
        # unauthorized: the body is never read.
        out = self.raw(
            b"POST /v1/vm/vm-x/exec HTTP/1.1\r\nHost: x\r\nContent-Length: " + str(len(body)).encode()
            + b"\r\n\r\n" + body
        )
        self.assertTrue(out.startswith(b"HTTP/1.1 401"), out)
        self.assertIn(b"Connection: close", out)
        self.assertTrue(self.closed)
        # the body was never answered as a request of its own.
        self.assertEqual(out.count(b"HTTP/1.1 "), 1)

    def test_a_body_over_the_limit_closes_the_connection(self):
        # only the headers go out: the 413 must come back, and the connection
        # end, without the server waiting on a body it will not read.
        out = self.raw(
            f"POST /v1/vm HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer test-token\r\n"
            f"Content-Length: {fc_agent.MAX_BODY_BYTES + 1}\r\n\r\n".encode()
        )
        self.assertTrue(out.startswith(b"HTTP/1.1 413"), out)
        self.assertIn(b"Connection: close", out)
        self.assertTrue(self.closed)

    def test_a_read_body_keeps_the_connection(self):
        c = self.conn()
        resp, _ = self.call(c, "POST", "/v1/artifacts/x/pull", body=b"{}", headers=AUTH)
        # a missing field, but the body was read, so the connection lives.
        self.assertEqual(resp.status, 400)
        self.assertNotEqual(resp.getheader("Connection"), "close")
        sock = self.open_sock(c)
        self.call(c, "GET", "/healthz", headers=AUTH)
        self.assertIs(c.sock, sock)

    def test_serves_every_chunk_of_mem_on_one_connection(self):
        mem = b"a" * PAGE + b"b" * PAGE + b"c" * PAGE
        digest = write_snapshot(f"snap-{self.name}", mem)
        c = self.conn()
        sock = None
        for i in (2, 0, 1):
            resp, body = self.call(c, "GET", f"/v1/artifacts/{digest}/files/mem?snapshot=snap-{self.name}",
                                   headers={fc_agent.ARTIFACT_GRANT_HEADER: grant_for(digest),
                                            "Range": f"bytes={i * PAGE}-{(i + 1) * PAGE - 1}"})
            self.assertEqual(resp.status, 206)
            self.assertEqual(body, mem[i * PAGE:(i + 1) * PAGE])
            sock = sock or self.open_sock(c)
            self.assertIs(c.sock, sock)

    def test_a_short_artifact_body_closes_the_connection(self):
        digest = write_snapshot(f"snap-{self.name}", b"m" * PAGE)
        # the deadline has already passed, so nothing of the body is sent.
        with mock.patch.object(fc_agent, "ARTIFACT_TRANSFER_TIMEOUT", -1):
            out = self.raw(
                f"GET /v1/artifacts/{digest}/files/mem HTTP/1.1\r\nHost: x\r\n"
                f"{fc_agent.ARTIFACT_GRANT_HEADER}: {grant_for(digest)}\r\n\r\n".encode()
            )
        head, _, rest = out.partition(b"\r\n\r\n")
        self.assertIn(f"Content-Length: {PAGE}".encode(), head)
        # closed at the short body, rather than left open for the peer to
        # wait on bytes that never come.
        self.assertLess(len(rest), PAGE)
        self.assertTrue(self.closed)


class TimeoutLogTest(unittest.TestCase):
    """the request timeout closes idle keep-alive connections all the time;
    only a timeout partway through a request is logged."""

    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        patcher = mock.patch.object(fc_agent.Handler, "timeout", 0.2)
        patcher.start()
        self.addCleanup(patcher.stop)

    def logged_after(self, data: bytes) -> list[str]:
        lines = []
        with mock.patch.object(fc_agent.Handler, "log_message",
                               lambda h, fmt, *args: lines.append(fmt % args)):
            s = socket.create_connection(self.server.server_address, timeout=5)
            try:
                s.sendall(data)
                # the server closes once its timeout fires.
                while s.recv(65536):
                    pass
            finally:
                s.close()
        return [line for line in lines if "timed out" in line]

    def test_an_idle_connection_closes_quietly(self):
        out = self.logged_after(b"GET /healthz HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer test-token\r\n\r\n")
        self.assertEqual(out, [])

    def test_a_stall_partway_through_the_headers_is_logged(self):
        out = self.logged_after(b"GET /healthz HTTP/1.1\r\nHost: x\r\n")
        self.assertEqual(len(out), 1, out)


class ArtifactCacheTest(unittest.TestCase):
    def setUp(self):
        self.name = self.id().rsplit(".", 1)[1].replace("_", "-")
        fc_agent.forget_artifacts()

    def test_a_second_lookup_does_not_list_the_store(self):
        digest = write_snapshot(f"snap-{self.name}", b"m" * PAGE)
        first = fc_agent.artifact_file(digest, "mem")
        with mock.patch.object(fc_agent.os, "listdir", side_effect=AssertionError("store listed")):
            self.assertEqual(fc_agent.artifact_file(digest, "mem"), first)

    def test_a_deleted_entry_is_never_served(self):
        snap = f"snap-{self.name}"
        digest = write_snapshot(snap, b"m" * PAGE)
        fc_agent.artifact_file(digest, "mem", snap)
        fc_agent.snapshot_delete(snap)
        self.assertEqual(fc_agent._artifact_paths, {})
        with self.assertRaises(fc_agent.HTTPError) as e:
            fc_agent.artifact_file(digest, "mem", snap)
        self.assertEqual(e.exception.code, 404)

    def test_an_entry_removed_behind_the_cache_is_never_served(self):
        snap = f"snap-{self.name}"
        digest = write_snapshot(snap, b"m" * PAGE)
        fc_agent.artifact_file(digest, "mem", snap)
        # no forget_artifacts: an operator's rm, say.
        fc_agent.run(["rm", "-rf", str(fc_agent.SNAPSHOTS_DIR / snap)])
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.artifact_file(digest, "mem", snap)

    def test_a_rewritten_entry_falls_back_to_the_scan(self):
        a, b = f"a-{self.name}", f"b-{self.name}"
        digest = write_snapshot(a, b"m" * PAGE)
        self.assertEqual(fc_agent.artifact_file(digest, "mem")[0].parent.name, a)
        # b holds the same artifact; a's record is rewritten to another one.
        d = fc_agent.SNAPSHOTS_DIR / b
        d.mkdir()
        (d / "mem").write_bytes(b"m" * PAGE)
        (d / "meta.json").write_text(json.dumps({"snapshot_id": b, "digest": digest}))
        (fc_agent.SNAPSHOTS_DIR / a / "meta.json").write_text(json.dumps({"digest": "f" * 64}))
        self.assertEqual(fc_agent.artifact_file(digest, "mem")[0].parent.name, b)

    def test_a_cached_entry_that_now_escapes_the_store_is_refused(self):
        snap = f"snap-{self.name}"
        digest = write_snapshot(snap, b"m" * PAGE)
        fc_agent.artifact_file(digest, "mem", snap)
        outside = Path(tempfile.mkdtemp()) / "mem"
        outside.write_bytes(b"secret")
        mem = fc_agent.SNAPSHOTS_DIR / snap / "mem"
        mem.unlink()
        mem.symlink_to(outside)
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.artifact_file(digest, "mem", snap)

    def test_the_cache_is_per_snapshot_and_per_file(self):
        snap = f"snap-{self.name}"
        digest = write_snapshot(snap, b"m" * PAGE)
        rootfs, _ = fc_agent.artifact_file(digest, "rootfs.ext4", snap)
        mem, _ = fc_agent.artifact_file(digest, "mem", snap)
        self.assertEqual((rootfs.name, mem.name), ("rootfs.ext4", "mem"))
        with self.assertRaises(fc_agent.HTTPError):
            fc_agent.artifact_file(digest, "mem", "snap-other")


class ArtifactCachePullTest(AgentServer, unittest.TestCase):
    def test_a_pulled_artifact_drops_the_cache(self):
        digest = write_snapshot(f"src-{self.name}", b"m" * PAGE)
        fc_agent.artifact_file(digest)
        self.assertTrue(fc_agent._artifact_paths)
        fc_agent.pull_artifact(digest, f"http://127.0.0.1:{self.port}", grant_for(digest), f"dst-{self.name}")
        self.assertEqual(fc_agent._artifact_paths, {})

if __name__ == "__main__":
    unittest.main()
