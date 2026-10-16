#!/usr/bin/env python3
"""Lightweight, structure-agnostic Markdown link, anchor, and reachability auditor.

Intentional scope (standard repository Markdown, not a full CommonMark parser):
- Discovers non-ignored repository files via `git ls-files --cached --others --exclude-standard`.
- Strips fenced code blocks (``` / ~~~), inline code spans (`...`), and HTML comments (<!-- ... -->).
- Validates single-line Markdown links (`[text](dest)`, `[text][ref]`), reference definitions
  (`[ref]: dest`), HTML `<a href>` / `<img src>` tags, case-exact file paths, and ATX `#` heading
  `#anchor` fragments.
- Computes BFS reachability from root `README.md` + runtime/build code references (`//go:embed`,
  scripts/configs), separating test-only or colocated package notes (`internal/`, `examples/`, `cmd/`).
- Flags relative links inside `docs/agent/` and 40/64-char hex hashes.
"""

from __future__ import annotations

import collections
import html.parser
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
FENCE_RE = re.compile(r"^[ \t]*(`{3,}|~{3,})")
ATX_HEADING_RE = re.compile(r"^[ \t]{0,3}#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$")
REF_DEF_RE = re.compile(
    r"^[ \t]{0,3}\[([^\^\]][^\]]*)\]:[ \t]*(<[^>\s]+>|\S+)(?:[ \t]+(?:\"[^\"]*\"|'[^']*'|\([^)]*\)))?[ \t]*$"
)
EMBED_LINE_RE = re.compile(r"^[ \t]*//go:embed[ \t]+(.+)$", re.MULTILINE)
QUOTED_MD_RE = re.compile(r"""["'`]([A-Za-z0-9_./*-]+\.(?:md|markdown))["'`]""")

CODE_SUFFIXES = {".go", ".mjs", ".js", ".ts", ".sh", ".py", ".yml", ".yaml", ".json"}
TEST_FILE_RE = re.compile(
    r"(?:_test\.go$|(?:^|[._-])(?:test|tests)\.[A-Za-z0-9]+$|(?:^|[._-])spec\.(?:mjs|js|ts|py|sh)$|^test_)",
    re.IGNORECASE,
)


