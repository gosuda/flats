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
    db.exec("INSERT OR REPLACE INTO flats_docs_checkpoints VALUES(?,?,?,?,?)", [
      s.row.doc,
      s.row.seq,
      s.row.chain,
      base64(state || validateState(s.doc)),
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
    db.exec(
      "INSERT INTO flats_docs_documents VALUES(?,?,?,?,?,?,?)",
      Object.values(row),
    );
    db.exec("INSERT INTO flats_docs_checkpoints VALUES(?,?,?,?,?)", [
      row.doc,
      0,
      row.chain,
      base64(state),
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
      const merged = mergeText(
          db.query("SELECT seed_text FROM flats_docs_documents WHERE doc=?", [
            source.path,
          ])[0].seed_text,
          s.doc.getText("markdown").toString(),
          source.text,
          (markdown) =>
            preserveConflict(db, source.path, runtimeGeneration, markdown),
        ),
        sv = Y.encodeStateVector(s.doc);
      applyText(s.doc.getText("markdown"), merged);
      activationState = validateState(s.doc);
      activation = append(
        db,
        s,
        "activation:" + runtimeGeneration + ":" + versionHash,
        base64(Y.encodeStateAsUpdate(s.doc, sv)),
      );
      db.exec(
        "UPDATE flats_docs_documents SET seed_hash=?,seed_text=? WHERE doc=?",
        [source.hash, source.text, source.path],
      );
      s.row.seed_hash = source.hash;
      s.row.seed_text = source.text;
    }
    db.exec(
      "INSERT INTO flats_docs_activations VALUES(?,?,?) ON CONFLICT(doc,hash) DO UPDATE SET version=excluded.version",
      [source.path, versionHash, runtimeGeneration],
    );
    pruneConflicts(db, source.path);
  }
  if (activation) compact(db, s, activationState);
  return { ...s, activation };
}
function preserveConflict(db, path, generation, markdown) {
  db.exec("INSERT OR IGNORE INTO flats_docs_conflicts VALUES(?,?,?)", [
    path,
    generation,
    markdown,
  ]);
}
function pruneConflicts(db, path) {
  const rows = db.query(
    "SELECT generation,length(CAST(markdown AS BLOB)) AS bytes FROM flats_docs_conflicts WHERE doc=? ORDER BY generation DESC",
    [path],
  );
  let bytes = 0,
    count = 0;
  for (const row of rows) {
    bytes += row.bytes;
    if (++count > LIMITS.conflicts || bytes > LIMITS.conflictBytes) {
      db.exec(
        "DELETE FROM flats_docs_conflicts WHERE doc=? AND generation<=?",
        [path, row.generation],
      );
      break;
    }
  }
}
export function conflictList(db, path) {
  return db.query(
    "SELECT generation,length(CAST(markdown AS BLOB)) AS bytes FROM flats_docs_conflicts WHERE doc=? ORDER BY generation DESC LIMIT ?",
    [path, LIMITS.conflicts],
  );
}
export function transaction(db, fn) {
  db.exec("BEGIN IMMEDIATE");
  try {
    const result = fn();
    db.exec("COMMIT");
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

export function current(db, source, versionHash, cached, generation) {
  db.exec("BEGIN");
  try {
    const ready = db.query(
      "SELECT version FROM flats_docs_activations WHERE doc=? AND hash=?",
      [source.path, versionHash],
    )[0];
    const highest = db.query(
      "SELECT coalesce(max(version),0) AS version FROM flats_docs_activations",
    )[0].version;
    if (ready && (ready.version >= generation || highest > generation)) {
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
  return transaction(db, () =>
    activate(db, source, versionHash, cached, generation),
  );
}
export function rejectionCode(e) {
  return e.transient ? null : e.code || null;
}
