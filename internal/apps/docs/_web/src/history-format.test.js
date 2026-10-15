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
