import { test } from "node:test";
import { diagramBlocks } from "./live.js";
import assert from "node:assert/strict";
import { ChangeSet, EditorState } from "@codemirror/state";
import {
  diagramKeys,
  updatePreview,
  trackFocusExit,
  diagramClosures,
  setDiagramEditing,
  sourceAction,
} from "./diagram-preview.js";

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
    edit = {
      setAttribute(name, value) {
        this[name] = value;
      },
    },
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
  assert.equal(edit.textContent, "Edit source");
  assert.equal(edit["aria-expanded"], "false");
  assert.equal(div.dataset.active, "false");
  clearTimeout(div.diagramTimer); // Widget.destroy cancels a detached preview.
  t.mock.timers.tick(1000);
  assert.deepEqual(drawn, ["C"]);
});

test("toolbar focus exit settles outside the editor and cancels on destruction", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let listener,
    exits = 0;
  const inside = {},
    outside = {};
  const root = {
    ownerDocument: { activeElement: inside },
    contains: (element) => element === inside,
    addEventListener(type, fn) {
      assert.equal(type, "focusout");
      listener = fn;
    },
    removeEventListener(type, fn) {
      assert.equal(type, "focusout");
      assert.equal(fn, listener);
      listener = null;
    },
  };
  const cleanup = trackFocusExit(root, () => exits++);
  listener();
  t.mock.timers.tick(0);
  assert.equal(exits, 0, "moving between toolbar controls keeps editing");
  root.ownerDocument.activeElement = outside;
  listener();
  assert.equal(exits, 0, "no synchronous dispatch during focus event");
  t.mock.timers.tick(0);
  assert.equal(exits, 1);
  listener();
  cleanup();
  t.mock.timers.tick(1);
  assert.equal(exits, 1, "destroyed editors do not dispatch");
  assert.equal(listener, null);
});

test("Done closes and Edit source reopens nested and EOF fences without changing text", () => {
  for (const text of [
    "```mermaid\ngraph TD; A-->B\n```",
    "> - ```mermaid\n>   graph TD; A-->B\n>   ```",
  ]) {
    let state = EditorState.create({ doc: text });
    let blocks = diagramBlocks(state, true);
    let keys = diagramKeys(blocks);
    const key = keys.get(blocks[0].from);
    state = state.update({ selection: { anchor: blocks[0].edit } }).state;
    const close = state.update({
      effects: setDiagramEditing.of({ key, open: false }),
    });
    let closed = diagramClosures(
      diagramBlocks(close.state, true),
      keys,
      new Set(),
      close,
    );
    assert.ok(closed.has(key), "caret inside the fence must not reopen Done");
    assert.equal(
      close.state.doc.toString(),
      text,
      "Done preserves Markdown and undo history",
    );
    assert.equal(close.state.selection.main.head, state.selection.main.head);
    const focusOnly = close.state.update({});
    closed = diagramClosures(
      diagramBlocks(focusOnly.state, true),
      keys,
      closed,
      focusOnly,
    );
    assert.ok(closed.has(key), "toolbar focus alone keeps source closed");
    const reopen = focusOnly.state.update({
      effects: setDiagramEditing.of({ key, open: true }),
      selection: { anchor: blocks[0].edit },
    });
    closed = diagramClosures(
      diagramBlocks(reopen.state, true),
      keys,
      closed,
      reopen,
    );
    assert.equal(closed.has(key), false);
    assert.equal(reopen.state.doc.toString(), text);
    const button = {
      setAttribute(name, value) {
        this[name] = value;
      },
    };
    sourceAction(button, true);
    assert.equal(button.textContent, "Done");
    assert.equal(button["aria-expanded"], "true");
  }
});
