## Markdown documents (`type: "docs"`)

A docs flat runs the built-in collaborative editor: people edit live Markdown
in the browser, and your published versions merge into their edits. A
Private link opens the editor and can toggle between Edit and View; a Public
link is view only (a read-only rendered document).

### Write and edit

1. For an existing docs flat, call `get_document {slug, doc?}` first. It
   returns the **live** Markdown including people's edits, or the Current
   Draft's file (`source: "draft"`) when no docs version runs. It is
   idempotent and non-destructive but may seed or activate live state.
   For a small change to a running document, prefer `update_document`
   (below) over replacing the whole Draft.
2. `save_document {slug, markdown, title?, path?, message?}` saves a Draft
   holding one Markdown document (`index.md` by default) plus a `flats.json`
   with `type: "docs"` and `name` from `title`. It replaces the complete
   Current Draft, creates a missing flat and never publishes.
3. For several documents or assets, use `save_version` with the complete
   bundle and a `flats.json` of `{"type":"docs"}`.
4. Review with `open_preview` version 0, then `publish` and wait for approval
   (`topic.approvals`).

### Edit live text

`update_document {slug, doc?, ops, if_hash?}` edits the **live** Markdown of a
running docs version directly, the way a person edits in the Private editor:
there is no Draft, no publish and **no approval**. Connected editors see the
change at once and their own concurrent edits elsewhere are kept.
**On a Public flat the change is visible to everyone on the internet as soon
as the call returns**; such results carry `public_notice`, so tell the user.
Use it for small corrections and additions; use `save_document` + `publish`
for a rewrite, several documents or assets, or when no docs version runs
(that refuses with `not_deployed`).

1. Read `get_document {slug, doc?, blocks: true}`. It returns `hash` (of the
   whole document) and `blocks: [{hash, kind, level?, line, preview}]`. A
   block is a run of non-blank lines; a fenced code block is one block and a
   `#` heading line is always its own block. One read lists at most 1,000
   blocks; `blocks_total` gives the count and `block_offset` the next page.
2. Send 1–32 `ops`. They apply in order, each to the result of the previous
   one, and **all apply or none do**:
   * `{op: "replace", find, with, nth?}` replaces exact text. `find` must occur
     once, or give `nth` (1-based). An empty `with` deletes the text.
   * `{op: "replace_block", block, with}` and `{op: "delete_block", block}`
     act on a whole block named by its hash.
   * `{op: "insert", text, before|after|section_end: <block hash>}`, or
     `{op: "insert", text, at: "start"|"end"}`. `section_end` takes a heading
     block and inserts after the last block before the next heading of the
     same or a higher level. Inserted text becomes its own block(s): Flats
     adds the blank lines around it. To add a list item or a sentence to an
     existing block, use `replace` on its last line instead.
   * `nth` also picks among identical blocks with the same hash. Because it
     selects by position, any op with `nth` needs `if_hash`.
3. The guards are the text itself: if someone changed the `find` text or the
   block since you read it, the hash no longer matches and the whole call is
   refused with `edit_conflict` and **changes nothing** (their edit wins).
   Read again and rebuild your operations; never overwrite people's text to
   win. `if_hash` (from `get_document`) additionally refuses the call if
   anything in the document changed.
4. The result gives the new `seq` and `hash` and, per operation, the line and
   the characters removed and inserted. Each applied edit is recorded in
   `get_logs` (kind `document`) with that summary, not the text. Flats keeps
   no separate copy of the text an edit replaced: to undo, apply the reverse
   edit while the text is still there.

Limits: a request of at most 512 KiB, one resulting update of at most
256 KiB, Markdown up to 1 MiB per document and the document's stored history
(`document_capacity`); about 10 edits per second per flat (`unavailable`
when exceeded: wait and retry). Any other `unavailable` says whether the edit
was rolled back; if it may have applied, read `get_document` before retrying
so an insert is not applied twice.

Live edits are not part of any published version: `list_versions` and the
Current Draft do not change. `get_document` returns them because it reads
live text. The next published version merges into them exactly as into
people's edits (below): where the version changes the same lines, the
version wins and the discarded live text is kept for recovery. To keep a live edit in a
future version, copy the live text from `get_document` into your next Draft.

When a version activates, its Markdown is merged line by line into the live
text: disjoint and adjacent human edits survive; overlapping lines prefer the
version. When a merge discards human text, the previous live Markdown is kept
for private recovery: responses list `conflicts: [{generation, bytes}]`, and
`get_document {slug, doc?, conflict: <generation>}` returns the preserved text.
Each document keeps the newest 8 records within 2 MiB; older ones are gone.

### Bundle rules

* Every `.md`/`.markdown` file is one document: valid UTF-8, at most 1 MiB
  each and 4 MiB together, at most 128 documents. Other files are assets,
  served as static files. Paths under `_docs/` are reserved.
* `entry` is the main document. Default: `index.md`, then `README.md`, then the
  only `.md`/`.markdown` file at any depth. It must be a Markdown file.
* `name` is the document title shown in the editor (default: the first `# `
  heading of the entry, else its path).
* `kind`, `spa` and `not_found` are rejected (the host chooses the runtime).
  `health` defaults to `/_docs/healthz`; a different value is rejected.
  `screenshot` keeps its meaning.
* Raw HTML in Markdown is not rendered and unsafe links are rejected.
  Relative links and image paths resolve relative to the document's directory.
* A docs version is recorded as kind `server`: snapshots, isolated previews
  and rollback behave as for server flats (`topic.rollback-data`). Docs flats
  get no outbound network, even with saved origin grants.

The editor's runtime layout, storage schema and wire protocol are host
internals, not part of this contract or of runtime API v1.

### Not in scope

Comments, rich-text (WYSIWYG) editing, DOCX/PDF export, account management,
offline editing beyond in-memory reconnect buffering, multi-host replication,
and arbitrary HTML in Markdown.
