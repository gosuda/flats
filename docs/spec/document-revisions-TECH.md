# Document revisions: technical design

Behavior: the Revision model in [content-models.md](content-models.md).
Store: the revision store of [design-type-TECH.md](design-type-TECH.md),
reused for documents.

## Goal

Documents (`type: "docs"`) get revisions as the Revision model requires:

- whole-flat revisions, created by agent writes, by people's edit groups and
  by rollbacks, in the shared revision store with its quotas, pins and
  retention;
- listing, viewing (with the revision's own assets), pinning and rolling back
  inside the document page, and through the MCP revision tools;
- `flats_docs_conflicts` replaced by revisions, including a migration of the
  records that exist today.

This is a milestone of the Revision model, not all of it. These rules of
content-models stay open and are listed in its "Current state":

- rule 8: single-document saves now preserve sibling files, manifest settings
  and the existing entry. Explicit whole-bundle replacement still uses
  `save_version`; moving it to a Revision-model tool remains a separate change;
- quota rule 3: a person's live edit is not checked against the disk quota,
  as today.

## Where history is captured

A document's live text is a Yjs document per Markdown file in the docs
worker's SQLite database. People edit it through the worker, which commits
each update in its own transaction; the host never sees those updates. The
manifest and assets are files of the running docs version, on the host.

- **The worker captures revisions.** Every revision boundary (an agent write,
  a closing edit group, a rollback, a new version's merge) happens inside one
  worker transaction, the same one that changes the text. The worker records
  the whole flat at that boundary in an **outbox** in its database. Nothing
  can commit between the capture and the change, and a crash keeps both or
  neither.
- **The host keeps the history.** It imports outbox entries in order and
  idempotently into the design revision store (`revisions`,
  `revision_files`, content-addressed blobs) and acknowledges them, after
  which the worker drops them. Quotas, retention, pins, the blob sweep, the
  console settings, MCP and the page's history all read the host store.
- **Operations that span a version switch are durable in the worker.** A save
  or a rollback that starts a new version first writes an *intent* into the
  worker database (which the old and the new version share). The new
  version's activation consumes it in its own transaction, so the operation
  completes atomically whenever the new version first runs, even after a
  host crash.

## Worker side (`internal/apps/docs/_web/src`)

### Tables

Created idempotently with the existing ones, all `flats_docs_` prefixed:

```sql
meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)
  -- 'store':  random id of this database's outbox, set once
  -- 'newest': JSON {source, documents: [{path, hash, size}]} of the newest entry
  -- 'active': JSON {hash, generation, docs: [path]}: the version whose flat activation last ran
  -- 'intent': JSON, at most one (see Intents)
recorded(doc TEXT PRIMARY KEY, epoch TEXT NOT NULL, seq INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL)
  -- each document's committed position when last recorded, and its text hash there
groups(editor TEXT PRIMARY KEY, label TEXT NOT NULL, started_at INTEGER NOT NULL, last_at INTEGER NOT NULL, dirty INTEGER NOT NULL)
  -- edit groups; dirty: edited since the last entry
outbox(seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL,
       kind TEXT NOT NULL, authors TEXT NOT NULL, message TEXT NOT NULL,
       rollback_of INTEGER, op TEXT, source TEXT NOT NULL, documents TEXT NOT NULL)
  -- source: content hash of the version whose non-document files the entry has
  -- documents: JSON [{path, hash, size}], at most 129 (128 plus a migration override)
texts(hash TEXT PRIMARY KEY, text TEXT NOT NULL, refs INTEGER NOT NULL, staged_at INTEGER)
ops(id TEXT PRIMARY KEY, seq INTEGER NOT NULL)
  -- rollbacks that committed, with their entry; deleted when that entry is acknowledged
```

The store is initialized once `meta.store` exists.

**Text references.** `refs` counts every place that needs a text: one per
document file of each unacknowledged entry (two paths with the same text
count twice) and one per `recorded` row. A text therefore stays in `texts`
while it is the recorded text of a document, even after every entry that
listed it is acknowledged, and later entries can reuse it without loading the
document. Rows with `refs = 0` are deleted at once, except staged rows (a
non-null `staged_at`), which wait for the operation that staged them or for
one hour.

### Non-document files

An entry names its manifest and assets by `source`, the content hash of a
version (`content.hash`), not file by file: a bundle may hold 20,000 files,
whose list must not travel through the outbox. The host keeps the file list
of every source it ever started (see [Store](#store)). Documents are listed
in the entry itself.

### Recording

`record(db, kind, message, shape, options)` runs inside the caller's
transaction. It is refused in a worker that is not the active one (its
generation is below `meta.active.generation`).

1. Build the entry's documents from `shape`. A document whose committed
   `(epoch, seq)` equals its `recorded` row reuses that row's hash and size.
   Any other document is loaded from committed state, hashed and destroyed
   one at a time, its text is inserted into `texts` (or found there), and its
   `recorded` row moves to the new text, adjusting both texts' `refs`.
2. `options.overrides` maps paths to explicit texts (migration). They are
   hashed and stored like loaded texts but never touch `recorded`.
3. When the source and documents equal `meta.newest` and the entry is not
   forced, stop: no entry.
4. Otherwise insert the entry, increment each listed text's `refs`, set
   `meta.newest`, clear `dirty` on every group, and record `options.op` in
   `ops` with the entry's seq.

Authors are `options.authors` when given (an agent's or a rollback's label),
else the labels of the groups that are `dirty` at that moment, in first-edit
order; at most 32, each at most 200 characters. The message is at most 500
characters.

Shapes:

- **current:** source `content.hash`, and every document of
  `content.documents` with its committed text.
- **before:** the source of `meta.newest`, and every document that
  `meta.newest` lists with its committed text now: the flat as people last
  saw it, while this worker already runs a newer version whose merge has not
  run.

**Text dirty**: some document listed in `meta.newest` has a committed
`(epoch, seq)` other than its `recorded` row.

### Flat activation

A version is activated for the whole flat at once. Every path that needs a
document (browser join, catch-up, update, read, `/_docs/healthz` for the
host's health trial, the edit endpoint, and the `sync`, `intent` and
`restore` host operations) first passes one gate: when `meta.active.hash` differs from `content.hash`
and this worker's generation is not below the highest activation generation
(the existing fence), the flat activation runs in that transaction:

1. Initialize the store when needed ([below](#initialization-and-migration)).
2. When text dirty: a `person` entry of the **before** shape. People's text
   that the merge may discard is now in the history.
3. When `meta.intent` names this version (`source = content.hash`):
   - kind `rollback`: restore every document to the intent's text
     ([Restoring text](#restoring-text)), then record a forced `rollback`
     entry of the current shape with the intent's label, `rollback_of` and
     `op`;
   - kind `agent`: merge every document (the existing seed and merge), then
     record an `agent` entry of the current shape with the intent's label,
     message and `op`.

   Without a matching intent, merge every document and record an `agent`
   entry with no authors.
4. Delete the intent and every group; set `meta.active` to this version's
   hash, generation and document paths. Delete the `recorded` rows of
   documents this version no longer has, releasing their texts: the entry of
   step 2 already holds what they last contained.

A restart with the same content (only a new runtime generation) passes the
gate without activating, and records nothing. A worker older than the active
one never activates or records; it may still serve reads while it drains.

The merge itself is unchanged, except that it no longer writes
`flats_docs_conflicts`: what it discards is in the entry of step 2.

**Stale writes.** A browser update reaching an older worker after a newer
activation is refused with `readonly` when the active version no longer has
that document (`meta.active.docs`), so an edit to a removed document cannot
commit. Edits to documents that still exist commit as today and reach the
next entry of the active worker.

### Revision boundaries

| Boundary | Where | In the same transaction |
|---|---|---|
| a person's update | `update()` (not a duplicate) | upsert `groups[editor]`: `started_at` kept or set, `last_at` = now, `dirty` = 1 |
| an agent live edit | `POST /_docs/api/edit` | when text dirty, a `person` entry (current shape); delete all groups; apply the edit; when the text changed, an `agent` entry crediting the request's `label`, with its `op` |
| a new version | flat activation | see above |
| edit groups closing | `POST /_docs/host/revisions/sync` | when a group is due (idle 30 s, or begun 5 minutes ago) and text dirty: a `person` entry (current shape); then delete the due groups |
| a rollback within the running version | `POST /_docs/host/revisions/restore` | when text dirty, a `person` entry; delete all groups; restore every document; a forced `rollback` entry with its `op` |

- Person and agent entries are not forced: identical content makes none.
- An agent edit that changes nothing records nothing. The groups it deleted
  had no unrecorded edits.
- Groups that are not due survive a `person` entry but lose `dirty`, so a
  later entry credits them again only after they edit again. A due group
  without unrecorded edits is just deleted.

**Editor identity.** `hello` gains `editor`, a random id the page keeps in
local storage next to its collaborator name. The group key is `ts:<login>`
for a verified Tailscale login (`Tailscale-User-Login`), else
`browser:<editor id>`; the label is the verified name or login, else the
collaborator name. A browser without an id gets one per connection.

### Intents

`meta.intent` holds at most one pending operation that starts a new version:

```json
{"id": "<op id>", "source": "<content hash>", "kind": "agent",
 "label": "...", "message": "...", "rollback_of": 12,
 "documents": [{"path": "...", "hash": "..."}]}
```

`kind` is `agent` or `rollback`; `rollback_of` and `documents` belong to a
rollback.

- The host writes it through the running (old) worker just before it starts
  the new version, under the flat lock. Writing an intent replaces an older
  one.
- Flat activation consumes it as above. `sync` runs the activation gate first
  and then deletes any intent that remains: the version it named either never
  started (a failed health check, a crash before the switch) or already
  activated. The host calls `sync` after every operation and at startup, so an
  intent does not outlive its operation. The host writes no intent for an
  unchanged save, which starts no version.
- A rollback intent's texts must be staged (`stage`) before the intent is
  written; the intent holds one `refs` on each until it is consumed or
  deleted.

### Restoring text

Used by a rollback intent and by `restore`, for every document of the target,
even one whose text already matches:

1. Replace the committed text with the target text by the bounded character
   diff (`applyText`), as one update with id `restore:<op id>`, and set
   `seed_text` to the target text (`seed_hash` keeps the running source). The
   update bypasses the 256 KiB limit for incoming browser updates, as
   activation merges do.
2. When that update would exceed the document's state, structure or receipt
   limits, reset the document instead: delete its updates, checkpoint,
   receipts and activation rows and seed it again with the target text under
   a new epoch. Connected editors then get `diverged`, which offers download
   and reload. A restore therefore never fails on those limits.

The next activation of a different source merges base = the restored text,
ours = live, theirs = the new source, so a later save merges against what the
rollback restored.

### Host operations

All require `X-Flats-Access: private` and `X-Flats-Host-Op: revisions`, which
only core sets (it strips every client spelling of `X-Flats-Host-Op`). Others
get 403. Bodies are JSON. `sync`, `intent` and `restore` pass the activation
gate first. The transport operations (`outbox`, `text`, `ack`, `stage`, `op`)
do not: they only need an initialized store, so the host can always drain
and acknowledge the backlog, which frees space, even while an activation
keeps failing.

| Route | Purpose |
|---|---|
| `POST /_docs/host/revisions/sync` | Deletes a remaining intent, closes due groups. Returns `{store, ready, intent}`: `ready` is the number of unacknowledged entries, `intent` the id of the remaining intent or null |
| `GET /_docs/host/revisions/outbox?after=` | Entries with `seq > after`, oldest first, read with one SQL statement per entry and stopping once the page holds 4 MiB of JSON (always at least one entry): `{store, entries: [{seq, created_at, kind, authors, message, rollback_of, op, source, documents}], more}` |
| `GET /_docs/host/revisions/text?hash=` | `{hash, text}` from `texts`; 404 when absent |
| `POST /_docs/host/revisions/ack` | `{store, through}`: deletes entries up to `through`, releasing their texts. Idempotent; another `store` is refused |
| `POST /_docs/host/revisions/stage` | `{text}` (at most 1 MiB UTF-8): stores it with `staged_at`; returns `{hash}` |
| `POST /_docs/host/revisions/intent` | `{id, source, kind, label, message, rollback_of, documents}`: writes `meta.intent` (texts must be staged or present) |
| `POST /_docs/host/revisions/restore` | `{id, label, rollback_of, documents: [{path, hash}], deadline_ms}`: a rollback within the running version (its documents must be exactly the request's). Returns `{store, seq}` |
| `GET /_docs/host/revisions/op?id=` | `{committed, seq?}`: whether a rollback with this id committed and its entry is still unacknowledged |

- **Size.** No request carries more than one text, so each fits the 10 MiB
  request limit. One entry lists at most 129 documents. Their paths are file
  paths of the bundle, at most 4 KiB each, so even with every character
  escaped six times by the SQL bridge's JSON encoding, one entry's row stays
  near 3.2 MiB, below the 16 MiB query result limit; a page stops at 4 MiB,
  below the 32 MiB response limit. A text response holds one document of at
  most 1 MiB.
- **Deadlines.** `restore` checks `deadline_ms` immediately before COMMIT, as
  live edits do. When its response is lost, core asks `op?id=` after the
  deadline to learn whether it committed.
- **Rollback receipts.** A rollback (from `restore` or a consumed intent)
  writes its `ops` row with its entry, and the row is deleted only when that
  entry is acknowledged, that is, after the host imported it. So for a
  rollback the host has not imported, `committed: false` means it did not
  commit; there is no bounded cache that could forget it.
- **Duration.** Flat activation merges every document in one transaction, as
  the publish health check already does on its isolated copy within the same
  request deadline. Recording loads only changed documents.

### Initialization and migration

A database without `meta.store` is initialized by the first transaction that
passes the activation gate, before anything else in it:

1. Generate `meta.store`.
2. Seed every document of `content.documents` that has no stored state yet
   (seeding discards nothing).
3. For each `flats_docs_conflicts` row, oldest generation first, record a
   forced `recovered` entry with the message "migrated recovery text" and the
   **stored** shape, with that document overridden by the preserved text (a
   document that is not in the shape is added). Overrides leave `recorded`
   alone. A preserved text above 1 MiB (impossible for rows this app wrote,
   kept as a guard) is skipped with a log line.
4. Record a forced `created` entry of the **stored** shape with the message
   "revision history started".
5. Delete the `flats_docs_conflicts` rows.

The **stored** shape is source `content.hash` and every document of
`content.documents` with its committed text, before any merge of this
version. When this version's merge has not run yet, flat activation then
runs it in the same transaction, finds the flat clean and captures nothing
more. A crash repeats all of it. The conflicts table stays in the schema,
unused.

### Removed

The user-facing conflict surfaces go, since revisions replace them: the
private route `GET /_docs/api/conflict`; `conflicts` in document reads,
`welcome` and the `conflicts` WebSocket message; the page's "Publication
replaced some edits" notice; `get_document {conflict}` and
`Service.GetDocumentConflict`.

## Host side (`internal/core`)

### Store

- `content_state` rows of type `docs`, phase `ready`, are created by the first
  import.
- One store migration adds `import_store TEXT` and `import_seq INTEGER` to
  `content_state` (the worker store and the last entry seq imported),
  `source TEXT` to `revisions`, and the `content_sources` and
  `content_holds` tables (below).
- Blobs live under `<flat>/revisions/` (designs keep `<flat>/design/`). The
  blob helpers take the store root; the root follows the type.
- A document's revision stores its documents in `revision_files` and names
  its other files by `revisions.source`; reading a revision expands the
  source (`GetRevision`, `changes`, rollback), and its `files` and `size`
  count both. A bundle of 20,000 assets therefore adds no rows per revision.
- Documents leave `content_head` and `content_groups` empty: the newest
  revision references what the head would, and groups live in the worker.
- **Sources.** A new table `content_sources(flat, source, path, hash, size)`
  holds the non-document files of every docs version the host started. When
  a docs runtime is built on the live data directory, the host stores its
  non-document files as blobs and their rows, before the worker can name the
  source. Import expands an entry's `source` from it. `ReferencedHashes`
  includes these rows. The sweep deletes a source only when no revision, no
  head, no stored version that is not pruned, and not the running version
  have it. When an entry names a source with no rows, import first rebuilds
  them from a stored version with that content hash.
- **Holds.** A new table `content_holds(flat, op, number)`, keyed by
  `(flat, op)`, holds the target revision of each rollback in progress.
  `retainTx` skips every revision that any hold names, like a pinned one,
  without counting it as a pin. Two rollbacks of the same target have two
  holds, and settling one leaves the other.

### Importing

`importDocsLocked(slug, budget)` runs under the flat lock against the live
docs instance, through internal request helpers that do not take the lock
again. It makes progress across calls: blobs it stored stay stored, so a call
that stops at its budget leaves the next call only the missing ones.

1. When `import_store` equals the worker's `store`, acknowledge through
   `import_seq` first (this also repairs an acknowledgement lost in a crash).
   A different store starts from 0: the first import, or a replaced worker
   database.
2. Read outbox pages after the cursor until the outbox is empty or the budget
   (64 MiB of fetched text per call) is spent.
3. For each entry: fetch its missing document texts (`text?hash=`, checked
   against their hash) and store their blobs. When the budget runs out, stop:
   the entry and the ones after it wait for the next call, which finds the
   blobs already fetched and continues with the rest.
4. For the entries whose blobs are complete, one content transaction inserts
   each as the next revision (kind, authors, message, `rollback_of`, time,
   its documents and its source; `changes` against the previous revision,
   both expanded), deletes the hold whose rollback id is the entry's `op`,
   and advances the cursor. Then acknowledge.
5. After the last page, run `retainTx` once.

An entry whose `source` the host cannot resolve (no rows and no stored
version to rebuild them from) cannot be imported: the import stops there with
an event and the rest waits; the worker keeps the entry. Since running and
stored versions keep their sources, that needs the host database to have lost
rows.

Interactive reads (`list_revisions`, the page's history) import within the
budget and then read whatever is in the store; the rest follows at the next
tick. Startup imports every document before the startup retention pass.

The blob sweep imports under the same lock first and skips a document whose
import failed or left entries in the outbox, so a blob a pending entry needs
is never deleted.

### Writes

All run under `lockStorage(slug)`.

- **Docs saves** (`SaveVersion` with a docs manifest: `save_document`,
  `create_flat {type: docs}`, `save_version` with a docs bundle, `flats
  deploy`, the REST upload):
  1. When a docs version is running: `intent {kind: agent, label:
     agentLabel(via), message, source: <the Draft's hash>, id}`.
  2. `saveDraftLocked`, `applyDocsLocked` (health check, then the switch), as
     today. A failed health check leaves the old version and its intent,
     which the `sync` below deletes.
  3. `sync`, then import. The `agent` revision is in the history.
  - An unchanged save (same content hash, still running) writes no intent and
    starts nothing, as today.
  - A save that cannot record (the worker refuses or is unreachable after the
    switch) still succeeded; an event says so and the ticker imports later.
- **`update_document`**: the edit request gains `label` and `op`; after a
  changed edit, import. The result reports `revision` when that import found
  the edit's entry.
- **The ticker.** Every 15 seconds the service visits each running docs flat
  whose lock is free: `sync`, then import, which also runs when `sync`
  failed, so the ungated transport routes drain the backlog during a failing
  activation. At startup it
  visits every docs flat once after the instances start, which also migrates
  them.

### Rollback

`RollbackRevision(slug, n)` on a document, under `lockStorage`:

1. Load revision `n`; a missing revision is `not_found`. A revision with more
   than 128 documents (only a migrated recovery revision can have one) is
   refused with `invalid`: its texts stay readable, and an agent or person can
   copy them back. Hold it: insert a
   hold with a new rollback id in one content transaction, before anything
   can run retention. Then import.
2. Stage every document text of `n` on the running worker (`stage`, one per
   request).
3. When `n`'s non-document files and set of documents equal the running
   version's: `restore` with every document. One transaction does the whole
   rollback.
4. Otherwise:
   1. `intent {kind: rollback, rollback_of: n, label, documents, source: <hash
      of n's bundle>, id}`;
   2. build the bundle from `n`'s blobs (manifest, assets, each document with
      its text in `n`) and start it with `saveDraftLocked` and
      `applyDocsLocked`. This internal path skips the disk-quota check (a
      rollback is never refused for disk quota) and the upload-only limit of
      4 MiB of Markdown per bundle, which live text may have outgrown; every
      file keeps its own limits;
   3. `sync`. The new version's flat activation has restored `n` and
      recorded the `rollback` entry in one transaction.
5. Import, then `sync` again if step 4 failed, which deletes an unused intent.

**Releasing a hold.** Importing the rollback entry (whose `op` is the hold's
id) deletes that hold in the same transaction, so retention can delete `n` only
after the rollback revision exists. A rollback that did not happen releases
it too: when `op?id=` says the operation did not commit and no intent with
that id remains (`sync` returns the remaining intent's id). Since a committed
rollback keeps its `ops` row until its entry is imported, that answer is
definitive. The ticker checks every
remaining hold that way, so a hold left by a crash ends once its operation is
settled: imported if the new version consumed the intent, released if the
intent was deleted.

A crash after step 4.2 leaves the intent in the worker database: the new
version consumes it when it first activates, at startup at the latest. A lost
`restore` response is settled through `op?id=` after its deadline. Either
way the call reports what happened, and repeating a failed rollback is safe.

### Kinds

`created`, `agent`, `person` and `rollback` as for designs, plus `recovered`
for migrated recovery text.

## Serving revisions on the document's origin

`docsAssets` gains a revision router in front of the worker:

| Route | Purpose |
|---|---|
| `GET /_docs/api/revisions?before=&limit=` | newest first: the design page shape `{revisions, next, pins, pin_quota, revision_quota}` |
| `GET /_docs/api/revisions/{n}` | `{revision, documents: [{path, title}], files: [{path, hash, size}], previous}`; titles as the app derives them (manifest name for the entry, else the first `#` heading, else the path); `files` are the other files; `previous` is the newest kept revision before `n`, or 0 |
| `GET /_docs/api/revisions/{n}/document?doc=` | `{doc, markdown, hash}` |
| `GET /_docs/api/revisions/{n}/files/<path>` | a non-document file of that revision, under the inert-asset rules of current assets (fixed MIME types, `nosniff`, the asset CSP, attachment for other types) |
| `POST /_docs/api/revisions/{n}/pin` | `{pinned}` |
| `POST /_docs/api/revisions/{n}/rollback` | `{name?}` |

- The worker's host operations live under a separate prefix,
  `/_docs/host/revisions/`, need the host-only header, and are called by core
  directly on the worker instance, not through this router.
- **Access.** Every route needs `X-Flats-Access: private`; on a public route
  they all return 404.
- **Write protection.** POSTs also need `Content-Type: application/json`,
  `X-Flats-Docs: 1` and `Sec-Fetch-Site: same-origin` (or, without it,
  `Origin` equal to the request's scheme and host).
- **Labels.** A rollback credits the verified Tailscale name or login, else
  the page's collaborator name (cleaned, at most 80 characters), else
  "Person".
- Reads take blob leases as design snapshots do. Errors are `{code,
  message}` with 404 `not_found`, 409 `conflict`, 400 `invalid` or 503
  `unavailable`.

## Document page

Private pages only; a public page has no history control and calls none of
these routes.

- **History button** in the header after the Edit | View group. It opens the
  history panel: a 320 px right-hand column on desktop, a full-screen sheet up
  to 899 px wide. `Esc` closes it.
- **Entries**, newest first, 50 per page with "Load more":
  - who and what: the authors ("Quiet Otter, Lucky Fox"), "Agent (MCP)",
    "Restored revision 12", "Created", "Recovered text";
  - relative time (absolute in the tooltip) and the documents it changed, by
    title, with "this document" for the open one;
  - a pin toggle (`aria-pressed`). A full pin quota shows its refusal in the
    panel;
  - the panel refreshes every 10 seconds while open and after its own actions.
- **Viewing a revision** replaces the document surface with a read-only
  render of the open document as of that revision, under a banner: "Revision
  12 · 3 hours ago" with "Show changes", "Restore this revision" and "Back to
  live". The editor stays connected in the background.
  - Images and links to the flat's files resolve to the revision's file route.
  - **Highlight changes** (the default) marks, in the rendered document, what
    this revision changed since the revision before it that still exists
    (`previous` in the revision route): changed text tinted and underlined
    line by line, changed tables, lists, code and rules outlined, images whose
    file changed between the two outlined, and a "− N lines removed here" note
    where text went. Links that use a changed reference definition count as
    changed. Every mark has a screen-reader label.
  - **Source changes** shows the same comparison as a Markdown line diff, and
    **Compare with live** a line diff against the live text, both with
    `+`/`−` markers.
  - The page diffs on its main thread with a 400 ms and 2,000-edit limit;
    texts that differ more show a note instead of a comparison.
  - A document missing from that revision says so and lists the revision's
    documents.
- **Restore** confirms first ("Restore every document to revision 12? The
  current text stays in the history."), calls rollback, returns to live and
  shows "Restored revision 12". The text arrives through the normal sync, or,
  after a reset, through the page's reload offer.
- The panel follows `_web/DESIGN.md`: system type, thin borders, the green
  accent for selection and focus, no shadows.

## Agent surface (`internal/mcpx`)

- `list_revisions`, `get_revision`, `pin_revision` and `rollback_revision`
  accept documents. `get_revision {path}` returns a document's text, or an
  asset as base64, as of that revision.
- `get_document` loses `conflict`. `update_document` results gain
  `revision`, the revision the edit created, once it is imported. Saves
  report nothing new: `list_revisions` shows their revision.
- `topic.docs` describes revisions instead of conflict records; the
  instructions keep their byte cap; the generated references are rebuilt.

## Console

The overview's "Revision history" quota settings are shown for documents too;
`GET`/`PUT /console/api/flats/{slug}/revision-quota` accept both types. Host
defaults are the existing `system.revision_quota` and `system.pin_quota`.

## Core API shape

- `revisionFlat(ctx, slug)` accepts any Revision-model flat (list, get, pin,
  quotas, retention, sweep); design-only paths keep `designFlat`.
- `RollbackRevision`, `ListRevisions`, `GetRevision` and `PinRevision`
  dispatch on the type; for documents they import first.
- A new `ErrNoRevisions` ("flat has no revisions: only documents and designs
  do"), category `no_revisions`, replaces `ErrNotDesign` on the revision
  tools; design tools keep `ErrNotDesign`.

## Tests

- **docs app (node):** recording shapes and text references (including after
  acknowledgement and two paths with one text); text dirty tracking; groups,
  their `dirty` marks and closing with a fake clock; the activation gate,
  including a browser that joins first, a restart that records nothing and a
  stale worker; intents of both kinds and stale intents; restoring text
  (exact text, merge base reset, reset on capacity); `restore`, `stage`,
  `outbox` paging, `text`, `ack`, `op` and their access checks; migration of
  conflict rows with several rows for one document; stale writes to removed
  documents; reserved `restore:` ids.
- **core:**
  - saves create `created`, then `agent` revisions; unchanged saves none;
  - people's edits through the worker become a `person` revision after the
    ticker, crediting the editor; groups close before agent writes;
  - rollback within the running version, and with a removed document and a
    changed asset (new version), including a crash after the switch; one
    `rollback` revision; a later save merges against the restored text;
  - import idempotency: a repeated page, a lost acknowledgement, a replaced
    worker database, and an import that stops at its budget and resumes;
  - holds: a rollback target surviving quota 1 across a budget-limited import
    and a crash, released after a failed rollback, and kept by a committed
    but unimported rollback while a second rollback of the same target fails;
  - an activation that fails while transport operations still drain the
    backlog;
  - migration of existing `flats_docs_conflicts` rows; pins and quota
    overrides; the sweep skipping a flat with pending entries.
- **handler:** the revision routes' access and write protection; file route
  MIME and CSP.
- **mcpx:** revision tools on documents; `get_document` without `conflict`.
- **browser:** the history panel, viewing, the diff and restore on a dev host.

## Out of scope

The two open rules under [Goal](#goal), and moving live text into the host
revision store.
