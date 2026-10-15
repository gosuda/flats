import { test } from "node:test";
import assert from "node:assert/strict";
import { ago, who, changed, changeLines } from "./history-format.js";

test("revisions say who made them", () => {
  assert.equal(who({ kind: "created" }), "Created");
  assert.equal(who({ kind: "agent", authors: [] }), "Agent");
  assert.equal(who({ kind: "agent", authors: ["Agent"] }), "Agent");
  assert.equal(
    who({ kind: "person", authors: ["Quiet Otter", "Lucky Fox"] }),
    "Quiet Otter, Lucky Fox",
  );
  assert.equal(who({ kind: "person", authors: [] }), "Edits");
  assert.equal(
    who({ kind: "rollback", rollback_of: 12, authors: ["Ada"] }),
    "Restored revision 12 · Ada",
  );
  assert.equal(who({ kind: "recovered", authors: [] }), "Recovered text");
});

test("changed documents are named by title, the open one as this document", () => {
  const titles = new Map([
    ["a.md", "Alpha"],
    ["b.md", "Beta"],
  ]);
  assert.equal(
    changed(
      {
        kind: "person",
        changes: { added: [], changed: ["a.md", "img.png"], removed: [] },
      },
      titles,
      "b.md",
    ),
    "Alpha",
  );
  assert.equal(
    changed(
      {
        kind: "person",
        changes: { added: ["b.md"], changed: [], removed: [] },
      },
      titles,
      "b.md",
    ),
    "this document",
  );
  assert.equal(
    changed(
      {
        kind: "agent",
        changes: { added: [], changed: ["x.png"], removed: [] },
      },
      titles,
      "a.md",
    ),
    "No text changes",
  );
  assert.equal(changed({ kind: "created", changes: {} }, titles, "a.md"), "");
  assert.equal(
    changed(
      {
        kind: "agent",
        changes: {
          added: ["c.md", "d.md", "e.md", "f.md"],
          changed: [],
          removed: [],
        },
      },
      titles,
      "a.md",
    ),
    "c, d, e and 1 more",
  );
});

test("line changes mark removed and added lines", () => {
  const lines = changeLines("a\nb\nc\n", "a\nB\nc\nd\n");
  assert.deepEqual(
    lines.map((l) => [l.kind, l.line]),
    [
      ["same", "a"],
      ["removed", "b"],
      ["added", "B"],
      ["same", "c"],
      ["added", "d"],
    ],
  );
  assert.ok(changeLines("same\n", "same\n").every((l) => l.kind === "same"));
});

test("times are relative", () => {
  const now = Date.UTC(2026, 0, 2, 12);
  assert.match(ago(new Date(now - 10 * 1000), now), /now/);
  assert.match(ago(new Date(now - 5 * 60 * 1000), now), /5 minutes ago/);
  assert.match(ago(new Date(now - 3 * 3600 * 1000), now), /3 hours ago/);
});

test("line changes map to the blocks that show them", async () => {
  const { createMarkdown } = await import("./markdown.js");
  const { lineChanges, markChanges } = await import("./history-format.js");
  const md = createMarkdown();
  const before =
    "# Title\n\nKeep this.\n\nOld line.\n\n- one\n- two\n\nGone.\n\nLast.\n";
  const after =
    "# Title\n\nKeep this.\n\nNew line.\n\n- one\n- two changed\n\nLast.\n";
  const changes = lineChanges(before, after);
  const tokens = md.parse(after, {});
  const out = markChanges(tokens, changes);
  const marked = tokens.filter((t) => t.meta?.revChanged);
  assert.equal(out.marked, 2);
  // The changed paragraph, and the tight list's item (its paragraph has no tag).
  assert.deepEqual(
    marked.map((t) => t.type),
    ["paragraph_open", "list_item_open"],
  );
  assert.equal(tokens[tokens.indexOf(marked[1]) + 2].content, "two changed");
  // A replaced line is a change, not a removal.
  assert.equal(tokens.filter((t) => t.meta?.revRemoved).length, 1);
  // "Gone." was removed before "Last.".
  const last = tokens.find(
    (t) =>
      t.type === "paragraph_open" &&
      tokens[tokens.indexOf(t) + 1].content === "Last.",
  );
  assert.equal(last.meta.revRemoved, 2);
  assert.equal(out.tail, 0);
  // Removed at the end.
  const end = markChanges(md.parse("a\n", {}), lineChanges("a\n\nb\n", "a\n"));
  assert.equal(end.tail, 2);
  // Nothing changed.
  assert.equal(
    markChanges(md.parse(after, {}), lineChanges(after, after)).marked,
    0,
  );
});

