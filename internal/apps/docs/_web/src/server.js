import content from "./content.js";
import { clientJS, clientCSS } from "./assets.js";
import * as Y from "yjs";
import {
  LIMITS,
  base64,
  unbase64,
  parseFrame,
  utf8Bytes,
  decodePresence,
  encodePresence,
  cleanName,
  safeCursor,
} from "./protocol.js";
import {
  schema,
  conflictList,
  current,
  rejectionCode,
  transaction,
  activate,
  append,
  compact,
  validateState,
  history,
  digest,
  validateUpdate,
  structureCount,
  validateIncremental,
} from "./storage.js";
const rooms = new Map(),
  connections = new Map();
let queued = 0;
const sourceFor = (path) => content.documents.find((d) => d.path === path);
const titleFor = (d) =>
  (d.path === content.entry && content.title) ||
  (/^#\s+(.+)$/m.exec(d.text) || [])[1] ||
  d.path;
const escapeHTML = (s) =>
  String(s).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
function send(c, m) {
  if (c.closed) return;
  const n = ++c.serial,
    frame = JSON.stringify({ ...m, n }),
    size = utf8Bytes(frame);
  if (
    c.bytes + size > LIMITS.sendBytes ||
    queued + size > LIMITS.flatSendBytes
  ) {
    c.closed = true;
    c.ws.close(1008, "slow consumer");
    return;
  }
  c.bytes += size;
  queued += size;
  c.sent.set(n, size);
  try {
    c.ws.send(frame);
  } catch {
    c.closed = true;
    c.ws.close(1011, "send failed");
  }
}
function received(c, n) {
  if (!Number.isSafeInteger(n) || n < 0 || n > c.serial)
    throw new Error("invalid receipt");
  for (const [id, size] of c.sent) {
    if (id > n) break;
    c.bytes -= size;
    queued -= size;
    c.sent.delete(id);
  }
}
function broadcast(room, m, except) {
  for (const c of room.peers)
    if (
      c !== except &&
      c.joined &&
      (!["aw", "conflicts"].includes(m.t) || !c.readonly)
    )
      send(c, m);
}
function error(c, code, message, fatal = true) {
  send(c, { t: "error", code, message });
  if (fatal) {
    c.closed = true;
    c.ws.close(1008, code);
  }
}
function drop(room) {
  if (rooms.get(room.path) === room) rooms.delete(room.path);
  for (const c of room.peers) {
    c.closed = true;
    c.ws.close(1012, "resync");
  }
  room.doc?.destroy();
  room.doc = null;
  room.size = 0;
  room.structs = 0;
}
function roomState(room, s) {
  const previous = room.doc;
  room.doc = s.doc;
  room.row = s.row;
  room.size = s.doc._flatsStateSize || 0;
  room.structs = structureCount(s.doc);
  if (previous && previous !== s.doc) previous.destroy();
}
function committed(room, s, except) {
  if (s.reset) {
    broadcast(
      room,
      {
        t: "update",
        u: base64(Y.encodeStateAsUpdate(s.doc)),
        seq: s.row.seq,
        chain: s.row.chain,
      },
      except,
    );
  } else
    for (const row of s.changes)
      broadcast(
        room,
        { t: "update", u: row.data, seq: row.seq, chain: row.chain },
        except,
      );
  if (s.activation) broadcast(room, s.activation, except);
  if (s.conflicts) broadcast(room, { t: "conflicts", conflicts: s.conflicts });
  roomState(room, s);
}
function roomBudget(room, state, doc, readonly) {
  let size = state.length,
    structs = structureCount(doc);
  for (const r of rooms.values())
    if (
      r !== room &&
      [...r.peers].some((c) => !c.closed && c.readonly === readonly)
    ) {
      size += r.size || 0;
      structs += r.structs || 0;
    }
  if (
    size > (readonly ? 2 * 1024 * 1024 : LIMITS.roomBytes) ||
    structs > (readonly ? 20000 : LIMITS.roomStructs)
  )
    throw Object.assign(new Error("active room memory capacity"), {
      code: "capacity",
    });
}
function rate(c, size, type) {
  const now = Date.now(),
    elapsed = (now - c.last) / 1000;
  c.last = now;
  c.tokens = Math.min(120, c.tokens + elapsed * 60);
  c.input = Math.min(1024 * 1024, c.input + elapsed * 256 * 1024);
  c.tokens--;
  c.input -= size;
  if (c.tokens < 0 || c.input < 0) throw new Error("rate limit");
  if (type === "aw") {
    c.awTokens = Math.min(20, c.awTokens + ((now - c.awLast) / 1000) * 20) - 1;
    c.awLast = now;
    if (c.awTokens < 0) throw new Error("awareness rate limit");
  }
}
// Only joined, authorized writers may consume the shared update budget.
function rateUpdate(r, size) {
  const now = Date.now();
  r.tokens = Math.min(
    80,
    (r.tokens ?? 80) + ((now - (r.last || now)) / 1000) * 40,
  );
  r.input =
    Math.min(
      512 * 1024,
      (r.input ?? 512 * 1024) + ((now - (r.last || now)) / 1000) * 512 * 1024,
    ) - size;
  r.last = now;
  r.tokens--;
  if (r.tokens < 0 || r.input < 0) throw new Error("room rate limit");
}
function presence(c, m) {
  if (c.readonly)
    return error(c, "readonly", "Presence requires editing access");
  const p = decodePresence(unbase64(m.u, LIMITS.awareness)),
    room = c.room;
  if (c.awID !== undefined && c.awID !== p.id)
    throw new Error("awareness id changed");
  for (const peer of room.peers)
    if (peer !== c && peer.awID === p.id) throw new Error("awareness id owned");
  c.awID = p.id;
  if (c.aw && p.clock < c.aw.clock) return;
  const cursor = safeCursor(p.state?.cursor);
  if (cursor && utf8Bytes(JSON.stringify(cursor)) > 2048)
    throw new Error("cursor too large");
  c.aw = {
    id: p.id,
    clock: p.clock,
    state:
      p.state === null
        ? null
        : {
            user: {
              name: c.you.name,
              verified: c.you.verified,
              color: color(p.id),
              colorLight: color(p.id) + "33",
            },
            ...(cursor ? { cursor } : {}),
          },
  };
  broadcast(room, { t: "aw", u: base64(encodePresence([c.aw])) });
}
function color(id) {
  return ["#3b65b9", "#ac4e69", "#30836b", "#9064b1", "#aa691b", "#297d96"][
    id % 6
  ];
}
function joined(c, m, env) {
  if (c.joined || m.v !== 1 || m.doc !== c.room.path)
    throw new Error("invalid hello");
  const sv = unbase64(m.sv, 8192);
  if (Y.decodeStateVector(sv).size > 1024)
    throw new Error("state vector limit");
  const room = c.room,
    db = env.DB;
  let s;
  try {
    s = current(
      db,
      sourceFor(room.path),
      content.hash,
      room.row ? room : undefined,
      c.ws.runtimeGeneration,
    );
    roomBudget(room, { length: s.doc._flatsStateSize }, s.doc, c.readonly);
  } catch (e) {
    if (!rejectionCode(e)) drop(room);
    else {
      room.doc?.destroy();
      room.doc = null;
    }
    throw e;
  }
  committed(room, s);
  if (!history(db, s.row, m.known)) {
    send(c, { t: "diverged", epoch: s.row.epoch, seq: s.row.seq });
    c.diverged = true;
    return;
  }
  const verified =
    c.ws.headers.get("tailscale-user-name") ||
    c.ws.headers.get("tailscale-user-login");
  c.you = { name: cleanName(verified || m.name), verified: !!verified };
  c.joined = true;
  send(c, {
    t: "welcome",
    v: 1,
    doc: room.path,
    epoch: s.row.epoch,
    seq: s.row.seq,
    chain: s.row.chain,
    readonly: c.readonly,
    u: base64(Y.encodeStateAsUpdate(s.doc, sv)),
    sv: base64(Y.encodeStateVector(s.doc)),
    you: c.you,
    ...(!c.readonly ? { conflicts: conflictList(db, room.path) } : {}),
  });
  const states = [...room.peers]
    .filter((p) => p.aw && p !== c)
    .map((p) => p.aw);
  if (!c.readonly && states.length)
    send(c, { t: "aw", u: base64(encodePresence(states)) });
}
function update(c, m, env, size) {
  if (c.readonly) return error(c, "readonly", "This connection is read-only");
  rateUpdate(c.room, size);
  if (
    typeof m.id !== "string" ||
    !/^[A-Za-z0-9:_-]{1,128}$/.test(m.id) ||
    m.id.startsWith("activation:") ||
    m.id.startsWith("seed:")
  )
    throw new Error("invalid update id");
  const bytes = unbase64(m.u);
  validateUpdate(bytes);
  const db = env.DB,
    room = c.room;
  let result;
  try {
    result = transaction(db, () => {
      const s = activate(
          db,
          sourceFor(room.path),
          content.hash,
          room,
          c.ws.runtimeGeneration,
        ),
        duplicate = db.query(
          "SELECT * FROM flats_docs_receipts WHERE doc=? AND id=?",
          [room.path, m.id],
        )[0];
      if (duplicate) {
        if (duplicate.hash !== digest(m.u))
          throw Object.assign(
            new Error("update id reused with different bytes"),
            { code: "invalid_update" },
          );
        return { s, ack: duplicate };
      }
      try {
        Y.applyUpdate(s.doc, bytes);
      } catch (e) {
        if (
          e?.name === "InternalError" ||
          /out of memory/i.test(String(e?.message))
        )
          throw e;
        e.code = "invalid_update";
        throw e;
      }
      const size = validateIncremental(s.doc, bytes.length);
      roomBudget(room, { length: size }, s.doc, c.readonly);
      const event = append(db, s, m.id, m.u);
      compact(db, s);
      return { s, event, ack: event };
    });
  } catch (e) {
    if (!rejectionCode(e)) drop(room);
    else {
      room.doc?.destroy();
      room.doc = null;
    }
    throw e;
  }
  if (result.s.activation || result.s.reset || result.s.changes.length)
    result.s.conflicts = conflictList(db, room.path);
  committed(room, result.s);
  if (result.event) broadcast(room, result.event, c);
  send(c, { t: "ack", id: m.id, seq: result.ack.seq, chain: result.ack.chain });
}
function ping(c, env) {
  const room = c.room;
  let s;
  try {
    s = current(
      env.DB,
      sourceFor(room.path),
      content.hash,
      room.row ? room : undefined,
      c.ws.runtimeGeneration,
    );
    roomBudget(room, { length: s.doc._flatsStateSize }, s.doc, c.readonly);
  } catch (e) {
    if (!rejectionCode(e)) drop(room);
    else {
      room.doc?.destroy();
      room.doc = null;
    }
    throw e;
  }
  if (s.row.seq !== room.row?.seq)
    s.conflicts = conflictList(env.DB, room.path);
  committed(room, s);
  send(c, {
    t: "pong",
    epoch: s.row.epoch,
    seq: s.row.seq,
    chain: s.row.chain,
  });
}
function html(d, readonly, url) {
  const host = url.host;
  if (!/^[a-zA-Z0-9.:[\]-]+$/.test(host))
    return new Response("Invalid host", { status: 400 });
  const csp = `default-src 'self'; script-src 'self'; connect-src 'self' ws://${host} wss://${host}; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`;
  return new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${escapeHTML(titleFor(d))} · Flats</title><link rel="stylesheet" href="/_docs/assets/client.css"><script type="module" src="/_docs/assets/client.js"></script></head><body data-doc="${escapeHTML(d.path)}" data-readonly="${readonly}"><header><a class="brand" href="/" aria-label="Flats documents">Flats<span> / docs</span></a><h1>${escapeHTML(titleFor(d))}</h1><span id="status" role="status" aria-live="polite">Connecting…</span><div id="presence" aria-label="People in this document"></div></header><div id="notice" role="alert" hidden></div><div id="conflict-notice" role="status" hidden></div><div class="workspace"><nav id="documents" aria-label="Documents" hidden></nav><main><div class="toolbar"><span id="path">${escapeHTML(d.path)}</span><div id="modes" role="group" aria-label="Document view"><button data-mode="edit" aria-pressed="false">Edit</button><button data-mode="split" aria-pressed="true">Split</button><button data-mode="preview" aria-pressed="false">Preview</button></div></div><div id="panes"><section id="editor" aria-label="Markdown editor"></section><article id="preview" aria-label="Document preview"></article></div></main></div><dialog id="name-dialog" aria-labelledby="name-title"><form method="dialog"><h2 id="name-title">Your name</h2><p>Let collaborators know who is here.</p><label for="display-name">Display name</label><input id="display-name" maxlength="64" autocomplete="nickname" required><button>Join document</button></form></dialog></body></html>`,
    {
      headers: {
        "content-type": "text/html; charset=utf-8",
        "content-security-policy": csp,
        "x-content-type-options": "nosniff",
        "referrer-policy": "same-origin",
        "cache-control": "no-store",
      },
    },
  );
}
export default {
  fetch(request, env) {
    const url = new URL(request.url),
      path = url.pathname;
    if (request.method !== "GET" && request.method !== "HEAD")
      return new Response("Method not allowed", { status: 405 });
    if (path === "/_docs/healthz") {
      if (request.headers.get("x-flats-health") !== "1")
        return new Response("ok");
      schema(env.DB);
      transaction(env.DB, () => {
        for (const d of content.documents) {
          const s = activate(
            env.DB,
            d,
            content.hash,
            undefined,
            request.runtimeGeneration,
          );
          s.doc.destroy();
        }
      });
      return new Response("ok");
    }
    if (
      path === "/_docs/assets/client.js" ||
      path === "/_docs/assets/client.css"
    )
      return new Response(path.endsWith(".js") ? clientJS : clientCSS, {
        headers: {
          "content-type": path.endsWith(".js")
            ? "text/javascript; charset=utf-8"
            : "text/css; charset=utf-8",
          "x-content-type-options": "nosniff",
          "cache-control": "no-cache",
        },
      });
    if (path === "/_docs/api/documents")
      return Response.json({
        format: 1,
        entry: content.entry,
        title: content.title,
        documents: content.documents.map((d) => ({
          path: d.path,
          title: titleFor(d),
        })),
      });
    let docpath;
    if (path === "/_docs/api/document" || path === "/_docs/api/conflict")
      docpath = url.searchParams.get("doc") || content.entry;
    else if (path === "/") docpath = content.entry;
    else {
      try {
        docpath = decodeURIComponent(path.slice(1));
      } catch {
        return new Response("Not found", { status: 404 });
      }
      if (!sourceFor(docpath)) {
        if (sourceFor(docpath + ".md")) docpath += ".md";
        else if (sourceFor(docpath + ".markdown")) docpath += ".markdown";
      }
    }
    const d = sourceFor(docpath);
    if (
      !d ||
      (path.startsWith("/_docs/") &&
        path !== "/_docs/api/document" &&
        path !== "/_docs/api/conflict")
    )
      return new Response("Unknown document", { status: 404 });
    const privateAccess = request.headers.get("x-flats-access") === "private";
    if (path === "/_docs/api/conflict") {
      if (!privateAccess) return new Response("Forbidden", { status: 403 });
      const generation = Number(url.searchParams.get("generation"));
      if (!Number.isSafeInteger(generation) || generation < 1)
        return new Response("Not found", { status: 404 });
      const row = env.DB.query(
        "SELECT generation,markdown FROM flats_docs_conflicts WHERE doc=? AND generation=?",
        [d.path, generation],
      )[0];
      if (!row) return new Response("Not found", { status: 404 });
      const headers = {
        "cache-control": "no-store",
        "x-content-type-options": "nosniff",
        "content-security-policy":
          "default-src 'none'; frame-ancestors 'none'; sandbox",
      };
      return url.searchParams.get("view") === "1"
        ? new Response(row.markdown, {
            headers: {
              ...headers,
              "content-type": "text/plain; charset=utf-8",
            },
          })
        : Response.json(row, { headers });
    }
    if (path !== "/_docs/api/document")
      return html(d, request.headers.get("x-flats-access") !== "private", url);
    const s = current(
      env.DB,
      d,
      content.hash,
      undefined,
      request.runtimeGeneration,
    );
    const result = {
      format: 1,
      doc: d.path,
      markdown: s.doc.getText("markdown").toString(),
      epoch: s.row.epoch,
      seq: s.row.seq,
      chain: s.row.chain,
      source: "live",
      ...(privateAccess ? { conflicts: conflictList(env.DB, d.path) } : {}),
    };
    s.doc.destroy();
    return Response.json(result);
  },
  websocket: {
    open(ws) {
      ws.setSendLimits(LIMITS.sendBytes, LIMITS.flatSendBytes);
      const url = new URL(ws.url),
        path = url.searchParams.get("doc") || content.entry;
      const origin = ws.headers.get("origin");
      if (origin && origin !== url.origin) {
        ws.close(1008, "cross-origin connection");
        return;
      }
      if (url.pathname !== "/_docs/ws" || !sourceFor(path)) {
        ws.close(1008, "unknown document");
        return;
      }
      for (const peer of connections.values())
        if (Date.now() - peer.last > (peer.joined ? 60000 : 15000)) {
          peer.closed = true;
          peer.ws.close(1008, "idle connection");
        }
      const readonly = ws.headers.get("x-flats-access") !== "private";
      const peers = [...connections.values()].filter(
        (c) => c.readonly === readonly && !c.closed,
      );
      const activePaths = new Set(peers.map((c) => c.room.path));
      let room = rooms.get(path);
      if (
        peers.length >= (readonly ? 40 : LIMITS.connections) ||
        peers.filter((c) => c.room.path === path).length >=
          (readonly ? 10 : LIMITS.perRoom) ||
        (!activePaths.has(path) &&
          activePaths.size >= (readonly ? 4 : LIMITS.rooms))
      ) {
        ws.close(1008, "connection capacity");
        return;
      }
      if (!room) {
        room = { path, peers: new Set(), size: 0 };
        rooms.set(path, room);
      }
      const now = Date.now(),
        c = {
          ws,
          room,
          readonly,
          serial: 0,
          sent: new Map(),
          bytes: 0,
          tokens: 120,
          input: 1024 * 1024,
          last: now,
          awTokens: 20,
          awLast: now,
        };
      room.peers.add(c);
      connections.set(ws.id, c);
    },
    message(ws, data, env) {
      const c = connections.get(ws.id);
      if (!c || c.closed || c.diverged) return;
      try {
        const m = parseFrame(data),
          size = utf8Bytes(data);
        rate(c, size, m.t);
        if (m.t === "received") {
          received(c, m.n);
          return;
        }
        if (m.t === "hello") {
          joined(c, m, env);
          return;
        }
        if (!c.joined) throw new Error("hello required");
        if (m.t === "update") update(c, m, env, size);
        else if (m.t === "aw") presence(c, m);
        else if (m.t === "ping") ping(c, env);
        else throw new Error("unknown message");
      } catch (e) {
        if (
          e?.name === "InternalError" ||
          /out of memory/i.test(String(e?.message))
        )
          throw e;
        const code =
          rejectionCode(e) ||
          (/too large|limit/.test(String(e.message))
            ? "capacity"
            : "invalid_update");
        if (!c.closed)
          error(
            c,
            code,
            "Sync stopped; download your local Markdown and reload",
          );
        console.warn("docs rejected:", String(e.message));
      }
    },
    close(ws) {
      const c = connections.get(ws.id);
      if (!c) return;
      connections.delete(ws.id);
      queued -= c.bytes;
      c.bytes = 0;
      c.room.peers.delete(c);
      if (c.aw) {
        broadcast(c.room, {
          t: "aw",
          u: base64(encodePresence([{ ...c.aw, state: null }])),
        });
      }
      if (!c.room.peers.size) {
        if (rooms.get(c.room.path) === c.room) rooms.delete(c.room.path);
        c.room.doc?.destroy();
      }
    },
  },
};
