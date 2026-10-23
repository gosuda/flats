import { test } from "node:test";
import assert from "node:assert/strict";
import * as Y from "yjs";
import { RemoteBatcher, MAX_WAIT } from "./remote-batch.js";
import { Provider } from "./provider.js";
import { base64, unbase64 } from "./protocol.js";

// A fake clock: applying costs `cost` ms and rendering `render` ms, timers
// run when the clock passes them.
function clock() {
  const c = {
    t: 0,
    timers: [],
    setTimer(f, ms) {
      const timer = { at: c.t + ms, f };
      c.timers.push(timer);
      return timer;
    },
    clearTimer(timer) {
      c.timers = c.timers.filter((x) => x !== timer);
    },
    // advance moves the clock to t + ms, running due timers in order.
    advance(ms) {
      const end = c.t + ms;
      for (;;) {
        c.timers.sort((a, b) => a.at - b.at);
        const next = c.timers[0];
        if (!next || next.at > end) break;
        c.timers.shift();
        c.t = Math.max(c.t, next.at);
        next.f();
      }
      c.t = Math.max(c.t, end);
    },
  };
  return c;
}

function batcher(c, { cost = 0, render = 0 } = {}) {
  const batches = [];
  const b = new RemoteBatcher(
    (updates) => {
      batches.push({ at: c.t, updates });
      c.t += typeof cost === "function" ? cost(updates) : cost;
    },
    {
      now: () => c.t,
      setTimer: c.setTimer,
      clearTimer: c.clearTimer,
      afterRender: (done) =>
        c.setTimer(() => {
          c.t += render;
          done(render);
        }, 0),
    },
  );
  return { b, batches };
}

test("an idle page applies a remote update at once", () => {
  const c = clock(),
    { b, batches } = batcher(c);
  b.push("a");
  assert.deepEqual(batches, [{ at: 0, updates: ["a"] }]);
  c.advance(0);
  assert.equal(b.state, "idle");
  b.push("b");
  assert.equal(batches.length, 2);
});

test("batches wait as long as the last one took to apply and render", () => {
  const c = clock(),
    { b, batches } = batcher(c, { cost: 30, render: 20 });
  b.push("u0");
  // Updates keep arriving every 5 ms for two seconds.
  for (let i = 1; i < 400; i++) {
    c.advance(5);
    b.push("u" + i);
  }
  c.advance(2000);
  // Nothing is lost, order is kept, and every batch after the first holds
  // what arrived while the page applied, rendered and paused.
  assert.deepEqual(
    batches.flatMap((x) => x.updates),
    Array.from({ length: 400 }, (_, i) => "u" + i),
  );
  assert.ok(batches.length < 400 / 5, `${batches.length} batches`);
  // Between the end of one batch's rendering and the start of the next,
  // the page waits at least that batch's cost (30 + 20 ms).
  for (let i = 1; i < batches.length; i++)
    assert.ok(
      batches[i].at - batches[i - 1].at >= 30 + 20 + 50,
      `batch ${i} at ${batches[i].at} after ${batches[i - 1].at}`,
    );
  assert.equal(b.lastCost, 50);
  assert.equal(b.state, "idle");
});

test("the wait follows the measured cost and stops at one second", () => {
  const c = clock();
  // A batch costs 400 ms per update, rendering nothing.
  const { b, batches } = batcher(c, { cost: (u) => u.length * 400 });
  b.push("first");
  for (let i = 0; i < 10; i++) b.push("x" + i);
  // first cost 400 ms; the ten others wait 400 ms, then cost 4000 ms.
  c.advance(400);
  assert.equal(batches.length, 2);
  assert.equal(batches[1].at, 800);
  assert.equal(batches[1].updates.length, 10);
  // The second batch took 4000 ms: the clock is at 4800.
  assert.equal(c.t, 4800);
  b.push("late");
  c.advance(MAX_WAIT - 1);
  assert.equal(batches.length, 2, "waits at most a second, not 4 s");
  c.advance(1);
  assert.equal(batches.length, 3);
  assert.equal(batches[2].at, 800 + 4000 + MAX_WAIT);
});

test("a batch too cheap to measure lets the next apply at once", () => {
  const c = clock(),
    { b, batches } = batcher(c, { cost: 0.2 });
  b.push("a");
  c.advance(0);
  b.push("b");
  assert.equal(batches.length, 2);
});

test("flush applies the queue now; destroy drops it", () => {
  const c = clock(),
    { b, batches } = batcher(c, { cost: 100 });
  b.push("a");
  b.push("b");
  b.push("c");
  assert.equal(batches.length, 1);
  b.flush();
  assert.deepEqual(batches[1].updates, ["b", "c"]);
  // The stale render and timer of the first batch do nothing; the flushed
  // batch's pause (100 ms) ends with nothing queued.
  c.advance(150);
  assert.equal(batches.length, 2);
  assert.equal(b.state, "idle");
  b.push("d");
  assert.equal(batches.length, 3);
  b.push("e");
  b.destroy();
  c.advance(5000);
  assert.equal(batches.length, 3);
  b.push("f");
  assert.equal(batches.length, 3);
});

