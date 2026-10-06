---
name: flats-deploy
description: Deploy a website or small web app the user built to their own Flats host, then verify it. Use when the user asks to deploy, publish, host, share, preview or roll back a site "on Flats" or "as a flat". Handles static builds and server flats (JavaScript handler with SQLite). Do not use for cloud hosting providers, for running a local dev server, or to change who can see a flat without the user asking.
---

# Deploy a site with Flats

Flats hosts sites on the operator’s own machine. Uploads save immutable Private **Draft revisions**, without a published number. **Publish** freezes a Draft and requests operator approval; only approved successful publication creates v1, v2, … and makes it current. Activation and rollback reuse published numbers and also require approval. Publication, visibility, and connection state are independent.

Use the Flats MCP tools or CLI on the host. Prefer CLI for build output on disk; use MCP `save_draft` with inline files otherwise. MCP and CLI requests wait for console approval. Console routes have CSRF protection but no separate authentication; local processes can send those headers, so run only trusted agents on the host. This workflow must leave approval decisions to the user.

## Runtime API discovery

Before authoring a server app, read MCP resource `flats://docs/runtime-api/v1`
or call the read-only `get_runtime_reference` tool with `{}`. The complete
[runtime API v1 reference](../../../../docs/runtime-api-v1.md) ships in the host;
MCP clients do not need this skill installed. It defines synchronous FILES/DB
methods, arguments, returns, missing keys, text/binary encoding, errors,
limits, persistence, request/response helpers, app environment variables and secrets. FILES is a per-flat
local-disk string store, not S3. Use application base64 text for binary storage.

## Markdown documents

Read `flats://docs/content-types/v1` or call `get_content_types` first. For an
existing docs flat, call `get_document {slug, doc?}` before preparing an edit;
it returns live Markdown including people's changes, or Current Draft content
when no docs version runs. Call `save_document {slug, markdown, title?, path?,
message?}` to create or replace the complete Draft (default `index.md`). This
never publishes; use `save_version` for multiple documents and assets. Review
with `open_preview` version 0, then request `publish` with the returned revision
and hash, give the operator the approval URL, and wait for approval using the
normal workflow below. Never approve the publication yourself. Code rollback
merges the target version's Markdown into live content, keeping independent
human edits; conflicting lines prefer the target. Runtime generation and
send-budget helpers are built-in host internals outside runtime API v1.

## Workflow

1. Build the site into an output directory (`dist/`, `out/`, `build/`). Static builds need `index.html` at the root. Pick a slug of 3–54 lowercase letters, digits and single hyphens, starting with a letter.
2. Save Private Draft content: CLI `flats draft my-blog ./dist --expected-revision 0` for a first save; on later saves, read Draft metadata and supply its revision. MCP `save_draft` takes actual `files` and `expected_revision`. A conflict preserves existing content; read and review the latest Draft before retrying. Compatibility `flats deploy ./dist --flat my-blog --save-only` also saves Draft.
3. Review privately with `flats preview my-blog` or MCP `open_preview` targeting Draft/version 0. Draft previews use Local loopback or explicitly permitted Tailscale with existing tailnet ACLs. Public visitors keep seeing the current published version. Read the preview state; do not claim a starting or unavailable URL works.
4. Request publication with `flats publish my-blog --revision N --hash HASH` or MCP `publish`. CLI exit 3 (also JSON mode), or `pending_approval`, means waiting. Give the operator the approval URL and frozen candidate identity. `deploy:true` on a save is also only a publish request. Never approve your own requests or send console decision headers to bypass the user’s approval.
5. Poll read-only MCP `get_approval` or `flats approvals --json` (match the returned approval ID). While pending, report “waiting for operator approval”; on rejected or failed, report that outcome and its typed cause. Do not report a version as live from a save result or pending request.
6. After `approved`, read `get_flat` / `flats info`, confirm the actual current version and a ready endpoint, and fetch the page. Report the verified version, address and observed health. If Public, repeat its internet-access notice. Provider connection alone never proves approval or publication. Recheck externally shared links over time.

Rollback (`flats rollback my-blog`, optionally `--to N`, or MCP `rollback`) requests approval to serve an earlier published version. Ordinary rollback preserves live data. Use `--restore-data` / `restore_data:true` only when the user explicitly wants replacement: the request freezes a snapshot and hash, backs up current live data, then replaces DB and captured FILES. Historical DB-only snapshots preserve FILES. Writes since the snapshot stop being live. Report pending and poll the approval before claiming restoration.

## Manifest (`flats.json`, optional, at the build root)

```json
{ "name": "My blog", "kind": "static", "entry": "index.html", "spa": false,
  "not_found": "404.html", "health": "/", "screenshot": "screenshot.png" }
```

`spa: true` serves the entry for unknown routes (client-side routers).
`screenshot` becomes the thumbnail in the operator's console; include one when
you can capture it. `health` must be a URL path such as `/healthz`. Unknown
fields are rejected; every problem comes back at once with a fix.

## Server flats

