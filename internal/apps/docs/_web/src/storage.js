import * as Y from "yjs";
import { sha256 } from "@noble/hashes/sha256";
import { bytesToHex } from "@noble/hashes/utils";
import { encodeUtf8 } from "lib0/string";
import { base64, unbase64, utf8Bytes, LIMITS } from "./protocol.js";
import { mergeText, applyText } from "./merge.js";
export const digest = (s) =>
  globalThis.__flats_docsCodec
    ? globalThis.__flats_docsCodec.digest(s)
    : bytesToHex(sha256(encodeUtf8(s)));
export function schema(db) {
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_conflicts(doc TEXT NOT NULL, generation INTEGER NOT NULL, markdown TEXT NOT NULL, PRIMARY KEY(doc,generation))",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_documents(doc TEXT PRIMARY KEY, epoch TEXT NOT NULL, seq INTEGER NOT NULL, chain TEXT NOT NULL, seed_hash TEXT NOT NULL, seed_text TEXT NOT NULL, updated_at INTEGER NOT NULL)",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_updates(doc TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL, chain TEXT NOT NULL, data TEXT NOT NULL, size INTEGER NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(doc,seq), UNIQUE(doc,id))",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_checkpoints(doc TEXT PRIMARY KEY, seq INTEGER NOT NULL, chain TEXT NOT NULL, state TEXT NOT NULL, created_at INTEGER NOT NULL)",
  );
  // Compact data, retain exact dedupe and chain history. Admission bounds receipts.
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_receipts(doc TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL, chain TEXT NOT NULL, hash TEXT NOT NULL, PRIMARY KEY(doc,seq), UNIQUE(doc,id))",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_activations(doc TEXT NOT NULL, hash TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY(doc,hash))",
  );
  // Revision capture (revisions.js).
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_recorded(doc TEXT PRIMARY KEY, epoch TEXT NOT NULL, seq INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL)",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_groups(editor TEXT PRIMARY KEY, label TEXT NOT NULL, started_at INTEGER NOT NULL, last_at INTEGER NOT NULL, dirty INTEGER NOT NULL)",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_outbox(seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL, kind TEXT NOT NULL, authors TEXT NOT NULL, message TEXT NOT NULL, rollback_of INTEGER, op TEXT, source TEXT NOT NULL, documents TEXT NOT NULL)",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_texts(hash TEXT PRIMARY KEY, text TEXT NOT NULL, refs INTEGER NOT NULL, staged_at INTEGER)",
  );
  db.exec(
    "CREATE TABLE IF NOT EXISTS flats_docs_ops(id TEXT PRIMARY KEY, seq INTEGER NOT NULL)",
  );
  migrate(db);
}
export function history(db, row, known) {
  if (!known) return true;
  if (
    known.epoch !== row.epoch ||
    !Number.isSafeInteger(known.seq) ||
    known.seq < 0 ||
    known.seq > row.seq ||
    typeof known.chain !== "string"
  )
    return false;
  if (known.seq === row.seq) return known.chain === row.chain;
  return (
    db.query("SELECT chain FROM flats_docs_receipts WHERE doc=? AND seq=?", [
      row.doc,
      known.seq,
    ])[0]?.chain === known.chain
  );
}
export function validateDocument(doc) {
  const text = doc.getText("markdown");
  if (
    (text.length * 3 > LIMITS.text &&
      utf8Bytes(text.toString()) > LIMITS.text) ||
    doc.share.size !== 1
  )
    throw Object.assign(new Error("document limit or invalid shared type"), {
      code: "capacity",
    });
  if (structureCount(doc) > LIMITS.structs)
    throw Object.assign(new Error("CRDT structure limit"), {
      code: "capacity",
    });
}
export function validateState(doc) {
  validateDocument(doc);
  const state = Y.encodeStateAsUpdate(doc);
  if (state.length > LIMITS.state)
    throw Object.assign(new Error("CRDT state limit"), { code: "capacity" });
  doc._flatsStateSize = state.length;
  return state;
}
// An update contains its own struct/delete-set metadata. This conservative
// bound avoids serializing a large document on every keystroke. Checkpoints
// reset it to the exact encoded size; near the cap, check the exact size.
export function validateIncremental(doc, added) {
  validateDocument(doc);
  const size = (doc._flatsStateSize || 0) + added + 64;
  if (size > LIMITS.state) return validateState(doc).length;
  doc._flatsStateSize = size;
  return size;
}
export function load(db, path, cached) {
  try {
    return loadCommitted(db, path, cached);
  } catch (e) {
    e.transient = true;
    throw e;
  }
}
function loadCommitted(db, path, cached) {
  const row = db.query(
    "SELECT doc,epoch,seq,chain,seed_hash,updated_at FROM flats_docs_documents WHERE doc=?",
    [path],
  )[0];
  if (!row) return null;
  if (cached && !history(db, row, cached.row))
    throw new Error("restored history");
  const cp = db.query("SELECT seq FROM flats_docs_checkpoints WHERE doc=?", [
    path,
  ])[0];
  let doc = cached?.doc,
    seq = cached?.row.seq || 0,
    reloaded = false;
  if (!doc || seq < cp.seq) {
    reloaded = true;
    doc = new Y.Doc();
    doc.getText("markdown");
    const checkpoint = unbase64(
      db.query("SELECT state FROM flats_docs_checkpoints WHERE doc=?", [
        path,
      ])[0].state,
      LIMITS.state,
    );
    Y.applyUpdate(doc, checkpoint);
    doc._flatsStateSize = checkpoint.length;
    seq = cp.seq;
  }
  const rows = db.query(
    "SELECT * FROM flats_docs_updates WHERE doc=? AND seq>? ORDER BY seq LIMIT 65",
    [path, seq],
  );
  if (rows.length > 64) throw new Error("replay limit");
  let added = 0;
  for (const u of rows) {
    const bytes = unbase64(u.data, LIMITS.state);
    Y.applyUpdate(doc, bytes);
    added += bytes.length + 64;
  }
  if (rows.length || reloaded) validateIncremental(doc, added);
  return {
    doc,
    row,
    changes: cached ? rows.filter((u) => u.seq > cached.row.seq) : rows,
    reset: cached && cached.row.seq < cp.seq,
  };
}
export function append(db, s, id, data) {
  if (
    s.row.seq >= LIMITS.receipts ||
    db.query("SELECT count(*) AS n FROM flats_docs_receipts")[0].n >=
      LIMITS.totalReceipts
  )
    throw Object.assign(new Error("history capacity reached"), {
      code: "capacity",
    });
  const seq = s.row.seq + 1,
    chain = digest(s.row.chain + "\n" + id),
    now = Date.now();
  db.exec("INSERT INTO flats_docs_updates VALUES(?,?,?,?,?,?,?)", [
    s.row.doc,
    seq,
    id,
    chain,
    data,
    data.length,
    now,
  ]);
  db.exec("INSERT INTO flats_docs_receipts VALUES(?,?,?,?,?)", [
    s.row.doc,
    seq,
    id,
    chain,
    digest(data),
  ]);
  db.exec(
    "UPDATE flats_docs_documents SET seq=?,chain=?,updated_at=? WHERE doc=?",
    [seq, chain, now, s.row.doc],
  );
  s.row = { ...s.row, seq, chain, updated_at: now };
  return { t: "update", u: data, seq, chain };
}
export function compact(db, s, state) {
  const stats = db.query(
    "SELECT count(*) AS n,coalesce(sum(size),0) AS bytes FROM flats_docs_updates WHERE doc=?",
    [s.row.doc],
  )[0];
  if (stats.n >= 64 || stats.bytes >= 512 * 1024) {
    const encoded = base64(state || validateState(s.doc));
    db.exec("INSERT OR REPLACE INTO flats_docs_checkpoints VALUES(?,?,?,?,?)", [
      s.row.doc,
      s.row.seq,
      s.row.chain,
      encoded,
      Date.now(),
    ]);
    db.exec("DELETE FROM flats_docs_updates WHERE doc=? AND seq<=?", [
      s.row.doc,
      s.row.seq,
    ]);
  }
}
export function activate(db, source, versionHash, cached, runtimeGeneration) {
  if (!Number.isSafeInteger(runtimeGeneration) || runtimeGeneration < 1)
    throw new Error("host runtime generation unavailable");
  let s = load(db, source.path, cached),
    // The committed text before a merge, for revision capture.
    before,
    activation = null,
    activationState;
  const highest = db.query(
    "SELECT coalesce(max(version),0) AS version FROM flats_docs_activations",
  )[0].version;
  if (!s) {
    if (runtimeGeneration < highest)
      throw new Error("obsolete source cannot seed a document");
    if (
      db.query("SELECT count(*) AS n FROM flats_docs_documents")[0].n >=
        LIMITS.documents ||
      db.query("SELECT count(*) AS n FROM flats_docs_receipts")[0].n >=
        LIMITS.totalReceipts
    )
      throw Object.assign(new Error("stored document capacity"), {
        code: "capacity",
      });
    const doc = new Y.Doc();
    doc.getText("markdown").insert(0, source.text);
    const state = validateState(doc),
      epoch = crypto.randomUUID();
    const row = {
      doc: source.path,
      epoch,
      seq: 0,
      chain: digest(epoch),
      seed_hash: source.hash,
      seed_text: source.text,
      updated_at: Date.now(),
    };
    const encoded = base64(state);
    // The seed text lives in flats_docs_seeds (see migrate).
    db.exec(
      "INSERT INTO flats_docs_documents VALUES(?,?,?,?,?,?,?)",
      Object.values({ ...row, seed_text: "" }),
    );
    setSeed(db, row.doc, source.text);
    db.exec("INSERT INTO flats_docs_checkpoints VALUES(?,?,?,?,?)", [
      row.doc,
      0,
      row.chain,
      encoded,
      row.updated_at,
    ]);
    db.exec("INSERT INTO flats_docs_receipts VALUES(?,?,?,?,?)", [
      row.doc,
      0,
      "seed:" + epoch,
      row.chain,
      digest("seed:" + epoch),
    ]);
    s = { doc, row, changes: [] };
  }
  // Keep the legacy column name for existing databases; values now carry
  // host generations. A repeated hash may be a newer rollback activation.
  const documentGeneration = db.query(
    "SELECT coalesce(max(version),0) AS version FROM flats_docs_activations WHERE doc=?",
    [source.path],
  )[0].version;
  if (runtimeGeneration > documentGeneration && runtimeGeneration >= highest) {
    const seen = db.query(
      "SELECT hash FROM flats_docs_activations WHERE doc=? AND hash=?",
      [source.path, versionHash],
    )[0];
    if (
      !seen &&
      db.query("SELECT count(*) AS n FROM flats_docs_activations WHERE doc=?", [
        source.path,
      ])[0].n >= LIMITS.activations
    )
      throw Object.assign(new Error("activation capacity"), {
        code: "capacity",
      });
    if (s.row.seed_hash !== source.hash) {
      before = s.doc.getText("markdown").toString();
      const merged = mergeText(seedText(db, source.path), before, source.text),
        sv = Y.encodeStateVector(s.doc);
      applyText(s.doc.getText("markdown"), merged);
      activationState = validateState(s.doc);
      activation = append(
        db,
        s,
        "activation:" + runtimeGeneration + ":" + versionHash,
        base64(Y.encodeStateAsUpdate(s.doc, sv)),
      );
      db.exec("UPDATE flats_docs_documents SET seed_hash=? WHERE doc=?", [
        source.hash,
        source.path,
      ]);
      setSeed(db, source.path, source.text);
      s.row.seed_hash = source.hash;
      s.row.seed_text = source.text;
    }
    db.exec(
      "INSERT INTO flats_docs_activations VALUES(?,?,?) ON CONFLICT(doc,hash) DO UPDATE SET version=excluded.version",
      [source.path, versionHash, runtimeGeneration],
    );
  }
  if (activation) compact(db, s, activationState);
  return { ...s, activation, before };
}
// WAL_REST_FRAMES bounds the write-ahead log at rest: after every commit
// the app reads how many frames the log holds and truncates it when there
// are more (see commit). One transaction can exceed it only until its own
// commit returns, or while a reader keeps the truncation from finishing.
export const WAL_REST_FRAMES = 64;
const hasTable = (db, name) =>
  db.query("SELECT 1 AS x FROM sqlite_master WHERE type='table' AND name=?", [
    name,
  ]).length > 0;
