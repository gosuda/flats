"""Regression checks for the review's concrete gate false-pass classes."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from gate import Host, accepted, data_impact, failed, source_identity


class HarnessChecks(unittest.TestCase):
    def test_acceptance_requires_manager_identity_and_human_boundary(self):
        report = dict(full_suite_selected=True, exit=0, lane_complete=True,
                      identity_bound=True, human_approval="proven")
        self.assertTrue(accepted(report))
        for field, value in (("lane_complete", False), ("identity_bound", False),
                             ("human_approval", "OPEN"), ("full_suite_selected", False), ("exit", 1)):
            self.assertFalse(accepted({**report, field: value}), field)

    def test_failure_needs_its_cause(self):
        failed({"status": "failed", "result": "draft revision changed",
                "result_data": {"status": "failed", "failure_code": "stale_approval"}}, "draft-drift")
        with self.assertRaises(AssertionError):
            failed({"status": "failed", "result": "database unavailable"}, "draft-drift")
        with self.assertRaises(AssertionError):
            failed({"status": "failed", "result": "500 internal error"}, "health")

    def test_impact_requires_a_field_and_truthful_value(self):
        self.assertEqual(data_impact({"data_impact": "none", "health_data": "not_run", "live_data": "untouched"}), "none")
        with self.assertRaises(AssertionError):
            data_impact({"error": 'some mention of "data_impact"'})
        with self.assertRaises(AssertionError):
            data_impact({"data_impact": "none"}, changed=True)

    def test_source_identity_uses_harness_root_from_non_git_cwd(self):
        previous = Path.cwd()
        with tempfile.TemporaryDirectory() as folder:
            try:
                os.chdir(folder)
                identity = source_identity()
            finally:
                os.chdir(previous)
        self.assertEqual(len(identity["head"]), 40)
        self.assertIn("scripts/lifecycle-gate.sh", identity["harness_sha256"])

    def test_draft_upload_cannot_create_an_approval_or_use_version_route(self):
        with tempfile.TemporaryDirectory() as folder:
            host = Host("unused", folder)
            calls = []
            host.ok = lambda *args, **kwargs: calls.append((args, kwargs)) or {"draft": {"revision": 1}}
            host.save("example", {"index.html": "draft"})
            self.assertEqual(calls[0][0][:2], ("POST", "/api/flats/example/draft"))
            self.assertEqual(calls[0][1]["status"], 201)
            with self.assertRaises(AssertionError):
                host.save("example", {"index.html": "draft"}, deploy=True)
            historical = Host("pinned-archive-binary", folder, legacy=True)
            historical.ok = host.ok
            historical.save("example", {"index.html": "historical"})
            self.assertEqual(calls[-1][0][:2], ("POST", "/api/flats/example/versions"))

    def test_version_request_freezes_the_observed_draft_and_requires_new_action(self):
        with tempfile.TemporaryDirectory() as folder:
            host = Host("unused", folder)
            calls = []
            def response(method, path, body=None, **kwargs):
                calls.append((method, path, body))
                if method == "GET":
                    return {"revision": 7, "hash": "frozen-hash"}
                return {"status": "pending_approval", "approval": {"id": "request", "action": "create_version"}}
            host.ok = response
            self.assertEqual(host.create_version("example"), "request")
            self.assertEqual(calls[-1], ("POST", "/api/flats/example/versions", {"revision": 7, "hash": "frozen-hash"}))
            host.ok = lambda *args, **kwargs: ({"revision": 7, "hash": "frozen-hash"} if args[0] == "GET"
                                              else {"status": "pending_approval", "approval": {"id": "wrong", "action": "publish"}})
            with self.assertRaises(AssertionError):
                host.create_version("example")

    def test_stop_rejects_nonzero_exit_and_recovered_panic(self):
        for marker, exit_code in (("", 7), ("panic: simulated handler panic", 0)):
            with self.subTest(marker=marker, exit_code=exit_code), tempfile.TemporaryDirectory() as folder:
                host = Host("unused", folder)
                host.log = open(Path(folder) / "serve.log", "wb")
                host.proc = subprocess.Popen(["python3", "-c", f"print({marker!r}); raise SystemExit({exit_code})"],
                                             stdout=host.log, stderr=host.log)
                host.proc.wait(timeout=5)
                with self.assertRaises(AssertionError):
                    host.stop()


if __name__ == "__main__":
    unittest.main()
