#!/usr/bin/env python3
"""Structure-agnostic Markdown link and reachability auditor.

Does not hardcode directory layouts or file naming conventions:
1. Validates that all relative Markdown links resolve to existing files.
2. Builds a link graph starting from root README.md and any paths referenced
   in source code (*.go,//go:embed) to find unreachable (orphan) Markdown files.
3. Flags files containing full 40-character hex hashes as potential volatile receipts.
"""

from __future__ import annotations

import pathlib
import re
import sys

LINK_RE = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")
HEX40_RE = re.compile(r"\b[0-9a-f]{40}\b")
EMBED_RE = re.compile(r"//go:embed\s+([^\n]+)")
QUOTED_MD_RE = re.compile(r'["`]([A-Za-z0-9_./*-]+\.md)["`]')

IGNORED_DIRS = {".git", "node_modules", "dist", "vendor", ".cursor", ".claude", ".agents"}


def find_repo_root() -> pathlib.Path:
    cur = pathlib.Path.cwd().resolve()
    for parent in [cur, *cur.parents]:
        if (parent / "go.mod").exists() or (parent / ".git").exists():
            return parent
    return cur


def iter_files(root: pathlib.Path, suffix: str) -> list[pathlib.Path]:
    result: list[pathlib.Path] = []
    for p in root.rglob(f"*{suffix}"):
        if any(part in IGNORED_DIRS for part in p.relative_to(root).parts):
            continue
        if p.is_file():
            result.append(p.resolve())
    return sorted(result)


def strip_code(md_text: str) -> str:
    text = re.sub(r"```[\s\S]*?```", "", md_text)
    return re.sub(r"`[^`\n]+`", "", text)


def extract_relative_targets(md_file: pathlib.Path) -> list[ tuple[str, pathlib.Path] ]:
    text = strip_code(md_file.read_text(encoding="utf-8", errors="replace"))
    targets: list[tuple[str, pathlib.Path]] = []
    for match in LINK_RE.finditer(text):
        raw = match.group(1).strip()
        if not raw or raw.startswith(("#", "http://", "https://", "mailto:", "flats://", "data:")):
            continue
        path_part = raw.split("#", 1)[0].strip()
        if not path_part:
            continue
        targets.append((raw, (md_file.parent / path_part).resolve()))
    return targets


def discover_code_entrypoints(root: pathlib.Path, all_md: set[pathlib.Path]) -> set[pathlib.Path]:
    seeds: set[pathlib.Path] = set()
    root_readme = (root / "README.md").resolve()
    if root_readme in all_md:
        seeds.add(root_readme)

    for go_file in iter_files(root, ".go"):
        text = go_file.read_text(encoding="utf-8", errors="replace")
        for m in EMBED_RE.finditer(text):
            for pattern in m.group(1).split():
                for matched in go_file.parent.glob(pattern):
                    resolved = matched.resolve()
                    if resolved in all_md:
                        seeds.add(resolved)
                    elif resolved.is_dir():
                        seeds.update(p for p in all_md if resolved in p.parents)
        for m in QUOTED_MD_RE.finditer(text):
            rel = m.group(1)
            for base in (go_file.parent, root):
                candidate = (base / rel).resolve()
                if candidate in all_md:
                    seeds.add(candidate)
    return seeds


def find_unreachable(root: pathlib.Path, md_files: list[pathlib.Path]) -> list[pathlib.Path]:
    all_md = set(md_files)
    queue = list(discover_code_entrypoints(root, all_md))
    visited: set[pathlib.Path] = set(queue)

    while queue:
        cur = queue.pop(0)
        for _, target in extract_relative_targets(cur):
            candidates: list[pathlib.Path] = []
            if target.is_dir():
                dir_readme = (target / "README.md").resolve()
                if dir_readme in all_md:
                    candidates.append(dir_readme)
                else:
                    candidates.extend(p for p in all_md if target in p.parents)
            elif target in all_md:
                candidates.append(target)

            for c in candidates:
                if c not in visited:
                    visited.add(c)
                    queue.append(c)

    return sorted(all_md - visited)


def main() -> int:
    root = find_repo_root()
    md_files = iter_files(root, ".md")

    broken: list[str] = []
    hash_heavy: list[str] = []

    for md in md_files:
        rel_md = md.relative_to(root)
        for raw, resolved in extract_relative_targets(md):
            if not resolved.exists():
                broken.append(f"{rel_md}: broken link -> {raw}")

        text = md.read_text(encoding="utf-8", errors="replace")
        shas = HEX40_RE.findall(text)
        if shas:
            hash_heavy.append(f"{rel_md}: {len(shas)} full 40-char hash literal(s)")

    orphans = find_unreachable(root, md_files)

    print(f"=== Documentation Graph Audit ({root}) ===")
    print(f"Scanned {len(md_files)} Markdown files.\n")

    if broken:
        print(f"[FAIL] Broken relative links ({len(broken)}):")
        for b in broken:
            print(f"  - {b}")
        print()
    else:
        print("[PASS] Zero broken relative links.\n")

    if orphans:
        print(f"[WARN] Unreachable (orphan) Markdown files ({len(orphans)}):")
        for o in orphans:
            print(f"  - {o.relative_to(root)}")
        print()
    else:
        print("[PASS] All Markdown files are reachable from README.md or Go code.\n")

    if hash_heavy:
        print(f"[INFO] Files with 40-char hashes (review for volatile receipts) ({len(hash_heavy)}):")
        for h in hash_heavy:
            print(f"  - {h}")
        print()

    return 1 if broken else 0


if __name__ == "__main__":
    sys.exit(main())
