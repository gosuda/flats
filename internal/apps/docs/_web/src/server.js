import content from "./content.js";
import { clientJS, clientCSS } from "./assets.js";
import * as Y from "yjs";
import {
  LIMITS,
  GUEST,
  Bucket,
  base64,
  unbase64,
  parseFrame,
  utf8Bytes,
  decodePresence,
  encodePresence,
  cleanName,
  cleanGuestName,
  safeCursor,
} from "./protocol.js";
import {
  schema,
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
  insertedClients,
  hasGC,
  hasPending,
  guestLines,
} from "./storage.js";
import { EDIT_LIMITS, outline, planEdit } from "./edit.js";
import * as revisions from "./revisions.js";
const rooms = new Map(),
  connections = new Map();
// Outstanding output of members, and of guests and viewers together.
let queued = 0,
  publicQueued = 0;
const sourceFor = (path) => content.documents.find((d) => d.path === path);
// The flat activation of this worker's version, for current().
const ensureFlat = (generation) => (db) =>
  revisions.ensureFlat(db, content, generation);
const headingOf = (text) => (/^#\s+(.+)$/m.exec(text) || [])[1];
const titleFor = (d) =>
  (d.path === content.entry && content.title) || headingOf(d.text) || d.path;
// Public pages never name a document by text it may no longer have
// (docs-public-edit.md rule 16a): the open document's title comes from its
// committed text, and the document list names the others by the manifest
// title or their path.
const publicTitle = (d, markdown) =>
  (d.path === content.entry && content.title) ||
  (markdown === undefined ? null : headingOf(markdown)) ||
  d.path.replace(/\.(md|markdown)$/, "");
const escapeHTML = (s) =>
  String(s).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
// Members connect on a Private route; on a Public route a connection is a
// guest after a valid guest hello while the flat is open for editing, else
// a viewer (docs/spec/docs-public-edit-TECH.md 3.3).
const MEMBER = "member",
  GUEST_KIND = "guest",
  VIEWER = "viewer";
// The public access level and epoch this VM last applied to its sockets.
let applied = null;
// Viewer numbers: token -> {n, open, released}, one numbering for the flat.
const viewerTokens = new Map();
// Limits of all guests of the flat (TECH 3.8).
const guestLimits = {
  starts: new Bucket(GUEST.startsPerMinute / 60, GUEST.startsPerMinute),
  updates: new Bucket(GUEST.flatUpdates, GUEST.flatUpdates),
  bytes: new Bucket(GUEST.flatBytes, GUEST.flatBytes),
  presence: new Bucket(GUEST.flatPresence, GUEST.flatPresence),
};
// The guest and viewer counts last written to meta.presence.
let presenceWritten = { guests: 0, viewers: 0, at: 0 };
const PRESENCE_REFRESH = 10 * 1000;
const randomToken = () =>
  Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
    b.toString(16).padStart(2, "0"),
  ).join("");
