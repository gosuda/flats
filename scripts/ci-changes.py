"""Select full CI unless a PR changes only regular spec or research Markdown."""

import os
import re
import subprocess
from pathlib import Path

PROSE_PATH = re.compile(rb"docs/(?:spec|research)/(?:README|[a-z0-9][a-z0-9-]*)\.md")
RAW_HEADER = re.compile(rb":([0-7]{6}) ([0-7]{6}) [0-9a-f]{40} [0-9a-f]{40} [ADMT]")
FILE_MODES = {b"000000", b"100644"}
RESERVED_NAMES = {b"con", b"prn", b"aux", b"nul", b"readme"} | {
    f"{prefix}{number}".encode("ascii")
    for prefix in ("com", "lpt")
    for number in range(1, 10)
}


def git(*args: str) -> bytes:
    return subprocess.run(["git", *args], check=True, stdout=subprocess.PIPE).stdout


def documentation_only(diff: bytes) -> bool:
    if not diff:
        return False
    fields = diff.split(b"\0")
    if fields.pop() != b"" or len(fields) % 2:
        raise ValueError("incomplete NUL-delimited Git diff")
    prose = True
    for header, path in zip(fields[::2], fields[1::2], strict=True):
        match = RAW_HEADER.fullmatch(header)
        if match is None:
            raise ValueError(f"unexpected Git diff header: {header!r}")
        # Inspect both modes, including deleted symlinks and type changes.
        stem = path.rsplit(b"/", 1)[-1].removesuffix(b".md")
        if (
            not set(match.groups()) <= FILE_MODES
            or PROSE_PATH.fullmatch(path) is None
            or stem in RESERVED_NAMES
        ):
            prose = False
    return prose


def main() -> None:
    full = True
    if os.environ["GITHUB_EVENT_NAME"] == "pull_request":
        parents = git("show", "-s", "--format=%P", "HEAD").decode("ascii").split()
        if parents != [os.environ["PR_BASE_SHA"], os.environ["PR_HEAD_SHA"]]:
            raise ValueError("checkout is not the expected pull-request merge")
        # The default checkout is the tested merge. Disable rename detection
        # so moving code into an allowed directory still includes its old path.
        diff = git(
            "diff", "--raw", "--no-abbrev", "--no-renames", "-z", "HEAD^1", "HEAD", "--"
        )
        full = not documentation_only(diff)
    with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as output:
        output.write(f"full={str(full).lower()}\n")
    print("Selected full CI" if full else "Selected documentation checks")


if __name__ == "__main__":
    main()
