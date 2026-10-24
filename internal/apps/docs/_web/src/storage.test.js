import { test } from "node:test";
import assert from "node:assert/strict";
import * as Y from "yjs";
import {
  validateState,
  validateIncremental,
  validateUpdate,
  guestLines,
  insertedClients,
  hasGC,
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
test("guest lines mark the lines holding a guest's characters", () => {
  const doc = new Y.Doc();
  doc.clientID = 1;
  const t = doc.getText("markdown");
  t.insert(0, "one\ntwo\nthree\nfour\nfive\n");
  const guest = new Y.Doc();
  guest.clientID = 7;
  Y.applyUpdate(guest, Y.encodeStateAsUpdate(doc));
  const g = guest.getText("markdown");
  g.insert(4, "G1"); // line 2
  g.insert(g.toString().indexOf("four") + 2, "x\ny"); // lines 4 and 5
  Y.applyUpdate(doc, Y.encodeStateAsUpdate(guest, Y.encodeStateVector(doc)));
  assert.equal(t.toString(), "one\nG1two\nthree\nfox\nyur\nfive\n");
  assert.deepEqual(guestLines(doc, new Set([7])), [
    [2, 2],
    [4, 5],
  ]);
  assert.deepEqual(guestLines(doc, new Set()), []);
  // A member deleting the guest's characters unmarks them; text around
  // them does not.
  t.delete(4, 2);
  t.insert(0, "new\n");
  assert.deepEqual(guestLines(doc, new Set([7])), [[5, 6]]);
  // A guest newline alone marks the line it ends; adjacent ranges merge.
  const d2 = new Y.Doc();
  d2.clientID = 7;
  d2.getText("markdown").insert(0, "a\nb\n");
  assert.deepEqual(guestLines(d2, new Set([7])), [[1, 2]]);
});
test("guest lines stay bounded by merging, never unmarking", () => {
  const doc = new Y.Doc();
  doc.clientID = 1;
  const t = doc.getText("markdown");
  t.insert(0, "m\n".repeat(5000));
  const guest = new Y.Doc();
  guest.clientID = 7;
  Y.applyUpdate(guest, Y.encodeStateAsUpdate(doc));
  for (let line = 4999; line >= 0; line -= 2)
    guest.getText("markdown").insert(line * 2, "g");
  Y.applyUpdate(doc, Y.encodeStateAsUpdate(guest, Y.encodeStateVector(doc)));
  const ranges = guestLines(doc, new Set([7]));
  assert.ok(ranges.length <= 1024, String(ranges.length));
  const marked = (n) => ranges.some(([a, b]) => a <= n && n <= b);
  for (let line = 2; line <= 5000; line += 2) assert.ok(marked(line), line);
});
test("updates name the client ids they insert with and GC structs", () => {
  const d = new Y.Doc();
  d.clientID = 42;
  d.getText("markdown").insert(0, "abc");
  const decoded = validateUpdate(Y.encodeStateAsUpdate(d));
  assert.deepEqual([...insertedClients(decoded)], [42]);
  assert.equal(hasGC(decoded), false);
  // A content-less clock jump: one GC struct.
  const gc = Uint8Array.from([1, 1, 42, 3, 0, 5, 0]);
  const withGC = validateUpdate(gc);
  assert.equal(hasGC(withGC), true);
  assert.equal(insertedClients(withGC).size, 0);
});
