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
