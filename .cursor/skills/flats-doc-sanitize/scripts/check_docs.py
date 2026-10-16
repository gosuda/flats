#!/usr/bin/env python3
"""Structure-agnostic Markdown link, anchor, and reachability auditor.

Does not hardcode directory layouts or file naming conventions:
1. Discovers tracked Markdown files via `git ls-files` from the Git top-level root.
2. Validates relative Markdown/reference/HTML links, case-exact file names, and `#anchor` fragments.
3. Builds a BFS reachability graph starting from root `README.md` and runtime/build code entrypoints
   (`//go:embed`, build scripts) while reporting test-only or colocated Markdown files separately.
4. Flags relative links inside `docs/agent/topics/` (which break when served via MCP/llms.txt)
   and lines containing 40-char (SHA-1) or 64-char (SHA-256) hex hashes.
"""

from __future__ import annotations

import collections
import os
import pathlib
import re
import shlex
import subprocess
import sys
import unicodedata
import urllib.parse

SCHEME_RE = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")
HEX_HASH_RE = re.compile(r"\b(?:[0-9a-fA-F]{64}|[0-9a-fA-F]{40})\b")
FENCE_OPEN_RE = re.compile(r"^[ \t]{0,3}(`{3,}|~{3,})")
HEADING_RE = re.compile(r"^[ \t]{0,3}(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$")
REF_DEF_RE = re.compile(r"^[ \t]{0,3}\[([^\]]+)\]:[ \t]*(\S+)(?:[ \t]+(?:\"[^\"]*\"|'[^']*'|\([^)]*\)))?[ \t]*$")
HTML_ATTR_RE = re.compile(r"""<(?:a|img)\b[^>]*?\b(?:href|src)\s*=\s*(?:"([^"]+)"|'([^']+)')""", re.IGNORECASE)
HTML_ID_RE = re.compile(r"""\b(?:id|name)\s*=\s*(?:"([^"]+)"|'([^']+)')""", re.IGNORECASE)
EMBED_LINE_RE = re.compile(r"^[ \t]*//go:embed[ \t]+(.+)$", re.MULTILINE)
QUOTED_MD_RE = re.compile(r"""["'`]([A-Za-z0-9_./*-]+\.(?:md|markdown))["'`]""")

CODE_SUFFIXES = {".go", ".mjs", ".js", ".ts", ".sh", ".py", ".yml", ".yaml"}


def find_repo_root() -> pathlib.Path:
    script_dir = pathlib.Path(__file__).resolve().parent
    for start in (pathlib.Path.cwd().resolve(), script_dir):
        try:
            out = subprocess.check_output(
                ["git", "-C", str(start), "rev-parse", "--show-toplevel"],
                stderr=subprocess.DEVNULL,
                text=True,
            ).strip()
            if out:
                return pathlib.Path(out).resolve()
        except Exception:
            pass
    for parent in [script_dir, *script_dir.parents]:
        if (parent / ".git").exists():
            return parent.resolve()
    return pathlib.Path.cwd().resolve()


def list_tracked_files(root: pathlib.Path) -> list[pathlib.Path]:
    try:
        out = subprocess.check_output(
            ["git", "-C", str(root), "ls-files", "-z"],
            stderr=subprocess.DEVNULL,
        )
        rels = [p for p in out.decode("utf-8", errors="replace").split("\0") if p]
        return sorted((root / rel) for rel in rels if (root / rel).is_file())
    except Exception:
        return sorted(p for p in root.rglob("*") if p.is_file() and ".git" not in p.parts)


