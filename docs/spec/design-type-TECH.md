# Design content type: technical design

Behavior: [design-type.md](design-type.md). Revision rules:
[content-models.md](content-models.md).

## Overview

A design runs on the host itself, not in a JavaScript worker:

- **Content** lives in a revision store owned by `internal/core`. File blobs
  are content-addressed under the flat directory; metadata is in `flats.db`.
- **Serving** is a Go handler in `internal/design`, installed as the flat's
  live instance. It serves the canvas app, the design's files, a JSON API and
  an event stream on the flat's own origin.
- **The canvas app** is plain JavaScript and CSS embedded from
  `internal/design/web`. It has no build step and no third-party code.

Designs therefore need no runtime: they work on `--runtime=false` hosts. The
same store is meant for documents later.

## Type and lifecycle integration

### Type

- `internal/contenttype`: `Design = "design"`, listed after `page` as
  `{Design, "Design", "Canvas of HTML artboards that people and agents edit live"}`.
- `contenttype.RevisionModel(t)` is true for `docs` and `design`. Every check
  of "no versions" uses it:
  - `refuseDocs` becomes `refuseUnversioned`, returning `ErrDocsUnversioned`
    for documents and `ErrDesignUnversioned` for designs. Both wrap
    `ErrInvalid`, and each message names its own tools.
  - `prunableVersions` keeps one version for both.
  - The console and the MCP `flatInfo` shape treat both alike.

### Bundles

`ParseManifest` accepts `type: "design"` only through
`bundle.ParseDesignShell`, which core calls when it creates a design. On
every public bundle path, including a first save to an absent slug, a design
manifest is refused with `ErrDesignUnversioned` and the fix "create designs
with create_flat {type: design} or save_design". The public paths are
`save_version`, `save_draft`, `save_page`, `save_version_from_dir`, `flats
deploy`, `flats draft`, the REST upload and `SaveVersion` from any `Via`.
`SaveVersion` is reached on these paths only after this check. The
unknown-type message lists four types.

### Creating a design

`CreateDesign` runs entirely under `s.lock(slug)`:

1. Validate the files, create the flat row, then store and sync the blobs
   under the flat's directory. A failure removes the new row and directory.
2. In one SQLite transaction, write the `content_state` row (`phase =
   'init'`) and revision 1 (`created`) from the starter files.
3. Store the shell: a Draft revision whose bundle is only
   `flats.json {"type":"design","name":...}`.
4. Publish that revision at once through `publishRevision`, as
   `applyDocsLocked` does.
5. Set `content_state.phase = 'ready'`.

`save_design` on an absent slug calls the same function with the agent's
files instead of the starter.

- **Recovery.** A crash between steps 1 and 2 leaves an untyped flat, with
  a row (so it is listed and owned) but no live version, Draft or marker. That is the same state as a flat
  created empty, and it becomes a design on its first `save_design`
  (adoption). A crash later in the sequence leaves a marker.
- **The marker.** A `content_state` row is the durable marker that a flat is
  a design. `flatType` returns `design` for a flat that has the row but no live
  version or Draft yet. At startup, `startPendingDesigns` finishes steps 3 to
  5 for every row still in `init`, under the flat lock. Because the type is
  known from step 2 on, a racing first save of another type is refused.
- **Serving.** `buildWithEnvironment` checks
  `contenttype.FromManifest(v.Manifest) == Design` before switching on the
  kind. It returns
  `&deployed{version: v, handler: inst, inst: inst, redact: identity, environment: snapshot}`,
  where `inst` is the flat's design instance (below). Health checks and
  activation therefore get the fields they use.
- **The shell never changes.** No path besides `CreateDesign` and recovery
  writes it.

### Teardown

The design instance implements `core.Instance`, so the existing teardown
paths reach it through `deployed.inst.Stop()`: rename (`rename.go`), delete,
replacement and host shutdown.

`Stop`:

