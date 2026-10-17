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
CSS as strings, plus `mermaid-asset.js`, a module exporting the minified
Mermaid browser bundle as a string plus its build-time gzip representation
encoded as base64. No Node, network, WebAssembly, TextEncoder, or TextDecoder
is needed at deployment time. Third-party license texts cover the 90 packages
actually bundled, as determined by esbuild's input graph (a package whose
license is only a README section, such as fastdom, contributes that section).

`dist/BUILD-INPUTS.sha256` covers sorted `_web/src/**`, package metadata and
the build script and every other dist output (server and licenses). `TestBuildInputs` recomputes it without Node. Changing a
source or unit test requires rebuilding the committed output. `fixtures.mjs`
creates real Yjs bytes for Go integration tests, which re-execute the test
binary as the actual Flats worker and materialize a temporary app directory.

## Implementation

The server keeps one plain Yjs `Y.Text("markdown")` per document in the
flat's SQLite database, whose committed log is authoritative. Revision
capture (`revisions.js`) records whole-flat revisions in an outbox in the
same transaction as each revision boundary; the host imports them, and the
page's History panel (`history.js`) reads them back through host routes.
The routes, schema, collaboration model, merge rules, revision capture,
protocol and limits are specified in
[docs/internal/docs-app.md](../../../docs/internal/docs-app.md); this page
does not repeat them.

The browser uses CodeMirror 6, Markdown language support and y-codemirror.next
with local-origin undo. A custom provider keeps pending ids/bytes through
reconnect, clears them only on ACK, relays awareness, and stops with a local
download on divergent history or fatal rejection. Markdown-it disables raw
HTML and validates links. A ```` ```mermaid ```` fence renders as a placeholder that
Mermaid 11.17.2 (strict security level, light/dark theme from the color scheme)
draws in the browser. Mermaid is fetched only for documents with diagrams,
from `/_docs/assets/mermaid-<sha256[:16]>.js` with immutable caching. The server
negotiates gzip or identity using `Accept-Encoding`, includes `Vary`, and
returns 406 if neither representation is acceptable. The approximately
3.45 MB module compresses to 0.96 MB with gzip. Compression happens at build
time; the worker decodes base64 with the bounded host codec into a binary
response. The build rejects gzip payloads above that codec's 1.5 MiB limit.
The worker dynamically imports `mermaid-asset.js` only when that path is
requested, so other workers never load it. Drawings are cached by theme and
source, concurrent identical requests share rendering work, and stale work
cannot overwrite a newer theme or source. Diagrams near the viewport render
first, and work for removed placeholders is discarded.

Diagram controls provide an expanded view with zoom, pan and fit, source
viewing/copying, and SVG or PNG download. The expanded view supports keyboard use,
Escape to close, and restoring focus to its opener. In Edit mode, Edit source
opens the editable fence and Done returns to the rendered diagram; there is
no duplicate read-only Source toggle. View mode keeps that Source toggle. Loading and syntax errors are distinguished, source is
preserved on failure, and failed loads can be retried. The live-preview editor
also handles diagrams inside quotes and lists. While source is being edited,
an optional preview refreshes after typing pauses. PNG export snapshots the
current appearance onto the document background at up to 2× resolution,
bounded by 8,192 pixels per side and 16 megapixels. PNG uses locally available
fonts; page web fonts are not embedded, so text metrics can differ. Unsupported
external resources, browser canvas restrictions, or oversized diagrams report
an error and leave SVG export available.

Public viewers construct no editor, cannot send presence and receive no
private identities/cursors. On fatal failure the provider stops retries,
retains pending edits, disables editing and offers a Markdown download. Light/dark,
mobile gutters, a single live-preview surface (Markdown renders in place while
editing; its syntax shows only on the cursor's lines), a private Edit/View toggle
(private links open in Edit; public links are View only, with no controls), a
History panel on private links, presence, save state, a random
collaborator name (no join prompt) and a "Powered by Flats" footer at the end
of the rendered document are included. The page shows document titles only,
never file paths. UI decisions are recorded in `_web/DESIGN.md`.

## Required host integration

The host materializes `FS()` next to a generated `content.js`, runs `Entry`
with a positive, monotonically increasing `RuntimeSpec.Generation`, and uses
`Hash()` to decide whether the embedded app changed. The runtime layout,
asset routing, trusted headers, health signal, generation fence and the host
internals outside runtime API v1 that must accompany the app are specified
in [docs/internal/docs-app.md](../../../docs/internal/docs-app.md).

## Operational limits

Pending browser edits survive connection loss, not reload or browser crash;
the page warns before a reload or close loses local text the server does not have, also after sync stopped. Acknowledged edits survive
worker death/restart and compaction; restoring an older database produces
`diverged` for clients that remember the lost history. A document can reach
its structure/history cap before its visible text cap (see the
[application limits](../../../docs/internal/docs-app.md#wire-protocol-json-text-frames-version-1));
a revision rollback resets a document that cannot take its restore update. `get_document` is idempotent/non-destructive but may
seed or merge state and therefore does not advertise read-only MCP
annotations. Workers share SQLite on one host, not a multi-host replication
protocol.

The browser test uses separate Chromium contexts and synthetic Korean
composition events plus actual Korean/emoji insertion. It does not automate
a physical operating-system IME. Other browser engines, peak heap/RSS and production network latency have not been measured.

See [TESTING.md](TESTING.md) for commands, measurements and artifacts.
