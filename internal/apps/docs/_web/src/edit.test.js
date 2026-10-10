import test from "node:test";
import assert from "node:assert/strict";
import * as Y from "yjs";
import { blocks, outline, planEdit, blockHash } from "./edit.js";
import { digest } from "./storage.js";

const hashOf = (text, body) => {
  assert.ok(text.includes(body), body);
  return blockHash(body, digest);
};
// Positional nth needs if_hash; pin it to the text unless a test sets it.
const plan = (text, ops, extra = {}) =>
  planEdit(
    text,
    {
      ops,
      ...(ops.some?.((op) => op?.nth !== undefined)
        ? { if_hash: digest(text) }
        : {}),
      ...extra,
    },
    digest,
  );
const rejects = (fn, code, pattern) =>
  assert.throws(
    fn,
    (e) => e.code === code && (!pattern || pattern.test(e.message)),
  );

const doc = `# Title

Intro line one
intro line two

## Plan

- step one
- step two

\`\`\`js
const a = 1;

const b = 2;
\`\`\`

## Notes

Last paragraph.
`;

test("blocks split on blank lines, keep fences whole and isolate headings", () => {
  const list = blocks(doc).map((b) => [b.kind, doc.slice(b.start, b.end)]);
  assert.deepEqual(list, [
    ["heading", "# Title"],
    ["text", "Intro line one\nintro line two"],
    ["heading", "## Plan"],
    ["text", "- step one\n- step two"],
    ["code", "```js\nconst a = 1;\n\nconst b = 2;\n```"],
    ["heading", "## Notes"],
    ["text", "Last paragraph."],
  ]);
  assert.deepEqual(
    blocks("#tag\n# real\ntext\n~~~\nunclosed\n\nstill code").map(
      (b) => b.kind,
    ),
    ["text", "heading", "text", "code"],
  );
  assert.deepEqual(blocks(""), []);
  assert.deepEqual(blocks("\n\n  \n"), []);
});

test("outline reports hash, kind, level, line and a short preview", () => {
  const o = outline(doc, digest).blocks;
  assert.equal(o.length, 7);
  assert.deepEqual(o[2], {
    hash: hashOf(doc, "## Plan"),
    kind: "heading",
    level: 2,
    line: 6,
    preview: "## Plan",
  });
  assert.equal(o[4].line, 11);
  assert.match(o[0].hash, /^[0-9a-f]{16}$/);
  const long = outline("x".repeat(100), digest).blocks[0].preview;
  assert.equal(long, "x".repeat(60) + "…");
});

test("replace finds exact text, requires uniqueness and honours nth", () => {
  const r = plan(doc, [{ op: "replace", find: "step two", with: "step 2" }]);
  assert.ok(r.text.includes("- step 2\n"));
  assert.deepEqual(r.summary, [
    { op: "replace", line: 9, removed: 3, inserted: 1 },
  ]);
  // Minimal splice: only "two" -> "2" changes.
  assert.deepEqual(r.splices, [
    { at: doc.indexOf("step two") + 5, remove: 3, insert: "2" },
  ]);
  rejects(
    () => plan(doc, [{ op: "replace", find: "step", with: "x" }]),
    "edit_conflict",
    /more than once/,
  );
  const nth = plan(doc, [
    { op: "replace", find: "step", with: "Step", nth: 2 },
  ]);
  assert.ok(nth.text.includes("- step one\n- Step two"));
  rejects(
    () => plan(doc, [{ op: "replace", find: "missing", with: "x" }]),
    "edit_conflict",
    /not found/,
  );
  rejects(
    () => plan(doc, [{ op: "replace", find: "step", with: "x", nth: 3 }]),
    "edit_conflict",
  );
  const del = plan(doc, [{ op: "replace", find: " line two", with: "" }]);
  assert.ok(del.text.includes("intro\n"));
});

