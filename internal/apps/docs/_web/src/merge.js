import { diffChars, diffArrays } from "diff";
import { utf8Bytes, LIMITS } from "./protocol.js";
// Preserve line endings, including the absence of a final newline.
const lines = (text) => {
  const result = [];
  let start = 0;
  for (
    let end = text.indexOf("\n");
    end !== -1;
    end = text.indexOf("\n", start)
  ) {
    result.push(text.slice(start, end + 1));
    start = end + 1;
  }
  if (start < text.length) result.push(text.slice(start));
  return result;
};
export function mergeText(base, ours, theirs, conflict = () => {}) {
  if (ours === base || ours === theirs) return theirs;
  if (theirs === base) return ours;
  // Count before allocating line arrays (a 1 MiB file can contain 1M lines).
  const countLines = (text) => {
    let n = text.endsWith("\n") ? 0 : 1;
    for (let i = text.indexOf("\n"); i !== -1; i = text.indexOf("\n", i + 1))
      if (++n > 16000) break;
    return n;
  };
  const baseCount = countLines(base);
  if (
    baseCount + countLines(ours) > 16000 ||
    baseCount + countLines(theirs) > 16000
  ) {
    conflict(ours);
    return theirs;
  }
  const original = lines(base);
  const edits = (next) => {
    const target = lines(next);
    const changes =
      original.length + target.length > 16000
        ? undefined
        : diffArrays(original, target, {
            maxEditLength: 128,
          });
    if (!changes) return null;
    let pos = 0,
      list = [],
      current;
    for (const c of changes) {
      if (c.added || c.removed) {
        if (!current) current = { start: pos, end: pos, insert: [] };
        if (c.removed) {
          pos += c.value.length;
          current.end = pos;
        } else current.insert.push(...c.value);
      } else {
        if (current) {
          list.push(current);
          current = null;
        }
        pos += c.value.length;
      }
    }
    if (current) list.push(current);
    return list;
  };
  const oursEdits = edits(ours),
    theirsEdits = edits(theirs);
  if (!oursEdits || !theirsEdits) {
    conflict(ours);
    return theirs;
  }
  const overlaps = (a, b) =>
    a.start === a.end && b.start === b.end
      ? a.start === b.start
      : a.start === a.end
        ? a.start > b.start && a.start < b.end
        : b.start === b.end
          ? b.start > a.start && b.start < a.end
          : a.start < b.end && b.start < a.end;
  const kept = oursEdits.filter(
    (a) => !theirsEdits.some((b) => overlaps(a, b)),
  );
  const apply = (edits) => {
    const result = [...original];
    for (const e of [...edits].sort(
      (a, b) => b.start - a.start || b.end - a.end,
    ))
      result.splice(e.start, e.end - e.start, ...e.insert);
    return result.join("");
  };
  const merged = apply([...kept, ...theirsEdits]);
  // A version can include the same replacement plus an adjacent added line.
  const preserved = (a) =>
    theirsEdits.some(
      (b) =>
        a.start === b.start &&
        a.end === b.end &&
        a.insert.length > 0 &&
        b.insert.some((_, i) =>
          a.insert.every((line, j) => b.insert[i + j] === line),
        ),
    );
  const discarded = oursEdits.some((a) => !kept.includes(a) && !preserved(a));
  if (utf8Bytes(merged) > LIMITS.text) {
    conflict(ours);
    return theirs;
  }
  if (discarded) {
    const humanPreferred = apply([
      ...theirsEdits.filter((b) => !oursEdits.some((a) => overlaps(a, b))),
      ...oursEdits,
    ]);
    if (humanPreferred !== merged) conflict(ours);
  }
  return merged;
}
export function applyText(text, next) {
  const old = text.toString();
  if (old === next) return;
  // Bounded Myers diff; pathological input falls back to a common-prefix/suffix edit.
  let changes =
    old.length + next.length > 65536
      ? undefined
      : diffChars(old, next, { maxEditLength: 64 });
  if (!changes) {
    let p = 0,
      end = 0;
    // Native substring comparisons skip unchanged long spans in QuickJS.
    const shared = Math.min(old.length, next.length);
    while (
      p + 1024 <= shared &&
      old.slice(p, p + 1024) === next.slice(p, p + 1024)
    )
      p += 1024;
    while (p < shared && old[p] === next[p]) p++;
    while (
      end + 1024 <= shared - p &&
      old.slice(old.length - end - 1024, old.length - end) ===
        next.slice(next.length - end - 1024, next.length - end)
    )
      end += 1024;
    while (
      end < old.length - p &&
      end < next.length - p &&
      old[old.length - 1 - end] === next[next.length - 1 - end]
    )
      end++;
    // Do not cut a surrogate pair.
    if (p && /[\uD800-\uDBFF]/.test(old[p - 1])) p--;
    if (end && /[\uDC00-\uDFFF]/.test(old[old.length - end])) end--;
    changes = [
      { value: old.slice(0, p) },
      { value: old.slice(p, old.length - end), removed: true },
      { value: next.slice(p, next.length - end), added: true },
      { value: old.slice(old.length - end) },
    ];
  }
  text.doc.transact(() => {
    let pos = 0;
    for (const c of changes) {
      if (c.removed) text.delete(pos, c.value.length);
      else if (c.added) {
        text.insert(pos, c.value);
        pos += c.value.length;
      } else pos += c.value.length;
    }
  }, "activation");
}
