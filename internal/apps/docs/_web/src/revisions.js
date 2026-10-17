// Revision capture: every revision boundary records the whole flat in an
// outbox, in the same transaction as the change it precedes or follows. The
// host imports the outbox into its revision store and acknowledges it.
// See docs/spec/document-revisions-TECH.md.
import * as Y from "yjs";
import { base64, utf8Bytes, LIMITS } from "./protocol.js";
import { applyText } from "./merge.js";
import {
  load,
  activate,
  append,
  compact,
  digest,
  validateIncremental,
  setSeed,
  WAL_REST_FRAMES,
} from "./storage.js";

export const GROUP_IDLE = 30 * 1000;
export const GROUP_MAX_AGE = 5 * 60 * 1000;
const STAGED_TTL = 60 * 60 * 1000;
const PAGE_BYTES = 4 * 1024 * 1024;
const MAX_AUTHORS = 32;

export function getMeta(db, key) {
  const row = db.query("SELECT value FROM flats_docs_meta WHERE key=?", [
    key,
  ])[0];
  return row ? JSON.parse(row.value) : null;
}
function setMeta(db, key, value) {
  if (value === null) db.exec("DELETE FROM flats_docs_meta WHERE key=?", [key]);
  else {
    const json = JSON.stringify(value);
    db.exec("INSERT OR REPLACE INTO flats_docs_meta VALUES(?,?)", [key, json]);
  }
}

// --- texts and their references ---

// A text's reference count and staging time live in flats_docs_text_refs,
// apart from its row in flats_docs_texts, so changing them never rewrites
// a large text (storage.js migrate). Every text row has a refs row.
function refsOf(db, hash) {
  return db.query("SELECT refs FROM flats_docs_text_refs WHERE hash=?", [
    hash,
  ])[0];
}
function insertText(db, hash, text, refs, stagedAt) {
  db.exec("INSERT INTO flats_docs_texts VALUES(?,?,0,NULL)", [hash, text]);
  db.exec("INSERT INTO flats_docs_text_refs VALUES(?,?,?)", [
    hash,
    refs,
    stagedAt,
  ]);
}
// addRef adds a reference, storing the text when it is new.
function addRef(db, hash, text) {
  if (refsOf(db, hash)) {
    db.exec("UPDATE flats_docs_text_refs SET refs=refs+1 WHERE hash=?", [hash]);
    return;
  }
  if (typeof text !== "string") throw new Error("revision text is missing");
  insertText(db, hash, text, 1, null);
}
// release drops one reference. The text stays until dropUnused at the end
// of the operation: a later document of the same operation may reference
// it again.
function release(db, hash) {
  db.exec("UPDATE flats_docs_text_refs SET refs=refs-1 WHERE hash=?", [hash]);
}
function textOf(db, hash) {
  return db.query("SELECT text FROM flats_docs_texts WHERE hash=?", [hash])[0]
    ?.text;
}

// --- documents ---

function position(db, path) {
  return db.query("SELECT epoch,seq FROM flats_docs_documents WHERE doc=?", [
    path,
  ])[0];
}
function committedText(db, path) {
  const s = load(db, path);
  if (!s) return null;
  try {
    return s.doc.getText("markdown").toString();
  } finally {
    s.doc.destroy();
  }
}
// The recorded file of a document, loading its committed text only when it
// moved since it was last recorded. known maps paths to their committed text
// when the caller already has it, or to {hash, size} of a text it stored.
function documentFile(db, path, known) {
  const pos = position(db, path);
  if (!pos) return null;
  const rec = db.query("SELECT * FROM flats_docs_recorded WHERE doc=?", [
    path,
  ])[0];
  if (rec && rec.epoch === pos.epoch && rec.seq === pos.seq)
    return { path, hash: rec.hash, size: rec.bytes };
  const given = known && Object.hasOwn(known, path) ? known[path] : undefined;
  let hash, size;
  if (given && typeof given === "object") {
    ({ hash, size } = given);
    addRef(db, hash);
  } else {
    const text = given ?? committedText(db, path);
    hash = digest(text);
    size = utf8Bytes(text);
    addRef(db, hash, text);
  }
  if (rec) release(db, rec.hash);
  db.exec("INSERT OR REPLACE INTO flats_docs_recorded VALUES(?,?,?,?,?)", [
    path,
    pos.epoch,
    pos.seq,
    hash,
    size,
  ]);
  return { path, hash, size };
}

