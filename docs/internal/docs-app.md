# Docs app: host integration and internals

This is maintainer documentation for the built-in collaborative Markdown app
(`internal/apps/docs`) and its host integration. It is not agent-facing: the
agent contract for `type: "docs"` is the generated
[content types reference](../content-types.md) (guide `topic.docs`), and none
of the helpers below are part of runtime API v1.

Stored versions keep exactly the uploaded files. `kind` of a docs version is
recorded as `server` (it runs in a worker and owns live data, so snapshots,
data-isolated previews and health copies behave as for server flats). Views
report `type` next to `kind`.

## Docs runtime layout

On activation or preview, core materializes a read-only runtime directory under
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

### Trusted access header

Core sets `X-Flats-Access` on every request it hands to a flat handler (all
types) and deletes any client-sent value first:

* `private` — a private route (Local, Tailscale) or a Draft preview.
* `public` — a public route (Portal, Tailscale Funnel).

Apps must treat a missing or unknown value as `public`. Identity headers
(`Tailscale-User-Login`, `Tailscale-User-Name`) keep their existing meaning:
set from WhoIs on Tailscale, stripped on public routes.

Core also strips every client spelling of `X-Flats-Health` and
`X-Flats-Host-Op` on private, public, and preview routes. Only its own health
trial sets `X-Flats-Health: 1`, and only `Service.UpdateDocument` sets
`X-Flats-Host-Op: edit`, after stripping client headers. Private access by
itself authorizes neither health activation nor a host edit.

## Docs app contract (internal/apps/docs)

The app owns the flat's whole origin. Reserved paths start with `/_docs/`.
`FS()`, `Entry = "server.js"`, `ContentModule = "content.js"` and `Hash()`
remain the host interface. The committed `dist/server.js` is one ESM bundle,
including the client JavaScript and CSS as strings; its only external import
is the host-generated `./content.js`. No Node, CDN or outbound network is used
at runtime. `dist/THIRD_PARTY_LICENSES.txt` contains bundled licenses and
`dist/BUILD-INPUTS.sha256` binds the source inputs and every other dist file; a Go test verifies both.

| Route | Purpose |
|---|---|
| `GET /` | entry document workspace for `private`, live rendered view otherwise |
| `GET /<path>` | exact Markdown path, or `<path>.md` / `<path>.markdown` |
| `GET /_docs/healthz` | Cheap `200 ok` without DB access for visitors; only trusted host health trials activate all documents atomically |
| `GET /_docs/api/documents` | `{format:1, entry, title, documents: [{path, title}]}` |
| `GET /_docs/api/document?doc=<path>` | `{format:1, doc, markdown, hash, epoch, seq, chain, source:"live"}`; private/host reads additionally include `conflicts:[{generation,bytes}]`, and with `blocks=1` one page (1,000 blocks from `block_offset`) of the block outline `blocks:[{hash,kind,level?,line,preview}]` plus `blocks_total`; default doc is entry |
| `POST /_docs/api/edit?doc=<path>` | Host management only (`private` plus `X-Flats-Host-Op: edit`, see below): guarded live edit; others receive 403 |
| `GET /_docs/api/conflict?doc=<path>&generation=<id>` | Private only: `{generation,markdown}`; `view=1` returns `text/plain; charset=utf-8` with nosniff, restrictive CSP and no-store for viewing/copying; public receives 403, evicted/missing id 404 |
| `GET /_docs/assets/client.js` | self-contained browser ESM bundle |
| `GET /_docs/assets/client.css` | workspace CSS |
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
  * `conflicts(doc, generation, markdown, PRIMARY KEY(doc,generation))`
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
  whenever overlap discards human text absent from the version, or a fallback
  replaces live text, the exact
  pre-activation live Markdown is retained in `flats_docs_conflicts`
  for private recovery. Routine private API and host/MCP responses list only
  `conflicts: [{generation, bytes}]` (newest first); public responses omit it.
  Full text is requested by generation through the private conflict API or
  privileged MCP `get_document {slug, doc?, conflict: generation}`.
  Each document retains the newest whole records that fit both 8 records and
  2 MiB UTF-8. Older records are deleted in the same activation transaction,
  including when rolling back to previously seen hashes. A new generation
  also trims older unbounded records from prior app versions. This recovery
  window is finite. Private editors receive a small notice and a view/copy link
  on welcome or activation catch-up; it does not block editing.
  A deterministic character diff is bounded to 64 edits and 65,536 combined
  UTF-16 code units; overflow replaces only the differing prefix/suffix span.
  Only the trusted host health signal activates every source document
  atomically on the isolated health copy, so failures block publication.
  Normal reads, joins and commits can still seed/activate the live database.
  Visitor health requests never read the DB or run activation.
