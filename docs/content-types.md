# Content types

Contract version: `1`. MCP resource: `flats://docs/content-types/v1`.
Read-only fallback tool: `get_content_types` (no arguments); the document part
is also `guide {"items":["topic.docs"]}`.

A flat's **type** says what an agent authored; the **kind** says how Flats runs
it. Agents upload content of a type; a type adapter in the host turns that
content into something Flats can serve. Versions, drafts, approvals, previews,
data and exposure work the same for every type.

| Type | Agent authors | Host runs it as | Live data |
|---|---|---|---|
| `flat` (default) | a website: static files, or a server module (`kind` `static`/`server`) | static file server or the server-flat runtime | server flats: `env.DB` / `env.FILES` |
| `docs` | Markdown documents (plus images/other assets) | the built-in collaborative editor app on the server-flat runtime (kind `server`) | the live collaborative document state |

Slides and other types are candidates for later adapters; they are not
implemented. Websites are described in `flats://docs/runtime-api/v1`.

The built-in editor uses host helpers (`runtimeGeneration`,
`ws.setSendLimits`, `__flats_docsCodec`) that are not part of runtime API v1
and are not supported for agent-authored server flats.

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
2. `save_document {slug, markdown, title?, path?, message?}` saves a Draft
   holding one Markdown document (`index.md` by default) plus a `flats.json`
   with `type: "docs"` and `name` from `title`. It replaces the complete
   Current Draft, creates a missing flat and never publishes.
3. For several documents or assets, use `save_version` with the complete
   bundle and a `flats.json` of `{"type":"docs"}`.
4. Review with `open_preview` version 0, then `publish` and wait for approval
   (`topic.approvals`).

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
