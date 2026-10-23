import { test } from "node:test";
import assert from "node:assert/strict";
import { Pending, Provider } from "./provider.js";
import * as Y from "yjs";
import { base64, unbase64 } from "./protocol.js";
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
    onAccess: () => true,
    onWelcome() {},
    unsent: [],
    raw: new Map(),
    inflight: new Set(),
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
      onError: () => false,
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
    unsent: [],
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

// A simulated page and server: a fake WebSocket under mocked timers. The
// server applies updates in order, refuses any that leave pending structs
// and, with a bucket, answers busy until an update arrives with retry.
function simulate(t, { bucket } = {}) {
  t.mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"] });
  const saved = globalThis.location;
  globalThis.location = { href: "https://flat.test/a.md", protocol: "https:" };
  t.after(() => (globalThis.location = saved));
  const server = new Y.Doc(),
    frames = [],
    applied = [],
    appliedAt = new Map(),
    committed = new Set(),
    errors = [];
  let seq = 0,
    tokens = bucket?.burst ?? 0,
    refilled = 0;
  class Socket {
    constructor() {
      this.readyState = 0;
      this.bufferedAmount = 0;
      this.busy = false;
      setTimeout(() => {
        this.readyState = 1;
        this.onopen();
      }, 1);
    }
    reply(m) {
      setTimeout(() => {
        if (this.readyState === 1) this.onmessage({ data: JSON.stringify(m) });
      }, 5);
    }
    close() {
      if (this.readyState === 3) return;
      this.readyState = 3;
      setTimeout(() => this.onclose({ code: 1000, reason: "" }), 0);
    }
    send(data) {
      const m = JSON.parse(data);
      if (m.t === "hello")
        this.reply({
          t: "welcome",
          v: 1,
          doc: "a.md",
          epoch: "e",
          seq,
          chain: String(seq),
          readonly: false,
          u: base64(Y.encodeStateAsUpdate(server)),
          access: "edit",
          access_epoch: 3,
          me: { kind: "guest", name: "Mina" },
        });
      else if (m.t === "ping")
        this.reply({ t: "pong", epoch: "e", seq, chain: String(seq) });
      else if (m.t === "update") this.update(m);
    }
    update(m) {
      const u = unbase64(m.u, 1024 * 1024);
      frames.push({
        t: Date.now(),
        size: u.length,
        id: m.id,
        retry: !!m.retry,
      });
      if (committed.has(m.id)) {
        this.reply({ t: "ack", id: m.id, seq, chain: String(seq) });
        return;
      }
      if (bucket) {
        const now = Date.now();
        tokens = Math.min(
          bucket.burst,
          tokens + ((now - refilled) / 1000) * bucket.rate,
        );
        refilled = now;
        if (m.retry) this.busy = false;
        if (this.busy || tokens < 1) {
          this.busy = true;
          this.reply({ t: "busy", id: m.id });
          return;
        }
        tokens--;
      }
      Y.applyUpdate(server, u);
      if (server.store.pendingStructs || server.store.pendingDs)
        errors.push("pending state after " + m.id);
      committed.add(m.id);
      applied.push(m.id);
      appliedAt.set(m.id, Date.now());
      seq++;
      this.reply({ t: "ack", id: m.id, seq, chain: String(seq) });
    }
  }
  const doc = new Y.Doc();
  const provider = new Provider(doc, "a.md", {
    readonly: false,
    WebSocketClass: Socket,
    onStop: (message) => errors.push(message),
    onAccess: (m) => {
      provider.paced = m.me?.kind === "guest";
      return true;
    },
  });
  t.after(() => provider.destroy());
  // Mocked ticks do not run timers set during the same tick, so time moves
  // a millisecond at a time.
  const advance = (ms) => {
    for (let i = 0; i < ms; i++) t.mock.timers.tick(1);
  };
  return { doc, provider, server, frames, applied, appliedAt, errors, advance };
}

// paced checks every one-second window of frames against the guest pace.
function assertPaced(frames) {
  for (const f of frames) {
    const recent = frames.filter((g) => g.t > f.t - 1000 && g.t <= f.t);
    assert.ok(recent.length <= 4, `${recent.length} frames before ${f.t}`);
    const bytes = recent.reduce((n, g) => n + g.size, 0);
    assert.ok(bytes <= 64 * 1024, `${bytes} bytes before ${f.t}`);
  }
}

