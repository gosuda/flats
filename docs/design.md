# Flats design

Flats is one CGO-free Go binary (`flats`) that hosts agent-built websites
("flats") on the operator's own machine. This document fixes the decisions the
PRD left to design: the manifest format, the server-flat handler ABI, node key
expiry, secret storage and the process layout. It also records the HTTP API
that every surface shares.

## Process layout

```
flats serve                     (one long-running process, launchd-managed on macOS)
├── core.Service                flats, versions, deploys, previews, approvals (internal/core)
├── store                       SQLite metadata  <data>/flats.db (modernc.org/sqlite, WAL)
├── private network             one tsnet node per flat, per preview, plus "flats" for the console
│                               (internal/expose/tsnet; internal/expose/local for dev/tests)
├── public network              one Portal exposure per public flat (internal/expose/portal)
├── management server           HTTP API + MCP (/mcp) + web console, on the "flats" tsnet node
│                               and on 127.0.0.1:7878 for local agents and the CLI
└── server-flat workers         `flats worker` child processes, one per running server flat
                                version (re-exec of the same binary; internal/runtime)
```

Data directory (default `~/Library/Application Support/Flats`, override with
`--data` or `FLATS_DATA`; a relative path is made absolute at startup, since
workers run with `/` as their working directory):

```
flats.db                        metadata
secret.key                      32-byte AES-256-GCM key for flat secrets (0600)
flats/<slug>/versions/<n>/      immutable version files (read-only, 0444)
flats/<slug>/data/              server-flat data: db.sqlite and files/
flats/<slug>/previews/<host>/   preview copy of data/ (removed with the preview)
tsnet/<host>/                   tsnet node state (one directory per node)
portal/<slug>.json              Portal identity (keeps the public hostname stable)
```

## Model and flows

* **Version** = the validated upload written to `versions/<n>` plus a row with
  number, content hash (SHA-256 over sorted `path NUL sha256` lines), size, file
  count, kind, manifest, optional git SHA and dirty flag. Versions never change.
* **Save vs deploy.** Saving never touches live. `deploy(n)` builds the
  handler for version n, runs the health check (GET manifest `health`, default
  `/`, 15 s timeout, 2xx/3xx passes), then moves the live pointer in one SQLite
  transaction (compare-and-swap on the previous live version) and swaps an
  in-memory atomic pointer. A failed check stops the new handler and keeps the
  old one live; the error carries the status and the first 300 bytes of body.
* **Rollback** = deploy of an earlier version (default: the version that was
  live before the current one), recorded with kind `rollback`.
* **Data belongs to the flat** (`data/`), not to versions; rollback restores
  code only.
* **Retention.** After every save and deploy, files of versions beyond the
  newest `keep_versions` (default 10) are deleted unless the version is live or
  previewed; the row stays with `pruned=1`.

## Manifest (`flats.json`, optional)

```json
{
  "name": "My blog",
  "kind": "static",
  "entry": "index.html",
  "spa": false,
  "not_found": "404.html",
  "health": "/",
  "screenshot": "screenshot.png"
}
```

* `kind`: `static` (default) or `server`.
* static `entry` defaults to `index.html` and must exist at the bundle root.
  `spa: true` serves the entry for unknown extension-less paths. `/about`
  falls back to `about.html`.
* server `entry` defaults to the first of `server.js`, `index.js`,
  `main.wasm`, `server.wasm`; `.js`/`.mjs` run in QuickJS, `.wasm` runs as a
  WASI module.
* Unknown fields are rejected. Every validation error carries a `fix` hint.
* Uploads: tar, tar.gz or zip (or an in-memory file list from MCP). Limits:
  total uncompressed size `upload_max_bytes` (20 MB), 20 000 files, no links,
  no special files, no paths outside the root; a single wrapping directory
  (e.g. `dist/`) is stripped. `.git`, `node_modules`, `.DS_Store`, `__MACOSX`
  are skipped.