test("block operations are guarded by the content hash", () => {
  const para = hashOf(doc, "Intro line one\nintro line two");
  const r = plan(doc, [
    { op: "replace_block", block: para, with: "\nNew intro.\n" },
  ]);
  assert.ok(r.text.startsWith("# Title\n\nNew intro.\n\n## Plan"));
  // A human edit to the block changes its hash: their edit wins.
  const edited = doc.replace("line two", "line 2");
  rejects(
    () => plan(edited, [{ op: "replace_block", block: para, with: "x" }]),
    "edit_conflict",
    /changed or removed/,
  );
  const notes = hashOf(doc, "## Notes");
  const d = plan(doc, [{ op: "delete_block", block: para }]);
  assert.ok(d.text.startsWith("# Title\n\n## Plan"));
  const last = plan(doc, [
    { op: "delete_block", block: hashOf(doc, "Last paragraph.") },
  ]);
  assert.ok(last.text.endsWith("## Notes\n"));
  rejects(
    () => plan(doc, [{ op: "replace_block", block: notes, with: "  \n" }]),
    "invalid",
  );
  rejects(
    () => plan(doc, [{ op: "delete_block", block: "nothex" }]),
    "invalid",
  );
  const dup = "same\n\nother\n\nsame\n";
  const same = hashOf(dup, "same");
  rejects(
    () => plan(dup, [{ op: "delete_block", block: same }]),
    "edit_conflict",
    /more than once/,
  );
  assert.equal(
    plan(dup, [{ op: "delete_block", block: same, nth: 2 }]).text,
    "same\n\nother\n",
  );
  assert.equal(
    plan("only", [{ op: "delete_block", block: hashOf("only", "only") }]).text,
    "",
  );
});

test("insert before, after, at the end of a section and of the document", () => {
  const plan2 = hashOf(doc, "## Plan");
  const s = plan(doc, [
    { op: "insert", section_end: plan2, text: "- step three" },
  ]);
  assert.ok(s.text.includes("const b = 2;\n```\n\n- step three\n\n## Notes"));
  const b = plan(doc, [
    { op: "insert", before: plan2, text: "Before plan.\n" },
  ]);
  assert.ok(b.text.includes("intro line two\n\nBefore plan.\n\n## Plan"));
  const a = plan(doc, [{ op: "insert", after: plan2, text: "After heading." }]);
  assert.ok(a.text.includes("## Plan\n\nAfter heading.\n\n- step one"));
  const end = plan(doc, [{ op: "insert", at: "end", text: "Fin." }]);
  assert.ok(end.text.endsWith("Last paragraph.\n\nFin.\n"));
  const start = plan(doc, [{ op: "insert", at: "start", text: "Top." }]);
  assert.ok(start.text.startsWith("Top.\n\n# Title"));
  assert.equal(
    plan("", [{ op: "insert", at: "end", text: "Hi" }]).text,
    "Hi\n",
  );
  assert.equal(
    plan("no newline", [{ op: "insert", at: "end", text: "Hi" }]).text,
    "no newline\n\nHi\n",
  );
  const last = hashOf(doc, "## Notes");
  const sec = plan(doc, [
    { op: "insert", section_end: last, text: "Appendix." },
  ]);
  assert.ok(sec.text.endsWith("Last paragraph.\n\nAppendix.\n"));
  rejects(
    () =>
      plan(doc, [
        {
          op: "insert",
          section_end: hashOf(doc, "Last paragraph."),
          text: "x",
        },
      ]),
    "invalid",
    /heading/,
  );
  rejects(
    () => plan(doc, [{ op: "insert", text: "x" }]),
    "invalid",
    /exactly one/,
  );
  rejects(
    () => plan(doc, [{ op: "insert", at: "end", before: plan2, text: "x" }]),
    "invalid",
  );
  rejects(
    () => plan(doc, [{ op: "insert", at: "middle", text: "x" }]),
    "invalid",
  );
  rejects(
    () => plan(doc, [{ op: "insert", at: "end", text: "\n\n" }]),
    "invalid",
  );
});

