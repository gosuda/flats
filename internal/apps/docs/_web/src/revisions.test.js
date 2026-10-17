import { test } from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import { mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import * as Y from "yjs";
import {
  schema,
  current,
  transaction,
  activate,
  append,
  compact,
  digest,
  seedText,
  WAL_REST_FRAMES,
} from "./storage.js";
import { base64 } from "./protocol.js";
import * as revisions from "./revisions.js";

// The worker's env.DB shape over an in-memory SQLite database, or with
// file: true over a database file in write-ahead log mode, as the worker
// uses, whose log tests can measure (walBytes).
function database({ file = false, schema: create = true, pageSize, at } = {}) {
  let path = at || ":memory:";
  if (file && !at) {
    const dir = mkdtempSync(join(tmpdir(), "flats-docs-"));
    process.on("exit", () => rmSync(dir, { recursive: true, force: true }));
    path = join(dir, "db.sqlite");
  }
  const raw = new DatabaseSync(path);
  if (pageSize) raw.exec(`PRAGMA page_size=${pageSize}`);
  if (file) raw.exec("PRAGMA journal_mode=WAL");
  const db = {
    path: file ? path : null,
    // onExec, when a test sets it, runs before a statement.
    onExec: null,
    exec(sql, params) {
      db.onExec?.(sql);
      if (!params) raw.exec(sql);
      else raw.prepare(sql).run(...params);
    },
    // onQuery, when a test sets it, runs before a query.
    onQuery: null,
    query(sql, params = []) {
      db.onQuery?.(sql);
      return raw
        .prepare(sql)
        .all(...params)
        .map((r) => ({ ...r }));
    },
  };
  if (create) schema(db);
  return db;
}
const walBytes = (db) => {
  try {
    return statSync(db.path + "-wal").size;
  } catch {
    return 0;
  }
};

const doc = (path, text) => ({ path, text, hash: digest(text) });
const content = (hash, ...documents) => ({ hash, documents });
let generation = 1;
const next = () => ++generation;

// A person's edit: append text to a document as one update.
function personEdit(
  db,
  c,
  path,
  text,
  editor = "browser:a",
  label = "Ada",
  gen = generation,
) {
  transaction(db, () => {
    revisions.ensureFlat(db, c, gen);
    const s = activate(
      db,
      c.documents.find((d) => d.path === path),
      c.hash,
      undefined,
      gen,
    );
    const sv = Y.encodeStateVector(s.doc),
      t = s.doc.getText("markdown");
    t.insert(t.length, text);
    append(
      db,
      s,
      "u:" + Math.random().toString(36).slice(2),
      base64(Y.encodeStateAsUpdate(s.doc, sv)),
    );
    s.doc.destroy();
    revisions.touchGroup(db, editor, label);
  });
}
const entries = (db) => revisions.outbox(db, 0).entries;
const textOf = (db, e, path) =>
  revisions.textOf(db, e.documents.find((d) => d.path === path).hash);
const refs = (db) =>
  Object.fromEntries(
    db
      .query("SELECT hash,refs FROM flats_docs_text_refs")
      .map((r) => [r.hash, r.refs]),
  );

test("the first activation records a created entry, and a restart records nothing", () => {
  const db = database(),
    c = content("v1", doc("a.md", "# A\n"), doc("b.md", "# B\n"));
  const gen = next();
  transaction(db, () => revisions.ensureFlat(db, c, gen));
  let e = entries(db);
  assert.equal(e.length, 1);
  assert.equal(e[0].kind, "created");
  assert.equal(e[0].message, "");
  assert.equal(textOf(db, e[0], "a.md"), "# A\n");
  // A new runtime generation of the same content is no new activation.
  const later = next();
  transaction(db, () =>
    assert.equal(revisions.ensureFlat(db, c, later), false),
  );
  assert.equal(entries(db).length, 1);
});

test("a new version keeps people's text before its merge, crediting them", () => {
  const db = database(),
    v1 = content("v1", doc("a.md", "line\n"));
  transaction(db, () => revisions.ensureFlat(db, v1, next()));
  personEdit(db, v1, "a.md", "human\n");
  const v2 = content("v2", doc("a.md", "rewritten\n"));
  transaction(db, () =>
    revisions.setIntent(db, {
      id: "op-save-0001",
      source: "v2",
      kind: "agent",
      label: "Agent",
      message: "rewrite",
    }),
  );
  const gen = next();
  transaction(db, () => revisions.ensureFlat(db, v2, gen));
  const e = entries(db);
  assert.deepEqual(
    e.map((x) => x.kind),
    ["created", "person", "agent"],
  );
  assert.deepEqual(e[1].authors, ["Ada"]);
  assert.equal(e[1].source, "v1");
  assert.equal(textOf(db, e[1], "a.md"), "line\nhuman\n");
  assert.deepEqual(e[2].authors, ["Agent"]);
  assert.equal(e[2].message, "rewrite");
  assert.equal(e[2].op, "op-save-0001");
  assert.equal(revisions.getMeta(db, "intent"), null);
  assert.equal(db.query("SELECT count(*) AS n FROM flats_docs_groups")[0].n, 0);
});

test("groups close when due and credit only who edited since", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  personEdit(db, c, "a.md", "ada\n", "browser:a", "Ada");
  const t0 = Date.now();
  transaction(db, () => revisions.sync(db, c, generation, t0));
  assert.equal(entries(db).length, 1, "not due yet");
  personEdit(db, c, "a.md", "bo\n", "browser:b", "Bo");
  db.exec("UPDATE flats_docs_groups SET last_at=? WHERE editor='browser:a'", [
    t0 - revisions.GROUP_IDLE,
  ]);
  transaction(db, () => revisions.sync(db, c, generation, t0));
  let e = entries(db);
  assert.equal(e.length, 2);
  assert.deepEqual(e[1].authors, ["Ada", "Bo"]);
  // Bo's group survives without unrecorded edits: the next due close
  // records nothing and credits nobody twice.
  assert.equal(db.query("SELECT count(*) AS n FROM flats_docs_groups")[0].n, 1);
  transaction(db, () =>
    revisions.sync(db, c, generation, t0 + revisions.GROUP_MAX_AGE),
  );
  assert.equal(entries(db).length, 2);
});

