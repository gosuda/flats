"""Keep the CI shard lists in step, and pin which packages each shard tests.

scripts/ci-test.sh defines the shards of go test ./.... CI runs them in lanes,
the release workflow and the commit image fallback each run all of them, and
a shard missing from one list would silently drop its tests there.
"""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = ROOT / "scripts"


def case_labels(path):
    """The labels of a script's top-level case statement, without *."""
    return re.findall(r"^([a-z][a-z0-9-]*)\)", path.read_text(), re.M)


def one_line(pattern, path):
    match = re.search(pattern, path.read_text(), re.M)
    if not match:
        raise AssertionError(f"{path.name}: no match for {pattern}")
    return match.group(1)


class ShardListTests(unittest.TestCase):
    def setUp(self):
        self.shards = case_labels(SCRIPTS / "ci-test.sh")
        self.assertIn("other", self.shards)

    def test_ci_lanes_run_every_shard_once(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        lanes = [line.split() for line in re.findall(r"^ +shards: (.+)$", ci, re.M)]
        listed = [shard for lane in lanes for shard in lane]
        self.assertEqual(sorted(listed), sorted(self.shards))

    def test_release_and_image_fallback_run_every_shard(self):
        release = one_line(r"^ +shard: \[(.+)\]$", ROOT / ".github/workflows/release.yml")
        fallback = one_line(r"for shard in (.+); do", SCRIPTS / "build-commit-image.sh")
        self.assertEqual(sorted(s.strip() for s in release.split(",")), sorted(self.shards))
        self.assertEqual(sorted(fallback.split()), sorted(self.shards))

    def test_routine_and_scheduled_race_lanes_cover_every_lane_once(self):
        ci = ROOT / ".github/workflows/ci.yml"
        scheduled = ROOT / ".github/workflows/notebook-memory-race.yml"
        routine = one_line(r"^ +lane: \[(.+)\]$", ci).split(", ")
        large = one_line(r"^ +lane: \[(.+)\]$", scheduled).split(", ")
        self.assertEqual(routine, ["core-1", "core-2", "services", "notebook-1", "notebook-2", "board-1", "board-2"])
        groups = case_labels(SCRIPTS / "ci-memory.sh")
        self.assertEqual(large, ["notebook-large-" + group for group in groups])
        self.assertEqual(sorted(routine + large), sorted(case_labels(SCRIPTS / "ci-race.sh")))

    def test_large_memory_groups_run_on_prs_without_race(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        job = ci.split("\n  notebook-memory:\n", 1)[1].split("\n  # Native macOS:", 1)[0]
        groups = re.search(r"for group in (.+); do", job).group(1).split()
        self.assertEqual(groups, case_labels(SCRIPTS / "ci-memory.sh"))
        self.assertIn('scripts/ci-memory.sh "$group" || failed=1', job)
        self.assertIn('exit "$failed"', job)
        self.assertNotIn("-race", job)
        self.assertIn("timeout-minutes: 20", job)
        self.assertIn("scripts/ci-memory.sh", ci.split("      - name: Lint\n", 1)[1])

    def test_memory_races_are_weekly_or_manual_and_read_only(self):
        workflow = (ROOT / ".github/workflows/notebook-memory-race.yml").read_text()
        triggers = workflow.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(re.findall(r"^  (\w+):", triggers, re.M), ["schedule", "workflow_dispatch"])
        self.assertIn("cron: '17 3 * * 1'", triggers)
        self.assertIn("permissions:\n  contents: read\n", workflow)
        self.assertNotIn(": write", workflow)
        self.assertIn("timeout-minutes: 30", workflow)
        self.assertIn('scripts/ci-race.sh "${{ matrix.lane }}"', workflow)

    def test_large_reader_lane_includes_directly_flagged_tests(self):
        script = (SCRIPTS / "ci-memory.sh").read_text()
        patterns = re.findall(r"^\s*pattern='(.+)'$", script, re.M)[:2]
        self.assertEqual(len(patterns), 2)
        flagged = set()
        for path in (ROOT / "internal/notebook").glob("*_test.go"):
            functions = re.split(r"(?m)^func (\w+)\([^\n]*\) [^{]*\{", path.read_text())
            for name, body in zip(functions[1::2], functions[2::2]):
                if name.startswith("Test") and 'os.Getenv("NOTEBOOK_LARGE_MEMORY_TESTS")' in body:
                    flagged.add(name)
        self.assertIn("TestMarkdownRetainedMemory", flagged)
        for name in flagged:
            with self.subTest(test=name):
                self.assertEqual(sum(re.fullmatch(p, name) is not None for p in patterns), 1)


class ShardPackageTests(unittest.TestCase):
    """Run ci-test.sh with a stub go command on fixture source layouts."""

    CURRENT = ["cmd/flats", "internal/apps/docs", "internal/board", "internal/board/engine",
               "internal/board/benchdata/cmd/benchdata", "internal/boardx", "internal/core",
               "internal/notebook", "internal/runtime", "internal/runtimex"]

    def run_shard(self, shard, module="github.com/gosuda/flats", packages=None):
        packages = self.CURRENT if packages is None else packages
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            for package in packages:
                (work / package).mkdir(parents=True, exist_ok=True)
            listing = "\n".join(f"{module}/{p}" for p in packages)
            stub = work / "bin" / "go"
            stub.parent.mkdir()
            stub.write_text(f"""#!/bin/sh
case "$1 $2" in
"list -m") echo {module} ;;
"list ./...") cat <<'LIST'
{listing}
LIST
;;
test*) shift; echo "$@" ;;
*) exit 9 ;;
esac
""")
            stub.chmod(0o755)
            env = {**os.environ, "PATH": f"{stub.parent}:{os.environ['PATH']}"}
            result = subprocess.run(["sh", str(SCRIPTS / "ci-test.sh"), shard], cwd=work,
                                    env=env, capture_output=True, text=True)
            return result.returncode, result.stdout.split(), result.stderr

    def other_packages(self, module, packages):
        code, args, stderr = self.run_shard("other", module, packages)
        self.assertEqual(code, 0, stderr)
        return [a.removeprefix(module + "/") for a in args]

    def test_board_and_other_split_the_current_layout(self):
        code, args, _ = self.run_shard("board")
        self.assertEqual((code, args), (0, ["./internal/board/..."]))
        self.assertEqual(self.other_packages("github.com/gosuda/flats", self.CURRENT),
                         ["cmd/flats", "internal/boardx", "internal/core", "internal/notebook", "internal/runtimex"])

    def test_notebook_runs_in_exactly_one_shard(self):
        hits = {}
        for shard in case_labels(SCRIPTS / "ci-test.sh"):
            code, args, stderr = self.run_shard(shard)
            self.assertEqual(code, 0, stderr)
            hits[shard] = [a for a in args if "internal/notebook" in a]
        self.assertEqual({s: h for s, h in hits.items() if h},
                         {"other": ["github.com/gosuda/flats/internal/notebook"]})

    def race_args(self, *args):
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / "go"
            stub.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
            stub.chmod(0o755)
            env = {**os.environ, "PATH": f"{directory}:{os.environ['PATH']}"}
            result = subprocess.run(["sh", str(SCRIPTS / "ci-race.sh"), *args],
                                    env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.splitlines()

    def test_split_race_lanes_complement_each_other(self):
        # The second lane skips exactly what the first runs, so every test of
        # the package runs in one of them, including new or renamed tests.
        for package, first, second in [("./internal/notebook/...", "notebook-1", "notebook-2"),
                                        ("./internal/core", "core-1", "core-2"),
                                        ("./internal/board", "board-1", "board-2")]:
            with self.subTest(package=package):
                a, b = self.race_args(first), self.race_args(second)
                self.assertEqual(a[-1], package)
                self.assertEqual(b[-1], package)
                self.assertEqual(a[a.index("-run") + 1], b[b.index("-skip") + 1])
                self.assertNotIn("-skip", a)
                self.assertNotIn("-run", b)

    def test_notebook_race_lanes_keep_the_long_timeout(self):
        for lane in ("notebook-1", "notebook-2"):
            with self.subTest(lane=lane):
                args = self.race_args(lane)
                self.assertEqual(args[:3], ["test", "-race", "-timeout=18m"])
                # go test keeps the last -timeout, so a caller's value overrides the default.
                self.assertIn("-timeout=30m", self.race_args(lane, "-timeout=30m")[3:])

    def test_notebook_large_lanes_enable_only_memory_tests(self):
        want = {
            "notebook-large-reader": [
                "^(TestReaderRetainedMemoryAndContinuations|TestReaderSelectionPreparationMemory|"
                "TestReaderCollectionQueryMemory|TestReaderHistoricalSourceMemory)$",
                "./internal/notebook"],
            "notebook-large-markdown": ["^TestMarkdownRetainedMemory$", "./internal/notebook"],
            "notebook-large-source": ["^TestNotebookSourceHistoryMemory$", "./internal/core"],
        }
        for lane, (selector, package) in want.items():
            with self.subTest(lane=lane), tempfile.TemporaryDirectory() as directory:
                stub = Path(directory) / "go"
                stub.write_text('#!/bin/sh\nprintf "%s\\n" "$NOTEBOOK_LARGE_MEMORY_TESTS" "$@"\n')
                stub.chmod(0o755)
                env = {**os.environ, "PATH": f"{directory}:{os.environ['PATH']}",
                       "NOTEBOOK_LARGE_MEMORY_TESTS": "0"}
                for script, group, flags in [
                    ("ci-race.sh", lane, ["-race"]),
                    ("ci-memory.sh", lane.removeprefix("notebook-large-"), []),
                ]:
                    result = subprocess.run(
                        ["sh", str(SCRIPTS / script), group, "-v"],
                        env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.splitlines(), [
                        "1", "test", *flags, "-v", "-count=1", "-timeout=25m", "-run",
                        selector, package,
                    ])

    def test_memory_dispatch_rejects_unknown_groups_and_propagates_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / "go"
            stub.write_text('#!/bin/sh\nexit 7\n')
            stub.chmod(0o755)
            env = {**os.environ, "PATH": f"{directory}:{os.environ['PATH']}"}
            for group in ("reader", "markdown", "source"):
                for script, name, code in [("ci-memory.sh", group, 7),
                                           ("ci-race.sh", "notebook-large-" + group, 1)]:
                    result = subprocess.run(["sh", str(SCRIPTS / script), name],
                                            env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode, code)
            for args in ([], ["unknown"]):
                result = subprocess.run(["sh", str(SCRIPTS / "ci-memory.sh"), *args],
                                        env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 2)

    def test_memory_job_runs_every_group_after_a_failure(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        job = ci.split("\n  notebook-memory:\n", 1)[1].split("\n  # Native macOS:", 1)[0]
        command = job.split("        run: |\n", 1)[1]
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            stub = work / "scripts/ci-memory.sh"
            stub.parent.mkdir()
            stub.write_text('#!/bin/sh\necho "$1"\n[ "$1" != "$FAIL_GROUP" ]\n')
            stub.chmod(0o755)
            for failed_group in ("reader", "markdown", "source", "none"):
                result = subprocess.run(["sh", "-c", command], cwd=work,
                                        env={**os.environ, "FAIL_GROUP": failed_group},
                                        capture_output=True, text=True)
                self.assertEqual(result.stdout.splitlines(), ["reader", "markdown", "source"])
                self.assertEqual(result.returncode, 0 if failed_group == "none" else 1)

    def test_layout_without_board(self):
        packages = [p for p in self.CURRENT if not p.startswith("internal/board/") and p != "internal/board"]
        code, args, stderr = self.run_shard("board", packages=packages)
        self.assertEqual((code, args), (0, []))
        self.assertIn("nothing to run", stderr)
        self.assertEqual(self.other_packages("github.com/gosuda/flats", packages),
                         ["cmd/flats", "internal/boardx", "internal/core", "internal/notebook", "internal/runtimex"])

    def test_older_module_path(self):
        module = "example.org/old/flats"
        self.assertEqual(self.other_packages(module, self.CURRENT),
                         ["cmd/flats", "internal/boardx", "internal/core", "internal/notebook", "internal/runtimex"])

    def test_missing_runtime_and_docs_run_nothing(self):
        packages = ["cmd/flats", "internal/core"]
        for shard in ("runtime-soak", "runtime-codec", "runtime", "docs-soak", "docs-budget", "docs"):
            with self.subTest(shard=shard):
                code, args, stderr = self.run_shard(shard, packages=packages)
                self.assertEqual((code, args), (0, []))
                self.assertIn("nothing to run", stderr)

    def test_other_refuses_an_empty_package_list(self):
        code, args, _ = self.run_shard("other", packages=["internal/runtime", "internal/board"])
        self.assertNotEqual(code, 0)
        self.assertEqual(args, [])


if __name__ == "__main__":
    unittest.main()
