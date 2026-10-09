# Flats design

Flats is one CGO-free Go binary (`flats`) that hosts agent-built websites
("flats") on the operator's own machine. This document fixes the decisions the
PRD left to design: the manifest format, the server-flat handler ABI, node key
expiry, secret storage and the process layout. It also records the HTTP API
that every surface shares.

## Process layout

```
flats serve                     (one long-running process; launchd on macOS, systemd on Linux)
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

Data directory (default `~/Library/Application Support/Flats`; `host.data_dir`
in `config.json`, or `--data`/`FLATS_DATA` for a host started without
`--config`; a relative path is made absolute at startup, since workers run
with `/` as their working directory). [Configuration and
storage](configuration.md) is the full map, including what to back up:

```
config.json                     operator configuration (see configuration.md)
config-history/                 previous config.json copies
flats.lock                      lifetime exclusive advisory lock (never unlink while hosts run)
flats.db                        metadata and durable runtime activation generations
backups/                        flats.db copies taken before schema migrations
secret.key                      32-byte AES-256-GCM key for flat secrets (0600)
flats/<slug>/versions/<n>/      immutable version files (read-only, 0444)
flats/<slug>/data/              server-flat data: db.sqlite and files/
flats/<slug>/previews/<host>/   preview copy of data/ (removed with the preview)
tsnet/<host>/                   tsnet node state (one directory per node)
runtime/docs/<app>-<content>/  read-only embedded docs modules and generated content.js
portal/<slug>.json              Portal identity (keeps the public hostname stable)
```

The host acquires `<data>/flats.lock` immediately after creating the directory,
before opening SQLite, restoring flats or initializing runtime/network state.
The advisory lock uses the persistent file inode; do not remove or replace it.
Successful shutdown and process exit release it, and failed startup cleans up
before releasing it. `secret.key` is generated into a private synced temporary
file and atomically linked into place without replacing a concurrent winner;
readers see either no key or all 32 bytes.

Shutdown drains the management server (5s; force-close on failure), stops core
workers, closes public then private networks, closes SQLite and releases the
lock. Failures are joined with component names and returned to the caller;
`flats serve` reports them as command errors. Host and Portal Close are
idempotent, including concurrent calls. Portal has one outer 30s network bound
and a 30s per-exposure bound spanning HTTP/watch drain, SDK close and listener
pump. Normal Portal shutdown drains HTTP and explicitly closes exposures before
cancelling their SDK parent context, so unregister errors reach the caller.
The outer bound also covers admitted Serve/Stop/SetHidden operations; cancellation
at the deadline unblocks context-aware SDK work. SDK close is attempted even if
drain times out. Tailscale already bounds
Stop/Close at 30s. Expected listener closure/context cancellation is ignored;
other errors and exceeded deadlines are reported. A stuck third-party call
cannot be forcibly stopped in Go: cleanup may finish later, or only at process
exit. On any shutdown deadline, the host retains its directory lock until
process exit to prevent another host from racing background state cleanup.
These bounds cover the network implementations, not arbitrary worker/store
failures or an injected implementation that ignores the network contract.

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
* **Retention.** After every deploy (publish, redeploy or rollback), files of
  versions beyond the newest `keep_versions` (default 10) are deleted unless
  the version is live; the deploy closes the flat's previews first, so a
  previewed version is protected only if its preview failed to close. The row
  stays with `pruned=1`.

## Manifest (`flats.json`, optional)

```json
{
  "type": "flat",
  "name": "My blog",
  "kind": "static",
  "entry": "index.html",
  "spa": false,
  "not_found": "404.html",
  "health": "/",
  "screenshot": "screenshot.png"
}
```

* `type`: `flat` (default website) or `docs` (Markdown documents).
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

## Content types

The [content types contract](content-types.md) describes `flat` websites and
`docs` Markdown documents. Docs uploads retain their original files and run
through the embedded docs app on the existing server-flat worker. Core writes
read-only modules under `runtime/docs/`, serves uploaded assets, and sets
`X-Flats-Access: private|public` according to each route. Types are derived
from stored manifest JSON; there is no additional type store column. A durable
host counter orders runtime activations independently of published versions;
rollback merges target Markdown against the live document's current seed.
Public and preview route lifetimes cancel admitted requests, closing upgraded
connections when exposure is withdrawn.

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
  shows the conflict). Visibility is only `private` or `public`; older stored
  `public-listed`/`public-unlisted` values read as `public`, and Flats always
  serves public flats unhidden in relay listings. Relays: the
  Portal CLI default (discovery, up to 3 active relays) unless
  `portal.relays` in `config.json` lists explicit relays.
  `portal.discovery: false` uses only the explicit relays (it requires at
  least one) and `portal.max_active_relays` caps the relays discovery adds
  (default 3). These settings are read when `flats serve` starts; the
  settings API reports them in `apply_on_restart` and returns
  `restart_required` when a change needs a restart.
  With discovery and no explicit relays, the first discovered relay that
  becomes ready for a flat is pinned: added as an explicit relay (which
  discovery never drops) and saved to `portal/<slug>.relay`, so the flat's
  public URL survives discovery reshuffles and restarts. On a real run,
  discovery replaced both relays of a public flat within two minutes, which
  broke the link the operator had shared. The pin moves only when its relay
  fails permanently (for example the name is taken there) or has not been
  ready for 24 hours while another relay is; other relays keep serving
  meanwhile.
  Discovery-only startup can take about
  a minute; the API returns the public URL once a relay is ready. Public is
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
* **One ACME account.** tailscaled keeps a node's Let's Encrypt account key
  in `<node dir>/certs`; Flats gives every node the same key
  (`tsnet/acme-account.key.pem`, adopted from an existing node the first
  time), because Let's Encrypt allows only 10 new accounts per IP address
  in 3 hours.
* **macOS fork safety.** A fork in this multi-threaded process can wedge the
  child in Network.framework's atfork handler (golang/go#56784); the parent
  then holds `syscall.ForkLock`, which darwin socket creation also needs,
  and the process loses its networking. tailscale's LocalAPI client forked
  `lsof` on every request to find the GUI app's token; Flats sets fixed
  in-process credentials so it never does (the in-process LocalAPI needs no
  token), and `internal/forkwatch` kills a child that stays between fork and
  exec for over 10 s (no `P_EXEC` flag), which releases the lock. Worker
  starts are the only forks left.
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
* `credentials.tailscale_authkey_file` in `config.json` names a file with a
  reusable untagged auth key for new nodes; it is never logged and only read
  at startup. A host started with `--config` refuses the `TS_*` environment
  variables tsnet would otherwise read.

## App environment variables

Ordinary app-scoped strings are stored in `flats.db` separately from encrypted
secrets. Management API, CLI, MCP and console clients can read and write these
values; they are unsuitable for credentials. Names match `[A-Z_][A-Z0-9_]*`,
with a maximum of 64 characters. `DB`, `FILES`, `__PROTO__`, `PROTOTYPE` and `CONSTRUCTOR` are reserved for new writes in both namespaces.
Values may be empty, are at most 64 KiB, and must be valid UTF-8 without NUL. Ordinary
variables and secrets cannot share a name, even if their values match.

`flats env set <slug> <name> <value>`, `flats env ls <slug>` and
`flats env rm <slug> <name>` manage ordinary variables. JavaScript receives
strings at `env.NAME`; WASI receives environment variables. No host environment
is inherited and no values are substituted into static/frontend files or builds.
Activation combines ordinary variables with decrypted secrets in one consistent
store snapshot without exposing secret values through ordinary management responses.
Historical secret records are retained without applying the new write validation
at read time. JavaScript continues to expose `DB` and `FILES` as host bindings
even if historical secrets have those names; WASI receives their historical
string values. Other historical names and values keep their runtime behavior,
including WASI rejecting NUL values as before. New writes must satisfy the
current validation, even when replacing a historical secret.

Settings are desired configuration, not code-version metadata. Saving or deleting
a value leaves running workers and previews on their startup snapshot. An approved
deployment, redeployment, rollback or standalone data snapshot restoration captures current variables and secrets when activation
begins and uses that snapshot for both health checking and live startup. Settings
are not pinned when approval is requested or to historical code versions. Writes
after capture apply at the next activation. Preview creation and Flats host
restart load current settings; automatic worker restarts reuse their captured
snapshot. To apply a change intentionally, redeploy using the existing operator
approval flow.

## Outbound HTTP permissions

App origin grants are separate from inbound network provider permissions. They
default to deny and are operator-managed through console or local CLI; MCP can
read them with `get_network`. Activation captures grants with env/secrets, and
automatic worker replacement retains that snapshot. Clearing desired grants
requires redeploy to revoke running access. The host validates public addresses,
pins DNS results, ignores proxies and refuses automatic redirect following.
See [external API setup and limits](external-api.md) for the complete contract
and separate browser-side CORS/CSP behavior. WASI has no outbound HTTP ABI.

## Secrets

Values are sealed with AES-256-GCM (random nonce, AAD = secret name) under
`<data>/secret.key`. Only the console and the local CLI (`flats secret set`,
loopback only) can set or delete values; APIs and MCP return names and update
times only. The CLI is recognized by its `X-Flats-Client: cli` header on the
loopback listener, so this stops MCP and remote API clients, not a process
with a shell on the Flats host (see Approvals). Values reach JavaScript as
`env.NAME` and WASI as environment variables from the captured activation
snapshot. Changes apply on the next approved deployment, redeployment, rollback or standalone data snapshot restoration,
or Flats host restart; automatic worker restarts reuse the captured settings.

## Approvals

Agents (MCP, CLI, HTTP API) cannot publish, activate, roll back, restore
data, change visibility in either direction (private → public or public →
private) or delete a flat on their own. Each such request freezes its
parameters in a pending approval and returns
`approval_url` = `<console>/approvals/<id>`. Nothing changes until the
operator approves it in the console; a frozen parameter that changed before
the decision makes the approval fail as `stale_approval`. Deleting a flat
from the console itself applies directly after its confirm dialog.

There are no accounts, so operator authority rests on where a request comes
from and what it carries. Enforced:

* MCP, the CLI and `/api` have no approve, reject or settings operation, and
  their visibility and delete calls only create approvals. `flats config`
  edits `config.json` only while no host runs (it takes the data lock).
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

The authoritative [runtime API v1 reference](runtime-api-v1.md) is embedded
in the binary and discoverable as MCP resource `flats://docs/runtime-api/v1`
or read-only tool `get_runtime_reference`. It requires no skill/source access.