- marks the instance stopped, so new requests get 503;
- cancels a context that every SSE stream and long read selects on;
- waits for in-flight requests to finish, up to 5 seconds.

A rename builds a new instance for the new slug.

Groups and state live in SQLite and follow a rename through `ON UPDATE
CASCADE`, so nothing in memory needs moving.

### Type stickiness

`typeNoun` gets "design", and `typeChangeError` gets the fix "use
save_design". `flatType` reads the live manifest, and before publication the
`content_state` marker.

## Revision store

### Disk

```
<data>/flats/<slug>/design/blobs/<hh>/<sha256>    file bytes, mode 0444
<data>/flats/<slug>/design/tmp/                   staging for new blobs
```

Blobs live under the flat directory, so rename, delete, backups and `dirSize`
(the disk quota) cover them unchanged.

**Writing a blob:**

1. Create any missing directory of `design/tmp` and `design/blobs/<hh>`,
   syncing each parent after it gains an entry.
2. Write the bytes to `tmp/<random>`.
3. `fsync` the file.
4. Rename it to `blobs/<hh>/<hash>`. When the target exists, drop the temp
   file instead.
5. `fsync` `blobs/<hh>`.

Only then may a transaction reference the hash. Blobs never change once
written.

### Schema

A migration step adds these tables to `internal/store` and bumps
`user_version`:

```sql
CREATE TABLE content_state (     -- one row per Revision-model flat on this store
  flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  type TEXT NOT NULL,            -- 'design'
  phase TEXT NOT NULL,           -- 'init' | 'ready'
  seq INTEGER NOT NULL,          -- bumps on every head change
  gen INTEGER NOT NULL,          -- flat-wide file generation counter, never reused
  revision_quota INTEGER, pin_quota INTEGER);   -- per-flat overrides, NULL = host default
CREATE TABLE content_head (      -- current files
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  path TEXT NOT NULL, hash TEXT NOT NULL, size INTEGER NOT NULL,
  gen INTEGER NOT NULL,          -- a fresh counter value when an agent write or a rollback writes this file
  PRIMARY KEY (flat, path));
CREATE TABLE content_groups (    -- open groups of a person's edits
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  editor TEXT NOT NULL, label TEXT NOT NULL,
  started_at INTEGER NOT NULL, last_at INTEGER NOT NULL,
  PRIMARY KEY (flat, editor));
CREATE TABLE revisions (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  number INTEGER NOT NULL, created_at INTEGER NOT NULL,
  kind TEXT NOT NULL,            -- created | agent | person | rollback
  authors TEXT NOT NULL,         -- JSON list of labels
  message TEXT NOT NULL, rollback_of INTEGER,
  pinned INTEGER NOT NULL DEFAULT 0,
  changes TEXT NOT NULL,         -- JSON {added, changed, removed} against the previous revision
  files INTEGER NOT NULL, size INTEGER NOT NULL,
  PRIMARY KEY (flat, number));
CREATE TABLE revision_files (
  flat TEXT NOT NULL, number INTEGER NOT NULL,
  path TEXT NOT NULL, hash TEXT NOT NULL, size INTEGER NOT NULL,
  PRIMARY KEY (flat, number, path),
  FOREIGN KEY (flat, number) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE);
CREATE INDEX revision_files_hash ON revision_files(flat, hash);
CREATE INDEX content_head_hash ON content_head(flat, hash);
```

- Revision numbers increase per flat and are never reused. The next number is
  `max(number) + 1`, or `1`. `content_state` keeps no counter, so a deleted
  newest revision cannot recur: the newest revision is never deleted.
- `changes` is computed at commit against the revision before it. It survives
  retention of that revision. Revision 1 lists every file as added.

### Writes

Every write runs under `s.lock(slug)`, the per-flat lock that rename, delete
and lifecycle actions also take. Each write follows the same order:

1. store and sync its new blobs;
2. run one `BEGIN IMMEDIATE` transaction for head, groups and revisions;
3. publish the event.

The transaction always closes with
`PRAGMA wal_checkpoint(PASSIVE)`. Durability of references against blob
deletion comes from the sweep rule below, not from the synchronous mode.

- **`commitTx(kind, authors, message, rollbackOf)`** turns the current head
  into revision `n+1`. It copies the `content_head` rows and computes
  `changes`. It never touches `content_groups` and never runs retention: each
  caller decides which groups close and runs `retainTx` afterwards, in the
  same transaction.
- **Agent write** (`WriteDesign`, one MCP call):
  1. When the head differs from the latest revision, `commitTx(person,
     <labels of all open groups>)` keeps the pre-write state.
  2. Delete every open group.
  3. Apply the files and deletes. Each written file whose content changes
     gets a fresh value of the flat's generation counter.
  4. Validate the resulting design.
  5. `commitTx(agent)`, then `retainTx()`.

  An optional `expected_revision` refuses with `conflict` when the latest
  revision differs.
- **Person operation** (`ApplyDesignOp`): check the guards, apply the change
  to the head, `seq++`, and upsert the editor's `content_groups` row: keep
  `started_at`, update `last_at`.
- **Group closing.** A service ticker runs every 5 seconds.
  - A group is due when its editor has been idle for 30 seconds or the group
    began 5 minutes ago.
  - For each flat with a due group, under the flat lock, when the head differs
    from the latest revision, it commits a `person` revision. That revision
    credits the due group's editor, and every other editor whose edits it
    includes.
  - It then deletes only the due group's row. Other editors keep their groups
    and deadlines, and their later edits go into later revisions.
  - This is the per-editor deadline rule of content-models, with whole-flat
    snapshots. After a restart, overdue groups close on the first tick.
- **Rollback(R):**
  1. Load R's file rows before anything else. A missing R is `not_found`.
  2. When the head differs from the latest revision, `commitTx(person, ...)`
     keeps the pre-rollback state, with no retention.
  3. Delete every open group. Replace the head with R's rows. Every restored
     file gets a fresh generation, even when its bytes did not change, so any
     change prepared before the rollback is refused.
  4. `commitTx(rollback, rollbackOf=R)`, then `retainTx()`.

  All of this is one transaction, so R stays protected until it is copied. A
  rollback stores no blob, so the disk quota does not apply.
- **`retainTx()`** counts every unpinned revision, the newest included, and
  deletes the oldest unpinned revisions while the count exceeds the quota. It
  never deletes the newest revision, so a quota of 1 keeps exactly one. The
  quota is the flat override, else the setting `revision_quota` (default 100);
  0 means unlimited.
  - Lowering `revision_quota` in the host settings runs `retainTx` for every
    design flat, each under its flat lock, before the settings call returns.
  - Lowering a flat override does the same for that flat.
  - Startup runs it for every design, so an interrupted pass completes.
- **Pin(n, on)**: pinning when `pin_quota` (setting default 20, override, 0 =
  unlimited) is reached is refused with `conflict`. Unpinning runs `retainTx`.
- **Blob sweep (garbage collection).** Blobs are never deleted inline.
  - A sweep runs at startup, every 10 minutes and after a retention pass that
    deleted revisions.
  - For each design it takes the flat lock, then runs
    `PRAGMA wal_checkpoint(FULL)` and checks its result row: unless it is not
    busy and every WAL frame was checkpointed, the sweep deletes nothing for
    that flat and tries again at the next sweep. Committed metadata is thus
    in the database file before any blob is deleted.
  - It lists blob files and deletes those that are:
    - not referenced by `content_head` or `revision_files`;
    - older than 10 minutes, so a blob stored for a transaction still in
      flight is never taken;
    - not held by a read lease (below).
  - It also deletes `tmp/` entries older than 10 minutes.
  - The sweep also reclaims blobs from failed writes and crashes.