// textDirty reports whether a document of the newest entry moved since.
export function textDirty(db) {
  const newest = getMeta(db, "newest");
  if (!newest) return false;
  for (const d of newest.documents) {
    const pos = position(db, d.path),
      rec = db.query("SELECT epoch,seq FROM flats_docs_recorded WHERE doc=?", [
        d.path,
      ])[0];
    if (!pos) continue;
    if (!rec || rec.epoch !== pos.epoch || rec.seq !== pos.seq) return true;
  }
  return false;
}

// --- groups ---

export function touchGroup(db, editor, label, now = Date.now()) {
  db.exec(
    "INSERT INTO flats_docs_groups VALUES(?,?,?,?,1) ON CONFLICT(editor) DO UPDATE SET label=excluded.label,last_at=excluded.last_at,dirty=1",
    [editor, String(label).slice(0, 200), now, now],
  );
}
function dirtyLabels(db) {
  return db
    .query(
      "SELECT label FROM flats_docs_groups WHERE dirty=1 ORDER BY started_at,editor",
    )
    .map((g) => g.label);
}

// --- recording ---

const sameDocuments = (a, b) =>
  a.length === b.length &&
  a.every((d, i) => d.path === b[i].path && d.hash === b[i].hash);

// record appends one outbox entry, unless forced is false and nothing
// changed since the newest entry. It returns the entry's seq, or 0.
export function record(db, o) {
  const overrides = o.overrides || {},
    paths = [...new Set([...o.paths, ...Object.keys(overrides)])].sort(),
    documents = [],
    texts = {};
  for (const path of paths) {
    if (Object.hasOwn(overrides, path)) {
      const text = overrides[path];
      if (typeof text === "object") {
        // Already stored (see store): its row exists.
        documents.push({ path, hash: text.hash, size: text.size });
        continue;
      }
      const d = { path, hash: digest(text), size: utf8Bytes(text) };
      texts[d.hash] = text;
      documents.push(d);
      continue;
    }
    const d = documentFile(db, path, o.known);
    if (d) documents.push(d);
  }
  const newest = getMeta(db, "newest");
  if (
    !o.force &&
    newest &&
    newest.source === o.source &&
    sameDocuments(newest.documents, documents)
  ) {
    // Edits that ended unchanged need no revision, so nothing stays reserved.
    clearReservations(db);
    return 0;
  }
  const authors = (o.authors ?? dirtyLabels(db))
    .filter((a) => typeof a === "string" && a)
    .map((a) => a.slice(0, 200))
    .slice(0, MAX_AUTHORS);
  db.exec(
    "INSERT INTO flats_docs_outbox(created_at,kind,authors,message,rollback_of,op,source,documents) VALUES(?,?,?,?,?,?,?,?)",
    [
      Date.now(),
      o.kind,
      JSON.stringify(authors),
      String(o.message || "").slice(0, 500),
      o.rollbackOf ?? null,
      o.op ?? null,
      o.source,
      JSON.stringify(documents),
    ],
  );
  const seq = db.query("SELECT last_insert_rowid() AS seq")[0].seq;
  for (const d of documents) addRef(db, d.hash, texts[d.hash]);
  // The host stores every text the previous entry did not list as a new
  // blob when it imports this one; until it reports that blob stored, the
  // text counts toward the disk quota (see checkDiskQuota). The documents
  // are recorded now, so their pending reservation ends.
  const listed = new Set((newest?.documents || []).map((d) => d.hash));
  for (const d of documents)
    if (!listed.has(d.hash))
      db.exec("INSERT OR IGNORE INTO flats_docs_unimported VALUES(?,?)", [
        d.hash,
        d.size,
      ]);
  clearReservations(db);
  setMeta(db, "newest", { source: o.source, documents });
  db.exec("UPDATE flats_docs_groups SET dirty=0");
  if (o.op && o.kind === "rollback")
    db.exec("INSERT OR REPLACE INTO flats_docs_ops VALUES(?,?)", [o.op, seq]);
  return seq;
}

