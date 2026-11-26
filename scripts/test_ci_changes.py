"""Exercise CI selection against real Git merge trees and Windows packages."""

import os
import re
import subprocess
import sys
import tempfile
import unittest
from collections.abc import Generator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/ci-changes.py"


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
    def test_windows_runs_only_the_site_package(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        job = workflow.split("\n  windows:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertIn("go test -v -count=1 ./internal/site", job)
        self.assertNotIn("scripts/ci-test.sh", job)
        self.assertNotIn("go list ./...", job)

    def test_windows_archive_comes_from_the_container_build(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        container = workflow.split("\n  container:\n", 1)[1].split("\n  commit-images:\n", 1)[0]
        windows = workflow.split("\n  windows:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertIn("windows/amd64", container)
        upload = re.search(r"^ *name: (flats-windows-archive-.+)$", container, re.M)
        self.assertIsNotNone(upload)
        assert upload is not None
        self.assertIn(f"name: {upload.group(1)}", windows)
        self.assertIn("if-no-files-found: error", container)

    def test_windows_waits_for_the_container_build(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        windows = workflow.split("\n  windows:\n", 1)[1].split("\n  install-macos:\n", 1)[0]
        self.assertIn("needs: [changes, container]", windows)
        self.assertIn("needs.container.result == 'success'", windows)


if __name__ == "__main__":
    unittest.main()