A server flat runs in a `flats worker` child process (one per running
version, previews included). The parent proxies HTTP to the worker over a
Unix socket; the worker gets the version directory, its data directory and
its flat's configured environment variables and secrets, and nothing else from the parent.

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

* `env` holds the flat's ordinary environment variables and secrets plus `env.DB` (`query(sql, ...params)` →
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
  quota. JS has no general filesystem or socket access; outbound `fetch` is host-mediated and requires exact operator-granted origins.
* `.wasm` entries: a WASI preview1 module reading the request as JSON on stdin
  and writing response JSON on stdout. Each request creates a fresh instance.
  Environment includes only the flat's configured environment variables and secrets, with no inherited host process environment. Clocks, cancellable
  sleeps and CSPRNG are available. There are no preopened directories, filesystem/network
  mounts, SQLite/FILES host imports or WebSocket connection API. The worker's
  host-side data directory is **not** mounted into the WASI guest.

| Capability | JavaScript (`.js` / `.mjs`) | WASI preview1 (`.wasm`) |
|---|---|---|
| Request/response | Worker-style `fetch(request, env)` | JSON stdin/stdout; fresh command instance per request |
| SQLite | `env.DB.query` / `exec` | Unavailable |
| Persistent files | `env.FILES.get` / `put` / `delete` / `list` | Unavailable |
| WebSocket | `websocket.open` / `message` / `close`, `ws.send` | Unavailable; Upgrade remains an ordinary HTTP request |
| Outbound network | Bounded HTTP(S) `fetch` to exact operator-granted origins; no sockets | Unavailable |
| Filesystem | Read-only bundled modules; no arbitrary host filesystem; persistence via DB/FILES | No mounts or preopened directories |
| App environment variables and secrets | `env.NAME` (strings) | Only the flat's configured variables and secrets as environment variables |
| Clocks/randomness | timers, `Date`, Web Crypto CSPRNG | WASI clocks, cancellable sleeps and CSPRNG |

