// Formatting of the history panel, kept free of browser APIs for tests.
import { diffLines } from "diff";

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
export function ago(date, now = Date.now()) {
  const seconds = Math.round((date.getTime() - now) / 1000),
    abs = Math.abs(seconds);
  if (abs < 45) return relative.format(0, "second");
  for (const [unit, size] of [
    ["minute", 60],
    ["hour", 3600],
    ["day", 86400],
    ["week", 604800],
  ])
    if (
      abs <
      size *
        (unit === "week" ? 5 : unit === "day" ? 7 : unit === "hour" ? 24 : 60)
    )
      return relative.format(Math.round(seconds / size), unit);
  return date.toLocaleDateString();
}

// who describes what made a revision, for its first line.
export function who(r) {
  const authors = (r.authors || []).join(", ");
  switch (r.kind) {
    case "created":
      return "Created";
    case "rollback":
      return (
        `Restored revision ${r.rollback_of}` + (authors ? ` · ${authors}` : "")
      );
    case "recovered":
      return "Recovered text";
    case "agent":
      return authors || "Agent";
    default:
      return authors || "Edits";
  }
}

export const isMarkdown = (p) => /\.(md|markdown)$/i.test(p);
const baseName = (p) =>
  p
    .split("/")
    .pop()
    .replace(/\.(md|markdown)$/i, "");

// changed lists the documents a revision changed, by title.
export function changed(r, titles, current) {
  const c = r.changes || {},
    paths = [
      ...(c.added || []),
      ...(c.changed || []),
      ...(c.removed || []),
    ].filter(isMarkdown);
  if (!paths.length) return r.kind === "created" ? "" : "No text changes";
  const names = paths.map((p) =>
    p === current ? "this document" : titles.get(p) || baseName(p),
  );
  const shown = names.slice(0, 3).join(", ");
  return names.length > 3 ? `${shown} and ${names.length - 3} more` : shown;
}

// The page diffs on its main thread: a comparison that would take long, as
// for two very different 1 MiB texts, stops and reports that instead.
const DIFF_LIMITS = { timeout: 400, maxEditLength: 2000 };

// changeLines is a line diff of a revision's text against another text, or
// null when the texts differ too much to compare here.
export function changeLines(before, after) {
  const parts = diffLines(before, after, DIFF_LIMITS);
  if (!parts) return null;
  const out = [];
  for (const part of parts) {
    const lines = part.value.replace(/\n$/, "").split("\n");
    const kind = part.added ? "added" : part.removed ? "removed" : "same";
    for (const line of lines) out.push({ kind, line });
  }
  return out;
}

// lineChanges compares a revision's text with the one before it. changed
// holds the 0-based lines of after that are new or changed; removed maps a
// line of after to how many lines were removed just before it (a removal
// at the end maps to the line count). It returns null when the texts differ
// too much to compare here.
export function lineChanges(before, after) {
  const changed = new Set(),
    removed = new Map(),
    parts = diffLines(before, after, DIFF_LIMITS);
  if (!parts) return null;
  let line = 0;
  // Removed lines next to added visible lines were replaced: the added lines
  // show the change. Replaced by blank lines only, they are a removal.
  const visible = (part) => part?.added && part.value.trim() !== "";
  parts.forEach((part, i) => {
    const count = part.count ?? part.value.split("\n").length - 1;
    // Blank lines render nothing: only lines with text are changes.
    if (part.added)
      part.value
        .split("\n")
        .slice(0, count)
        .forEach((text, j) => text.trim() && changed.add(line + j));
    if (part.removed && !visible(parts[i + 1]) && !visible(parts[i - 1]))
      removed.set(line, (removed.get(line) || 0) + count);
    if (!part.removed) line += count;
  });
  return { changed, removed, lines: line };
}

// Block tokens a change is shown on: the innermost rendered block of the
// Markdown source lines it covers.
const LEAF = new Set([
  "paragraph_open",
  "heading_open",
  "fence",
  "code_block",
  "hr",
  "tr_open",
]);
// Containers a change falls back to when no leaf covers its line, as the
// delimiter row of a table does.
const CONTAINER = new Set([
  "table_open",
  "blockquote_open",
  "bullet_list_open",
  "ordered_list_open",
]);

// markChanges marks the block tokens of a parsed revision that cover
// changed lines (meta.revChanged) or follow removed lines
// (meta.revRemoved, the number of lines). A paragraph inside a tight list
// renders no tag of its own, so its list item is marked instead. A changed
// line no leaf covers marks its innermost container; one outside every
// block, such as a link reference definition, is counted as unshown. It
// returns {marked, tail, unshown, lines}.
export function markChanges(tokens, { changed, removed, lines }) {
  const items = [],
    covered = new Set();
  let marked = 0;
  const removals = [...removed].sort((a, b) => a[0] - b[0]);
  const mark = (block) => {
    block.meta ||= {};
    if (!block.meta.revChanged) marked++;
    block.meta.revChanged = true;
  };
  const target = (i) => {
    const t = tokens[i];
    return t.type === "paragraph_open" && t.hidden && items.length
      ? items[items.length - 1]
      : t;
  };
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.type === "list_item_open") items.push(t);
    else if (t.type === "list_item_close") items.pop();
    if (!LEAF.has(t.type) || !t.map) continue;
    const [start, end] = t.map,
      block = target(i);
    block.meta ||= {};
    for (let l = start; l < end; l++)
      if (changed.has(l)) {
        covered.add(l);
        mark(block);
      }
    // Lines removed inside a block change it; lines removed before it
    // show as a marker above it.
    while (removals.length && removals[0][0] < end) {
      const [at, count] = removals.shift();
      if (at > start) mark(block);
      else block.meta.revRemoved = (block.meta.revRemoved || 0) + count;
    }
  }
  let unshown = 0;
  const containers = tokens.filter((t) => CONTAINER.has(t.type) && t.map);
  for (const l of changed) {
    if (covered.has(l)) continue;
    let inner = null;
    for (const t of containers) if (t.map[0] <= l && l < t.map[1]) inner = t;
    if (inner) mark(inner);
    else unshown++;
  }
  // Text removed at the very end has no block after it.
  const tail = removals.reduce((n, [, count]) => n + count, 0);
  return { marked, tail: tail || 0, unshown, lines };
}