Set `"kind": "server"` and ship `server.js`:

```js
export default {
  async fetch(request, env) {
    // request: { method, url, headers, body }
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (at TEXT)");
    env.DB.exec("INSERT INTO hits VALUES (datetime('now'))");
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    return Response.json({ hits: n });
  }
}
```

The handler runs in a sandbox (QuickJS on WebAssembly): no Node.js APIs, no
npm packages that need Node, no general filesystem or socket API. Bounded
global `fetch` supports HTTP(S) APIs only after an operator grants exact origins. Use `env.DB`
(SQLite: `query`, `exec`), `env.FILES` (`get`, `put`, `delete`, `list`) and
ordinary environment variables and secrets as `env.NAME`. Live DB/FILES data survives redeploys and ordinary code rollbacks.
Previews use isolated copies. Candidate health checks use isolated DB/FILES copies. After approval, starting the live runtime can write live data, even if activation fails; inspect reported data impact and keep startup paths free of destructive mutations. Bundle dependencies for the sandbox; uploaded relative ES module imports
are supported. Server handlers must serve their own UI/assets.

A `.wasm` server is a fresh WASI preview1 command per HTTP request. Read
`{method, url, headers, body}` JSON from stdin and write `{status, headers,
body}` JSON to stdout. Only the flat's configured environment variables and secrets are injected as environment variables; there is no inherited host environment. Clocks and CSPRNG are enabled. WASI has no SQLite
or persistent FILES host ABI, filesystem mounts, outbound network or WebSocket
API. Choose JavaScript when the app needs `env.DB`, `env.FILES`, Web Crypto
or WebSocket callbacks; WASI does not share those JS host objects.

## Visibility and approvals — never decide for the user

- **Private** (default): Local on this device through localhost, or Tailscale for people/devices allowed by the existing tailnet ACL. This does not mean owner-only.
- **Public**: anyone on the internet through explicitly permitted Portal or Tailscale Funnel; Funnel visitors do not need Tailscale. A URL or domain does not define visibility.

Use only `private` / `public`. Both directions require explicit operator approval, as do publish, activation, rollback, data restore and deletion. Give the approval link and poll `get_approval`; never treat Public→Private as immediate. Public requires a published version. Nonlocal providers need a host grant/configuration plus per-flat operator permission; Tailscale connectivity does not grant Funnel. Agents cannot set those permissions. A provider failure never authorizes switching providers.

## App environment variables

Use MCP `list_env {slug}`, `set_env {slug, name, value}` and
`delete_env {slug, name}`, or `flats env set <flat> NAME VALUE`,
`flats env ls <flat>` and `flats env rm <flat> NAME`. Empty strings are allowed.
Ordinary values are readable by management clients and stored without secret
encryption. Use the secret workflow below for credentials.

Names match `[A-Z_][A-Z0-9_]*`, at most 64 characters; `DB`, `FILES`,
`__PROTO__`, `PROTOTYPE` and `CONSTRUCTOR` are reserved. Values are at most 64 KiB of valid UTF-8 without NUL. An ordinary variable cannot
share a secret name: remove the old kind before changing kinds. Server JavaScript
reads `env.NAME`; WASI gets environment variables. They do not enter frontend
bundles, static files or builds.

Saving does not update running handlers or previews. Approved deployment, redeployment, rollback or standalone data snapshot
restoration captures current variables and secrets when activation begins; its
health check and live worker share that snapshot. Settings are not pinned to the
approval request or code version. Writes after capture apply at the next
activation. New previews and a Flats host restart load current settings; automatic
worker restarts reuse the captured snapshot. Redeploy with the existing approval
flow to apply changes deliberately.

## Secrets

You can list secret names (`list_secrets`, `flats secret ls`) but never set or
read values. Ask the user to set them in the Flats console or with
`flats secret set <flat> NAME` on the Flats host. Secret changes apply on approved
deployment, redeployment, rollback or standalone data snapshot restoration, or Flats host restart. Automatic worker
restarts reuse their captured settings. To apply changes deliberately: redeploy the live version (`flats deploy --flat <flat> --version <live>`
or MCP `deploy`), then report pending and poll approval, or ask the user to click **Redeploy (apply environment)** in the
console.

## External APIs

Read `get_network` / `flats network ls <flat>` before relying on server fetch.
Agents cannot grant origins; ask the operator to use console settings or
`flats network set <flat> https://api.example.com` on the host. Grants default
to deny and follow the captured activation settings, including worker restart
semantics described above. Revoking grants requires redeploy to revoke running
access. Do not request raw sockets, private/metadata endpoints, arbitrary ports
or automatic redirect following. Use server-only env/secrets for tokens and a
narrow same-origin handler for browser clients. Keep health checks local: checks
and previews can make real external calls when granted. Browser fetch needs
real CORS/CSP and HTTPS compatibility; never embed server credentials or disable
browser protections. Read the runtime reference and docs/external-api.md for
request/response size, deadline, concurrency and rate limits before generating
an integration.