def strip_fences_and_comments(text: str) -> str:
    """Replace fenced code blocks and HTML comments with newlines to preserve line numbers."""
    text = re.sub(
        r"<!--[\s\S]*?-->",
        lambda m: "\n" * m.group(0).count("\n"),
        text,
    )
    lines = text.splitlines(keepends=True)
    out: list[str] = []
    in_fence = False
    fence_char = ""
    fence_len = 0

    for line in lines:
        if not in_fence:
            m = FENCE_OPEN_RE.match(line)
            if m:
                marker = m.group(1)
                in_fence = True
                fence_char = marker[0]
                fence_len = len(marker)
                out.append("\n" if line.endswith("\n") else "")
                continue
            out.append(line)
        else:
            close_re = re.compile(rf"^[ \t]{{0,3}}{re.escape(fence_char)}{{{fence_len},}}[ \t]*\r?\n?$")
            if close_re.match(line):
                in_fence = False
            out.append("\n" if line.endswith("\n") else "")
    return "".join(out)


def strip_inline_code(text: str) -> str:
    """Replace inline code spans (`...`, ``...``) with spaces, preserving newlines."""
    return re.sub(
        r"(`+)(.+?)\1",
        lambda m: "".join("\n" if ch == "\n" else " " for ch in m.group(0)),
        text,
        flags=re.DOTALL,
    )


def github_slug(heading_text: str) -> str:
    s = re.sub(r"`+", "", heading_text)
    s = re.sub(r"<[^>]+>", "", s)
    s = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", s)
    s = s.strip().lower()
    out: list[str] = []
    for ch in s:
        if ch in (" ", "\t"):
            out.append("-")
        elif ch == "-" or ch == "_":
            out.append(ch)
        else:
            cat = unicodedata.category(ch)
            if cat.startswith(("L", "N")):
                out.append(ch)
    return "".join(out)


def extract_anchors(prose_with_inline: str, prose_no_inline: str) -> set[str]:
    anchors: set[str] = set()
    counts: dict[str, int] = {}

    for m in HTML_ID_RE.finditer(prose_no_inline):
        val = (m.group(1) or m.group(2) or "").strip()
        if val:
            anchors.add(val)
            anchors.add(val.lower())

    for line in prose_with_inline.splitlines():
        m = HEADING_RE.match(line)
        if not m:
            continue
        base = github_slug(m.group(2))
        if not base:
            continue
        idx = counts.get(base, 0)
        counts[base] = idx + 1
        slug = base if idx == 0 else f"{base}-{idx}"
        anchors.add(slug)
    return anchors


def clean_destination(raw_dest: str) -> str:
    s = raw_dest.strip()
    if s.startswith("<") and ">" in s:
        return s[1 : s.index(">")].strip()
    # Strip optional title: path "title" or path 'title' or path (title)
    m = re.match(r'^(\S+?)(?:\s+(?:"[^"]*"|\'[^\']*\'|\([^)]*\)))?$', s)
    if m:
        return m.group(1)
    return s.split()[0] if s.split() else ""


def scan_inline_links(line: str) -> list[tuple[str, bool]]:
    """Return (destination, is_ref) from inline `[text](dest)` and `[text][ref]`."""
    results: list[tuple[str, bool]] = []
    n = len(line)
    i = 0
    while i < n:
        if line[i] == "[":
            # Find matching ']' handling nested brackets
            depth = 1
            j = i + 1
            while j < n and depth > 0:
                if line[j] == "\\":
                    j += 2
                    continue
                if line[j] == "[":
                    depth += 1
                elif line[j] == "]":
                    depth -= 1
                j += 1
            if depth == 0 and j < n:
                if line[j] == "(":
                    # Find matching ')' handling nested parentheses and angle brackets
                    p_depth = 1
                    k = j + 1
                    in_angles = False
                    while k < n and p_depth > 0:
                        if line[k] == "\\":
                            k += 2
                            continue
                        if line[k] == "<":
                            in_angles = True
                        elif line[k] == ">":
                            in_angles = False
                        elif not in_angles:
                            if line[k] == "(":
                                p_depth += 1
                            elif line[k] == ")":
                                p_depth -= 1
                        k += 1
                    if p_depth == 0:
                        dest = clean_destination(line[j + 1 : k - 1])
                        if dest:
                            results.append((dest, False))
                        i = k
                        continue
                elif line[j] == "[":
                    k = line.find("]", j + 1)
                    if k != -1:
                        ref_label = line[j + 1 : k].strip() or line[i + 1 : j - 1].strip()
                        if ref_label:
                            results.append((ref_label, True))
                        i = k + 1
                        continue
        i += 1
    return results


