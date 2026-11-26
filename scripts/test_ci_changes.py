"""Exercise CI selection against real Git merge trees and Windows packages."""

import ast
import os
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest
from collections.abc import Generator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/ci-changes.py"


def condition_result(expression: str, values: Mapping[str, str | bool]) -> bool:
    """Evaluate only the boolean operations used by these workflow guards."""
    expression = expression.removeprefix("$" + "{{").removesuffix("}}").strip()
    for name, value in values.items():
        expression = expression.replace(name, repr(value))
    expression = expression.replace("&&", " and ").replace("||", " or ")
    expression = re.sub(r"!(?!=)", "not ", expression).strip()

    def evaluate(node: ast.AST) -> str | bool:
        if isinstance(node, ast.Constant) and type(node.value) in (str, bool):
            return node.value
        if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
            return not evaluate(node.operand)
        if isinstance(node, ast.BoolOp):
            if isinstance(node.op, ast.And):
                return all(evaluate(item) for item in node.values)
            if isinstance(node.op, ast.Or):
                return any(evaluate(item) for item in node.values)
        if isinstance(node, ast.Compare) and len(node.ops) == 1:
            left, right = evaluate(node.left), evaluate(node.comparators[0])
            if type(left) is not type(right):
                raise AssertionError("mixed-type comparison in a workflow guard")
            # GitHub compares strings without case; selector output is normalized.
            if isinstance(left, str) and isinstance(right, str):
                left, right = left.casefold(), right.casefold()
            if isinstance(node.ops[0], ast.Eq):
                return left == right
            if isinstance(node.ops[0], ast.NotEq):
                return left != right
        raise AssertionError(f"unsupported workflow guard: {ast.dump(node)}")

    return bool(evaluate(ast.parse(expression, mode="eval").body))


