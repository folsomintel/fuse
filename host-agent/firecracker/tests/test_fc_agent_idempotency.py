"""a mutating request carrying Idempotency-Key runs once per key: a repeat,
a concurrent repeat and a retry after a dropped connection all get the first
answer, and an expired key runs again. stdlib-only, no VMs.
"""
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

_spec = importlib.util.spec_from_file_location("fc_agent", Path(__file__).parent.parent / "fc-agent.py")
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)

AUTH = {"Authorization": "Bearer test-token"}
DIGEST = "a" * 64


class IdempotencyTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state = mock.patch.object(fc_agent, "STATE_DIR", Path(self.tmp.name))
        self.state.start()
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()
        self.state.stop()
        self.tmp.cleanup()

    def call(self, method, path, body=None, key=None):
        headers = dict(AUTH)
        if key:
            headers["Idempotency-Key"] = key
        data = json.dumps(body).encode() if body is not None else None
        if data is not None:
            headers["Content-Type"] = "application/json"
        c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=10)
        try:
            c.request(method, path, body=data, headers=headers)
            r = c.getresponse()
            return r.status, r.read(), r.getheader("Idempotent-Replayed")
        finally:
            c.close()

    def fake_create(self):
        calls = []

        def create(req, source_rootfs=None):
            calls.append(req)
            return {"vm_id": req["name"], "name": req["name"], "url": f"http://h/{len(calls)}"}

        return calls, mock.patch.object(fc_agent, "create_vm", side_effect=create)

    def test_repeat_create_makes_one_vm(self):
        calls, p = self.fake_create()
        with p:
            first = self.call("POST", "/v1/vm", {"name": "a"}, key="inv_1:create.boot/1")
            second = self.call("POST", "/v1/vm", {"name": "a"}, key="inv_1:create.boot/1")
        self.assertEqual(len(calls), 1)
        self.assertEqual(first[0], 200)
        self.assertEqual(second[:2], first[:2])
        self.assertIsNone(first[2])
        self.assertEqual(second[2], "true")

    def test_unkeyed_repeat_runs_twice(self):
        calls, p = self.fake_create()
        with p:
            self.call("POST", "/v1/vm", {"name": "a"})
            self.call("POST", "/v1/vm", {"name": "a"})
        self.assertEqual(len(calls), 2)

    def test_concurrent_repeat_waits_for_the_first(self):
        started, release = threading.Event(), threading.Event()
        calls = []

        def slow_create(req, source_rootfs=None):
            calls.append(req)
            started.set()
            release.wait(5)
            return {"vm_id": "a", "name": "a", "url": "http://h/1"}

        results = []
        with mock.patch.object(fc_agent, "create_vm", side_effect=slow_create):
            t1 = threading.Thread(target=lambda: results.append(self.call("POST", "/v1/vm", {"name": "a"}, key="k1")))
            t1.start()
            self.assertTrue(started.wait(5))
            t2 = threading.Thread(target=lambda: results.append(self.call("POST", "/v1/vm", {"name": "a"}, key="k1")))
            t2.start()
            time.sleep(0.2)
            # the repeat is parked, not running a second create.
            self.assertEqual(len(calls), 1)
            self.assertEqual(len(results), 0)
            release.set()
            t1.join(5)
            t2.join(5)
        self.assertEqual(len(calls), 1)
        self.assertEqual(len(results), 2)
        self.assertEqual(results[0][:2], results[1][:2])

    def test_expired_key_runs_again(self):
        calls, p = self.fake_create()
        with p:
            self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            with mock.patch.object(fc_agent, "IDEMPOTENCY_TTL", 0.0):
                time.sleep(0.01)
                _, _, replayed = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
        self.assertEqual(len(calls), 2)
        self.assertIsNone(replayed)

    def test_sweep_removes_expired_records(self):
        fc_agent.idem_save("old", "POST /v1/vm", 200, "application/json", b"{}")
        p = fc_agent._idem_path("old")
        os.utime(p, (0, 0))
        fc_agent.idem_save("new", "POST /v1/vm", 200, "application/json", b"{}")
        with mock.patch.object(fc_agent, "_idem_last_sweep", 0.0):
            fc_agent.idem_sweep()
        self.assertFalse(p.exists())
        self.assertTrue(fc_agent._idem_path("new").exists())

    def test_dropped_connection_after_move_does_not_move_again(self):
        moved = threading.Event()
        calls = []

        def pull(*args):
            calls.append(args)
            moved.set()
            return {"snapshot_id": "s1", "digest": DIGEST}

        body = json.dumps({"peer_url": "http://peer", "grant": "g"}).encode()
        with mock.patch.object(fc_agent, "pull_artifact", side_effect=pull):
            # send the move and hang up without reading the answer.
            s = socket.create_connection(("127.0.0.1", self.srv.server_port))
            s.sendall(
                f"POST /v1/artifacts/{DIGEST}/pull HTTP/1.1\r\nHost: x\r\n"
                f"Authorization: Bearer test-token\r\nIdempotency-Key: inv_2:move.pull/1\r\n"
                f"Content-Type: application/json\r\nContent-Length: {len(body)}\r\n\r\n".encode() + body
            )
            s.close()
            self.assertTrue(moved.wait(5))
            for _ in range(100):
                if fc_agent.idem_load("inv_2:move.pull/1"):
                    break
                time.sleep(0.02)
            code, out, replayed = self.call(
                "POST", f"/v1/artifacts/{DIGEST}/pull",
                {"peer_url": "http://peer", "grant": "g"}, key="inv_2:move.pull/1",
            )
        self.assertEqual(len(calls), 1)
        self.assertEqual(code, 200)
        self.assertEqual(json.loads(out)["snapshot_id"], "s1")
        self.assertEqual(replayed, "true")

    def test_key_reused_for_another_request_is_a_422(self):
        calls, p = self.fake_create()
        with p, mock.patch.object(fc_agent, "destroy_vm") as destroy:
            self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            code, _, _ = self.call("DELETE", "/v1/vm/a", key="k1")
        self.assertEqual(code, 422)
        destroy.assert_not_called()

    def test_server_error_is_not_stored(self):
        with mock.patch.object(fc_agent, "create_vm", side_effect=[RuntimeError("boom"), {"vm_id": "a", "url": ""}]) as create:
            first, _, _ = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            second, _, replayed = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
        self.assertEqual((first, second), (500, 200))
        self.assertEqual(create.call_count, 2)
        self.assertIsNone(replayed)

    def test_client_error_is_stored(self):
        with mock.patch.object(fc_agent, "create_vm", side_effect=fc_agent.HTTPError(409, "vm a already exists")) as create:
            first = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
            second = self.call("POST", "/v1/vm", {"name": "a"}, key="k1")
        self.assertEqual(create.call_count, 1)
        self.assertEqual(second[:2], first[:2])

    def test_repeat_destroy_replays_204(self):
        with mock.patch.object(fc_agent, "destroy_vm") as destroy:
            first = self.call("DELETE", "/v1/vm/a", key="k1")
            second = self.call("DELETE", "/v1/vm/a", key="k1")
        self.assertEqual(destroy.call_count, 1)
        self.assertEqual((first[0], second[0]), (204, 204))
        self.assertEqual(second[2], "true")

    def test_bad_key_is_a_400(self):
        code, _, _ = self.call("POST", "/v1/vm", {"name": "a"}, key="bad key")
        self.assertEqual(code, 400)

    def test_get_ignores_key(self):
        code, _, replayed = self.call("GET", "/healthz", key="k1")
        code2, _, replayed2 = self.call("GET", "/healthz", key="k1")
        self.assertEqual((code, code2), (200, 200))
        self.assertIsNone(replayed2)