class LinkRef:
    def __init__(self, line_no: int, raw: str, target_path: pathlib.Path | None, fragment: str):
        self.line_no = line_no
        self.raw = raw
        self.target_path = target_path
        self.fragment = fragment


def parse_links_in_file(root: pathlib.Path, md_file: pathlib.Path, prose_no_inline: str) -> list[LinkRef]:
    lines = prose_no_inline.splitlines()
    ref_defs: dict[str, str] = {}

    for line in lines:
        m = REF_DEF_RE.match(line)
        if m:
            label = m.group(1).strip().lower()
            dest = clean_destination(m.group(2))
            if label and dest:
                ref_defs[label] = dest

    raw_occurrences: list[tuple[int, str]] = []
    for idx, line in enumerate(lines, start=1):
        if REF_DEF_RE.match(line):
            m = REF_DEF_RE.match(line)
            if m:
                raw_occurrences.append((idx, clean_destination(m.group(2))))
            continue
        for dest, is_ref in scan_inline_links(line):
            if is_ref:
                resolved_def = ref_defs.get(dest.lower())
                if resolved_def:
                    raw_occurrences.append((idx, resolved_def))
            else:
                raw_occurrences.append((idx, dest))
        for hm in HTML_ATTR_RE.finditer(line):
            attr_val = (hm.group(1) or hm.group(2) or "").strip()
            if attr_val:
                raw_occurrences.append((idx, attr_val))

    links: list[LinkRef] = []
    for line_no, raw in raw_occurrences:
        if not raw or raw.startswith("//") or SCHEME_RE.match(raw):
            continue
        if raw.startswith("#"):
            links.append(LinkRef(line_no, raw, md_file, raw[1:]))
            continue

        path_part, _, frag = raw.partition("#")
        path_part = path_part.split("?", 1)[0].strip()
        path_part = urllib.parse.unquote(path_part)
        frag = urllib.parse.unquote(frag.strip())

        if not path_part:
            links.append(LinkRef(line_no, raw, md_file, frag))
            continue

        if path_part.startswith("/"):
            candidate = root / path_part.lstrip("/")
        else:
            candidate = md_file.parent / path_part

        # Normalize '..' without resolving case on macOS APFS
        norm = pathlib.Path(os.path.normpath(str(candidate)))
        links.append(LinkRef(line_no, raw, norm, frag))

    return links


def check_exact_case_exists(root: pathlib.Path, target: pathlib.Path, tracked_set: set[pathlib.Path], tracked_dirs: set[pathlib.Path]) -> tuple[bool, str]:
    if target in tracked_set or target in tracked_dirs or target == root:
        return True, ""
    if target.exists():
        # Verify case-exact components from root
        try:
            rel = target.relative_to(root)
        except ValueError:
            return True, ""
        cur = root
        for part in rel.parts:
            try:
                entries = set(os.listdir(cur))
            except OSError:
                return False, "not found"
            if part not in entries:
                return False, f"case mismatch on '{part}' (breaks on Linux/GitHub)"
            cur = cur / part
        return True, ""
    return False, "file not found"