* The host allocates a strictly increasing activation generation for each
  runtime start, including live/health/restore trials, Draft previews, host
  restart restore and worker crash recovery. The `runtime_generation` counter in
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
  Restoring a flat's data does not restore the host counter. `Version` remains
  the published number used in logs, including 0 for Drafts.
  The app records the largest applied generation in the legacy `activations.version`
  column. A newer generation advances the fence even when the source is unchanged.
  When its source hash differs from `seed_hash`, it merges base = current
  `seed_text`, ours = live text, theirs = target text. Revisited hashes are
  allowed: code rollback reapplies the target's agent text and retains independent
  human edits. Activation update ids include the generation, so overlapping
  workers cannot apply the same merge twice. Older workers catch up and may
  append people's updates, but never merge obsolete source or seed a missing
  document after a newer generation was observed anywhere in that database.
  Removed documents retain stored state but the current app never lists or
  opens them. Data-isolated previews inherit copied history and use a fresh
  generation; their merges do not affect live data.
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
  the call refuses with `not_deployed` and a hint to use `save_document` and
  `publish`. Agent-authored server flats never see the header, and a
  browser on a private route cannot add it (core strips it), so the endpoint
  is not a cross-site write path.
* **Same authority as people's edits.** One `BEGIN IMMEDIATE` transaction runs
  `activate` (catch-up and any pending activation merge), resolves the
  operations against the committed text, applies them as minimal `Y.Text`
  deletes/inserts in one Yjs transaction, checks `validateUpdate`, the
  256 KiB decoded update limit and `validateIncremental` (1 MiB Markdown,
  state and structure limits), `append`s one update with a unique
  `agent:<uuid>` receipt id, and `compact`s. Nothing is reported before
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
* **Versions.** Live edits are not part of a published version or the Draft.
  The next activation merges `seed_text` → live → version exactly as for
  human edits: disjoint agent edits survive, overlapping ones prefer the
  version and the discarded live text goes to `flats_docs_conflicts`.
* **Audit and recovery.** Core records each applied edit in the flat event
  log (kind `document`): document, operation, line and removed/inserted
  counts, seq before/after, receipt id and resulting hash, never the text.
  The receipt id prefix `agent:` identifies agent updates in the update log.
  Pre-edit text is not retained separately: conflict records are keyed by
  activation generation and bounded to 8 per document, so sharing them would
  let routine agent edits evict human recovery text. Edits are small and
  guarded, the agent read the text it changed, and the result reports what
  changed, so the reverse edit restores it.
* **Public flats.** Live text is public at once. The tool description,
  MCP instructions, `topic.docs` and every result on a Public flat
  (`public_notice`) say so; there is deliberately no approval step, matching
  private human editing.
* **Not done.** Idempotency keys (a retry after a lost response may apply an
  insert twice; guards make repeated replacements fail instead), and a
  console/HTTP management API for the same operation.

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
only lowers budgets, and accepts connection budgets of 1 KiB–32 MiB and flat
budgets between that connection budget and 256 MiB. App delivery receipts also
bound unprocessed output; native budgets cannot be bypassed by forged receipts.
Adopting this embedded app requires the matching host implementation.

### Wire protocol (JSON text frames, version 1)

Binary fields are canonical RFC 4648 base64. Client → server:

| `t` | Fields | Meaning |
|---|---|---|
| `hello` | `v:1, doc, sv, known?:{epoch,seq,chain}, name?` | join; client state vector and last observed committed history |
| `update` | `id, u` | local update, resent unchanged until ACK |
| `aw` | `u` | one awareness client id, owned by the socket |
| `ping` | — | heartbeat/catch-up, about every 15 seconds |
| `received` | `n` | cumulative server-envelope delivery receipt; releases app output credit, never acknowledges a document edit |

Every server envelope has a monotonically increasing per-socket `n`:

| `t` | Fields (besides `n`) | Meaning |
|---|---|---|
| `welcome` | `v:1, doc, epoch, seq, chain, readonly, u, sv, you:{name,verified}, conflicts?` | server diff missing from client sv and server vector |
| `update` | `u, seq, chain` | committed peer update or catch-up/full checkpoint |
| `ack` | `id, seq, chain` | exact id durably committed; a retry can ACK an older seq |
| `aw` | `u` | relayed/rewritten awareness state or removal |
| `conflicts` | `conflicts:[{generation,bytes}]` | private activation catch-up notice metadata; no full text |
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
| Conflict recovery per document | Newest 8 records within 2 MiB UTF-8; full text only on private request |
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
