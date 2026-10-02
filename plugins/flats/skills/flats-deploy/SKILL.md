---
name: flats-deploy
description: Deploy a website or small web app the user built to their own Flats host, then verify it. Use when the user asks to deploy, publish, host, share, preview or roll back a site "on Flats" or "as a flat". Handles static builds and server flats (JavaScript handler with SQLite). Do not use for cloud hosting providers, for running a local dev server, or to change who can see a flat without the user asking.
---

# Deploy a site with Flats

Flats hosts each site as a *flat* on the operator's own machine. Every upload
becomes an immutable **version**; **deploying** a version makes it live after a
health check, and **rollback** redeploys an earlier one. New flats are
**private**: only devices on the operator's Tailscale network can open them.

You can use either the MCP tools (server `flats`, endpoint
`http://127.0.0.1:7878/mcp` on the Flats host, or `https://flats.<tailnet>.ts.net/mcp`)
or the `flats` CLI. Prefer the CLI when the build output is on disk on the
Flats host; use MCP `save_version` with inline files otherwise.

## Workflow

1. **Build** the site so you have an output directory (e.g. `dist/`, `out/`,
   `build/`). Its root must contain `index.html` for a static flat.
2. **Pick a slug**: 3-54 characters, lowercase letters, digits and single
   hyphens, starting with a letter (`my-blog`). It becomes the address.
3. **Save and deploy**:
   - CLI: `flats deploy ./dist --flat my-blog` (records the git commit and
     dirty state automatically). Add `--save-only` to save without deploying.
   - MCP: `save_version` with `files` (`encoding: "utf8"` or `"base64"`) and
     `deploy: true`, or `save_version_from_dir` when you run on the Flats host.
4. **Verify**: open the returned `private_url` (fetch it) and check the page.
   On failure the response lists `problems` with a `fix` for each, or the
   failed health check. The previous live version keeps serving; fix and
   upload again rather than retrying blindly.
5. **Report** the URL, version number and health result to the user.

To let the user review before going live: save only, then `open_preview`
(`flats preview my-blog`) and share the preview URL. Previews close on the
next deploy or after 24 hours without visits.

Rollback: `flats rollback my-blog` (previous version) or `--to N`.

## Manifest (`flats.json`, optional, at the build root)

```json
{ "name": "My blog", "kind": "static", "entry": "index.html", "spa": false,
  "not_found": "404.html", "health": "/", "screenshot": "screenshot.png" }
```

`spa: true` serves the entry for unknown routes (client-side routers).
`screenshot` becomes the thumbnail in the operator's console; include one when
you can capture it. Unknown fields are rejected.

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
secrets as `env.NAME`. Data survives deploys and rollbacks. Bundle your code
into one ES module.

## Visibility and approvals — never decide this for the user

- `private` (default): tailnet only.
- `public-unlisted`: anyone **with the URL** can open it. It is only hidden
  from Portal relay listings — it is NOT private. Always tell the user that.
- `public-listed`: public and listed on Portal relays.

Making a flat public or deleting it needs the operator's approval. The tool
returns `pending_approval` and an `approval_url`: give that link to the user
and stop; do not try to approve it yourself or work around it. Making a flat
less public applies immediately.

## Secrets

You can list secret names (`list_secrets`, `flats secret ls`) but never set or
read values. Ask the user to set them in the Flats console or with
`flats secret set <flat> NAME` on the Flats host, then redeploy.
