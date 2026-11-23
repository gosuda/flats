"""Test the lifecycle gate wrapper: verdict rules, output filtering and the driver.

The wrapper runs scripts/lifecycle-gate.sh and decides the verdict from the
gate's evidence.json. These tests use synthetic reports and stub gates in
temporary directories. They run on Linux, as the CI helper lane does.
"""

import contextlib
import importlib.util
import io
import json
import os
import re
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
LIFECYCLE = ROOT / "internal" / "lifecyclecheck"
WRAPPER = ROOT / "scripts" / "ci-lifecycle-gate.py"

sys.path.insert(0, str(LIFECYCLE))
import gate  # noqa: E402

spec = importlib.util.spec_from_file_location("ci_lifecycle_gate", WRAPPER)
wrapper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wrapper)

LOCAL_LANE = "actual-binary-local"
ADAPTER_LANE = "production-provider-manager"
EXPECTED = {name: LOCAL_LANE for name, _ in gate.LOCAL_CASES}
EXPECTED.update({name: ADAPTER_LANE for name, _ in gate.ADAPTER_CASES})
OPEN = "OPEN: forged headers are not human proof"
CANARY = "5f3c1a9e0b7d4c2e8a6f1b3d9c7e5a20"
SECRET = "test-access-secret-7731"
SECRET_NAME = re.compile(r"TOKEN|SECRET|KEY|PASS|AUTH")
WITHHELD = "[line withheld: it contains a protected value]"
TABLE_ROW = re.compile(r"^\| ([a-z0-9-]+) \|")


def passing_cases():
    return [
        {"id": name, "lane": lane, "outcome": "pass", "error": ""}
        for name, lane in EXPECTED.items()
    ]


def passing_report(**changes):
    report = {
        "mode": "acceptance",
        "full_suite_selected": True,
        "lane_complete": True,
        "identity_bound": True,
        "exit": 1,
        "human_approval": OPEN,
        "cases": passing_cases(),
    }
    report.update(changes)
    return report


def table_rows(summary):
    return [
        line for line in summary.splitlines()
        if (match := TABLE_ROW.match(line)) and match.group(1) in EXPECTED
    ]


class PinTests(unittest.TestCase):
    def test_case_count_is_pinned(self):
        self.assertEqual(len(gate.LOCAL_CASES) + len(gate.ADAPTER_CASES), 28)


class VerdictTests(unittest.TestCase):
    def assertPasses(self, report, process_exit):
        self.assertEqual(wrapper.verdict(report, process_exit, EXPECTED), [])

    def assertFails(self, report, process_exit):
        self.assertTrue(wrapper.verdict(report, process_exit, EXPECTED))

    def test_open_approval_with_exit_one_and_all_cases_passing_passes(self):
        self.assertPasses(passing_report(), 1)

    def test_a_failed_case_fails(self):
        cases = passing_cases()
        cases[0]["outcome"] = "fail"
        self.assertFails(passing_report(cases=cases), 1)

    def test_open_approval_with_exit_zero_fails(self):
        self.assertFails(passing_report(exit=0), 0)

    def test_process_exit_two_fails(self):
        self.assertFails(passing_report(exit=2), 2)

    def test_report_exit_must_equal_process_exit(self):
        self.assertFails(passing_report(exit=0), 1)

    def test_each_lane_flag_must_hold(self):
        for key in ("lane_complete", "identity_bound", "full_suite_selected"):
            with self.subTest(key=key):
                self.assertFails(passing_report(**{key: False}), 1)

    def test_mode_must_be_acceptance(self):
        self.assertFails(passing_report(mode="legacy-preparation"), 1)

    def test_unknown_human_approval_value_fails(self):
        self.assertFails(passing_report(human_approval="yes"), 1)

    def test_proven_with_nonzero_exit_fails(self):
        self.assertFails(passing_report(human_approval="proven", exit=1), 1)

    def test_proven_with_zero_exit_passes(self):
        self.assertPasses(passing_report(human_approval="proven", exit=0), 0)

    def test_case_list_must_match_the_gate(self):
        duplicate = passing_cases() + passing_cases()[:1]
        extra = passing_cases() + [{"id": "unknown-case", "lane": LOCAL_LANE, "outcome": "pass"}]
        wrong_lane = passing_cases()
        wrong_lane[0]["lane"] = ADAPTER_LANE
        cases = {
            "missing": passing_cases()[:-1],
            "duplicate": duplicate,
            "extra": extra,
            "wrong lane": wrong_lane,
        }
        for name, rows in cases.items():
            with self.subTest(name=name):
                self.assertFails(passing_report(cases=rows), 1)


class DriverTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.tmp = self.root / "tmp"
        self.tmp.mkdir()
        self.summary = self.root / "summary.md"

    def stub(self, body):
        path = self.root / "gate.sh"
        path.write_text("#!/bin/sh\n" + body)
        path.chmod(0o755)
        return path

    def evidence(self, content):
        path = self.root / "evidence-source.json"
        path.write_text(content if isinstance(content, str) else json.dumps(content))
        return path

    def evidence_body(self, source, exit_code=1, directories=1):
        lines = []
        for _ in range(directories):
            lines += [
                'd=$(mktemp -d "$TMPDIR/flats-lifecycle-gate.XXXXXX")',
                f"cp '{source}' \"$d/evidence.json\"",
                'echo "Evidence: $d/evidence.json"',
            ]
        lines.append(f"exit {exit_code}")
        return "\n".join(lines) + "\n"

    def run_wrapper(self, body, *, env=None, unset=(), timeout=wrapper.GATE_TIMEOUT_SECONDS):
        gate_path = self.stub(body)
        values = {
            "TMPDIR": str(self.tmp),
            "LIFECYCLE_GATE_SENTINEL": CANARY,
            "GITHUB_STEP_SUMMARY": str(self.summary),
            "TEST_ACCESS_TOKEN": SECRET,
            **(env or {}),
        }
        out = io.StringIO()
        with mock.patch.dict(os.environ):
            for name in list(os.environ):
                if SECRET_NAME.search(name):
                    del os.environ[name]
            os.environ.update(values)
            for name in unset:
                os.environ.pop(name, None)
            with contextlib.redirect_stdout(out):
                code = wrapper.main(["--gate", str(gate_path)], timeout=timeout)
        summary = self.summary.read_text() if self.summary.exists() else ""
        return code, out.getvalue(), summary

    def test_no_evidence_fails_and_the_summary_holds_the_verdict(self):
        code, out, summary = self.run_wrapper('echo "PASS: draft-save-conflict"\nexit 1\n')
        self.assertEqual(code, 1)
        self.assertIn("FAIL", out)
        self.assertIn("FAIL", summary)

    def test_failing_evidence_fails(self):
        report = passing_report()
        report["cases"][0]["outcome"] = "fail"
        source = self.evidence(report)
        code, out, _ = self.run_wrapper(self.evidence_body(source))
        self.assertEqual(code, 1)
        self.assertIn("FAIL", out)

    def test_passing_evidence_with_exit_one_passes(self):
        source = self.evidence(passing_report())
        code, out, summary = self.run_wrapper(self.evidence_body(source, exit_code=1))
        self.assertEqual(code, 0, out)
        self.assertIn("PASS", out.splitlines()[-1])
        self.assertEqual(len(table_rows(summary)), 28)

    def test_two_new_evidence_directories_fail(self):
        source = self.evidence(passing_report())
        code, out, _ = self.run_wrapper(self.evidence_body(source, directories=2))
        self.assertEqual(code, 1)

    def test_unreadable_evidence_fails(self):
        source = self.evidence("{not json")
        code, _, _ = self.run_wrapper(self.evidence_body(source))
        self.assertEqual(code, 1)

    def test_non_object_evidence_fails(self):
        source = self.evidence("[]")
        code, _, _ = self.run_wrapper(self.evidence_body(source))
        self.assertEqual(code, 1)

    def test_unset_tmpdir_fails(self):
        source = self.evidence(passing_report())
        code, _, _ = self.run_wrapper(self.evidence_body(source), unset=("TMPDIR",))
        self.assertEqual(code, 1)

    def test_missing_or_short_canary_fails_before_the_gate_starts(self):
        source = self.evidence(passing_report())
        for label, kwargs in [
            ("unset", {"unset": ("LIFECYCLE_GATE_SENTINEL",)}),
            ("15 characters", {"env": {"LIFECYCLE_GATE_SENTINEL": CANARY[:15]}}),
        ]:
            with self.subTest(label=label):
                marker = self.root / f"ran-{label.replace(' ', '-')}"
                body = f"touch '{marker}'\n" + self.evidence_body(source)
                code, _, _ = self.run_wrapper(body, **kwargs)
                self.assertEqual(code, 1)
                self.assertFalse(marker.exists())

    def test_timeout_with_output_withholds_the_canary_and_fails(self):
        body = 'echo "FAIL: draft-save-conflict: $LIFECYCLE_GATE_SENTINEL"\nexec sleep 60\n'
        code, out, _ = self.run_wrapper(body, timeout=3)
        self.assertEqual(code, 1)
        self.assertIn(WITHHELD, out)
        self.assertNotIn(CANARY, out)
        self.assertIn("FAIL", out.splitlines()[-1])

    def test_timeout_without_output_fails_without_a_crash(self):
        code, out, _ = self.run_wrapper("exec sleep 60\n", timeout=3)
        self.assertEqual(code, 1)
        self.assertIn("FAIL", out.splitlines()[-1])

    def test_canary_in_a_case_line_is_withheld_and_fails(self):
        report = passing_report()
        report["cases"][0] = {
            "id": "draft-save-conflict",
            "lane": LOCAL_LANE,
            "outcome": "fail",
            "error": f"boom {CANARY}",
        }
        source = self.evidence(report)
        body = 'echo "FAIL: draft-save-conflict: $LIFECYCLE_GATE_SENTINEL"\n' + self.evidence_body(source)
        code, out, summary = self.run_wrapper(body)
        self.assertEqual(code, 1)
        self.assertIn(WITHHELD, out)
        self.assertNotIn(CANARY, out)
        self.assertNotIn(CANARY, summary)
        self.assertIn("withheld", out.splitlines()[-1])

    def test_canary_split_across_two_lines_replaces_the_whole_output(self):
        body = (
            f'echo "{CANARY[:16]}"\n'
            f'echo "{CANARY[16:]}"\n'
            + self.evidence_body(self.evidence(passing_report()))
        )
        code, out, _ = self.run_wrapper(body)
        self.assertEqual(code, 1)
        self.assertIn("[gate output withheld]", out)
        self.assertNotIn(CANARY[:16], out)
        self.assertNotIn(CANARY[16:], out)
        self.assertIn("leak", out.splitlines()[-1])

    def test_secret_value_in_an_informational_line_is_withheld_and_fails(self):
        body = 'echo "info: $TEST_ACCESS_TOKEN"\n' + self.evidence_body(self.evidence(passing_report()))
        code, out, _ = self.run_wrapper(body)
        self.assertEqual(code, 1)
        self.assertIn(WITHHELD, out)
        self.assertNotIn(SECRET, out)

    def test_output_without_a_protected_value_follows_the_verdict_rules_only(self):
        body = 'echo "PASS: draft-save-conflict"\n' + self.evidence_body(self.evidence(passing_report()))
        code, out, _ = self.run_wrapper(body)
        self.assertEqual(code, 0, out)

    def test_canary_only_in_evidence_json_fails_as_a_leak(self):
        report = passing_report()
        report["cases"][0]["error"] = f"stored {CANARY}"
        source = self.evidence(report)
        code, out, summary = self.run_wrapper(self.evidence_body(source))
        self.assertEqual(code, 1)
        self.assertNotIn(CANARY, out)
        self.assertNotIn(CANARY, summary)
        self.assertIn("leak", out.splitlines()[-1])


if __name__ == "__main__":
    unittest.main()
