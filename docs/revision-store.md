# Revision store

Documents (`docs`) and designs (`design`) follow the Revision model of
[content-models.md](spec/content-models.md): no Draft or user versions, every
edit unit becomes a whole-flat revision, with revision and pin quotas and
rollback. This page describes the store that holds those revisions for both
types. It is owned by `internal/core` (`design.go`, `docsrev.go`) with tables
in `internal/store/content.go`.

- A **design** writes the store directly: its current files ("head") and its
  open edit groups live here, and every write commits a revision here.
- A **document** keeps its live text in its docs worker. The worker records
  each revision boundary in an outbox in the same database transaction as the
  change, and the host imports the outbox into this store. The worker side
  (outbox, intents, text restore, host operations) is in
  [the docs app internals](internal/docs-app.md).

Both types also keep internal backing versions under `versions/<n>` (a
design's one-file shell, each docs bundle that ran) so the running instance
has a version to serve. They are not user versions: version tools refuse
these types, and pruning removes their files (the row stays, marked pruned)
except for the live bundle, the newest other one and any bundle an open
preview or a pending activation or rollback approval names.

## Schema

Migration 11 adds the first five tables; migration 12 adds the document
columns and the last two tables. Every table references `flats(slug)` (or
`revisions`) with `ON DELETE CASCADE ON UPDATE CASCADE`, so rename and delete
carry the whole history and nothing in memory needs moving.

| Table | Holds |
| --- | --- |
| `content_state` | One row per Revision-model flat: `type` (`design` or `docs`), `phase` (`init` or `ready`), `seq` (bumped on every head change), `gen` (flat-wide file generation counter, never reused), `revision_quota` and `pin_quota` (per-flat overrides, NULL = host default), and the document import cursor `import_store`, `import_seq`. |
| `content_head` | A design's current files: `path`, `hash`, `size`, `gen`. |
| `content_groups` | A design's open edit groups, one per editor: `label`, `started_at`, `last_at`. |
| `revisions` | `(flat, number)` with `created_at`, `kind`, `authors` (JSON), `message`, `rollback_of`, `pinned`, `changes` (JSON `{added, changed, removed}` against the previous revision), `files`, `size` and, for documents, `source`. |
| `revision_files` | The files of a revision. For a document only its Markdown documents; its other files are named by `revisions.source`. |
| `content_sources` | `(flat, source, path, hash, size)`: the non-document files of each docs bundle the host started, keyed by its content hash. A bundle without such files has one row with an empty path. |
| `content_holds` | `(flat, op, number)`: the target revision of each document rollback in progress. |

`content_state.type` is the durable type marker of a Revision-model flat. A
design has it from the first step of its creation, before its shell is live,
so a racing first save of another type is refused. A document's row (phase
`ready`) is created by its first import. Documents leave `content_head` and
`content_groups` empty: the newest revision references what the head would,
and edit groups live in the worker.

Revision kinds are `created`, `agent`, `person` and `rollback`, plus
`recovered` for document text migrated from the retired
`flats_docs_conflicts` records. A revision's `files` and `size` count all of
its files, a document's source included.

**Numbers.** The next revision number is `max(number) + 1`, or 1. Retention
never deletes the newest revision, so a number is never reused.
`content_state` keeps no revision counter.

## Blobs

File bytes are content-addressed by SHA-256 under the flat's directory:

```text
flats/<slug>/design/blobs/<hh>/<sha256>      design blobs, mode 0444
flats/<slug>/design/tmp/                     design staging
flats/<slug>/revisions/blobs/<hh>/<sha256>   document blobs, mode 0444
flats/<slug>/revisions/tmp/                  document staging
```

Because they live under the flat directory, rename, delete, backups and the
per-flat disk quota cover them unchanged. Storing a blob:

1. Reuse an existing blob with the same hash.
2. Otherwise create any missing `tmp` and `blobs/<hh>` directory, syncing the
   parent after each new entry.
3. Write the bytes to a new file in `tmp/`, `fsync` it and make it read-only.
4. Rename it to `blobs/<hh>/<hash>`, then `fsync` `blobs/<hh>`.