// store keeps a text for an entry recorded later in the same transaction,
// so the caller need not hold it meanwhile.
function store(db, text) {
  const hash = digest(text);
  const size = utf8Bytes(text);
  if (!refsOf(db, hash)) insertText(db, hash, text, 0, null);
  return { hash, size };
}
// dropUnused deletes the texts nothing references any more, except staged
// ones. Every operation that releases or stores texts ends with it.
export function dropUnused(db) {
  deleteTexts(db, "refs<=0 AND staged_at IS NULL");
}
// deleteTexts deletes the texts whose refs rows match where, with those rows.
function deleteTexts(db, where, params = []) {
  db.exec(
    `DELETE FROM flats_docs_texts WHERE hash IN (SELECT hash FROM flats_docs_text_refs WHERE ${where})`,
    params,
  );
  db.exec(`DELETE FROM flats_docs_text_refs WHERE ${where}`, params);
}

const currentShape = (content) => ({
  source: content.hash,
  paths: content.documents.map((d) => d.path),
});
function beforeShape(db) {
  const newest = getMeta(db, "newest");
  return { source: newest.source, paths: newest.documents.map((d) => d.path) };
}

// recordPeople keeps people's unrecorded edits as one person entry and
// deletes every group.
function recordPeople(db, shape) {
  if (textDirty(db)) record(db, { kind: "person", ...shape });
  db.exec("DELETE FROM flats_docs_groups");
}

// --- flat activation ---

function highestGeneration(db) {
  return db.query(
    "SELECT coalesce(max(version),0) AS version FROM flats_docs_activations",
  )[0].version;
}
// stale reports whether a newer worker activated this flat.
export function stale(db, generation) {
  const active = getMeta(db, "active");
  return !!active && generation < active.generation;
}
// removedByNewer reports a document that a newer active version no longer
// has: writes to it would never reach a revision.
export function removedByNewer(db, generation, path) {
  const active = getMeta(db, "active");
  return (
    !!active && generation < active.generation && !active.docs.includes(path)
  );
}

function initialize(db, content, generation) {
  setMeta(db, "store", crypto.randomUUID());
  // A document that already had text starts its history here; a new one is
  // simply created.
  const existing =
    db.query("SELECT count(*) AS n FROM flats_docs_documents")[0].n > 0;
  for (const d of content.documents)
    if (!position(db, d.path))
      activate(db, d, content.hash, undefined, generation).doc.destroy();
  const shape = currentShape(content);
  const rows = db.query(
    "SELECT doc,generation FROM flats_docs_conflicts ORDER BY generation,doc",
  );
  for (const row of rows) {
    const markdown = db.query(
      "SELECT markdown FROM flats_docs_conflicts WHERE doc=? AND generation=?",
      [row.doc, row.generation],
    )[0].markdown;
    if (utf8Bytes(markdown) > LIMITS.text) {
      console.warn("skipped oversized recovery text of", row.doc);
      continue;
    }
    record(db, {
      kind: "recovered",
      message: "migrated recovery text",
      ...shape,
      overrides: { [row.doc]: markdown },
      force: true,
      authors: [],
    });
  }
  record(db, {
    kind: "created",
    message: existing ? "revision history started" : "",
    ...shape,
    force: true,
    authors: [],
  });
  db.exec("DELETE FROM flats_docs_conflicts");
}

// releaseIntent drops the intent and the references it holds.
function releaseIntent(db, intent) {
  for (const d of intent.documents || []) release(db, d.hash);
  setMeta(db, "intent", null);
}

