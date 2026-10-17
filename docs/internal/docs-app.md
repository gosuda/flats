# Docs app: host integration and internals

This is maintainer documentation for the built-in collaborative Markdown app
(`internal/apps/docs`) and its host integration. It is not agent-facing: the
agent contract for `type: "docs"` is the generated
[content types reference](../content-types.md) (guide `topic.docs`), and none
of the helpers below are part of runtime API v1.

A document has no Draft, versions, previews or publishing for the agent: a
save that changes the bundle starts it at once, without approval. Internally
each changed save is stored as the next running version, with exactly the
uploaded files, and its `kind` is recorded as `server` (it runs in a worker
and owns live data). Views report `type` next to `kind`. Each changed save
first runs the health check on an isolated copy of the data; a failure
refuses the save and leaves the running document as it was. A document keeps
no data snapshots, and previews of a document are refused.

A changed save moves the flat's live version, and so does a revision
rollback whose target has other non-document files or another set of
documents. Live edits by people and through `update_document`, and a
rollback within the running version, do not. A visibility approval of a
document does not freeze the live version, so none of these makes a pending
one stale; approving it exposes the document as it is then
([content models](../spec/content-models.md), Revision model rule 2).

## Docs runtime layout

When a docs version starts (live, or for the health check), core materializes a read-only runtime directory under
`host.data_dir` from `config.json` (see [configuration and storage](../configuration.md)):

```
<data>/runtime/docs/<app-hash[:12]>-<content-hash[:16]>/
  server.js, ...      the embedded app (internal/apps/docs, FS())
  content.js          generated from the version
```

It is written to a private temporary directory, made read-only (files 0444)
and renamed into place; an existing directory with the same name is reused.
Directories no longer referenced by any running instance may be removed.
The container’s writable `/data` volume includes this runtime directory; no
extra image dependency or writable root filesystem is needed.
The worker is started with that directory as `Dir` and `server.js` as
`Entry`, and with the flat's normal data directory, secrets and limits.

`content.js` is a single generated ES module (JSON is valid module syntax):

```js
export default {
  "format": 1,
  "hash": "<version content hash>",
  "entry": "index.md",
  "title": "<manifest name, or empty>",
  "documents": [{"path": "index.md", "hash": "<sha256 hex of the file bytes>", "text": "..."}],
  "assets": ["images/diagram.png"]
};
```

Documents are sorted by path. Assets are every non-Markdown file except
`flats.json`.

### Host routing for docs flats

The docs handler is the worker plus a thin host wrapper:

* `GET`/`HEAD` of `/<asset path>` for a listed asset is served by the host from
  the version directory (static file semantics), never by the worker.
* Everything else goes to the worker.
* Every asset response has `X-Content-Type-Options: nosniff` and a sandboxing
  Content Security Policy that forbids scripts, connections and same-origin
  document access; it allows only inline styles and same-site or `data:`
  images, so an SVG or image opened directly still renders. Audio and video
  get `default-src 'none'; media-src 'self'` without `sandbox`, because
  browsers do not play a media file opened in a sandboxed document.
  Only known image (raster and SVG), media, font, and plain-text extensions are
  served inline, with fixed MIME types. Other files, including HTML,
  JavaScript, compressed SVG (`.svgz`) and PDF, use `application/octet-stream`
  and `Content-Disposition: attachment`.
  This applies to already-saved bundles as well as new uploads.

### Trusted access header

Core sets `X-Flats-Access` on every request it hands to a flat handler (all
types) and deletes any client-sent value first:

* `private` — a route of a private provider (Local, Tailscale, NetBird).
* `public` — a route of a public provider (for example Portal or Tailscale
  Funnel).

Apps must treat a missing or unknown value as `public`. Identity headers
(`Tailscale-User-Login`, `Tailscale-User-Name`) keep their existing meaning:
set from WhoIs on Tailscale, stripped on public routes.

