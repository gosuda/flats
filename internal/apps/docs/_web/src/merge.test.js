import { test } from "node:test";
import assert from "node:assert/strict";
import * as Y from "yjs";
import { mergeText, applyText } from "./merge.js";
test("merge independent agent/person lines, conflicts prefer version", () => {
  assert.equal(
    mergeText("a\nb\nc\n", "person\nb\nc\n", "a\nb\nagent\n"),
    "person\nb\nagent\n",
  );
  assert.equal(mergeText("a\nb\n", "a\nperson\n", "a\nagent\n"), "a\nagent\n");
  assert.equal(mergeText("a", "a", "b"), "b");
  assert.equal(mergeText("", "person", "agent"), "agent");
  assert.equal(mergeText("a\nb", "a\nb!", "A\nb"), "A\nb!");
});
test("minimal character diff preserves unchanged Yjs positions and Unicode", () => {
  const d = new Y.Doc(),
    t = d.getText("markdown");
  t.insert(0, "한국어 🌍\nunchanged\nold");
  const rel = Y.createRelativePositionFromTypeIndex(t, 12);
  applyText(t, "한국어 🌍\nunchanged\nnew");
  assert.equal(t.toString(), "한국어 🌍\nunchanged\nnew");
  assert.equal(Y.createAbsolutePositionFromRelativePosition(rel, d).index, 12);
  for (const next of ["", "🙂한국어", "x".repeat(5000), "🌍"]) {
    applyText(t, next);
    assert.equal(t.toString(), next);
  }
});
test("three-way line merge preserves separated insertions, deletions and adjacent changes", () => {
  for (let i = 0; i < 40; i++) {
    const base = ["one\n", "two\n", "three\n", "four\n"];
    const ours = [...base],
      theirs = [...base];
    ours[0] = "person " + i + "\n";
    theirs[1] = "agent " + i + "\n";
    assert.equal(
      mergeText(base.join(""), ours.join(""), theirs.join("")),
      ours[0] + theirs[1] + base[2] + base[3],
    );
  }
  assert.equal(mergeText("a\nb\nc\n", "a\nc\n", "a\nb\nC\n"), "a\nC\n");
  assert.equal(
    mergeText("a\nb\n", "person\na\nb\n", "a\nagent\nb\n"),
    "person\na\nagent\nb\n",
  );
});

test("bounded rewrites deterministically preserve recoverable live text", () => {
  for (const n of [100, 400, 2000, 20000]) {
    const base = "old\n".repeat(n),
      ours = base + "human\n",
      theirs = "new\n".repeat(n);
    const conflicts = [];
    assert.equal(
      mergeText(base, ours, theirs, (s) => conflicts.push(s)),
      theirs,
    );
    assert.deepEqual(conflicts, [ours]);
  }
});

test("disjoint edits that exceed the text cap fall back with recovery", () => {
  const ours = "a\n" + "x".repeat(600000) + "\n",
    theirs = "y".repeat(600000) + "\nb\n",
    conflicts = [];
  assert.equal(
    mergeText("a\nb\n", ours, theirs, (s) => conflicts.push(s)),
    theirs,
  );
  assert.deepEqual(conflicts, [ours]);
});
test("large overlapping long-line rewrites prefer version", () => {
  const base = "a".repeat(1024 * 1024 - 20),
    ours = base + "human",
    theirs = "b".repeat(1024 * 1024),
    saved = [];
  assert.equal(
    mergeText(base, ours, theirs, (s) => saved.push(s)),
    theirs,
  );
  assert.deepEqual(saved, [ours]);
});

test("large disjoint changes preserve human and agent lines", () => {
  for (const size of [90, 150, 500, 1024]) {
    const padding = "x".repeat(size * 1024 - 40);
    const base = "human old\n" + padding + "\nagent old\n";
    assert.equal(
      mergeText(
        base,
        base.replace("human old", "human new"),
        base.replace("agent old", "agent new"),
      ),
      base.replace("human old", "human new").replace("agent old", "agent new"),
    );
  }
});

test("line splitting preserves blank lines, CRLF and absent final newlines", () => {
  for (const [separator, suffix] of [
    ["\n", ""],
    ["\n", "\n"],
    ["\r\n", "\r\n"],
  ]) {
    const base =
      separator + "human old" + separator + separator + "agent old" + suffix;
    assert.equal(
      mergeText(
        base,
        base.replace("human old", "human new"),
        base.replace("agent old", "agent new"),
      ),
      base.replace("human old", "human new").replace("agent old", "agent new"),
    );
  }
});

test("overlapped paragraph typo fix preserves the complete live text once", () => {
  const base = "This paragraf describes the feature.\n\nOther line.\n";
  const ours = base
    .replace("paragraf", "paragraph")
    .replace("Other line", "Human line");
  const theirs = base.replace("the feature", "the updated feature");
  const saved = [];
  assert.equal(
    mergeText(base, ours, theirs, (s) => saved.push(s)),
    theirs.replace("Other line", "Human line"),
  );
  assert.deepEqual(saved, [ours]);
});

test("version already includes human correction without a conflict record", () => {
  const base = "This paragraf describes the feature.\nOther line.\n";
  const human = base.replace("paragraf", "paragraph");
  for (const version of [
    human,
    human.replace("\nOther", "\nAdded line.\nOther"),
  ]) {
    const saved = [];
    assert.equal(
      mergeText(base, human, version, (s) => saved.push(s)),
      version,
    );
    assert.deepEqual(saved, []);
  }
});