- **Read leases.** A request that serves a blob reads the needed rows and
  takes an in-memory lease on those hashes while holding the flat lock, which
  the sweep also holds, so no selected blob can be swept in between. It then
  releases the lock, reads the bytes, and holds the leases until its handler
  returns. The sweep skips
  leased hashes. Export and revision reads use the same mechanism, so a zip
  is one coherent snapshot.
- **Disk quota.** Before storing new blobs, a write checks `dirSize(flatDir)`
  plus the new unique bytes against the quota. A failure is refused with the
  existing quota `ValidationError`. A person's operation runs the same check,
  and its future revision adds only rows. A rollback adds no bytes.
- **Limits.**
  - A design has at most 512 files.
  - Each file is at most `upload_max_bytes`, and one agent write is at most
    `upload_max_bytes` in total.
  - `list_revisions` pages are at most 100 entries.

### Paths

Every path in writes, lookups, routes and the zip follows one portable
grammar:

- segments match `[A-Za-z0-9_][A-Za-z0-9._-]{0,99}`, separated by `/`;
- at most 8 segments and 255 bytes in total;
- no `.`, `..` or empty segment, no backslash, colon or drive syntax;
- no leading `_design`, `flats.json` or `.well-known`.

Two paths may not differ only in letter case, and no file may also be a
directory prefix of another file. Requests for any other path return 404.

### Validation of a design

- **`canvas.json`** is required, as a JSON object of at most 256 KiB:
  - `title`: a string;
  - `boards`: keys are `.html` paths present in the head, and values hold:
    - `x` and `y`: finite numbers;
    - `w` and `h`: 40 to 8000;
    - optional `title`: at most 200 characters;
    - optional `fill`: a boolean;
  - `order`: a permutation of the board keys. Missing keys are appended in
    path order;
  - `notes`: at most 200, keyed by `[A-Za-z0-9_-]{1,40}`. Each has `x`, `y`,
    `text` (at most 4000 characters), and optional `w` and `color` (`gray`,
    `yellow`, `blue`, `green`, `pink` or `purple`);
  - `launch`: `{"view":"canvas"}` or `{"view":"focused","board":<key>}`.

  Unknown top-level keys are kept. All problems are reported at once as a
  `ValidationError`.
- **Artboards** are UTF-8, with at most one valid tweaks block each.

## Artboard editing model (`internal/design/source.go`)

A tokenizer pass (`golang.org/x/net/html`) records each token's byte range.

- **Editable text elements.** For each start tag in source order, an element
  is text-editable when all of these hold:
  - it is outside `head`, `template`, `svg`, `math` and `noscript`;
  - it is not itself in the excluded list (`html`, `head`, `body`, `script`,
    `style`, `title`, `textarea`, `template`, `svg`, `math`, `select`,
    `option`, `iframe`, `object` or `noscript`);
  - its content up to the matching explicit end tag has non-whitespace text,
    and only allowed inline tags (`b`, `strong`, `i`, `em`, `u`, `s`, `small`,
    `sub`, `sup`, `code`, `mark`, `span` and `br`), each closed explicitly;
  - it has no comments;
  - no editable ancestor covers it.

  Candidates are scanned linearly, each over at most 20,000 tokens, with a tag
  stack of at most 256.
- **Node ids.** The k-th editable element in source order is node `k`. Node
  ids are only meaningful together with the file's `gen` and an exact guard
  on the element's content (below).
- **Serving rewrite.** It applies only to the canvas's iframes, not to
  export:
  - it adds `data-flats-node="k"` to each editable element's start tag;
  - before `</head>` (else after `<head>`, before `<body>`, or after the
    doctype) it inserts `<style data-flats-host>` with the tweak CSS and
    `<script src="/_design/app/bridge.js" data-flats-host>`.
