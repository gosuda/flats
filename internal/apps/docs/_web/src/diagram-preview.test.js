import { test } from "node:test";
import assert from "node:assert/strict";
import { ChangeSet } from "@codemirror/state";
import { diagramKeys, updatePreview } from "./diagram-preview.js";

test("diagram identity survives shifts but never follows a deleted block", () => {
  const initial = diagramKeys([{ from: 10 }, { from: 30 }]);
  const shift = ChangeSet.of({ from: 0, insert: "hello" }, 50);
  const moved = diagramKeys([{ from: 15 }, { from: 35 }], initial, shift);
  assert.equal(moved.get(15), initial.get(10));
  assert.equal(moved.get(35), initial.get(30));
  const replace = ChangeSet.of({ from: 10, to: 20, insert: "new diagram" }, 50);
  const changed = diagramKeys([{ from: 10 }, { from: 31 }], initial, replace);
  assert.notEqual(changed.get(10), initial.get(10));
  assert.equal(changed.get(31), initial.get(30));
});

test("preview rejects foreign DOM; same-block edits cancel immediately and debounce only drawing", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const key = {},
    edit = {},
    diagram = { source: "A", current: true };
  const div = {
    dataset: { code: "A" },
    querySelector: (s) => (s === ".mermaid-edit" ? edit : diagram),
  };
  const drawn = [];
  const invalidate = (el, code) => {
    el.source = code;
    el.current = false;
  };
  const draw = (el, code) => drawn.push(code);
  const previous = { key, active: true, code: "A" };
  assert.equal(
    updatePreview(
      div,
      { key: {}, active: true, code: "WRONG" },
      previous,
      invalidate,
      draw,
    ),
    false,
  );
  assert.equal(diagram.source, "A");
  assert.equal(
    updatePreview(
      div,
      { key, active: true, code: "B" },
      previous,
      invalidate,
      draw,
    ),
    true,
  );
  assert.equal(
    diagram.source,
    "B",
    "copy sees current source before rendering",
  );
  assert.equal(
    diagram.current,
    false,
    "old async completion is invalidated immediately",
  );
  t.mock.timers.tick(200);
  updatePreview(
    div,
    { key, active: true, code: "C" },
    previous,
    invalidate,
    draw,
  );
  t.mock.timers.tick(299);
  assert.deepEqual(drawn, []);
  t.mock.timers.tick(1);
  assert.deepEqual(drawn, ["C"]);
  updatePreview(
    div,
    { key, active: false, code: "D" },
    previous,
    invalidate,
    draw,
  );
  assert.equal(edit.hidden, false);
  clearTimeout(div.diagramTimer); // Widget.destroy cancels a detached preview.
  t.mock.timers.tick(1000);
  assert.deepEqual(drawn, ["C"]);
});