test("nth is positional and must be pinned with if_hash", () => {
  const text = "# A\n\nfoo\n\n# B\n\nfoo\n";
  const foo = hashOf(text, "foo"),
    op = { op: "replace_block", block: foo, with: "bar", nth: 2 };
  rejects(() => planEdit(text, { ops: [op] }, digest), "invalid", /if_hash/);
  const pinned = { ops: [op], if_hash: digest(text) };
  assert.equal(
    planEdit(text, pinned, digest).text,
    "# A\n\nfoo\n\n# B\n\nbar\n",
  );
  // A person prepends another identical block: the edit cannot move to # A.
  rejects(
    () => planEdit("# New\n\nfoo\n\n" + text, pinned, digest),
    "edit_conflict",
    /if_hash/,
  );
});

test("start and end inserts use the real text edges", () => {
  const at = (text, where) =>
    plan(text, [{ op: "insert", at: where, text: "Top" }]).text;
  assert.equal(at("\n\n# H\n", "start"), "Top\n\n\n# H\n");
  assert.equal(at("# H\n", "start"), "Top\n\n# H\n");
  assert.equal(at("", "start"), "Top\n");
  assert.equal(at("# H\n\n\n", "end"), "# H\n\n\nTop\n");
  assert.equal(at("# H\n", "end"), "# H\n\nTop\n");
  assert.equal(at("# H", "end"), "# H\n\nTop\n");
  assert.equal(at("\n\n", "end"), "\n\nTop\n");
  for (const t of ["\n\n# H\n", "# H\n\n\n", "# H"])
    for (const w of ["start", "end"]) assert.equal(blocks(at(t, w)).length, 2);
});

test("inserts stay separate blocks next to headings and fences", () => {
  const tight = "# H\nParagraph\n";
  const h = hashOf(tight, "# H"),
    p = hashOf(tight, "Paragraph");
  const after = plan(tight, [{ op: "insert", after: h, text: "Inserted." }]);
  assert.equal(after.text, "# H\n\nInserted.\n\nParagraph\n");
  const before = plan(tight, [{ op: "insert", before: p, text: "Inserted." }]);
  assert.equal(before.text, "# H\n\nInserted.\n\nParagraph\n");
  const section = plan(tight, [{ op: "insert", section_end: h, text: "End." }]);
  assert.equal(section.text, "# H\nParagraph\n\nEnd.\n");
  const fenced = "```\ncode\n```\nAfter\n";
  const f = hashOf(fenced, "```\ncode\n```");
  assert.equal(
    plan(fenced, [{ op: "insert", after: f, text: "X" }]).text,
    "```\ncode\n```\n\nX\n\nAfter\n",
  );
  for (const r of [after, before, section])
    assert.equal(blocks(r.text).length, 3);
});

test("nth reaches any occurrence and line numbers stay linear", () => {
  const many = "a".repeat(1500);
  const r = plan(many, [{ op: "replace", find: "a", with: "z", nth: 1200 }]);
  assert.equal(r.text[1199], "z");
  rejects(
    () => plan(many, [{ op: "replace", find: "a", with: "z", nth: 1501 }]),
    "edit_conflict",
    /occurs 1500 time/,
  );
  const paras = Array.from({ length: 20000 }, (_, i) => "p" + i).join("\n\n");
  const started = Date.now();
  const o = outline(paras, (s) => s, 19000);
  assert.equal(o.total, 20000);
  assert.equal(o.blocks.length, 1000);
  assert.equal(o.blocks[999].line, 39999);
  assert.equal(outline(paras, (s) => s, 20000).blocks.length, 0);
  assert.ok(Date.now() - started < 2000, "outline is not linear");
});