// migrate keeps large payloads in rows nothing rewrites often:
// * flats_docs_text_refs holds revision text references and staging times,
//   apart from the text rows, so a reference change never rewrites a large
//   text;
// * flats_docs_seeds holds each document's merge base (seed text), apart
//   from its flats_docs_documents row, whose seq and chain change with
//   every update.
// A database from before a table gets it once, in one transaction that
// moves the existing values there; the old columns stay, unused (0, NULL
// or empty).
const migrated = new WeakSet();
export function migrate(db) {
  if (migrated.has(db)) return;
  if (
    !hasTable(db, "flats_docs_text_refs") ||
    !hasTable(db, "flats_docs_seeds") ||
    !hasTable(db, "flats_docs_unimported")
  ) {
    db.exec("BEGIN IMMEDIATE");
    try {
      if (!hasTable(db, "flats_docs_unimported")) {
        db.exec(
          "CREATE TABLE flats_docs_unimported(hash TEXT PRIMARY KEY, size INTEGER NOT NULL)",
        );
        // Every text an unacknowledged entry lists may still need its
        // host blob.
        reserveOutbox(db);
        if (hasTable(db, "flats_docs_meta"))
          db.exec("DELETE FROM flats_docs_meta WHERE key='unimported'");
      }
      if (!hasTable(db, "flats_docs_text_refs")) {
        db.exec(
          "CREATE TABLE flats_docs_text_refs(hash TEXT PRIMARY KEY, refs INTEGER NOT NULL, staged_at INTEGER)",
        );
        if (hasTable(db, "flats_docs_texts"))
          db.exec(
            "INSERT INTO flats_docs_text_refs SELECT hash,refs,staged_at FROM flats_docs_texts",
          );
      }
      if (!hasTable(db, "flats_docs_seeds")) {
        db.exec(
          "CREATE TABLE flats_docs_seeds(doc TEXT PRIMARY KEY, text TEXT NOT NULL)",
        );
        if (hasTable(db, "flats_docs_documents")) {
          db.exec(
            "INSERT INTO flats_docs_seeds SELECT doc,seed_text FROM flats_docs_documents",
          );
          db.exec("UPDATE flats_docs_documents SET seed_text=''");
        }
      }
      db.exec("COMMIT");
    } catch (e) {
      try {
        db.exec("ROLLBACK");
      } catch {}
      throw e;
    }
  }
  migrated.add(db);
}
// reserveOutbox reserves, for a database from before the disk quota check,
// the host blobs of every text its unacknowledged entries list: the host
// may not have stored them yet (flats_docs_unimported, see revisions.js).
// It reads the outbox one entry at a time, as the outbox host operation
// does, so a large backlog never exceeds the query result limit.
function reserveOutbox(db) {
  if (!hasTable(db, "flats_docs_outbox")) return;
  for (let seq = 0; ; ) {
    const entry = db.query(
      "SELECT seq,documents FROM flats_docs_outbox WHERE seq>? ORDER BY seq LIMIT 1",
      [seq],
    )[0];
    if (!entry) return;
    seq = entry.seq;
    for (const d of JSON.parse(entry.documents))
      if (typeof d?.hash === "string" && Number.isSafeInteger(d.size))
        db.exec("INSERT OR IGNORE INTO flats_docs_unimported VALUES(?,?)", [
          d.hash,
          d.size,
        ]);
  }
}
// seedText is a document's merge base; setSeed replaces it.
export function seedText(db, doc) {
  return db.query("SELECT text FROM flats_docs_seeds WHERE doc=?", [doc])[0]
    ?.text;
}
export function setSeed(db, doc, text) {
  db.exec("INSERT OR REPLACE INTO flats_docs_seeds VALUES(?,?)", [doc, text]);
}
function begin(db, sql) {
  migrate(db);
  db.exec(sql);
}
// commit commits, then reads how many frames the write-ahead log holds and
// truncates the log when there are more than WAL_REST_FRAMES. The count
// comes from a NOOP checkpoint (SQLite 3.51+), which copies nothing: a
// PASSIVE one would copy and sync every small commit (two fsyncs and a
// database truncate on top of the commit's own), so small commits now
// collect in the log until the truncation copies them at once. A truncation
// a reader kept from finishing (busy) or that failed is remembered in
// meta.wal_truncate, shared by every VM, and retried after every later
// commit until one succeeds that saw the latest failure: a fully copied log can restart for a small
// transaction while its file keeps its earlier size, which the frame count
// alone would no longer show.
function commit(db) {
  db.exec("COMMIT");
  // The marker holds the generation of the failure it records, taken from
  // meta.wal_generation, a counter that is never reset: a success clears
  // only the generation it read, so a failure recorded meanwhile (even
  // after another success deleted the marker) stays.
  let owed;
  try {
    owed = db.query(
      "SELECT value FROM flats_docs_meta WHERE key='wal_truncate'",
    )[0]?.value;
    const row = db.query("PRAGMA wal_checkpoint(NOOP)")[0];
    if (owed === undefined && !(row && row.log > WAL_REST_FRAMES)) return;
    const done = db.query("PRAGMA wal_checkpoint(TRUNCATE)")[0];
    if (done && !done.busy) {
      if (owed !== undefined)
        db.exec(
          "DELETE FROM flats_docs_meta WHERE key='wal_truncate' AND value=?",
          [owed],
        );
      return;
    }
  } catch {}
  try {
    db.exec("BEGIN IMMEDIATE");
    try {
      db.exec(
        // Above any marker too, in case one predates the counter.
        "INSERT INTO flats_docs_meta SELECT 'wal_generation',CAST(max(coalesce((SELECT CAST(value AS INTEGER) FROM flats_docs_meta WHERE key='wal_generation'),0),coalesce((SELECT CAST(value AS INTEGER) FROM flats_docs_meta WHERE key='wal_truncate'),0))+1 AS TEXT) WHERE 1 ON CONFLICT(key) DO UPDATE SET value=excluded.value",
      );
      db.exec(
        "INSERT OR REPLACE INTO flats_docs_meta SELECT 'wal_truncate',value FROM flats_docs_meta WHERE key='wal_generation'",
      );
      db.exec("COMMIT");
    } catch (e) {
      db.exec("ROLLBACK");
      throw e;
    }
  } catch {}
}
export function transaction(db, fn) {
  begin(db, "BEGIN IMMEDIATE");
  try {
    const result = fn();
    commit(db);
    return result;
  } catch (e) {
    try {
      db.exec("ROLLBACK");
    } catch {}
    throw e;
  }
}

