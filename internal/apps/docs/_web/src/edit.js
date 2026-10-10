// Guarded live edits for host management clients (MCP update_document).
// planEdit is pure: it resolves every operation against the committed text,
// in order, and either returns splices or throws without changing anything.
export const EDIT_LIMITS = Object.freeze({
  ops: 32,
  body: 512 * 1024,
  outline: 1000,
  // Block operations index and hash every block, which costs about 30 µs
  // per block in the worker; this keeps a call well inside its deadline.
  blocks: 50000,
});
const fence = /^ {0,3}(`{3,}|~{3,})/,
  heading = /^ {0,3}(#{1,6})(?:[ \t]|$)/,
  blank = /^[ \t\r]*$/;

function fail(code, message, op) {
  throw Object.assign(new Error(message), {
    code,
    ...(op === undefined ? {} : { op }),
  });
}

// Blocks are maximal runs of non-blank lines. A fenced code block is one
// block including its blank lines, and an ATX heading line is always its own
// block. start/end are UTF-16 offsets; end excludes the trailing newline.
export const blocks = (text) => scan(text, 0).blocks;

// scan parses blocks from pos, a line start outside any block. Before each
// new block it asks stop(start); a non-negative answer ends the scan there
// and is returned as next.
function scan(text, pos, stop) {
  const out = [];
  let current = null,
    open = null;
  const close = () => {
    if (current) out.push(current);
    current = null;
  };
  let end = 0;
  const lineText = () => text.slice(pos, end);
  while (pos <= text.length) {
    const nl = text.indexOf("\n", pos);
    end = nl === -1 ? text.length : nl;
    if (pos === text.length && nl === -1) break;
    // Cheap character checks on the text itself; a line is sliced only for
    // the rare regular expression, which keeps the split linear and fast.
    let j = 0;
    while (j < 3 && text.charCodeAt(pos + j) === 32) j++;
    const c = text.charCodeAt(pos),
      d = pos + j < end ? text.charCodeAt(pos + j) : -1,
      line = lineText;
    if (open) {
      current.end = end;
      const m = d === 96 || d === 126 ? fence.exec(line()) : null;
      if (
        m &&
        m[1][0] === open[0] &&
        m[1].length >= open.length &&
        blank.test(line().slice(m[0].length))
      ) {
        open = null;
        close();
      }
    } else if (
      pos === end ||
      ((c === 32 || c === 9 || c === 13) && blank.test(line()))
    )
      close();
    else {
      const f = d === 96 || d === 126 ? fence.exec(line()) : null,
        h = d === 35 ? heading.exec(line()) : null,
        isFence =
          f && !(f[1][0] === "`" && line().slice(f[0].length).includes("`"));
      if (isFence || h || !current) {
        close();
        const next = stop ? stop(pos) : -1;
        if (next >= 0) return { blocks: out, next };
      }
      if (isFence) {
        current = { start: pos, end, kind: "code" };
        open = f[1];
      } else if (h) {
        out.push({ start: pos, end, kind: "heading", level: h[1].length });
      } else if (current) current.end = end;
      else current = { start: pos, end, kind: "text" };
    }
    if (nl === -1) break;
    pos = nl + 1;
  }
  close();
  return { blocks: out, next: -1 };
}

// reindex updates list (blocks of the text before splice s) for the text
// after it. It re-parses from the block before the change only until a new
// block starts where an unchanged old block started, then reuses and shifts
// the rest, so each operation costs about the size of the changed region.
export function reindex(index, text, s, digest) {
  const list = index.list;
  const delta = s.insert.length - s.remove,
    changeEnd = s.at + s.insert.length;
  // Last block starting at or before the change.
  let lo = 0,
    hi = list.length - 1,
    idx = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (list[mid].start <= s.at) {
      idx = mid;
      lo = mid + 1;
    } else hi = mid - 1;
  }
  const k = idx >= 1 ? idx - 1 : 0,
    from = idx >= 1 ? list[k].start : 0;
  const oldAt = (start) => {
    let lo = k,
      hi = list.length - 1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      if (list[mid].start === start) return mid;
      if (list[mid].start < start) lo = mid + 1;
      else hi = mid - 1;
    }
    return -1;
  };
  const r = scan(text, from, (start) =>
    start >= changeEnd ? oldAt(start - delta) : -1,
  );
  const next = r.next >= 0 ? r.next : list.length,
    removed = list.splice(k, next - k, ...r.blocks);
  if (delta)
    for (let t = k + r.blocks.length; t < list.length; t++) {
      list[t].start += delta;
      list[t].end += delta;
    }
  if (index.byHash) {
    for (const b of removed) {
      const bucket = index.byHash.get(b.hash);
      bucket.splice(bucket.indexOf(b), 1);
    }
    hashInto(r.blocks, text, digest);
    for (const b of r.blocks) addHash(index.byHash, b);
  }
}

// Hashing every block one host call at a time is slow in the worker, so
// blocks are hashed in batches when the host codec is available.
export function hashBlocks(bodies, digest) {
  const codec = globalThis.__flats_docsCodec;
  if (!codec?.digests) return bodies.map((b) => blockHash(b, digest));
  const out = [];
  for (let i = 0; i < bodies.length; ) {
    let size = 0,
      j = i;
    while (j < bodies.length && (j === i || size + bodies[j].length < 1 << 20))
      size += bodies[j++].length;
    const all = codec.digests(bodies.slice(i, j));
    for (let k = 0; k < j - i; k++) out.push(all.slice(k * 64, k * 64 + 16));
    i = j;
  }
  return out;
}
function hashInto(list, text, digest) {
  const need = list.filter((b) => b.hash === undefined),
    hashes = hashBlocks(
      need.map((b) => text.slice(b.start, b.end)),
      digest,
    );
  need.forEach((b, i) => (b.hash = hashes[i]));
}
function addHash(map, b) {
  const bucket = map.get(b.hash);
  if (bucket) bucket.push(b);
  else map.set(b.hash, [b]);
}

// Native string operations count newlines much faster than a loop.
const newlines = (text, a, b) => {
  const part = text.slice(a, b);
  return part.length - part.replaceAll("\n", "").length;
};

// outline lists one page of blocks with the guard hash a caller passes
// back, and the total block count. Line numbers are counted in one pass.
export function outline(text, digest, offset = 0, limit = EDIT_LIMITS.outline) {
  const list = blocks(text),
    page = [];
  let pos = 0,
    line = 1;
  for (let k = offset; k < list.length && page.length < limit; k++) {
    const b = list[k];
    for (
      let i = text.indexOf("\n", pos);
      i !== -1 && i < b.start;
      i = text.indexOf("\n", i + 1)
    )
      line++;
    pos = b.start;
    const body = text.slice(b.start, b.end);
    page.push({
      body,
      kind: b.kind,
      ...(b.level ? { level: b.level } : {}),
      line,
      preview: preview(body),
    });
  }
  const hashes = hashBlocks(
    page.map((p) => p.body),
    digest,
  );
  return {
    blocks: page.map(({ body, ...p }, i) => ({ hash: hashes[i], ...p })),
    total: list.length,
  };
}
export const blockHash = (body, digest) => digest(body).slice(0, 16);
function preview(body) {
  const first = body.slice(0, 200).split("\n", 1)[0];
  const chars = Array.from(first);
  return chars.length > 60 ? chars.slice(0, 60).join("") + "…" : first;
}

const str = (v, name, i, { empty = false } = {}) => {
  if (typeof v !== "string")
    fail("invalid", `op ${i + 1}: ${name} must be a string`, i);
  if (!empty && v === "")
    fail("invalid", `op ${i + 1}: ${name} must not be empty`, i);
  return v;
};
const nthOf = (v, i) => {
  if (v === undefined || v === null) return 0;
  if (!Number.isSafeInteger(v) || v < 1)
    fail("invalid", `op ${i + 1}: nth must be a positive integer`, i);
  return v;
};
const allowed = {
  replace: ["find", "with", "nth"],
  replace_block: ["block", "with", "nth"],
  delete_block: ["block", "nth"],
  insert: ["text", "before", "after", "section_end", "at", "nth"],
};

function pick(list, nth, i, what, hint) {
  if (!list.length)
    fail("edit_conflict", `op ${i + 1}: ${what} not found; ${hint}`, i);
  if (nth) {
    if (nth > list.length)
      fail(
        "edit_conflict",
        `op ${i + 1}: nth ${nth} but ${what} occurs ${list.length} time(s)`,
        i,
      );
    return list[nth - 1];
  }
  if (list.length > 1)
    fail(
      "edit_conflict",
      `op ${i + 1}: ${what} occurs more than once; add nth or make it unique`,
      i,
    );
  return list[0];
}

// Block hashes are memoized on the block objects reindex keeps, and looked
// up through a hash map that reindex maintains.
function findBlock(text, index, hash, nth, i, digest) {
  if (typeof hash !== "string" || !/^[0-9a-f]{16}$/.test(hash))
    fail(
      "invalid",
      `op ${i + 1}: block must be a 16-hex hash from get_document blocks`,
      i,
    );
  if (!index.byHash) {
    hashInto(index.list, text, digest);
    index.byHash = new Map();
    for (const b of index.list) addHash(index.byHash, b);
  }
  return pick(
    [...(index.byHash.get(hash) || [])].sort((a, b) => a.start - b.start),
    nth,
    i,
    `block ${hash}`,
    "it was changed or removed since you read it; read get_document again",
  );
}

// Shrinks a splice to the characters that actually change, without cutting a
// surrogate pair, so concurrent edits next to unchanged text keep their place.
function minimal(text, at, remove, insert) {
  const old = text.slice(at, at + remove);
  let p = 0,
    e = 0;
  while (p < old.length && p < insert.length && old[p] === insert[p]) p++;
  if (p && /[\uD800-\uDBFF]/.test(old[p - 1])) p--;
  while (
    e < old.length - p &&
    e < insert.length - p &&
    old[old.length - 1 - e] === insert[insert.length - 1 - e]
  )
    e++;
  if (e && /[\uDC00-\uDFFF]/.test(old[old.length - e])) e--;
  return {
    at: at + p,
    remove: old.length - p - e,
    insert: insert.slice(p, insert.length - e),
  };
}

// planEdit applies ops in order to text and returns the resulting text,
// the splices to replay on Y.Text, and a per-operation summary.
export function planEdit(text, request, hashText) {
  const digest = hashText;
  // The block index is built once, when an operation first needs it.
  let index = null;
  if (!request || typeof request !== "object" || Array.isArray(request))
    fail("invalid", "body must be an object with ops");
  for (const key of Object.keys(request))
    if (key !== "ops" && key !== "if_hash")
      fail("invalid", `unknown field ${key}`);
  const ops = request.ops;
  if (!Array.isArray(ops) || !ops.length)
    fail("invalid", "ops must be a non-empty array");
  if (ops.length > EDIT_LIMITS.ops)
    fail("invalid", `at most ${EDIT_LIMITS.ops} ops per call`);
  // nth is positional: a concurrent insertion of the same text earlier in
  // the document would silently move it, so it must be pinned to the text
  // that was read.
  if (
    request.if_hash === undefined &&
    ops.some((op) => op && op.nth !== undefined && op.nth !== null)
  )
    fail(
      "invalid",
      "nth selects by position; also send if_hash from get_document so a concurrent edit cannot move the match",
    );
  if (request.if_hash !== undefined) {
    if (
      typeof request.if_hash !== "string" ||
      !/^[0-9a-f]{64}$/.test(request.if_hash)
    )
      fail("invalid", "if_hash must be the 64-hex hash from get_document");
    if (hashText(text) !== request.if_hash)
      fail(
        "edit_conflict",
        "the document changed since you read it (if_hash differs); read get_document again",
      );
  }
  const splices = [],
    summary = [];
  // Line numbers count from the previous operation's position, which a
  // later splice (always at or after it) does not move.
  let mark = { at: 0, line: 1 };
  const lineOf = (text, at) => {
    const line =
      at >= mark.at
        ? mark.line + newlines(text, mark.at, at)
        : mark.line - newlines(text, at, mark.at);
    mark = { at, line };
    return line;
  };
  ops.forEach((op, i) => {
    if (!op || typeof op !== "object" || Array.isArray(op))
      fail("invalid", `op ${i + 1} must be an object`, i);
    const keys = allowed[op.op];
    if (!keys)
      fail(
        "invalid",
        `op ${i + 1}: op must be replace, replace_block, delete_block or insert`,
        i,
      );
    for (const key of Object.keys(op))
      if (key !== "op" && !keys.includes(key))
        fail("invalid", `op ${i + 1}: unknown field ${key} for ${op.op}`, i);
    const nth = nthOf(op.nth, i);
    let at, remove, insert;
    if (op.op === "replace") {
      const find = str(op.find, "find", i),
        found = [];
      for (
        let p = text.indexOf(find);
        p !== -1;
        p = text.indexOf(find, p + 1)
      ) {
        found.push(p);
        // Scan only as far as needed: through nth, or to a second match.
        if (found.length >= (nth || 2)) break;
      }
      at = pick(
        found,
        nth,
        i,
        "find text",
        "the text changed since you read it; read get_document again",
      );
      remove = find.length;
      insert = str(op.with, "with", i, { empty: true });
    } else {
      if (!index) {
        index = { list: blocks(text), byHash: null };
        if (index.list.length > EDIT_LIMITS.blocks)
          fail(
            "capacity",
            `op ${i + 1}: the document has ${index.list.length} blocks; block operations support at most ${EDIT_LIMITS.blocks}. Use replace with find text instead`,
            i,
          );
      }
      const list = index.list;
      if (op.op === "replace_block" || op.op === "delete_block") {
        const b = findBlock(text, index, op.block, nth, i, digest);
        if (op.op === "replace_block") {
          insert = str(op.with, "with", i).replace(/^\n+|\n+$/g, "");
          if (!insert.trim())
            fail("invalid", `op ${i + 1}: with is blank; use delete_block`, i);
          at = b.start;
          remove = b.end - b.start;
        } else {
          const k = list.indexOf(b),
            next = list[k + 1],
            prev = list[k - 1];
          insert = "";
          if (next) {
            at = b.start;
            remove = next.start - b.start;
          } else if (prev) {
            at = prev.end;
            remove = b.end - prev.end;
          } else {
            at = b.start;
            remove = b.end - b.start;
          }
        }
      } else {
        const body = str(op.text, "text", i).replace(/^\n+|\n+$/g, "");
        if (!body.trim()) fail("invalid", `op ${i + 1}: text is blank`, i);
        const anchors = ["before", "after", "section_end", "at"].filter(
          (k) => op[k] !== undefined,
        );
        if (anchors.length !== 1)
          fail(
            "invalid",
            `op ${i + 1}: insert needs exactly one of before, after, section_end or at`,
            i,
          );
        const anchor = anchors[0];
        let target, where;
        if (anchor === "at") {
          if (op.at !== "start" && op.at !== "end")
            fail("invalid", `op ${i + 1}: at must be start or end`, i);
          if (nth)
            fail(
              "invalid",
              `op ${i + 1}: nth applies to block anchors only`,
              i,
            );
          where = op.at;
        } else {
          target = findBlock(text, index, op[anchor], nth, i, digest);
          where = anchor === "before" ? "before" : "after";
          if (anchor === "section_end") {
            if (target.kind !== "heading")
              fail(
                "invalid",
                `op ${i + 1}: section_end needs a heading block`,
                i,
              );
            const k = list.indexOf(target);
            let last = k;
            while (
              last + 1 < list.length &&
              !(
                list[last + 1].kind === "heading" &&
                list[last + 1].level <= target.level
              )
            )
              last++;
            target = list[last];
          }
        }
        remove = 0;
        if (where === "start") {
          // The very start of the text, before any leading blank lines.
          at = 0;
          insert = body + (text.startsWith("\n") ? "\n" : text ? "\n\n" : "\n");
        } else if (where === "end") {
          // The very end, after any trailing blank lines.
          at = text.length;
          const trailing = text.length - text.replace(/\n+$/, "").length;
          insert =
            (!text.trim() ? "" : (["\n\n", "\n"][trailing] ?? "")) +
            body +
            "\n";
        } else {
          // Keep a blank line on both sides, also next to a heading or a
          // fence that has no blank line before its neighbour.
          const k = list.indexOf(target),
            separated = (a, b) => text.slice(a, b).split("\n").length - 1 >= 2;
          if (where === "before") {
            const prev = list[k - 1];
            at = target.start;
            insert =
              (prev && !separated(prev.end, target.start) ? "\n" : "") +
              body +
              "\n\n";
          } else {
            const next = list[k + 1];
            at = target.end;
            insert =
              "\n\n" +
              body +
              (next && !separated(target.end, next.start) ? "\n" : "");
            if (target.end === text.length) insert += "\n";
          }
        }
      }
    }
    const s = minimal(text, at, remove, insert);
    const line = lineOf(text, at);
    text = text.slice(0, s.at) + s.insert + text.slice(s.at + s.remove);
    if (s.remove || s.insert) {
      splices.push(s);
      if (index) reindex(index, text, s, digest);
    }
    summary.push({
      op: op.op,
      line,
      removed: s.remove,
      inserted: s.insert.length,
    });
  });
  return { text, splices, summary };
}