// ensureFlat runs the flat activation of this worker's version when it has
// not run, inside the caller's transaction. It returns true when it ran.
export function ensureFlat(db, content, generation) {
  if (!Number.isSafeInteger(generation) || generation < 1)
    throw new Error("host runtime generation unavailable");
  const active = getMeta(db, "active");
  if (active?.hash === content.hash) return false;
  if (generation < highestGeneration(db) || stale(db, generation)) return false;
  if (!getMeta(db, "store")) initialize(db, content, generation);
  const intent = getMeta(db, "intent"),
    mine = intent && intent.source === content.hash ? intent : null;
  const shape = currentShape(content);
  if (mine?.kind === "rollback") {
    recordPeople(db, beforeShape(db));
    const target = new Map(mine.documents.map((d) => [d.path, d.hash]));
    for (const d of content.documents) {
      const text = textOf(db, target.get(d.path));
      if (typeof text !== "string")
        throw new Error("rollback text of " + d.path + " is missing");
      restoreText(db, content, d, text, mine.id, generation);
    }
    record(db, {
      kind: "rollback",
      message: mine.message || "",
      ...shape,
      force: true,
      authors: [mine.label],
      rollbackOf: mine.rollback_of,
      op: mine.id,
    });
  } else {
    // People's text a merge may discard is kept first, as the flat was
    // before this version. The activation loads every document anyway, so
    // both entries use the texts it has instead of loading them again. Each
    // text is stored as soon as it is known and only its hash is kept: a
    // flat of 128 large documents does not fit the worker's memory twice.
    const dirty = textDirty(db),
      prior = dirty ? beforeShape(db) : null,
      wanted = new Set(prior?.paths),
      known = {},
      before = {};
    for (const d of content.documents) {
      const s = activate(db, d, content.hash, undefined, generation);
      const text = s.doc.getText("markdown").toString();
      s.doc.destroy();
      known[d.path] = store(db, text);
      if (wanted.has(d.path))
        before[d.path] =
          s.before === undefined ? known[d.path] : store(db, s.before);
    }
    if (dirty)
      record(db, {
        kind: "person",
        source: prior.source,
        paths: prior.paths.filter((p) => !Object.hasOwn(before, p)),
        overrides: before,
      });
    db.exec("DELETE FROM flats_docs_groups");
    record(db, {
      kind: "agent",
      message: mine?.message || "",
      ...shape,
      authors: mine?.label ? [mine.label] : [],
      op: mine?.id,
      known,
    });
  }
  if (intent) releaseIntent(db, intent);
  finishActivation(db, content, generation);
  dropUnused(db);
  return true;
}

// finishActivation marks this version as the flat's active one.
function finishActivation(db, content, generation) {
  db.exec("DELETE FROM flats_docs_groups");
  // Documents this version no longer has stop holding their recorded text:
  // the entries above hold what they last contained.
  const paths = new Set(content.documents.map((d) => d.path));
  for (const row of db.query("SELECT doc,hash FROM flats_docs_recorded"))
    if (!paths.has(row.doc)) {
      db.exec("DELETE FROM flats_docs_recorded WHERE doc=?", [row.doc]);
      release(db, row.hash);
    }
  setMeta(db, "active", {
    hash: content.hash,
    generation,
    docs: [...paths],
  });
}

// --- restoring text ---

function resetDocument(db, content, source, text, generation) {
  for (const table of [
    "flats_docs_updates",
    "flats_docs_checkpoints",
    "flats_docs_receipts",
    "flats_docs_activations",
    "flats_docs_documents",
  ])
    db.exec(`DELETE FROM ${table} WHERE doc=?`, [source.path]);
  activate(
    db,
    { path: source.path, hash: source.hash, text },
    content.hash,
    undefined,
    generation,
  ).doc.destroy();
}

// restoreText makes a document's committed text exactly text and its merge
// base text, as one update; a document whose history cannot take that
// update is reset to a fresh state instead.
export function restoreText(db, content, source, text, op, generation) {
  let s;
  try {
    // Activating a new version merges first, which can itself reach a
    // history limit: the reset below covers that too.
    s = activate(db, source, content.hash, undefined, generation);
  } catch (e) {
    if (e?.code !== "capacity") throw e;
    resetDocument(db, content, source, text, generation);
    setSeed(db, source.path, text);
    return;
  }
  try {
    const ytext = s.doc.getText("markdown");
    if (ytext.toString() !== text) {
      const sv = Y.encodeStateVector(s.doc);
      applyText(ytext, text);
      try {
        const bytes = Y.encodeStateAsUpdate(s.doc, sv);
        validateIncremental(s.doc, bytes.length);
        append(db, s, "restore:" + op, base64(bytes));
        compact(db, s);
      } catch (e) {
        if (e?.code !== "capacity") throw e;
        s.doc.destroy();
        resetDocument(db, content, source, text, generation);
      }
    }
  } finally {
    s.doc.destroy();
  }
  setSeed(db, source.path, text);
}

