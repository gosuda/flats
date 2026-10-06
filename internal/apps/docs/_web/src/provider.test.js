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
  for (const code of ["capacity", "invalid_update", "readonly"]) {
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
    provider.connect();
    assert.equal(closed, 1);
  }
});
test("private conflict metadata reaches the notice without stopping sync", () => {
  const p = Object.create(Provider.prototype),
    model = new Pending(),
    notices = [];
  Object.assign(p, {
    model,
    onConflicts: (m) => notices.push(m),
    send() {},
    state() {},
  });
  const metadata = [{ generation: 2, bytes: 100 }];
  p.receive({ t: "conflicts", conflicts: metadata, n: 1 });
  assert.deepEqual(notices, [metadata]);
  assert.equal(model.stopped, false);
  model.readonly = true;
  p.receive({ t: "conflicts", conflicts: metadata, n: 2 });
  assert.equal(notices.length, 1);
});
