import { test } from "node:test";
import assert from "node:assert/strict";
import { Pending, Provider } from "./provider.js";
import * as Y from "yjs";
import { base64 } from "./protocol.js";
test("only ACK clears save state, older ACK never rewinds known history", () => {
  const p = new Pending();
  p.connected = true;
  p.add("a", "AAA=");
  assert.equal(p.label(), "Saving…");
  p.remember({ epoch: "e", seq: 5, chain: "5" });
  assert.equal(p.items.size, 1);
  p.ack("unknown");
  assert.equal(p.label(), "Saving…");
  p.ack("a");
  p.remember({ seq: 3, chain: "3" });
  assert.equal(p.label(), "Saved");
  assert.equal(p.known.seq, 5);
  p.connected = false;
  assert.equal(p.label(), "Offline — changes will sync");
  p.readonly = true;
  assert.equal(p.label(), "Read-only");
});
test("welcome resends exact pending ids; updates do not acknowledge; diverged stops", () => {
  const d = new Y.Doc(),
    state = new Pending();
  state.add("same-id", "AAA=");
  const sent = [];
  const provider = Object.create(Provider.prototype);
  Object.assign(provider, {
    doc: d,
    path: "a.md",
    model: state,
    awareness: { setLocalStateField() {} },
    send: (m) => sent.push(m),
    sendAwareness() {},
    onWelcome() {},
    onState() {},
    onStop() {},
    socket: { close() {} },
  });
  const server = new Y.Doc();
  server.getText("markdown").insert(0, "server");
  provider.receive({
    t: "welcome",
    v: 1,
    doc: "a.md",
    epoch: "e",
    seq: 0,
    chain: "0",
    readonly: false,
    u: base64(Y.encodeStateAsUpdate(server)),
    you: { name: "Guest", verified: false },
    n: 1,
  });
  assert.deepEqual(sent[0], { t: "update", id: "same-id", u: "AAA=" });
  assert.equal(state.label(), "Saving…");
  provider.receive({ t: "diverged", epoch: "e", seq: 0 });
  assert.equal(state.stopped, true);
  assert.equal(state.items.size, 1);
});

test("permanent error codes stop retries and retain Markdown and pending bytes", () => {
  for (const code of ["capacity", "invalid_update", "readonly", "disk_quota"]) {
    const provider = Object.create(Provider.prototype),
      model = new Pending();
    model.add("pending", "AAA=");
    const d = new Y.Doc();
    d.getText("markdown").insert(0, "local text");
    let notice = "",
      closed = 0;
    Object.assign(provider, {
      model,
      doc: d,
      onState() {},
      onStop(m) {
        notice = m;
      },
      socket: {
        close() {
          closed++;
        },
      },
    });
    provider.receive({ t: "error", code });
    assert.equal(model.stopped, true);
    assert.equal(model.items.size, 1);
    assert.equal(d.getText("markdown").toString(), "local text");
    assert.match(notice, /Download/);
    if (code === "disk_quota") assert.match(notice, /disk quota/);
    provider.connect();
    assert.equal(closed, 1);
  }
});

test("local text that never reached the server stays unsynced after a stop", () => {
  const model = new Pending();
  assert.equal(model.unsynced(), false);
  model.add("pending", "AAA=");
  model.stopped = true;
  assert.equal(model.unsynced(), true);
  model.ack("pending");
  assert.equal(model.unsynced(), false);
  // An edit too large to queue stops sync and is still unsynced.
  const provider = Object.create(Provider.prototype);
  let notice = "";
  Object.assign(provider, {
    doc: new Y.Doc(),
    model: new Pending(),
    onState() {},
    onStop(m) {
      notice = m;
    },
    socket: { close() {} },
  });
  provider.queue(new Uint8Array(300 * 1024), null);
  assert.equal(provider.model.stopped, true);
  assert.equal(provider.model.items.size, 0);
  assert.equal(provider.model.unsynced(), true);
  assert.match(notice, /too large/);
});