class _HTMLScanner(html.parser.HTMLParser):
    def __init__(self, line_offset: int = 0) -> None:
        super().__init__(convert_charrefs=True)
        self.line_offset = line_offset
        self.anchors: set[str] = set()
        self.links: list[tuple[int, str]] = []

    def _handle(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        tag_l = tag.lower()
        line_no = self.line_offset + self.getpos()[0]
        for k, v in attrs:
            if not v or not v.strip():
                continue
            kl, val = k.lower(), v.strip()
            if kl == "id" or (kl == "name" and tag_l == "a"):
                self.anchors.add(val)
            if tag_l in ("a", "img", "link", "script", "source") and kl in ("href", "src"):
                self.links.append((line_no, val))

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self._handle(tag, attrs)

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self._handle(tag, attrs)


def find_repo_root() -> pathlib.Path:
    script_dir = pathlib.Path(__file__).resolve().parent
    for start in (script_dir, pathlib.Path.cwd().resolve()):
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
    raise RuntimeError(f"Could not determine Git repository root from {script_dir}")


def list_repo_files(root: pathlib.Path) -> list[pathlib.Path]:
    out = subprocess.check_output(
        ["git", "-C", str(root), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        stderr=subprocess.PIPE,
    )
    rels = {p for p in out.decode("utf-8", errors="replace").split("\0") if p}
    return sorted((root / rel) for rel in rels if (root / rel).is_file())


def strip_code_and_comments(text: str) -> tuple[str, str]:
    """Return (without_fences_or_comments, without_inline_code_or_comments), preserving line numbers."""
    lines = text.splitlines(keepends=True)
    fenced_out: list[str] = []
    in_fence = False
    fence_char = ""
    fence_len = 0

    for line in lines:
        m = FENCE_RE.match(line)
        if not in_fence:
            if m:
                marker = m.group(1)
                in_fence = True
                fence_char = marker[0]
                fence_len = len(marker)
                fenced_out.append("\n" if line.endswith("\n") else "")
                continue
            fenced_out.append(line)
        else:
            if m and m.group(1)[0] == fence_char and len(m.group(1)) >= fence_len and line[m.end() :].strip() == "":
                in_fence = False
            fenced_out.append("\n" if line.endswith("\n") else "")

    no_fences = "".join(fenced_out)
    # Single-pass scanner for unescaped inline code (`...`) and HTML comments (<!-- ... -->)
    # so neither escaped backticks (\`), backticks inside comments, nor `<!--` inside inline code
    # can corrupt the other.
    buf_f = list(no_fences)
    buf_i = list(no_fences)
    n = len(no_fences)
    i = 0
    while i < n:
        if no_fences[i] == "\\":
            i += 2
            continue
        if no_fences.startswith("<!--", i):
            end_c = no_fences.find("-->", i + 4)
            if end_c == -1:
                i += 4
                continue
            end_idx = end_c + 3
            for k in range(i, end_idx):
                if buf_f[k] != "\n":
                    buf_f[k] = " "
                if buf_i[k] != "\n":
                    buf_i[k] = " "
            i = end_idx
            continue
        if no_fences[i] == "`":
            j = i
            while j < n and no_fences[j] == "`":
                j += 1
            ticks = no_fences[i:j]
            # Search for matching closing ticks of the exact same length before a blank line (\n[ \t]*\n)
            k = j
            closed_end = -1
            while k < n:
                if no_fences[k] == "\n":
                    ws = k + 1
                    while ws < n and no_fences[ws] in (" ", "\t", "\r"):
                        ws += 1
                    if ws < n and no_fences[ws] == "\n":
                        break
                if no_fences[k] == "`":
                    m_end = k
                    while m_end < n and no_fences[m_end] == "`":
                        m_end += 1
                    if (m_end - k) == len(ticks):
                        closed_end = m_end
                        break
                    k = m_end
                    continue
                k += 1
            if closed_end != -1:
                for p in range(i, closed_end):
                    if buf_i[p] != "\n":
                        buf_i[p] = " "
                i = closed_end
                continue
            i = j
            continue
        i += 1

    return "".join(buf_f), "".join(buf_i)


def github_slug(heading_text: str) -> str:
    # Protect inline code spans with placeholders first so links wrapping inline code
    # (e.g., `# [Use `flats`][ref]`) strip cleanly before inline code is restored.
    code_spans: list[str] = []

    def _save_code(m: re.Match[str]) -> str:
        idx = len(code_spans)
        code_spans.append(m.group(0).strip("`"))
        return f"\x00CODE{idx}\x00"

    s = re.sub(r"(?<!\\)`+[^`]*`+", _save_code, heading_text)
    s = re.sub(r"<[^>]+>", "", s)
    s = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", s)
    s = re.sub(r"!?\[([^\]]+)\]\[[^\]]*\]", r"\1", s)
    s = re.sub(r"!?\[([^\]]+)\]", r"\1", s)
    s = re.sub(r"(\*{1,3})(.+?)\1", r"\2", s)
    s = re.sub(r"\x00CODE(\d+)\x00", lambda m: code_spans[int(m.group(1))], s)
    s = s.strip().lower()
    out: list[str] = []
    for ch in s:
        if ch in (" ", "\t"):
            out.append("-")
        elif ch in ("-", "_"):
            out.append(ch)
        elif unicodedata.category(ch).startswith(("L", "N", "M")):
            out.append(ch)
    return "".join(out)


def scan_html(prose_no_inline: str) -> tuple[set[str], list[tuple[int, str]]]:
    anchors: set[str] = set()
    links: list[tuple[int, str]] = []
    lines = prose_no_inline.splitlines(keepends=True)
    buf: list[str] = []
    start_line = 0

    def _flush() -> None:
        if not buf:
            return
        chunk = "".join(buf)
        if "<" in chunk:
            sc = _HTMLScanner(line_offset=start_line)
            sc.CDATA_CONTENT_ELEMENTS = ()
            sc.RCDATA_CONTENT_ELEMENTS = ()
            try:
                sc.feed(chunk)
                sc.close()
            except Exception:
                pass
            anchors.update(sc.anchors)
            links.extend(sc.links)

    for idx, line in enumerate(lines):
        if line.strip() == "":
            _flush()
            buf = []
        else:
            if not buf:
                start_line = idx
            buf.append(line)
    _flush()
    return anchors, links


def extract_anchors(prose_with_inline: str, html_anchors: set[str]) -> set[str]:
    anchors: set[str] = set()
    counts: dict[str, int] = {}
    for line in prose_with_inline.splitlines():
        m = ATX_HEADING_RE.match(line)
        if not m:
            continue
        base = github_slug(m.group(1))
        if not base:
            continue
        idx = counts.get(base, 0)
        counts[base] = idx + 1
        slug = base if idx == 0 else f"{base}-{idx}"
        while slug in anchors:
            idx += 1
            counts[base] = idx + 1
            slug = f"{base}-{idx}"
        anchors.add(slug)
    return anchors | html_anchors


def clean_destination(raw_dest: str) -> str:
    s = raw_dest.strip()
    if s.startswith("<") and ">" in s:
        return s[1 : s.index(">")].strip()
    m = re.match(r'^(\S+?)(?:\s+(?:"[^"]*"|\'[^\']*\'|\([^)]*\)))?$', s)
    return m.group(1) if m else (s.split()[0] if s.split() else "")


def scan_line_links(line: str, ref_defs: dict[str, tuple[int, str]], used_refs: set[str]) -> list[str]:
    results: list[str] = []
    n = len(line)
    i = 0
    while i < n:
        if line[i] == "\\":
            i += 2
            continue
        if line[i] == "[":
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
            if depth == 0:
                label = line[i + 1 : j - 1]
                if "[" in label:
                    results.extend(scan_line_links(label, ref_defs, used_refs))
                if j < n and line[j] == "(":
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
                            results.append(dest)
                        i = k
                        continue
                elif j < n and line[j] == "[":
                    k = line.find("]", j + 1)
                    if k != -1:
                        ref_key = (" ".join(line[j + 1 : k].split()) or " ".join(label.split())).lower()
                        if ref_key in ref_defs:
                            used_refs.add(ref_key)
                            results.append(ref_defs[ref_key][1])
                        i = k + 1
                        continue
                else:
                    ref_key = " ".join(label.split()).lower()
                    if ref_key in ref_defs:
                        used_refs.add(ref_key)
                        results.append(ref_defs[ref_key][1])
                    i = j
                    continue
        i += 1
    return results


class LinkRef:
    def __init__(self, line_no: int, raw: str, target_path: pathlib.Path | None, fragment: str):
        self.line_no = line_no
        self.raw = raw
        self.target_path = target_path
        self.fragment = fragment


def parse_links_in_file(
    root: pathlib.Path,
    md_file: pathlib.Path,
    prose_no_inline: str,
    html_links: list[tuple[int, str]],
) -> list[LinkRef]:
    lines = prose_no_inline.splitlines()
    ref_defs: dict[str, tuple[int, str]] = {}
    used_refs: set[str] = set()
    raw_occurrences: list[tuple[int, str]] = []

    for idx, line in enumerate(lines, start=1):
        m = REF_DEF_RE.match(line)
        if m:
            key = " ".join(m.group(1).split()).lower()
            dest = clean_destination(m.group(2))
            if key and dest and key not in ref_defs:
                ref_defs[key] = (idx, dest)

    for idx, line in enumerate(lines, start=1):
        if REF_DEF_RE.match(line):
            continue
        for dest in scan_line_links(line, ref_defs, used_refs):
            raw_occurrences.append((idx, dest))

    for key, (def_line, dest) in ref_defs.items():
        if key not in used_refs:
            raw_occurrences.append((def_line, dest))
    raw_occurrences.extend(html_links)

    links: list[LinkRef] = []
    for line_no, raw in raw_occurrences:
        if not raw or raw.startswith("//") or SCHEME_RE.match(raw):
            continue
        if raw.startswith("#"):
            links.append(LinkRef(line_no, raw, md_file, urllib.parse.unquote(raw[1:].strip())))
            continue
        path_part, _, frag = raw.partition("#")
        path_part = urllib.parse.unquote(path_part.split("?", 1)[0].strip())
        frag = urllib.parse.unquote(frag.strip())
        if not path_part:
            links.append(LinkRef(line_no, raw, md_file, frag))
            continue
        candidate = (root / path_part.lstrip("/")) if path_part.startswith("/") else (md_file.parent / path_part)
        links.append(LinkRef(line_no, raw, pathlib.Path(os.path.normpath(str(candidate))), frag))
    return links


def check_target_valid(
    root: pathlib.Path,
    target: pathlib.Path,
    repo_file_set: set[pathlib.Path],
    repo_dirs: set[pathlib.Path],
) -> tuple[bool, str]:
    try:
        rel = target.relative_to(root)
    except ValueError:
        return False, "link points outside repository root"
    if target in repo_file_set or target in repo_dirs:
        return True, ""
    if target.exists():
        cur = root
        for part in rel.parts:
            try:
                entries = set(os.listdir(cur))
            except OSError:
                return False, "file not found"
            if part not in entries:
                return False, f"case mismatch on '{part}' (breaks on Linux/GitHub)"
            cur = cur / part
        return False, "target is gitignored (will break on GitHub)"
    return False, "file not found"


def discover_code_references(
    root: pathlib.Path,
    repo_files: list[pathlib.Path],
    md_set: set[pathlib.Path],
) -> tuple[set[pathlib.Path], set[pathlib.Path]]:
    runtime_seeds: set[pathlib.Path] = set()
    test_refs: set[pathlib.Path] = set()

    for f in repo_files:
        if f.suffix.lower() not in CODE_SUFFIXES:
            continue
        rel_parts = f.relative_to(root).parts
        if rel_parts and rel_parts[0] in (".cursor", ".claude", ".agents"):
            continue
        is_test = any(p in ("testdata", "test", "tests", "__tests__") for p in rel_parts[:-1]) or bool(
            TEST_FILE_RE.search(f.name)
        )
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
                    exact_direct = (f.parent / pat).resolve()
                    for matched in f.parent.glob(pat):
                        if matched.is_file() and matched in md_set:
                            if (
                                not allow_hidden
                                and matched.resolve() != exact_direct
                                and any(p.startswith((".", "_")) for p in matched.relative_to(f.parent).parts)
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
    try:
        root = find_repo_root()
        repo_files = list_repo_files(root)
    except Exception as exc:
        print(f"[FAIL] Repository discovery failed: {exc}", file=sys.stderr)
        return 1

    md_files = [
        p
        for p in repo_files
        if p.suffix.lower() in (".md", ".markdown")
        and p.relative_to(root).parts[0] not in (".cursor", ".claude", ".agents")
    ]
    if not md_files:
        print(f"[FAIL] Scanned 0 Markdown files under {root}. Check repository root.", file=sys.stderr)
        return 1

    repo_file_set = set(repo_files)
    repo_dirs: set[pathlib.Path] = {root}
    for p in repo_files:
        for parent in p.parents:
            if parent == root or root in parent.parents:
                repo_dirs.add(parent)

    md_set = set(md_files)
    anchors_by_file: dict[pathlib.Path, set[str]] = {}
    links_by_file: dict[pathlib.Path, list[LinkRef]] = {}
    hash_findings: list[str] = []
    agent_rel_link_warnings: list[str] = []

    for md in md_files:
        raw_text = md.read_text(encoding="utf-8", errors="replace")
        prose_with_inline, prose_no_inline = strip_code_and_comments(raw_text)
        html_anchors, html_links = scan_html(prose_no_inline)

        anchors_by_file[md] = extract_anchors(prose_with_inline, html_anchors)
        links = parse_links_in_file(root, md, prose_no_inline, html_links)
        links_by_file[md] = links

        rel_md = md.relative_to(root)
        if len(rel_md.parts) >= 3 and rel_md.parts[:2] == ("docs", "agent"):
            for lk in links:
                if lk.target_path is not None and lk.target_path != md:
                    agent_rel_link_warnings.append(
                        f"{rel_md}:{lk.line_no}: relative link '{lk.raw}' inside docs/agent/ (breaks when served via MCP/llms.txt)"
                    )

        hash_lines = [
            idx for idx, line in enumerate(prose_with_inline.splitlines(), start=1) if HEX_HASH_RE.search(line)
        ]
        if hash_lines:
            preview = ", ".join(str(n) for n in hash_lines[:8]) + (", ..." if len(hash_lines) > 8 else "")
            hash_findings.append(f"{rel_md}: {len(hash_lines)} line(s) with 40/64-char hashes (lines {preview})")

    broken_links: list[str] = []
    for md in md_files:
        rel_md = md.relative_to(root)
        for lk in links_by_file[md]:
            if lk.target_path is None:
                continue
            ok, reason = check_target_valid(root, lk.target_path, repo_file_set, repo_dirs)
            if not ok:
                broken_links.append(f"{rel_md}:{lk.line_no}: broken link '{lk.raw}' ({reason})")
                continue

            target_md: pathlib.Path | None = None
            if lk.target_path in md_set:
                target_md = lk.target_path
            elif lk.target_path in repo_dirs and (lk.target_path / "README.md") in md_set:
                target_md = lk.target_path / "README.md"

            if lk.fragment and target_md is not None:
                if lk.fragment not in anchors_by_file.get(target_md, set()):
                    broken_links.append(
                        f"{rel_md}:{lk.line_no}: broken anchor '{lk.raw}' (#{lk.fragment} not found in {target_md.relative_to(root)})"
                    )

    runtime_seeds, test_only_refs = discover_code_references(root, repo_files, md_set)
    seeds: set[pathlib.Path] = set(runtime_seeds)
    if (root / "README.md") in md_set:
        seeds.add(root / "README.md")

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
            elif lk.target_path in repo_dirs and (lk.target_path / "README.md") in md_set:
                candidates.append(lk.target_path / "README.md")
            elif lk.target_path in repo_file_set and (lk.target_path.parent / "README.md") in md_set:
                candidates.append(lk.target_path.parent / "README.md")
            for c in candidates:
                if c not in visited:
                    visited.add(c)
                    queue.append(c)

    doc_orphans: list[pathlib.Path] = []
    colocated_notes: list[pathlib.Path] = []
    for u in sorted(md_set - visited):
        if u.relative_to(root).parts[0] in ("internal", "examples", "cmd") or u in test_only_refs:
            colocated_notes.append(u)
        else:
            doc_orphans.append(u)

    print(f"=== Documentation Graph Audit ({root}) ===")
    print(f"Scanned {len(md_files)} repository Markdown files.\n")

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