test("guest pacing loses nothing at 50 keystrokes per second for 30 seconds", (t) => {
  const { doc, provider, server, frames, applied, errors, advance } =
    simulate(t);
  advance(20);
  assert.equal(provider.model.connected, true);
  const text = doc.getText("markdown");
  for (let i = 0; i < 1500; i++) {
    // Typing with some backspaces, as people do.
    if (i % 10 === 9) text.delete(text.length - 1, 1);
    else text.insert(text.length, String.fromCharCode(97 + (i % 26)));
    advance(20);
  }
  assert.equal(provider.model.label(), "Saving…");
  advance(10000);
  assert.deepEqual(errors, []);
  assert.ok(server.getText("markdown").toString() === text.toString());
  assert.equal(provider.model.items.size, 0);
  assert.equal(provider.model.label(), "Saved");
  assertPaced(frames);
  // Merged: far fewer frames than keystrokes, each id applied once.
  assert.ok(frames.length <= 4 * 31, `${frames.length} frames`);
  assert.equal(new Set(applied).size, applied.length);
});

test("busy resends every unacknowledged update in order, the first with retry", (t) => {
  // All guests of the flat together allow an update every 2 seconds here,
  // fewer than one guest's pace.
  const { doc, provider, server, frames, applied, appliedAt, errors, advance } =
    simulate(t, { bucket: { rate: 0.5, burst: 1 } });
  advance(20);
  const text = doc.getText("markdown");
  // Pastes too large to merge, so every one is its own update.
  const paste = (c) => text.insert(text.length, c.repeat(40 * 1024));
  for (const c of "abcdefgh") {
    paste(c);
    advance(100);
  }
  advance(60000);
  assert.deepEqual(errors, []);
  assert.ok(server.getText("markdown").toString() === text.toString());
  assert.equal(provider.model.items.size, 0);
  assert.equal(applied.length, 8);
  // The server applied the updates in the order they were made.
  const made = [...new Set(frames.map((f) => f.id))];
  assert.deepEqual(applied, made);
  assert.ok(frames.some((f) => f.retry));
  // After each busy the first frame resent is the oldest one not applied.
  for (const f of frames)
    if (f.retry)
      assert.equal(
        f.id,
        made.find((id) => !(appliedAt.get(id) < f.t)),
      );
  assertPaced(frames);
});

test("only an unsent update is merged, and only up to 64 KiB", () => {
  const sent = [];
  const provider = Object.create(Provider.prototype);
  Object.assign(provider, {
    model: new Pending(),
    paced: true,
    pacer: { delay: () => 1000, record() {} },
    unsent: [],
    raw: new Map(),
    inflight: new Set(),
    send: (m) => sent.push(m),
    onState() {},
  });
  provider.model.connected = true;
  const doc = new Y.Doc(),
    text = doc.getText("markdown");
  doc.on("update", (u, origin) => provider.queue(u, origin));
  t_insert(text, "a");
  t_insert(text, "b");
  assert.equal(provider.model.items.size, 1, "typing merges");
  text.insert(text.length, "x".repeat(70 * 1024));
  assert.equal(provider.model.items.size, 2, "a large paste is its own update");
  t_insert(text, "c");
  assert.equal(provider.model.items.size, 3, "nothing merges past 64 KiB");
  // Sent updates keep their exact bytes for retries.
  provider.pacer.delay = () => 0;
  provider.flush();
  // A guest keeps at most two updates unacknowledged.
  assert.equal(sent.length, 2);
  provider.inflight.delete(sent[0].id);
  provider.flush();
  assert.equal(sent.length, 3);
  t_insert(text, "d");
  assert.equal(provider.model.items.size, 4);
  const server = new Y.Doc();
  for (const u of provider.model.items.values())
    Y.applyUpdate(server, unbase64(u, 1024 * 1024));
  assert.equal(server.getText("markdown").toString(), text.toString());
});
function t_insert(text, s) {
  text.insert(text.length, s);
}

test("guest frames: level and viewers reach the page, a handled error does not stop it", () => {
  const seen = [];
  const provider = Object.create(Provider.prototype);
  Object.assign(provider, {
    model: new Pending(),
    send() {},
    onState() {},
    onLevel: (m) => seen.push(["level", m.access, m.access_epoch]),
    onViewers: (m) => seen.push(["viewers", m.n]),
    onError: (m) => {
      seen.push(["error", m.code]);
      return true;
    },
    onStop: () => seen.push(["stop"]),
  });
  provider.receive({ t: "level", access: "edit", access_epoch: 4, n: 1 });
  provider.receive({ t: "viewers", n: [1, 2], n2: 0 });
  provider.receive({ t: "error", code: "editing_ended" });
  assert.deepEqual(seen, [
    ["level", "edit", 4],
    ["viewers", [1, 2]],
    ["error", "editing_ended"],
  ]);
  assert.equal(provider.model.stopped, false);
});