// --- host operations ---

export function sync(db, content, generation, now = Date.now()) {
  ensureFlat(db, content, generation);
  // An intent left after the activation gate is stale: its version never
  // started, or it already activated.
  const intent = getMeta(db, "intent");
  if (intent && !stale(db, generation)) releaseIntent(db, intent);
  const groups = db.query("SELECT * FROM flats_docs_groups");
  const due = groups.filter(
    (g) => now - g.last_at >= GROUP_IDLE || now - g.started_at >= GROUP_MAX_AGE,
  );
  if (due.length && !stale(db, generation)) {
    if (textDirty(db)) record(db, { kind: "person", ...currentShape(content) });
    for (const g of due)
      db.exec("DELETE FROM flats_docs_groups WHERE editor=?", [g.editor]);
  }
  // Nothing unrecorded: no revision is coming, so nothing stays reserved.
  if (!textDirty(db)) clearReservations(db);
  deleteTexts(db, "refs<=0 AND staged_at IS NOT NULL AND staged_at<?", [
    now - STAGED_TTL,
  ]);
  dropUnused(db);
  return {
    store: getMeta(db, "store"),
    ready: db.query("SELECT count(*) AS n FROM flats_docs_outbox")[0].n,
    intent: getMeta(db, "intent")?.id ?? null,
  };
}

export function outbox(db, after) {
  const entries = [];
  let bytes = 0,
    seq = after,
    more = false;
  for (;;) {
    const row = db.query(
      "SELECT * FROM flats_docs_outbox WHERE seq>? ORDER BY seq LIMIT 1",
      [seq],
    )[0];
    if (!row) break;
    if (entries.length && bytes + row.documents.length > PAGE_BYTES) {
      more = true;
      break;
    }
    bytes += row.documents.length + row.authors.length + row.message.length;
    entries.push({
      seq: row.seq,
      created_at: row.created_at,
      kind: row.kind,
      authors: JSON.parse(row.authors),
      message: row.message,
      rollback_of: row.rollback_of ?? undefined,
      op: row.op ?? undefined,
      source: row.source,
      documents: JSON.parse(row.documents),
    });
    seq = row.seq;
  }
  return { store: getMeta(db, "store"), entries, more };
}

// ack deletes the imported entries. budget, measured by the host after it
// stored their blobs, replaces the budget in the same transaction as their
// reservations end, so no edit in between misses both.
export function ack(db, store, through, budget) {
  if (store !== getMeta(db, "store")) throw coded("conflict", "another outbox");
  if (!Number.isSafeInteger(through) || through < 0)
    throw coded("invalid", "invalid through");
  if (budget !== undefined) setBudget(db, budget);
  for (;;) {
    const row = db.query(
      "SELECT seq,documents FROM flats_docs_outbox WHERE seq<=? ORDER BY seq LIMIT 1",
      [through],
    )[0];
    if (!row) break;
    const documents = JSON.parse(row.documents);
    for (const d of documents) release(db, d.hash);
    // An imported entry's texts are host blobs now, which the host measures.
    forgetUnimported(
      db,
      documents.map((d) => d.hash),
    );
    db.exec("DELETE FROM flats_docs_ops WHERE seq=?", [row.seq]);
    db.exec("DELETE FROM flats_docs_outbox WHERE seq=?", [row.seq]);
  }
  dropUnused(db);
}