- **Tweak-only transform.** It adds only the tweak `<style>` at the same
  point, with no node ids and no bridge, and keeps relative asset paths. It
  is used for export, for direct opens (`GET /<path>`) and for files of a
  viewed revision, so an artboard always shows its saved tweak values.
- **`text.set {path, gen, node, tag, inner_hash, html}`:**
  1. Require the file's current `gen` to equal `gen`.
  2. Re-scan the file and require node `k` to have that tag, with the SHA-256
     of its exact inner source bytes equal to `inner_hash`. Otherwise refuse
     with `conflict`.
  3. Sanitize `html` as inline content of that tag. The allow list above is
     kept, with only `class` and `style` attributes; other elements are
     unwrapped and comments dropped. Text keeps HTML whitespace: a line
     break is an explicit `<br>` from the editor, and source newlines stay
     whitespace. The result is at
     most 64 KiB and has text.
  4. Splice it into the inner range.

  The canvas app gets `inner_hash` and the current inner source from
  `GET /_design/api/node`, never from the artboard.
- **Tweaks block.** A tweaks block is
  `<script type="application/json" data-flats-tweaks>`, at most one per
  artboard. It is an object of at most 64 entries keyed by
  `[a-z][a-z0-9-]{0,39}`, each `{type, label, value, ...}`:

  | Type | Value | Other fields |
  |---|---|---|
  | `color` | `#rgb`, `#rrggbb` or `#rrggbbaa` | |
  | `number` | finite | `min`, `max`, `step`, `unit`: `px`, `rem`, `em`, `%` or empty |
  | `text` | at most 200 characters | |
  | `select` | one of `options` | `options`: 1 to 32 strings |
  | `boolean` | `true` or `false` | |

  - **CSS.** The tweak CSS sets each tweak as `--<name>` on `:root`. Text, and
    select values that are not identifiers, are CSS strings. `<`, `>`, quotes
    and backslashes are escaped, so the value cannot end the style element.
    Booleans become `1` or `0`.
  - **`tweak.set {path, gen, name, value, base_value}`** requires the current
    `gen`, and `base_value` equal to the current value. It validates the new
    value, re-marshals the block (sorted keys, 2-space indent, `<` escaped)
    and splices it in.
- **Canvas operations.** Each one requires the current `gen` of
  `canvas.json`. It then decodes the file, changes the named fields and
  re-marshals with 2-space indentation, keeping unknown keys.

  | Operation | Guard |
  |---|---|
  | `board.set {path, fields, base}` (`x`, `y`, `w`, `h`, `title`) | each field's `base` equals its current value |
  | `board.order {path, to: front\|back, base_index}` | the board's current index equals `base_index` |
  | `note.add {id?, fields}` | the id must not exist |
  | `note.set {id, fields, base}` | as `board.set` |
  | `note.delete {id, base}` | `base` is the complete note and must equal it |

  The resulting canvas is validated as above.

## Serving (`internal/design/handler.go`)

The instance serves one flat's origin. It takes access from `X-Flats-Access`,
which `trustedAccess` sets; anything but `private` is public.

| Route | Access | Purpose |
|---|---|---|
| `GET /` | all | canvas app shell |
| `GET /_design/app/<file>` | all | embedded app assets |
| `POST /_design/api/session` | all | issue a resource capability (below) |
| `GET /_design/api/state` | all | `{access, visibility, seq, revision, title, canvas, files:[{path,size,hash,gen}], artboards:{path:{tweaks}}}`; public responses omit `gen` |
| `GET /_design/api/events` | all | SSE: `hello {seq}`, then `change {seq, revision, paths}`, `resync`; heartbeat every 25 s |
| `GET /_design/api/node?path=&node=` | private | `{tag, inner, inner_hash, gen}` of an editable element |
| `POST /_design/api/ops` | private | one person operation `{editor, op}` |
| `GET /_design/api/revisions?before=&limit=` | private | newest first, `{number, created_at, kind, authors, message, pinned, rollback_of, changes, files, size}`, plus `next` |
| `GET /_design/api/revisions/{n}` | private | one revision with its file list |
| `POST /_design/api/revisions/{n}/pin` | private | `{pinned: bool}` |
| `POST /_design/api/revisions/{n}/rollback` | private | roll back to `n` |
| `GET /_design/c/<cap>/<path>` | holder of `cap` | a file of the cap's scope: the head (artboards get the serving rewrite) or one revision (no bridge) |
| `GET /_design/export.zip` | all | the export transform over one coherent head snapshot |
| `GET /<path>` | all | a head file, for opening an artboard directly |

