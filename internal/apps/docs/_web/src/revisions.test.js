import { test } from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import * as Y from "yjs";
import {
  schema,
  current,
  transaction,
  activate,
  append,
  digest,
} from "./storage.js";
import { base64 } from "./protocol.js";
import * as revisions from "./revisions.js";

// The worker's env.DB shape over an in-memory SQLite database.
function database() {
  const raw = new DatabaseSync(":memory:");
  const db = {
    exec(sql, params) {
      if (!params) raw.exec(sql);
      else raw.prepare(sql).run(...params);
    },
    query(sql, params = []) {
      return raw
        .prepare(sql)
        .all(...params)
        .map((r) => ({ ...r }));
    },
  };
  schema(db);
  return db;
}

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
      .query("SELECT hash,refs FROM flats_docs_texts")
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
  assert.equal(
    db.query("SELECT seed_text FROM flats_docs_documents WHERE doc='a.md'")[0]
      .seed_text,
    "one\n",
  );
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
    "SELECT epoch,seq,seed_text FROM flats_docs_documents WHERE doc='a.md'",
  )[0];
  assert.notEqual(row.epoch, epoch);
  assert.equal(row.seed_text, "one\n");
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
    "SELECT seq,seed_text FROM flats_docs_documents WHERE doc='a.md'",
  )[0];
  assert.equal(row.seed_text, "old\n");
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

// A person's edit as the WebSocket path commits it, quota check included.
function checkedEdit(db, c, path, text) {
  transaction(db, () => {
    const s = activate(
      db,
      c.documents.find((d) => d.path === path),
      c.hash,
      undefined,
      generation,
    );
    const sv = Y.encodeStateVector(s.doc),
      t = s.doc.getText("markdown");
    t.insert(t.length, text);
    append(
      db,
      s,
      "q:" + Math.random().toString(36).slice(2),
      base64(Y.encodeStateAsUpdate(s.doc, sv)),
    );
    revisions.touchGroup(db, "browser:a", "Ada");
    try {
      revisions.checkDiskQuota(db, { path, text: t });
    } finally {
      s.doc.destroy();
    }
  });
}
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
const budget = (db, quota, other = 0) =>
  transaction(db, () => revisions.setBudget(db, { quota, other }));

test("without a disk quota nothing is reserved or refused", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  checkedEdit(db, c, "a.md", "edit\n");
  assert.equal(revisions.getMeta(db, "pending"), null);
  assert.equal(revisions.getMeta(db, "budget"), null);
});

test("a person's edit reserves its future revision and is refused when it would not fit", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  // The created entry is not imported yet: its texts count once more.
  assert.equal(revisions.getMeta(db, "unimported"), 2);
  transaction(db, () => revisions.ack(db, revisions.getMeta(db, "store"), 1));
  assert.equal(revisions.getMeta(db, "unimported"), null);
  budget(db, dbBytes(db) + 1024 * 1024, 4096);
  checkedEdit(db, c, "a.md", "small\n");
  // The edited document will be stored twice when its group closes; far
  // from the quota, its size is the bound of three bytes per character.
  assert.deepEqual(revisions.getMeta(db, "pending"), {
    "a.md": { bytes: 24, exact: false },
  });
  // An edit whose document would no longer fit twice is refused, and
  // nothing of it commits.
  budget(db, dbBytes(db) + 4096 + 2 * 8 + 100, 4096);
  assert.throws(
    () => checkedEdit(db, c, "a.md", "y".repeat(100)),
    (e) =>
      e.code === "disk_quota" && /over its \d+-byte disk quota/.test(e.message),
  );
  assert.equal(committed(db, c, "a.md"), "x\nsmall\n");
  assert.deepEqual(revisions.getMeta(db, "pending"), {
    "a.md": { bytes: 24, exact: false },
  });
  // Near the quota the exact size counts: this edit's bound would not fit,
  // its exact size does (with room for the pages its update takes).
  const big = "y".repeat(100_000),
    exact = 2 * (8 + big.length);
  budget(db, 4096 + dbBytes(db) + exact + 256 * 1024, 4096);
  assert.ok(2 * 3 * (8 + big.length) > exact + 256 * 1024);
  checkedEdit(db, c, "a.md", big);
  assert.deepEqual(revisions.getMeta(db, "pending"), {
    "a.md": { bytes: 8 + big.length, exact: true },
  });
  // Recording the group moves the reservation to the import.
  transaction(db, () => revisions.sync(db, c, generation, Date.now() + 60_000));
  assert.equal(revisions.getMeta(db, "pending"), null);
  assert.equal(revisions.getMeta(db, "unimported"), 8 + 100_000);
  // Turning the quota off drops the reservations it kept.
  budget(db, 0);
  assert.equal(revisions.getMeta(db, "budget"), null);
  checkedEdit(db, c, "a.md", "y".repeat(100));
});

test("a quota that starts reserves what people already edited", () => {
  const db = database(),
    c = content("v1", doc("a.md", "x\n"), doc("b.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  personEdit(db, c, "b.md", "edited\n");
  budget(db, 1 << 30);
  assert.deepEqual(revisions.getMeta(db, "pending"), {
    "b.md": { bytes: 9, exact: true },
  });
  // A rollback is never refused, even over the quota.
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
  assert.throws(
    () => budget(db, -1),
    (e) => e.code === "invalid",
  );
});

test("near the quota every pending document's size becomes exact", () => {
  const db = database(),
    c = content("v1", doc("a.md", "a\n"), doc("b.md", "b\n"));
  transaction(db, () => revisions.ensureFlat(db, c, next()));
  transaction(db, () => revisions.ack(db, revisions.getMeta(db, "store"), 1));
  budget(db, 1 << 30);
  checkedEdit(db, c, "b.md", "z".repeat(50_000));
  assert.deepEqual(revisions.getMeta(db, "pending")["b.md"], {
    bytes: 3 * 50_002,
    exact: false,
  });
  // Only exact sizes fit: b's bound alone would exceed the quota.
  budget(db, dbBytes(db) + 2 * (50_002 + 3) + 128 * 1024);
  checkedEdit(db, c, "a.md", "!");
  assert.deepEqual(revisions.getMeta(db, "pending"), {
    "a.md": { bytes: 3, exact: true },
    "b.md": { bytes: 50_002, exact: true },
  });
});