@dataclass(frozen=True)
class Repository:
    root: Path

    def git(self, *args: str) -> str:
        return subprocess.run(
            ["git", "-C", str(self.root), *args],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()

    def write(self, name: str, text: str = "# Document\n") -> Path:
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
        return path

    def commit(self) -> str:
        self.git("add", "-A")
        self.git(
            "-c", "commit.gpgsign=false", "commit", "-qm", "fixture", "--allow-empty"
        )
        return self.git("rev-parse", "HEAD")

    def merge(self) -> dict[str, str]:
        head = self.git("rev-parse", "HEAD")
        self.git("checkout", "-q", "base")
        base = self.git("rev-parse", "HEAD")
        self.git("-c", "commit.gpgsign=false", "merge", "--no-ff", "-qm", "merge", head)
        return {"PR_BASE_SHA": base, "PR_HEAD_SHA": head}


@contextmanager
def repository() -> Generator[Repository]:
    with tempfile.TemporaryDirectory() as directory:
        repo = Repository(Path(directory))
        repo.git("init", "-q", "-b", "base")
        repo.git("config", "user.name", "CI fixture")
        repo.git("config", "user.email", "ci@example.invalid")
        repo.write("docs/spec/existing.md")
        repo.write("internal/example.go", "package example\n")
        repo.commit()
        repo.git("checkout", "-qb", "pr")
        yield repo


def select(
    repo: Repository,
    context: Mapping[str, str],
    *,
    event: str = "pull_request",
    extra_env: Mapping[str, str] | None = None,
) -> tuple[subprocess.CompletedProcess[str], str]:
    # Keep action outputs out of the fixture's commits.
    with tempfile.TemporaryDirectory() as directory:
        output = Path(directory) / "output"
        env = {
            **os.environ,
            **context,
            "GITHUB_EVENT_NAME": event,
            "GITHUB_OUTPUT": str(output),
            **(extra_env or {}),
        }
        result = subprocess.run(
            [sys.executable, "-B", str(SCRIPT)],
            cwd=repo.root,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
        return result, output.read_text() if output.exists() else ""


class ChangeSelectionTests(unittest.TestCase):
    def assert_selection(self, repo: Repository, *, full: bool) -> None:
        repo.commit()
        result, output = select(repo, repo.merge())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, f"full={str(full).lower()}\n")

    def test_prose_additions_edits_and_deletions(self) -> None:
        with repository() as repo:
            repo.write("docs/spec/existing.md", "# Revised\n")
            repo.write("docs/spec/README.md")
            repo.write("docs/research/new-findings.md")
            self.assert_selection(repo, full=False)
        with repository() as repo:
            (repo.root / "docs/spec/existing.md").unlink()
            self.assert_selection(repo, full=False)

    def test_non_prose_paths_and_nonportable_names_keep_full_ci(self) -> None:
        paths = (
            "internal/example.go",
            "go.mod",
            ".github/workflows/ci.yml",
            "docs/agent/topics/runtime.md",
            "docs/runtime-api-v1.md",
            "docs/content-types.md",
            "docs/README.md",
            "README.md",
            "plugins/flats/skills/flats-deploy/SKILL.md",
            ".agents/skills/x/SKILL.md",
            "docs/spec/nested/notes.md",
            "docs/specs/notes.md",
            "docs/spec/notes.go",
            "docs/spec/notes.MD",
            "docs/spec/Notes.md",
            "docs/spec/readme.md",
            "docs/spec/con.md",
            "docs/spec/CON.md",
            "docs/spec/lpt1.md",
            "docs/spec/com9.md",
            "docs/spec/a:b.md",
            "docs/spec/two words.md",
            "docs/spec/line\nfull=false.md",
            "docs/spec/.hidden.md",
            "docs/spec/-flag.md",
        )
        for path in paths:
            with self.subTest(path=path), repository() as repo:
                repo.write(path, "changed\n")
                self.assert_selection(repo, full=True)

    def test_mixed_and_empty_changes_keep_full_ci(self) -> None:
        with repository() as repo:
            repo.write("docs/spec/new.md")
            repo.write("internal/example.go", "package changed\n")
            self.assert_selection(repo, full=True)
        with repository() as repo:
            self.assert_selection(repo, full=True)

    def test_renames_include_both_paths(self) -> None:
        cases = (
            ("internal/example.go", "docs/spec/moved.md", True),
            ("docs/spec/existing.md", "internal/moved.go", True),
            ("docs/spec/existing.md", "docs/research/moved.md", False),
        )
        for old, new, full in cases:
            with self.subTest(old=old, new=new), repository() as repo:
                destination = repo.root / new
                destination.parent.mkdir(parents=True, exist_ok=True)
                (repo.root / old).rename(destination)
                self.assert_selection(repo, full=full)

    def test_symlinks_and_executable_modes_keep_full_ci(self) -> None:
        for mode in ("symlink", "executable"):
            for change in ("add", "delete", "type"):
                with self.subTest(mode=mode, change=change), repository() as repo:
                    if change != "add":
                        repo.git("checkout", "-q", "base")
                    path = repo.write("docs/spec/special.md")
                    if mode == "symlink":
                        path.unlink()
                        path.symlink_to("existing.md")
                    else:
                        path.chmod(0o755)
                    if change != "add":
                        repo.commit()
                        repo.git("checkout", "-q", "pr")
                        repo.git("merge", "--ff-only", "base")
                        path.unlink()
                        if change == "type":
                            repo.write("docs/spec/special.md")
                    self.assert_selection(repo, full=True)

    def test_gitlink_is_not_a_regular_document(self) -> None:
        with repository() as repo:
            sha = repo.git("rev-parse", "HEAD")
            repo.git(
                "update-index",
                "--add",
                "--cacheinfo",
                f"160000,{sha},docs/spec/module.md",
            )
            repo.git("-c", "commit.gpgsign=false", "commit", "-qm", "gitlink")
            result, output = select(repo, repo.merge())
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output, "full=true\n")

    def test_diff_is_not_limited_to_a_hosted_file_list(self) -> None:
        with repository() as repo:
            for index in range(3101):
                repo.write(f"docs/spec/file-{index}.md")
            # Sorts after the documents, beyond hosted file-list limits.
            repo.write("internal/example.go", "package changed\n")
            self.assert_selection(repo, full=True)

    def test_base_branch_code_changes_are_not_pr_changes(self) -> None:
        with repository() as repo:
            repo.write("docs/spec/proposal.md")
            repo.commit()
            repo.git("checkout", "-q", "base")
            repo.write("internal/base-only.go", "package example\n")
            repo.commit()
            repo.git("checkout", "-q", "pr")
            result, output = select(repo, repo.merge())
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output, "full=false\n")

    def test_shallow_merge_checkout(self) -> None:
        for depth in (1, 2):
            with self.subTest(depth=depth), repository() as repo:
                repo.write("docs/spec/new.md")
                repo.commit()
                context = repo.merge()
                with tempfile.TemporaryDirectory() as directory:
                    clone = Repository(Path(directory) / "clone")
                    repo.git(
                        "clone",
                        "-q",
                        "--depth",
                        str(depth),
                        repo.root.as_uri(),
                        str(clone.root),
                    )
                    result, output = select(clone, context)
                    if depth == 1:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertEqual(output, "")
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(output, "full=false\n")

    def test_wrong_merge_context_never_emits_a_selection(self) -> None:
        with repository() as repo:
            sha = repo.git("rev-parse", "HEAD")
            result, output = select(repo, {"PR_BASE_SHA": sha, "PR_HEAD_SHA": sha})
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(output, "")
            repo.write("docs/spec/new.md")
            repo.commit()
            context = repo.merge()
            for key in ("PR_BASE_SHA", "PR_HEAD_SHA"):
                result, output = select(repo, {**context, key: "0" * 40})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, "")

    def test_git_errors_and_malformed_output_never_emit_a_selection(self) -> None:
        for raw, code in ((b"incomplete", 0), (b"bad\0path\0", 0), (b"", 7)):
            with self.subTest(raw=raw, code=code), repository() as repo:
                stub = repo.root / "bin/git"
                stub.parent.mkdir()
                stub.write_text(
                    f"#!{sys.executable}\nimport os, sys\n"
                    "if sys.argv[1] == 'show':\n"
                    "    print(os.environ['PR_BASE_SHA'], os.environ['PR_HEAD_SHA'])\n"
                    "else:\n"
                    f"    sys.stdout.buffer.write({raw!r})\n"
                    f"    sys.exit({code})\n",
                    encoding="utf-8",
                )
                stub.chmod(0o755)
                result, output = select(
                    repo,
                    {"PR_BASE_SHA": "1" * 40, "PR_HEAD_SHA": "2" * 40},
                    extra_env={"PATH": str(stub.parent)},
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, "")

    def test_other_events_never_select_documentation_only(self) -> None:
        with repository() as repo:
            for event in ("push", "schedule", "workflow_dispatch", "future_event"):
                with self.subTest(event=event):
                    result, output = select(repo, {}, event=event)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(output, "full=true\n")