export function stage(db, text, now = Date.now()) {
  const size = typeof text === "string" ? utf8Bytes(text) : Infinity;
  if (size > LIMITS.text)
    throw coded("invalid", "a staged text is at most 1 MiB");
  const hash = digest(text);
  if (refsOf(db, hash))
    db.exec("UPDATE flats_docs_text_refs SET staged_at=? WHERE hash=?", [
      now,
      hash,
    ]);
  else insertText(db, hash, text, 0, now);
  return hash;
}

const opID = (id) =>
  typeof id === "string" && /^[A-Za-z0-9_-]{8,120}$/.test(id);
function targetDocuments(db, documents) {
  if (!Array.isArray(documents) || documents.length > 128)
    throw coded("invalid", "documents must list at most 128 documents");
  const out = [];
  for (const d of documents) {
    if (typeof d?.path !== "string" || typeof d?.hash !== "string")
      throw coded("invalid", "invalid document");
    if (textOf(db, d.hash) === undefined)
      throw coded("invalid", "text of " + d.path + " is not staged");
    out.push({ path: d.path, hash: d.hash });
  }
  return out;
}

export function setIntent(db, body) {
  if (!opID(body?.id) || typeof body.source !== "string")
    throw coded("invalid", "invalid intent");
  if (body.kind !== "agent" && body.kind !== "rollback")
    throw coded("invalid", "invalid intent kind");
  const documents =
    body.kind === "rollback" ? targetDocuments(db, body.documents) : [];
  const old = getMeta(db, "intent");
  for (const d of documents) addRef(db, d.hash);
  if (old) releaseIntent(db, old);
  setMeta(db, "intent", {
    id: body.id,
    source: body.source,
    kind: body.kind,
    label: String(body.label || "").slice(0, 200),
    message: String(body.message || "").slice(0, 500),
    rollback_of: Number.isSafeInteger(body.rollback_of)
      ? body.rollback_of
      : undefined,
    documents,
  });
  dropUnused(db);
}

// tryEnsureFlat runs the activation gate in a savepoint. When the version's
// merge reaches a history limit, it undoes what the attempt wrote and
// returns that error, so a rollback can still recover the flat; any other
// failure is thrown.
export function tryEnsureFlat(db, content, generation) {
  db.exec("SAVEPOINT flats_docs_gate");
  try {
    ensureFlat(db, content, generation);
    db.exec("RELEASE flats_docs_gate");
    return null;
  } catch (e) {
    db.exec("ROLLBACK TO flats_docs_gate");
    db.exec("RELEASE flats_docs_gate");
    if (e?.code !== "capacity") throw e;
    return e;
  }
}

export function restore(db, content, generation, body) {
  if (!opID(body?.id) || !Number.isSafeInteger(body.rollback_of))
    throw coded("invalid", "invalid restore");
  // A version whose activation cannot finish is recovered by this restore:
  // it replaces every document and then counts as that activation.
  const failed = tryEnsureFlat(db, content, generation);
  if (failed && !getMeta(db, "newest")) throw failed;
  if (stale(db, generation)) throw coded("unavailable", "obsolete worker");
  const documents = targetDocuments(db, body.documents),
    target = new Map(documents.map((d) => [d.path, d.hash]));
  if (
    target.size !== content.documents.length ||
    content.documents.some((d) => !target.has(d.path))
  )
    throw coded("invalid", "documents differ from the running version");
  recordPeople(db, failed ? beforeShape(db) : currentShape(content));
  for (const d of content.documents)
    restoreText(
      db,
      content,
      d,
      textOf(db, target.get(d.path)),
      body.id,
      generation,
    );
  const seq = record(db, {
    kind: "rollback",
    ...currentShape(content),
    force: true,
    authors: [String(body.label || "").slice(0, 200)],
    rollbackOf: body.rollback_of,
    op: body.id,
  });
  if (failed) {
    const intent = getMeta(db, "intent");
    if (intent) releaseIntent(db, intent);
    finishActivation(db, content, generation);
  }
  dropUnused(db);
  return { store: getMeta(db, "store"), seq };
}

export function opStatus(db, id) {
  const row = db.query("SELECT seq FROM flats_docs_ops WHERE id=?", [id])[0];
  return row ? { committed: true, seq: row.seq } : { committed: false };
}

export { textOf };

