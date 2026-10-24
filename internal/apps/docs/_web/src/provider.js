import * as Y from "yjs";
import { randomUUID } from "./uuid.js";
import {
  Awareness,
  encodeAwarenessUpdate,
  applyAwarenessUpdate,
  removeAwarenessStates,
} from "y-protocols/awareness";
import { base64, unbase64, LIMITS, cleanName } from "./protocol.js";
import { RemoteBatcher } from "./remote-batch.js";
// A guest's unacknowledged updates (see Provider.flush).
const GUEST_WINDOW = 2;
// Receipts (see Provider.receipt).
const RECEIPT_BYTES = 256 * 1024,
  RECEIPT_DELAY = 200;

export class Pending {
  constructor() {
    this.items = new Map();
    this.bytes = 0;
    this.known = null;
    this.connected = false;
    this.readonly = false;
    this.stopped = false;
    // A local change that could not even be queued (see Provider.update).
    this.unqueued = false;
  }
  // unsynced reports local text the server does not have: queued updates,
  // which a stop keeps, or a change that could not be queued.
  unsynced() {
    return this.items.size > 0 || this.unqueued;
  }
  add(id, u) {
    if (this.items.has(id)) return;
    this.fit(u.length);
    this.items.set(id, u);
    this.bytes += u.length;
  }
  // replace swaps the bytes of a pending update that was never sent, for a
  // merged one. Its place in the order stays.
  replace(id, u) {
    const old = this.items.get(id);
    this.fit(u.length - old.length);
    this.items.set(id, u);
    this.bytes += u.length - old.length;
  }
  fit(more) {
    if (this.bytes + more > 2 * 1024 * 1024)
      throw new Error(
        "Offline buffer is full. Download your text before reloading.",
      );
  }
  ack(id) {
    const u = this.items.get(id);
    if (u) {
      this.bytes -= u.length;
      this.items.delete(id);
    }
  }
  remember(m) {
    if (
      !this.known ||
      (m.epoch && m.epoch !== this.known.epoch) ||
      m.seq > this.known.seq
    )
      this.known = {
        epoch: m.epoch || this.known?.epoch,
        seq: m.seq,
        chain: m.chain,
      };
  }
  label() {
    return this.stopped
      ? "Sync stopped"
      : this.readonly
        ? "Read-only"
        : !this.connected
          ? "Offline — changes will sync"
          : this.items.size
            ? "Saving…"
            : "Saved";
  }
}

// Guests send at most 4 update frames and 64 KiB of decoded updates per
// second, below the server's per-guest limits (TECH 3.8 and 4); unsent
// updates are merged while the result stays within 64 KiB.
export const GUEST_PACE = Object.freeze({
  frames: 4,
  bytes: 64 * 1024,
  window: 1000,
});
// After a busy frame the page waits this long, then resends in order.
export const BUSY_WAIT = 1000;

// Pacer keeps the frames sent in the last window. delay is how long a
// frame of size bytes must wait; a frame larger than the byte budget is
// sent alone once the window is empty.
export class Pacer {
  constructor(limits = GUEST_PACE, now = () => Date.now()) {
    this.limits = limits;
    this.now = now;
    this.log = [];
  }
  delay(size) {
    const { frames, bytes, window } = this.limits,
      t = this.now();
    while (this.log.length && this.log[0].t <= t - window) this.log.shift();
    let count = this.log.length,
      sum = this.log.reduce((n, e) => n + e.size, 0);
    if (count < frames && (count === 0 || sum + size <= bytes)) return 0;
    for (const e of this.log) {
      count--;
      sum -= e.size;
      if (count < frames && (count === 0 || sum + size <= bytes))
        return e.t + window - t;
    }
    return 0;
  }
  record(size) {
    this.log.push({ t: this.now(), size });
  }
}