A transaction may reference a hash only after its blob is stored. Blobs never
change once written and are deleted only by the [sweep](#blob-sweep).

## Writes

Every write runs under the flat's lock, the one rename, delete and lifecycle
actions take; writes that change storage also refuse while an unfinished
rename or delete journal names the slug. Each write:

1. stores and syncs its new blobs;
2. runs one transaction on the store's single connection for head, groups,
   revisions and retention;
3. notifies the live instance after the commit.

`commitTx(kind, authors, message, rollbackOf)` turns the current head into the
next revision: it copies the head rows, computes `changes` and truncates the
message to 500 characters. It never closes groups and never runs retention:
each caller decides which groups close and runs `retainTx` afterwards in the
same transaction.

**Design agent write** (`WriteDesign`, one `save_design` call). An absent slug
creates the design, and an untyped flat (created empty, or left by an
interrupted creation) becomes one. An optional `expected_revision` is checked
before any blob is stored and again in the transaction; it is refused with
`conflict` when it is not the latest revision or when people's edits are not
yet a revision. In the transaction:

1. When the head differs from the latest revision, a `person` revision
   crediting every open group keeps the state before the write; then every
   group closes.
2. Apply the files and deletes. A file whose content changes gets a fresh
   value of the flat's generation counter.
3. `commitTx(agent)`, then `retainTx()`.

**Person operation** (`ApplyDesignOp`, from the canvas). The operation's
guards are checked against the head, its new blobs are stored, and the
transaction writes the head, bumps `seq` and opens or extends the editor's
group (keeping `started_at`, updating `last_at`).

**Group closing.** A ticker runs every 5 seconds, and once at startup. A group
is due when its editor has been idle for 30 seconds or the group began 5
minutes ago. For each due group, under the flat lock, a head that differs from
the latest revision becomes a `person` revision crediting every editor with an
open group; then only the due group closes. Other editors keep their groups,
and their later edits go into later revisions.

**Design rollback to R** (`RollbackRevision`), all in one transaction:

1. Load R's file rows; a missing R is `not_found`.
2. Close every group, keeping a differing head as a `person` revision.
3. Replace the head with R's rows. Every restored file gets a fresh
   generation, even when its bytes did not change, so any operation prepared
   before the rollback is refused.
4. `commitTx(rollback, rollbackOf=R)`, then `retainTx()`.

R cannot be deleted before it is copied, and a rollback stores no blob.

**Creating a design** (`CreateDesign`, under the flat lock):

1. Validate the files, create the flat row and store the blobs. A failure
   removes the new row and directory.
2. In one transaction write `content_state` (phase `init`), the head and
   revision 1 (`created`).
3. Save a Draft holding only `flats.json` `{"type":"design","name":...}` and
   make it live at once as an internal version (the shell).
4. Set phase `ready`.

A crash between steps 1 and 2 leaves an untyped flat, which the next
`save_design` adopts. At startup every row still in `init` finishes steps 3
and 4 under its flat lock. No other path writes the shell.

## Quotas and retention

The revision quota is the flat's override, else `system.revision_quota`
(default 100); the pin quota is the override, else `system.pin_quota`
(default 20). Both are counts, 0 means unlimited.

`retainTx` counts the revisions that are neither pinned nor held, the newest
included, and deletes the oldest of them while the count exceeds the revision
quota. It never deletes the newest revision, so a quota of 1 keeps exactly
one. Retention runs:

- in the transaction of every committed design write, group closing and
  rollback, and after every document import that added revisions;
- after an unpin;
- after a per-flat override changes, for that flat;
- after `system.revision_quota` changes, for every document and design, each
  under its flat lock, before the settings call returns;
- at startup: designs right away, documents after their first import.

Pinning while the pin count has reached the pin quota is refused with
`conflict`; Flats never unpins by itself. A lowered pin quota keeps every pin.
A retention pass that deleted revisions starts a blob sweep of that flat.

## Reading

Reads of file bytes (revision files, design snapshots of the head or of a
revision, exports) select their rows and take an in-memory read lease on those
hashes while holding the flat lock, which the sweep also takes. They then release the
lock, read the bytes, and drop the leases when the handler returns, so a zip
export or a revision view is one coherent snapshot and no selected blob is
swept in between. A design's head snapshot reads the head, the latest
revision and `seq` in one transaction. A document revision's file list
expands its source. Lists are newest first, at most 100 per page, with `next`.

A document's history read, pin, quota read or quota change first imports its
outbox (at most every 2 seconds per flat), so it sees recent revisions.

## Documents

### Sources

When a docs version is built on the live data directory, the host stores
that bundle's non-Markdown files as blobs and records them in
`content_sources` under the bundle's content hash, before the worker can name
that source. An outbox entry lists its documents and names everything else by
`source`. A source without rows is rebuilt from a stored, unpruned version or
the Draft with that hash.

### Importing

`importDocsLocked` runs under the flat lock against the running docs
instance, through the worker's host operations:

1. When a cursor exists, acknowledge through `import_seq` first; this repairs
   an acknowledgement a crash lost. An outbox with another store id starts
   from 0: the first import, or a replaced worker database.
2. Read outbox pages after the cursor until the outbox is empty or 64 MiB of
   fetched text is spent.
3. For each entry, fetch each missing document text, check it against its
   hash and store its blob. When the budget runs out, stop: that entry and
   later ones wait for the next call, which finds the blobs already stored.
4. For the entries whose blobs and source are complete, one transaction
   inserts each as the next revision (kind, authors, message, `rollback_of`,
   time, documents and source; `changes` against the previous revision, both
   expanded), deletes the hold whose id is the entry's `op`, and advances the
   cursor. Entries the cursor already passed are skipped. Then acknowledge.
5. When revisions were added, run retention.

An entry whose source cannot be resolved stops the import with an event; the
worker keeps it and later entries.

### When documents sync and import

- **Saves** (`save_document`, `create_flat {type: docs}`, `flats
  save-document`, the REST document upload): when a docs version runs and
  the save starts a new one, the host first writes an `agent` intent with the
  agent's label and message through the running worker. Then the save
  applies, and the host syncs and imports. A save that cannot record (the
  worker refuses or is unreachable after the switch) still succeeded; an
  event says so and the ticker imports later.
- **`update_document`**: the edit carries a label and an op id; after a
  changed edit the host imports, and the result reports the revision when
  that import found the edit's entry.
- **Ticker**: every 15 seconds each running document whose lock is free is
  synced and imported. Import also runs when sync failed, so the worker's
  transport routes drain the backlog while an activation keeps failing. At
  startup every running document is synced, imported (which also migrates
  it) and then retained.

### Document rollback to n

`RollbackRevision` on a document runs under the flat lock with a new op id:

1. **Hold** n: insert a `content_holds` row in one transaction, importing
   first only when n does not exist yet. Retention skips held revisions
   without counting them as pins.
2. Refuse with `invalid` a revision of more than 128 documents (only a
   migrated recovery revision can hold one); its texts stay readable.
3. Sync and import. The document must be running.
4. Stage every document text of n on the worker.
5. When n's documents and non-document files have the paths and source of
   the running version, call `restore`: one worker transaction restores
   every document and records the `rollback` entry. Its deadline is 12
   seconds; when the response is lost, the host asks `op?id=` after the
   deadline whether it committed.
6. Otherwise write a `rollback` intent, rebuild n's bundle from its blobs and
   start it as a new internal version. This path skips the per-flat disk
   quota and the upload-only limit of 4 MiB of Markdown per bundle; every
   file keeps its own limits. The new version's first activation restores
   the texts and records the entry in one transaction, at startup at the
   latest after a crash.
7. Sync and import; the result is the imported rollback revision, or
   `unavailable` when it is recorded but not imported yet.

**Releasing a hold.** Importing the rollback's entry deletes its hold in the
same transaction, so retention can delete n only after the rollback revision
exists. After every sync, each hold that is not in progress in this process
and is not the worker's remaining intent is checked with `op?id=`: an
operation that did not commit is released. A committed rollback keeps its
worker receipt until its entry is imported, so that answer is definitive.

## Blob sweep

Blobs are never deleted inline. A sweep runs every 10 minutes for every
document and design, and for one flat after a retention pass deleted
revisions. For each flat it:

1. takes the flat lock, refusing (skipping the flat) while a rename or delete
   journal names the slug;
2. for a document: imports first and skips the flat when the document is not
   running, the import failed or entries remain in the outbox, since a
   pending entry may name blobs no revision references yet. It then drops
   sources that no revision names and that are not the running version, an
   unpruned stored version or the Draft;
3. runs `PRAGMA wal_checkpoint(FULL)` and skips the flat unless the
   checkpoint completed, so the committed metadata that dropped a reference
   is durable before a blob it referenced is deleted;
4. deletes every blob that is not referenced by `content_head`,
   `revision_files` or `content_sources`, not leased, and older than 10
   minutes, so a blob stored for a transaction still in flight is never
   taken;
5. deletes `tmp/` entries older than 10 minutes.

The sweep also reclaims blobs left by failed writes and crashes.

## Disk quota

A design write or person operation (a design holds at most 512 files, each
file and each agent write at most `system.upload_max_bytes`) checks the flat directory's size plus its
new unique blob bytes against the per-flat disk quota and is refused with a
validation error. A document save checks the flat directory plus the whole
bundle. A rollback of either type adds no bytes it is charged for and is never
refused for disk quota. Live edits to a document (people's edits and
`update_document`) and the revisions recording them are not checked against
the disk quota; see the Current state of
[content-models.md](spec/content-models.md#current-state).
