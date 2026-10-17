## Markdown documents (`type: "docs"`)

A docs flat runs the built-in collaborative editor: people edit live Markdown
in the browser. A Private link opens the editor and can toggle between Edit
and View; a Public link is view only (a read-only rendered document).

A document has **no Draft, versions, previews or publish**: those are for
websites. Creating or saving a document runs it at once, Private, with no
approval, and its Private URL serves it as soon as the host is ready. Its
history is kept as **revisions** instead (below).
Visibility still works as for websites: making it Public (or Private again)
and deleting it wait for the operator's approval (`topic.approvals`). On
documents, `list_versions`, `get_draft`, `create_version`, `deploy`, `open_preview`
and `rollback` are refused (`refusal.invalid`).

### Create and write

1. `create_flat {slug, name?, type: "docs"}` creates a document holding
   `index.md` with a `# <name>` heading and starts it. Report its
   `private_url` once `get_flat` shows `private_state: ready`.
2. For an existing docs flat, call `get_document {slug, doc?}` first. It
   returns the **live** Markdown including people's edits. It is idempotent
   and non-destructive but may seed or activate live state. For a small
   change, prefer `update_document` (below) over saving the whole document.
3. `save_document {slug, markdown, title?, path?, message?}` saves one
   Markdown document (`index.md` by default), creating a missing flat, and
   runs it at once. Other documents, images and assets are preserved, as are
   existing `flats.json` settings. An optional `title` changes only the
   manifest's `name`. Adding a document keeps the current entry; if it was
   implicit, the host makes it explicit when needed. People's concurrent edits
   are merged as described below. The result names the saved `doc` and the
   `entry`; `get_document` without `doc` reads the entry, which may differ
   from the default save path `index.md`. The result's `flat` shows `running`.
4. For several documents or assets, use `save_document {slug, replace_bundle: true,
   files: [{path, content, encoding}]}` with the complete bundle, including
   `flats.json` with `{"type":"docs"}`. This explicitly replaces the whole
   bundle: omitted documents and assets are deleted. It runs at once.
   The CLI equivalent is `flats save-document <slug> <dir|archive>`; the HTTP
   archive route is `POST /api/flats/{slug}/document`. Both replace the whole
   bundle explicitly. Draft tools and routes refuse document content.
5. A slug keeps its type: saving a document into a website or a page, or
   either into a document, is refused (`refusal.conflict`); use another slug.

If a saved document cannot start (for example its health check fails), the
save is refused and the running document keeps its previous text. Fix the
startup problem and repeat that same save: a different sibling save does not
carry the failed change. If no document has ever started, the files remain
saved internally until its first successful start. A single-document save validates the resulting
bundle against upload limits. If retained files put it over a limit, shrink the
target document further, raise `upload_max_bytes` for an upload-size refusal,
or explicitly save the complete desired bundle. No files are deleted to make
room; the disk quota still applies. **On a Public document a save, like a live edit,
is visible to everyone at once**; tell the user.

### Edit live text

`update_document {slug, doc?, ops, if_hash?}` edits the **live** Markdown of a
running document directly, the way a person edits in the Private editor: there
is no publish and **no approval**. Connected editors see the
change at once and their own concurrent edits elsewhere are kept.
**On a Public flat the change is visible to everyone on the internet as soon
as the call returns**; such results carry `public_notice`, so tell the user.
Use it for small corrections and additions; use `save_document` for a
rewrite of one document, or when the document is not running (which refuses
`update_document` with `not_deployed`). Repeat `save_document` to write other
documents; use an explicit whole-bundle save for assets or replacement.

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
4. The result gives the new `seq` and `hash`, the `revision` the edit
   created and, per operation, the line and the characters removed and
   inserted. Each applied edit is also recorded in `get_logs` (kind
   `document`) with that summary, not the text. To undo an edit, roll back to
   the revision before it (below) or apply the reverse edit.

Limits: a request of at most 512 KiB, one resulting update of at most
256 KiB, Markdown up to 1 MiB per document, the document's stored history,
block operations only on documents of at most 50,000 blocks (use
`replace` beyond that), and the per-flat disk quota, which must also hold the
revision the edit creates (`document_capacity`); about 10 edits per second per flat (`unavailable`
when exceeded: wait and retry). Any other `unavailable` says whether the edit
applied (at which seq; do not repeat it) or did not apply (retry). Only if
Flats could not check is the outcome unknown: then read `get_document`
before retrying so an insert is not applied twice.

