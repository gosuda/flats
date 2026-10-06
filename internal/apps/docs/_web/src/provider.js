import * as Y from "yjs";
import {
  Awareness,
  encodeAwarenessUpdate,
  applyAwarenessUpdate,
  removeAwarenessStates,
} from "y-protocols/awareness";
import { base64, unbase64, LIMITS, cleanName } from "./protocol.js";
export class Pending {
  constructor() {
    this.items = new Map();
    this.bytes = 0;
    this.known = null;
    this.connected = false;
    this.readonly = false;
    this.stopped = false;
  }
  add(id, u) {
    if (this.items.has(id)) return;
    if (this.bytes + u.length > 2 * 1024 * 1024)
      throw new Error(
        "Offline buffer is full. Download your text before reloading.",
      );
    this.items.set(id, u);
    this.bytes += u.length;
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
export class Provider {
  constructor(
    doc,
    path,
    {
      readonly = false,
      name = "Guest",
      onState = () => {},
      onWelcome = () => {},
      onStop = () => {},
      onConflicts = () => {},
      WebSocketClass = globalThis.WebSocket,
    } = {},
  ) {
    this.doc = doc;
    this.path = path;
    this.name = cleanName(name);
    this.model = new Pending();
    this.model.readonly = readonly;
    this.onState = onState;
    this.onWelcome = onWelcome;
    this.onStop = onStop;
    this.onConflicts = onConflicts;
    this.WS = WebSocketClass;
    this.attempt = 0;
    this.awareness = new Awareness(doc);
    this.destroyed = false;
    this.generation = 0;
    this.update = (u, origin) => {
      if (origin === this || this.model.readonly || this.model.stopped) return;
      try {
        if (u.length > LIMITS.update)
          throw new Error(
            "This edit is too large to sync. Download your text before reloading.",
          );
        const id = crypto.randomUUID();
        this.model.add(id, base64(u));
        if (this.model.connected) this.send({ t: "update", id, u: base64(u) });
        this.state();
      } catch (e) {
        this.stop(e.message);
      }
    };
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
      if (!this.model.connected) this.connect();
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
  state() {
    this.onState(this.model.label(), this.model);
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
    if (this.destroyed || this.model.stopped) return;
    if (globalThis.navigator?.onLine === false) {
      this.model.connected = false;
      this.state();
      return;
    }
    const generation = ++this.generation,
      url = new URL("/_docs/ws", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("doc", this.path);
    const socket = (this.socket = new this.WS(url.href));
    socket.onopen = () => {
      if (generation !== this.generation) return;
      this.send({
        t: "hello",
        v: 1,
        doc: this.path,
        sv: base64(Y.encodeStateVector(this.doc)),
        known: this.model.known || undefined,
        name: this.name,
      });
    };
    socket.onmessage = (e) => {
      if (generation !== this.generation) return;
      try {
        this.receive(JSON.parse(e.data));
      } catch {
        this.stop(
          "The server sent an invalid sync message. Download your text and reload.",
        );
      }
    };
    socket.onclose = (event) => {
      if (generation !== this.generation) return;
      this.model.connected = false;
      if (
        event.code === 1008 &&
        ![
          "send byte limit",
          "flat send byte limit",
          "send queue full",
          "slow consumer",
          "connection capacity",
          "idle connection",
        ].includes(event.reason) &&
        !this.destroyed &&
        !this.model.stopped
      ) {
        this.stop("Sync was rejected. Download your local text and reload.");
        return;
      }
      removeAwarenessStates(
        this.awareness,
        [...this.awareness.getStates().keys()].filter(
          (id) => id !== this.doc.clientID,
        ),
        this,
      );
      this.state();
      if (!this.destroyed && !this.model.stopped)
        this.reconnectTimer = setTimeout(
          () => this.connect(),
          Math.min(15000, 500 * 2 ** Math.min(this.attempt++, 5)) *
            (0.75 + Math.random() * 0.5),
        );
    };
    socket.onerror = () => socket.close();
    this.state();
  }
  receive(m) {
    this.lastServer = Date.now();
    if (m.t === "diverged") {
      this.stop(
        "The document history was restored. Your local text is available to download; reload to use the restored document.",
      );
      return;
    }
    if (m.t === "error") {
      this.stop(
        m.code === "readonly"
          ? "Editing permission changed. Download your text and reload."
          : "An edit was rejected. Download your text and reload to synchronize.",
      );
      return;
    }
    if (m.t === "welcome") {
      if (m.v !== 1 || m.doc !== this.path) throw new Error("invalid welcome");
      if (this.model.known && this.model.known.epoch !== m.epoch)
        throw new Error("epoch changed");
      this.model.readonly = m.readonly;
      Y.applyUpdate(this.doc, unbase64(m.u, LIMITS.state), this);
      this.model.remember(m);
      this.model.connected = true;
      this.attempt = 0;
      this.awareness.setLocalStateField("user", {
        name: m.you.name,
        verified: m.you.verified,
        color: "#3b65b9",
        colorLight: "#3b65b933",
      });
      this.onWelcome(m);
      if (!m.readonly && m.conflicts) this.onConflicts(m.conflicts);
      if (!m.readonly)
        for (const [id, u] of this.model.items)
          this.send({ t: "update", id, u });
      else if (this.model.items.size) {
        this.stop(
          "This connection is now read-only. Download your unsaved text.",
        );
        return;
      }
      this.sendAwareness();
    } else if (m.t === "update") {
      Y.applyUpdate(this.doc, unbase64(m.u, LIMITS.state), this);
      this.model.remember(m);
    } else if (m.t === "ack") {
      this.model.ack(m.id);
      this.model.remember(m);
    } else if (m.t === "aw") {
      applyAwarenessUpdate(
        this.awareness,
        unbase64(m.u, LIMITS.awareness * 50),
        this,
      );
    } else if (m.t === "conflicts") {
      if (!this.model.readonly) this.onConflicts(m.conflicts);
    } else if (m.t === "pong") {
      this.model.remember(m);
    } else throw new Error("unknown response");
    if (Number.isSafeInteger(m.n)) this.send({ t: "received", n: m.n });
    this.state();
  }
  rename(name) {
    this.name = cleanName(name);
    this.socket?.close();
  }
  stop(message) {
    this.model.stopped = true;
    this.model.connected = false;
    clearTimeout(this.reconnectTimer);
    this.socket?.close();
    this.state();
    this.onStop(message);
  }
  destroy() {
    this.destroyed = true;
    globalThis.removeEventListener?.("offline", this.offline);
    globalThis.removeEventListener?.("online", this.online);
    clearInterval(this.heartbeat);
    clearTimeout(this.awTimer);
    clearTimeout(this.reconnectTimer);
    this.awareness.setLocalState(null);
    this.awareness.destroy();
    this.doc.off("update", this.update);
    this.socket?.close();
  }
}
