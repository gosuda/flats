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


class ShardPackageTests(unittest.TestCase):
    """Run ci-test.sh with a stub go command on fixture source layouts."""

    CURRENT = ["cmd/flats", "internal/apps/docs", "internal/board", "internal/board/engine",
               "internal/board/benchdata/cmd/benchdata", "internal/boardx", "internal/core",
               "internal/runtime", "internal/runtimex"]

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
                         ["cmd/flats", "internal/boardx", "internal/core", "internal/runtimex"])

    def test_layout_without_board(self):
        packages = [p for p in self.CURRENT if not p.startswith("internal/board/") and p != "internal/board"]
        code, args, stderr = self.run_shard("board", packages=packages)
        self.assertEqual((code, args), (0, []))
        self.assertIn("nothing to run", stderr)
        self.assertEqual(self.other_packages("github.com/gosuda/flats", packages),
                         ["cmd/flats", "internal/boardx", "internal/core", "internal/runtimex"])

    def test_older_module_path(self):
        module = "example.org/old/flats"
        self.assertEqual(self.other_packages(module, self.CURRENT),
                         ["cmd/flats", "internal/boardx", "internal/core", "internal/runtimex"])

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