test("a refused guest update halts sync and keeps the refused and later text", () => {
  const provider = Object.create(Provider.prototype),
    model = new Pending();
  model.add("A", "AAA=");
  let closed = 0;
  Object.assign(provider, {
    model,
    socket: {
      close() {
        closed++;
      },
    },
    onState() {},
    onError: () => {
      provider.halt();
      return true;
    },
    onStop: () => assert.fail("the page explains the refusal itself"),
  });
  provider.receive({ t: "error", code: "guest_size" });
  assert.equal(model.stopped, true);
  assert.equal(closed, 1);
  assert.equal(model.unsynced(), true);
  // A later local change B is not sent, and nothing reconnects.
  provider.queue(new Uint8Array([0]), null);
  assert.equal(model.items.size, 1);
  provider.connect();
  assert.equal(closed, 1);
});

test("a dropped welcome changes nothing; a full reader pool pauses reconnects", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"] });
  const saved = globalThis.location;
  globalThis.location = { href: "https://flat.test/a.md", protocol: "https:" };
  t.after(() => (globalThis.location = saved));
  const sockets = [];
  class Socket {
    constructor() {
      sockets.push(this);
      this.readyState = 0;
      setTimeout(() => {
        this.readyState = 1;
        this.onopen();
      }, 1);
    }
    send() {}
    close() {}
  }
  let pause = true;
  const doc = new Y.Doc();
  const provider = new Provider(doc, "a.md", {
    WebSocketClass: Socket,
    onAccess: () => false,
    onCapacity: () => pause,
  });
  t.after(() => provider.destroy());
  t.mock.timers.tick(5);
  const server = new Y.Doc();
  server.getText("markdown").insert(0, "text");
  provider.receive({
    t: "welcome",
    v: 1,
    doc: "a.md",
    epoch: "e",
    seq: 1,
    chain: "1",
    readonly: true,
    u: base64(Y.encodeStateAsUpdate(server)),
  });
  assert.equal(doc.getText("markdown").toString(), "");
  assert.equal(provider.model.connected, false);
  sockets[0].onclose({ code: 1008, reason: "connection capacity" });
  t.mock.timers.tick(60000);
  assert.equal(sockets.length, 1);
  assert.equal(provider.paused, true);
  provider.reconnect();
  assert.equal(provider.paused, false);
  assert.equal(sockets.length, 2);
  // Members reconnect after the same close as before.
  pause = false;
  sockets[1].onclose({ code: 1008, reason: "connection capacity" });
  t.mock.timers.tick(60000);
  assert.ok(sockets.length > 2);
});

test("receipts cover every frame up to their number: one per busy moment, at once after 256 KiB", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"] });
  const saved = globalThis.location;
  globalThis.location = { href: "https://flat.test/a.md", protocol: "https:" };
  t.after(() => (globalThis.location = saved));
  const sent = [];
  let socket;
  class Socket {
    constructor() {
      socket = this;
      this.readyState = 0;
      this.bufferedAmount = 0;
      setTimeout(() => {
        this.readyState = 1;
        this.onopen();
      }, 1);
    }
    close() {}
    send(data) {
      const m = JSON.parse(data);
      if (m.t === "received") sent.push(m.n);
    }
  }
  const provider = new Provider(new Y.Doc(), "a.md", {
    readonly: true,
    WebSocketClass: Socket,
  });
  t.after(() => provider.destroy());
  t.mock.timers.tick(2);
  const frame = (n, pad = "") =>
    socket.onmessage({
      data: JSON.stringify({
        t: "pong",
        epoch: "e",
        seq: 0,
        chain: "0",
        n,
        pad,
      }),
    });
  for (let n = 1; n <= 50; n++) frame(n);
  assert.deepEqual(sent, []);
  t.mock.timers.tick(200);
  assert.deepEqual(sent, [50]);
  frame(51, "x".repeat(300 * 1024));
  assert.deepEqual(sent, [50, 51]);
  t.mock.timers.tick(500);
  assert.deepEqual(sent, [50, 51]);
});