def discover_code_references(
    root: pathlib.Path,
    tracked_files: list[pathlib.Path],
    md_set: set[pathlib.Path],
) -> tuple[set[pathlib.Path], set[pathlib.Path]]:
    """Return (runtime_seeds, test_only_refs)."""
    runtime_seeds: set[pathlib.Path] = set()
    test_refs: set[pathlib.Path] = set()

    for f in tracked_files:
        if f.suffix not in CODE_SUFFIXES:
            continue
        rel_parts = f.relative_to(root).parts
        if rel_parts and rel_parts[0] in (".cursor", ".claude", ".agents"):
            continue
        is_test = f.name.endswith("_test.go") or "test" in f.name.lower() or "testdata" in rel_parts
        text = f.read_text(encoding="utf-8", errors="replace")

        if f.suffix == ".go":
            for m in EMBED_LINE_RE.finditer(text):
                try:
                    patterns = shlex.split(m.group(1))
                except ValueError:
                    patterns = m.group(1).split()
                for pat in patterns:
                    allow_hidden = pat.startswith("all:")
                    if allow_hidden:
                        pat = pat[4:]
                    for matched in f.parent.glob(pat):
                        if matched.is_file() and matched in md_set:
                            if not allow_hidden and any(
                                p.startswith((".", "_")) for p in matched.relative_to(f.parent).parts
                            ):
                                continue
                            (test_refs if is_test else runtime_seeds).add(matched)
                        elif matched.is_dir():
                            for md in md_set:
                                if matched in md.parents:
                                    if not allow_hidden and any(
                                        p.startswith((".", "_")) for p in md.relative_to(matched).parts
                                    ):
                                        continue
                                    (test_refs if is_test else runtime_seeds).add(md)

        for m in QUOTED_MD_RE.finditer(text):
            raw_p = m.group(1)
            if "*" in raw_p:
                continue
            for base in (f.parent, root):
                cand = pathlib.Path(os.path.normpath(str(base / raw_p)))
                if cand in md_set:
                    (test_refs if is_test else runtime_seeds).add(cand)

    return runtime_seeds, test_refs