// A provider under mocked timers whose server sends remote updates. Applying
// a remote batch costs 50 ms on the page's clock.
function page(t) {
  t.mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"] });
  const saved = globalThis.location;
  globalThis.location = { href: "https://flat.test/a.md", protocol: "https:" };
  t.after(() => (globalThis.location = saved));
  const server = new Y.Doc(),
    sockets = [],
    hellos = [],
    received = [];
  let seq = 0,
    clockMs = 0;
  class Socket {
    constructor() {
      this.readyState = 0;
      this.bufferedAmount = 0;
      sockets.push(this);
      setTimeout(() => {
        this.readyState = 1;
        this.onopen();
      }, 1);
    }
    reply(m) {
      setTimeout(() => {
        if (this.readyState === 1) this.onmessage({ data: JSON.stringify(m) });
      }, 2);
    }
    close() {
      if (this.readyState === 3) return;
      this.readyState = 3;
      setTimeout(() => this.onclose({ code: 1000, reason: "" }), 0);
    }
    send(data) {
      const m = JSON.parse(data);
      received.push(m);
      if (m.t === "hello") {
        hellos.push(m);
        this.reply({
          t: "welcome",
          v: 1,
          doc: "a.md",
          epoch: "e",
          seq,
          chain: String(seq),
          readonly: false,
          u: base64(Y.encodeStateAsUpdate(server, unbase64(m.sv, 1 << 20))),
        });
      } else if (m.t === "update") {
        Y.applyUpdate(server, unbase64(m.u, 1 << 20));
        seq++;
        this.reply({ t: "ack", id: m.id, seq, chain: String(seq) });
      }
    }
  }
  const doc = new Y.Doc(),
    applies = [],
    errors = [];
  doc.on("update", (_, origin) => {
    if (origin === provider) {
      applies.push(Date.now());
      clockMs += 50;
    }
  });
  const provider = new Provider(doc, "a.md", {
    WebSocketClass: Socket,
    onStop: (message) => errors.push(message),
    batch: {
      now: () => Date.now() + clockMs,
      afterRender: (done) => setTimeout(() => done(0), 0),
    },
  });
  t.after(() => provider.destroy());
  const advance = (ms) => {
    for (let i = 0; i < ms; i++) t.mock.timers.tick(1);
  };
  // remote: another client's edit, committed by the server and sent on.
  const other = new Y.Doc();
  other.on("update", (u) => {
    Y.applyUpdate(server, u);
    seq++;
    sockets
      .at(-1)
      .reply({ t: "update", u: base64(u), seq, chain: String(seq) });
  });
  const remote = (s) =>
    other.getText("markdown").insert(other.getText("markdown").length, s);
  return {
    doc,
    provider,
    server,
    sockets,
    hellos,
    received,
    applies,
    errors,
    advance,
    remote,
    other,
  };
}

test("a page applies a stream of remote updates in batches and loses none", (t) => {
  const { doc, provider, server, applies, errors, advance, remote, other } =
    page(t);
  advance(20);
  Y.applyUpdate(other, Y.encodeStateAsUpdate(server));
  for (let i = 0; i < 200; i++) {
    remote(`[r${i}]`);
    advance(5);
  }
  // While remote updates keep arriving, applies are spaced by at least the
  // 50 ms each costs.
  for (let i = 1; i < applies.length; i++)
    assert.ok(applies[i] - applies[i - 1] >= 50, `apply ${i}`);
  assert.ok(applies.length < 40, `${applies.length} applies`);
  advance(2000);
  assert.deepEqual(errors, []);
  assert.equal(
    doc.getText("markdown").toString(),
    server.getText("markdown").toString(),
  );
  assert.equal(provider.model.known.seq, 200);
});

test("local typing is sent and saved at once while a remote batch waits", (t) => {
  const { doc, provider, server, received, advance, remote, other } = page(t);
  advance(20);
  Y.applyUpdate(other, Y.encodeStateAsUpdate(server));
  remote("a");
  advance(3);
  remote("b");
  advance(3);
  // "b" waits behind the 50 ms pause after "a".
  assert.equal(provider.remote.pending, 1);
  doc.getText("markdown").insert(0, "L");
  assert.equal(received.at(-1).t, "update");
  assert.equal(provider.model.label(), "Saving…");
  advance(5);
  assert.equal(provider.remote.pending, 1, "the remote batch still waits");
  assert.equal(provider.model.label(), "Saved");
  advance(100);
  assert.equal(provider.remote.pending, 0);
  assert.equal(
    doc.getText("markdown").toString(),
    server.getText("markdown").toString(),
  );
});

test("a reconnect while a batch waits applies it before the hello", (t) => {
  const { doc, provider, server, sockets, hellos, advance, remote, other } =
    page(t);
  advance(20);
  Y.applyUpdate(other, Y.encodeStateAsUpdate(server));
  remote("a");
  advance(3);
  remote("b");
  remote("c");
  advance(3);
  assert.equal(provider.remote.pending, 2);
  // A local edit that is not yet acknowledged when the connection drops.
  doc.getText("markdown").insert(0, "L");
  sockets.at(-1).close();
  advance(1);
  assert.equal(provider.model.connected, false);
  assert.equal(provider.model.label(), "Offline — changes will sync");
  advance(2000);
  assert.equal(hellos.length, 2);
  // The new hello's state vector already includes b and c.
  const sv = Y.decodeStateVector(unbase64(hellos[1].sv, 1 << 20));
  assert.equal(sv.get(other.clientID), 3);
  assert.equal(provider.model.known.seq >= 3, true);
  advance(100);
  assert.equal(provider.model.label(), "Saved");
  assert.equal(provider.remote.pending, 0);
  assert.equal(
    doc.getText("markdown").toString(),
    server.getText("markdown").toString(),
  );
  assert.equal(server.getText("markdown").toString().includes("L"), true);
});

test("a stop applies the remote updates already received", (t) => {
  const { doc, provider, advance, remote, other, server } = page(t);
  advance(20);
  Y.applyUpdate(other, Y.encodeStateAsUpdate(server));
  remote("a");
  advance(3);
  remote("b");
  advance(3);
  assert.equal(provider.remote.pending, 1);
  provider.receive({ t: "diverged", epoch: "e", seq: 0 });
  assert.equal(provider.model.stopped, true);
  assert.equal(doc.getText("markdown").toString(), "ab");
});