### Write protection

Every POST except `session` requires all of:

- private access;
- `Content-Type: application/json`;
- `X-Flats-Design: 1`;
- `Sec-Fetch-Site: same-origin`, or, when the header is absent, `Origin`
  equal to the request's scheme and host.

Artboard documents run at an opaque origin, so all of these fail for them.
`session` follows the same origin rule without the access requirement.

### Artboard isolation and capabilities

- **Sandbox.** Every design file response carries:
  - `Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads`;
  - `Referrer-Policy: no-referrer`;
  - `X-Content-Type-Options: nosniff`;
  - `Cache-Control: no-cache` with a content ETag.

  Artboards always run at an opaque origin, even when opened directly, and
  cannot call the API.
- **Capabilities.** An artboard at an opaque origin needs CORS to `fetch` its
  bundled data or load its fonts, but the files of a Private design must not
  be readable by other websites. So CORS is granted only to a capability
  path:
  - `POST /_design/api/session {revision?}` returns `{cap}`, a 128-bit random
    token bound to this flat, the access level of the request and one scope:
    the head, or the one revision named (private access only). It expires
    after 12 hours and is kept in memory, at most 256 per flat with the
    oldest evicted. Only the app shell can obtain one, because the origin
    rule blocks other websites and artboards.
  - An artboard learns its own token from its URL, so a token reads only
    what that frame shows. A live canvas never holds a revision token, and
    viewing a revision gets a token for that revision only.
  - The canvas loads artboards from `/_design/c/<cap>/<path>`, so relative
    URLs inside them resolve under the same capability.
  - Responses there carry `Access-Control-Allow-Origin: *`. A third party
    without the token gets 404.
  - A revision-scoped token also requires private access at request time.
  - Every use extends a token's expiry by 12 hours, so an open canvas keeps
    working; eviction removes the least recently extended.
  - Tokens die when the instance stops. The canvas renews its token after
    every event-stream reconnect (a restart also drops the stream) and
    reloads the frames that used the old one.
- **Direct opens.** `GET /<path>` sends no CORS on a Private route. Its fonts
  and fetches may fail when the file is opened directly, which the guide
  notes. On a Public route it sends `Access-Control-Allow-Origin: *`, because
  that content is world-readable anyway.
- **The app shell** sends:
  - `Content-Security-Policy: default-src 'self'; frame-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'`;
  - `X-Frame-Options: SAMEORIGIN`.

  Artboard iframes carry no `sandbox` attribute: some embedded browsers
  refuse sandboxed frames outright, and the response's CSP sandbox already
  puts every design file at an opaque origin.

### Events

Each design instance has a broadcaster.

- A write publishes `change` after its transaction commits.
- A new stream first sends `hello {seq}`.
- The client always subscribes before it reads state, then fetches state.
  When an event's `seq` is newer than the state it holds, it refetches. It
  also refetches after every reconnect and on `resync`.
- At most 32 streams per flat. Each queue holds 16 events; a full queue is
  cleared and gets `resync`.
- All streams close when the instance stops.
- Every SSE and export write runs under a 15-second write deadline. `Stop`
  cancels every request context, and export checks it before each file and
  each write. `Stop` also sets the write deadline of every open stream and
  export to now, which interrupts a blocked write. It then waits for the
  handlers to return before rename or delete moves the files.

