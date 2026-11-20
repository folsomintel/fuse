"""unit tests for the benchmark sampler, leak audit and report.

    python3 -m unittest discover -s benchmark/tests -v
"""
import json
import os
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import leak_audit  # noqa: E402
import report  # noqa: E402
import sampler  # noqa: E402

MEMINFO = """MemTotal:       65536000 kB
MemFree:         1000000 kB
MemAvailable:   32768000 kB
Shmem:            204800 kB
SwapTotal:       1048576 kB
SwapFree:         524288 kB
"""
PSI_MEM = """some avg10=1.50 avg60=0.80 avg300=0.10 total=12345
full avg10=0.25 avg60=0.10 avg300=0.00 total=678
"""
PSI_CPU = "some avg10=12.00 avg60=3.00 avg300=1.00 total=99\n"


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def slurp(path):
    with open(path) as f:
        return f.read()


def fake_proc(root):
    write(os.path.join(root, "meminfo"), MEMINFO)
    write(os.path.join(root, "pressure", "memory"), PSI_MEM)
    write(os.path.join(root, "pressure", "cpu"), PSI_CPU)
    write(os.path.join(root, "loadavg"), "3.25 2.00 1.00 5/900 1234\n")
    for pid, comm, pss in [(100, "firecracker", 1000), (101, "firecracker", 2500), (200, "python3", 9999)]:
        write(os.path.join(root, str(pid), "comm"), comm + "\n")
        write(os.path.join(root, str(pid), "smaps_rollup"), f"Rss: {pss * 2} kB\nPss: {pss} kB\n")
    # a firecracker that exited between listdir and the smaps read
    write(os.path.join(root, "102", "comm"), "firecracker\n")


class SamplerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.proc = self.tmp.name
        fake_proc(self.proc)

    def tearDown(self):
        self.tmp.cleanup()

    def test_sample_fields(self):
        s = sampler.sample(self.proc, self.proc, now=10.0)
        self.assertEqual(s["mem_total_kb"], 65536000)
        self.assertEqual(s["mem_available_kb"], 32768000)
        self.assertEqual(s["swap_used_kb"], 524288)
        self.assertEqual(s["psi_mem_some"], 1.5)
        self.assertEqual(s["psi_mem_full"], 0.25)
        self.assertEqual(s["psi_cpu_some"], 12.0)
        self.assertIsNone(s["psi_io_some"])
        self.assertEqual(s["load1"], 3.25)
        self.assertEqual(s["fc_count"], 3)
        self.assertEqual(s["fc_pss_kb"], 3500)  # the exited one is skipped
        self.assertEqual(s["t"], 10.0)

    def test_without_pss(self):
        self.assertIsNone(sampler.sample(self.proc, self.proc, with_pss=False)["fc_pss_kb"])

    def test_missing_proc_is_all_null(self):
        s = sampler.sample(os.path.join(self.proc, "nope"), self.proc)
        self.assertIsNone(s["mem_available_kb"])
        self.assertIsNone(s["fc_pss_kb"])
        self.assertEqual(s["fc_count"], 0)
        json.dumps(s)

    def test_once_prints_json(self):
        import contextlib
        import io
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            sampler.main(["--once", "--proc", self.proc, "--sys", self.proc])
        self.assertEqual(json.loads(buf.getvalue())["fc_count"], 3)


