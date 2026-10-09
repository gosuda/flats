#!/usr/bin/env python3
"""Resolve main commits introduced by a push into at most four build lanes."""

import json
import os
import re
import subprocess
import sys


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def commit(value):
    if not re.fullmatch(r"[0-9a-f]{40}", value):
        raise ValueError("commit must be a full, lowercase 40-character SHA")
    if git("rev-parse", f"{value}^{{commit}}") != value:
        raise ValueError("expected a commit object")
    return value


def select(event_name, event, requested):
    if event_name == "workflow_dispatch":
        sha = commit(requested)
        if subprocess.run(["git", "merge-base", "--is-ancestor", sha, "origin/main"],
                          check=False).returncode != 0:
            raise ValueError("requested commit is not reachable from origin/main")
        return [sha]
    if event_name != "push" or event["ref"] != "refs/heads/main":
        raise ValueError("only main pushes and manual runs are supported")
    after = commit(event["after"])
    before = event["before"]
    if before == "0" * 40:
        # Branch creation publishes its tip, not an unbounded history backfill.
        return [after]
    if not re.fullmatch(r"[0-9a-f]{40}", before):
        raise ValueError("invalid before SHA")
    try:
        commit(before)
    except subprocess.CalledProcessError:
        # A force-pushed previous tip might no longer have a branch reference.
        git("fetch", "--no-tags", "origin", before)
        commit(before)
    return git("rev-list", "--first-parent", "--reverse", after, f"^{before}").splitlines()


def matrix(commits):
    # The Actions matrix has a 256-job limit. Four serial lanes retain every
    # commit even for a large push, without truncating the event commit list.
    return {"include": [{"lane": n, "commits": commits[n::4]}
                        for n in range(min(4, len(commits)))]}


if __name__ == "__main__":
    try:
        with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as source:
            event = json.load(source)
        commits = select(os.environ["GITHUB_EVENT_NAME"], event,
                         os.environ.get("REQUESTED_COMMIT", ""))
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write(f"matrix={json.dumps(matrix(commits), separators=(',', ':'))}\n")
            output.write(f"count={len(commits)}\n")
        print(f"Selected {len(commits)} commit(s)")
    except (ValueError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