def main() -> int:
    root = find_repo_root()
    tracked = list_tracked_files(root)

    # Exclude tool/skill dirs from project doc audit
    md_files = [
        p
        for p in tracked
        if p.suffix.lower() in (".md", ".markdown")
        and p.relative_to(root).parts[0] not in (".cursor", ".claude", ".agents")
    ]
    if not md_files:
        print(f"[FAIL] Scanned 0 Markdown files under {root}. Check repository root.", file=sys.stderr)
        return 1

    tracked_set = set(tracked)
    tracked_dirs: set[pathlib.Path] = {root}
    for p in tracked:
        for parent in p.parents:
            if parent == root or root in parent.parents:
                tracked_dirs.add(parent)

    md_set = set(md_files)
    anchors_by_file: dict[pathlib.Path, set[str]] = {}
    links_by_file: dict[pathlib.Path, list[LinkRef]] = {}
    hash_findings: list[str] = []
    agent_rel_link_warnings: list[str] = []

    for md in md_files:
        raw_text = md.read_text(encoding="utf-8", errors="replace")
        prose_with_inline = strip_fences_and_comments(raw_text)
        prose_no_inline = strip_inline_code(prose_with_inline)

        anchors_by_file[md] = extract_anchors(prose_with_inline, prose_no_inline)
        links = parse_links_in_file(root, md, prose_no_inline)
        links_by_file[md] = links

        rel_md = md.relative_to(root)
        if len(rel_md.parts) >= 3 and rel_md.parts[:2] == ("docs", "agent"):
            for lk in links:
                if lk.target_path is not None and lk.target_path != md:
                    agent_rel_link_warnings.append(
                        f"{rel_md}:{lk.line_no}: relative link '{lk.raw}' inside docs/agent/ (breaks when served via MCP/llms.txt)"
                    )

        hash_lines: list[int] = []
        for idx, line in enumerate(prose_with_inline.splitlines(), start=1):
            if HEX_HASH_RE.search(line):
                hash_lines.append(idx)
        if hash_lines:
            preview = ", ".join(str(n) for n in hash_lines[:8])
            if len(hash_lines) > 8:
                preview += ", ..."
            hash_findings.append(f"{rel_md}: {len(hash_lines)} line(s) with 40/64-char hashes (lines {preview})")

    broken_links: list[str] = []
    for md in md_files:
        rel_md = md.relative_to(root)
        for lk in links_by_file[md]:
            if lk.target_path is None:
                continue
            ok, reason = check_exact_case_exists(root, lk.target_path, tracked_set, tracked_dirs)
            if not ok:
                broken_links.append(f"{rel_md}:{lk.line_no}: broken link '{lk.raw}' ({reason})")
                continue

            target_md: pathlib.Path | None = None
            if lk.target_path in md_set:
                target_md = lk.target_path
            elif lk.target_path in tracked_dirs and (lk.target_path / "README.md") in md_set:
                target_md = lk.target_path / "README.md"

            if lk.fragment and target_md is not None:
                known_anchors = anchors_by_file.get(target_md, set())
                if lk.fragment not in known_anchors and lk.fragment.lower() not in known_anchors:
                    broken_links.append(
                        f"{rel_md}:{lk.line_no}: broken anchor '{lk.raw}' (#{lk.fragment} not found in {target_md.relative_to(root)})"
                    )

    # Reachability BFS from root README.md + runtime/build code seeds
    runtime_seeds, test_only_refs = discover_code_references(root, tracked, md_set)
    seeds: set[pathlib.Path] = set(runtime_seeds)
    root_readme = root / "README.md"
    if root_readme in md_set:
        seeds.add(root_readme)

    queue: collections.deque[pathlib.Path] = collections.deque(sorted(seeds))
    visited: set[pathlib.Path] = set(seeds)

    while queue:
        cur = queue.popleft()
        for lk in links_by_file.get(cur, []):
            if lk.target_path is None:
                continue
            candidates: list[pathlib.Path] = []
            if lk.target_path in md_set:
                candidates.append(lk.target_path)
            elif lk.target_path in tracked_dirs:
                dir_readme = lk.target_path / "README.md"
                if dir_readme in md_set:
                    candidates.append(dir_readme)
            elif lk.target_path in tracked_set:
                # Linking to a non-Markdown file inside a directory (e.g. examples/foo/server.js)
                # also credits that directory's README.md if present.
                sibling_readme = lk.target_path.parent / "README.md"
                if sibling_readme in md_set:
                    candidates.append(sibling_readme)

            for c in candidates:
                if c not in visited:
                    visited.add(c)
                    queue.append(c)

    unreached = sorted(md_set - visited)
    doc_orphans: list[pathlib.Path] = []
    colocated_notes: list[pathlib.Path] = []

    for u in unreached:
        rel_parts = u.relative_to(root).parts
        if rel_parts[0] in ("internal", "examples", "cmd") or u in test_only_refs:
            colocated_notes.append(u)
        else:
            doc_orphans.append(u)

    print(f"=== Documentation Graph Audit ({root}) ===")
    print(f"Scanned {len(md_files)} tracked Markdown files.\n")

    if broken_links:
        print(f"[FAIL] Broken links / case mismatches / broken anchors ({len(broken_links)}):")
        for b in broken_links:
            print(f"  - {b}")
        print()
    else:
        print("[PASS] Zero broken links, case mismatches, or broken anchors.\n")

    if agent_rel_link_warnings:
        print(f"[WARN] Relative links inside docs/agent/ ({len(agent_rel_link_warnings)}):")
        for w in agent_rel_link_warnings:
            print(f"  - {w}")
        print()

    if doc_orphans:
        print(f"[WARN] Unreachable documentation files under docs/ ({len(doc_orphans)}):")
        for o in doc_orphans:
            print(f"  - {o.relative_to(root)}")
        print()
    else:
        print("[PASS] All docs/ Markdown files are reachable from README.md or runtime/build code.\n")

    if colocated_notes:
        print(f"[INFO] Colocated package/test Markdown files not linked from root index ({len(colocated_notes)}):")
        for c in colocated_notes:
            suffix = " (referenced in tests)" if c in test_only_refs else ""
            print(f"  - {c.relative_to(root)}{suffix}")
        print()

    if hash_findings:
        print(f"[INFO] Files containing 40/64-char hex hashes ({len(hash_findings)}):")
        for h in hash_findings:
            print(f"  - {h}")
        print()

    return 1 if broken_links else 0


if __name__ == "__main__":
    sys.exit(main())
