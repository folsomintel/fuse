"""the agent logs the orchestrator's X-Request-ID on every request, so a
failed api call can be followed from the orchestrator log into this one.
stdlib-only, no VMs.
"""
import http.client
import importlib.util
import io
import os
import tempfile
import threading
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


class RequestIDLogTest(unittest.TestCase):
    def setUp(self):
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()

    def get(self, path: str, headers: dict) -> str:
        log = io.StringIO()
        with mock.patch("sys.stderr", log):
            c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=5)
            c.request("GET", path, headers={**AUTH, **headers})
            c.getresponse().read()
            c.close()
            # the access line is written after the response; wait for it.
            for _ in range(50):
                if log.getvalue():
                    break
                threading.Event().wait(0.02)
        return log.getvalue()

    def test_logs_request_id(self):
        out = self.get("/healthz", {"X-Request-ID": "req_0123abcd"})
        self.assertIn("request_id=req_0123abcd", out)

    def test_omits_missing_id(self):
        out = self.get("/healthz", {})
        self.assertIn("/healthz", out)
        self.assertNotIn("request_id=", out)

    def test_drops_malformed_id(self):
        out = self.get("/healthz", {"X-Request-ID": "bad id\x1b[31m"})
        self.assertNotIn("request_id=", out)
        self.assertNotIn("bad id", out)

    def test_failure_line_carries_id(self):
        with mock.patch.object(fc_agent, "host_capacity", side_effect=RuntimeError("boom")):
            out = self.get("/v1/capacity", {"X-Request-ID": "req_fail1"})
        self.assertIn("GET /v1/capacity failed request_id=req_fail1", out)


if __name__ == "__main__":
    unittest.main()
