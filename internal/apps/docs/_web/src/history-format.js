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

// changeLines is a line diff of a revision's text against the live text.
export function changeLines(before, after) {
  const out = [];
  for (const part of diffLines(before, after)) {
    const lines = part.value.replace(/\n$/, "").split("\n");
    const kind = part.added ? "added" : part.removed ? "removed" : "same";
    for (const line of lines) out.push({ kind, line });
  }
  return out;
}

// lineChanges compares a revision's text with the one before it. changed
// holds the 0-based lines of after that are new or changed; removed maps a
// line of after to how many lines were removed just before it (a removal
// at the end maps to the line count).
export function lineChanges(before, after) {
  const changed = new Set(),
    removed = new Map();
  let line = 0;
  const parts = diffLines(before, after);
  parts.forEach((part, i) => {
    const count = part.count ?? part.value.split("\n").length - 1;
    if (part.added) for (let j = 0; j < count; j++) changed.add(line + j);
    // Removed lines next to added ones were replaced: the added lines show
    // the change.
    const replaced = parts[i + 1]?.added || parts[i - 1]?.added;
    if (part.removed && !replaced)
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

// markChanges marks the block tokens of a parsed revision that cover
// changed lines (meta.revChanged) or follow removed lines
// (meta.revRemoved, the number of lines). A paragraph inside a tight list
// renders no tag of its own, so its list item is marked instead. It returns
// how many blocks it marked.
export function markChanges(tokens, { changed, removed, lines }) {
  const items = [];
  let marked = 0;
  const removals = [...removed].sort((a, b) => a[0] - b[0]);
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
        if (!block.meta.revChanged) marked++;
        block.meta.revChanged = true;
        break;
      }
    // Lines removed inside a block change it; lines removed before it
    // show as a marker above it.
    while (removals.length && removals[0][0] < end) {
      const [at, count] = removals.shift();
      if (at > start) {
        if (!block.meta.revChanged) marked++;
        block.meta.revChanged = true;
      } else block.meta.revRemoved = (block.meta.revRemoved || 0) + count;
    }
  }
  // Text removed at the very end has no block after it.
  const tail = removals.reduce((n, [, count]) => n + count, 0);
  return { marked, tail: tail || 0, lines };
}
