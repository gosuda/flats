---
name: flats-deploy
description: Deploy a website or small web app the user built to their own Flats host, then verify it. Use when the user asks to deploy, publish, host, share, preview or roll back a site "on Flats" or "as a flat". Handles static builds and server flats (JavaScript handler with SQLite). Do not use for cloud hosting providers, for running a local dev server, or to change who can see a flat without the user asking.
---

# Deploy a site with Flats

Flats hosts sites on the operator’s own machine. Uploads save immutable Private **Draft revisions**, without a published number. **Publish** freezes a Draft and requests operator approval; only approved successful publication creates v1, v2, … and makes it current. Activation and rollback reuse published numbers and also require approval. Publication, visibility, and connection state are independent.

Use the Flats MCP tools or CLI on the host. Prefer CLI for build output on disk; use MCP `save_draft` with inline files otherwise. Agent access never grants operator authority.

## Runtime API discovery

Before authoring a server app, read MCP resource `flats://docs/runtime-api/v1`
or call the read-only `get_runtime_reference` tool with `{}`. The complete
[runtime API v1 reference](../../../../docs/runtime-api-v1.md) ships in the host;
MCP clients do not need this skill installed. It defines synchronous FILES/DB
methods, arguments, returns, missing keys, text/binary encoding, errors,
limits, persistence, request/response helpers and secrets. FILES is a per-flat
local-disk string store, not S3. Use application base64 text for binary storage.

## Workflow

1. Build the site into an output directory (`dist/`, `out/`, `build/`). Static builds need `index.html` at the root. Pick a slug of 3–54 lowercase letters, digits and single hyphens, starting with a letter.
2. Save Private Draft content: CLI `flats draft my-blog ./dist --expected-revision 0` for a first save; on later saves, read Draft metadata and supply its revision. MCP `save_draft` takes actual `files` and `expected_revision`. A conflict preserves existing content; read and review the latest Draft before retrying. Compatibility `flats deploy ./dist --flat my-blog --save-only` also saves Draft.
3. Review privately with `flats preview my-blog` or MCP `open_preview` targeting Draft/version 0. Draft previews use Local loopback or explicitly permitted Tailscale with existing tailnet ACLs. Public visitors keep seeing the current published version. Read the preview state; do not claim a starting or unavailable URL works.
4. Request publication with `flats publish my-blog --revision N --hash HASH` or MCP `publish`. CLI exit 3 (also JSON mode), or `pending_approval`, means waiting. Give the operator the approval URL and frozen candidate identity. `deploy:true` on a save is also only a publish request. Never approve, retrieve credentials, forge a console session, or work around the boundary.
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
npm packages that need Node, no file system or network. Use `env.DB`
(SQLite: `query`, `exec`), `env.FILES` (`get`, `put`, `delete`, `list`) and
secrets as `env.NAME`. Live DB/FILES data survives redeploys and ordinary code rollbacks.
Previews use isolated copies. Candidate health checks use isolated DB/FILES copies. After approval, starting the live runtime can write live data, even if activation fails; inspect reported data impact and keep startup paths free of destructive mutations. Bundle dependencies for the sandbox; uploaded relative ES module imports
are supported. Server handlers must serve their own UI/assets.

A `.wasm` server is a fresh WASI preview1 command per HTTP request. Read
`{method, url, headers, body}` JSON from stdin and write `{status, headers,
body}` JSON to stdout. Only the flat's secrets are injected as environment
variables; there is no separate variable configuration or inherited host
environment. Clocks and CSPRNG are enabled. WASI has no SQLite
or persistent FILES host ABI, filesystem mounts, outbound network or WebSocket
API. Choose JavaScript when the app needs `env.DB`, `env.FILES`, Web Crypto
or WebSocket callbacks; WASI does not share those JS host objects.

## Visibility and approvals — never decide for the user

- **Private** (default): Local on this device through localhost, or Tailscale for people/devices allowed by the existing tailnet ACL. This does not mean owner-only.
- **Public**: anyone on the internet through explicitly permitted Portal or Tailscale Funnel; Funnel visitors do not need Tailscale. A URL or domain does not define visibility.

Use only `private` / `public`. Both directions require explicit operator approval, as do publish, activation, rollback, data restore and deletion. Give the approval link and poll `get_approval`; never treat Public→Private as immediate. Public requires a published version. Nonlocal providers need a host grant/configuration plus per-flat operator permission; Tailscale connectivity does not grant Funnel. Agents cannot set those permissions. A provider failure never authorizes switching providers.

## Secrets

You can list secret names (`list_secrets`, `flats secret ls`) but never set or
read values. Ask the user to set them in the Flats console or with
`flats secret set <flat> NAME` on the Flats host. Secrets apply when a version
starts: redeploy the live version (`flats deploy --flat <flat> --version <live>`
or MCP `deploy`), then report pending and poll approval, or ask the user to click **Redeploy (apply secrets)** in the
console.
