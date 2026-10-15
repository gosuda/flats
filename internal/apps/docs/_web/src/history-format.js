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
