"""Check cache permissions and connect read-only jobs to current CI writers."""

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / ".github/actions/setup-go/metadata.sh"


def required_match(pattern: str, text: str) -> re.Match[str]:
    match = re.search(pattern, text, re.MULTILINE)
    if match is None:
        raise AssertionError(f"no match for {pattern}")
    return match


def workflow_job(workflow: str, name: str) -> str:
    text = (ROOT / ".github/workflows" / workflow).read_text()
    return required_match(
        rf"(?s)^  {re.escape(name)}:\n.*?(?=^  [\w-]+:\n|\Z)", text
    ).group()


def setup_input(job: str, name: str) -> str:
    match = re.search(rf"^ +{re.escape(name)}: (.+)$", job, re.MULTILINE)
    if match is None:
        action = (SCRIPT.parent / "action.yml").read_text()
        definition = required_match(
            rf"(?s)^  {re.escape(name)}:\n.*?(?=^  [\w-]+:\n|^runs:)", action
        )
        match = required_match(r"^    default: (.+)$", definition.group())
    return match.group(1).strip("\"'")


def matrix_value(template: str, row: dict[str, str]) -> str:
    def replace(match: re.Match[str]) -> str:
        for field in match.group(1).split(" || "):
            if value := row.get(field.removeprefix("matrix."), ""):
                return value
        raise AssertionError(f"no value for {match.group()} in {row}")

    return re.sub(r"\$\{\{ (matrix\.\w+(?: \|\| matrix\.\w+)*) \}\}", replace, template)


def race_packages(lane: str) -> set[str]:
    with tempfile.TemporaryDirectory() as directory:
        go = Path(directory) / "go"
        go.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
        go.chmod(0o755)
        result = subprocess.run(
            ["sh", str(ROOT / "scripts/ci-race.sh"), lane],
            env={**os.environ, "PATH": f"{directory}:{os.environ['PATH']}"},
            capture_output=True,
            text=True,
            check=True,
        )
    return {
        arg.removesuffix("/...")
        for arg in result.stdout.splitlines()
        if arg.startswith("./")
    }


class CacheMetadataTests(unittest.TestCase):
    def metadata(
        self,
        scope: str = "root",
        event: str = "push",
        ref: str = "refs/heads/main",
        root_hash: str = "root-deps",
    ) -> tuple[subprocess.CompletedProcess[str], dict[str, str]]:
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            go = work / "go"
            go.write_text("""#!/bin/sh
case "$2" in
GOVERSION) echo go1.27.1 ;;
GOCACHE) echo /fixture/build ;;
GOMODCACHE) echo /fixture/modules ;;
*) exit 1 ;;
esac
""")
            go.chmod(0o755)
            output = work / "output"
            output.touch()
            env = {
                **os.environ,
                "PATH": f"{work}:{os.environ['PATH']}",
                "MODULE_SCOPE": scope,
                "ROOT_HASH": root_hash,
                "NETBIRD_HASH": "both-module-deps",
                "GITHUB_OUTPUT": str(output),
                "GITHUB_EVENT_NAME": event,
                "GITHUB_REF": ref,
            }
            result = subprocess.run(
                ["bash", str(SCRIPT)],
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            values = dict(
                line.split("=", 1) for line in output.read_text().splitlines()
            )
            return result, values

    def test_root_and_nested_dependency_scopes(self) -> None:
        for scope, expected in [("root", "root-deps"), ("netbird", "both-module-deps")]:
            with self.subTest(scope=scope):
                result, values = self.metadata(scope=scope)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(values["hash"], expected)
                self.assertEqual(values["toolchain"], "go1.27.1")
                self.assertRegex(values["week"], r"^\d{4}-\d{2}$")
                self.assertEqual(values["save"], "true")

    def test_non_main_push_events_never_write_cache(self) -> None:
        for event, ref in [
            ("pull_request", "refs/pull/1/merge"),
            ("workflow_dispatch", "refs/heads/main"),
            ("push", "refs/tags/v1.0.0"),
            ("push", "refs/heads/topic"),
        ]:
            with self.subTest(event=event, ref=ref):
                result, values = self.metadata(event=event, ref=ref)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn("save", values)

    def test_invalid_scope_and_missing_manifests_fail(self) -> None:
        for kwargs in [{"scope": "unknown"}, {"root_hash": ""}]:
            result, values = self.metadata(**kwargs)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn("save", values)


class CacheReaderTests(unittest.TestCase):
    def test_release_shards_restore_their_ci_lane_cache(self) -> None:
        ci = workflow_job("ci.yml", "test")
        cache = setup_input(ci, "cache-key")
        scope = setup_input(ci, "module-scope")
        self.assertEqual(setup_input(ci, "save-build"), "true")
        writers = {
            shard: matrix_value(cache, {"lane": lane})
            for lane, shards in re.findall(
                r"^ +- lane: ([\w-]+)\n +shards: (.+)$", ci, re.MULTILINE
            )
            for shard in shards.split()
        }
        self.assertTrue(writers)

        release = workflow_job("release.yml", "test")
        shards = required_match(r"^ +shard: \[(.+)\]$", release)
        aliases = dict(
            re.findall(
                r"^ +- \{shard: ([\w-]+), cache: ([\w-]+)\}$", release, re.MULTILINE
            )
        )
        keys = re.findall(r"^ +(?:key: )?(go-build-.+)$", release, re.MULTILINE)
        self.assertEqual(len(keys), 3, "check the exact key and both restore prefixes")
        for shard in shards.group(1).split(", "):
            with self.subTest(shard=shard):
                row = {"shard": shard, "cache": aliases.get(shard, "")}
                self.assertIn(shard, writers)
                for key in keys:
                    self.assertIn(
                        f"-{writers[shard]}-{scope}--", matrix_value(key, row)
                    )
        self.assertNotIn("uses: actions/cache@", release)
        self.assertNotIn("uses: ./.github/actions/setup-go", release)

    def test_memory_races_read_current_writers_for_the_same_packages(self) -> None:
        ci = workflow_job("ci.yml", "race")
        cache = setup_input(ci, "cache-key")
        scope = setup_input(ci, "module-scope")
        excluded = required_match(
            r"^\$\{\{ !endsWith\(matrix.lane, '([^']+)'\) \}\}$",
            setup_input(ci, "save-build"),
        )
        writers = {
            (
                matrix_value(cache, {"lane": lane, "cache": cohort}),
                scope,
            ): race_packages(lane)
            for lane, cohort in re.findall(
                r"^ +- \{lane: ([\w-]+), cache: ([\w-]+)\}$", ci, re.MULTILINE
            )
            if not lane.endswith(excluded.group(1))
        }
        self.assertTrue(writers)

        scheduled = workflow_job("notebook-memory-race.yml", "race")
        cache = setup_input(scheduled, "cache-key")
        scope = setup_input(scheduled, "module-scope")
        lanes = required_match(r"^ +lane: \[(.+)\]$", scheduled)
        aliases = dict(
            re.findall(
                r"^ +- \{lane: ([\w-]+), cache: ([\w-]+)\}$", scheduled, re.MULTILINE
            )
        )
        for lane in lanes.group(1).split(", "):
            with self.subTest(lane=lane):
                key = (
                    matrix_value(cache, {"lane": lane, "cache": aliases.get(lane, "")}),
                    scope,
                )
                self.assertIn(key, writers)
                packages = race_packages(lane)
                self.assertTrue(packages)
                self.assertLessEqual(packages, writers[key])
        self.assertEqual(setup_input(scheduled, "save-build"), "false")


if __name__ == "__main__":
    unittest.main()
