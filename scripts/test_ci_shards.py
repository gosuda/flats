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

    def test_ci_runs_every_race_lane(self):
        ci = ROOT / ".github/workflows/ci.yml"
        lanes = one_line(r"^ +lane: \[(.+)\]$", ci)
        self.assertEqual(sorted(s.strip() for s in lanes.split(",")),
                         sorted(case_labels(SCRIPTS / "ci-race.sh")))

    def test_large_reader_lane_includes_directly_flagged_tests(self):
        script = (SCRIPTS / "ci-race.sh").read_text()
        patterns = re.findall(r"^\s*large_(?:reader|markdown)='(.+)'$", script, re.M)
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

    def test_large_reader_race_lane_has_its_own_timeout(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        race = ci.split("\n  race:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertEqual(re.findall(r"^    timeout-minutes: (.+)$", race, re.M),
                         ["${{ startsWith(matrix.lane, 'notebook-large') && 30 || 20 }}"])

    def test_large_lanes_run_one_command_each(self):
        script = (SCRIPTS / "ci-race.sh").read_text()
        for lane in ("notebook-large-reader", "notebook-large-markdown", "notebook-large-source"):
            with self.subTest(lane=lane):
                body = script.split(f"\n{lane})\n", 1)[1].split("\n\t;;", 1)[0]
                self.assertEqual(len(re.findall(r"^\trun ", body, re.M)), 1)
                self.assertIn("-timeout=25m", body)


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

    def test_notebook_race_lane_runs_the_whole_package(self):
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / "go"
            stub.write_text('#!/bin/sh\necho "$@"\n')
            stub.chmod(0o755)
            env = {**os.environ, "PATH": f"{directory}:{os.environ['PATH']}"}
            result = subprocess.run(["sh", str(SCRIPTS / "ci-race.sh"), "notebook"],
                                    env=env, capture_output=True, text=True)
        self.assertEqual((result.returncode, result.stdout.split()),
                         (0, ["test", "-race", "./internal/notebook/..."]))

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
                result = subprocess.run(
                    ["sh", str(SCRIPTS / "ci-race.sh"), lane, "-v"],
                    env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.splitlines(), [
                    "1", "test", "-race", "-v", "-count=1", "-timeout=25m", "-run",
                    selector, package,
                ])

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
