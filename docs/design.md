# Flats design

Flats is one CGO-free Go binary (`flats`) that hosts agent-built websites,
pages, documents and designs ("flats") on the operator's own machine. This
document is the architecture overview and trust model: the process layout,
the manifest and content types, how content becomes live, exposure, app
settings and secrets, approvals and the server-flat handler ABI. The HTTP API,
CLI, MCP and console that share one service are described in
[surfaces.md](surfaces.md).

## Process layout

```
flats serve                     (one long-running process; launchd on macOS, systemd on Linux)
├── core.Service                flats, Drafts, versions, deploys, previews, approvals and
│                               revisions (internal/core)
├── store                       SQLite metadata <data>/flats.db (modernc.org/sqlite, WAL)
├── provider manager            network providers (internal/expose/provider): local,
│                               tailscale, tailscale-funnel, portal, zrok, cloudflare-quick,
│                               netbird, netbird-expose
├── management server           HTTP API + MCP (/mcp) + web console + /metrics on 127.0.0.1:7878,
│                               and on each configured console domain
├── design instances            in-process canvas handlers, one per live design (internal/design)
└── server-flat workers         `flats worker` child processes, one per running server flat
                                version or document (re-exec of the same binary; internal/runtime)
```

The default private network is `local`: Flats serves each flat and preview at
`http://<host>.localhost:<port>` on a loopback port. A flat gets a route on
another network only when the host permits that provider and the flat holds
its grant. Tailscale gives every permitted flat, preview and console domain
its own node; NetBird gives every permitted flat and console domain its own
peer, but no preview; the console runs on a tailnet or NetBird
only when `tailscale.console_hosts` or `netbird.console_hosts` lists names
(both empty by default). Public providers serve only Public flats. The
operator guide is [Private and public access](networking.md) and the provider
contract is [Network provider lifecycle](network-provider-lifecycle.md).