### Rate limiting

Operations go through the flat's existing request limiter, plus a per-flat
operation limiter of 20 per second.

## Bridge and editing (`internal/design/web`)

The bridge runs inside the artboard and is untrusted by design: any script in
the artboard can send the same messages. The parent accepts a message only
when `event.source` is an iframe it created. It treats every message as a
hint and never as an authorized change.

- **Bridge to parent:**
  - `ready`;
  - `error {message}`;
  - `navigate {href}`, for a click on a link that resolves to an artboard of
    this design;
  - `pick {gen, node, rect}`, in edit mode, when an element with
    `data-flats-node` is clicked. `gen` is the file generation the host
    wrote into the bridge's script tag when it served those bytes, so node
    ids are always read against the generation they were numbered in.
- **Parent to bridge:**
  - `mode {view|edit|play}`;
  - `tweak {name, css}`, for live preview;
  - `outline {node}`, which highlights the edited element.
- **Editing text.** On `pick`, the parent:
  1. fetches `/_design/api/node?path&node&gen`. A `gen` other than the
     file's current one is refused with `conflict`, and the canvas reloads
     that frame before another selection. Otherwise it returns the current
     inner source, `inner_hash` and `gen`;
  2. opens its own editor, a `contenteditable` box in the canvas document,
     over the element's rectangle and prefilled with that inner source;
  3. sends `text.set` with the server's values when the person confirms with
     Enter, Done or a click outside. Escape cancels.

  The person's keystrokes go to the parent's editor, never to the artboard.
  An artboard script can therefore open an editor, but cannot type into it or
  confirm it. Every write needs input the person made in the canvas app.

## Canvas app (`internal/design/web/`)

The app is ES modules with no dependencies:

- `app.js`: boot, state, events and the toolbar;
- the canvas: pan and zoom (wheel, pinch, space+drag), fit and zoom steps;
- the toolbar badge shows the flat's visibility (Private or Public) and
  "view only" on a public route; a visibility change refreshes it live;
- frames: lazy iframes near the viewport, title strips clipped to the frame
  width at any zoom, error badges, move and resize handles;
- notes;
- focus and play;
- edit mode and the parent-side text editor;
- the tweaks panel;
- the history panel, with "load more" through `before`;
- a read-only revision view;
- print: one artboard per page, using named `@page` sizes.

Light and dark follow `prefers-color-scheme`. Buttons are labelled, and there
are shortcuts: `F` fit, `E` edit, `Esc` close. A public route hides every
editing and history control.

The editor id is a random id in `sessionStorage`. The author label is
"Person", or the Tailscale login when the host passes one (`TrustedEditor`).

## Agent surface (`internal/mcpx`)

- **`create_flat {type: "design"}`** calls `CreateDesign` with the starter:
  - `canvas.json`: one board, `main.html` 1280×800 at 0,0, and one note;
  - `main.html`: a small page with a tweaks block.
- **`save_design`**
  `{slug, files: [{path, content, encoding}], delete: [path], message?, expected_revision?, title?}`:
  - creates the design when the slug is absent;
  - is one revision, and leaves files it does not name alone;
  - returns `revision`, `changed`, `warnings` and `private_url`.
  - Its `warnings` are a light lint:
    - a board without `<title>`;
    - a `fill` board without a viewport meta;
    - an unpinned external script or stylesheet, reusing `bundle.Lint`'s
      check;
    - overlapping boards.
- **`get_design {slug, path?}`** returns the state, or one file's text or
  base64, with its hash and `gen`.
- **Revision tools.** They refuse non-revision flats, and documents until
  documents adopt this store.
  - `list_revisions {slug, before?, limit?}` returns newest first, at most 100,
    with `next`.
  - `get_revision {slug, revision, path?}`.
  - `pin_revision {slug, revision, pinned}`.
  - `rollback_revision {slug, revision}`.