export function validateUpdate(bytes) {
  let reader;
  const decoded = Y.decodeUpdateV2(
    bytes,
    class extends Y.UpdateDecoderV1 {
      constructor(decoder) {
        super(decoder);
        reader = decoder;
      }
    },
  );
  if (reader.pos !== bytes.length) throw new Error("trailing update data");
  if (decoded.structs.length > 4096 || decoded.ds.clients.size > 1024)
    throw new Error("update structure limit");
  let ranges = 0;
  for (const items of decoded.ds.clients.values())
    for (const item of items) {
      ranges++;
      if (
        !Number.isSafeInteger(item.clock) ||
        !Number.isSafeInteger(item.len) ||
        item.clock < 0 ||
        item.len < 0
      )
        throw new Error("invalid delete range");
    }
  if (ranges > 4096) throw new Error("delete range limit");
  for (const s of decoded.structs) {
    if (
      !Number.isSafeInteger(s.id.clock) ||
      !Number.isSafeInteger(s.length) ||
      s.id.clock < 0 ||
      s.length < 0 ||
      s.length > LIMITS.text
    )
      throw new Error("invalid struct");
    if (s instanceof Y.GC) continue;
    if (
      (typeof s.parent === "string" && s.parent !== "markdown") ||
      s.parentSub != null ||
      !(
        s.content instanceof Y.ContentString ||
        s.content instanceof Y.ContentDeleted
      )
    )
      throw new Error("only plain Markdown text is supported");
  }
  return decoded;
}
export function structureCount(doc) {
  let n = 0;
  for (const items of doc.store.clients.values()) n += items.length;
  return n;
}