Live edits change only the live text, not the files the document was last
saved with. The next `save_document` merges into them exactly as into
people's edits (below): where the save changes the same lines, the save wins,
and the live text before the save stays in the revision closed just before
it. To keep a live edit in a save, start from the live text that
`get_document` returns.

When a document is saved, its Markdown is merged line by line into the live
text: disjoint and adjacent human edits survive; overlapping lines prefer the
saved text.

### Revisions

Every change of a document is kept as a revision of the whole flat (all its
documents and assets):

* each `save_document`, docs bundle save or `update_document` call is one
  revision, credited to you;
* people's edits in the browser are grouped per person: a group becomes one
  revision when that person has been idle for 30 seconds or 5 minutes after
  it began;
* before your save or edit applies, people's edits not yet in a revision
  become one, so the state just before your write is always kept.

| Tool | Use |
| --- | --- |
| `list_revisions {slug, before?, limit?}` | newest first, with kind (`created`, `agent`, `person`, `rollback`, `recovered`), authors, message, pinned and changed paths; pass `next` as `before` for older ones |
| `get_revision {slug, revision, path?}` | one revision's files, or with `path` a document's Markdown (or an asset as base64) as it was then |
| `rollback_revision {slug, revision}` | every document and asset becomes as it was in that revision, as a new revision; it can be undone the same way. Live at once; on a Public flat everyone sees it |
| `pin_revision {slug, revision, pinned}` | pinned revisions are never deleted automatically; a full pin quota refuses a new pin |

None of them needs approval. Old unpinned revisions are deleted beyond the
host's quota (default 100 kept, 20 pins); the operator can change both per
flat. People see the same history in the document's History panel on its
Private link. `recovered` revisions hold text that an older Flats preserved
when a save overwrote people's edits.

### Bundle rules

* Every `.md`/`.markdown` file is one document: valid UTF-8, at most 1 MiB
  each and 4 MiB together, at most 128 documents. Other files are assets,
  served as static files. Images (including SVG), audio, video, fonts and
  plain text (`.txt`, `.csv`, `.log`) open in the browser; every other asset,
  such as HTML, scripts or PDF, downloads instead. Assets never run scripts.
  Paths under `_docs/` are reserved.
* `entry` is the main document. Default: `index.md`, then `README.md`, then the
  only `.md`/`.markdown` file at any depth. It must be a Markdown file.
* `name` is the document title shown in the editor (default: the first `# `
  heading of the entry, else its path).
* `kind`, `spa` and `not_found` are rejected (the host chooses the runtime).
  `health` defaults to `/_docs/healthz`; a different value is rejected.
  `screenshot` keeps its meaning.
* Raw HTML in Markdown is not rendered and unsafe links are rejected.
  Relative links and image paths resolve relative to the document's directory.
* A fenced code block whose language is exactly `mermaid` is drawn as a
  [Mermaid](https://mermaid.js.org/) diagram (strict security level: no
  click handlers or HTML labels) in the light or dark theme of the reader.
  Diagram controls offer an expanded view with zoom, pan and fit, source
  viewing/copying, and SVG or PNG download. The expanded view supports keyboard
  navigation and Escape to close, restoring focus to its opener.
  Invalid syntax preserves the source and shows a syntax error; loading
  failures offer retry. Diagrams in quotes and lists also render in Edit.
  While editing a diagram's source, an optional preview updates after typing
  pauses. In Edit mode, Edit source opens the editable fence and Done returns
  to its diagram. View mode retains a read-only Source toggle.
* A document runs as kind `server` but keeps no data snapshots: it has no
  rollback or data restore. Docs flats get no outbound network, even with
  saved origin grants, and do not use environment variables or secrets.

The editor's runtime layout, storage schema and wire protocol are host
internals, not part of this contract or of runtime API v1.

### Not in scope

Comments, rich-text (WYSIWYG) editing, DOCX/PDF export, account management,
offline editing beyond in-memory reconnect buffering, multi-host replication,
and arbitrary HTML in Markdown.