class NaturalIdempotencyTest(unittest.TestCase):
    """the calls that are safe to repeat without a key."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.vms = mock.patch.object(fc_agent, "VMS_DIR", Path(self.tmp.name))
        self.vms.start()

    def tearDown(self):
        self.vms.stop()
        self.tmp.cleanup()

    def test_destroy_of_missing_vm_succeeds(self):
        self.assertIsNone(fc_agent.destroy_vm("nope"))

    def test_create_with_same_spec_returns_existing_vm(self):
        req = {"name": "a", "cpus": 2}
        meta = {"vm_id": "a", "name": "a", "url": "http://h/1", "create_spec": fc_agent.create_spec_digest(req)}
        fc_agent.vm_dir("a").mkdir(parents=True)
        fc_agent.save_meta(meta)
        with mock.patch.object(fc_agent, "setup_tap") as tap:
            self.assertEqual(fc_agent.create_vm(dict(req)), meta)
        tap.assert_not_called()

    def test_create_with_other_spec_is_a_conflict(self):
        meta = {"vm_id": "a", "name": "a", "create_spec": fc_agent.create_spec_digest({"name": "a", "cpus": 2})}
        fc_agent.vm_dir("a").mkdir(parents=True)
        fc_agent.save_meta(meta)
        rootfs = Path(self.tmp.name) / "rootfs.ext4"
        rootfs.write_bytes(b"")
        with mock.patch.object(fc_agent, "BASE_ROOTFS", rootfs):
            with self.assertRaises(fc_agent.HTTPError) as e:
                fc_agent.create_vm({"name": "a", "cpus": 4})
        self.assertEqual(e.exception.code, 409)


if __name__ == "__main__":
    unittest.main()
