import { test } from "node:test";
import assert from "node:assert/strict";
import * as Y from "yjs";
import {
  validateState,
  validateIncremental,
  validateUpdate,
} from "./storage.js";
test("incremental bound covers real encoded state with concurrent clients, deletes and pending updates", () => {
  const authority = new Y.Doc();
  authority.getText("markdown").insert(0, "한국어 🙂\n" + "base\n".repeat(100));
  validateState(authority);
  const clients = Array.from({ length: 4 }, () => {
    const d = new Y.Doc();
    Y.applyUpdate(d, Y.encodeStateAsUpdate(authority));
    return d;
  });
  for (let i = 0; i < 200; i++) {
    const d = clients[i % 4],
      sv = Y.encodeStateVector(d),
      text = d.getText("markdown");
    if (i % 3 === 0 && text.length > 20) text.delete(5, 4);
    else text.insert(Math.min(10, text.length), "edit " + i + "🙂");
    const bytes = Y.encodeStateAsUpdate(d, sv);
    validateUpdate(bytes);
    Y.applyUpdate(authority, bytes);
    const bound = validateIncremental(authority, bytes.length);
    assert.ok(Y.encodeStateAsUpdate(authority).length <= bound);
    for (const peer of clients) Y.applyUpdate(peer, bytes);
    if (i % 64 === 63) validateState(authority);
  }
  const pending = new Y.Doc(),
    sv0 = Y.encodeStateVector(pending);
  pending.getText("markdown").insert(0, "first");
  const first = Y.encodeStateAsUpdate(pending, sv0),
    sv1 = Y.encodeStateVector(pending);
  pending.getText("markdown").insert(5, "second");
  const second = Y.encodeStateAsUpdate(pending, sv1);
  for (const bytes of [second, first]) {
    Y.applyUpdate(authority, bytes);
    const bound = validateIncremental(authority, bytes.length);
    assert.ok(Y.encodeStateAsUpdate(authority).length <= bound);
  }
});
test("updates may only contain plain Markdown, not other roots, embeds or formatting", () => {
  for (const setup of [
    (d) => d.getText("other").insert(0, "x"),
    (d) => d.getMap("markdown").set("x", 1),
    (d) => {
      d.getText("markdown").insert(0, "x");
      d.getText("markdown").format(0, 1, { bold: true });
    },
  ]) {
    const d = new Y.Doc();
    setup(d);
    assert.throws(() => validateUpdate(Y.encodeStateAsUpdate(d)));
  }
});
test("updates reject truncated or trailing binary data", () => {
  const doc = new Y.Doc();
  doc.getText("markdown").insert(0, "한국어 🙂");
  const update = Y.encodeStateAsUpdate(doc);
  validateUpdate(update);
  assert.throws(() => validateUpdate(update.slice(0, -1)));
  const trailing = new Uint8Array(update.length + 1);
  trailing.set(update);
  assert.throws(() => validateUpdate(trailing), /trailing/);
});