WASI request JSON is `{method, url, headers, body}` (body is a string or null).
Response JSON is `{status, headers, body}` (body is a string); binary
responses put base64 text in `body` and set `body_base64: true`. Stderr is forwarded as flat log lines.
These are executable contracts in `internal/runtime` tests, including rejected
JS host imports, absent arbitrary filesystem/socket access, bounded outbound fetch, app-scoped environment,
clocks/randomness, and no WebSocket negotiation.

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
| GET | /api/flats/{slug}/document?doc= | live docs Markdown, else Current Draft |
| GET | /api/status | host, networks, limits, build version |
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
| GET | /api/flats/{slug}/network | `{origins, updated_at, note}`; desired server HTTP(S) grants |
| PUT | /api/flats/{slug}/network | `{origins: [...]}`; operator console or local CLI on loopback; `[]` clears |
| GET | /api/flats/{slug}/env | `{env: [{name, value, updated_at}], note}`; ordinary values only |
| PUT | /api/flats/{slug}/env/{name} | `{value}` (string, including empty); save desired config |
| DELETE | /api/flats/{slug}/env/{name} | remove ordinary variable |
| GET | /api/flats/{slug}/secrets | names only |
| PUT/DELETE | /api/flats/{slug}/secrets/{name} | CLI on loopback only (operator) |
| GET | /api/flats/{slug}/stats | daily page views and top paths (`?days=7` or `30`, up to 365 UTC calendar days) |
| GET | /api/approvals/{id} | poll an approval |