const validToken = (t) => typeof t === "string" && /^[0-9a-f]{32}$/.test(t);
const coded = (code, message) => Object.assign(new Error(message), { code });
function send(c, m) {
  if (c.closed) return;
  const n = ++c.serial,
    frame = JSON.stringify({ ...m, n }),
    size = utf8Bytes(frame),
    shared = c.public ? publicQueued : queued;
  if (
    c.bytes + size > LIMITS.sendBytes ||
    shared + size > (c.public ? LIMITS.publicSendBytes : LIMITS.flatSendBytes)
  ) {
    c.closed = true;
    c.ws.close(1008, "slow consumer");
    return;
  }
  charge(c, size);
  c.sent.set(n, size);
  try {
    c.ws.send(frame);
  } catch {
    c.closed = true;
    c.ws.close(1011, "send failed");
  }
}
// charge adds size to a connection's outstanding bytes and its class's.
function charge(c, size) {
  c.bytes += size;
  if (c.public) publicQueued += size;
  else queued += size;
}
function received(c, n) {
  if (!Number.isSafeInteger(n) || n < 0 || n > c.serial)
    throw new Error("invalid receipt");
  for (const [id, size] of c.sent) {
    if (id > n) break;
    charge(c, -size);
    c.sent.delete(id);
  }
}
function broadcast(room, m, except) {
  for (const c of room.peers)
    if (c !== except && c.joined && (m.t !== "aw" || !c.readonly)) send(c, m);
}
const MESSAGES = {
  disk_quota:
    "This edit would put the flat over its disk quota and was not saved; download your local Markdown",
  editing_ended: "The owner turned off guest editing",
  full: "This document is full",
  name_invalid:
    "A name is 1 to 40 characters, without control or invisible characters",
  guest_rate: "Changes arrived too fast, so editing stopped",
  guest_size: "This change is too large, so editing stopped",
  guest_room:
    "The rest of the room is kept for the documents' owners, so editing stopped",
  guest_client: "This change edits text it may not, so editing stopped",
};
function error(c, code, message, fatal = true) {
  send(c, {
    t: "error",
    code,
    message:
      message ||
      MESSAGES[code] ||
      "Sync stopped; download your local Markdown and reload",
  });
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
  roomState(room, s);
}
// A room is a guest room once a guest joined it, until it closes; a member
// room has a member and never had a guest; a reader room only ever had
// viewers. Guest rooms are bounded by the per-document limits and the
// guests' reserve, not by the member or reader sums (TECH 3.3).
function roomClass(room, kind) {
  if (room.guest || kind === GUEST_KIND) return GUEST_KIND;
  if (
    kind === MEMBER ||
    [...room.peers].some((c) => !c.closed && c.kind === MEMBER)
  )
    return MEMBER;
  return VIEWER;
}
function roomBudget(room, state, doc, kind) {
  const cls = roomClass(room, kind);
  if (cls === GUEST_KIND) return;
  let size = state.length,
    structs = structureCount(doc);
  for (const r of rooms.values())
    if (
      r !== room &&
      [...r.peers].some((c) => !c.closed) &&
      roomClass(r) === cls
    ) {
      size += r.size || 0;
      structs += r.structs || 0;
    }
  const reader = cls === VIEWER;
  if (
    size > (reader ? 2 * 1024 * 1024 : LIMITS.roomBytes) ||
    structs > (reader ? 20000 : LIMITS.roomStructs)
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
  // A guest's excess presence is dropped instead (presence).
  if (type === "aw" && c.kind !== GUEST_KIND) {
    c.awTokens = Math.min(20, c.awTokens + ((now - c.awLast) / 1000) * 20) - 1;
    c.awLast = now;
    if (c.awTokens < 0) throw new Error("awareness rate limit");
  }
}
// Only joined, authorized members may consume the shared update budget.
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

// --- public access level (TECH 3.1, 3.2) ---

// A new flat's database has no schema before its first document loads.
function readAccess(db) {
  try {
    return revisions.accessOf(db);
  } catch (e) {
    if (!String(e?.message).includes("no such table")) throw e;
    return { level: "", epoch: 0 };
  }
}
const accessFields = () =>
  applied.level
    ? { access: applied.level, access_epoch: applied.epoch }
    : { access_epoch: applied.epoch };
// reconcileAccess applies the stored level to every open socket: guests of
// another epoch are closed with editing_ended, and every socket that knows
// another level or epoch gets a level frame.
function reconcileAccess(env) {
  applied = readAccess(env.DB);
  for (const c of connections.values()) {
    if (c.closed || !c.joined) continue;
    if (
      c.kind === GUEST_KIND &&
      (applied.level !== "edit" || c.epoch !== applied.epoch)
    ) {
      error(c, "editing_ended");
      continue;
    }
    if (c.level !== applied.level || c.epochKnown !== applied.epoch) {
      send(c, { t: "level", ...accessFields() });
      c.level = applied.level;
      c.epochKnown = applied.epoch;
    }
  }
}
// guestCapable reports a Public socket whose request says edit at the
// stored level's epoch.
function guestCapable(c) {
  return (
    applied.level === "edit" &&
    c.ws.headers.get("x-flats-public-access") === "edit" &&
    c.ws.headers.get("x-flats-access-epoch") === String(applied.epoch)
  );
}

// --- pools (TECH 3.3) ---

function live(kind) {
  return [...connections.values()].filter((c) => !c.closed && c.kind === kind);
}
function poolFull(kind, path) {
  const [total, perRoom, roomsMax] =
    kind === MEMBER
      ? [LIMITS.connections, LIMITS.perRoom, LIMITS.rooms]
      : kind === GUEST_KIND
        ? [GUEST.connections, GUEST.perRoom, GUEST.rooms]
        : [GUEST.viewers, GUEST.viewersPerRoom, GUEST.viewerRooms];
  const peers = live(kind);
  if (peers.length >= total) return true;
  if (peers.filter((c) => c.path === path).length >= perRoom) return true;
  if (kind === GUEST_KIND) {
    const room = rooms.get(path),
      guestRooms = [...rooms.values()].filter(
        (r) => r.guest && r.peers.size,
      ).length;
    return !room?.guest && guestRooms >= roomsMax;
  }
  const paths = new Set(peers.map((c) => c.path));
  return !paths.has(path) && paths.size >= roomsMax;
}
function attach(c) {
  let room = rooms.get(c.path);
  if (!room) {
    room = { path: c.path, peers: new Set(), size: 0, guest: false };
    rooms.set(c.path, room);
  }
  room.peers.add(c);
  c.room = room;
}

// --- viewers (spec rules 9, 10) ---

function viewerNumber(token, now) {
  for (const [t, v] of viewerTokens)
    if (!v.open && now - v.released >= GUEST.viewerHold) viewerTokens.delete(t);
  let v = validToken(token) ? viewerTokens.get(token) : undefined;
  if (!v) {
    const taken = new Set([...viewerTokens.values()].map((x) => x.n));
    let n = 1;
    while (taken.has(n)) n++;
    token = randomToken();
    v = { n, open: 0, released: 0 };
    viewerTokens.set(token, v);
  }
  v.open++;
  return { token, v };
}
function viewersFrame(room) {
  const numbers = [
    ...new Set(
      [...room.peers]
        .filter((c) => c.joined && !c.closed && c.kind === VIEWER)
        .map((c) => c.viewer.n),
    ),
  ].sort((a, b) => a - b);
  return numbers.length > GUEST.viewersListed
    ? { t: "viewers", count: numbers.length }
    : { t: "viewers", n: numbers };
}
const seesViewers = (c) =>
  c.joined && !c.closed && (c.kind === MEMBER || c.kind === GUEST_KIND);
// flushViewers sends each room's changed viewer list to its members and
// guests, at most once a second.
function flushViewers(now = Date.now()) {
  for (const room of rooms.values()) {
    if (!room.viewersDue || now - (room.viewersAt || 0) < 1000) continue;
    room.viewersDue = false;
    const frame = viewersFrame(room),
      key = JSON.stringify(frame);
    if (key === (room.viewersSent ?? '{"t":"viewers","n":[]}')) continue;
    room.viewersSent = key;
    room.viewersAt = now;
    for (const c of room.peers) if (seesViewers(c)) sendViewers(c, frame);
  }
}
// sendViewers sends a viewers frame. Its n is the list of viewer numbers,
// so it carries no envelope number and takes no output credit; it is small
// and sent at most once a second per room.
function sendViewers(c, frame) {
  if (c.closed) return;
  try {
    c.ws.send(JSON.stringify(frame));
  } catch {
    c.closed = true;
    c.ws.close(1011, "send failed");
  }
}
// flushPresence writes the guest and viewer counts core reports: when they
// change (at most once a second) and, while a Public socket is open, every
// 10 seconds; the last Public socket closing writes zeros (TECH 3.9).
function flushPresence(env, now = Date.now()) {
  let guests = 0,
    viewers = 0,
    open = false,
    members = false;
  for (const c of connections.values()) {
    if (c.closed) continue;
    if (c.public) open = true;
    else members = true;
    if (!c.joined) continue;
    if (c.kind === GUEST_KIND) guests++;
    else if (c.kind === VIEWER) viewers++;
  }
  const changed =
    guests !== presenceWritten.guests || viewers !== presenceWritten.viewers;
  if (!changed && !(open && now - presenceWritten.at >= PRESENCE_REFRESH))
    return;
  if (changed && (guests || viewers) && now - presenceWritten.at < 1000) return;
  const value = { guests, viewers, at: now };
  // The counts are best effort. While any socket is open this VM never
  // waits for a writer another VM holds: a refused write is tried again
  // after a later event (at least the next heartbeat). Once every socket has
  // closed, waiting delays nobody, and no later event may come.
  const wait = !open && !members,
    db = env.DB,
    busy = db.query("PRAGMA busy_timeout")[0]?.timeout ?? 5000;
  if (!wait) db.exec("PRAGMA busy_timeout = 0");
  try {
    transaction(db, () => revisions.setMeta(db, "presence", value));
    presenceWritten = value;
  } catch (e) {
    if (
      e?.name === "InternalError" ||
      /out of memory/i.test(String(e?.message))
    )
      throw e;
    if (!/SQLITE_BUSY|database is locked/i.test(String(e?.message)))
      console.warn("docs presence not written:", String(e?.message));
  } finally {
    if (!wait) db.exec(`PRAGMA busy_timeout = ${Number(busy) || 5000}`);
  }
}

// --- presence (TECH 3.4) ---

// presence relays a member's or guest's awareness state, built once per
// audience: members see guests with the guest mark, guests see members only
// under their page nickname.
function presence(c, m, env) {
  if (c.readonly)
    return error(c, "readonly", "Presence requires editing access");
  const guest = c.kind === GUEST_KIND;
  if (guest && (!c.awBucket.take(1) || !guestLimits.presence.take(1))) return;
  const p = decodePresence(unbase64(m.u, LIMITS.awareness)),
    room = c.room;
  if (c.awID !== undefined && c.awID !== p.id)
    throw new Error("awareness id changed");
  for (const peer of room.peers)
    if (peer !== c && peer.awID === p.id) throw new Error("awareness id owned");
  if (c.awID === undefined) {
    if (guest) guestAwareness(c, p.id, env);
    // Reserved before the id is relayed, so no guest can claim it first.
    else revisions.reserveMemberClient(env.DB, c.path, p.id, c.editor);
  }
  c.awID = p.id;
  if (c.aw && p.clock < c.aw.clock) return;
  const cursor = safeCursor(p.state?.cursor);
  if (cursor && utf8Bytes(JSON.stringify(cursor)) > 2048)
    throw new Error("cursor too large");
  const state = (name, verified) =>
    p.state === null
      ? null
      : {
          user: {
            name,
            verified,
            ...(guest ? { guest: true } : {}),
            color: color(p.id),
            colorLight: color(p.id) + "33",
          },
          ...(cursor ? { cursor } : {}),
        };
  c.aw = { id: p.id, clock: p.clock, state: state(c.you.name, c.you.verified) };
  c.awGuest = guest
    ? c.aw
    : { id: p.id, clock: p.clock, state: state(c.nick || "Member", false) };
  relayPresence(room, c.aw, c.awGuest);
}
function relayPresence(room, forMembers, forGuests) {
  let members, guests;
  for (const peer of room.peers) {
    if (!peer.joined || peer.closed) continue;
    if (peer.kind === MEMBER)
      send(peer, {
        t: "aw",
        u: (members ??= base64(encodePresence([forMembers]))),
      });
    else if (peer.kind === GUEST_KIND)
      send(peer, {
        t: "aw",
        u: (guests ??= base64(encodePresence([forGuests]))),
      });
  }
}
// A guest's awareness id follows its client id rule: its own or new.
function guestAwareness(c, id, env) {
  const db = env.DB,
    row = db.query(
      "SELECT owner FROM flats_docs_guest_clients WHERE doc=? AND client=?",
      [c.path, id],
    )[0];
  if (!row && !c.room.doc) catchUp(c.room, env, c.ws.runtimeGeneration);
  if (row ? row.owner !== c.key : c.room.doc.store.clients.has(id))
    throw coded("guest_client", "this presence belongs to another editor");
}
function color(id) {
  return ["#3b65b9", "#ac4e69", "#30836b", "#9064b1", "#aa691b", "#297d96"][
    id % 6
  ];
}

// --- joining ---

// publicHello admits a Public socket as a guest or a viewer, after this VM
// applied the stored level to every open socket.
function publicHello(c, m, env) {
  if (c.joined || m.v !== 1 || m.doc !== c.path)
    throw new Error("invalid hello");
  reconcileAccess(env);
  const g = m.guest;
  if (g && typeof g === "object" && !Array.isArray(g) && guestCapable(c)) {
    const name = cleanGuestName(g.name);
    if (name === null) return error(c, "name_invalid");
    if (poolFull(GUEST_KIND, c.path) || !guestLimits.starts.take(1))
      return error(c, "full");
    let token = g.token,
      issued;
    if (!validToken(token)) issued = token = randomToken();
    Object.assign(c, {
      kind: GUEST_KIND,
      readonly: false,
      epoch: applied.epoch,
      key: "guest:" + digest(token).slice(0, 16),
      name,
      frames: new Bucket(GUEST.frames, GUEST.framesBurst),
      bytesIn: new Bucket(GUEST.bytes, GUEST.bytesBurst),
      awBucket: new Bucket(GUEST.presence, GUEST.presence),
    });
    c.editor = c.key;
    c.you = { name, verified: false };
    attach(c);
    joined(c, m, env, {
      me: { kind: GUEST_KIND, name },
      ...(issued ? { guest_token: issued } : {}),
    });
    if (c.joined) c.room.guest = true;
    return;
  }
  if (poolFull(VIEWER, c.path)) {
    c.closed = true;
    c.ws.close(1008, "connection capacity");
    return;
  }
  const { token, v } = viewerNumber(m.viewer_token, Date.now());
  Object.assign(c, { kind: VIEWER, viewer: v });
  attach(c);
  joined(c, m, env, {
    me: { kind: VIEWER, label: "Viewer " + v.n },
    viewer_token: token,
  });
  c.room.viewersDue = true;
}
function joined(c, m, env, extra) {
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
      ensureFlat(c.ws.runtimeGeneration),
    );
    roomBudget(room, { length: s.doc._flatsStateSize }, s.doc, c.kind);
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
  if (c.kind !== GUEST_KIND) {
    const login = c.ws.headers.get("tailscale-user-login"),
      verified = c.ws.headers.get("tailscale-user-name") || login;
    c.you = { name: cleanName(verified || m.name), verified: !!verified };
    if (c.kind === MEMBER) {
      reconcileAccess(env);
      // Guests see a member only under the page's nickname (rule 15).
      c.nick = cleanGuestName(m.nick) || "";
      // Edit groups are per person: a verified login, else the browser's own id.
      c.editor = login
        ? "ts:" + login.slice(0, 256)
        : "browser:" +
          (typeof m.editor === "string" &&
          /^[A-Za-z0-9_-]{8,64}$/.test(m.editor)
            ? m.editor
            : crypto.randomUUID());
    }
  }
  c.joined = true;
  c.level = applied.level;
  c.epochKnown = applied.epoch;
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
    ...accessFields(),
    guest_text:
      applied.level === "edit" || revisions.hasGuestText(db, room.path),
    ...extra,
  });
  if (c.kind === VIEWER) return;
  const states = [...room.peers]
    .filter((p) => p.aw && p !== c && !p.closed)
    .map((p) => (c.kind === GUEST_KIND ? p.awGuest : p.aw));
  if (states.length) send(c, { t: "aw", u: base64(encodePresence(states)) });
  const viewers = viewersFrame(room);
  if (viewers.count || viewers.n.length) sendViewers(c, viewers);
}