test("texts stay referenced while recorded and go once nothing needs them", () => {
  const db = database(),
    v1 = content("v1", doc("a.md", "a\n"), doc("gone.md", "gone\n"));
  transaction(db, () => revisions.ensureFlat(db, v1, next()));
  const store = revisions.getMeta(db, "store");
  let e = entries(db);
  transaction(db, () => revisions.ack(db, store, e.at(-1).seq));
  // Recorded rows keep both texts after acknowledgement.
  assert.equal(revisions.textOf(db, digest("a\n")), "a\n");
  assert.equal(revisions.textOf(db, digest("gone\n")), "gone\n");
  // A version without gone.md: once its entries are acknowledged, its text
  // is released; a.md stays as the recorded text.
  const v2 = content("v2", doc("a.md", "a\n"));
  transaction(db, () => revisions.ensureFlat(db, v2, next()));
  e = entries(db);
  transaction(db, () => revisions.ack(db, store, e.at(-1).seq));
  assert.equal(revisions.textOf(db, digest("gone\n")), undefined);
  assert.equal(refs(db)[digest("a\n")], 1);
  assert.equal(
    db.query("SELECT count(*) AS n FROM flats_docs_recorded")[0].n,
    1,
  );
});

test("two paths with one text count twice", () => {
  const db = database(),
    c = content("v1", doc("a.md", "same\n"), doc("b.md", "same\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  // Two recorded rows and two occurrences in the entry.
  assert.equal(refs(db)[digest("same\n")], 4);
  transaction(db, () =>
    revisions.ack(db, revisions.getMeta(db, "store"), entries(db).at(-1).seq),
  );
  assert.equal(refs(db)[digest("same\n")], 2);
});

test("migration turns conflict records into recovered entries, oldest first", () => {
  const db = database(),
    c = content("v1", doc("a.md", "now\n"));
  // State written by an earlier app: a stored document and two records.
  transaction(db, () =>
    activate(db, c.documents[0], c.hash, undefined, next()).doc.destroy(),
  );
  db.exec("INSERT INTO flats_docs_conflicts VALUES('a.md',5,'older\n')");
  db.exec("INSERT INTO flats_docs_conflicts VALUES('removed.md',7,'kept\n')");
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  const e = entries(db);
  assert.deepEqual(
    e.map((x) => x.kind),
    ["recovered", "recovered", "created"],
  );
  assert.equal(textOf(db, e[0], "a.md"), "older\n");
  assert.equal(textOf(db, e[1], "removed.md"), "kept\n");
  assert.equal(textOf(db, e[1], "a.md"), "now\n");
  assert.equal(e[2].message, "revision history started");
  assert.equal(
    db.query("SELECT count(*) AS n FROM flats_docs_conflicts")[0].n,
    0,
  );
  // Overrides did not move the recorded text of a.md.
  assert.equal(
    db.query("SELECT hash FROM flats_docs_recorded WHERE doc='a.md'")[0].hash,
    digest("now\n"),
  );
});

test("a restore is exact, resets the merge base and is recorded once", () => {
  const db = database(),
    c = content("v1", doc("a.md", "one\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  personEdit(db, c, "a.md", "two\n");
  const hash = transaction(db, () => revisions.stage(db, "one\n"));
  const out = transaction(db, () =>
    revisions.restore(db, c, generation, {
      id: "op-restore-001",
      label: "Ada",
      rollback_of: 1,
      documents: [{ path: "a.md", hash }],
    }),
  );
  const e = entries(db);
  assert.deepEqual(
    e.map((x) => x.kind),
    ["created", "person", "rollback"],
  );
  assert.equal(e[2].rollback_of, 1);
  assert.equal(out.seq, e[2].seq);
  assert.deepEqual(revisions.opStatus(db, "op-restore-001"), {
    committed: true,
    seq: e[2].seq,
  });
  const s = current(db, c.documents[0], c.hash, undefined, generation);
  assert.equal(s.doc.getText("markdown").toString(), "one\n");
  s.doc.destroy();
  assert.equal(seedText(db, "a.md"), "one\n");
  // Acknowledging the rollback forgets its receipt.
  transaction(db, () =>
    revisions.ack(db, revisions.getMeta(db, "store"), e.at(-1).seq),
  );
  assert.deepEqual(revisions.opStatus(db, "op-restore-001"), {
    committed: false,
  });
});

test("a restore that would exceed the history limit resets the document", () => {
  const db = database(),
    c = content("v1", doc("a.md", "one\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  personEdit(db, c, "a.md", "two\n");
  // The document's history is full: an append is refused.
  db.exec("UPDATE flats_docs_documents SET seq=100000 WHERE doc='a.md'");
  const epoch = db.query(
    "SELECT epoch FROM flats_docs_documents WHERE doc='a.md'",
  )[0].epoch;
  const hash = transaction(db, () => revisions.stage(db, "one\n"));
  transaction(db, () =>
    revisions.restore(db, c, generation, {
      id: "op-restore-002",
      label: "Ada",
      rollback_of: 1,
      documents: [{ path: "a.md", hash }],
    }),
  );
  const row = db.query(
    "SELECT epoch,seq FROM flats_docs_documents WHERE doc='a.md'",
  )[0];
  assert.notEqual(row.epoch, epoch);
  assert.equal(seedText(db, "a.md"), "one\n");
  assert.equal(entries(db).at(-1).kind, "rollback");
});

test("a rollback intent restores the new version's documents when it activates", () => {
  const db = database(),
    v1 = content("v1", doc("a.md", "old\n"), doc("b.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, v1, next()));
  personEdit(db, v1, "a.md", "edit\n");
  // Revision content: only a.md, as "old\n". The new version's source text
  // differs from it, as a rebuilt bundle's would when people edited since.
  const hash = transaction(db, () => revisions.stage(db, "old\n"));
  transaction(db, () =>
    revisions.setIntent(db, {
      id: "op-rollback-01",
      source: "v3",
      kind: "rollback",
      label: "Ada",
      rollback_of: 1,
      documents: [{ path: "a.md", hash }],
    }),
  );
  const v3 = content("v3", doc("a.md", "something else\n"));
  transaction(db, () => revisions.ensureFlat(db, v3, next()));
  const e = entries(db);
  assert.deepEqual(
    e.map((x) => x.kind),
    ["created", "person", "rollback"],
  );
  assert.deepEqual(
    e[2].documents.map((d) => d.path),
    ["a.md"],
  );
  assert.equal(textOf(db, e[2], "a.md"), "old\n");
  assert.equal(revisions.getMeta(db, "intent"), null);
  assert.deepEqual(revisions.getMeta(db, "active").docs, ["a.md"]);
  // b.md was removed: an older worker can no longer write to it.
  assert.equal(revisions.removedByNewer(db, 2, "b.md"), true);
});

test("sync deletes an intent whose version never started", () => {
  const db = database(),
    c = content("v1", doc("a.md", "a\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  const hash = transaction(db, () => revisions.stage(db, "a\n"));
  transaction(db, () =>
    revisions.setIntent(db, {
      id: "op-never-001",
      source: "v9",
      kind: "rollback",
      label: "Ada",
      rollback_of: 1,
      documents: [{ path: "a.md", hash }],
    }),
  );
  const out = transaction(db, () => revisions.sync(db, c, generation));
  assert.equal(out.intent, null);
  assert.equal(revisions.getMeta(db, "intent"), null);
});

test("an older worker neither activates nor records", () => {
  const db = database(),
    v1 = content("v1", doc("a.md", "a\n"));
  const old = next();
  transaction(db, () => revisions.ensureFlat(db, v1, old));
  const v2 = content("v2", doc("a.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, v2, next()));
  const before = entries(db).length;
  transaction(db, () => assert.equal(revisions.ensureFlat(db, v1, old), false));
  assert.equal(revisions.stale(db, old), true);
  assert.equal(entries(db).length, before);
});

test("a rollback whose version merge reaches the history limit resets instead", () => {
  const db = database(),
    v1 = content("v1", doc("a.md", "old\n"));
  transaction(db, () => revisions.ensureFlat(db, v1, next()));
  personEdit(db, v1, "a.md", "edit\n");
  db.exec("UPDATE flats_docs_documents SET seq=100000 WHERE doc='a.md'");
  const hash = transaction(db, () => revisions.stage(db, "old\n"));
  transaction(db, () =>
    revisions.setIntent(db, {
      id: "op-rollback-02",
      source: "v4",
      kind: "rollback",
      label: "Ada",
      rollback_of: 1,
      documents: [{ path: "a.md", hash }],
    }),
  );
  // v4's source differs from the seed, so activating it merges first.
  transaction(db, () =>
    revisions.ensureFlat(db, content("v4", doc("a.md", "merge me\n")), next()),
  );
  assert.equal(entries(db).at(-1).kind, "rollback");
  const row = db.query(
    "SELECT seq FROM flats_docs_documents WHERE doc='a.md'",
  )[0];
  assert.equal(seedText(db, "a.md"), "old\n");
  assert.ok(row.seq < 100000);
});

test("a rollback recovers a version whose activation reaches a history limit", () => {
  const db = database(),
    v1 = content("v1", doc("a.md", "old\n"));
  transaction(db, () => revisions.ensureFlat(db, v1, next()));
  personEdit(db, v1, "a.md", "edit\n");
  db.exec("UPDATE flats_docs_documents SET seq=100000 WHERE doc='a.md'");
  const v2 = content("v2", doc("a.md", "saved\n"));
  const gen = next();
  // The new version cannot activate: its merge cannot append.
  assert.throws(
    () => transaction(db, () => revisions.ensureFlat(db, v2, gen)),
    /capacity/,
  );
  assert.equal(revisions.getMeta(db, "active").hash, "v1");
  // An intent can still be written, and a restore recovers the flat.
  const hash = transaction(db, () => revisions.stage(db, "old\n"));
  transaction(db, () =>
    revisions.setIntent(db, {
      id: "op-later-0001",
      source: "v9",
      kind: "agent",
      label: "Agent",
    }),
  );
  transaction(db, () =>
    revisions.restore(db, v2, gen, {
      id: "op-recover-01",
      label: "Ada",
      rollback_of: 1,
      documents: [{ path: "a.md", hash }],
    }),
  );
  const e = entries(db);
  assert.deepEqual(
    e.map((x) => x.kind),
    ["created", "person", "rollback"],
  );
  assert.equal(textOf(db, e[1], "a.md"), "old\nedit\n");
  assert.equal(e[1].source, "v1");
  assert.equal(textOf(db, e[2], "a.md"), "old\n");
  assert.equal(revisions.getMeta(db, "active").hash, "v2");
  assert.equal(revisions.getMeta(db, "intent"), null);
});

test("a text one document releases can be taken up by another in the same activation", () => {
  const db = database();
  transaction(db, () =>
    revisions.ensureFlat(
      db,
      content("v1", doc("a.md", "old A\n"), doc("b.md", "old B\n")),
      next(),
    ),
  );
  const store = revisions.getMeta(db, "store");
  transaction(db, () => revisions.ack(db, store, entries(db).at(-1).seq));
  // a.md moves on and b.md takes a.md's old text: recording a.md drops the
  // last reference to "old A" before b.md needs it.
  transaction(db, () =>
    revisions.ensureFlat(
      db,
      content("v2", doc("a.md", "new A\n"), doc("b.md", "old A\n")),
      next(),
    ),
  );
  const e = entries(db);
  assert.equal(textOf(db, e.at(-1), "b.md"), "old A\n");
  assert.equal(textOf(db, e.at(-1), "a.md"), "new A\n");
  transaction(db, () => revisions.ack(db, store, e.at(-1).seq));
  assert.deepEqual(refs(db), {
    [digest("new A\n")]: 1,
    [digest("old A\n")]: 1,
  });
});

// A person's update as the WebSocket path commits it, quota check included.
function applyBody(db, c, path, update) {
  const s = activate(
    db,
    c.documents.find((d) => d.path === path),
    c.hash,
    undefined,
    generation,
  );
  try {
    Y.applyUpdate(s.doc, update);
    append(db, s, "q:" + Math.random().toString(36).slice(2), base64(update));
    compact(db, s);
    revisions.touchGroup(db, "browser:a", "Ada");
    revisions.checkDiskQuota(db, { path, text: s.doc.getText("markdown") });
  } finally {
    s.doc.destroy();
  }
}
// editUpdate is the update that appends text to a document, or deletes its
// last remove characters.
function editUpdate(db, c, path, text, remove = 0) {
  const s = current(
    db,
    c.documents.find((d) => d.path === path),
    c.hash,
    undefined,
    generation,
  );
  try {
    const sv = Y.encodeStateVector(s.doc),
      t = s.doc.getText("markdown");
    if (remove) t.delete(t.length - remove, remove);
    else t.insert(t.length, text);
    return Y.encodeStateAsUpdate(s.doc, sv);
  } finally {
    s.doc.destroy();
  }
}
const checkedEdit = (db, c, path, text, remove = 0) => {
  const update = editUpdate(db, c, path, text, remove);
  transaction(db, () => applyBody(db, c, path, update));
};
const committed = (db, c, path) => {
  const s = current(
    db,
    c.documents.find((d) => d.path === path),
    c.hash,
    undefined,
    generation,
  );
  try {
    return s.doc.getText("markdown").toString();
  } finally {
    s.doc.destroy();
  }
};
const dbBytes = (db) =>
  db.query("PRAGMA page_count")[0].page_count *
  db.query("PRAGMA page_size")[0].page_size;
// The write-ahead log's bound at rest, which the check reserves: 64 frames.
const restWal = (db) =>
  32 + 64 * (db.query("PRAGMA page_size")[0].page_size + 24);
const MIN_WAL = 32 + 64 * (4096 + 24);
const DAY = 24 * 60 * 60 * 1000;
const budget = (db, quota, other = 0, ttl = DAY) =>
  transaction(db, () => revisions.setBudget(db, { quota, other, ttl_ms: ttl }));
const unimported = (db) =>
  db.query("SELECT coalesce(sum(size),0) AS n FROM flats_docs_unimported")[0].n;
// closeGroups closes every edit group, as the host's sync does when due.
const closeGroups = (db, c) =>
  transaction(db, () =>
    revisions.sync(db, c, generation, Date.now() + 600_000),
  );
const ackAll = (db, budget) => {
  const e = entries(db);
  transaction(db, () =>
    revisions.ack(db, revisions.getMeta(db, "store"), e.at(-1).seq, budget),
  );
};
const refused = (e) => e.code === "disk_quota";
// atRest checks that a database file's write-ahead log is within its bound
// after a commit.
const atRest = (db) =>
  assert.ok(
    walBytes(db) <= restWal(db),
    `log of ${walBytes(db)} bytes, over ${restWal(db)} at rest`,
  );
// fits checks that the recorded revisions, this database plus the blobs the
// host will store, and the write-ahead log (as it is, for a database file)
// fit the quota.
const fits = (db, quota) => {
  const wal = db.path ? walBytes(db) : MIN_WAL;
  assert.ok(
    dbBytes(db) + unimported(db) + wal <= quota,
    `${dbBytes(db)} + ${unimported(db)} + ${wal} > ${quota}`,
  );
  if (db.path) atRest(db);
};

test("live edits need a budget the host refreshed; quota 0 checks nothing", async () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  // No budget yet, or one that ran out: the edit fails as transient.
  const transient = (e) => e.transient === true && e.code === "unavailable";
  assert.throws(() => checkedEdit(db, c, "a.md", "edit\n"), transient);
  budget(db, 0, 0, 1);
  await new Promise((r) => setTimeout(r, 5));
  assert.throws(() => checkedEdit(db, c, "a.md", "edit\n"), transient);
  assert.equal(committed(db, c, "a.md"), "x\n");
  budget(db, 0);
  checkedEdit(db, c, "a.md", "edit\n");
  assert.equal(revisions.getMeta(db, "pending"), null);
  // A shorter lifetime replaces a longer one, even for the same budget.
  budget(db, 0, 0, 1);
  await new Promise((r) => setTimeout(r, 5));
  assert.throws(() => checkedEdit(db, c, "a.md", "edit\n"), transient);
  for (const bad of [
    { quota: -1, other: 0, ttl_ms: 1 },
    { quota: 1, other: 0 },
    { quota: 1, other: 0, ttl_ms: 0 },
    { quota: 1.5, other: 0, ttl_ms: 1 },
  ])
    assert.throws(
      () => transaction(db, () => revisions.setBudget(db, bad)),
      (e) => e.code === "invalid",
    );
});

test("an admitted person's edit always leaves room for the revision its group stores", () => {
  const db = database({ file: true }),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 0);
  ackAll(db);
  const big = "y".repeat(10_000);
  // The edit's own pages, measured in a transaction that is undone.
  budget(db, 1 << 30);
  const update = editUpdate(db, c, "a.md", big),
    undo = new Error("undo");
  let after = 0;
  assert.throws(
    () =>
      transaction(db, () => {
        applyBody(db, c, "a.md", update);
        after = dbBytes(db);
        throw undo;
      }),
    (e) => e === undo,
  );
  assert.ok(after > dbBytes(db), "the edit took no pages");
  assert.equal(committed(db, c, "a.md"), "x\n");
  // Reserving only twice the text is not enough: recording it takes pages
  // and rows besides the text, so that quota refuses the edit.
  const twice = MIN_WAL + after + 2 * (2 + big.length);
  budget(db, twice);
  assert.throws(() => checkedEdit(db, c, "a.md", big), refused);
  assert.equal(committed(db, c, "a.md"), "x\n");
  // At the smallest quota that admits it, closing its group still fits.
  let quota = twice;
  for (;;) {
    budget(db, (quota += 4096));
    try {
      checkedEdit(db, c, "a.md", big);
      break;
    } catch (e) {
      if (!refused(e)) throw e;
    }
  }
  closeGroups(db, c);
  assert.equal(revisions.getMeta(db, "pending"), null);
  assert.equal(unimported(db), 2 + big.length);
  fits(db, quota);
});

test("documents with one text count as separate copies", () => {
  // Four identical documents, edited alike (one shared text and blob),
  // then each differently: at the smallest quota admitting all four edits,
  // recording them still fits.
  const big = "y".repeat(400_000);
  const setup = () => {
    const db = database({ file: true }),
      c = content(
        "v1",
        ...["a", "b", "c", "d"].map((n) => doc(n + ".md", big)),
      );
    transaction(db, () => revisions.ensureFlat(db, c, next()));
    budget(db, 1 << 30);
    for (const d of c.documents) checkedEdit(db, c, d.path, "+");
    closeGroups(db, c);
    ackAll(db);
    return { db, c };
  };
  let { db, c } = setup();
  let quota = dbBytes(db) + MIN_WAL;
  for (;;) {
    budget(db, quota);
    try {
      for (const [i, d] of c.documents.entries())
        checkedEdit(db, c, d.path, "abcd"[i]);
      break;
    } catch (e) {
      if (!refused(e)) throw e;
      ({ db, c } = setup());
      quota += 256 * 1024;
    }
  }
  closeGroups(db, c);
  assert.equal(
    new Set(
      entries(db)
        .at(-1)
        .documents.map((d) => d.hash),
    ).size,
    4,
  );
  fits(db, quota);
});

test("text an update makes visible by completing an earlier one counts", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  // A remote replica inserts a character, then 200 KB after it; the
  // second update arrives first and stays pending (invisible).
  const remote = new Y.Doc();
  const s = current(db, c.documents[0], c.hash, undefined, generation);
  Y.applyUpdate(remote, Y.encodeStateAsUpdate(s.doc));
  s.doc.destroy();
  const t = remote.getText("markdown");
  let sv = Y.encodeStateVector(remote);
  t.insert(t.length, "p");
  const first = Y.encodeStateAsUpdate(remote, sv);
  sv = Y.encodeStateVector(remote);
  t.insert(t.length, "z".repeat(200_000));
  const second = Y.encodeStateAsUpdate(remote, sv);
  // An unrelated small edit, made before the pending update arrives (a
  // later local edit would carry the pending structs along).
  const control = editUpdate(db, c, "a.md", "q");
  transaction(db, () => applyBody(db, c, "a.md", second));
  assert.equal(committed(db, c, "a.md"), "x\n");
  // Room for the visible text and another small edit, but not for the
  // 200 KB the small first update makes visible.
  budget(db, MIN_WAL + dbBytes(db) + 400 * 1024);
  const undo = new Error("undo");
  assert.throws(
    () =>
      transaction(db, () => {
        applyBody(db, c, "a.md", control);
        throw undo;
      }),
    (e) => e === undo,
  );
  assert.throws(
    () => transaction(db, () => applyBody(db, c, "a.md", first)),
    refused,
  );
  assert.equal(committed(db, c, "a.md"), "x\n");
});

test("near the quota only the edited text is sized; others once", () => {
  const db = database({ file: true }),
    c = content("v1", doc("a.md", "a\n"), doc("b.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  checkedEdit(db, c, "b.md", "z".repeat(50_000));
  checkedEdit(db, c, "a.md", "y".repeat(300_000));
  assert.deepEqual(revisions.getMeta(db, "pending")["b.md"], {
    bytes: 3 * 50_002,
    exact: false,
  });
  // The smallest quota admitting one more character in a.md: there the
  // three-bytes-per-character bounds do not fit, the exact sizes do.
  let quota = dbBytes(db) + MIN_WAL;
  for (;;) {
    budget(db, (quota += 4096));
    try {
      checkedEdit(db, c, "a.md", "!");
      break;
    } catch (e) {
      if (!refused(e)) throw e;
    }
  }
  assert.deepEqual(revisions.getMeta(db, "pending")["b.md"], {
    bytes: 50_002,
    exact: true,
  });
  // A burst of small edits near the quota sizes the edited text in memory
  // each time and never loads the other document again.
  budget(db, (quota += 64 * 1024));
  const { sized, loaded } = revisions.quotaStats;
  for (let i = 0; i < 40; i++) checkedEdit(db, c, "a.md", "!");
  assert.equal(revisions.quotaStats.loaded, loaded);
  assert.ok(revisions.quotaStats.sized - sized <= 40);
  closeGroups(db, c);
  fits(db, quota);
});

test("the first check in a fresh worker uses the database's page size", async () => {
  // A module instance that has never read a page size, as in a newly
  // started worker VM.
  const fresh = await import(`./revisions.js?fresh=${Date.now()}`);
  const setup = () => {
    const db = database(),
      c = content("v1", doc("a.md", "a\n"));
    transaction(db, () => revisions.ensureFlat(db, c, next()));
    budget(db, 1 << 30);
    ackAll(db);
    checkedEdit(db, c, "a.md", "y".repeat(300_000));
    return db;
  };
  // The smallest quota at which the pending edit fits only once it is
  // sized exactly.
  const first = setup();
  let quota = dbBytes(first) + MIN_WAL;
  const { loaded } = revisions.quotaStats;
  for (;;) {
    budget(first, (quota += 4096));
    try {
      transaction(first, () => revisions.checkDiskQuota(first));
      break;
    } catch (e) {
      if (!refused(e)) throw e;
    }
  }
  assert.ok(revisions.quotaStats.loaded > loaded);
  // The same state, checked first by the fresh instance, is admitted too.
  const second = setup();
  budget(second, quota);
  transaction(second, () => fresh.checkDiskQuota(second));
  assert.equal(fresh.quotaStats.loaded, 1);
});

test("an edit undone before its group closes reserves nothing afterwards", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  checkedEdit(db, c, "a.md", "abc");
  checkedEdit(db, c, "a.md", "", 3);
  assert.equal(committed(db, c, "a.md"), "x\n");
  assert.ok(revisions.getMeta(db, "pending")["a.md"].bytes > 0);
  // The group closes with the text it started with: no entry, no reservation.
  const count = entries(db).length;
  closeGroups(db, c);
  assert.equal(entries(db).length, count);
  assert.equal(revisions.getMeta(db, "pending"), null);
});

test("an acknowledgement ends only its entries' reservations and installs the new budget", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  checkedEdit(db, c, "a.md", "first\n");
  closeGroups(db, c);
  checkedEdit(db, c, "a.md", "second\n");
  closeGroups(db, c);
  const e = entries(db);
  assert.equal(e.length, 3);
  assert.equal(unimported(db), 2 + 8 + 15);
  // A partial import acknowledges the first two entries only, with the
  // budget the host measured after storing their blobs.
  transaction(db, () =>
    revisions.ack(db, revisions.getMeta(db, "store"), e[1].seq, {
      quota: 1 << 30,
      other: 4242,
      ttl_ms: DAY,
    }),
  );
  assert.equal(unimported(db), 15);
  assert.equal(revisions.getMeta(db, "budget").other, 4242);
  ackAll(db);
  assert.equal(unimported(db), 0);
  // An invalid budget refuses the whole acknowledgement.
  assert.throws(
    () =>
      transaction(db, () =>
        revisions.ack(db, revisions.getMeta(db, "store"), 0, { quota: -1 }),
      ),
    (x) => x.code === "invalid",
  );
});

test("a budget ends the reservations of texts whose blobs the host already stored", () => {
  const db = database(),
    c = content("v1", doc("a.md", "a\n"), doc("b.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  checkedEdit(db, c, "a.md", "one\n");
  checkedEdit(db, c, "b.md", "two\n");
  closeGroups(db, c);
  const [entry] = entries(db),
    a = entry.documents.find((d) => d.path === "a.md"),
    b = entry.documents.find((d) => d.path === "b.md");
  assert.equal(unimported(db), a.size + b.size);
  // A partial import stored a's blob but not b's before it ran out of
  // budget: the entry stays unacknowledged, and the budget measured with
  // a's blob in place stops reserving it (an unknown hash changes nothing).
  transaction(db, () =>
    revisions.setBudget(db, {
      quota: 1 << 30,
      other: 100,
      ttl_ms: DAY,
      stored: [a.hash, "f".repeat(64)],
    }),
  );
  assert.equal(entries(db).length, 1);
  assert.equal(unimported(db), b.size);
  ackAll(db, { quota: 1 << 30, other: 200, ttl_ms: DAY, stored: [b.hash] });
  assert.equal(unimported(db), 0);
  for (const stored of [["x"], "nope", Array(10_001).fill("f".repeat(64))])
    assert.throws(
      () =>
        transaction(db, () =>
          revisions.setBudget(db, { quota: 1, other: 0, ttl_ms: 1, stored }),
        ),
      (e) => e.code === "invalid",
    );
});

test("a quota that starts reserves what people already edited; a rollback is never refused", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"), doc("b.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 0);
  personEdit(db, c, "b.md", "edited\n");
  budget(db, 1 << 30);
  assert.deepEqual(revisions.getMeta(db, "pending"), {
    "b.md": { bytes: 9, exact: true },
  });
  budget(db, 1);
  transaction(db, () =>
    revisions.restore(db, c, generation, {
      id: "op-quota-0001",
      label: "Ada",
      rollback_of: 1,
      documents: [
        { path: "a.md", hash: digest("x\n") },
        { path: "b.md", hash: digest("b\n") },
      ],
    }),
  );
  assert.equal(committed(db, c, "b.md"), "b\n");
});

// committedLog commits fn in a transaction of its own (without the
// truncation after a commit) and returns the bytes of log it wrote.
function committedLog(db, fn) {
  const before = walBytes(db);
  db.exec("BEGIN IMMEDIATE");
  fn();
  db.exec("COMMIT");
  return walBytes(db) - before;
}

test("recording rewrites no large text to count its references", () => {
  // A tiny document next to ten large ones, all recorded and imported.
  const db = database({ file: true }),
    c = content(
      "v1",
      doc("tiny.md", "t\n"),
      ...Array.from({ length: 10 }, (_, i) =>
        doc(`big${i}.md`, String(i).repeat(400_000)),
      ),
    );
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  atRest(db);
  const quota = dbBytes(db) + 1_200_000;
  budget(db, quota);
  checkedEdit(db, c, "tiny.md", "!");
  atRest(db);
  // Closing the group references every document's text once more; that
  // changes small reference rows only.
  const wrote = committedLog(db, () =>
    revisions.sync(db, c, generation, Date.now() + 600_000),
  );
  assert.ok(wrote < 32 * 4120, `closing the group wrote ${wrote} bytes`);
  transaction(db, () => {});
  fits(db, quota);
});

test("an update rewrites no large seed text", () => {
  // A 1 MB document: each update advances its seq and chain, which live
  // apart from its seed text.
  const db = database({ file: true }),
    c = content("v1", doc("a.md", "s".repeat(1_000_000)));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  atRest(db);
  for (let i = 0; i < 3; i++) {
    const update = editUpdate(db, c, "a.md", "!");
    const wrote = committedLog(db, () => applyBody(db, c, "a.md", update));
    assert.ok(wrote < 16 * 4120, `update ${i} wrote ${wrote} bytes of log`);
  }
  transaction(db, () => {});
  atRest(db);
});

test("the write-ahead log stays within its bound at rest, also with small pages", () => {
  for (const pageSize of [512, 4096]) {
    const db = database({ file: true, pageSize }),
      c = content("v1", doc("a.md", "s".repeat(300_000)));
    assert.equal(db.query("PRAGMA page_size")[0].page_size, pageSize);
    transaction(db, () => revisions.ensureFlat(db, c, next()));
    atRest(db);
    budget(db, 1 << 30);
    for (let i = 0; i < 70; i++) {
      // Compactions, recorded revisions and acknowledgements among them.
      checkedEdit(db, c, "a.md", "y".repeat(i % 10 ? 10 : 50_000));
      atRest(db);
      if (i % 20 === 19) {
        closeGroups(db, c);
        atRest(db);
        ackAll(db);
        atRest(db);
      }
    }
  }
});

test("an older database moves its text references into their own table", () => {
  const db = database({ schema: false });
  db.exec(
    "CREATE TABLE flats_docs_texts(hash TEXT PRIMARY KEY, text TEXT NOT NULL, refs INTEGER NOT NULL, staged_at INTEGER)",
  );
  db.exec("INSERT INTO flats_docs_texts VALUES('a','A',2,NULL),('b','B',0,5)");
  schema(db);
  assert.deepEqual(
    db.query(
      "SELECT hash,refs,staged_at FROM flats_docs_text_refs ORDER BY hash",
    ),
    [
      { hash: "a", refs: 2, staged_at: null },
      { hash: "b", refs: 0, staged_at: 5 },
    ],
  );
  // The staged text stays until its staging expires, as before.
  transaction(db, () => revisions.dropUnused(db));
  assert.equal(revisions.textOf(db, "b"), "B");
  // A second run changes nothing.
  schema(db);
  assert.equal(
    db.query("SELECT count(*) AS n FROM flats_docs_text_refs")[0].n,
    2,
  );
});

test("an older database moves its seed texts into their own table", () => {
  const db = database({ schema: false });
  db.exec(
    "CREATE TABLE flats_docs_documents(doc TEXT PRIMARY KEY, epoch TEXT NOT NULL, seq INTEGER NOT NULL, chain TEXT NOT NULL, seed_hash TEXT NOT NULL, seed_text TEXT NOT NULL, updated_at INTEGER NOT NULL)",
  );
  db.exec(
    "INSERT INTO flats_docs_documents VALUES('a.md','e',3,'c','h','base\n',1)",
  );
  schema(db);
  assert.equal(seedText(db, "a.md"), "base\n");
  assert.equal(
    db.query("SELECT seed_text FROM flats_docs_documents")[0].seed_text,
    "",
  );
  schema(db);
  assert.equal(seedText(db, "a.md"), "base\n");
});

test("small commits stay in the log until a truncation copies them at once", () => {
  const db = database({ file: true }),
    c = content("v1", doc("a.md", "s".repeat(1000)));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  let kept = 0,
    truncated = 0;
  for (let i = 0; i < 40; i++) {
    checkedEdit(db, c, "a.md", "y");
    atRest(db);
    const { log, checkpointed } = db.query("PRAGMA wal_checkpoint(NOOP)")[0];
    assert.ok(log <= WAL_REST_FRAMES, `${log} frames at rest`);
    // Either the commit's frames are still only in the log, or this commit
    // truncated it; copying each small commit on its own costs syncs.
    if (log === 0) truncated++;
    else {
      assert.equal(checkpointed, 0, `edit ${i} copied ${checkpointed} frames`);
      kept++;
    }
  }
  assert.ok(kept > 0 && truncated > 0, `kept ${kept}, truncated ${truncated}`);
});

test("a truncation a reader blocked is retried until it succeeds", () => {
  const db = database({ file: true }),
    c = content("v1", doc("a.md", "s".repeat(1000)));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  budget(db, 1 << 30);
  ackAll(db);
  atRest(db);
  // A reader takes the newest snapshot right after a large transaction
  // commits, before the frame count is read: the truncation can still copy
  // all of it, but it is busy.
  const reader = new DatabaseSync(db.path);
  db.onQuery = (sql) => {
    if (sql !== "PRAGMA wal_checkpoint(NOOP)") return;
    db.onQuery = null;
    reader.exec("BEGIN");
    reader.prepare("SELECT count(*) AS n FROM flats_docs_meta").get();
  };
  checkedEdit(db, c, "a.md", "y".repeat(200_000));
  assert.equal(db.onQuery, null, "the reader was never started");
  assert.ok(walBytes(db) > restWal(db), "the large transaction left no log");
  reader.exec("COMMIT");
  // SQLite may restart a fully copied log for the next writer while its
  // file keeps its size; a restarting checkpoint from another connection
  // makes that happen here.
  reader.exec("PRAGMA wal_checkpoint(RESTART)");
  reader.close();
  // The next transaction is small and writes from the log's start, so its
  // frame count is small; the owed truncation still runs.
  checkedEdit(db, c, "a.md", "!");
  atRest(db);
  assert.equal(revisions.getMeta(db, "wal_truncate"), null);
});

test("a truncation's success keeps a failure another VM records meanwhile", () => {
  // Two VMs on one database file.
  const a = database({ file: true }),
    c = content("v1", doc("a.md", "s".repeat(1000)));
  transaction(a, () => revisions.ensureFlat(a, c, next()));
  budget(a, 1 << 30);
  ackAll(a);
  const b = database({ file: true, at: a.path });
  a.exec("INSERT INTO flats_docs_meta VALUES('wal_truncate','1')");
  // A's owed truncation succeeds; before A clears the marker, B commits a
  // large transaction whose own truncation a reader blocks.
  const reader = new DatabaseSync(a.path);
  let started = false;
  a.onExec = (sql) => {
    if (!sql.startsWith("DELETE FROM flats_docs_meta WHERE key='wal_truncate'"))
      return;
    a.onExec = null;
    b.onQuery = (q) => {
      if (q !== "PRAGMA wal_checkpoint(NOOP)") return;
      b.onQuery = null;
      started = true;
      reader.exec("BEGIN");
      reader.prepare("SELECT count(*) AS n FROM flats_docs_meta").get();
    };
    checkedEdit(b, c, "a.md", "y".repeat(200_000));
    reader.exec("COMMIT");
  };
  transaction(a, () => {});
  // commit() ignores errors thrown while it truncates, so check here that
  // the blocked truncation really ran.
  assert.ok(started, "the reader was never started");
  // B's failure is still owed.
  assert.notEqual(revisions.getMeta(a, "wal_truncate"), null);
  reader.exec("PRAGMA wal_checkpoint(RESTART)");
  reader.close();
  checkedEdit(a, c, "a.md", "!");
  atRest(a);
  assert.equal(revisions.getMeta(a, "wal_truncate"), null);
});

test("an older database reserves the blobs its unimported entries still need", () => {
  const db = database({ schema: false });
  db.exec(
    "CREATE TABLE flats_docs_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)",
  );
  db.exec(
    "CREATE TABLE flats_docs_outbox(seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL, kind TEXT NOT NULL, authors TEXT NOT NULL, message TEXT NOT NULL, rollback_of INTEGER, op TEXT, source TEXT NOT NULL, documents TEXT NOT NULL)",
  );
  // A backlog of many entries of 128 documents, read one entry at a time.
  const hash = (e, d) =>
    (e.toString(16).padStart(8, "0") + d.toString(16).padStart(8, "0")).repeat(
      4,
    );
  for (let e = 0; e < 300; e++)
    db.exec(
      "INSERT INTO flats_docs_outbox(created_at,kind,authors,message,source,documents) VALUES(1,'person','[]','','v1',?)",
      [
        JSON.stringify(
          Array.from({ length: 128 }, (_, d) => ({
            path: `d${d}.md`,
            hash: hash(e % 150, d),
            size: 10 + d,
          })),
        ),
      ],
    );
  schema(db);
  const rows = db.query(
    "SELECT count(*) AS n, sum(size) AS bytes FROM flats_docs_unimported",
  )[0];
  assert.equal(rows.n, 150 * 128);
  assert.equal(rows.bytes, 150 * (128 * 10 + (127 * 128) / 2));
});

test("a truncation's success never clears a later failure's generation", () => {
  // Three VMs on one database file.
  const a = database({ file: true }),
    c = content("v1", doc("a.md", "s".repeat(1000)));
  transaction(a, () => revisions.ensureFlat(a, c, next()));
  budget(a, 1 << 30);
  ackAll(a);
  const b = database({ file: true, at: a.path }),
    v = database({ file: true, at: a.path });
  // A failure is owed. A reads it and truncates; before A clears it, B
  // truncates and clears it, then C's own truncation is blocked and owes a
  // new failure. A's delayed clearing must leave C's.
  a.exec("INSERT INTO flats_docs_meta VALUES('wal_generation','1')");
  a.exec("INSERT INTO flats_docs_meta VALUES('wal_truncate','1')");
  const reader = new DatabaseSync(a.path);
  let started = false;
  a.onExec = (sql) => {
    if (!sql.startsWith("DELETE FROM flats_docs_meta WHERE key='wal_truncate'"))
      return;
    a.onExec = null;
    transaction(b, () => {});
    assert.equal(revisions.getMeta(b, "wal_truncate"), null);
    v.onQuery = (q) => {
      if (q !== "PRAGMA wal_checkpoint(NOOP)") return;
      v.onQuery = null;
      started = true;
      reader.exec("BEGIN");
      reader.prepare("SELECT count(*) AS n FROM flats_docs_meta").get();
    };
    checkedEdit(v, c, "a.md", "y".repeat(200_000));
    reader.exec("COMMIT");
  };
  transaction(a, () => {});
  // commit() ignores errors thrown while it truncates, so check here that
  // the blocked truncation really ran.
  assert.ok(started, "the reader was never started");
  assert.equal(revisions.getMeta(a, "wal_truncate"), 2);
  reader.exec("PRAGMA wal_checkpoint(RESTART)");
  reader.close();
  checkedEdit(a, c, "a.md", "!");
  atRest(a);
  assert.equal(revisions.getMeta(a, "wal_truncate"), null);
  assert.equal(revisions.getMeta(a, "wal_generation"), 2);
});