test("change mapping covers blank replacements, tables, definitions and limits", async () => {
  const { createMarkdown } = await import("./markdown.js");
  const { lineChanges, markChanges, changeLines } = await import(
    "./history-format.js"
  );
  const md = createMarkdown();
  const run = (a, b) => {
    const tokens = md.parse(b, {});
    return { tokens, out: markChanges(tokens, lineChanges(a, b)) };
  };
  // Text replaced by a blank line is a removal, not nothing.
  let { tokens, out } = run("one\n\ngone\n\nthree\n", "one\n\n\n\nthree\n");
  assert.ok(out.tail || tokens.some((t) => t.meta?.revRemoved));
  // A table's delimiter row marks the table.
  ({ tokens, out } = run("|a|b|\n|-|-|\n|1|2|\n", "|a|b|\n|:-|-:|\n|1|2|\n"));
  assert.ok(tokens.find((t) => t.type === "table_open").meta?.revChanged);
  // A changed link definition is counted as not shown.
  ({ out } = run("[x]\n\n[x]: /old\n", "[x]\n\n[x]: /new\n"));
  assert.equal(out.unshown, 1);
  // Adding a paragraph leaves no unshown blank line.
  ({ out } = run("a\n", "a\n\nb\n"));
  assert.equal(out.unshown, 0);
  assert.equal(out.marked, 1);
  // Texts too different to diff quickly are reported, not compared.
  const big = (w) =>
    Array.from({ length: 8192 }, (_, i) => w + i + " " + w.repeat(100)).join(
      "\n",
    );
  const start = Date.now();
  assert.equal(lineChanges(big("x"), big("y")), null);
  assert.equal(changeLines(big("x"), big("y")), null);
  assert.ok(Date.now() - start < 3000);
});

test("a split paragraph and links to a changed definition are marked", async () => {
  const { createMarkdown } = await import("./markdown.js");
  const { lineChanges, markChanges } = await import("./history-format.js");
  const md = createMarkdown();
  // One paragraph becomes two: both are marked.
  let tokens = md.parse("one\n\ntwo\n", {});
  let out = markChanges(tokens, lineChanges("one\ntwo\n", "one\n\ntwo\n"));
  assert.equal(out.marked, 2);
  // Two splits next to each other mark all three paragraphs.
  tokens = md.parse("a\n\nb\n\nc\n", {});
  out = markChanges(tokens, lineChanges("a\nb\nc\n", "a\n\nb\n\nc\n"));
  assert.equal(out.marked, 3);
  // A tight list becomes loose: the items around the blank line are marked.
  tokens = md.parse("- a\n\n- b\n", {});
  out = markChanges(tokens, lineChanges("- a\n- b\n", "- a\n\n- b\n"));
  assert.ok(out.marked >= 1);
  // Several blank lines splitting a paragraph also mark both parts.
  tokens = md.parse("one\n\n\ntwo\n", {});
  out = markChanges(tokens, lineChanges("one\ntwo\n", "one\n\n\ntwo\n"));
  assert.equal(out.marked, 2);
  // The blocks holding consumers of a changed definition are marked.
  const after = "See [the docs][x].\n\nOther.\n\n[x]: /new\n";
  tokens = md.parse(after, {});
  out = markChanges(
    tokens,
    lineChanges(after.replace("/new", "/old"), after),
    new Set([1]),
  );
  const marked = tokens.filter((t) => t.meta?.revChanged);
  assert.equal(marked.length, 1);
  assert.equal(
    tokens[tokens.indexOf(marked[0]) + 1].content,
    "See [the docs][x].",
  );
  // A blank line added inside code changes the code block.
  const fenced = "```js\na();\n\n\nb();\n```\n";
  tokens = md.parse(fenced, {});
  out = markChanges(tokens, lineChanges("```js\na();\n\nb();\n```\n", fenced));
  assert.ok(tokens.find((t) => t.type === "fence").meta?.revChanged);
  // A blank line after a heading changes nothing.
  out = markChanges(
    md.parse("# T\n\ntext\n", {}),
    lineChanges("# T\ntext\n", "# T\n\ntext\n"),
  );
  assert.equal(out.marked, 0);
});
