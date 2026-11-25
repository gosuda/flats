"""Keep the lifecycle gate jobs to their cache, permission, trigger and secret rules.

The tests read the workflow files as text, as test_ci_cache.py does. The
pinned revision must match the one that the gate module names.
"""

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github" / "workflows"
GATE_MODULE = ROOT / "internal" / "lifecyclecheck" / "gate.py"
FORBIDDEN_USES = ("uses: actions/cache@", "uses: ./.github/actions/setup-go")


def workflow_text(name):
    path = WORKFLOWS / name
    return path.read_text() if path.exists() else ""


def job_text(workflow, name):
    match = re.search(rf"(?ms)^  {re.escape(name)}:\n.*?(?=^  [\w-]+:\n|\Z)", workflow)
    if match is None:
        raise AssertionError(f"no job named {name}")
    return match.group()


def step_text(job, marker):
    start = job.find(marker)
    if start < 0:
        raise AssertionError(f"no step with {marker}")
    end = job.find("\n      - ", start + 1)
    return job[start:] if end < 0 else job[start:end]


class LifecycleGateWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.gate_workflow = workflow_text("lifecycle-gate.yml")
        self.ci = workflow_text("ci.yml")
        self.gate_job = job_text(self.gate_workflow, "gate")
        self.legacy_job = job_text(self.ci, "legacy-build")
        self.jobs = {"gate": self.gate_job, "legacy-build": self.legacy_job}
        self.pinned = re.search(
            r'^LEGACY_REVISION = "([0-9a-f]{40})"', GATE_MODULE.read_text(), re.M
        ).group(1)

    def test_gate_workflow_uses_no_cache_saving_or_shared_setup_action(self):
        for forbidden in FORBIDDEN_USES:
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, self.gate_workflow)

    def test_legacy_build_job_uses_no_cache_saving_or_shared_setup_action(self):
        for forbidden in FORBIDDEN_USES:
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, self.legacy_job)

    def test_each_new_job_restores_caches_and_disables_setup_go_cache(self):
        for name, job in self.jobs.items():
            with self.subTest(job=name):
                self.assertGreaterEqual(job.count("uses: actions/cache/restore@v4"), 2)
                self.assertIn("cache: false", step_text(job, "uses: actions/setup-go@v5"))

    def test_permissions_are_read_only(self):
        block = re.search(r"^permissions:\n((?:  .*\n)+)", self.gate_workflow, re.M)
        self.assertIsNotNone(block, "no top-level permissions block")
        self.assertEqual(block.group(1), "  contents: read\n")
        for name, text in [("lifecycle-gate.yml", self.gate_workflow), ("legacy-build", self.legacy_job)]:
            with self.subTest(text=name):
                self.assertNotIn(": write", text)

    def test_schedule_is_weekly_off_the_hour_and_manual_dispatch_exists(self):
        crons = re.findall(r"cron: '([^']+)'", self.gate_workflow)
        self.assertEqual(crons, ["23 5 * * 3"])
        self.assertNotEqual(crons[0].split()[0], "0")
        notebook = re.search(r"cron: '([^']+)'", workflow_text("notebook-memory-race.yml")).group(1)
        self.assertNotEqual(crons[0], notebook)
        triggers = self.gate_workflow.split("\npermissions:", 1)[0]
        self.assertIn("  workflow_dispatch:", triggers)

    def test_each_new_job_sets_its_timeout(self):
        self.assertIn("timeout-minutes: 30", self.gate_job)
        self.assertIn("timeout-minutes: 20", self.legacy_job)

    def test_gate_sets_the_canary_before_the_wrapper_runs(self):
        canary = 'echo "LIFECYCLE_GATE_SENTINEL=$(openssl rand -hex 16)" >> "$GITHUB_ENV"'
        wrapper = "python3 -B scripts/ci-lifecycle-gate.py"
        self.assertIn(canary, self.gate_job)
        self.assertIn(wrapper, self.gate_job)
        self.assertLess(self.gate_job.index(canary), self.gate_job.index(wrapper))

    def test_gate_has_the_repository_guard(self):
        self.assertRegex(
            self.gate_job, r"(?m)^    if: \$\{\{ github\.repository == 'gosuda/flats' \}\}$"
        )

    def test_gate_workflow_publishes_no_artifact_and_names_no_secret(self):
        self.assertNotIn("upload-artifact", self.gate_workflow)
        self.assertNotIn("secrets.", self.gate_workflow)

    def test_each_new_job_checkout_does_not_persist_credentials(self):
        for name, job in self.jobs.items():
            with self.subTest(job=name):
                self.assertRegex(
                    job, r"uses: actions/checkout@v4\n\s+with:\n\s+persist-credentials: false"
                )

    def test_legacy_build_runs_on_full_pull_requests_and_runs_the_prepare_step(self):
        condition = re.search(r"(?m)^    if: (.+)$", self.legacy_job).group(1)
        self.assertIn("github.event_name == 'pull_request'", condition)
        self.assertIn("needs.changes.outputs.full == 'true'", condition)
        self.assertIn("needs: changes", self.legacy_job)
        self.assertIn("scripts/lifecycle-gate.sh --prepare-only", self.legacy_job)

    def test_both_jobs_fetch_the_pinned_revision_of_the_gate_module(self):
        for name, job in self.jobs.items():
            with self.subTest(job=name):
                fetched = re.search(r"git fetch --no-tags --depth=1 origin ([0-9a-f]{40})", job)
                self.assertIsNotNone(fetched, "no pinned fetch")
                self.assertEqual(fetched.group(1), self.pinned)

    def test_both_jobs_run_on_the_linux_runner(self):
        for name, job in self.jobs.items():
            with self.subTest(job=name):
                self.assertIn("runs-on: depot-ubuntu-24.04", job)


if __name__ == "__main__":
    unittest.main()
