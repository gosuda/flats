#!/usr/bin/env python3
"""Structure-agnostic Markdown link, anchor, and reachability auditor.

Does not hardcode directory layouts or file naming conventions:
1. Discovers non-ignored repository files via `git ls-files --cached --others --exclude-standard`
   from the Git top-level root (covering both tracked and newly created unstaged docs while
   rejecting links to gitignored or outside-repo paths).
2. Validates relative Markdown/reference/HTML links, case-exact file names, and case-sensitive
   `#anchor` fragments (ATX & Setext headings plus HTML id/name attributes inside HTML tags).
3. Builds a BFS reachability graph starting from root `README.md` and runtime/build code entrypoints
   (`//go:embed`, build/config files) while reporting test-only or colocated Markdown files separately.
4. Flags relative links inside `docs/agent/` (which break when served via MCP/llms.txt)
   and lines containing 40-char (SHA-1) or 64-char (SHA-256) hex hashes.
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
FENCE_OPEN_RE = re.compile(r"^[ \t]{0,3}(`{3,}|~{3,})")
THEMATIC_BREAK_RE = re.compile(r"^[ \t]{0,3}(?:(\*[ \t]*){3,}|(-[ \t]*){3,}|(_[ \t]*){3,})$")
LIST_MARKER_RE = re.compile(r"^([ \t]*)(?:[-*+]|\d+[.)])([ \t]+)")
HEADING_RE = re.compile(r"^[ \t]{0,3}(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$")
SETEXT_UNDERLINE_RE = re.compile(r"^[ \t]{0,3}(=+|-+)[ \t]*$")
REF_DEF_RE = re.compile(
    r"^[ \t]{0,3}\[([^\^\]][^\]]*|[^^\]]?)\]:[ \t]*(\S+)(?:[ \t]+(?:\"[^\"]*\"|'[^']*'|\([^)]*\)))?[ \t]*$"
)
HTML_LINK_TAG_NAMES = {"a", "img", "link", "script", "source"}
EMBED_LINE_RE = re.compile(r"^[ \t]*//go:embed[ \t]+(.+)$", re.MULTILINE)
QUOTED_MD_RE = re.compile(r"""["'`]([A-Za-z0-9_./*-]+\.(?:md|markdown))["'`]""")

CODE_SUFFIXES = {".go", ".mjs", ".js", ".ts", ".sh", ".py", ".yml", ".yaml", ".json"}
TEST_FILE_RE = re.compile(
    r"(?:_test\.go$|(?:^|[._-])(?:test|tests)\.[A-Za-z0-9]+$|(?:^|[._-])spec\.(?:mjs|js|ts|py|sh)$|^test_)",
    re.IGNORECASE,
)


class _DocHTMLScanner(html.parser.HTMLParser):
    """Spec-compliant HTML start-tag scanner for links (href/src) and anchors (id/name)."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.anchors: set[str] = set()
        self.links: list[tuple[int, str]] = []

    def _record_tag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        tag_l = tag.lower()
        line_no = self.getpos()[0]
        for attr_name, attr_val in attrs:
            if not attr_val:
                continue
            name_l = attr_name.lower()
            val = attr_val.strip()
            if not val:
                continue
            if name_l == "id" or (name_l == "name" and tag_l == "a"):
                self.anchors.add(val)
            if tag_l in HTML_LINK_TAG_NAMES and name_l in ("href", "src"):
                self.links.append((line_no, val))

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self._record_tag(tag, attrs)

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self._record_tag(tag, attrs)


def scan_html_in_prose(prose_no_inline: str) -> tuple[set[str], list[tuple[int, str]]]:
    scanner = _DocHTMLScanner()
    try:
        scanner.feed(prose_no_inline)
        scanner.close()
    except Exception:
        pass
    return scanner.anchors, scanner.links


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
    """Return all tracked + untracked non-ignored files in the repository."""
    out = subprocess.check_output(
        ["git", "-C", str(root), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        stderr=subprocess.PIPE,
    )
    rels = {p for p in out.decode("utf-8", errors="replace").split("\0") if p}
    return sorted((root / rel) for rel in rels if (root / rel).is_file())


def visual_width(s: str) -> int:
    col = 0
    for ch in s:
        if ch == "\t":
            col += 4 - (col % 4)
        else:
            col += 1
    return col


def leading_spaces(line: str) -> int:
    col = 0
    for ch in line:
        if ch == " ":
            col += 1
        elif ch == "\t":
            col += 4 - (col % 4)
        else:
            break
    return col


def strip_fences_and_comments(text: str) -> str:
    """Replace fenced code blocks, indented code blocks, and HTML comments with newlines."""
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
    prev_blank = True
    in_indented_code = False
    list_stack: list[int] = []

    for line in lines:
        if not in_fence:
            m = FENCE_OPEN_RE.match(line)
            if m:
                marker = m.group(1)
                in_fence = True
                in_indented_code = False
                fence_char = marker[0]
                fence_len = len(marker)
                out.append("\n" if line.endswith("\n") else "")
                continue

            is_blank = line.strip() == ""
            if is_blank:
                prev_blank = True
                out.append(line)
                continue

            indent = leading_spaces(line)

            # Pop list contexts whose content column is strictly greater than this line's indent
            # unless this line is a new sibling list item at that level.
            while list_stack and indent < list_stack[-1]:
                list_stack.pop()

            code_threshold = (list_stack[-1] + 4) if list_stack else 4
            if indent >= code_threshold and (prev_blank or in_indented_code):
                in_indented_code = True
                out.append("\n" if line.endswith("\n") else "")
                continue

            in_indented_code = False
            if not THEMATIC_BREAK_RE.match(line):
                lm = LIST_MARKER_RE.match(line)
                if lm and indent < code_threshold:
                    list_stack.append(visual_width(lm.group(0)))

            prev_blank = False
            out.append(line)
        else:
            close_re = re.compile(rf"^[ \t]{{0,3}}{re.escape(fence_char)}{{{fence_len},}}[ \t]*\r?\n?$")
            if close_re.match(line):
                in_fence = False
                prev_blank = True
            out.append("\n" if line.endswith("\n") else "")
    return "".join(out)


def strip_inline_code(text: str) -> str:
    """Replace inline code spans (`...`, ``...``) with spaces, preserving newlines.

    Does not match across blank lines (paragraph boundaries) so a stray unmatched
    backtick cannot blank out subsequent paragraphs.
    """
    return re.sub(
        r"(`+)((?:(?!\n[ \t]*\n).)+?)\1",
        lambda m: "".join("\n" if ch == "\n" else " " for ch in m.group(0)),
        text,
        flags=re.DOTALL,
    )


def github_slug(heading_text: str) -> str:
    # Strip HTML tags only outside inline code spans, while keeping text inside `...`
    parts = re.split(r"(`+[^`]*`+)", heading_text)
    cleaned_parts: list[str] = []
    for part in parts:
        if part.startswith("`") and part.endswith("`"):
            cleaned_parts.append(part.strip("`"))
        else:
            cleaned_parts.append(re.sub(r"<[^>]+>", "", part))
    s = "".join(cleaned_parts)
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


def build_line_starts(text: str) -> list[int]:
    starts = [0]
    for idx, ch in enumerate(text):
        if ch == "\n":
            starts.append(idx + 1)
    return starts


def line_of_offset(line_starts: list[int], pos: int) -> int:
    lo, hi = 0, len(line_starts) - 1
    while lo <= hi:
        mid = (lo + hi) // 2
        if line_starts[mid] <= pos:
            lo = mid + 1
        else:
            hi = mid - 1
    return hi + 1


def extract_anchors(prose_with_inline: str, html_anchors: set[str], prose_no_inline: str) -> set[str]:
    heading_anchors: set[str] = set()
    counts: dict[str, int] = {}

    def register_heading(raw_heading: str) -> None:
        base = github_slug(raw_heading)
        if not base:
            return
        idx = counts.get(base, 0)
        counts[base] = idx + 1
        slug = base if idx == 0 else f"{base}-{idx}"
        while slug in heading_anchors:
            idx += 1
            counts[base] = idx + 1
            slug = f"{base}-{idx}"
        heading_anchors.add(slug)

    lines_with_inline = prose_with_inline.splitlines()
    lines_no_inline = prose_no_inline.splitlines()
    n = len(lines_with_inline)
    i = 0
    para_lines: list[str] = []
    while i < n:
        line = lines_with_inline[i]
        no_inline = lines_no_inline[i]
        m = HEADING_RE.match(line)
        if m:
            para_lines.clear()
            register_heading(m.group(2))
            i += 1
            continue

        if para_lines and SETEXT_UNDERLINE_RE.match(no_inline):
            register_heading(" ".join(p.strip() for p in para_lines))
            para_lines.clear()
            i += 1
            continue

        stripped = no_inline.strip()
        if not stripped or REF_DEF_RE.match(no_inline) or stripped.startswith(("#", "-", "*", "+", ">", "|")):
            para_lines.clear()
        else:
            para_lines.append(line)
        i += 1

    return heading_anchors | html_anchors


def clean_destination(raw_dest: str) -> str:
    s = raw_dest.strip()
    if s.startswith("<") and ">" in s:
        return s[1 : s.index(">")].strip()
    m = re.match(r'^(\S+?)(?:\s+(?:"[^"]*"|\'[^\']*\'|\([^)]*\)))?$', s, flags=re.DOTALL)
    if m:
        return m.group(1)
    return s.split()[0] if s.split() else ""


def scan_markdown_links(
    text: str,
    ref_defs: dict[str, tuple[int, str]],
    used_refs: set[str],
) -> list[tuple[int, str]]:
    """Scan full document text for inline `[text](dest)`, `[text][ref]`, and `[ref]`.

    Supports multiline link text/destinations and nested images inside links.
    Returns list of (1-based line_no, resolved_destination).
    """
    results: list[tuple[int, str]] = []
    line_starts = build_line_starts(text)

    n = len(text)
    i = 0
    while i < n:
        if i == 0 or text[i - 1] == "\n":
            line_end = text.find("\n", i)
            if line_end == -1:
                line_end = n
            if REF_DEF_RE.match(text[i:line_end]):
                i = line_end + 1
                continue

        if text[i] == "\\":
            i += 2
            continue

        if text[i] == "[":
            depth = 1
            j = i + 1
            while j < n and depth > 0:
                if text[j] == "\\":
                    j += 2
                    continue
                if text[j] == "\n" and j + 1 < n and text[j + 1] == "\n":
                    break
                if text[j] == "[":
                    depth += 1
                elif text[j] == "]":
                    depth -= 1
                j += 1

            if depth == 0:
                label_text = text[i + 1 : j - 1]
                if "[" in label_text:
                    inner_line_base = line_of_offset(line_starts, i + 1) - 1
                    for sub_line, sub_dest in scan_markdown_links(label_text, ref_defs, used_refs):
                        results.append((inner_line_base + sub_line, sub_dest))

                if j < n and text[j] == "(":
                    p_depth = 1
                    k = j + 1
                    in_angles = False
                    while k < n and p_depth > 0:
                        if text[k] == "\\":
                            k += 2
                            continue
                        if text[k] == "\n" and k + 1 < n and text[k + 1] == "\n":
                            break
                        if text[k] == "<":
                            in_angles = True
                        elif text[k] == ">":
                            in_angles = False
                        elif not in_angles:
                            if text[k] == "(":
                                p_depth += 1
                            elif text[k] == ")":
                                p_depth -= 1
                        k += 1
                    if p_depth == 0:
                        dest = clean_destination(text[j + 1 : k - 1])
                        if dest:
                            results.append((line_of_offset(line_starts, i), dest))
                        i = k
                        continue
                elif j < n and text[j] == "[":
                    k = text.find("]", j + 1)
                    if k != -1 and "\n\n" not in text[j + 1 : k]:
                        ref_label = (" ".join(text[j + 1 : k].split()) or " ".join(label_text.split())).lower()
                        if ref_label in ref_defs:
                            used_refs.add(ref_label)
                            results.append((line_of_offset(line_starts, i), ref_defs[ref_label][1]))
                        i = k + 1
                        continue
                else:
                    norm_label = " ".join(label_text.split()).lower()
                    if norm_label and norm_label in ref_defs:
                        used_refs.add(norm_label)
                        results.append((line_of_offset(line_starts, i), ref_defs[norm_label][1]))
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
    extra_ref_defs: list[tuple[int, str]] = []
    used_refs: set[str] = set()
    raw_occurrences: list[tuple[int, str]] = []

    for idx, line in enumerate(lines, start=1):
        m = REF_DEF_RE.match(line)
        if m:
            label = " ".join(m.group(1).split()).lower()
            dest = clean_destination(m.group(2))
            if label and dest:
                # CommonMark: first reference definition wins; keep later duplicates for validation too
                if label not in ref_defs:
                    ref_defs[label] = (idx, dest)
                else:
                    extra_ref_defs.append((idx, dest))

    raw_occurrences.extend(scan_markdown_links(prose_no_inline, ref_defs, used_refs))

    for label, (def_line, dest) in ref_defs.items():
        if label not in used_refs:
            raw_occurrences.append((def_line, dest))
    raw_occurrences.extend(extra_ref_defs)
    raw_occurrences.extend(html_links)

    links: list[LinkRef] = []
    for line_no, raw in raw_occurrences:
        if not raw or raw.startswith("//") or SCHEME_RE.match(raw):
            continue
        if raw.startswith("#"):
            links.append(LinkRef(line_no, raw, md_file, urllib.parse.unquote(raw[1:].strip())))
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

        norm = pathlib.Path(os.path.normpath(str(candidate)))
        links.append(LinkRef(line_no, raw, norm, frag))

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


def is_test_source(rel_parts: tuple[str, ...], file_name: str) -> bool:
    if any(p in ("testdata", "test", "tests", "__tests__") for p in rel_parts[:-1]):
        return True
    return bool(TEST_FILE_RE.search(file_name))


def discover_code_references(
    root: pathlib.Path,
    repo_files: list[pathlib.Path],
    md_set: set[pathlib.Path],
) -> tuple[set[pathlib.Path], set[pathlib.Path]]:
    """Return (runtime_seeds, test_only_refs)."""
    runtime_seeds: set[pathlib.Path] = set()
    test_refs: set[pathlib.Path] = set()

    for f in repo_files:
        if f.suffix.lower() not in CODE_SUFFIXES:
            continue
        rel_parts = f.relative_to(root).parts
        if rel_parts and rel_parts[0] in (".cursor", ".claude", ".agents"):
            continue
        is_test = is_test_source(rel_parts, f.name)
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
        prose_with_inline = strip_fences_and_comments(raw_text)
        prose_no_inline = strip_inline_code(prose_with_inline)
        html_anchors, html_links = scan_html_in_prose(prose_no_inline)

        anchors_by_file[md] = extract_anchors(prose_with_inline, html_anchors, prose_no_inline)
        links = parse_links_in_file(root, md, prose_no_inline, html_links)
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
                known_anchors = anchors_by_file.get(target_md, set())
                if lk.fragment not in known_anchors:
                    broken_links.append(
                        f"{rel_md}:{lk.line_no}: broken anchor '{lk.raw}' (#{lk.fragment} not found in {target_md.relative_to(root)})"
                    )

    # Reachability BFS from root README.md + runtime/build code seeds
    runtime_seeds, test_only_refs = discover_code_references(root, repo_files, md_set)
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
            elif lk.target_path in repo_dirs:
                dir_readme = lk.target_path / "README.md"
                if dir_readme in md_set:
                    candidates.append(dir_readme)
            elif lk.target_path in repo_file_set:
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