Core also strips every client spelling of `X-Flats-Health` and
`X-Flats-Host-Op` on private, public, and preview routes. Only its own health
trial sets `X-Flats-Health: 1`. Only core's own calls on the worker set
`X-Flats-Host-Op`, after stripping client headers: `edit` for a live edit and
its receipt lookup, `revisions` for the [revision host
operations](#host-operations). Private access by itself authorizes neither
health activation nor a host operation.

## Docs app contract (internal/apps/docs)

The app owns the flat's whole origin. Reserved paths start with `/_docs/`.
`FS()`, `Entry = "server.js"`, `ContentModule = "content.js"` and `Hash()`
remain the host interface. The committed `dist/server.js` is one ESM bundle,
including the client JavaScript and CSS as strings; its only static external
import is the host-generated `./content.js`. `dist/mermaid-asset.js` exports
the Mermaid browser bundle as a string and is dynamically imported only to
serve the Mermaid route. No Node, CDN or outbound network is used
at runtime. `dist/THIRD_PARTY_LICENSES.txt` contains bundled licenses and
`dist/BUILD-INPUTS.sha256` binds the source inputs and every other dist file; a Go test verifies both.

| Route | Purpose |
|---|---|
| `GET /` | entry document workspace for `private`, live rendered view otherwise |
| `GET /<path>` | exact Markdown path, or `<path>.md` / `<path>.markdown` |
| `GET /_docs/healthz` | Cheap `200 ok` without DB access for visitors; only trusted host health trials activate all documents atomically |
| `GET /_docs/api/documents` | `{format:1, entry, title, documents: [{path, title}]}` |
| `GET /_docs/api/document?doc=<path>` | `{format:1, doc, markdown, hash, epoch, seq, chain, source:"live"}`; private/host reads with `blocks=1` add one page (1,000 blocks from `block_offset`) of the block outline `blocks:[{hash,kind,level?,line,preview}]` plus `blocks_total`; default doc is entry |
| `POST /_docs/api/edit?doc=<path>` | Host management only (`private` plus `X-Flats-Host-Op: edit`, see below): guarded live edit; others receive 403 |
| `GET /_docs/api/receipt?doc=<path>&id=<id>` | Host management only: `{committed, seq?, chain?}` for a live-edit receipt id |
| `/_docs/api/revisions…` | Served by the host before the worker (private only): the document page's history; see [history routes](#history-routes) |
| `/_docs/host/revisions/…` | Host management only (`private` plus `X-Flats-Host-Op: revisions`): revision capture, outbox, staging, intents and restore; see [host operations](#host-operations) |
| `GET /_docs/assets/client.js` | self-contained browser ESM bundle |
| `GET /_docs/assets/client.css` | workspace CSS |
| `GET /_docs/assets/mermaid-<hash>.js` | Mermaid browser ESM, loaded by the client only for documents with diagrams; `<hash>` is the first 16 hex digits of its SHA-256, and it is served `immutable` |
| `GET /_docs/ws?doc=<path>` | collaboration WebSocket; default doc is entry |

HEAD has GET semantics without a response body. Other HTTP methods receive
405, except `POST /_docs/api/edit`. Unknown/removed documents and reserved routes receive 404. WebSocket
admission happens in `open`, after the runtime handshake; invalid paths or
cross-origin browser connections are closed with 1008. Private hostnames are
validated at ingress: Local routes require `<flat>.localhost`; Tailscale
accepts only the node name, known DNS/certificate names and node IPs, including
when plain HTTP is used. Origin must still match the admitted request origin.

HTML escapes interpolated titles and paths, sets `nosniff`, `Referrer-Policy:
same-origin`, and a CSP allowing same-origin scripts, same-host HTTP/WebSocket
connections, same-origin/data images, and the inline styles CodeMirror needs.
Raw HTML is disabled in Markdown; unsafe links are rejected. Relative links
and image paths resolve relative to the document's directory. Preview is
rendered in the client; assets in `content.assets` are still the host's responsibility.

### Collaboration model

* Yjs **13.6.33**: one `Y.Doc` per document, with one `Y.Text` named
  `markdown`. Only plain-text Yjs updates are accepted; other shared roots,
  maps, embeds, nested types and formatting attributes are rejected. Korean
  text and emoji remain Markdown source. The client uses CodeMirror 6,
  `y-codemirror.next`, and a `Y.UndoManager` that tracks only its local binding
  origin (including its own undo/redo), never remote or activation changes.
* The serial WebSocket VM has one room per connected document. HTTP VMs load
  committed state per API request and keep no mutable document replicas.
  The last connection leaving destroys its room. The server creates no
  awareness timers; browser heartbeats drive catch-up, and new admissions
  opportunistically close idle sockets.
* **The SQLite log is authoritative.** Writes use `BEGIN IMMEDIATE`; already activated API reads and heartbeats
  use a deferred read snapshot and do not reserve SQLite's writer lock.
  A room catches up from committed rows before each append or heartbeat,
  including rows from overlapping workers. If compaction moved past its
  watermark, it reloads the checkpoint and broadcasts committed full state.
  No ACK or document broadcast is emitted before COMMIT. Transient database, catch-up, obsolete-worker and COMMIT failures roll back,
  drop the room and close its clients with 1012 for resync. Deterministic
  client rejections send `error` (`capacity`, `invalid_update`, or `readonly`)
  then close only that socket with 1008; healthy peers stay connected.
  Bounded binary/type properties are checked before applying. A deterministic
  rejection after applying invalidates only the room's cached document; peers
  stay connected and the next operation reloads committed state. No rejected
  text is broadcast or retained in the usable cache.
* Tables are created idempotently on first document use, all with the
  `flats_docs_` prefix:
  * `documents(doc PRIMARY KEY, epoch, seq, chain, seed_hash, seed_text, updated_at)`
  * `updates(doc, seq, id, chain, data, size, created_at, PRIMARY KEY(doc,seq), UNIQUE(doc,id))`
  * `checkpoints(doc PRIMARY KEY, seq, chain, state, created_at)`
  * `receipts(doc, seq, id, chain, hash, PRIMARY KEY(doc,seq), UNIQUE(doc,id))`
  * `activations(doc, hash, version, PRIMARY KEY(doc,hash))`
  * `conflicts(doc, generation, markdown, PRIMARY KEY(doc,generation))`: no
    longer written; [initialization](#initialization-and-migration) migrates
    its rows into revisions
  * `meta`, `recorded`, `groups`, `outbox`, `texts`, `ops`: revision capture,
    see [worker tables](#worker-tables)
  Binary state/update data is canonical base64 TEXT; `updates.size` counts
  stored base64 bytes. Receipts retain SHA-256 of canonical update data,
  exact ids and chain history after log compaction. They are bounded and
  never silently discarded. An identical retry returns its original ACK;
  the same id with different bytes is rejected. `seed:`, `activation:` and
  `agent:` (host live edits) id prefixes are reserved. Initial seq is 0 and chain is SHA-256 of the
  random epoch; each subsequent chain is SHA-256 of
  `previousChain + "\n" + updateId` (UTF-8).
* At 64 log rows or 512 KiB of base64 log data, compaction encodes the Yjs
  state at the explicit current seq, upserts its checkpoint, and deletes
  only rows `seq <= watermark` in the same transaction. It preserves the
  CRDT identities, epoch, receipts and acknowledged edits. Replays contain
  at most 64 rows. The application does not rebuild a CRDT from visible
  Markdown during compaction.
* Seeding and activation merge run inside the same transaction as the hash
  check. Merge is line-based (base = stored seed, ours = live text, theirs =
  version text); disjoint and adjacent changes survive, overlapping edits
  prefer the version. Myers line diff has no wall-clock timeout: each diff is bounded to 128
  edits and 16,000 combined input lines. Native newline searches count lines
  before allocating arrays; there is no additional character cap. On overflow or a combined result above 1 MiB the version wins;
  the text a merge discards is in the revision captured just before it:
  every new version is activated for the whole flat at once, and that
  activation first records people's unrecorded text in the same
  transaction ([flat activation](#flat-activation)).
  A deterministic character diff is bounded to 64 edits and 65,536 combined
  UTF-16 code units; overflow replaces only the differing prefix/suffix span.
  Only the trusted host health signal activates every source document
  atomically on the isolated health copy, so a failure refuses the save
  before the live database is touched.
  Normal reads, joins and commits can still seed/activate the live database.
  Visitor health requests never read the DB or run activation.
* The host allocates a strictly increasing activation generation for each
  runtime start, including the save-time health trial, the live start, host
  restart and worker crash recovery. The `runtime_generation` counter in
  `flats.db` is created by schema migration 9, independently of host settings
  in `config.json`, and is
  atomically committed before startup; failed starts consume a generation.
  Its initial value exceeds stored published version numbers (including the
  earlier app's version metadata), and subsequent starts increment it regardless
  of wall-clock changes. Crash recovery is allowed only for the newest runtime
  using that data directory: draining workers cannot restart with fresh source
  authority. Failed startup restores the previous runtime's recovery ownership.
  Stopping a candidate restores the preceding runtime's recovery claim;
  explicit retirement clears ownership. It fails closed at JavaScript's maximum safe integer.
  `Version` remains the running version number used in logs.
  The app records the largest applied generation in the legacy `activations.version`
  column. A newer generation advances the fence even when the source is unchanged.
  When its source hash differs from `seed_hash`, it merges base = current
  `seed_text`, ours = live text, theirs = target text. Revisited hashes are
  allowed: a save that returns to earlier content merges like any other,
  retaining independent human edits. A revision rollback ends with the exact
  target text instead of a merge result: it [restores](#restoring-text) that
  text and makes it the merge base.
  Activation update ids include the generation, so overlapping
  workers cannot apply the same merge twice. Older workers catch up and may
  append people's updates, but never merge obsolete source or seed a missing
  document after a newer generation was observed anywhere in that database.
  Removed documents retain stored state but the current app never lists or
  opens them. The health trial runs on a copy of the data with a fresh
  generation; its merges do not affect live data.
* Presence is ephemeral and owned by one awareness client id per socket.
  Other sockets cannot replace that id's state. The server validates cursor
  positions, overwrites user identity/color, relays states, and broadcasts
  removal on close. Tailscale name/login is shown with `verified:true`;
  otherwise the hello name is shown with `verified:false`. Presence is relayed
  only to private editors; public/read-only sockets receive no presence and
  cannot submit it. The browser asks
  once per visit when no saved name exists; localStorage is best-effort.
* Only `x-flats-access: private` may submit document updates. Missing,
  unknown, or public values are read-only. Public viewers receive live
  Markdown rendering without constructing an editor. Private pages open in
  Edit and can toggle to View, the same rendered surface, without
  reconnecting; View is presentation only and the socket stays writable. Authorization remains
  the host's trusted route decision for the lifetime of that connection;
  core cancels the public route epoch when exposure is withdrawn, including
  Private transitions, route stops, deletion and rename. The runtime proxy
  closes upgraded connections on request cancellation. Preview closure cancels
  its epoch too. Re-exposure creates a fresh epoch; stale handlers stay revoked.
* For honest clients, receiving state never means Saved: every local id must
  have a durable ACK. Pending updates remain in memory across reconnects,
  with exact ids and payloads. They are not persisted across browser reloads.
  Reconnect uses exponential backoff with jitter, state vectors, browser
  online/offline events and a 45-second heartbeat silence timeout.
  Divergence or an unrecoverable rejection stops sync, disables editing and
  offers local Markdown download and reload; buffered edits are retained.

### Agent live edits (MCP `update_document`)

Design for guarded agent edits of live Markdown, modelled on Claude Docs'
update operations. The agent contract is guide `topic.docs`.

* **Path.** MCP `update_document` → `Service.UpdateDocument` (per-flat lock,
  10 edits/s burst 20 per flat) → in-process `POST /_docs/api/edit` on the
  running docs handler with trusted `private` access and
  `X-Flats-Host-Op: edit`. Only a running docs version is edited; without one
  the call refuses with `not_deployed` and the hint "save it with
  `save_document` (it starts at once)". The request carries a `label` that
  credits the revision (`Agent` for MCP). Agent-authored server flats never
  see the header, and a
  browser on a private route cannot add it (core strips it), so the endpoint
  is not a cross-site write path.
* **Same authority as people's edits.** One `BEGIN IMMEDIATE` transaction runs
  `activate` (catch-up and any pending activation merge), resolves the
  operations against the committed text, applies them as minimal `Y.Text`
  deletes/inserts in one Yjs transaction, checks `validateUpdate`, the
  256 KiB decoded update limit and `validateIncremental` (1 MiB Markdown,
  state and structure limits), `append`s one update with a unique
  `agent:<random>` receipt id chosen by core (a reused id is refused), and
  `compact`s. Nothing is reported before
  COMMIT; any refusal rolls the whole transaction back. The CRDT is never
  rebuilt from Markdown, so concurrent edits elsewhere keep their positions.
  Splices are trimmed to the characters that actually change, without
  splitting surrogate pairs.
* **Broadcast.** The edit runs in an HTTP VM, which holds no rooms. After
  COMMIT it calls the non-enumerable `globalThis.__flats_docsWake(doc)`; the
  runtime posts a `wake` event to the worker's WebSocket VM without blocking
  (dropped when its queue is full), and the app's `websocket.wake` handler
  catches the room up exactly like a heartbeat and broadcasts the committed
  rows. A dropped wake, or a room in an overlapping worker, catches up at the
  next heartbeat (about 15 s).
* **Operations** (`edit.js`, pure): `replace {find, with, nth?}`,
  `replace_block {block, with, nth?}`, `delete_block {block, nth?}`,
  `insert {text, before|after|section_end: block | at: start|end, nth?}`.
  1–32 operations apply in order to the evolving text; the request is at most
  512 KiB. Block operations build one block index per call (split, then all
  hashes in batched `digests` codec calls), keep it in step with each splice
  by re-parsing only from the block before the change until old boundaries
  line up, and look blocks up through a hash map. They are refused on
  documents of more than 50,000 blocks (`capacity`): at roughly 30 µs per
  block in the worker that keeps a call far inside the request deadline. Blocks are maximal runs of non-blank lines; a fenced code block
  (including blank lines) is one block and an ATX heading line is its own
  block. A section is a heading plus the blocks up to the next heading of the
  same or a higher level. Inserted text is trimmed of outer newlines and
  separated from neighbours by one blank line.
* **Guards** ("their edit wins"). A block is named by the first 16 hex digits
  of SHA-256 of its exact text, from `get_document {blocks: true}`; a human
  edit changes the hash. `find` must match exactly and uniquely unless `nth`
  is given. `nth` is positional, so a concurrent identical insertion before
  the target would move it: any op with `nth` requires `if_hash`. The block
  outline is paged (1,000 blocks) so a 1 MiB document of one-line blocks
  stays within response limits. Optional `if_hash` (SHA-256 of the whole live Markdown, returned
  by every `get_document`) refuses any intervening change. A missing,
  changed or ambiguous target returns 409 `edit_conflict` with the operation
  index; malformed input 400 `invalid`; limits 413/422 `capacity`, mapped to
  `document_capacity`. All refusals change nothing. Content-addressed guards
  replace Claude Docs' block ids and revisions, which this CRDT text has no
  stable equivalent of: an unchanged block keeps its hash even while people
  edit other blocks, so independent work does not invalidate the guard.
* **Saves.** Live edits are not part of the saved bundle. The next save that
  changes a document merges `seed_text` → live → saved text exactly as for
  human edits: disjoint agent edits survive, overlapping ones prefer the
  save, and the live text before the merge is the revision captured just
  before it.
* **Audit and recovery.** Core records each applied edit in the flat event
  log (kind `document`): document, operation, line and removed/inserted
  counts, seq before/after, receipt id and resulting hash, never the text.
  The receipt id prefix `agent:` identifies agent updates in the update log;
  WebSocket editors cannot use it. When the edit's response is lost (worker
  failure, timeout or an unreadable reply), core looks its receipt id up
  through `/_docs/api/receipt` and always logs a `warn` event. The worker
  keeps working after a disconnect, so a missing receipt is conclusive only
  after the edit's commit deadline: core sends `deadline_ms` (12 s after
  sending), the app rolls back instead of committing after it (checked
  immediately before COMMIT), and core looks again once the deadline plus a
  2 s margin has passed. The result is "committed at seq N", "did not apply"
  or, only when the app cannot answer, "outcome unknown". Core waits up to
  20 s for the edit itself.
  Each applied edit is a revision, recorded in the edit's own transaction
  together with a revision of people's unrecorded edits before it, so the
  text before the edit can be restored with `rollback_revision`. The
  `update_document` result reports that `revision` once the host has
  imported it.
* **Public flats.** Live text is public at once. The tool description,
  MCP instructions, `topic.docs` and every result on a Public flat
  (`public_notice`) say so; there is deliberately no approval step, matching
  private human editing.
* **Not done.** Caller-supplied idempotency keys (each MCP call gets a new
  receipt id; a lost response is reconciled as above, so the agent learns
  whether to repeat), and a console/HTTP management API for the same
  operation.

### Built-in host internals (outside runtime API v1)

The built-in app also selects non-enumerable `globalThis.__flats_docsCodec`
for bounded host-native base64, UTF-8 and SHA-256 serialization. Binary inputs
are capped at 1.5 MiB; digest/byte-count strings at 4 MiB UTF-8. The server build
binds lib0's encoder/decoder to it without adding TextEncoder/TextDecoder
globals. It has no I/O authority and is not a runtime API v1 capability.

The built-in app requires read-only, non-enumerable `request.runtimeGeneration`
and `ws.runtimeGeneration`, supplied from `RuntimeSpec.Generation`, and
`ws.setSendLimits(connectionBytes, flatBytes)` (a non-enumerable prototype
method). The codec's `digests(list)` returns the concatenated SHA-256 hex digests of
up to 1,048,576 strings (8 MiB of JSON input) in one host call.
`globalThis.__flats_docsWake(doc)` (non-enumerable, every JS VM) and the
`wake` WebSocket event it produces are internal in the same way.
These are host internals for built-in apps, **not part of runtime API
v1** and not supported APIs for agent-authored server flats. They are visible
in JavaScript but do not change existing v1 request fields or queue defaults.
The byte helper bounds actual queued **and in-flight** Go text payload bytes,
only lowers budgets, and accepts connection budgets of 1 KiB–4 MiB and flat
budgets between that connection budget and 16 MiB (the host defaults). App delivery receipts also
bound unprocessed output; native budgets cannot be bypassed by forged receipts.
Adopting this embedded app requires the matching host implementation.

### Wire protocol (JSON text frames, version 1)

Binary fields are canonical RFC 4648 base64. Client → server:

| `t` | Fields | Meaning |
|---|---|---|
| `hello` | `v:1, doc, sv, known?:{epoch,seq,chain}, name?, editor?` | join; client state vector, last observed committed history, and the browser's random editor id that groups its edits into revisions |
| `update` | `id, u` | local update, resent unchanged until ACK |
| `aw` | `u` | one awareness client id, owned by the socket |
| `ping` | — | heartbeat/catch-up, about every 15 seconds |
| `received` | `n` | cumulative server-envelope delivery receipt; releases app output credit, never acknowledges a document edit |

Every server envelope has a monotonically increasing per-socket `n`:

| `t` | Fields (besides `n`) | Meaning |
|---|---|---|
| `welcome` | `v:1, doc, epoch, seq, chain, readonly, u, sv, you:{name,verified}` | server diff missing from client sv and server vector |
| `update` | `u, seq, chain` | committed peer update or catch-up/full checkpoint |
| `ack` | `id, seq, chain` | exact id durably committed; a retry can ACK an older seq |
| `aw` | `u` | relayed/rewritten awareness state or removal |
| `pong` | `epoch, seq, chain` | catch-up watermark |
| `diverged` | `epoch, seq` | known epoch/seq/chain is absent from retained committed history; do not push local state |
| `error` | `code, message` | rejected message; fatal rejection closes socket |

The provider merges the welcome diff and resends every pending id; all local
changes are already represented in that buffer, so no replacement snapshot
is pushed. Newer committed watermarks advance `known`; older retry ACKs do
not rewind it. A missing `known` is for fresh replicas. On restore,
retained chain receipts detect lost/forked acknowledged history even when
the database's original epoch was restored with it.

Final application limits (bytes mean UTF-8 or decoded binary as indicated):

| Resource | Bound |
|---|---|
| Incoming JSON envelope | 512 KiB UTF-8 |
| Decoded document update | 256 KiB; 4,096 structs; 1,024 delete-set clients / 4,096 ranges |
| Markdown per document | 1 MiB UTF-8 |
| Encoded Yjs state | 1.5 MiB; 20,000 integrated structs per document |
| Active rooms | editors: 8 / 4 MiB / 30,000 structs; public: separate 4 / 2 MiB / 20,000 structs |
| Connections | editors: 50 per document / 200 per instance; public: separate 10 / 40 |
| Awareness | 8 KiB decoded; one id per socket; 2 KiB cursor; 20/s with burst 20 per connection |
| Incoming connection rate | 60 frames/s, burst 120; 256 KiB/s, burst 1 MiB |
| Document update room rate | 40/s, burst 80; 512 KiB/s, burst 512 KiB (wire bytes) |
| App outstanding output / native queued output | 4 MiB per connection, 16 MiB per worker instance, including native in-flight frames |
| Browser pending update buffer / WebSocket bufferedAmount | 2 MiB each; one edit still must fit decoded update limit |
| Revision capture | Pending outbox entries and their texts until the host imports them; recorded texts of current documents |
| Persistent admissions | Upload validates at most 128 Markdown documents (with a fix hint); 128 stored documents; seq <= 100,000 per document; 200,000 total receipts; 1,000 content hashes per document |
| State-vector hello | 8 KiB decoded, 1,024 clients |
| Idle sockets | unfinished hello 15 s; joined silence 60 s; checked on new admission |

A cached room replays only new committed rows; unchanged heartbeats do not
rebuild, decode, validate or serialize document state. Reload/replay validates
once after all rows. Markdown size checks use `Y.Text.length * 3` as an O(1)
UTF-8 upper bound, computing exact bytes only near the cap, without percent
encoding. Near a state limit, the app checks exact encoding; otherwise it uses a
conservative increment bound, reset by each checkpoint. No accepted edit or
receipt is evicted to make room. Capacity rejection requires local download
and operator recovery/new data; compaction does not reset these history
limits. These are admission/work bounds, **not** a measured peak JS-heap or
worker-RSS guarantee. The runtime's 1 MiB message limit, 64 incoming events,
256 outgoing messages and callback/heap limits still apply. Oversized peer
state can be up to approximately 2 MiB of base64 in a welcome; the 512 KiB
limit applies to client → server envelopes.

Internal VM errors escape the WebSocket handler, causing runtime VM replacement
and retryable connection closure. Clients reconnect and resync from committed
SQLite state. Rejected updates invalidate the cached document while retaining its
last committed position, so another worker's compaction triggers a full reset.

## Document revisions

A document follows the Revision model of
[content models](../spec/content-models.md): agent writes, people's edit
groups and rollbacks create whole-flat revisions. The capture happens in the
docs worker, because people's edits never pass through the host; the host
keeps the history. This section is the worker side. Importing, the host's
revision store, quotas, pins, holds and the rollback transaction are in the
[revision store](../revision-store.md).

* **The worker captures revisions.** Every revision boundary (an agent
  write, a closing edit group, a rollback, a new version's merge) happens
  inside one worker transaction, the same one that changes the text. The
  worker records the whole flat at that boundary in an **outbox** in its
  database. Nothing can commit between the capture and the change, and a
  crash keeps both or neither.
* **The host keeps the history.** It imports outbox entries in order and
  idempotently into its revision store and acknowledges them, after which
  the worker drops them.
* **Operations that span a version switch are durable in the worker.** A
  save or a rollback that starts a new version first writes an
  [intent](#intents) into the worker database, which the old and the new
  version share. The new version's activation consumes it in its own
  transaction, so the operation completes whenever the new version first
  runs, even after a host crash.

### Worker tables

Created idempotently with the other tables, all `flats_docs_` prefixed:

```sql
meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)
  -- 'store':  random id of this database's outbox, set once
  -- 'newest': JSON {source, documents: [{path, hash, size}]} of the newest entry
  -- 'active': JSON {hash, generation, docs: [path]}: the version whose flat activation last ran
  -- 'intent': JSON, at most one (see Intents)
recorded(doc TEXT PRIMARY KEY, epoch TEXT NOT NULL, seq INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL)
  -- each document's committed position when last recorded, and its text hash there
groups(editor TEXT PRIMARY KEY, label TEXT NOT NULL, started_at INTEGER NOT NULL, last_at INTEGER NOT NULL, dirty INTEGER NOT NULL)
  -- open edit groups; dirty: edited since the last entry
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

An entry names its manifest and assets by `source`, the content hash of a
version, not file by file: a bundle may hold 20,000 files, whose list must
not travel through the outbox. The host keeps the file list of every source
it started. Documents are listed in the entry itself.

**Text references.** `refs` counts every place that needs a text: one per
document of each unacknowledged entry (two paths with the same text count
twice), one per `recorded` row and one per document of a pending rollback
intent. A text therefore stays in `texts` while it is the recorded text of a
document, even after every entry that listed it is acknowledged, and later
entries reuse it without loading the document. Every operation that releases
texts ends by deleting rows with `refs <= 0`, except staged rows (a non-null
`staged_at`), which wait for the operation that staged them; `sync` deletes
unreferenced staged rows after one hour.

### Recording

`record` runs inside the caller's transaction and appends one outbox entry
for a shape (a source and a list of document paths):

1. Build the entry's documents. A document whose committed `(epoch, seq)`
   equals its `recorded` row reuses that row's hash and size. Any other
   document is loaded from committed state (or taken from text the caller
   already has) and hashed, its text is inserted into `texts` or found
   there, and its `recorded` row moves to the new text, adjusting both
   texts' `refs`.
2. Overrides map paths to explicit texts (migration, and the text before an
   activation merge). They are stored like loaded texts but never touch
   `recorded`.
3. When the entry is not forced and its source and documents equal
   `meta.newest`, stop: no entry.
4. Otherwise insert the entry, add a reference to each listed text, set
   `meta.newest`, clear `dirty` on every group, and for a rollback record its
   operation id in `ops` with the entry's seq.

Authors are the given label (an agent's or a rollback's), else the labels of
the groups that are `dirty` at that moment, in first-edit order; at most 32,
each at most 200 characters. The message is at most 500 characters. Entry
kinds are `created`, `agent`, `person`, `rollback` and `recovered`.

Shapes:

* **current:** source `content.hash`, and every document of
  `content.documents` with its committed text.
* **before:** the source of `meta.newest`, and every document that
  `meta.newest` lists with its committed text now: the flat as people last
  saw it, while this worker already runs a newer version whose merge has not
  run.

**Text dirty**: some document listed in `meta.newest` has a committed
`(epoch, seq)` other than its `recorded` row.

### Flat activation

A version is activated for the whole flat at once. Every path that needs a
document (browser join, catch-up, update, read, the host's health trial, the
edit endpoint, and the `sync`, `intent` and `restore` host operations) first
passes one gate: when `meta.active.hash` differs from `content.hash` and this
worker's generation is neither below the highest activation generation nor
below `meta.active.generation`, the flat activation runs in that
transaction:

1. Initialize the store when needed ([below](#initialization-and-migration)).
2. When `meta.intent` names this version (`source = content.hash`) and is a
   rollback: when text dirty, record a `person` entry of the **before** shape
   and delete every group; restore every document to the intent's text
   ([restoring text](#restoring-text)); record a forced `rollback` entry of
   the current shape with the intent's label, message, `rollback_of` and
   operation id.
3. Otherwise merge every document (seed and line merge). When text dirty, a
   `person` entry of the **before** shape keeps people's text as it was
   before the merge, so nothing a merge discards is lost. Delete every group,
   then record an `agent` entry of the current shape, crediting the intent's
   label and message when an `agent` intent names this version, and no
   author otherwise. Identical content records no `agent` entry.
4. Release any intent, delete every group and set `meta.active` to this
   version's hash, generation and document paths. Delete the `recorded` rows
   of documents this version no longer has, releasing their texts: the
   entries above hold what they last contained.

A restart with the same content (only a new runtime generation) passes the
gate without activating and records nothing. A worker older than the active
one never activates or records; it may still serve reads while it drains.
The merge writes nothing to `flats_docs_conflicts`.

**Stale writes.** A browser update reaching an older worker after a newer
activation is refused with `readonly` when the active version no longer has
that document (`meta.active.docs`), so an edit to a removed document cannot
commit. Edits to documents that still exist commit and reach the next entry
of the active worker.

### Revision boundaries

| Boundary | Where | In the same transaction |
|---|---|---|
| a person's update | WebSocket `update` (not a duplicate) | upsert `groups[editor]`: `started_at` kept or set, `last_at` = now, label = the editor's shown name, `dirty` = 1 |
| an agent live edit | `POST /_docs/api/edit` | when text dirty, a `person` entry (current shape); delete all groups; apply the edit; when the text changed, an `agent` entry crediting the request's `label`, with the edit's receipt id as its operation |
| a new version | flat activation | see above |
| edit groups closing | `POST /_docs/host/revisions/sync` | when a group is due (idle 30 s, or begun 5 minutes ago) and text dirty: a `person` entry (current shape); then delete the due groups |
| a rollback within the running version | `POST /_docs/host/revisions/restore` | when text dirty, a `person` entry; delete all groups; restore every document; a forced `rollback` entry with its operation id |

* Person and agent entries are not forced: identical content makes none.
* An agent edit that changes nothing records nothing. The groups it deleted
  had no unrecorded edits.
* Groups that are not due survive a `person` entry but lose `dirty`, so a
  later entry credits them again only after they edit again. A due group
  without unrecorded edits is just deleted.
* The host calls `sync` every 15 seconds for each running document, so a
  group closes at most about 15 seconds after it is due.

**Editor identity.** `hello` carries `editor`, a random id (8–64 characters
of `A-Z a-z 0-9 _ -`) the page keeps in local storage next to its
collaborator name. The group key is `ts:<login>` for a verified Tailscale
login (`Tailscale-User-Login`), else `browser:<editor id>`; the label is the
verified name or login, else the collaborator name. A connection without a
valid id gets a random one.

### Intents

`meta.intent` holds at most one pending operation that starts a new version:

```json
{"id": "<op id>", "source": "<content hash>", "kind": "agent",
 "label": "...", "message": "...", "rollback_of": 12,
 "documents": [{"path": "...", "hash": "..."}]}
```

`kind` is `agent` or `rollback`; `rollback_of` and `documents` (at most 128)
belong to a rollback. Operation ids are 8–120 characters of
`A-Z a-z 0-9 _ -`; labels are cut to 200 characters and messages to 500.

* The host writes it through the running (old) worker just before it starts
  the new version, under the flat lock. Writing an intent replaces an older
  one. The host writes no intent for a save that starts no version.
* Flat activation consumes it as above. `sync` runs the activation gate first
  and then deletes any intent that remains: the version it named either never
  started (a failed health check, a crash before the switch) or already
  activated. The host calls `sync` after every operation and at startup, so an
  intent does not outlive its operation.
* A rollback intent's texts must be staged (`stage`) or present before the
  intent is written; the intent holds one reference on each until it is
  consumed or deleted.
* Writing an intent runs the activation gate in a savepoint first: when the
  running version's own activation fails on a history limit, the attempt is
  undone and the intent is still written, because a rollback is how such a
  flat recovers.

### Restoring text

Used by a rollback intent and by `restore`, for every document of the target,
even one whose text already matches:

1. Replace the committed text with the target text by the bounded character
   diff, as one update with id `restore:<op id>` (a reserved prefix browsers
   cannot use), and set `seed_text` to the target text (`seed_hash` keeps the
   running source). The update bypasses the 256 KiB limit for incoming
   browser updates, as activation merges do.
2. When activating the document or that update would exceed its state,
   structure or receipt limits, reset the document instead: delete its
   updates, checkpoint, receipts and activation rows and seed it again with
   the target text under a new epoch. Connected editors then get `diverged`,
   which offers download and reload. A restore therefore never fails on
   those limits.

The next activation of a different source merges base = the restored text,
ours = live, theirs = the new source, so a later save merges against what the
rollback restored.

`restore` within the running version also recovers a version whose own
activation fails on a history limit: the gate runs in a savepoint, and when
it fails the rollback replaces every document, records the people's text in
the **before** shape, releases any intent and then counts as that version's
activation. When the store has no entry yet, that failure is returned
instead.

### Host operations

All require `X-Flats-Access: private` and `X-Flats-Host-Op: revisions`, which
only core sets (it strips every client spelling of `X-Flats-Host-Op`). Others
get 403. Core calls them in process on the running worker instance, not
through the flat's routes, with a 30-second timeout per call. Bodies are
JSON; a POST body above 8 MiB of text is refused with 413. `sync`, `intent`
and `restore` pass the activation gate first. The transport operations
(`outbox`, `text`, `ack`, `stage`, `op`) do not: they only need the schema,
so the host can always drain and acknowledge the backlog, which frees space,
even while an activation keeps failing.

| Route | Purpose |
|---|---|
| `POST /_docs/host/revisions/sync` | Runs the gate, deletes a remaining intent, closes due groups, deletes unreferenced staged texts older than one hour. Returns `{store, ready, intent}`: `ready` is the number of unacknowledged entries, `intent` the id of the remaining intent or null |
| `GET /_docs/host/revisions/outbox?after=` | Entries with `seq > after`, oldest first, read with one SQL statement per entry and stopping before the page's document lists pass 4 MiB of JSON (always at least one entry): `{store, entries: [{seq, created_at, kind, authors, message, rollback_of?, op?, source, documents}], more}` |
| `GET /_docs/host/revisions/text?hash=` | `{hash, text}` from `texts`; 404 when absent |
| `POST /_docs/host/revisions/ack` | `{store, through}`: deletes entries up to `through` with their `ops` rows, releasing their texts. Idempotent; another `store` is refused with 409 |
| `POST /_docs/host/revisions/stage` | `{text}` (at most 1 MiB UTF-8): stores it with `staged_at`; returns `{hash}` |
| `POST /_docs/host/revisions/intent` | `{id, source, kind, label, message, rollback_of, documents}`: writes `meta.intent` (texts must be staged or present) |
| `POST /_docs/host/revisions/restore` | `{id, label, rollback_of, documents: [{path, hash}], deadline_ms}`: a rollback within the running version; its documents must be exactly the running version's. Returns `{store, seq}` |
| `GET /_docs/host/revisions/op?id=` | `{committed, seq?}`: whether a rollback with this id committed and its entry is still unacknowledged |

Refusals are `{code, message}` with 400 `invalid`, 404 `not_found`, 409
`conflict`, 422 `capacity` or 503 `unavailable`.

* **Size.** No request carries more than one text, so each fits the
  runtime's 10 MiB request limit. One entry lists at most 129 documents, so
  its row stays well below the 16 MiB query result limit, and a page stops
  at 4 MiB, below the 32 MiB response limit. A text response holds one
  document of at most 1 MiB.
* **Deadlines.** Core sends `restore` a `deadline_ms` 12 seconds after
  sending; the app checks it immediately before COMMIT and rolls back with
  503 after it, as live edits do. When the response is lost, core waits for
  the deadline plus a 2-second margin and asks `op?id=` whether it committed.
* **Rollback receipts.** A rollback (from `restore` or a consumed intent)
  writes its `ops` row with its entry, and the row is deleted only when that
  entry is acknowledged, that is, after the host imported it. So for a
  rollback the host has not imported, `committed: false` means it did not
  commit; there is no bounded cache that could forget it.
* **Duration.** Flat activation merges every document in one transaction,
  as the health check already does on its isolated copy within the same
  request deadline. Recording loads only changed documents.

### Initialization and migration

A database without `meta.store` is initialized by the first transaction that
passes the activation gate, before anything else in it:

1. Generate `meta.store`.
2. Seed every document of `content.documents` that has no stored state yet
   (seeding discards nothing).
3. For each `flats_docs_conflicts` row, oldest generation first, record a
   forced `recovered` entry with the message "migrated recovery text" and the
   current shape, with that document overridden by the preserved text (a
   document that is not in the shape is added). Overrides leave `recorded`
   alone. A preserved text above 1 MiB (impossible for rows the app wrote,
   kept as a guard) is skipped with a log line.
4. Record a forced `created` entry of the current shape, with the message
   "revision history started" when documents already had stored state.
5. Delete the `flats_docs_conflicts` rows. The table stays in the schema,
   unused.

The current shape here holds every document's committed text before any
merge of this version. When this version's merge has not run yet, flat
activation then runs it in the same transaction, finds the flat clean and
captures nothing more. A crash repeats all of it.

### History routes

The host serves the document page's history on the flat's own origin, in
front of the worker and only on the live data directory. Every route sends
`Cache-Control: no-store` and `nosniff`.

| Route | Purpose |
|---|---|
| `GET /_docs/api/revisions?before=&limit=` | newest first, 50 by default and at most 100: `{revisions, next, pins, pin_quota, revision_quota}` |
| `GET /_docs/api/revisions/{n}` | `{revision, entry, documents: [{path, title}], files: [{path, hash, size}], previous}`; titles as the app derives them (manifest name for the entry, else the first `#` heading, else the path); `files` are the other files; `previous` is the newest kept revision before `n`, or 0 |
| `GET /_docs/api/revisions/{n}/document?doc=` | `{doc, markdown, hash}` |
| `GET`/`HEAD /_docs/api/revisions/{n}/files/<path>` | a non-document file of that revision, under the inert-asset rules of current assets (fixed MIME types, `nosniff`, the asset or media CSP, attachment for other types) |
| `POST /_docs/api/revisions/{n}/pin` | `{pinned}`; returns `{status, revision, pinned}` |
| `POST /_docs/api/revisions/{n}/rollback` | `{name?}`; returns `{status, revision, rollback_of}`, where `revision` is the new rollback revision |

* **Access.** Every route needs `X-Flats-Access: private`; on a public route
  they all return 404.
* **Write protection.** POSTs also need `Content-Type: application/json` and
  `X-Flats-Docs: 1`, and `Sec-Fetch-Site: same-origin` (or, without it, an
  `Origin` equal to the request's scheme and host); others get 403
  `forbidden`. The body is at most 4 KiB.
* **Labels.** A rollback credits the verified Tailscale name or login, else
  the page's collaborator name (control characters and runs of spaces
  collapsed to one space, at most 80 characters), else "Person".
* Errors are `{code, message}` with 404 `not_found`, 409 `conflict`, 400
  `invalid` or 503 `unavailable`.

### History panel

Private pages only; a public page has no history control and calls none of
the history routes. The panel follows `_web/DESIGN.md`.

* **History button** in the header after the Edit | View group, shown while
  the connection is writable. It opens the history panel: a 320 px
  right-hand column on desktop, a full-screen sheet up to 899 px wide. `Esc`
  closes it.
* **Entries**, newest first, 50 per page with "Load more":
  * who and what: the authors ("Quiet Otter, Lucky Fox"), "Agent",
    "Restored revision 12", "Created", "Recovered text";
  * relative time (absolute in the tooltip) and the documents it changed, by
    title, with "this document" for the open one;
  * a pin toggle (`aria-pressed`). A full pin quota shows its refusal in the
    panel;
  * the panel refreshes every 10 seconds while open and visible, and after
    its own actions.
* **Viewing a revision** replaces the document surface with a read-only
  render of the open document as of that revision, under a banner:
  "Revision 12 · who · 3 hours ago" with the comparison modes, "Restore this
  revision" and "Back to live". The editor stays connected in the
  background.
  * Images and links to the flat's files resolve to the revision's file
    route.
  * **Highlight changes** (the default) marks, in the rendered document, what
    this revision changed since the revision before it that still exists
    (`previous`): changed text tinted and underlined line by line, changed
    tables, lists, code and rules outlined, images whose file changed
    between the two outlined, and an "N lines removed here" note where text
    went. Links that use a changed reference definition count as changed.
    Every mark has a screen-reader label.
  * **Source changes** shows the same comparison as a Markdown line diff, and
    **Compare with live** a line diff against the live text, both with
    `+`/`−` markers.
  * The page diffs on its main thread with a 400 ms and 2,000-edit limit;
    texts that differ more show a note instead of a comparison.
  * A document missing from that revision says so and lists the revision's
    documents.
* **Restore** confirms first ("Restore every document to revision 12? The
  current text stays in the history."), calls rollback, returns to live and
  shows "Restored revision 12". The text arrives through the normal sync,
  or, after a reset, through the page's reload offer.