Console surface (`/console/api`, `via` = `console`, guarded as above): the same
reads plus `POST /console/api/approvals/{id}/{approve|reject}`, deploy,
rollback, visibility, rename, delete, preview, env, secrets, `GET/PUT settings`,
`GET system`. Approve/reject return the approval plus `decided_by` (tailnet
login, console node only). `GET settings` lists `apply_on_restart`; `PUT
settings` saves to `config.json` and returns `applied` and
`restart_required` with the changed keys among them. `GET settings` also
describes the file (`config`: mode, ETag, `changed_on_disk`, read-only host,
network and credential values without paths, per-key sources and flag pins).
The console sends `If-Match`; a save answers 412 `config_changed` when
`config.json` changed since the host read it or the page is stale, and 409
when the key is pinned by a flag of a service that still runs without
`--config`. `POST settings/impact` reports, without writing, what the next
pruning would remove after lowering `keep_versions`, `events_keep` or
`preview_ttl_seconds`; the console shows it before such a save.

MCP (`/mcp`, Streamable HTTP, stateless): tools `list_flats`, `get_flat`,
`create_flat`, `save_version` (inline files, text or base64), `save_version_from_dir`
(loopback callers only), `deploy`, `rollback`, `list_versions`,
`open_preview`, `set_visibility`, `delete_flat`, `get_logs`,
`get_approval`, `get_network`, `list_env`, `set_env`, `delete_env`, `list_secrets`, `save_draft`, `get_draft`, `publish`,
`save_document`, `get_document`, `get_runtime_reference`, `get_content_types`.
Content type discovery: resource `flats://docs/content-types/v1`.


## Per-flat console management

The flat list's menu offers Share, Analytics and Settings. Share changes the
private / public visibility (an approval request either way) and provides Visit
and Copy link. Private means access through tailnet ACLs, not owner-only access.
Email invitations, profile showcasing and custom domains are not supported.

`/flats/{slug}/settings`, `/analytics` and `/database` share Settings, Analytics
and Database navigation; there is no Scheduled tab. Settings manages the display
name, slug, sharing, ordinary environment variables, encrypted secrets and deletion. The existing
`/flats/{slug}` page retains versions, previews, deployment and logs. Database
lists actual pre-deploy snapshots and links to deployment/rollback controls.

Analytics displays daily page requests and top paths for 7 or 30 days. Unique
visitors are not tracked. Per-path counters start with this update, omit query
strings, follow slug renames and disappear on deletion. In-memory path buffers
are bounded and flushed in transactional batches alongside existing counters.