## Exposure

* **Private.** Each deployed flat gets its own operator-owned (untagged) tsnet
  node with hostname `<slug>`; the URL is `https://<slug>.<tailnet>.ts.net`
  (ListenTLS; port 80 redirects to HTTPS). The node is created at first deploy
  and logged out (removed) when the flat is deleted. The middleware deletes any
  client-sent `Tailscale-User-*` header and sets `Tailscale-User-Login` and
  `Tailscale-User-Name` from `WhoIs` (left empty for tagged peers).
* **Origins.** Each flat and each preview is its own host, so cookies and
  storage never mix (`<slug>` vs `<slug>-<8 random>`).
* **Public.** One Portal exposure per public flat, using a persisted identity
  named `<slug>` (the relay rejects a name owned by another key: the console
  shows the conflict). `public-unlisted` sets `LeaseMetadata.Hide`; the change
  reaches relay listings at the next lease renewal (up to ~90 s). Relays: the
  Portal CLI default (discovery, up to 3 active relays) unless
  `portal_relays` (or the `--relays` flag, which wins) lists explicit relays.
  `portal_discovery=false` uses only the explicit relays (ignored when there
  are none) and `portal_max_relays` caps the relays discovery adds (0 = the
  Portal default, 3). These three settings are read when `flats serve`
  starts; the settings API reports them in `apply_on_restart` and returns
  `restart_required` when a change needs a restart. A stored value that is
  invalid is logged and ignored at startup, so it cannot stop the server.
  Discovery-only startup can take about
  a minute; the API returns the public URL once a relay is ready. Unlisted is
  never described as access control; every API response that returns a public
  URL carries the notice. Identity headers are stripped on this path.
* **Previews.** `open_preview(n)` creates an ephemeral tsnet node
  `<slug>-<8 random>` serving version n; server flats get a copy of the live
  data directory. Previews close on deploy of that flat or 24 h after the last
  visit.
* **Certificates and readiness.** When the tailnet issues certificates, a
  node serving HTTPS fetches its certificate as soon as it listens, retrying
  with backoff (30 s doubling to 10 min, 5 min per attempt) until it has one.
  Tailscale gets it from Let's Encrypt with a DNS-01 challenge: on a real
  tailnet that took about 78 s per host and sometimes more than 2 min. Until
  then the host is `starting` with detail "waiting for its HTTPS
  certificate"; it becomes `ready` only once a certificate was obtained (by
  the fetch or by a visitor's handshake). Flat views carry this as
  `private_state`/`private_detail`, previews as `state`/`detail`, so agents
  wait instead of treating a TLS error as a failed deploy. Each host's name
  appears in Certificate Transparency logs, and each new host uses one
  certificate of the Let's Encrypt per-domain weekly limit.
* **Stopping.** `Stop` (delete, preview close) logs the node out, closes it
  and removes its state. It waits at most 30 s: a node whose Tailscale backend
  hangs (seen once on a real tailnet, where a deploy then waited 10 minutes
  for a preview to close) is removed from the served set and finishes in the
  background, so deploys and deletes never block on it. `Close` at shutdown
  is bounded the same way.
* **Measured cost** (spike, testcontrol, darwin/arm64): first node ≈14 MB RSS,
  each further node ≈4.2 MB RSS, ≈1 MB live heap and ≈94 goroutines; `Up`
  ≈0.33 s.

## Node key expiry

Operator-owned nodes inherit the tailnet key expiry (180 days by default).
Flats polls each node's `Self.KeyExpiry` and `BackendState` every 5 minutes.

* Within 14 days of expiry, the console shows the node as `key-expiring`.
* On `NeedsLogin` the node is `needs-login`; the console shows the login URL
  (from the IPN bus) so the operator can re-authenticate, or the operator
  disables key expiry for the node in the Tailscale admin console.
* `flats serve --authkey-file` supplies a reusable untagged auth key for new
  nodes; it is never logged and only read at startup.