class LeakAuditTest(unittest.TestCase):
    def test_compare(self):
        before = {"a": 1, "b": 2, "c": None, "d": 5, "e": 0}
        after = {"a": 1, "b": 4, "c": None, "d": None, "e": 0}
        deltas, lost, skipped = leak_audit.compare(before, after)
        self.assertEqual(deltas, {"b": 2})
        self.assertEqual(lost, ["d"])
        self.assertEqual(skipped, ["c"])

    def test_negative_delta_is_a_delta(self):
        deltas, _, _ = leak_audit.compare({"a": 3}, {"a": 2})
        self.assertEqual(deltas, {"a": -1})

    def test_check_settles(self):
        # a count that drains back to the baseline within --settle passes
        counts = iter([5, 4, 3])
        clock = iter(range(100))
        res = leak_audit.check({"vms": 3}, settle=10, probes={"vms": lambda: next(counts)},
                               clock=lambda: next(clock), sleep=lambda _: None)
        self.assertTrue(res["ok"])

    def test_check_fails_after_settle(self):
        clock = iter(range(0, 1000, 5))
        res = leak_audit.check({"vms": 3}, settle=10, probes={"vms": lambda: 4},
                               clock=lambda: next(clock), sleep=lambda _: None)
        self.assertFalse(res["ok"])
        self.assertEqual(res["deltas"], {"vms": 1})

    def test_probe_exception_is_null(self):
        def boom():
            raise RuntimeError("x")
        self.assertEqual(leak_audit.snapshot({"x": boom}), {"x": None})

    def test_count_entries(self):
        with tempfile.TemporaryDirectory() as d:
            os.mkdir(os.path.join(d, "vm-1"))
            os.mkdir(os.path.join(d, "vm-2"))
            write(os.path.join(d, ".reflink-probe"), "x")
            self.assertEqual(leak_audit.count_entries(d), 2)
            self.assertEqual(leak_audit.count_entries(os.path.join(d, "missing")), 0)

    def test_iptables_counts_append_rules(self):
        save = "*filter\n:INPUT ACCEPT [0:0]\n-A FORWARD -i fc-tap0 -j DROP\n-A INPUT -p tcp -j ACCEPT\nCOMMIT\n"
        orig = leak_audit.run
        leak_audit.run = lambda cmd, timeout=30: save
        try:
            self.assertEqual(leak_audit.iptables_rules("iptables-save"), 2)
        finally:
            leak_audit.run = orig

    def test_missing_tool_is_null(self):
        self.assertIsNone(leak_audit.run(["definitely-not-a-tool-xyz"]))


