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


def sheet_function(script):
    """The text of the sheet function of ci-test.sh, which runs the sheet shards."""
    return script[script.index("\nsheet() {"):script.index("\n}\n", script.index("\nsheet() {"))]


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

    def test_gates_count_the_shards_and_lanes_of_the_workflows(self):
        gates = (ROOT / "docs/gates.md").read_text()
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        release = one_line(r"^ +shard: \[(.+)\]$", ROOT / ".github/workflows/release.yml").split(", ")
        lanes = re.findall(r"^ +shards: (.+)$", ci, re.M)
        races = one_line(r"^ +for lane in (.+); do$", ROOT / ".github/workflows/ci.yml").split()
        words = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
                 "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen"]

        def stated(pattern):
            found = re.findall(pattern, " ".join(gates.split()))
            self.assertTrue(found, pattern)
            return [words.index(word) for word in found]

        self.assertEqual(stated(r"go test \./\.\.\. +# as (\w+) shards"), [len(self.shards)])
        self.assertEqual(stated(r"CI runs the shards in (\w+) jobs"), [len(lanes)])
        self.assertEqual(stated(r"go test -race, (\w+) lanes in one main/manual job"), [len(races)])
        self.assertEqual(stated(r"runs all (\w+) race lanes sequentially"), [len(races)])
        self.assertEqual(stated(r"Release tests retain all (\w+) shards"), [len(release)])
        self.assertEqual(stated(r"the caches written by the (\w+) main CI lanes"), [len(lanes)])

    def test_routine_and_scheduled_race_lanes_cover_every_lane_once(self):
        ci = ROOT / ".github/workflows/ci.yml"
        scheduled = ROOT / ".github/workflows/notebook-memory-race.yml"
        routine = one_line(r"^ +for lane in (.+); do$", ci).split()
        large = one_line(r"^ +lane: \[(.+)\]$", scheduled).split(", ")
        self.assertEqual(routine, ["core-1", "core-2", "services", "notebook-1", "notebook-2", "board-1", "board-2", "sheet-1", "sheet-2", "sheet-3", "sheet-4", "sheet-5"])
        groups = case_labels(SCRIPTS / "ci-memory.sh")
        self.assertEqual(large, ["notebook-large-" + group for group in groups])
        self.assertEqual(sorted(routine + large), sorted(case_labels(SCRIPTS / "ci-race.sh")))

    def test_large_memory_groups_run_on_main_and_manual_without_race(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        job = ci.split("\n  notebook-memory:\n", 1)[1].split("\n  # Native Windows", 1)[0]
        groups = re.search(r"for group in (.+); do", job).group(1).split()
        self.assertEqual(groups, case_labels(SCRIPTS / "ci-memory.sh"))
        self.assertIn('scripts/ci-memory.sh "$group" || failed=1', job)
        self.assertIn('exit "$failed"', job)
        self.assertNotIn("-race", job)
        self.assertIn("github.event_name != 'pull_request'", job)
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

    def test_windows_native_regressions_use_the_required_temp_directories(self) -> None:
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        windows = ci.split("\n  windows:\n", 1)[1].split("\n  # Native macOS", 1)[0]
        steps = windows.split("      - name: ")[1:]
        regressions = [step for step in steps if "run: go test " in step]
        self.assertEqual(len(regressions), 5)
        self.assertTrue(regressions[0].startswith("Native path regressions on the system drive\n"))
        self.assertIn("./internal/site\n", regressions[0])
        self.assertNotIn("env:", regressions[0])
        self.assertNotIn("    env:\n", windows.split("    steps:\n", 1)[0])
        for step in regressions[1:]:
            with self.subTest(step=step.splitlines()[0]):
                self.assertIn("          TMP: ${{ runner.temp }}\n", step)
                self.assertIn("          TEMP: ${{ runner.temp }}\n", step)

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

    def run_shard(self, shard, module="github.com/gosuda/flats", packages=None, extra=()):
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
            result = subprocess.run(["sh", str(SCRIPTS / "ci-test.sh"), shard, *extra], cwd=work,
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

    SHEET_LANES = ("sheet-1", "sheet-2", "sheet-3", "sheet-4", "sheet-5")

    def test_sheet_race_lanes_run_the_engine_and_its_packages(self):
        first, second, third, fourth, fifth = (self.race_args(lane) for lane in self.SHEET_LANES)
        timeout = ["test", "-race", "-timeout=15m"]
        # sheet-1 runs some undo tests, sheet-2 the other undo tests.
        self.assertEqual(first[:4], timeout + ["-run"])
        self.assertTrue(first[4].startswith("^TestUndo("), first[4])
        self.assertEqual(first[5:], ["./internal/sheet"])
        self.assertEqual(second, timeout + ["-run", "^TestUndo", "-skip", first[4], "./internal/sheet"])
        for lane in (third, fourth):
            self.assertEqual(lane[:4], timeout + ["-run"])
            self.assertEqual(lane[5:], ["./internal/sheet"])
        # The fifth lane skips what the others run, and takes the packages below the engine.
        self.assertEqual(fifth, timeout + ["-skip", "|".join(["^TestUndo", third[4], fourth[4]]), "./internal/sheet/..."])
        # go test keeps the last -timeout, so a caller's value overrides the default.
        for lane in self.SHEET_LANES:
            self.assertEqual(self.race_args(lane, "-timeout=30m")[2:4], ["-timeout=15m", "-timeout=30m"])
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        routine = re.search(r"^ +for lane in (.+); do$", ci, re.M).group(1).split()
        for lane in self.SHEET_LANES:
            self.assertIn(lane, routine)

    def test_sheet_race_lanes_run_each_test_once(self):
        # go test matches -run and -skip against each top-level test name
        # (the patterns have no slash), so every test function of the sheet
        # packages runs in exactly one lane, and each name of a pattern
        # names a test.
        lanes = {lane: self.race_args(lane) for lane in self.SHEET_LANES}

        def runs(args, package, name):
            if args[-1] != "./internal/sheet/..." and args[-1] != "./" + package:
                return False
            run = args[args.index("-run") + 1] if "-run" in args else ""
            skip = args[args.index("-skip") + 1] if "-skip" in args else None
            return bool(re.search(run, name)) and not (skip and re.search(skip, name))

        names = {}
        for path in (ROOT / "internal/sheet").rglob("*_test.go"):
            package = str(path.parent.relative_to(ROOT))
            for name in re.findall(r"^func ((?:Test|Fuzz)\w*)\(\w+ \*testing\.[TF]\) {", path.read_text(), re.M):
                names[(package, name)] = path
        self.assertGreater(len(names), 400)
        for (package, name), path in sorted(names.items()):
            with self.subTest(test=name, package=package):
                ran = [lane for lane, args in lanes.items() if runs(args, package, name)]
                self.assertEqual(len(ran), 1, f"{name} of {path.name} runs in {ran}")
        engine = {name for package, name in names if package == "internal/sheet"}
        for lane in ("sheet-1", "sheet-3"):
            pattern = lanes[lane][4]
            for part in re.fullmatch(r"\^(?:TestUndo)?\((.+)\)\$", pattern).group(1).split("|"):
                full = part if lane == "sheet-3" else "TestUndo" + part
                self.assertTrue(any(re.fullmatch(full, name) for name in engine), f"{part} of {lane} names no test")
        for part in re.fullmatch(r"\^Test\((.+)\)", lanes["sheet-4"][4]).group(1).split("|"):
            self.assertTrue(any(name.startswith("Test" + part) for name in engine), f"{part} of sheet-4 names no test")

    SHEET_SHARDS = ("sheet-undo-1", "sheet-undo-2", "sheet")

    def test_sheet_runs_in_the_sheet_shards_alone(self):
        packages = self.CURRENT + ["internal/sheet", "internal/sheet/formula"]
        hits = {}
        for shard in case_labels(SCRIPTS / "ci-test.sh"):
            code, args, stderr = self.run_shard(shard, packages=packages)
            self.assertEqual(code, 0, stderr)
            hits[shard] = [a for a in args if "internal/sheet" in a]
        self.assertEqual({s: h for s, h in hits.items() if h},
                         {"sheet-undo-1": ["./internal/sheet"], "sheet-undo-2": ["./internal/sheet"],
                          "sheet": ["./internal/sheet/..."]})
        # Older source without the sheet has nothing to run there.
        for shard in self.SHEET_SHARDS:
            code, args, stderr = self.run_shard(shard)
            self.assertEqual((code, args), (0, []))
            self.assertIn("nothing to run", stderr)

    def test_sheet_shards_split_the_package_by_test_name(self):
        # sheet-undo-1 runs some undo tests, sheet-undo-2 the other undo
        # tests and sheet everything else, so every test runs once, also a
        # new or renamed one.
        args = {}
        for shard in self.SHEET_SHARDS:
            code, args[shard], stderr = self.run_shard(shard, packages=self.CURRENT + ["internal/sheet"])
            self.assertEqual(code, 0, stderr)
        first, second, rest = (args[s] for s in self.SHEET_SHARDS)
        undo = second[second.index("-run") + 1]
        self.assertEqual(undo, "^TestUndo")
        self.assertEqual(rest[rest.index("-skip") + 1], undo)
        self.assertNotIn("-run", rest)
        self.assertEqual(first[first.index("-run") + 1], second[second.index("-skip") + 1])
        self.assertNotIn("-skip", first)
        named = re.fullmatch(r"\^\((.+)\)\$", first[first.index("-run") + 1]).group(1).split("|")
        self.assertTrue(named)
        for name in named:
            self.assertTrue(name.startswith("TestUndo"), name)
        sources = "".join(p.read_text() for p in (ROOT / "internal/sheet").glob("*_test.go"))
        for name in named:
            self.assertIn(f"\nfunc {name}(t *testing.T) {{", sources, f"{name} is no test of internal/sheet")

    def test_sheet_shards_keep_their_timeout(self):
        packages = self.CURRENT + ["internal/sheet"]
        for shard, timeout in (("sheet-undo-1", "14m"), ("sheet-undo-2", "14m"), ("sheet", "17m")):
            with self.subTest(shard=shard):
                code, defaults, stderr = self.run_shard(shard, packages=packages)
                self.assertEqual(code, 0, stderr)
                self.assertEqual(defaults[:1], [f"-timeout={timeout}"])
                code, args, stderr = self.run_shard(shard, packages=packages, extra=["-timeout=30m"])
                self.assertEqual(code, 0, stderr)
                # go test keeps the last -timeout, so a caller's value overrides the default.
                self.assertEqual(args, [defaults[0], "-timeout=30m", *defaults[1:]])
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        job = ci.split("\n  test:\n", 1)[1].split("\n\n  ", 1)[0]
        self.assertIn("timeout-minutes: 20", job)
        for shard in self.SHEET_SHARDS:
            self.assertIn(f"          - lane: {shard}\n            shards: {shard}\n", ci)

    def test_sheet_full_size_cases_run_in_the_sheet_shards_alone(self):
        script = (SCRIPTS / "ci-test.sh").read_text()
        self.assertEqual(script.count("FLATS_SHEET_FULL=${FLATS_SHEET_FULL-1}"), 1)
        self.assertEqual(script.count("export FLATS_SHEET_FULL"), 1)
        self.assertIn("export FLATS_SHEET_FULL", sheet_function(script))
        other = script[script.index("\nother)"):script.index("\n*)")]
        self.assertNotIn("FLATS_SHEET_FULL", other)
        self.assertNotIn("sheet ", other)
        for name in ("ci-race.sh", "ci-memory.sh"):
            self.assertNotIn("FLATS_SHEET_FULL", (SCRIPTS / name).read_text(), name)
        self.assertNotIn("FLATS_SHEET_FULL", (ROOT / ".github/workflows/ci.yml").read_text())

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
        job = ci.split("\n  notebook-memory:\n", 1)[1].split("\n  # Native Windows", 1)[0]
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

    def test_race_job_runs_every_lane_after_a_failure(self) -> None:
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        job = ci.split("\n  race:\n", 1)[1].split("\n  notebook-memory:\n", 1)[0]
        command = job.split("        run: |\n", 1)[1]
        lanes = ["core-1", "core-2", "services", "notebook-1", "notebook-2", "board-1", "board-2",
                 "sheet-1", "sheet-2", "sheet-3", "sheet-4", "sheet-5"]
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            stub = work / "scripts/ci-race.sh"
            stub.parent.mkdir()
            stub.write_text('#!/bin/sh\necho "$1"\n[ "$1" != "$FAIL_LANE" ]\n')
            stub.chmod(0o755)
            for failed_lane in (*lanes, "none"):
                with self.subTest(failed_lane=failed_lane):
                    result = subprocess.run(
                        ["sh", "-e", "-c", command], cwd=work,
                        env={**os.environ, "FAIL_LANE": failed_lane},
                        capture_output=True, text=True,
                    )
                    self.assertEqual(result.stdout.splitlines(), lanes)
                    self.assertEqual(result.returncode, 0 if failed_lane == "none" else 1)

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


class SheetVariableTests(unittest.TestCase):
    """Account for every environment variable a sheet test reads.

    A test that runs only with a variable is skipped wherever the variable is
    not set, without a failure. docs/gates.md therefore lists each variable
    with the CI lane that sets it, or as a manual run with its command, and
    these tests keep the list, the test sources and the CI files in step.
    """

    HEADING = "\n### Sheet tests behind an environment variable\n"
    NAME = r"[A-Z][A-Z0-9_]*"
    # Where CI sets a variable of a sheet test, as the table writes it, and
    # the text of the CI files that the place stands for.
    PLACES = ("the sheet shards of `scripts/ci-test.sh`", "the `sheet-browser` job",
              "every job, set by the runner")

    def section(self):
        gates = (ROOT / "docs/gates.md").read_text()
        self.assertEqual(gates.count(self.HEADING), 1, "docs/gates.md lists the sheet variables once")
        return re.split(r"\n##+ ", gates.split(self.HEADING, 1)[1], maxsplit=1)[0]

    def rows(self):
        """Variable -> (CI or Manual, where), from the table of docs/gates.md."""
        rows = {}
        for line in self.section().splitlines():
            if not line.startswith("| `"):
                continue
            cells = [cell.strip() for cell in line.strip("|").split("|")]
            self.assertEqual(len(cells), 3, line)
            kind, _, where = cells[2].partition(": ")
            self.assertIn(kind, ("CI", "Manual"), line)
            for name in re.findall(rf"`({self.NAME})`", cells[0]):
                self.assertNotIn(name, rows, f"{name} has two rows")
                rows[name] = (kind, where.rstrip("."))
        return rows

    def read_by_tests(self):
        names = {}
        sources = [*(ROOT / "internal/sheet").rglob("*_test.go"), *(ROOT / "internal/core").glob("sheet*_test.go")]
        self.assertGreater(len(sources), 40)
        for path in sources:
            for name in re.findall(rf'os\.(?:Getenv|LookupEnv)\("({self.NAME})"\)', path.read_text()):
                names.setdefault(name, path.name)
        scripts = [*(ROOT / "internal/sheet/web").rglob("*.mjs"), *(ROOT / "internal/sheet/web").rglob("*.js")]
        self.assertGreater(len(scripts), 5)
        for path in scripts:
            for name in re.findall(rf"process\.env\.({self.NAME})", path.read_text()):
                names.setdefault(name, path.name)
        return names

    def set_by_ci(self):
        """Variable -> the place of PLACES where a CI file sets it."""
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        script = (SCRIPTS / "ci-test.sh").read_text()
        other = sheet_function(script)
        browser = ci.split("\n  sheet-browser:\n", 1)[1].split("\n\n", 1)[0]
        found = {"CI": self.PLACES[2]}
        for place, text in ((self.PLACES[0], other), (self.PLACES[1], browser)):
            for name in re.findall(rf"\b({self.NAME})=", text):
                if name.startswith("FLATS_SHEET_") or name == "CHROME":
                    self.assertNotIn(name, found, f"{name} is set in two lanes")
                    found[name] = place
        # No other CI file sets a variable of a sheet test.
        rest = script.replace(other, "") + ci.replace(browser, "")
        for path in [*SCRIPTS.glob("ci-*.sh"), *SCRIPTS.glob("build-*.sh"), *(ROOT / ".github/workflows").glob("*.yml")]:
            if path.name not in ("ci-test.sh", "ci.yml"):
                rest += path.read_text()
        self.assertEqual(re.findall(r"\b(FLATS_SHEET_[A-Z0-9_]*|CHROME)=", rest), [])
        return found

    def test_every_variable_a_sheet_test_reads_has_a_row(self):
        read, rows = self.read_by_tests(), self.rows()
        for name, source in sorted(read.items()):
            self.assertIn(name, rows, f"{source} reads {name}: add it to the sheet table of docs/gates.md, "
                          "with the CI lane that sets it or the command of its manual run")
        for name in sorted(rows):
            self.assertIn(name, read, f"no sheet test reads {name} any more: remove its row from docs/gates.md")
        # The full-size setting and the browser suites are in the list.
        self.assertIn("FLATS_SHEET_FULL", read)
        self.assertIn("FLATS_SHEET_E2E", read)

    def test_ci_rows_name_the_lane_that_sets_the_variable(self):
        rows, found = self.rows(), self.set_by_ci()
        listed = {name: where for name, (kind, where) in rows.items() if kind == "CI"}
        self.assertEqual(listed, found)

    def test_manual_rows_come_with_a_command(self):
        section = self.section()
        commands = "\n".join(re.findall(r"```sh\n(.*?)```", section, re.S))
        manual = [name for name, (kind, _) in self.rows().items() if kind == "Manual"]
        self.assertTrue(manual)
        for name in manual:
            with self.subTest(variable=name):
                self.assertRegex(commands, rf"\b{name}=\S+ .*go test ")

    def test_limits_fixture_runs_with_the_full_size_cases(self):
        source = (ROOT / "internal/sheet/bench_test.go").read_text()
        body = source.split("\nfunc TestLimitsFixture(t *testing.T) {\n", 1)[1].split("\n}\n", 1)[0]
        self.assertTrue(body.startswith("\tfull(t, "), "the limits fixture is a full-size case")
        self.assertNotIn("os.Getenv", body)
        self.assertIn("`TestLimitsFixture`", self.section())

    def test_browser_job_fails_without_a_browser(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        job = ci.split("\n  sheet-browser:\n", 1)[1].split("\n\n", 1)[0]
        # The suites skip when they find no browser. The job must not pass then.
        self.assertRegex(job, r"CHROME=\$\(command -v google-chrome\) \|\| \{[^}]*exit 1; \}")
        self.assertIn("export CHROME", job)
        self.assertRegex(job, r"FLATS_SHEET_E2E=1 go test -count=1 [^\n]*-run '\^TestBrowser' \./internal/sheet\Z")
        names = set()
        for path in (ROOT / "internal/sheet").glob("*_test.go"):
            text = path.read_text()
            functions = re.split(r"(?m)^func (\w+)\(t \*testing\.T\) \{", text)
            for name, body in zip(functions[1::2], functions[2::2]):
                if 'os.Getenv("FLATS_SHEET_E2E")' in body:
                    names.add(name)
        self.assertEqual(sorted(names), ["TestBrowser", "TestBrowserEscapedTextTab", "TestBrowserLargeFormatDelta", "TestBrowserScreenState"])
        for name in names:
            self.assertIn(f"`{name}`", self.section())


if __name__ == "__main__":
    unittest.main()