## Secrets

Values are sealed with AES-256-GCM (random nonce, AAD = secret name) under
`<data>/secret.key`. Only the console and the local CLI (`flats secret set`,
loopback only) can set or delete values; APIs and MCP return names and update
times only. The CLI is recognized by its `X-Flats-Client: cli` header on the
loopback listener, so this stops MCP and remote API clients, not a process
with a shell on the Flats host (see Approvals). Values reach a server flat only as environment variables of its
worker at start, so a change applies on the next deploy.

## Approvals

Agents (MCP, CLI, HTTP API) cannot widen exposure (private → unlisted →
listed) or delete a flat. Those requests create a pending approval and return
`approval_url` = `<console>/approvals/<id>`. Narrowing exposure applies
immediately. The operator decides in the console; console actions apply
directly after a confirm dialog.

There are no accounts, so operator authority rests on where a request comes
from and what it carries. Enforced:

* MCP, the CLI and `/api` have no approve, reject or settings operation, and
  their visibility and delete calls only create approvals.
* Every request to the management server (loopback listener and console
  node) must name the server in `Host`: `127.0.0.1`, `localhost` or `[::1]`
  with the listen port, or the console node's tailnet names (configured host,
  MagicDNS name and its first label, ports 80/443). Any `Origin` must be one
  of those too. This defeats DNS rebinding and cross-site requests.
* `/console/api` requires `X-Flats-Console: 1` and refuses requests carrying
  `X-Flats-Client` (the CLI and other clients identify themselves with it).
  Mutations also need `Sec-Fetch-Site: same-origin` and an `Origin` equal to
  the requested host, which the console page sends and cross-origin pages
  cannot.
* On the console node, the approver's tailnet login (from `WhoIs`, which the
  node sets after dropping client-sent `Tailscale-User-*` headers) is
  returned as `decided_by` and recorded in the flat's approval event (an
  approved delete removes the flat with its events, so only the response
  carries it). Decisions on the loopback listener are recorded without an
  identity.

Not enforced: all of the console checks are request headers. A process that
can send arbitrary HTTP from one of the operator's devices (for example an
agent with a shell on the Flats host, or on a device the tailnet ACL lets
reach the console node) can imitate the console page and approve or act
directly, and a process that can read the Flats data directory can read the
database and secret key. Approvals therefore hold against agents that only
use MCP, the CLI or the API, and against web pages, not against arbitrary
code running as the operator. Mitigations: write Tailscale ACLs so that only
the operator's browser devices can reach the console node (tag or name it in
the ACL), and run untrusted agents under another OS user or on another
machine, so they reach Flats only through MCP or the API.

## Server flats (handler ABI)

A server flat runs in a `flats worker` child process (one per running
version, previews included). The parent proxies HTTP to the worker over a
Unix socket; the worker gets the version directory, its data directory and
its environment, and nothing else from the parent.

JavaScript (QuickJS via qjs on wazero), modelled on `wasi:http`/Workers:

```js
export default {
  async fetch(request, env) {
    // request: { method, url, headers: {name: value}, body: string|null }
    const rows = env.DB.query("SELECT count(*) AS n FROM visits");
    return new Response(JSON.stringify(rows), { status: 200, headers: { "content-type": "application/json" } });
  }
}
```

* `env` holds secrets/env vars plus `env.DB` (`query(sql, ...params)` →
  rows, `exec(sql, ...params)` → `{changes, last_insert_id}`) backed by
  `data/db.sqlite`, and `env.FILES` (`get(key)`, `put(key, data)`,
  `delete(key)`, `list(prefix)`) backed by `data/files/`, and
  `crypto.getRandomValues` / `crypto.randomUUID` backed by the host CSPRNG.
* WebSocket: `export default { websocket: { open(ws), message(ws, data), close(ws) } }`
  for requests with `Upgrade: websocket`; `ws.send(text)`.