class ReportTest(unittest.TestCase):
    def test_pct_matches_lib_sh(self):
        # lib.sh: idx = int(p/100*n + 0.9999), clamped to [1, n]
        for n in range(1, 40):
            vals = list(range(1, n + 1))
            for p in (50, 90, 99):
                idx = min(max(int(p / 100 * n + 0.9999), 1), n)
                self.assertEqual(report.pct(vals, p), vals[idx - 1], (n, p))
        self.assertIsNone(report.pct([], 50))
        self.assertEqual(report.pct([None, 2.0], 50), 2.0)

    def arrivals(self, rate, n, create_s, failed=0):
        return [{"rate": rate, "i": i, "ok": i >= failed, "create_s": create_s, "exec_s": 0.1,
                 "total_s": create_s + 1} for i in range(n)]

    def test_knee_on_latency(self):
        rows = report.churn_rows(self.arrivals(1, 10, 2.0) + self.arrivals(2, 20, 3.0) + self.arrivals(4, 40, 5.0))
        self.assertEqual(report.find_knee(rows, factor=2), 4)

    def test_knee_on_errors(self):
        rows = report.churn_rows(self.arrivals(1, 100, 2.0) + self.arrivals(2, 100, 2.0, failed=2))
        self.assertEqual(report.find_knee(rows, err_limit=0.01), 2)
        self.assertIsNone(report.find_knee(rows, err_limit=0.05))

    def test_knee_none(self):
        self.assertIsNone(report.find_knee([]))
        rows = report.churn_rows(self.arrivals(1, 10, 2.0) + self.arrivals(2, 10, 2.5))
        self.assertIsNone(report.find_knee(rows))

    def test_nice_ticks(self):
        self.assertEqual(report.nice_ticks(0, 10), [0, 2, 4, 6, 8, 10])
        t = report.nice_ticks(0, 0)
        self.assertGreater(t[-1], t[0])
        # the axis always reaches the largest value
        self.assertEqual(report.nice_ticks(1, 16), [0, 5, 10, 15, 20])
        self.assertGreaterEqual(report.nice_ticks(0, 9.5)[-1], 9.5)

    def assert_svg(self, text):
        root = ET.fromstring(text)
        self.assertTrue(root.tag.endswith("svg"))
        return root

    def test_line_chart_is_valid_svg(self):
        svg = report.line_chart("t <&>", "x", "y", [("a", [(1, 2), (2, 3)]), ("b", [(1, None), (2, 1)])], marker=(2, "knee"))
        root = self.assert_svg(svg)
        circles = [e for e in root.iter() if e.tag.endswith("circle")]
        self.assertEqual(len(circles), 3)
        self.assertIn("prefers-color-scheme: dark", svg)

    def test_line_chart_empty(self):
        self.assertIn("no samples", report.line_chart("t", "x", "y", [("a", [])]))

    def test_suites_end_to_end(self):
        with tempfile.TemporaryDirectory() as out:
            def jl(name, rows):
                with open(os.path.join(out, name), "w") as f:
                    f.writelines(json.dumps(r) + "\n" for r in rows)
            jl("samples.jsonl", [{"t": 100 + i, "mem_available_kb": 1000 - i, "fc_pss_kb": i * 10, "psi_mem_some": 0.1}
                                 for i in range(5)])
            jl("batches.jsonl", [{"batch": 1, "live": 2, "mem_available_kb": 900, "fc_pss_kb": 200, "fc_count": 2},
                                 {"batch": 2, "live": 4, "mem_available_kb": 800, "fc_pss_kb": 400, "fc_count": 4}])
            jl("ops.jsonl", [{"batch": 1, "op": "create", "s": 1.0, "ok": True},
                             {"batch": 1, "op": "create", "s": 2.0, "ok": True},
                             {"batch": 1, "op": "exec", "s": 0.1, "ok": True},
                             {"batch": 2, "op": "create", "s": 3.0, "ok": True},
                             {"batch": 2, "op": "exec", "s": None, "ok": False}])
            jl("stop.jsonl", [{"batch": 2, "reason": "max vms 4"}])
            with open(os.path.join(out, "leak.json"), "w") as f:
                json.dump({"ok": True, "skipped": ["db_vms"]}, f)
            rows = report.report_ramp(out)
            self.assertEqual(rows[0]["create_p99"], 2.0)
            self.assertEqual(rows[1]["errors"], 1)
            summary = slurp(os.path.join(out, "summary.md"))
            self.assertIn("max live: 4, stop: max vms 4", summary)
            self.assertIn("leak audit: clean", summary)
            for name in ("ramp-memory.svg", "ramp-latency.svg", "host-memory.svg", "host-psi.svg"):
                self.assert_svg(slurp(os.path.join(out, name)))

            jl("arrivals.jsonl", self.arrivals(1, 10, 2.0) + self.arrivals(2, 10, 5.0))
            _, knee = report.report_churn(out, 2, 0.01)
            self.assertEqual(knee, 2)
            self.assert_svg(slurp(os.path.join(out, "churn-latency.svg")))

            jl("children.jsonl", [{"n": 1, "k": 1, "fork_s": 1.0, "first_exec_s": 2.0, "memory": "cold", "ok": True},
                                  {"n": 2, "k": 1, "fork_s": 1.0, "first_exec_s": 2.5, "memory": "live", "ok": True},
                                  {"n": 2, "k": 2, "fork_s": None, "first_exec_s": None, "memory": "", "ok": False}])
            jl("levels.jsonl", [{"n": 1, "mem_available_before_kb": 1000, "mem_available_after_kb": 900, "fc_pss_after_kb": 50},
                                {"n": 2, "mem_available_before_kb": 1000, "mem_available_after_kb": 700, "fc_pss_after_kb": 90}])
            rows = report.report_fanout(out)
            self.assertEqual([r["host_used_kb"] for r in rows], [100, 300])
            self.assertEqual(rows[1]["errors"], 1)
            self.assertEqual(rows[1]["live_memory"], 1)
            self.assert_svg(slurp(os.path.join(out, "fanout-memory.svg")))

    def test_leak_line_failed(self):
        with tempfile.TemporaryDirectory() as out:
            with open(os.path.join(out, "leak.json"), "w") as f:
                json.dump({"ok": False, "deltas": {"vm_dirs": 2}, "lost": ["db_vms"]}, f)
            self.assertEqual(report.leak_line(out), "leak audit: FAILED (vm_dirs +2, db_vms lost)")


if __name__ == "__main__":
    unittest.main()
