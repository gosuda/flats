# Flats Docs app

This directory owns the embedded app for `type: "docs"` flats. `FS()`,
`Entry`, `ContentModule`, and `Hash()` retain their existing Go interface.
Only `dist/` is embedded. The host supplies `content.js`; it is deliberately
absent from the build output.

## Build and development

```sh
npm ci --prefix internal/apps/docs/_web
npm test --prefix internal/apps/docs/_web
npm run fixtures --prefix internal/apps/docs/_web
npm run build --prefix internal/apps/docs/_web
go test ./internal/apps/docs/... ./internal/runtime/...
```

Dependencies are pinned in `_web/package.json` and its lockfile. The build
produces one minified server ESM bundle, embedding the client JavaScript and
CSS as strings. No Node, network, WebAssembly, TextEncoder, or TextDecoder
is needed at deployment time. Third-party license texts cover the 32 packages
actually bundled, as determined by esbuild's input graph.

`dist/BUILD-INPUTS.sha256` covers sorted `_web/src/**`, package metadata and
the build script and every other dist output (server and licenses). `TestBuildInputs` recomputes it without Node. Changing a
source or unit test requires rebuilding the committed output. `fixtures.mjs`
creates real Yjs bytes for Go integration tests, which re-execute the test
binary as the actual Flats worker and materialize a temporary app directory.

## Implementation

The server uses Yjs 13.6.33 with a single plain `Y.Text("markdown")` per
document. A serial WebSocket VM owns connected rooms; HTTP VMs load committed
state. SQLite transactions catch up before append, deduplicate exact update
ids, commit before ACK/broadcast, and compact at an explicit watermark.
Persistent receipts preserve exact dedupe and chain history after compaction.
Activation records and trusted host generations prevent old overlapping
workers from reversing a source merge. Three-way line merge preserves
independent edits; overlapping source edits prefer the new version. A bounded
character diff preserves unaffected CRDT positions. Neither diff uses a clock:
line diff has a 128-edit / 16,000-line bound without a character cap; character diff a 64-edit /
65,536-code-unit bound. Line overflow or a combined result above 1 MiB applies the version and preserves exact
live Markdown in `flats_docs_conflicts`. Discarding overlapping human text absent from the new version
also saves the complete pre-activation live text through the same recovery path.
Newest whole records are retained
within 8 records and 2 MiB UTF-8 per document, including repeat-hash rollbacks;
older records are evicted atomically. Only private API and internal host/MCP
reads list metadata (`generation`, `bytes`); public reads omit conflicts.
The private `/_docs/api/conflict?doc=...&generation=...` returns full text;
`view=1` serves text/plain UTF-8 with nosniff, restrictive CSP and no-store.
Agents can use privileged `get_document {slug, doc?, conflict: generation}`
(or host API `?conflict=<generation>`) to retrieve that preserved Markdown. Private editors see a non-blocking notice. The publish health check activates every document on
an isolated data copy; failures block publish without touching live data.

The browser uses CodeMirror 6, Markdown language support and y-codemirror.next
with local-origin undo. A custom provider keeps pending ids/bytes through
reconnect, clears them only on ACK, relays awareness, and stops with a local
download on divergent history or fatal rejection. Markdown-it disables raw
HTML and validates links. Public viewers construct no editor, cannot send presence and receive no
private identities/cursors. Public connections/rooms have separate smaller
budgets. Already activated API reads and heartbeats use deferred read snapshots.
Rooms catch up from their cached Y.Doc, validating once after replay. Unchanged
pings do no document rebuilding/validation/encoding. UTF-8 size uses an O(1)
upper bound, then an exact count near the limit without percent allocations.
A rejected mutation invalidates the cache for reload while keeping peers connected.
Permanent rejections send stable error codes and close only the sender with
1008; transient failures drop the room with 1012. The provider stops retries,
retains pending edits, disables editing and offers a Markdown download. Light/dark,
mobile gutters, Split/Edit/Preview, presence, save state, a random
collaborator name (no join prompt) and a "Powered by Flats" footer at the end
of the rendered document are included. The page shows document titles only,
never file paths. UI decisions are recorded in `_web/DESIGN.md`.

The precise routes, schema, protocol and limits are specified in
[docs/internal/docs-app.md](../../../docs/internal/docs-app.md).

## Required host integration

Materialize `FS()` next to a generated `content.js`, run `Entry` with a
positive, monotonically increasing `RuntimeSpec.Generation`, and use `Hash()`
when deciding whether the embedded app changed. Supply the content module
shape already specified in the contract. No changes to that shape are needed.

Forward HTTP/WebSocket routes and serve `content.assets` from the bundle.
Overwrite `X-Flats-Access` on every request, including upgrade requests:
only the exact value `private` enables writes. Strip all client spellings of
`X-Flats-Health` as well; set `X-Flats-Health: 1` only on the host's isolated
health trial. Every visitor health request, including private ones, returns
cheap `ok` with no DB access. Reads/joins/commits may activate live state. Supply trusted Tailscale name
or login headers when available. Validate the served Host at ingress as Local
and Tailscale providers do; the WebSocket Origin must match that host. Close sockets admitted through public handlers when public exposure
is withdrawn, and preview sockets when the preview closes; a socket retains its admission decision.

Host internals outside runtime API v1 must accompany the app:

* Non-enumerable `globalThis.__flats_docsCodec` for bounded base64, UTF-8
  and SHA-256. The server build selects this for lib0 serialization without
  introducing public TextEncoder/TextDecoder globals. This avoids interpreted
  per-byte loops at maximum document size; it has no I/O capabilities.
* Read-only `request.runtimeGeneration` and `ws.runtimeGeneration`, populated from
  `RuntimeSpec.Generation`. The persisted host counter survives restarts and clock changes.
  Content hashes alone cannot order an idle old worker
  against a newer activation.
* Optional `ws.setSendLimits(connectionBytes, flatBytes)`, bounding queued
  and in-flight Go payload bytes. App delivery receipts can be forged, so
  they cannot independently bound the host's outgoing queues. Budgets only
  decrease and existing apps keep their default behavior.

## Operational limits

Pending browser edits survive connection loss, not reload or browser crash.
The UI warns before closing with pending edits and offers local Markdown
download on fatal failure. Acknowledged edits survive worker death/restart
and compaction; restoring an older database produces `diverged` for clients
that remember the lost history.

Uploads validate the 128-document limit with a fix hint.
Internal VM failures trigger replacement and retryable closure; reconnect resyncs
committed state. Rejection preserves the room position across cache invalidation
and other-worker compaction.

Persistent receipts/activations have explicit admission caps and are never
silently evicted. Conflict recovery is a separate, evictable bounded window. A document can reach its structure/history cap before its
visible text cap; operators must recover/export rather than erase receipts
to keep accepting edits. Code rollback merges the target text into live data using the current
seed as base, preserving independent human edits even for an observed hash. Presence expiry is opportunistic; there are no server timers.
Stopping a candidate restores the preceding runtime's crash-recovery claim.
`get_document` is idempotent/non-destructive but may seed or merge state and
therefore does not advertise read-only MCP annotations.
Workers share SQLite on one host, not a multi-host replication protocol.

The browser test uses separate Chromium contexts and synthetic Korean
composition events plus actual Korean/emoji insertion. It does not automate
a physical operating-system IME. Other browser engines, peak heap/RSS and production network latency have not been measured.

See [TESTING.md](TESTING.md) for commands, measurements and artifacts.