class WindowsPackageTests(unittest.TestCase):
    def test_windows_keeps_native_regressions_and_running_host_backup(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        job = workflow.split("\n  windows:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertIn("go test -v -count=1 ./internal/site", job)
        self.assertIn("go test -v -count=1 ./internal/config", job)
        self.assertIn("go test -v -count=1 -timeout=30m ./internal/sheet/...", job)
        self.assertIn("timeout-minutes: 75", job)
        commands = dict(re.findall(
            r"go test -v -count=1 \./internal/(\w+) -run '([^']+)'", job
        ))
        for package in ("core", "backup"):
            functions = {
                name
                for path in (ROOT / "internal" / package).glob("*_windows_test.go")
                for name in re.findall(r"^func (Test\w+)\(", path.read_text(), re.M)
            }
            self.assertTrue(functions, package)
            for name in functions:
                with self.subTest(package=package, test=name):
                    self.assertIsNotNone(re.fullmatch(commands[package], name))
        self.assertIsNotNone(re.fullmatch(
            commands["app"], "TestBackupOfARunningHostRestoresToAWorkingHost"
        ))
        self.assertNotIn("scripts/ci-test.sh", job)
        self.assertNotIn("go list ./...", job)

    def test_windows_builds_its_archive_in_the_same_attempt(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        container = workflow.split("\n  container:\n", 1)[1].split("\n  commit-images:\n", 1)[0]
        windows = workflow.split("\n  windows:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertNotIn("windows/amd64", container)
        self.assertIn('FLATS_RELEASE_TARGETS="windows/amd64"', windows)
        self.assertIn('scripts/build-release.sh v0.0.0-ci "$RUNNER_TEMP/dist"', windows)
        self.assertNotIn("download-artifact", windows)
        self.assertNotIn("github.run_attempt", windows)

    def test_windows_results_do_not_depend_on_linux_or_each_other(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        windows = workflow.split("\n  windows:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertIn("needs: changes", windows)
        self.assertNotIn("needs.container", windows)
        self.assertNotIn("needs.netbird", windows)
        self.assertEqual(windows.count("!cancelled() && steps.setup.outcome == 'success'"), 6)
        self.assertIn("!cancelled() && steps.archive.outcome == 'success'", windows)

    def test_only_successful_explicit_prose_selection_skips_full_pr_ci(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        for job_name in ("legacy-build", "docs-web", "test", "netbird", "sheet-browser", "windows", "install-macos", "container"):
            match = re.search(rf"(?ms)^  {job_name}:\n.*?(?=^  [\w-]+:\n|\Z)", workflow)
            assert match is not None
            condition = re.search(r"^    if: (.+)$", match.group(), re.M)
            assert condition is not None
            self.assertIn("!cancelled()", condition.group())
            self.assertIn("needs.changes.result != 'success'", condition.group())
            self.assertIn("needs.changes.outputs.full != 'false'", condition.group())
        docs = workflow.split("\n  docs:\n", 1)[1].split("\n  docs-web:\n", 1)[0]
        self.assertIn("needs.changes.result == 'success'", docs)
        self.assertIn("needs.changes.outputs.full == 'false'", docs)

    def test_main_and_manual_runs_never_replace_each_others_publication(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        group = re.search(r"^  group: (.+)$", workflow, re.M)
        assert group is not None
        self.assertIn("github.event_name == 'pull_request'", group.group())
        self.assertIn("format('pr-{0}', github.event.pull_request.number)", group.group())
        self.assertIn("format('{0}-{1}', github.event_name, github.run_id)", group.group())


class JobSelectionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        self.jobs = {
            match.group(1): match.group()
            for match in re.finditer(
                r"(?ms)^  ([\w-]+):\n.*?(?=^  [\w-]+:\n|\Z)",
                self.workflow.split("\njobs:\n", 1)[1],
            )
        }

    def test_validation_job_sets_cover_events_and_selector_failures(self) -> None:
        full = {"legacy-build", "docs-web", "test", "netbird", "sheet-browser",
                "windows", "install-macos", "container"}
        heavy = {"race", "notebook-memory"}
        validation = set(self.jobs) - {"changes", "commit-images"}
        self.assertEqual(validation, full | heavy | {"docs"})
        cases = [
            ("pull_request", result, output,
             {"docs"} if result == "success" and output == "false" else full)
            for result in ("success", "failure", "skipped", "cancelled")
            for output in ("true", "false", "", "invalid")
        ]
        cases += [
            (event, "skipped", "", (full - {"legacy-build"}) | heavy)
            for event in ("push", "workflow_dispatch")
        ]
        for event, result, output, expected in cases:
            for cancelled in (False, True):
                with self.subTest(event=event, result=result, output=output, cancelled=cancelled):
                    values = {
                        "github.event_name": event,
                        "needs.changes.result": result,
                        "needs.changes.outputs.full": output,
                        "cancelled()": cancelled,
                    }
                    selected = {
                        name for name in validation
                        if condition_result(
                            re.search(r"^    if: (.+)$", self.jobs[name], re.M).group(1), values
                        )
                    }
                    self.assertEqual(selected, set() if cancelled else expected)
        triggers = self.workflow.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertIn("  workflow_dispatch:\n", triggers)
        self.assertIn("if: github.event_name == 'pull_request'", self.jobs["changes"])
        self.assertIn("cancel-in-progress: " + "$" + "{{ github.event_name == 'pull_request' }}",
                      self.workflow)

    def test_selector_reduces_coverage_only_after_exact_success(self) -> None:
        command = textwrap.dedent(self.jobs["changes"].split("        run: |\n", 1)[1])
        cases = [
            ("full=false\n", 0, "full=false\n"),
            ("full=true\n", 0, "full=true\n"),
            ("full=FALSE\n", 0, "full=true\n"),
            ("full=invalid\n", 0, "full=true\n"),
            ("", 0, "full=true\n"),
            ("full=false\nfull=true\n", 0, "full=true\n"),
            ("full=false\n", 7, "full=true\n"),
            ("", 7, "full=true\n"),
        ]
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            stub = work / "python3"
            stub.write_text(
                '#!/bin/sh\nprintf "%s" "$SELECTOR_OUTPUT" >"$GITHUB_OUTPUT"\n'
                'exit "$SELECTOR_EXIT"\n'
            )
            stub.chmod(0o755)
            output = work / "output"
            for raw, code, expected in cases:
                with self.subTest(raw=raw, code=code):
                    output.write_text("")
                    result = subprocess.run(
                        ["sh", "-e", "-c", command], cwd=work,
                        env={**os.environ, "PATH": f"{work}:{os.environ['PATH']}",
                             "TMPDIR": str(work), "GITHUB_OUTPUT": str(output),
                             "SELECTOR_OUTPUT": raw, "SELECTOR_EXIT": str(code)},
                        capture_output=True, text=True,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(output.read_text(), expected)
                    self.assertEqual(sorted(p.name for p in work.iterdir()), ["output", "python3"])

    def test_native_test_and_service_guards_are_independent(self) -> None:
        steps = self.jobs["windows"].split("      - name: ")[1:]
        tests = [step for step in steps if "run: go test " in step]
        service = next(step for step in steps if step.startswith("Install, crash, stop"))
        self.assertEqual(len(tests), 6)
        for setup in ("success", "failure"):
            for archive in ("success", "failure"):
                for cancelled in (False, True):
                    with self.subTest(setup=setup, archive=archive, cancelled=cancelled):
                        values = {
                            "steps.setup.outcome": setup,
                            "steps.archive.outcome": archive,
                            "cancelled()": cancelled,
                        }
                        for step in tests:
                            condition = re.search(r"^        if: (.+)$", step, re.M).group(1)
                            self.assertEqual(condition_result(condition, values),
                                             not cancelled and setup == "success")
                        condition = re.search(r"^        if: (.+)$", service, re.M).group(1)
                        self.assertEqual(condition_result(condition, values),
                                         not cancelled and archive == "success")


if __name__ == "__main__":
    unittest.main()