// --- updates ---

function validUpdateID(id) {
  if (
    typeof id !== "string" ||
    !/^[A-Za-z0-9:_-]{1,128}$/.test(id) ||
    id.startsWith("activation:") ||
    id.startsWith("seed:") ||
    id.startsWith("agent:") ||
    id.startsWith("restore:")
  )
    throw new Error("invalid update id");
}
function update(c, m, env, size) {
  if (c.readonly) return error(c, "readonly", "This connection is read-only");
  rateUpdate(c.room, size);
  validUpdateID(m.id);
  const bytes = unbase64(m.u);
  const clients = insertedClients(validateUpdate(bytes));
  commitUpdate(c, m, env, bytes, (db, s) => {
    revisions.checkMemberClients(db, c.room.path, clients);
    return () => {
      revisions.touchGroup(db, c.editor, c.you.name);
      // Checked last, with the revision this edit's group will need.
      revisions.checkDiskQuota(db, {
        path: c.room.path,
        text: s.doc.getText("markdown"),
      });
    };
  });
}
// guestUpdate admits a guest's update within the guest limits (TECH 3.5,
// 3.8): per socket, then the flat's guest buckets, which answer busy, then
// the room members keep, checked in the transaction that would commit.
function guestUpdate(c, m, env, size) {
  validUpdateID(m.id);
  // Over its own rate or the rate of all guests together a guest is slowed,
  // not refused (spec rule 22): the page waits and resends in order. Frames
  // held back by the runtime's ingress pacing can reach this VM together,
  // so even a paced page may meet its own bucket here.
  if (c.busy && m.retry !== true) return send(c, { t: "busy", id: m.id });
  if (
    !c.frames.take(1) ||
    !c.bytesIn.take(size) ||
    c.overBytes ||
    !guestLimits.updates.take(1)
  ) {
    c.busy = true;
    return send(c, { t: "busy", id: m.id });
  }
  c.busy = false;
  const bytes = unbase64(m.u);
  if (bytes.length > GUEST.update)
    throw coded("guest_size", "a guest's change is at most 64 KiB");
  const decoded = validateUpdate(bytes);
  if (hasGC(decoded))
    throw coded("guest_room", "a guest's change may not skip clocks");
  const clients = insertedClients(decoded),
    path = c.room.path;
  commitUpdate(
    c,
    m,
    env,
    bytes,
    (db, s) => {
      revisions.claimClients(db, path, c.key, clients, s.doc);
      const before = {
        length: s.doc.getText("markdown").length,
        structs: structureCount(s.doc),
        measured: revisions.measuredBytes(db),
      };
      return () => {
        guestShare(s.doc, before);
        if (Y.encodeStateVector(s.doc).length > GUEST.stateVector)
          throw coded(
            "guest_room",
            "this document has room for no more editors",
          );
        revisions.touchGuestGroup(db, c.name);
        revisions.checkDiskQuota(
          db,
          { path, text: s.doc.getText("markdown") },
          true,
        );
        revisions.chargeGuest(db, before.measured);
      };
    },
    true,
  );
}
// guestShare refuses a guest's update that takes the document above the
// guests' share of a document limit.
function guestShare(doc, before) {
  const text = doc.getText("markdown"),
    limit = (n) => n * GUEST.share,
    refuse = () => {
      throw coded(
        "guest_room",
        "the rest of this document is kept for its owners",
      );
    };
  if (
    text.length > before.length &&
    text.length * 3 > limit(LIMITS.text) &&
    utf8Bytes(text.toString()) > limit(LIMITS.text)
  )
    refuse();
  const structs = structureCount(doc);
  if (structs > before.structs && structs > limit(LIMITS.structs)) refuse();
  if ((doc._flatsStateSize || 0) > limit(LIMITS.state)) {
    validateState(doc);
    if (doc._flatsStateSize > limit(LIMITS.state)) refuse();
  }
}
// commitUpdate applies one update in its own transaction. check runs before
// the update applies and returns what runs after it was appended.
function commitUpdate(c, m, env, bytes, check, guest = false) {
  const db = env.DB,
    room = c.room;
  let result;
  try {
    result = transaction(db, () => {
      // An update in flight when the level drops never commits (TECH 3.1).
      if (guest) {
        const access = revisions.accessOf(db);
        if (access.level !== "edit" || access.epoch !== c.epoch)
          throw coded("editing_ended", "guest editing ended");
      }
      revisions.ensureFlat(db, content, c.ws.runtimeGeneration);
      // A newer version without this document would never record the edit.
      if (revisions.removedByNewer(db, c.ws.runtimeGeneration, room.path))
        throw Object.assign(new Error("this document was removed"), {
          code: "readonly",
        });
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
      const after = check(db, s);
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
      // Pending data would be encoded into welcomes and checkpoints without
      // ever being visible (TECH 3.7).
      if (hasPending(s.doc))
        throw coded("invalid_update", "this change depends on missing changes");
      const size = validateIncremental(s.doc, bytes.length);
      roomBudget(room, { length: size }, s.doc, c.kind);
      const event = append(db, s, m.id, m.u, guest);
      compact(db, s);
      after();
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
  committed(room, result.s);
  if (result.event) broadcast(room, result.event, c);
  send(c, { t: "ack", id: m.id, seq: result.ack.seq, chain: result.ack.chain });
}
// catchUp replays committed rows into the room and broadcasts them.
function catchUp(room, env, generation) {
  let s;
  try {
    s = current(
      env.DB,
      sourceFor(room.path),
      content.hash,
      room.row ? room : undefined,
      generation,
      ensureFlat(generation),
    );
    roomBudget(room, { length: s.doc._flatsStateSize }, s.doc);
  } catch (e) {
    if (!rejectionCode(e)) drop(room);
    else {
      room.doc?.destroy();
      room.doc = null;
    }
    throw e;
  }
  committed(room, s);
  return s;
}
// Every page sends a heartbeat about every 15 seconds; it also reconciles
// the level in case a signal was missed.
function ping(c, env) {
  reconcileAccess(env);
  if (c.closed) return;
  const s = catchUp(c.room, env, c.ws.runtimeGeneration);
  send(c, {
    t: "pong",
    epoch: s.row.epoch,
    seq: s.row.seq,
    chain: s.row.chain,
  });
}
function html(d, readonly, url, title) {
  const host = url.host;
  if (!/^[a-zA-Z0-9.:[\]-]+$/.test(host))
    return new Response("Invalid host", { status: 400 });
  const csp = `default-src 'self'; script-src 'self'; connect-src 'self' ws://${host} wss://${host}; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`;
  return new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${escapeHTML(title)} · Flats</title><link rel="stylesheet" href="/_docs/assets/client.css"><script type="module" src="/_docs/assets/client.js"></script></head><body data-doc="${escapeHTML(d.path)}" data-readonly="${readonly}"><header><h1>${escapeHTML(title)}</h1>${readonly ? "" : '<div id="modes" role="group" aria-label="Document mode"><button type="button" data-mode="edit" aria-pressed="true">Edit</button><button type="button" data-mode="view" aria-pressed="false">View</button></div><button id="history-toggle" type="button" aria-expanded="false" aria-controls="history" hidden>History</button>'}<span id="status" role="status" aria-live="polite">Connecting…</span><div id="presence" aria-label="People in this document"></div></header><div id="notice" role="alert" hidden></div><div class="workspace"><nav id="documents" aria-label="Documents" hidden></nav><main id="page"><div class="surface">${readonly ? "" : '<section id="revision-view" tabindex="-1" aria-label="Revision" hidden><div id="revision-banner"></div><div id="revision-body"></div></section>'}<section id="editor" aria-label="Document editor" hidden></section><article id="content" aria-label="Document"></article><footer class="powered">Powered by <a href="https://github.com/gosuda/flats" target="_blank" rel="noopener noreferrer">Flats</a></footer></div></main>${readonly ? "" : '<aside id="history" aria-label="Revision history" hidden><div class="history-head"><h2>History</h2><button id="history-close" type="button" aria-label="Close history">Close</button></div><p id="history-note" role="status" hidden></p><ol id="history-list"></ol><button id="history-more" type="button" hidden>Load more</button></aside>'}</div></body></html>`,
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
// One page of the block outline; a large document needs several reads.
function blockPage(markdown, raw) {
  const offset = Number(raw || 0),
    page = outline(
      markdown,
      digest,
      Number.isSafeInteger(offset) && offset >= 0 ? offset : 0,
    );
  return {
    blocks: page.blocks,
    blocks_total: page.total,
    block_offset: Number.isSafeInteger(offset) && offset >= 0 ? offset : 0,
  };
}
const json = (status, body) =>
  Response.json(body, { status, headers: { "cache-control": "no-store" } });
const editStatus = {
  invalid: 400,
  edit_conflict: 409,
  capacity: 422,
  disk_quota: 507,
};
// A host management edit: guarded operations resolved against committed
// text and appended as one Yjs update in the same transaction as human edits.
async function edit(request, env, d) {
  if (
    request.headers.get("x-flats-access") !== "private" ||
    request.headers.get("x-flats-host-op") !== "edit"
  )
    return json(403, {
      code: "readonly",
      message: "live edits need the host management channel",
    });
  const raw = await request.text();
  if (raw.length > EDIT_LIMITS.body || utf8Bytes(raw) > EDIT_LIMITS.body)
    return json(413, {
      code: "capacity",
      message: `edit request larger than ${EDIT_LIMITS.body} bytes; split it`,
    });
  let body;
  try {
    body = JSON.parse(raw);
  } catch {
    return json(400, { code: "invalid", message: "body must be JSON" });
  }
  // The host chooses the receipt id so it can look the edit up when the
  // response is lost.
  const id = body?.id === undefined ? "agent:" + crypto.randomUUID() : body.id,
    label = typeof body?.label === "string" ? body.label.slice(0, 200) : "";
  if (typeof id !== "string" || !/^agent:[A-Za-z0-9_-]{8,120}$/.test(id))
    return json(400, { code: "invalid", message: "invalid edit id" });
  // A host commit deadline (epoch ms): after it the edit rolls back instead
  // of committing, so the host can settle a lost response conclusively.
  const deadline = body?.deadline_ms;
  if (
    deadline !== undefined &&
    (!Number.isSafeInteger(deadline) || deadline < 0)
  )
    return json(400, { code: "invalid", message: "invalid deadline" });
  if (body && typeof body === "object") {
    delete body.id;
    delete body.deadline_ms;
    delete body.label;
  }
  const db = env.DB;
  let result;
  try {
    schema(db);
    result = transaction(db, () => {
      const applied = db.query(
        "SELECT seq FROM flats_docs_receipts WHERE doc=? AND id=?",
        [d.path, id],
      )[0];
      if (applied)
        throw Object.assign(
          new Error(`this edit was already applied at seq ${applied.seq}`),
          { code: "edit_conflict" },
        );
      const generation = request.runtimeGeneration;
      revisions.ensureFlat(db, content, generation);
      if (revisions.stale(db, generation))
        throw Object.assign(new Error("obsolete worker"), { code: "deadline" });
      const shape = {
        source: content.hash,
        paths: content.documents.map((x) => x.path),
      };
      // People's unrecorded edits become their own revision first.
      if (revisions.textDirty(db))
        revisions.record(db, { kind: "person", ...shape });
      db.exec("DELETE FROM flats_docs_groups");
      const s = activate(
        db,
        d,
        content.hash,
        undefined,
        request.runtimeGeneration,
      );
      try {
        const text = s.doc.getText("markdown"),
          before = s.row.seq,
          original = text.toString(),
          plan = planEdit(original, body, digest);
        // Operations that cancel out leave nothing to commit.
        if (plan.text === original) plan.splices = [];
        if (plan.splices.length) {
          const sv = Y.encodeStateVector(s.doc);
          s.doc.transact(() => {
            for (const p of plan.splices) {
              if (p.remove) text.delete(p.at, p.remove);
              if (p.insert) text.insert(p.at, p.insert);
            }
          }, "agent");
          if (text.toString() !== plan.text)
            throw new Error("edit replay mismatch");
          const bytes = Y.encodeStateAsUpdate(s.doc, sv);
          if (bytes.length > LIMITS.update)
            throw Object.assign(
              new Error(
                `edit update is ${bytes.length} bytes, above ${LIMITS.update}; split it into smaller calls`,
              ),
              { code: "capacity" },
            );
          try {
            validateUpdate(bytes);
          } catch (e) {
            // Structure and delete-range bounds: smaller edits fit.
            e.code = "capacity";
            throw e;
          }
          validateIncremental(s.doc, bytes.length);
          append(db, s, id, base64(bytes));
          compact(db, s);
          revisions.record(db, {
            kind: "agent",
            ...shape,
            authors: label ? [label] : [],
            known: { [d.path]: plan.text },
            op: id,
          });
        }
        revisions.dropUnused(db);
        if (plan.splices.length) revisions.checkDiskQuota(db);
        // Checked last, immediately before COMMIT.
        if (
          plan.splices.length &&
          deadline !== undefined &&
          Date.now() > deadline
        )
          throw Object.assign(new Error("edit deadline passed"), {
            code: "deadline",
          });
        return {
          format: 1,
          doc: d.path,
          seq_before: before,
          seq: s.row.seq,
          chain: s.row.chain,
          hash: digest(plan.text),
          bytes: utf8Bytes(plan.text),
          changed: plan.splices.length > 0,
          ...(plan.splices.length ? { id } : {}),
          ops: plan.summary,
        };
      } finally {
        s.doc.destroy();
      }
    });
  } catch (e) {
    if (
      e?.name === "InternalError" ||
      /out of memory/i.test(String(e?.message))
    )
      throw e;
    const code = rejectionCode(e);
    if (code && Object.hasOwn(editStatus, code))
      return json(editStatus[code], {
        code,
        message: String(e.message),
        ...(e.op === undefined ? {} : { op: e.op }),
      });
    console.warn("docs edit failed:", String(e?.message));
    return json(503, {
      code: "unavailable",
      message: "edit not applied; retry",
    });
  }
  // Connected editors catch up now rather than at their next heartbeat.
  if (result.changed) globalThis.__flats_docsWake?.(d.path);
  return json(200, result);
}
// Host only: revision capture and its outbox (docs/spec/document-revisions-TECH.md).
const revisionStatus = {
  invalid: 400,
  conflict: 409,
  not_found: 404,
  unavailable: 503,
};
async function revisionOp(request, env, url) {
  if (
    request.headers.get("x-flats-access") !== "private" ||
    request.headers.get("x-flats-host-op") !== "revisions"
  )
    return json(403, { code: "readonly", message: "host only" });
  const op = url.pathname.slice("/_docs/host/revisions/".length),
    db = env.DB,
    generation = request.runtimeGeneration,
    method = request.method;
  let body;
  if (method === "POST") {
    const raw = await request.text();
    // One text of at most 1 MiB, JSON-escaped (up to six characters each);
    // the runtime already bounds the request to 10 MiB.
    if (raw.length > 8 * 1024 * 1024)
      return json(413, { code: "invalid", message: "body too large" });
    try {
      body = JSON.parse(raw || "{}");
    } catch {
      return json(400, { code: "invalid", message: "body must be JSON" });
    }
  }
  try {
    schema(db);
    let result;
    if (op === "sync" && method === "POST")
      result = transaction(db, () => revisions.sync(db, content, generation));
    else if (op === "access" && method === "POST") {
      result = transaction(db, () =>
        revisions.setAccess(db, content, generation, body),
      );
      // The WebSocket VM closes guest sockets and tells every page.
      globalThis.__flats_docsSignal?.("access");
    } else if (op === "safe_point" && method === "POST")
      result = transaction(db, () =>
        revisions.safePoint(db, content, generation, body?.prev),
      );
    else if (op === "checkpoint" && method === "POST") {
      // Outside any transaction: copy the log into the database and
      // truncate it, so the host measures a small log.
      const row = db.query("PRAGMA wal_checkpoint(TRUNCATE)")[0];
      result = { busy: !!row?.busy };
    } else if (op === "budget/revoke" && method === "POST") {
      transaction(db, () => revisions.revokeBudget(db));
      result = { ok: true };
    } else if (op === "budget" && method === "POST") {
      transaction(db, () => revisions.setBudget(db, body));
      result = { ok: true };
    } else if (op === "outbox" && method === "GET")
      result = revisions.outbox(db, Number(url.searchParams.get("after") || 0));
    else if (op === "text" && method === "GET") {
      const text = revisions.textOf(db, url.searchParams.get("hash") || "");
      if (text === undefined)
        return json(404, { code: "not_found", message: "no such text" });
      result = { hash: url.searchParams.get("hash"), text };
    } else if (op === "ack" && method === "POST") {
      transaction(db, () =>
        revisions.ack(db, body.store, body.through, body.budget),
      );
      result = { ok: true };
    } else if (op === "stage" && method === "POST")
      result = { hash: transaction(db, () => revisions.stage(db, body.text)) };
    else if (op === "intent" && method === "POST") {
      // An intent can be written while this version's own activation
      // fails: a rollback is how such a flat recovers.
      transaction(db, () => {
        revisions.tryEnsureFlat(db, content, generation);
        revisions.setIntent(db, body);
      });
      result = { ok: true };
    } else if (op === "restore" && method === "POST") {
      const deadline = body?.deadline_ms;
      if (
        deadline !== undefined &&
        (!Number.isSafeInteger(deadline) || deadline < 0)
      )
        return json(400, { code: "invalid", message: "invalid deadline" });
      result = transaction(db, () => {
        const out = revisions.restore(db, content, generation, body);
        if (deadline !== undefined && Date.now() > deadline)
          throw Object.assign(new Error("restore deadline passed"), {
            code: "unavailable",
          });
        return out;
      });
      for (const d of content.documents) globalThis.__flats_docsWake?.(d.path);
    } else if (op === "op" && method === "GET")
      result = revisions.opStatus(db, url.searchParams.get("id") || "");
    else
      return json(404, {
        code: "not_found",
        message: "unknown revision operation",
      });
    return json(200, result);
  } catch (e) {
    if (
      e?.name === "InternalError" ||
      /out of memory/i.test(String(e?.message))
    )
      throw e;
    const code = rejectionCode(e);
    if (code && Object.hasOwn(revisionStatus, code))
      return json(revisionStatus[code], { code, message: String(e.message) });
    if (code === "capacity")
      return json(422, { code, message: String(e.message) });
    console.warn("docs revision operation failed:", op, String(e?.message));
    return json(503, {
      code: "unavailable",
      message: String(e?.message || "failed"),
    });
  }
}
// Only gzip and identity are available. Explicit coding preferences override
// the wildcard; an absent/empty field retains the uncompressed response.
function mermaidEncoding(header) {
  const qualities = new Map();
  for (const entry of (header || "").split(",")) {
    const [coding, ...parameters] = entry.trim().toLowerCase().split(";");
    if (!coding) continue;
    let quality = 1;
    for (const parameter of parameters) {
      // Extensions do not alter a coding's quality; malformed q values do.
      if (!/^\s*q\s*(?:=|$)/.test(parameter)) continue;
      const match = /^\s*q\s*=\s*(0(?:\.\d{0,3})?|1(?:\.0{0,3})?)\s*$/.exec(
        parameter,
      );
      quality = match ? Number(match[1]) : 0;
    }
    qualities.set(coding.trim(), quality);
  }
  const gzip = qualities.get("gzip") ?? qualities.get("*") ?? 0,
    identity = qualities.get("identity") ?? (qualities.get("*") === 0 ? 0 : 1);
  return gzip > 0 && gzip >= identity
    ? "gzip"
    : identity > 0
      ? "identity"
      : null;
}
export default {
  fetch(request, env) {
    const url = new URL(request.url),
      path = url.pathname;
    // Host only: whether a live edit with this receipt id committed.
    if (path === "/_docs/api/receipt" && request.method === "GET") {
      const d = sourceFor(url.searchParams.get("doc") || content.entry);
      if (
        request.headers.get("x-flats-access") !== "private" ||
        request.headers.get("x-flats-host-op") !== "edit"
      )
        return json(403, { code: "readonly", message: "host only" });
      if (!d)
        return json(404, { code: "not_found", message: "unknown document" });
      let row;
      try {
        row = env.DB.query(
          "SELECT seq,chain FROM flats_docs_receipts WHERE doc=? AND id=?",
          [d.path, url.searchParams.get("id") || ""],
        )[0];
      } catch (e) {
        // A database without the docs schema holds no receipts.
        if (!String(e?.message).includes("no such table")) throw e;
      }
      return row
        ? json(200, { committed: true, seq: row.seq, chain: row.chain })
        : json(200, { committed: false });
    }
    if (path.startsWith("/_docs/host/revisions/"))
      return revisionOp(request, env, url);
    if (path === "/_docs/api/edit" && request.method === "POST") {
      const d = sourceFor(url.searchParams.get("doc") || content.entry);
      if (!d)
        return json(404, { code: "not_found", message: "unknown document" });
      return edit(request, env, d);
    }
    if (request.method !== "GET" && request.method !== "HEAD")
      return new Response("Method not allowed", { status: 405 });
    if (path === "/_docs/healthz") {
      if (request.headers.get("x-flats-health") !== "1")
        return new Response("ok");
      schema(env.DB);
      transaction(env.DB, () => {
        // A flat activation that ran has activated every document of this
        // version in this transaction; loading each again would only repeat
        // the work.
        if (revisions.ensureFlat(env.DB, content, request.runtimeGeneration))
          return;
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
    // Mermaid is a large separate module, loaded only by the worker that
    // serves it. Its path carries its content hash (FLATS_MERMAID_URL, set by
    // build.mjs), so browsers keep it.
    if (path === FLATS_MERMAID_URL) {
      const encoding = mermaidEncoding(request.headers.get("accept-encoding")),
        headers = {
          "content-type": "text/javascript; charset=utf-8",
          "x-content-type-options": "nosniff",
          "cache-control": "public, max-age=31536000, immutable",
          vary: "Accept-Encoding",
        };
      if (!encoding)
        return new Response(null, {
          status: 406,
          headers: { vary: "Accept-Encoding", "cache-control": "no-store" },
        });
      if (encoding === "gzip") headers["content-encoding"] = "gzip";
      if (request.method === "HEAD") return new Response(null, { headers });
      return import("./mermaid-asset.js").then(
        (m) =>
          new Response(
            encoding === "gzip" ? unbase64(m.gzip, 1536 * 1024) : m.default,
            { headers },
          ),
      );
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
          title:
            request.headers.get("x-flats-access") === "private"
              ? titleFor(d)
              : publicTitle(d),
        })),
      });
    let docpath;
    if (path === "/_docs/api/document")
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
    if (!d || (path.startsWith("/_docs/") && path !== "/_docs/api/document"))
      return new Response("Unknown document", { status: 404 });
    const privateAccess = request.headers.get("x-flats-access") === "private";
    if (path !== "/_docs/api/document") {
      if (privateAccess) return html(d, false, url, titleFor(d));
      // Without the host's runtime generation (a bare runtime) the page is
      // named without reading the document.
      if (
        !Number.isSafeInteger(request.runtimeGeneration) ||
        request.runtimeGeneration < 1
      )
        return html(d, true, url, publicTitle(d));
      const live = current(
        env.DB,
        d,
        content.hash,
        undefined,
        request.runtimeGeneration,
        ensureFlat(request.runtimeGeneration),
      );
      const markdown = live.doc.getText("markdown").toString();
      live.doc.destroy();
      return html(d, true, url, publicTitle(d, markdown));
    }
    const s = current(
      env.DB,
      d,
      content.hash,
      undefined,
      request.runtimeGeneration,
      ensureFlat(request.runtimeGeneration),
    );
    const markdown = s.doc.getText("markdown").toString();
    let guest = {};
    if (privateAccess) {
      // Which lines hold guest text, for agents (TECH 2.8).
      const owned = revisions.ownedClients(env.DB, d.path);
      guest = {
        guest_text: owned.size > 0,
        guest_lines: guestLines(s.doc, owned),
      };
    }
    const result = {
      format: 1,
      doc: d.path,
      markdown,
      hash: digest(markdown),
      epoch: s.row.epoch,
      seq: s.row.seq,
      chain: s.row.chain,
      source: "live",
      ...guest,
      ...(privateAccess && url.searchParams.get("blocks") === "1"
        ? blockPage(markdown, url.searchParams.get("block_offset"))
        : {}),
    };
    s.doc.destroy();
    return Response.json(result);
  },
  websocket: {
    // Guests and viewers use separate runtime pools (internal/runtime/ws.go).
    publicPools: true,
    open(ws) {
      const member = ws.headers.get("x-flats-access") === "private";
      ws.setSendLimits(
        LIMITS.sendBytes,
        member ? LIMITS.flatSendBytes : LIMITS.publicSendBytes,
      );
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
      // A Public socket becomes a guest or a viewer at its hello, which
      // checks that kind's pool.
      if (member && poolFull(MEMBER, path)) {
        ws.close(1008, "connection capacity");
        return;
      }
      const now = Date.now(),
        c = {
          ws,
          path,
          kind: member ? MEMBER : null,
          public: !member,
          readonly: !member,
          serial: 0,
          sent: new Map(),
          bytes: 0,
          tokens: 120,
          input: 1024 * 1024,
          last: now,
          awTokens: 20,
          awLast: now,
        };
      if (member) attach(c);
      connections.set(ws.id, c);
    },
    message(ws, data, env) {
      const c = connections.get(ws.id);
      if (!c || c.closed || c.diverged) return;
      try {
        const m = parseFrame(data),
          size = utf8Bytes(data);
        // Bytes read from guest sockets count against all guests' budget,
        // refused frames too.
        if (c.kind === GUEST_KIND) c.overBytes = !guestLimits.bytes.count(size);
        rate(c, size, m.t);
        if (m.t === "received") {
          received(c, m.n);
          return;
        }
        if (m.t === "hello") {
          if (c.public) publicHello(c, m, env);
          else joined(c, m, env, {});
          return;
        }
        if (!c.joined) throw new Error("hello required");
        if (m.t === "update") {
          if (c.kind === GUEST_KIND) guestUpdate(c, m, env, size);
          else update(c, m, env, size);
        } else if (m.t === "aw") presence(c, m, env);
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
        if (!c.closed) error(c, code);
        console.warn("docs rejected:", String(e.message));
      } finally {
        settle(env);
      }
    },
    // Host-internal: another VM of this worker committed an agent edit.
    wake(doc, env) {
      const room = rooms.get(doc);
      if (!room?.row || !room.peers.size) return;
      const peers = [...room.peers];
      try {
        catchUp(room, env, peers[0].ws.runtimeGeneration);
      } catch (e) {
        if (
          e?.name === "InternalError" ||
          /out of memory/i.test(String(e?.message))
        )
          throw e;
        console.warn("docs wake failed:", String(e?.message));
      }
    },
    // Host-internal: the access operation stored a new level.
    signal(name, env) {
      if (name === "access") reconcileAccess(env);
      settle(env);
    },
    close(ws, env) {
      const c = connections.get(ws.id);
      if (!c) return;
      connections.delete(ws.id);
      charge(c, -c.bytes);
      if (c.viewer && !--c.viewer.open) c.viewer.released = Date.now();
      const room = c.room;
      if (room) {
        room.peers.delete(c);
        if (c.aw) {
          const gone = { ...c.aw, state: null };
          relayPresence(room, gone, gone);
        }
        if (c.kind === VIEWER) room.viewersDue = true;
        if (!room.peers.size) {
          if (rooms.get(room.path) === room) rooms.delete(room.path);
          room.doc?.destroy();
        }
      }
      settle(env);
    },
  },
};
// settle sends what waits for the end of an event: changed viewer lists
// and the presence counts core reports.
function settle(env) {
  flushViewers();
  if (env) flushPresence(env);
}
