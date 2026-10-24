"""Exact-source verification reuse and commit-selection regression fixtures."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("image_commits", SCRIPTS / "image-commits.py")
images = importlib.util.module_from_spec(spec)
spec.loader.exec_module(images)


class ImageCommitsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q")
        self.git("config", "user.email", "ci@example.invalid")
        self.git("config", "user.name", "CI fixture")
        self.commits = []
        for i in range(3):
            (self.root / "source").write_text(str(i))
            self.git("add", "source")
            self.git("commit", "-qm", f"fixture {i}")
            self.commits.append(self.git("rev-parse", "HEAD"))
        self.git("update-ref", "refs/remotes/origin/main", self.commits[-1])
        self.event = {"ref": "refs/heads/main", "before": self.commits[0],
                      "after": self.commits[-1], "deleted": False}

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.root), *args], text=True).strip()

    def select(self, event_name="push", event=None, requested=""):
        previous = Path.cwd()
        try:
            os.chdir(self.root)
            return images.select(event_name, event or self.event, requested)
        finally:
            os.chdir(previous)

    def test_multi_commit_push_preserves_intermediate_commits(self):
        self.assertEqual(self.select(), self.commits[1:])
        lanes = images.matrix(self.select())["include"]
        self.assertEqual([c for lane in lanes for c in lane["commits"]], self.commits[1:])

    def test_large_push_keeps_every_commit_in_four_lanes(self):
        commits = [f"{i:040x}" for i in range(1025)]
        lanes = images.matrix(commits)["include"]
        self.assertEqual(len(lanes), 4)
        selected = [c for lane in lanes for c in lane["commits"]]
        self.assertEqual(len(selected), len(commits))
        self.assertEqual(set(selected), set(commits))

    def test_branch_creation_selects_only_tip(self):
        self.assertEqual(self.select(event={**self.event, "before": "0" * 40}), [self.commits[-1]])

    def test_manual_historical_selection(self):
        self.assertEqual(self.select("workflow_dispatch", requested=self.commits[0]), [self.commits[0]])

    def test_exact_tip_is_only_reusable_proof(self):
        tip = self.commits[-1]
        self.assertEqual(images.verified_commit("push", self.event, tip, tip), tip)
        cases = [
            ("workflow_dispatch", self.event, tip, tip),
            ("pull_request", self.event, tip, tip),
            ("push", {**self.event, "ref": "refs/heads/topic"}, tip, tip),
            ("push", {**self.event, "deleted": True}, tip, tip),
            ("push", self.event, self.commits[1], tip),
            ("push", self.event, tip, self.commits[1]),
            ("push", self.event, "", tip),
        ]
        for case in cases:
            with self.subTest(case=case):
                self.assertEqual(images.verified_commit(*case), "")

    def run_build(self, *, commit=None, verified=None, failed="", event="push", fail_shard="",
                  attempt="1", image_exists=False, ref="refs/heads/main"):
        tip = self.commits[-1]
        scripts = self.root / "scripts"
        scripts.mkdir(exist_ok=True)
        (scripts / "build-commit-image.sh").write_bytes((SCRIPTS / "build-commit-image.sh").read_bytes())
        # Stop before compilation/publication, after recording whether the full
        # verification gate was reached. No real network or Docker use.
        for name, body in {
            "docker": 'echo "docker $*" >> "$CALL_LOG"\n[ "$IMAGE_EXISTS" = true ]\n',
            "go": 'echo "go $*" >> "$CALL_LOG"\n',
            "ci-test.sh": 'echo "$1" >> "$CALL_LOG"\n[ "$1" != "$FAIL_SHARD" ]\n',
            "build-release.sh": 'echo build >> "$CALL_LOG"\nexit 37\n',
        }.items():
            path = scripts / name
            path.write_text("#!/bin/sh\n" + body)
            path.chmod(0o755)
        log = self.root / "calls"
        log.write_text("")
        env = {**os.environ, "PATH": f"{scripts}:{os.environ['PATH']}",
               "RUNNER_TEMP": str(self.root), "GITHUB_EVENT_NAME": event,
               "GITHUB_REF": ref, "GITHUB_SHA": tip,
               "GITHUB_RUN_ATTEMPT": attempt, "IMAGE": "ghcr.io/example/flats",
               "IMAGE_EXISTS": "true" if image_exists else "false",
               "VERIFIED_COMMIT": tip if verified is None else verified,
               "FAILED_COMMIT": failed,
               "CALL_LOG": str(log), "FAIL_SHARD": fail_shard}
        result = subprocess.run(["sh", str(scripts / "build-commit-image.sh"), commit or tip],
                                cwd=self.root, env=env, capture_output=True, text=True)
        return result.returncode, log.read_text().splitlines()

    def test_failed_tip_is_only_the_exact_tip_without_proof(self):
        tip = self.commits[-1]
        self.assertEqual(images.failed_commit("push", self.event, tip, tip, ""), tip)
        cases = [
            ("push", self.event, tip, tip, tip),
            ("workflow_dispatch", self.event, tip, tip, ""),
            ("pull_request", self.event, tip, tip, ""),
            ("push", {**self.event, "ref": "refs/heads/topic"}, tip, tip, ""),
            ("push", {**self.event, "deleted": True}, tip, tip, ""),
            ("push", self.event, self.commits[1], tip, ""),
            ("push", self.event, tip, self.commits[1], ""),
            ("push", self.event, "", tip, ""),
        ]
        for case in cases:
            with self.subTest(case=case):
                self.assertEqual(images.failed_commit(*case), "")

    def test_shell_reports_failed_tip_without_verifying_or_building(self):
        code, calls = self.run_build(verified="", failed=self.commits[-1])
        self.assertNotEqual(code, 0)
        self.assertEqual(calls, [])

    def test_shell_verifies_intermediate_commit_of_a_failed_push(self):
        code, calls = self.run_build(commit=self.commits[1], verified="", failed=self.commits[-1])
        self.assertEqual(code, 37)
        self.assertEqual(calls, ["go vet ./...", "runtime-soak", "runtime-codec", "runtime",
                                 "docs-soak", "docs-budget", "docs", "board", "other", "build"])

    def test_retry_of_failed_tip_keeps_existing_image_and_reports_missing_one(self):
        tip = self.commits[-1]
        code, calls = self.run_build(verified="", failed=tip, attempt="2", image_exists=True)
        self.assertEqual(code, 0)
        self.assertEqual(calls, [f"docker buildx imagetools inspect ghcr.io/example/flats:sha-{tip}"])
        code, calls = self.run_build(verified="", failed=tip, attempt="2")
        self.assertNotEqual(code, 0)
        self.assertEqual(calls, [f"docker buildx imagetools inspect ghcr.io/example/flats:sha-{tip}"])

    def test_retry_after_tests_pass_reuses_the_tip_verification(self):
        code, calls = self.run_build(attempt="2")
        self.assertEqual(code, 37)
        self.assertEqual(calls[1:], ["build"])

    def test_shell_skips_only_exact_verified_tip(self):
        code, calls = self.run_build()
        self.assertEqual(code, 37)
        self.assertEqual(calls, ["build"])

    def test_shell_runs_all_shards_for_intermediate_manual_and_missing_proof(self):
        for kwargs in ({"commit": self.commits[1]}, {"event": "workflow_dispatch"}, {"verified": ""}):
            with self.subTest(kwargs=kwargs):
                code, calls = self.run_build(**kwargs)
                self.assertEqual(code, 37)
                self.assertEqual(calls, ["go vet ./...", "runtime-soak", "runtime-codec", "runtime",
                                         "docs-soak", "docs-budget", "docs", "board", "other", "build"])

    def test_push_retry_skips_existing_full_sha_image(self):
        # Historical batch entries are also preserved without republication.
        for commit in (self.commits[-1], self.commits[1]):
            with self.subTest(commit=commit):
                code, calls = self.run_build(commit=commit, attempt="2", image_exists=True)
                self.assertEqual(code, 0)
                self.assertEqual(calls, [f"docker buildx imagetools inspect ghcr.io/example/flats:sha-{commit}"])

    def test_push_retry_missing_image_keeps_normal_verification(self):
        code, calls = self.run_build(attempt="2", verified="")
        self.assertEqual(code, 37)
        self.assertEqual(calls[0], f"docker buildx imagetools inspect ghcr.io/example/flats:sha-{self.commits[-1]}")
        self.assertEqual(calls[1:], ["go vet ./...", "runtime-soak", "runtime-codec", "runtime",
                                    "docs-soak", "docs-budget", "docs", "board", "other", "build"])

    def test_manual_retry_and_first_attempt_do_not_inspect_registry(self):
        for kwargs in ({"event": "workflow_dispatch", "attempt": "2"}, {"attempt": "1"},
                       {"attempt": "2", "ref": "refs/heads/topic"}):
            with self.subTest(kwargs=kwargs):
                code, calls = self.run_build(image_exists=True, **kwargs)
                self.assertEqual(code, 37)
                self.assertFalse(any(call.startswith("docker ") for call in calls))
                self.assertEqual(calls[-1], "build")
                if kwargs.get("event") == "workflow_dispatch":
                    self.assertEqual(calls[0], "go vet ./...")

    def test_tooling_head_mismatch_does_not_reuse_proof(self):
        self.git("checkout", "--detach", "-q", self.commits[0])
        code, calls = self.run_build()
        self.assertEqual(code, 37)
        self.assertEqual(calls[0], "go vet ./...")
        self.assertEqual(calls[-2:], ["other", "build"])

    def test_tooling_head_mismatch_does_not_trust_failed_tip(self):
        self.git("checkout", "--detach", "-q", self.commits[0])
        code, calls = self.run_build(verified="", failed=self.commits[-1])
        self.assertEqual(code, 37)
        self.assertEqual(calls[0], "go vet ./...")
        self.assertEqual(calls[-2:], ["other", "build"])

    def test_failed_fallback_runs_remaining_shards_and_prevents_build(self):
        code, calls = self.run_build(verified="", fail_shard="runtime")
        self.assertNotEqual(code, 0)
        self.assertEqual(calls[-1], "other")
        self.assertNotIn("build", calls)


if __name__ == "__main__":
    unittest.main()