// Close reasons after which the page reconnects on its own.
const TRANSIENT = [
  "send byte limit",
  "flat send byte limit",
  "send queue full",
  "slow consumer",
  "connection capacity",
  "idle connection",
];

export class Provider {
  constructor(
    doc,
    path,
    {
      readonly = false,
      name = "Guest",
      editor,
      onState = () => {},
      onWelcome = () => {},
      onStop = () => {},
      // hello returns the guest and viewer fields of the next hello.
      hello = () => ({}),
      // onAccess sees a welcome before it is applied; false drops it (the
      // page connects again differently).
      onAccess = () => true,
      onLevel = () => {},
      onViewers = () => {},
      // onError may handle an error frame itself by returning true.
      onError = () => false,
      // onCapacity and onUnreachable return true to stop reconnecting until
      // resume: a full reader pool, or failed handshakes.
      onCapacity = () => false,
      onUnreachable = () => false,
      WebSocketClass = globalThis.WebSocket,
      // Options of the RemoteBatcher that applies remote updates.
      batch = {},
    } = {},
  ) {
    this.doc = doc;
    this.path = path;
    this.name = cleanName(name);
    this.editor = editor;
    this.model = new Pending();
    this.model.readonly = readonly;
    this.onState = onState;
    this.onWelcome = onWelcome;
    this.onStop = onStop;
    this.helloFields = hello;
    this.onAccess = onAccess;
    this.onLevel = onLevel;
    this.onViewers = onViewers;
    this.onError = onError;
    this.onCapacity = onCapacity;
    this.onUnreachable = onUnreachable;
    this.WS = WebSocketClass;
    this.attempt = 0;
    this.failures = 0;
    this.awareness = new Awareness(doc);
    this.destroyed = false;
    this.paused = false;
    this.generation = 0;
    // Sending: ids waiting to be sent in order, the raw bytes of those
    // never sent (only they are merged) and ids sent on this connection
    // since the last resend. paced is set for guests.
    this.paced = false;
    this.pacer = new Pacer();
    this.unsent = [];
    this.raw = new Map();
    this.inflight = new Set();
    this.retryNext = false;
    // Remote updates are applied in batches (remote-batch.js). Only the
    // editor's view of the document waits: acknowledgements, the known
    // history position, receipts and sending never depend on it, and a
    // welcome, a new connection's hello and a stop apply the queue first.
    this.remote = new RemoteBatcher(
      (updates) => this.applyRemote(updates),
      batch,
    );
    this.update = (u, origin) => this.queue(u, origin);
    doc.on("update", this.update);
    this.awUpdate = (_, origin) => {
      if (origin === this) return;
      clearTimeout(this.awTimer);
      this.awTimer = setTimeout(() => this.sendAwareness(), 80);
    };
    this.awareness.on("update", this.awUpdate);
    this.offline = () => {
      this.model.connected = false;
      this.state();
      this.socket?.close();
    };
    this.online = () => {
      clearTimeout(this.reconnectTimer);
      if (!this.model.connected && !this.paused) this.connect();
    };
    globalThis.addEventListener?.("offline", this.offline);
    globalThis.addEventListener?.("online", this.online);
    this.connect();
    this.heartbeat = setInterval(() => {
      if (this.model.connected) {
        if (Date.now() - this.lastServer > 45000) this.socket?.close();
        else this.send({ t: "ping" });
      }
    }, 15000);
  }
  // queue keeps a local update until its ACK and sends it when connected.
  queue(u, origin) {
    if (origin === this || this.model.readonly || this.model.stopped) return;
    try {
      if (u.length > LIMITS.update)
        throw new Error(
          "This edit is too large to sync. Download your text before reloading.",
        );
      const last = this.unsent.at(-1);
      if (this.paced && this.raw.has(last)) {
        const merged = Y.mergeUpdates([this.raw.get(last), u]);
        if (merged.length <= GUEST_PACE.bytes) {
          this.model.replace(last, base64(merged));
          this.raw.set(last, merged);
          this.state();
          return;
        }
      }
      const id = randomUUID();
      this.model.add(id, base64(u));
      this.raw.set(id, u);
      this.unsent.push(id);
      this.flush();
      this.state();
    } catch (e) {
      this.model.unqueued = true;
      this.stop(e.message);
    }
  }
  // flush sends waiting updates in order; a guest's are paced.
  flush() {
    clearTimeout(this.paceTimer);
    this.paceTimer = 0;
    if (!this.model.connected || this.model.stopped || this.busyTimer) return;
    while (this.unsent.length) {
      const id = this.unsent[0],
        u = this.model.items.get(id);
      if (u === undefined) {
        this.unsent.shift();
        continue;
      }
      // A guest keeps at most GUEST_WINDOW updates unacknowledged, so its
      // own updates never queue up in the connection ahead of its receipts
      // when the server reads Public connections slowly.
      if (this.paced && this.inflight.size >= GUEST_WINDOW) return;
      const size = this.raw.get(id)?.length ?? decodedLength(u);
      const wait = this.paced ? this.pacer.delay(size) : 0;
      if (wait > 0) {
        this.paceTimer = setTimeout(() => this.flush(), wait);
        return;
      }
      this.unsent.shift();
      this.raw.delete(id);
      const frame = { t: "update", id, u };
      if (this.retryNext) frame.retry = true;
      this.retryNext = false;
      this.send(frame);
      this.inflight.add(id);
      if (this.paced) this.pacer.record(size);
    }
  }
  // busy: the flat's guests together are over their limit and the server
  // applies nothing more from this connection until a retry. Wait, then
  // resend every unacknowledged update in its original order.
  busy(id) {
    if (!this.inflight.has(id)) return;
    this.inflight.clear();
    clearTimeout(this.paceTimer);
    this.paceTimer = 0;
    this.unsent = [...this.model.items.keys()];
    this.retryNext = true;
    clearTimeout(this.busyTimer);
    this.busyTimer = setTimeout(() => {
      this.busyTimer = 0;
      this.flush();
    }, BUSY_WAIT);
  }
  state() {
    this.onState(this.model.label(), this.model);
  }
  // catchUp asks for committed changes now instead of at the next
  // heartbeat, for example after a restore.
  catchUp() {
    if (this.model.connected) this.send({ t: "ping" });
  }
  send(m) {
    if (this.socket?.readyState === 1 && !this.model.stopped) {
      if (this.socket.bufferedAmount > 2 * 1024 * 1024) {
        this.socket.close();
        return;
      }
      this.socket.send(JSON.stringify(m));
    }
  }
  sendAwareness() {
    if (this.model.connected && !this.model.readonly)
      this.send({
        t: "aw",
        u: base64(encodeAwarenessUpdate(this.awareness, [this.doc.clientID])),
      });
  }
  connect() {
    if (this.destroyed || this.model.stopped || this.paused) return;
    if (globalThis.navigator?.onLine === false) {
      this.model.connected = false;
      this.state();
      return;
    }
    const generation = ++this.generation,
      url = new URL("/_docs/ws", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("doc", this.path);
    let opened = false;
    const socket = (this.socket = new this.WS(url.href));
    socket.onopen = () => {
      if (generation !== this.generation) return;
      opened = true;
      // The state vector then covers every update received so far.
      this.remote?.flush();
      this.send({
        t: "hello",
        v: 1,
        doc: this.path,
        sv: base64(Y.encodeStateVector(this.doc)),
        known: this.model.known || undefined,
        name: this.name,
        editor: this.editor,
        ...this.helloFields(),
      });
    };
    socket.onmessage = (e) => {
      if (generation !== this.generation) return;
      try {
        this.receive(JSON.parse(e.data), e.data.length);
      } catch {
        this.stop(
          "The server sent an invalid sync message. Download your text and reload.",
        );
      }
    };
    socket.onclose = (event) => {
      if (generation !== this.generation) return;
      this.disconnected();
      if (this.destroyed || this.model.stopped || this.paused) {
        this.state();
        return;
      }
      if (
        event.code === 1008 &&
        event.reason === "connection capacity" &&
        this.onCapacity()
      ) {
        this.paused = true;
        this.state();
        return;
      }
      if (event.code === 1008 && !TRANSIENT.includes(event.reason)) {
        this.stop("Sync was rejected. Download your local text and reload.");
        return;
      }
      // A handshake the server refused (HTTP 503 when its Public side is
      // full) looks like a network failure here.
      this.failures = opened ? 0 : this.failures + 1;
      if (!opened && this.onUnreachable(this.failures)) {
        this.paused = true;
        this.state();
        return;
      }
      this.state();
      this.reconnectTimer = setTimeout(
        () => this.connect(),
        Math.min(15000, 500 * 2 ** Math.min(this.attempt++, 5)) *
          (0.75 + Math.random() * 0.5),
      );
    };
    socket.onerror = () => socket.close();
    this.state();
  }
  // disconnected forgets what only the closed connection knew: peers'
  // presence and which updates it was sent.
  disconnected() {
    this.model.connected = false;
    // Frame numbers start again on the next connection.
    clearTimeout(this.receiptTimer);
    this.receiptTimer = this.unacked = this.unackedBytes = 0;
    this.inflight.clear();
    clearTimeout(this.paceTimer);
    clearTimeout(this.busyTimer);
    this.paceTimer = this.busyTimer = 0;
    removeAwarenessStates(
      this.awareness,
      [...this.awareness.getStates().keys()].filter(
        (id) => id !== this.doc.clientID,
      ),
      this,
    );
  }
  // reconnect drops the connection and connects at once with a new hello,
  // for example as a guest after the visitor joined.
  reconnect() {
    if (this.destroyed || this.model.stopped) return;
    this.paused = false;
    this.attempt = 0;
    this.failures = 0;
    clearTimeout(this.reconnectTimer);
    const old = this.socket;
    this.generation++;
    this.disconnected();
    old?.close();
    this.connect();
  }
  // pause closes the connection and waits for reconnect.
  pause() {
    this.paused = true;
    clearTimeout(this.reconnectTimer);
    this.socket?.close();
    this.state();
  }
  receive(m, size = 0) {
    this.lastServer = Date.now();
    if (m.t === "diverged") {
      this.stop(
        "The document history was restored. Your local text is available to download; reload to use the restored document.",
      );
      return;
    }
    if (m.t === "error") {
      if (this.onError(m)) return;
      this.stop(
        m.code === "readonly"
          ? "Editing permission changed. Download your text and reload."
          : m.code === "disk_quota"
            ? "This edit was not saved: the document is out of disk space. Download your text, ask the operator to free space or raise the disk quota, then reload."
            : "An edit was rejected. Download your text and reload to synchronize.",
      );
      return;
    }
    if (m.t === "welcome") {
      if (m.v !== 1 || m.doc !== this.path) throw new Error("invalid welcome");
      if (this.model.known && this.model.known.epoch !== m.epoch)
        throw new Error("epoch changed");
      if (this.onAccess(m) === false) return;
      this.model.readonly = m.readonly;
      this.remote?.flush();
      Y.applyUpdate(this.doc, unbase64(m.u, LIMITS.state), this);
      this.model.remember(m);
      this.model.connected = true;
      this.attempt = 0;
      this.failures = 0;
      const you = m.you || { name: m.me?.name, verified: false };
      this.awareness.setLocalStateField("user", {
        name: you.name,
        verified: you.verified,
        ...(m.me?.kind === "guest" ? { guest: true } : {}),
        color: "#3b65b9",
        colorLight: "#3b65b933",
      });
      this.onWelcome(m);
      if (!m.readonly) {
        // Every unacknowledged update is sent again, in order; the server
        // acknowledges ids it already committed.
        this.inflight.clear();
        this.retryNext = false;
        this.unsent = [...this.model.items.keys()];
        this.flush();
      } else if (this.model.items.size) {
        this.stop(
          "This connection is now read-only. Download your unsaved text.",
        );
        return;
      }
      this.sendAwareness();
    } else if (m.t === "update") {
      const u = unbase64(m.u, LIMITS.state);
      if (this.remote) this.remote.push(u);
      else Y.applyUpdate(this.doc, u, this);
      this.model.remember(m);
    } else if (m.t === "ack") {
      this.model.ack(m.id);
      this.inflight.delete(m.id);
      this.raw.delete(m.id);
      this.model.remember(m);
      if (this.paced && this.unsent.length) this.flush();
    } else if (m.t === "busy") {
      this.busy(m.id);
    } else if (m.t === "aw") {
      applyAwarenessUpdate(
        this.awareness,
        unbase64(m.u, LIMITS.awareness * 50),
        this,
      );
    } else if (m.t === "pong") {
      this.model.remember(m);
    } else if (m.t === "level") {
      this.onLevel(m);
    } else if (m.t === "viewers") {
      this.onViewers(m);
    } else throw new Error("unknown response");
    if (Number.isSafeInteger(m.n)) this.receipt(m.n, size);
    this.state();
  }
  // receipt acknowledges server frames. A receipt covers every frame up to
  // its number, so the page sends one for each busy moment instead of one
  // per frame: at once after 256 KiB, otherwise 200 ms after the first
  // unacknowledged frame. A Public connection's frames count against small
  // per-connection limits, which its own updates need too.
  receipt(n, size) {
    this.unacked = Math.max(this.unacked || 0, n);
    this.unackedBytes = (this.unackedBytes || 0) + size;
    if (this.unackedBytes >= RECEIPT_BYTES) return this.flushReceipt();
    this.receiptTimer ||= setTimeout(() => this.flushReceipt(), RECEIPT_DELAY);
  }
  flushReceipt() {
    clearTimeout(this.receiptTimer);
    this.receiptTimer = 0;
    if (!this.unacked) return;
    this.send({ t: "received", n: this.unacked });
    this.unacked = this.unackedBytes = 0;
  }
  // applyRemote applies a batch of remote updates as one.
  applyRemote(updates) {
    try {
      Y.applyUpdate(
        this.doc,
        updates.length === 1 ? updates[0] : Y.mergeUpdates(updates),
        this,
      );
    } catch {
      this.stop(
        "The server sent an invalid sync message. Download your text and reload.",
      );
    }
  }
  // halt stops syncing like stop, for a page that explains why itself.
  halt() {
    // Text the server sent is kept, as it would have been without batching.
    this.remote?.flush();
    this.model.stopped = true;
    this.model.connected = false;
    clearTimeout(this.reconnectTimer);
    clearTimeout(this.paceTimer);
    clearTimeout(this.busyTimer);
    this.socket?.close();
    this.state();
  }
  stop(message) {
    this.halt();
    this.onStop(message);
  }
  destroy() {
    this.destroyed = true;
    globalThis.removeEventListener?.("offline", this.offline);
    globalThis.removeEventListener?.("online", this.online);
    clearInterval(this.heartbeat);
    clearTimeout(this.awTimer);
    clearTimeout(this.reconnectTimer);
    clearTimeout(this.receiptTimer);
    clearTimeout(this.paceTimer);
    clearTimeout(this.busyTimer);
    this.remote?.destroy();
    this.awareness.setLocalState(null);
    this.awareness.destroy();
    this.doc.off("update", this.update);
    this.socket?.close();
  }
}

// decodedLength is the byte length of canonical base64.
function decodedLength(s) {
  const padding = s.endsWith("==") ? 2 : s.endsWith("=") ? 1 : 0;
  return (s.length / 4) * 3 - padding;
}