- **Version tools.** `save_version`, `save_draft`, `save_page`,
  `save_version_from_dir`, `publish`, `deploy`, `rollback`, `open_preview`,
  `list_versions` and `get_draft` refuse a design, or a design manifest, with
  `ErrDesignUnversioned`.
- **Errors** reuse the existing categories:
  - `invalid` for validation;
  - `conflict` for stale operations, `expected_revision` and a full pin quota;
  - `not_found` for missing revisions and files;
  - the existing disk-quota `ValidationError`.

## Guidance

- **`topic.canvas`** ("Designs") is a new guide topic. It is added to
  `TopicOrder` in `docs/runtime_reference.go`, to the guide tool's
  description and to `contentTypesSections`. It covers:
  - when to choose a design;
  - the files, the index schema and the path grammar;
  - the authoring rules: editable text, tweaks, links and `fill`;
  - direct opens and capabilities;
  - concurrency, `gen` and revisions;
  - design-specific verification: `get_design`, the canvas URL, and no
    previews or screenshot field;
  - craft: frame sizes, layout that survives editing, contrast, no filler,
    and real assets.
- **`topic.design`** gains one scoping paragraph. Its screenshot, Draft and
  preview steps apply to websites and pages. Designs follow `topic.canvas`.
  The phone-width rule applies to `fill` boards, not to fixed-size posters.
- **Index and inventories.** `topic.index` routes to `topic.canvas`, and the
  instructions get one line within the 2,300-byte cap. The MCP tool inventory
  test and the generated references are updated.

## Console (`internal/console`)

- **List.** `TYPES.design = {label: "Design", icon: "layout"}`, with a new
  `layout` icon and `--type-design` colors for both themes. `hasServer` is
  false for designs.
- **Overview.** Designs reuse the document overview (no versions or previews)
  with an "Open canvas" link.
- **Settings tab.** "Revision history" shows the two overrides, saved through
  `PUT /console/api/flats/{slug}/revision-quota`. Host settings gain both
  defaults.

## Configuration

`system.revision_quota` (default 100) and `system.pin_quota` (default 20).
Both are integers ≥ 0, and 0 means unlimited. A lowered value applies
immediately (see `retainTx`). Both are documented in
`docs/configuration.md`.

## Tests

- **store:** the migration, cascades on rename and delete, revision numbering.
- **core:**
  - creation and its recovery from an `init` row;
  - public bundle paths refused for design manifests, including absent slugs;
  - writes, validation and the path grammar (case and prefix collisions);
  - each operation's guards, including `gen` after an agent replace and
    stale `inner_hash`;
  - per-editor group deadlines with a fake clock, and the commit before
    agent writes and rollbacks;
  - rollback protecting R under quota 1;
  - retention counting the newest revision, lowered and zero quotas, and pins;
  - the disk quota for writes and operations, never for rollback;
  - the sweep, with grace, leases, orphans and a WAL checkpoint;
  - teardown on rename and delete.
- **design:**
  - the scanner table;
  - sanitizing;
  - tweaks and CSS escaping;
  - canvas validation;
  - ops.
- **handler:**
  - public refusals and the origin and header rules;
  - capabilities: issue, expiry, CORS only under a cap, 404 for an unknown
    token, and private-only revision files;
  - the CSP sandbox and the rewrite;
  - SSE `hello`, `resync`, the stream cap and closing on stop;
  - export contents, with no bridge or node ids;
  - revision paging.
- **mcpx:** the tool inventory, `create_flat` and `save_design`, the revision
  tools, refusals and guide topics.
- **console UI:** the design fixture.
- **docs:** the generated references.
- **Manual:** a dev host with a seeded design, in a browser.

## Dev host

The consoledev seed adds `studio`: a three-artboard prototype with links,
tweaks and notes.

## Out of scope

The PRD's non-goals, and moving documents onto this store. Documents keep
`flats_docs_conflicts` until they move, as content-models' "Current state"
records.