// --- disk quota (docs/revision-store.md#disk-quota) ---

const MAX_BUDGET_TTL = 24 * 60 * 60 * 1000,
  MAX_STORED = 10000;
// The write-ahead log is transient: after every commit the app truncates
// it when it holds more than WAL_REST_FRAMES frames (storage.js commit), so
// at rest it holds at most that many. The host's measurement leaves the log
// out of "other", and the check reserves exactly that much for it.

// clearReservations ends the reservations for people's unrecorded edits:
// they were just recorded, or no revision is coming.
function clearReservations(db) {
  setMeta(db, "pending", null);
}

// setBudget installs the host's budget: the flat's disk quota (0 = off),
// "other", the bytes the flat's directory uses apart from this database
// file, ttl_ms, how long it stays valid without a refresh, and stored, the
// texts whose host blobs that measurement includes (a partial import
// stores some before their entry is acknowledged). An unchanged budget
// whose deadline is no later than the new one and not yet half expired is
// not written again, so an idle document does not grow its write-ahead
// log; a shorter lifetime always replaces a longer one.
export function setBudget(db, body) {
  const { quota, other, ttl_ms: ttl, stored = [] } = body || {};
  if (
    ![quota, other, ttl].every((n) => Number.isSafeInteger(n) && n >= 0) ||
    ttl < 1 ||
    ttl > MAX_BUDGET_TTL ||
    !Array.isArray(stored) ||
    stored.length > MAX_STORED ||
    !stored.every((h) => typeof h === "string" && /^[0-9a-f]{64}$/.test(h))
  )
    throw coded("invalid", "invalid budget");
  // other was measured with these blobs in place.
  forgetUnimported(db, stored);
  const old = getMeta(db, "budget"),
    now = Date.now(),
    until = now + ttl;
  if (
    old &&
    old.quota === quota &&
    old.other === other &&
    old.until <= until &&
    old.until - now > ttl / 2
  )
    return;
  // Pending reservations are kept only while a quota applies: when one
  // starts, reserve what people already edited.
  if (quota === 0) clearReservations(db);
  else if (!old?.quota) {
    const pending = unrecordedBytes(db);
    setMeta(db, "pending", Object.keys(pending).length ? pending : null);
  }
  setMeta(db, "budget", { quota, other, until });
}
// unrecordedBytes maps every document people edited since it was last
// recorded to its committed text's exact size.
function unrecordedBytes(db) {
  const out = {};
  for (const d of getMeta(db, "newest")?.documents || []) {
    const pos = position(db, d.path),
      rec = db.query("SELECT epoch,seq FROM flats_docs_recorded WHERE doc=?", [
        d.path,
      ])[0];
    if (!pos || (rec && rec.epoch === pos.epoch && rec.seq === pos.seq))
      continue;
    const text = committedText(db, d.path);
    if (text !== null) out[d.path] = { bytes: utf8Bytes(text), exact: true };
  }
  return out;
}
// pageSizeOf reads a database's page size once; every bound below is in
// that database's pages.
const pageSizes = new WeakMap();
function pageSizeOf(db) {
  let n = pageSizes.get(db);
  if (!n) {
    n = db.query("PRAGMA page_size")[0].page_size;
    pageSizes.set(db, n);
  }
  return n;
}
function pageCount(db) {
  return db.query("PRAGMA page_count")[0].page_count;
}
// forgetUnimported ends the reservations of texts whose host blobs exist.
// Reservations live in flats_docs_unimported, one row per text, so no
// single value grows with a large backlog.
function forgetUnimported(db, hashes) {
  for (let i = 0; i < hashes.length; i += 500) {
    const batch = hashes.slice(i, i + 500);
    db.exec(
      `DELETE FROM flats_docs_unimported WHERE hash IN (${batch.map(() => "?").join(",")})`,
      batch,
    );
  }
}
function unimportedBytes(db) {
  return db.query(
    "SELECT coalesce(sum(size),0) AS n FROM flats_docs_unimported",
  )[0].n;
}
// Slack pages for B-tree splits: an insert splits at most one page per
// level (five levels or fewer here) in each tree it touches. Per recorded
// text: the texts table, its hash index, the recorded and the unimported
// tables. Per entry: the outbox and meta.newest rows.
const DOC_SLACK_PAGES = 16,
  ENTRY_SLACK_PAGES = 16;