// current reads a document's committed state, activating it first when
// needed. ensureFlat(db) runs the flat activation of the running version in
// the same write transaction (revisions.js); the read-only fast path is taken
// only once that activation ran, or when a newer worker owns the flat.
export function current(
  db,
  source,
  versionHash,
  cached,
  generation,
  ensureFlat,
) {
  db.exec("BEGIN");
  try {
    const ready = db.query(
      "SELECT version FROM flats_docs_activations WHERE doc=? AND hash=?",
      [source.path, versionHash],
    )[0];
    const highest = db.query(
      "SELECT coalesce(max(version),0) AS version FROM flats_docs_activations",
    )[0].version;
    const raw = db.query(
        "SELECT value FROM flats_docs_meta WHERE key='active'",
      )[0],
      active = raw ? JSON.parse(raw.value) : null,
      flatReady =
        !ensureFlat ||
        active?.hash === versionHash ||
        (active && active.generation > generation) ||
        highest > generation;
    if (
      flatReady &&
      ready &&
      (ready.version >= generation || highest > generation)
    ) {
      const s = load(db, source.path, cached);
      db.exec("COMMIT");
      return s;
    }
    db.exec("COMMIT");
  } catch (e) {
    try {
      db.exec("ROLLBACK");
    } catch {}
    // A fresh database has no schema yet.
    if (!String(e.message).includes("no such table")) throw e;
  }
  schema(db);
  return transaction(db, () => {
    ensureFlat?.(db);
    return activate(db, source, versionHash, cached, generation);
  });
}
export function rejectionCode(e) {
  return e.transient ? null : e.code || null;
}
