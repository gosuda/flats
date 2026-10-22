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

// credit describes what made a revision, for its first line, as parts:
// strings, and {guest: name} for a name a guest typed, which the panel
// draws as text followed by the guest badge (docs-public-edit.md rule 25).
export function credit(r) {
  const authors = (r.authors || []).join(", ");
  switch (r.kind) {
    case "created":
      return ["Created"];
    case "rollback":
      return [
        `Restored revision ${r.rollback_of}` + (authors ? ` · ${authors}` : ""),
      ];
    case "recovered":
      return ["Recovered text"];
    case "safe_point":
      return ["Safe point before guest editing"];
    case "agent":
      return [authors || "Agent"];
  }
  const parts = authors ? [authors] : [];
  for (const name of guestNames(r)) {
    if (parts.length) parts.push(", ");
    parts.push({ guest: name });
  }
  const more = Number.isSafeInteger(r.guests_more) ? r.guests_more : 0;
  if (more > 0)
    parts.push(
      (parts.length ? " and " : "") +
        `${more} ${parts.length ? "more " : ""}guest${more === 1 ? "" : "s"}`,
    );
  return parts.length ? parts : ["Edits"];
}

export const guestNames = (r) =>
  (r.guest_authors || []).filter((n) => typeof n === "string" && n);

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
  const blanks = [];
  // Removed lines next to added visible lines were replaced: the added lines
  // show the change. Replaced by blank lines only, they are a removal.
  const visible = (part) => part?.added && part.value.trim() !== "";
  parts.forEach((part, i) => {
    const count = part.count ?? part.value.split("\n").length - 1;
    // Blank lines render nothing themselves: lines with text are changes.
    if (part.added)
      part.value
        .split("\n")
        .slice(0, count)
        .forEach((text, j) =>
          text.trim() ? changed.add(line + j) : blanks.push(line + j),
        );
    if (part.removed && !visible(parts[i + 1]) && !visible(parts[i - 1]))
      removed.set(line, (removed.get(line) || 0) + count);
    if (!part.removed) line += count;
  });
  // A blank line added between two lines that stayed splits a paragraph or
  // loosens a list: the blocks on both sides changed.
  if (blanks.length) {
    const text = after.split("\n"),
      added = new Set(blanks),
      edited = new Set(changed),
      kept = (l) => text[l]?.trim() && !edited.has(l) && !added.has(l);
    // Each run of added blank lines, judged by the lines around it.
    for (let k = 0; k < blanks.length; ) {
      let end = k;
      while (blanks[end + 1] === blanks[end] + 1) end++;
      const before = blanks[k] - 1,
        after = blanks[end] + 1;
      // A heading is its own block: a blank line next to it changes nothing.
      const heading = (l) => /^ {0,3}#{1,6}(\s|$)/.test(text[l]);
      if (kept(before) && kept(after) && !heading(before) && !heading(after)) {
        changed.add(before);
        changed.add(after);
      }
      k = end + 1;
    }
  }
  return { changed, removed, blank: new Set(blanks), lines: line };
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
// block, such as a link reference definition, is counted as unshown.
// consumers holds the indexes of inline tokens that render differently for
// another reason (links to a changed definition); their blocks are marked.
// It returns {marked, tail, unshown, lines}.
export function markChanges(
  tokens,
  { changed, removed, blank = new Set(), lines },
  consumers,
) {
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
  // The block an inline token renders in, for links that use a changed
  // reference definition.
  let leaf = null;
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.type === "list_item_open") items.push(t);
    else if (t.type === "list_item_close") items.pop();
    if (t.type === "inline" && leaf && consumers?.has(i)) mark(leaf);
    if (!LEAF.has(t.type) || !t.map) continue;
    leaf = target(i);
    const [start, end] = t.map,
      block = target(i);
    block.meta ||= {};
    // Code shows its blank lines: an added one changes the block.
    const code = t.type === "fence" || t.type === "code_block";
    for (let l = start; l < end; l++)
      if (changed.has(l) || (code && blank.has(l))) {
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