* Limits per request: 64 MiB wasm memory and 10 s wall-clock time (sleeps,
  timers and database waits count); a runtime that hits a limit is discarded
  and replaced. ATTACH, VACUUM INTO and extension loading are blocked;
  env.FILES is capped at 1 GiB. Database growth is not capped by SQLite; the
  per-flat disk quota refuses new uploads and the console reports flats over
  quota. JS has no file system or network access except through `env`.
* `.wasm` entries: a WASI preview1 module reading the request as JSON on stdin
  and writing the response JSON on stdout, with the same env and data dir.

## HTTP API

All JSON. Errors: `{"error": "...", "problems": [{"path","message","fix"}], "health": {...}}`.
Status codes come from typed errors: 400 invalid input, 403 not allowed from
this surface, 404 not found, 409 conflict, not deployed or a feature disabled
on this host, 415 missing client header (below), 422 bundle validation or a
failed deploy health check, 500 server fault.
Agent surface (`/api`, `via` = `api`, or `cli` with `X-Flats-Client: cli`).
Browser requests with `Sec-Fetch-Site: cross-site` or `same-site`, or with an
`Origin` other than the requested host, are refused (403). Requests other than
GET/HEAD must send `Content-Type: application/json`, an archive type
(`application/gzip`, `application/x-tar`, `application/zip`,
`application/octet-stream`) or an `X-Flats-Client` header (415 otherwise), so
a web page cannot send them without a CORS preflight, which Flats never
answers:

| Method | Path | Purpose |
|---|---|---|
| GET | /api/status | host, networks, limits |
| GET | /api/flats | list |
| POST | /api/flats | `{slug, name}` create |
| GET | /api/flats/{slug} | details (URLs, live version, notice) |
| POST | /api/flats/{slug}/versions | body: tar/tar.gz/zip; query `git_sha`, `git_dirty`, `message`, `deploy=1` |
| GET | /api/flats/{slug}/versions | list |
| GET | /api/flats/{slug}/versions/{n} | one version |
| GET | /api/flats/{slug}/versions/{n}/files/{path} | a file (thumbnails) |
| POST | /api/flats/{slug}/deploy | `{version}` |
| POST | /api/flats/{slug}/rollback | `{version}` (0 = previous) |
| GET | /api/flats/{slug}/deployments | history |
| POST | /api/flats/{slug}/previews | `{version}` |
| GET | /api/flats/{slug}/previews | open previews |
| DELETE | /api/previews/{host} | close a preview |
| POST | /api/flats/{slug}/visibility | `{visibility, reason}` (may return pending_approval) |
| POST | /api/flats/{slug}/rename | `{slug}` |
| DELETE | /api/flats/{slug} | `?reason=` (returns pending_approval) |
| GET | /api/flats/{slug}/logs | `?kind=&after=&limit=` |
| GET | /api/flats/{slug}/secrets | names only |
| PUT/DELETE | /api/flats/{slug}/secrets/{name} | CLI on loopback only (operator) |
| GET | /api/flats/{slug}/stats | daily page views |
| GET | /api/approvals/{id} | poll an approval |

Console surface (`/console/api`, `via` = `console`, guarded as above): the same
reads plus `POST /console/api/approvals/{id}/{approve|reject}`, deploy,
rollback, visibility, rename, delete, preview, secrets, `GET/PUT settings`,
`GET system`. Approve/reject return the approval plus `decided_by` (tailnet
login, console node only). `GET settings` lists `apply_on_restart`; `PUT
settings` returns `restart_required` with the changed keys among them.

MCP (`/mcp`, Streamable HTTP, stateless): tools `list_flats`, `get_flat`,
`create_flat`, `save_version` (inline files, text or base64), `save_version_from_dir`
(loopback callers only), `deploy`, `rollback`, `list_versions`,
`open_preview`, `set_visibility`, `delete_flat`, `get_logs`,
`get_approval`, `list_secrets`.