// valuePages bounds the pages a value of n bytes takes: its overflow pages,
// rounded up, and the leaf holding its cell.
const valuePages = (n, pageSize) => Math.ceil(n / (pageSize - 4)) + 1;
function metaLength(db, key) {
  return (
    db.query("SELECT length(value) AS n FROM flats_docs_meta WHERE key=?", [
      key,
    ])[0]?.n || 0
  );
}
// recordingPages bounds the pages recording the pending documents will
// allocate: every document's text as its own row (documents with the same
// text now may differ by then), never counting pages it frees, one entry
// with its outbox row and meta.newest, and a page per listed document whose
// reference count changes.
function recordingPages(db, pending, listed) {
  const docs = Object.values(pending);
  if (!docs.length) return 0;
  const pageSize = pageSizeOf(db);
  let pages = ENTRY_SLACK_PAGES + listed;
  for (const p of docs)
    pages += valuePages(p.bytes, pageSize) + DOC_SLACK_PAGES;
  const list = 3 * metaLength(db, "newest") + 300 * docs.length + 4096;
  return pages + 2 * valuePages(list, pageSize);
}
// walReserve is the write-ahead log at rest: its 32-byte header and
// WAL_REST_FRAMES frames of a page plus a 24-byte header each.
const walReserve = (pageSize) => 32 + WAL_REST_FRAMES * (pageSize + 24);
// quotaStats counts, for tests, exact sizes taken from the edited text in
// memory (sized) and from other documents' committed state (loaded).
export const quotaStats = { sized: 0, loaded: 0 };
// checkDiskQuota refuses, inside the caller's transaction and after its
// writes, a change that would put the flat over its disk quota: this
// database as it now is, the texts recorded but not imported yet (the host
// will store each as a blob), the revision people's unrecorded edits will
// need when their groups close (every edited document's text as its own
// row in this database, plus its host blob) and the write-ahead log at
// rest. edited is a person's edit: the path and Y.Text of the document it
// changed. A pending document's size is first the bound of three UTF-8
// bytes per UTF-16 unit of its visible text (which includes text an update
// made visible by completing earlier ones), and is made exact only when
// the bounds would not fit. A budget the host has not refreshed in time
// admits nothing: the edit fails as transient, so editors resend it and
// update_document can retry. A rollback never calls this.
export function checkDiskQuota(db, edited) {
  const budget = getMeta(db, "budget");
  if (!budget || !(Date.now() <= budget.until))
    throw Object.assign(
      new Error("the host has not refreshed this document's disk budget"),
      { code: "unavailable", transient: true },
    );
  if (budget.quota === 0) return;
  const pending = getMeta(db, "pending") || {};
  if (edited)
    pending[edited.path] = { bytes: edited.text.length * 3, exact: false };
  const listed = getMeta(db, "newest")?.documents.length || 0;
  const pageSize = pageSizeOf(db);
  const total = () => {
    const recording = recordingPages(db, pending, listed);
    let n =
      budget.other +
      (pageCount(db) + recording) * pageSize +
      unimportedBytes(db) +
      walReserve(pageSize);
    for (const p of Object.values(pending)) n += p.bytes;
    return n;
  };
  if (total() > budget.quota)
    for (const [path, p] of Object.entries(pending)) {
      if (p.exact) continue;
      const own = path === edited?.path;
      quotaStats[own ? "sized" : "loaded"]++;
      const text = own ? edited.text.toString() : committedText(db, path);
      pending[path] = {
        bytes: text === null ? 0 : utf8Bytes(text),
        exact: true,
      };
    }
  if (edited) setMeta(db, "pending", pending);
  const used = total();
  if (used > budget.quota)
    throw coded(
      "disk_quota",
      `the flat would use ${used} bytes, over its ${budget.quota}-byte disk quota`,
    );
}

function coded(code, message) {
  return Object.assign(new Error(message), { code });
}