test("operations apply in order and the whole call is all-or-nothing", () => {
  const r = plan(doc, [
    { op: "replace", find: "# Title", with: "# Renamed" },
    {
      op: "insert",
      after: hashOf(doc.replace("# Title", "# Renamed"), "# Renamed"),
      text: "Subtitle.",
    },
  ]);
  assert.ok(r.text.startsWith("# Renamed\n\nSubtitle.\n\nIntro"));
  assert.equal(r.summary.length, 2);
  rejects(
    () =>
      plan(doc, [
        { op: "replace", find: "Intro", with: "Hello" },
        { op: "replace", find: "Intro", with: "again" },
      ]),
    "edit_conflict",
    /op 2/,
  );
});

test("if_hash guards the whole document", () => {
  const h = digest(doc);
  assert.ok(
    plan(doc, [{ op: "replace", find: "Title", with: "T" }], { if_hash: h })
      .text,
  );
  rejects(
    () =>
      plan(doc + "x", [{ op: "replace", find: "Title", with: "T" }], {
        if_hash: h,
      }),
    "edit_conflict",
    /if_hash/,
  );
  rejects(
    () =>
      plan(doc, [{ op: "replace", find: "Title", with: "T" }], {
        if_hash: "abc",
      }),
    "invalid",
  );
});

test("malformed requests are invalid", () => {
  rejects(() => planEdit(doc, null, digest), "invalid");
  rejects(() => planEdit(doc, { ops: [] }, digest), "invalid");
  rejects(
    () =>
      planEdit(
        doc,
        { ops: [{ op: "replace", find: "a", with: "b" }], extra: 1 },
        digest,
      ),
    "invalid",
    /unknown field/,
  );
  rejects(
    () =>
      plan(
        doc,
        Array(33).fill({ op: "replace", find: "Title", with: "Title" }),
      ),
    "invalid",
    /at most 32/,
  );
  rejects(() => plan(doc, [{ op: "rewrite" }]), "invalid");
  rejects(() => plan(doc, [{ op: "replace", find: "", with: "x" }]), "invalid");
  rejects(
    () => plan(doc, [{ op: "replace", find: "Title", with: 1 }]),
    "invalid",
  );
  rejects(
    () => plan(doc, [{ op: "replace", find: "Title", with: "x", nth: 0 }]),
    "invalid",
  );
  rejects(
    () => plan(doc, [{ op: "replace", find: "Title", with: "x", block: "a" }]),
    "invalid",
    /unknown field/,
  );
});

test("splices replay on Y.Text and keep concurrent edits elsewhere", () => {
  const a = new Y.Doc(),
    b = new Y.Doc();
  a.getText("markdown").insert(0, doc);
  Y.applyUpdate(b, Y.encodeStateAsUpdate(a));
  // Human edit in b, agent edit in a, both against the same base.
  b.getText("markdown").insert(doc.indexOf("Last"), "Very ");
  const r = plan(doc, [
    { op: "replace", find: "step one", with: "first step" },
    { op: "insert", at: "start", text: "Top." },
  ]);
  const t = a.getText("markdown");
  a.transact(() => {
    for (const s of r.splices) {
      if (s.remove) t.delete(s.at, s.remove);
      if (s.insert) t.insert(s.at, s.insert);
    }
  });
  assert.equal(t.toString(), r.text);
  Y.applyUpdate(a, Y.encodeStateAsUpdate(b));
  Y.applyUpdate(b, Y.encodeStateAsUpdate(a));
  const merged = t.toString();
  assert.equal(merged, b.getText("markdown").toString());
  assert.ok(merged.startsWith("Top.\n\n# Title"));
  assert.ok(merged.includes("- first step\n"));
  assert.ok(merged.includes("Very Last paragraph."));
});

test("emoji and Korean text keep surrogate pairs whole", () => {
  const text = "안녕 😀 world\n";
  const r = plan(text, [{ op: "replace", find: "😀", with: "😃" }]);
  assert.equal(r.text, "안녕 😃 world\n");
  for (const s of r.splices) {
    assert.ok(!/^[\uDC00-\uDFFF]/.test(s.insert));
    assert.ok(!/[\uD800-\uDBFF]$/.test(s.insert));
  }
});