The data directory defaults to `~/Library/Application Support/Flats` on macOS
(`host.data_dir` in `config.json`, or `--data`/`FLATS_DATA` for a host started
without `--config`; a relative path is made absolute at startup, since
workers run with `/` as their working directory). [Configuration and
storage](configuration.md#where-data-lives) maps every file and says what to
back up; the revision blob directories of documents and designs are described
in [Revision store](revision-store.md#blobs).

The host acquires `<data>/flats.lock` immediately after creating the directory,
before opening SQLite, restoring flats or initializing runtime/network state.
The advisory lock uses the persistent file inode; do not remove or replace it.
Successful shutdown and process exit release it, and failed startup cleans up
before releasing it. `secret.key` is generated into a private synced temporary
file and atomically linked into place without replacing a concurrent winner;
readers see either no key or all 32 bytes.

Shutdown drains the management server (5 s; force-close on failure), stops
core workers, closes public then private networks, closes SQLite and releases
the lock. Failures are joined with component names and returned to the caller;
`flats serve` reports them as command errors. Host and Portal Close are
idempotent, including concurrent calls. Portal has one outer 30 s network
bound and a 30 s per-exposure bound spanning HTTP/watch drain, SDK close and
listener pump. Normal Portal shutdown drains HTTP and explicitly closes
exposures before cancelling their SDK parent context, so unregister errors
reach the caller; SDK close is attempted even if drain times out. A Portal Stop
that fails keeps the exposure tracked, and a retry resumes the pending stages
instead of reporting success. The SDK closes an exposure only once, so a
failed unregister stays reported until the relay must have dropped the
unrenewed lease (2 minutes after close); Serve and SetHidden never revive an
exposure being torn down. Tailscale bounds Stop/Close at 30 s. Expected
listener closure and context cancellation are ignored; other errors and
exceeded deadlines are reported. A stuck third-party call cannot be forcibly
stopped in Go: cleanup may finish later, or only at process exit, so on any
shutdown deadline the host keeps its directory lock until process exit. These
bounds cover the network implementations, not arbitrary worker/store failures
or an injected implementation that ignores the network contract.

## Model and flows

Every flat follows one of two content models
([content-models.md](spec/content-models.md)). A flat keeps the type of its
first save, so its model never changes.

**Version model** (websites and pages):

* **Draft.** Every save validates the bundle and stores an immutable Draft
  revision; the Draft is always Private and never touches live content.
* **Version.** Only an approved `create_version` freezes the Draft as `vN`: the
  files go to `versions/<n>` and a row records number, content hash (SHA-256
  over sorted `path NUL sha256` lines), size, file count, kind, manifest,
  optional git SHA and dirty flag. Versions never change, are never deleted
  automatically and their numbers are never reused.
* **Deploy.** An approved activation of a saved version health-checks it on a
  copy of the data (GET manifest `health`, default `/`, 15 s timeout, 2xx/3xx
  passes; a failure reports the status and the first 300 bytes of body),
  snapshots the data, starts the version on live data and then moves the live
  pointer and records the deployment in one SQLite transaction
  (compare-and-swap on the previous live version). The previous handler
  drains for 5 s before it stops.
* **Rollback** is an activation of an earlier saved version (default: the
  version that was live before the current one), recorded with kind
  `rollback`. Data belongs to the flat (`data/`), not to versions: a rollback
  keeps the live data unless it is requested with `restore_data`, which
  restores the snapshot taken before the current version was deployed.
* **Quota.** A host version quota (default 20; each flat may override it, 0 is
  unlimited) refuses creation at request and approval time. Deleting a
  non-live version closes its previews and keeps data snapshots.

**Revision model** (documents and designs): there is no Draft, version,
preview or deployment. A save or an edit changes the live content at once,
without approval, and records a whole-flat revision; history, pins and
rollback live in the [revision store](revision-store.md). Visibility changes
and deleting the flat still need approval.

The lifecycle state machine, approval freezing and data effects are specified
in the [lifecycle core contract](lifecycle-core-contract.md).

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

* `type`: `flat` (website, the default), `page` (one static HTML page),
  `docs` (Markdown documents), `design` (canvas of HTML artboards) or `board`
  (whiteboard); only core writes a design or board manifest, see
  [Designs](#designs) and [Boards](#boards). A bundle without `type`,
  `kind`, `index.html` or a server entry that holds a Markdown entry is a
  `docs` bundle.
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
  are skipped. A tar.gz may hold at most 60 000 tar entries (directories and
  skipped files included) and decompress to at most the upload limit plus
  about 235 MiB (4 KiB per allowed entry and 1 MiB) of tar headers,
  metadata and skipped bodies.

## Content types

The [content types contract](content-types.md) is the agent-facing
reference. Version-model types (`flat`, `page`) are read from the stored
manifest. A Revision-model flat also has a `content_state` row whose `type` is
its durable type marker ([Revision store](revision-store.md#schema)).

**Type stickiness.** A flat's type is that of its live version; while nothing
runs, that of its latest saved version, else of its Draft, else of its
`content_state` row; a flat never saved is untyped and takes the type of its
first save. A save of another type is refused with `conflict` and a fix that
names the right tool. Activation re-checks the candidate's type at request
time and again when the approval applies, against the live (else latest
saved) version, so content of another type that an earlier release stored
can never go live.

**Revision-model refusals.** Documents, designs and boards refuse Deploy, Rollback,
CreateVersion, DeleteVersion, version quota overrides, previews,
ListVersions and GetDraft (`invalid`, with a message naming the type's own
tools). Revision tools refuse websites and pages (`no_revisions`), and board
tools refuse every other type (`not_board`).

### Pages

A page is one static HTML document, typically with charts or visualizations,
with the website lifecycle. Its manifest may omit `kind` or set `static`;
`spa`, `not_found` and `health` are refused. The entry defaults to
`index.html`, must end in `.html` or `.htm` and exist, and every other HTML
file is refused. A page is served by the static path, with no runtime. Saving
adds one non-blocking lint warning, `externalData`: a string literal naming an
absolute `http(s)`/`ws(s)` or protocol-relative URL as the first argument of
`fetch`, `new EventSource`, `new WebSocket`, `Papa.parse` or a d3-fetch style
loader (`.json`, `.csv`, `.tsv`, `.text`, `.xml`), or as the URL of
`XMLHttpRequest.open`, in the entry's inline scripts or bundled `.js`/`.mjs`
files. Data scripts (`importmap`, JSON, `text/plain`, `text/template`) and
code loading are not inspected, and dynamic URLs are not detected. A page has
no data, so `rollback` with `restore_data` is refused before an approval is
created.

### Documents

Docs uploads keep their original files and run through the embedded docs app
on the server-flat worker (so they need server flats enabled); core writes
read-only modules under `runtime/docs/`, serves uploaded assets and sets
`X-Flats-Access: private|public` on every request according to its route. A
durable host counter orders runtime activations independently of versions.
Each changed save starts a new internal version without approval; people's and
agents' live edits change the worker's text directly. The worker side is in
[the docs app internals](internal/docs-app.md).

### Designs

A design is a canvas of plain-HTML artboards that people and agents edit
live. It runs in the host process, not in a worker, so it needs no runtime.
Its files live in the [revision store](revision-store.md); its only bundle is
a shell `flats.json` that core writes once at creation, and every public
bundle path refuses a design manifest. A Go handler (`internal/design`)
serves the canvas app (plain JavaScript and CSS embedded from
`internal/design/web`, with no build step and no third-party code), the
design's files, a JSON API and an event stream on the flat's own origin.

**Paths.** Every path follows one grammar: segments match
`[A-Za-z0-9_][A-Za-z0-9._-]{0,99}`, at most 8 segments and 255 bytes, no `.`
or `..`, and not `flats.json`, `_design/...` or `.well-known/...`. Two paths
may not differ only in letter case, and no file may also be a directory of
another. A design holds at most 512 files; each file and each agent write is
at most `upload_max_bytes`. Artboards are the `.html` files; each is UTF-8
with at most one tweaks block (`<script type="application/json"
data-flats-tweaks>`).

**`canvas.json`** is required, a JSON object of at most 256 KiB: `title` (at
most 200 characters); `boards` keyed by artboard paths in the design, each
with finite `x`/`y`, `w`/`h` from 40 to 8000, optional `title` (200
characters) and `fill`; `order`, a permutation of the board keys (missing
keys are appended in path order); at most 200 `notes` keyed by
`[A-Za-z0-9_-]{1,40}`, each with `x`, `y`, `text` (4000 characters) and
optional `w` and `color` (`gray`, `yellow`, `blue`, `green`, `pink`,
`purple`); and `launch`, `{"view":"canvas"}` or
`{"view":"focused","board":<key>}`. Unknown top-level keys are kept. Every
problem is reported at once.

**Editing guards.** People's operations carry the file's generation (`gen`,
renewed when an agent write changes the file or a rollback restores it) and an exact
guard on what they change: a text edit names the editable element and the
SHA-256 of its inner source, a tweak or canvas edit names the base value. A
stale guard is refused with `conflict`. Text edits are sanitized to an inline
allow-list with only `class` and `style` attributes.

**Person operations** (`POST /_design/api/ops`, `{editor, op}`;
`internal/design/ops.go`):

| Operation | Guard |
| --- | --- |
| `text.set {path, gen, node, tag, inner_hash, html}` | `gen` is current, node `node` has tag `tag` and the SHA-256 of its exact inner source equals `inner_hash`; the sanitized result is at most 64 KiB and has text |
| `tweak.set {path, gen, name, value, base_value?}` | `gen` is current and, when given, `base_value` equals the current value; the new value is validated for its tweak type |
| `board.set {path, gen, fields, base}` (`x`, `y`, `w`, `h`, `title`) | each field given in `base` equals its current value |
| `board.order {path, gen, to: front\|back, base_index}` | the board's current index in `order` equals `base_index` |
| `note.add {gen, id?, fields}` | the id does not exist yet (a missing id is generated) |
| `note.set {gen, id, fields, base}` | as `board.set` |
| `note.delete {gen, id, base}` | `base` is the complete note (`x`, `y`, `w`, `text`, `color`) and equals it |

Every canvas operation also requires the current `gen` of `canvas.json`.
The `base` and `base_value` guards are checked only for what the caller
sends; an omitted guard does not protect against an intervening edit. Canvas
operations keep unknown keys and validate the resulting canvas as above. The
canvas app reads an element's `inner_hash` and inner source from
`GET /_design/api/node`, never from the artboard.

**Routes** (`internal/design/handler.go`):

| Route | Access | Purpose |
| --- | --- | --- |
| `GET /` | all | canvas app shell |
| `GET /_design/app/<file>` | all | embedded app assets |
| `POST /_design/api/session` | all | issue a resource capability |
| `GET /_design/api/state` | all | `{slug, access, seq, revision, pending, visibility, canvas, files, artboards}`; public responses omit file `gen` |
| `GET /_design/api/events` | all | event stream: `hello`, `change`, `resync`; heartbeat every 25 s |
| `GET /_design/api/node?path=&node=&gen=` | private | `{tag, inner, inner_hash, gen}` of an editable element |
| `POST /_design/api/ops` | private | one person operation |
| `GET /_design/api/revisions[/{n}]`, `POST …/{n}/pin`, `POST …/{n}/rollback` | private | history, pins and rollback ([revision store](revision-store.md)) |
| `GET /_design/c/<cap>/<path>` | holder of `cap` | a file of the capability's scope |
| `GET /_design/export.zip` | all | export of one coherent head snapshot |
| `GET /<path>` | all | a head file, for opening an artboard directly |

**Access and write protection.** The handler reads access from
`X-Flats-Access`, which core sets; anything but `private` is public. A public
route is view only: no editing, history, node or revision routes, and state
responses omit `gen`. Every POST except the session request requires private
access, `Content-Type: application/json`, `X-Flats-Design: 1` and
`Sec-Fetch-Site: same-origin` (or, when that header is absent, an `Origin`
equal to the request's scheme and host). The session request follows the same
origin rule without the access requirement. Person operations go through the
flat's request limiter and a per-design limit of 20 operations per second.

**Artboard isolation.** Every design file response carries
`Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups
allow-modals allow-downloads`, `Referrer-Policy: no-referrer`,
`X-Content-Type-Options: nosniff` and `Cache-Control: no-cache` with a content
ETag, so an artboard always runs at an opaque origin, even when opened
directly, and cannot call the API. The app shell sends
`Content-Security-Policy: default-src 'self'; frame-src 'self'; img-src 'self'
data:; style-src 'self'; script-src 'self'; connect-src 'self';
frame-ancestors 'self'; base-uri 'none'; form-action 'none'` and
`X-Frame-Options: SAMEORIGIN`. Artboard iframes carry no `sandbox` attribute,
since some embedded browsers refuse sandboxed frames and the response CSP
already isolates them.

**Capabilities.** An artboard at an opaque origin needs CORS to load its
bundled data or fonts, but a Private design's files must not be readable by
other sites, so CORS is granted only under a capability path. The app shell
obtains a 128-bit random token bound to the flat, the request's access level
and one scope (the head, or one revision on a private route only); the canvas
loads each artboard from `/_design/c/<cap>/<path>`, so relative URLs inside it
stay under the same token. Those responses send
`Access-Control-Allow-Origin: *`; an unknown token gets 404. Tokens live in
memory, at most 256 per flat with the least recently extended evicted; each
use extends a token to 12 hours, and all tokens end when the instance stops.
A direct `GET /<path>` sends CORS only on a Public route, whose content is
world-readable anyway.

**The bridge is untrusted.** A script in each served artboard reports picks
and navigation to the canvas, but any artboard script can send the same
messages. The canvas accepts messages only from iframes it created, treats
each as a hint, fetches the element's current source and guard from the API
itself, and edits in its own `contenteditable` box: keystrokes and
confirmation never reach the artboard, so every write needs input the person
made in the canvas app.

**Events and teardown.** A write publishes `change` after its transaction
commits; a stream starts with `hello {seq}` and sends a heartbeat every 25 s.
At most 32 streams per flat; each queue holds 16 events, and a full queue is
cleared and gets `resync`. Event-stream and export writes run under a 15 s
write deadline. Stopping the instance (rename, delete, replacement, shutdown)
refuses new requests with 503, cancels every request and interrupts blocked
writes, and waits up to 5 s for handlers to return.

### Boards

A board is an infinite-canvas whiteboard of stickies, shapes, connectors, text,
pen strokes, images and frames that people and agents edit live
([product spec](spec/board.md), [technical design](spec/board-TECH.md)). Like
a design it runs in the host process behind a shell `flats.json` that core
writes once at creation, and every public bundle path refuses a board
manifest. `internal/board` runs one `board.Instance` per board: a single apply
loop that changes `flats/<slug>/board/board.db`, the board's only source of
truth, the canvas app embedded from `internal/board/web` (plain JavaScript,
no build step, no third-party code), one same-origin WebSocket
(`/_board/ws`) and the board's HTTP routes ([Surfaces](surfaces.md)).

- **Editing.** People's batches merge per property bundle (the last write
  wins); agent calls (`update_board`, `import_board`) apply whole or not at
  all, with guards. Edits are durable before they are acknowledged.
- **History.** The instance records revision boundaries in an outbox in the
  same `board.db` transaction as the change; core imports them into the
  [revision store](revision-store.md#boards) under the flat lock, every 5
  seconds when entries are waiting and at once after agent writes and
  rollbacks.
- **Core's part.** Core builds the instance with the live shell version and
  gives it a backend: limits, the flat directory measurement for the disk
  quota, blob storage in `flats/<slug>/revisions/blobs`, history, pins and
  rollbacks, and an exclusive section under the flat lock for imports from the
  canvas. Health checks and trial runs of the shell get a plain handler and
  never open `board.db`.
- **Access.** The board trusts only the `X-Flats-Access` core sets; a Public
  route gets only the current board, read-only. Withdrawing Public access
  cancels the route epoch, which closes every open Public socket. Rename and
  delete stop the instance and tell open canvases why (`renamed`,
  `deleted`); host shutdown stops it silently.

## Exposure

Each flat and each preview is its own host and origin (`<slug>` and
`<slug>-<8 random>`), so cookies and storage never mix. Under one parent
domain, such as the reverse proxy's, sibling flats share a registrable domain
and can set each other's cookies unless that domain is on the Public Suffix
List. Local always serves the Private route on loopback. Tailscale, NetBird
and the Private reverse proxy (`proxy`) add Private routes only for flats that
hold their grant. A Public flat is also served by its permitted public
providers (Tailscale Funnel, Portal, zrok, a Cloudflare quick tunnel, NetBird
Expose or the public reverse proxy); Public is never access control, and every
response that returns a public URL carries that notice. Draft and version
previews (`open_preview`; version 0 is the Draft) are Private only, on Local
and, when the flat holds the grant, Tailscale and the Private reverse proxy;
they get an isolated copy of server data, close on the flat's next activation
and expire after `preview_ttl_seconds` (24 h) without a visit. Identity
headers are stripped on public routes and on the reverse proxy listener. A
flat reports the address on the host's default network when it has one there,
and a rename redirect keeps a visitor on the network they arrived on
([Providers](networking.md#providers)). The
operator guide is [Private and public access](networking.md); the provider
manager's contract is [Network provider lifecycle](network-provider-lifecycle.md).

Implementation notes that the provider docs do not repeat:

* **Tailscale nodes.** A Tailscale route is an operator-owned (untagged) tsnet
  node with hostname `<slug>` at `https://<slug>.<tailnet>.ts.net` (ListenTLS;
  port 80 redirects to HTTPS). It is created when the route opens and logged
  out (removed) when the flat is deleted or the grant revoked. The middleware
  deletes client-sent `Tailscale-User-*` headers and sets
  `Tailscale-User-Login` and `Tailscale-User-Name` from `WhoIs` (empty for
  tagged peers).
* **Certificates and readiness.** When the tailnet issues certificates, a node
  fetches its certificate as soon as it listens, retrying with backoff (30 s
  doubling to 10 min, 5 min per attempt). Until it has one the host is
  `starting` with detail "waiting for its HTTPS certificate"; it is `ready`
  only once a certificate was obtained. Flat views carry this as
  `private_state`/`private_detail`, previews as `state`/`detail`. Each host's
  name appears in Certificate Transparency logs and uses one certificate of
  the Let's Encrypt per-domain weekly limit.
* **One ACME account.** Every node uses the same Let's Encrypt account key
  (`tsnet/acme-account.key.pem`, adopted from an existing node the first
  time), because Let's Encrypt allows only 10 new accounts per IP address in
  3 hours.
* **Stopping.** `Stop` (delete, revocation, preview close) logs the node out,
  closes it and removes its state. It waits at most 30 s; a node whose backend
  hangs is removed from the served set and finishes in the background, so
  deploys and deletes never block on it.
* **macOS fork safety.** A fork in this multi-threaded process can wedge the
  child in Network.framework's atfork handler (golang/go#56784) while the
  parent holds `syscall.ForkLock`, which darwin socket creation also needs.
  Flats gives tsnet fixed in-process LocalAPI credentials so it never forks
  `lsof`, and `internal/forkwatch` kills a child that stays between fork and
  exec for over 10 s. Worker starts are the only forks left.
* **Reverse proxy.** The `proxy` and `proxy-public` providers register routes
  on one listener (`proxy.addr`) that an operator's reverse proxy forwards
  to. It routes by `proxy.url_template`: `<label>.<domain>` reaches the route
  of that label, and Flats sets `Host` and `X-Forwarded-Proto` from the
  template before a handler runs. Flats cannot observe the proxy, so a
  registered route is `ready` with a detail that says so
  ([Serving flats behind a reverse proxy](networking.md#serving-flats-behind-a-reverse-proxy)).
* **Portal listing.** Whether a Public flat appears in relay listings is the
  `portal.hide` host default (off) and a per-flat override (`default`,
  `hidden` or `listed`); a change updates a served route's metadata at once.
  `portal_hidden` on the flat view is the requested listing, not the relays'
  observed state. Hiding is not access control. Rename redirect aliases are
  always hidden.
* **Portal relays.** Relays come from the Portal defaults (discovery, up to 3
  active relays) unless `portal.relays` lists explicit ones;
  `portal.discovery: false` uses only the explicit relays and
  `portal.max_active_relays` caps those discovery adds. These settings apply
  at startup. With discovery and no explicit relays, the first discovered
  relay that becomes ready for a flat is pinned (kept as an explicit relay and
  saved to `portal/<slug>.relay`), so the flat's public URL survives discovery
  changes and restarts; the pin moves only when its relay fails permanently or
  has not been ready for 24 hours while another relay is.
* **Portal hostnames.** Each relay gives a flat a canonical
  `<slug>-<address>.<relay>` (a hash replaces slugs over 22 characters),
  bound to its Portal identity, and a friendly `<slug>.<relay>` alias granted
  only while no other identity holds that name. Flats shows the alias when
  granted and the canonical URL otherwise, and remembers each alias it showed
  in `portal/<slug>.alias`. When a ready relay no longer grants it, the status
  detail and log warn that links to the alias can reach another service, until
  the alias returns or the file is deleted. A renamed flat starts without
  alias memory.

## Node key expiry

Operator-owned nodes inherit the tailnet key expiry (180 days by default).
Flats polls each node's `Self.KeyExpiry` and `BackendState` every 5 minutes.

* Within 14 days of expiry, the console shows the node as `key-expiring`.
* On `NeedsLogin` the node is `needs-login`; the console shows the login URL
  (from the IPN bus) so the operator can re-authenticate, or the operator
  disables key expiry for the node in the Tailscale admin console.
* The reusable untagged auth key for new nodes is kept in `flats.db`,
  encrypted with `secret.key`, and never logged
  ([Operator keys](configuration.md#operator-keys)); a replacement applies to
  nodes started afterwards. A host started with `--config` refuses the `TS_*`
  environment variables tsnet would otherwise read.

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

Agents (MCP, CLI, HTTP API) cannot create or delete a version, deploy,
roll back, restore data, change visibility in either direction (private →
public or public → private) or delete a flat on their own. Each such request
freezes its parameters in a pending approval (`create_version`,
`delete_version`, `activate`, `rollback`, `restore_data`, `set_visibility`,
`delete`) and returns `approval_url` = `<console>/approvals/<id>`. Nothing
changes until the operator approves it in the console, or, while
[Autopilot](#autopilot) allows its operations, an agent approves it; a frozen
parameter that changed before the decision makes the approval fail as
`stale_approval`. A
console request for these actions also creates an approval, except two
destructive ones that apply directly after their confirm dialog: deleting a
flat and deleting a non-live version. Saves of documents and designs, and all
revision operations, need no approval (see [Model and flows](#model-and-flows)).

There are no accounts, so operator authority rests on where a request comes
from and what it carries. Enforced:

* MCP, the CLI and `/api` have no settings operation, and no approve or
  reject operation except the two MCP tools of [Autopilot](#autopilot) while
  it is on. Their approval-gated calls only create approvals. `flats config` edits
  `config.json` only while no host runs (it takes the data lock).
* Every request to the management server must name that listener in `Host`,
  from a list the guard reads per request: on the loopback listener
  `127.0.0.1`, `localhost` or `[::1]` (and a specific listen IP) with the
  listen port; on a Tailscale console domain its configured name, its MagicDNS
  name and that name's first label, on ports 80 and 443; on a NetBird console
  domain its configured name and the peer's NetBird names, on port 80. Any
  `Origin` must be one of those too. This defeats DNS rebinding and
  cross-site requests.
* `/console/api` requires `X-Flats-Console: 1` and refuses requests carrying
  `X-Flats-Client` (the CLI and other clients identify themselves with it).
  Mutations also need `Sec-Fetch-Site: same-origin` and an `Origin` equal to
  the requested host, which the console page sends and cross-origin pages
  cannot. A NetBird console domain is plain HTTP, where browsers send no Fetch
  Metadata; there a same-origin `http` `Origin` is accepted instead, while the
  custom header still forces a preflight that Flats never grants.
* Console decisions record who decided, as `decided_by` in the response, the
  approval record and the flat's approval event (an approved delete removes
  the flat with its events; the approval record stays): on a Tailscale console domain the
  approver's tailnet login from `WhoIs` (client-sent `Tailscale-User-*`
  headers are dropped first), on a NetBird console domain `netbird:<peer
  name>` of the requesting peer. Decisions on the loopback listener are
  recorded without an identity.

Not enforced: all of the console checks are request headers. A process that
can send arbitrary HTTP from one of the operator's devices (for example an
agent with a shell on the Flats host, or on a device the tailnet or NetBird
access policy lets reach a console domain) can imitate the console page and
approve or act directly, and a process that can read the Flats data directory
can read the database and secret key. There is no separate operator
credential, so an approval does not prove that a human decided. Approvals hold
against agents that only use MCP, the CLI or the API, and against web pages,
not against arbitrary code running as the operator. Mitigations: write
Tailscale ACLs or NetBird policies so that only the operator's browser devices
can reach the console domains, and run untrusted agents under another OS user
or on another machine, so they reach Flats only through MCP or the API.

### Autopilot

[Autopilot](spec/autopilot.md) lets agents decide pending approvals of the
operations the operator allows. It is off by default, and only the console
changes it ([Experiments routes](surfaces.md#experiments-routes)). Autopilot adds a
second decider and changes nothing else: an agent decision claims, checks and
applies an approval through the same code as a console decision, so stale
checks, provider permissions and the public-edit preflight apply to it as
well. With Autopilot off, MCP serves exactly what it serves without the
feature ([Autopilot tools](surfaces.md#autopilot-tools)).

**Operations.** Each operation is one bit of the stored set
([Autopilot settings](configuration.md#autopilot-settings)). Bit positions
never change or get reused. The console shows the operations in its own order.

| Bit | ID | Console label | Approvals it covers |
| --- | --- | --- | --- |
| 0 | `create_version` | Create versions | `create_version`, legacy `publish` |
| 1 | `delete_version` | Delete versions | `delete_version` |
| 2 | `deploy` | Deploy | `activate` |
| 3 | `rollback` | Roll back | `rollback` with `restore_data` false |
| 4 | `rollback_restore_data` | Roll back with data restore | `rollback` with `restore_data` true, `restore_data` |
| 5 | `make_public` | Make Public | `set_visibility` Private to Public |
| 6 | `make_private` | Make Private | `set_visibility` Public to Private |
| 7 | `public_editing` | Public editing | `set_visibility` that changes a Public access level |
| 8 | `delete_flat` | Delete flats | `delete` |

**What an approval needs.** `AutopilotNeeds` reads only the approval's action
and frozen parameters, never the flat's current state, and fails closed. It
reads parameters only when they are a JSON object with no top-level `null`
member that decodes into the action's parameter type; unknown members are
ignored, as the apply path ignores them. An absent `restore_data` reads as
false. For `set_visibility`:

- the frozen starting visibility must be present, and both raw visibilities
  must be `private`, `public`, `public-listed` or `public-unlisted`. The check
  runs before canonicalization, because canonicalization maps unknown values
  to `private`;
- the requested Public level is `edit` only when the approval froze an access
  sequence and `access` is `edit`, otherwise `view`;
- Private to Public needs Make Public, plus Public editing for `edit`. Public
  to Private needs Make Private. Public to Public needs Public editing when
  the frozen starting level is known and differs.

Any other approval is unclassified: an unknown action, unreadable parameters,
an unknown level, a missing starting level for Public to Public, or a request
for no change. No switch allows an unclassified approval, so agents can
neither approve nor reject it.

**Deciding.** `DecideAsAgent` follows `Decide` and adds these checks, in
order:

1. The agent name must be 1 to 100 code points and not blank (`invalid`).
   Flats keeps it as given and does not verify it.
2. If Autopilot is off, Flats refuses the call as `autopilot_off`.
3. Under the approval's lock, a missing approval is `not_found`. Flats
   refuses an unclassified approval, or one that needs an operation that is
   not allowed, as `autopilot_not_allowed`, for approve and reject alike. The
   message names the missing operations by label and ID, or the reason.
4. The status rules are those of `Decide`: approving an approved approval or
   rejecting a rejected one returns it unchanged without a write, and any
   other decision of a decided or `applying` approval is `conflict`.
5. A pending approval is claimed (`applying`) or rejected by one conditional
   `UPDATE` that also requires the `autopilot` row to be on and to allow every
   needed bit. Console setters are write transactions too, so a switch turned
   off before that statement commits refuses the call, and one turned off
   afterwards neither stops nor undoes the decision. When the statement
   changes nothing, Flats reads the approval and the row again only to name
   the error: `conflict`, `autopilot_off`, `autopilot_not_allowed`, or
   `autopilot_not_allowed` with "Autopilot changed during the call; nothing
   changed; call again".

A claimed approval then finishes exactly as an operator's, including after a
restart. Steps 2 and 3 use the row as read before the lock, only to give
early, specific refusals; step 5 decides. `autopilot_off` and
`autopilot_not_allowed` are listed in `AutopilotErrorCategories`, not in
`ErrorCategories`, because their refusal pages exist only while Autopilot is
on. They never reach a claimed approval, so they are never a `failure_code`.
The HTTP API maps both to 403.

**Records.** An agent decision stores `decided_by` as `autopilot:` followed by
a JSON object: `agent`, `network` (`local`, `listener`, `tailscale`, `netbird`
or `external`) and, when known, `ip`, `login`, `node`, `peer` or `origin`:

```text
autopilot:{"agent":"pr-bot","network":"tailscale","login":"bot@example.com"}
autopilot:{"agent":"pr-bot","network":"listener","ip":"192.168.1.20"}
```

A value counts as an agent decision only when it has the prefix, decodes, has
a non-blank agent and a known network. Console values (`console`, a tailnet
login, `netbird:<fqdn>`) never start with `autopilot:{`. Event texts name the
decider as `the operator` for operator values and, for an agent, as for
example `Autopilot (agent "pr-bot" via Tailscale, bot@example.com)`,
`Autopilot (agent "pr-bot" via Local)` or `Autopilot (agent "pr-bot" via the
console listener from 192.168.1.20)`. The agent name is quoted with Go `%q`, so
it cannot add lines to the event log. With that decider text:

- the decision event reads `<action> approved by <decider>: <result>` when
  the operation applied, `<action> failed by <decider>: <error>` when an
  approved operation failed, and `<action> rejected by <decider>` for a
  rejection, which also stores `rejected by <decider>` as its result;
- the events and host log lines of the applied operation say `via autopilot`
  where a console decision says `via console`. The apply path reads this from
  the stored `decided_by`, so an approval finished after a restart keeps it;
- when a restart finalizes an agent-decided approval from its applied receipt
  or recorded deployment, Flats writes the decision event that the normal path
  writes, only from the call that finalizes the approval. Operator decisions
  finalized that way still write none. The applied operation writes its own
  events while it applies, so a crash between the operation's commit and its
  event loses that event for either decider; recovery does not recreate it.
  The decision event and the approval record are the durable record;
- an approved deletion that succeeds writes no event, since the flat's events
  go with it; a failed one writes the failed decision event.
  The approval row has no foreign key to the flat and keeps `decided_by`.

**Arrival network.** The MCP handler records where each request arrived, in
this order:

1. A declared external origin: `external` with that origin.
2. A Tailscale console domain: `tailscale` with the caller's tailnet login, or
   its node name when the device is tagged. The tsnet identity handler puts
   both into the request context from `WhoIs`, not into headers.
3. A NetBird console domain: `netbird` with the calling peer's name, when
   NetBird knows it.
4. Otherwise the console listener. Only the socket's source address counts;
   `Forwarded`, `X-Forwarded-For`, `X-Real-Ip` and `Tailscale-User-*` are
   never read. The source is `local` when it is a loopback address or one of
   the host's own interface addresses (a process on the host that connects to
   a wildcard-bound listener through its LAN address), else `listener` with
   the source address. Interface addresses are cached for 10 seconds and read
   again once on a miss. An IPv6 link-local address matches only with the zone
   of the interface that holds it. An IPv4 link-local address never matches,
   because it carries no interface, so a caller using one is `listener`.

A reverse proxy on the host that is not declared as an external origin
therefore arrives as `local`, which is true of the connection it makes.

**Settings and the host log.** The `autopilot` row is the only source of
truth. Each console setter runs one `BEGIN IMMEDIATE` transaction that reads
the stored row, decides from it alone and writes the new row with its host
log line in `autopilot_changes`. A cancelled request cannot stop a started
change, and two console tabs changing different switches keep both changes.
Turning Autopilot on carries the operations the confirmation dialog listed;
when the stored ones differ, the host refuses the change as `conflict`. A call that
changes nothing writes and logs nothing.

After each change, and at startup, a sync reads the row and the change lines
not logged yet, writes those lines to the host log with the time of the
change, marks them logged (keeping the newest 100 logged lines) and replaces
the in-memory copy. When marking fails, the next sync writes those lines
again, so each change reaches the log at least once. MCP reads the in-memory copy.
After a failed read it reads again; while that fails it reports Autopilot off
and logs `autopilot: settings unavailable` at most once a minute. This hides
the tools and never authorizes anything, since decisions check the row
itself. The console's GET reads the database directly, so a database it
cannot read is an error there. Log lines name the change and who made it,
never the guide text:

```text
autopilot: turned on by tailnet user "alice@example.com"; agents may decide: create_version, deploy (at 2026-10-24T09:00:00Z)
autopilot: agents may no longer decide make_public; changed by NetBird peer "laptop.netbird.cloud" (at 2026-10-24T09:00:00Z)
autopilot: operation guide changed by the console on the loopback listener (no tailnet identity) (812 of 4000 characters) (at 2026-10-24T09:00:00Z)
```

The person is the tailnet user or node, or the NetBird peer, of a console
domain. Otherwise the line names how the console was reached: through an
external origin, on the loopback listener, from one of the host's own
addresses, or from another address on the console listener.

**Trust.** The Experiments page is guarded like the rest of the console, and
the guard is the one described under Approvals: a program that can send
console requests can turn Autopilot on or change switches. Agents are told
never to, and the host log records every change. Agent names are untrusted
text: bounded, quoted in events, inserted as text in the console and never
written to the host log. The operation guide reaches agents fenced in
`topic.autopilot`; Flats does not log or enforce it.

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
  env.FILES is capped at 1 GiB and 20,000 files and directories. The SQLite
  database file is capped at 1 GiB with a per-connection `max_page_count`, and
  a commit hook refuses commits while its write-ahead log is over 1 GiB (a
  reader holding an old snapshot keeps the log from restarting). Beyond those,
  the per-flat disk quota refuses new uploads and the console reports flats
  over quota. JS has no general filesystem or socket access; outbound `fetch` is host-mediated and requires exact operator-granted origins.
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

## Analytics

Each flat counts page requests per UTC day and per path; unique visitors are
not tracked. Query strings never enter analytics, since they may hold private
information. Path counts are buffered in memory per flat, bounded to 1000
day/path keys (further paths and paths over 1024 bytes count as `(other)`),
and flushed in transactional batches alongside the daily counters. Counters
follow slug renames and are deleted with the flat. The stats API returns up to
365 UTC calendar days (default 30); the console shows 7 or 30.

Analytics also reports each flat's **usage** per UTC day
([Metrics](spec/metrics.md#flat-usage)): every request that reaches the live
route, private or public, by status class, including the answers Flats gives
itself (rate limited, not deployed); response times; and, for server flats and
documents, the CPU time and peak memory of the workers serving the live route.
Previews are not usage. A request counts on the day its status is sent. A
`5xx` is an error; a request whose visitor left before the response was sent
counts as `closed`, not as an error. WebSocket connections and event streams
count with their status but are left out of response times. Websites, pages
and designs run inside the host process, so their CPU and memory are reported
as not measured separately, never as zero.

Counting happens in memory on the request path; the 30-second host sweep
writes every flat's day counts to `usagedays` in `flats.db` in one
transaction and merges them back if the write fails, so a slow database never
holds up a request. An idle worker with level memory and under 1 ms of new
CPU adds nothing to write. Reading stats never writes: it adds the counts still in memory to the
stored days. Usage rows are
keyed by the flat's uid, so they follow renames and are deleted with the flat;
a slug deleted and created again starts empty. Shutdown writes usage after the
workers have stopped, so their last CPU is kept. The stats API and the
console's Usage section read the same days as page views
([Response details](surfaces.md#response-details)).

### Metrics

`GET /metrics` answers in the Prometheus text exposition format (OpenMetrics
when the scraper asks for it). By default it is enabled only on the management
listener and includes per-flat series. Operators can disable it, select
`localhost`, `console` or `external` access points, and omit per-flat series
with `metrics.per_flat: false` through
[metrics configuration](configuration.md#prometheus-metrics), followed
by a restart. Missing settings use these defaults on existing installations too.
It is never served on a flat's route, a preview or a public provider.

`localhost` names the `host.console_addr` management listener, whose bind
address defaults to loopback. It does not restrict a deliberately configured
non-loopback bind, select only the DNS name localhost, or filter client source
addresses.
Managed console domains use `console`. Declared external-origin requests use
`external`, even when a reverse proxy connects to the management listener over
loopback. Exposure selection adds no authentication. Existing Host, Origin and
browser guards remain; admitted requests to a disabled or excluded endpoint
return 404. See [Request guards](surfaces.md#request-guards).

Metrics need no credential, so allow only trusted scrapers. Per-flat metrics
include the slugs of Private and unpublished flats by default. Host and
aggregate metrics remain available with per-flat metrics off. None of these
controls change Analytics, the stats API or usage collection. See
[Scraping metrics](networking.md#scraping-metrics).

A scrape reads only values the host keeps in memory: it never wakes a sleeping
document, starts a worker, reads SQLite, walks files or calls a network
provider. Traffic counters are current. Workers report their CPU and memory
every 15 seconds. Host-wide counts, pending approvals and the size of
`flats.db` are refreshed by each host sweep (normally every 30 seconds), whose
time is `flats_snapshot_timestamp_seconds`; disk usage is measured for every
flat about every 5 minutes. Route endpoint states are recomputed every 30
seconds, and once at start, from the providers' in-memory status by the
metrics endpoint itself. Counters start at zero when
the host starts; a renamed flat's series continue under the new slug and a
deleted flat's disappear.

Flats uses a private registry: only its own metrics and the Go runtime
(`go_*`) and process (`process_*`) figures of the host process are exposed,
never metrics that bundled libraries register. Labels carry only slugs, flat
types, provider IDs and fixed enums (plus the build strings of
`flats_build_info`), never paths, query strings, client addresses, user
agents, tailnet or NetBird identities, URLs or domain names. Per-flat series
carry `flat` (the slug) and `type` and are included by default. Explicit
`metrics.per_flat: false` omits them. This includes traffic, workers,
preview/check CPU and storage; host-wide board metrics remain available.

| Metric | Type | Extra labels |
| --- | --- | --- |
| `flats_build_info` | gauge (1) | `version`, `commit` |
| `flats_db_bytes` | gauge | |
| `flats_flats` | gauge | `type`, `visibility`, `running` |
| `flats_requests_total` | counter | `route` (`private`, `public`), `code` (`1xx` … `5xx`, `closed`) |
| `flats_rate_limited_total` | counter | |
| `flats_response_bytes_total` | counter | |
| `flats_open_streams` | gauge | |
| `flats_request_duration_seconds` | histogram | |
| `flats_worker_cpu_seconds_total` | counter | `role` (`live`, `preview`, `check`) |
| `flats_worker_memory_bytes` | gauge | (Linux) |
| `flats_worker_memory_sampled_timestamp_seconds` | gauge | (Linux) |
| `flats_worker_state` | gauge (1) | `state` |
| `flats_worker_unexpected_exits_total` | counter | |
| `flats_disk_bytes` | gauge | |
| `flats_disk_measured_timestamp_seconds` | gauge | |
| `flats_route_endpoints` | gauge (host) | `provider`, `state` |
| `flats_pending_approvals` | gauge (host) | |
| `flats_snapshot_timestamp_seconds` | gauge (host) | |
| `flats_management_requests_total` | counter (host) | `surface` (`api`, `console`, `mcp`, `metrics`, `other`), `code` |
| `flats_board_batches_total` | counter (host) | `result` (`applied`, `rejected`, `retry`) |
| `flats_board_connections` | gauge (host) | |
| `flats_board_import_seconds` | histogram (host) | |

`running` in `flats_flats` keeps its console meaning: a live document is
running even while its worker sleeps. `flats_worker_state` is the diagnostic
state of the worker serving the current version (`running`, `starting`,
`quiescing`, `sleeping`, `backoff`, `stopped`); `preview` and `check` CPU
belong to the flat's preview and activation-check workers, not to its usage.
The histogram buckets are 5 ms to 30 s, with an 11 s bound just above the
10 s runtime deadline; `closed` requests are not in it. An
endpoint state other than the known ones is reported as `other`, and so is a
flat type or visibility that is not a known word. Management requests are
counted by `1xx` … `5xx` only.
`flats_board_batches_total` counts people's edit batches on every board:
acknowledged, refused for good, or refused as transient for the canvas to
resend. `flats_board_import_seconds` times the outbox imports that recorded
revisions, with buckets from 5 ms to 10 s.

Worker CPU is user plus system time from the worker itself and, after it
exits, from its process state, so it includes workers that later stopped,
restarted or went to sleep. Worker memory is the private resident memory
(`RssAnon`) and is measured only on Linux; elsewhere the memory series are
absent and the stats API reports memory as `unsupported`. A sample is taken
every 15 seconds, so a short spike between samples can be missed. The host
process's own resident memory has a gap too: release builds are CGO-free, and
in that mode the process collector reports no resident memory on macOS; the Go
runtime series still show the host's Go memory.

## Surfaces

The HTTP API (`/api`), the CLI, MCP (`/mcp`) and the web console
(`/console/api`) all call the same core service. Their routes, status codes,
request guards and the console's screens are described in one place,
[HTTP API, CLI, MCP and console](surfaces.md). MCP tools and their guidance
are served by the host itself: the [runtime API v1 reference](runtime-api-v1.md),
the [content types contract](content-types.md) and the MCP `guide` tool.
