# Flats runtime API reference v1

Contract version: `1`. MCP resource: `flats://docs/runtime-api/v1`.
Read-only fallback tool: `get_runtime_reference` (no arguments).
This document ships inside the host binary; no checkout, source access, plugin,
or installed skill is needed. The tool returns `documentation_version`,
`uri`, `markdown`, `host_version` and the current `upload_limit_bytes`.
The version identifies the documented contract, separately from the host build.
Changes to this contract require a new reference version; corrections that do
not change behavior may revise v1. Record the document hash with gate evidence.

The sections below are also served one at a time by the read-only MCP tool
`guide` (for example `guide {"items":["topic.server","topic.server.db"]}`);
`topic.index` routes by task.

Non-website content types are described in `flats://docs/content-types/v1`.
Built-in app host internals (`runtimeGeneration`, `ws.setSendLimits` and
`__flats_docsCodec`) are explicitly outside runtime API v1.

## Connect from an MCP client

Connect using Streamable HTTP to the operator's console URL plus `/mcp`
(loopback default `http://127.0.0.1:7878/mcp`). Discover tools, then read this
resource with `resources/read`, call `get_runtime_reference` with `{}`, or call
`guide`. The management endpoint is privileged; use it only on a trusted
host/tailnet. See the README for installation and host/client setup.

## Static sites

A static flat serves the uploaded files as they are. Build first, then upload
the build output (`dist/`, `out/`, `build/`), never sources or `node_modules`.
Before writing pages, read `topic.design`: the page contract (title,
viewport, icon, both color schemes, phone width), design defaults and the
asset policy (bundle by default; CDN only with an exact version).

* The entry defaults to `index.html` at the bundle root. One wrapping
  directory such as `dist/` is stripped.
* `spa: true` serves the entry for unknown paths (client-side routers);
  `not_found: "404.html"` serves that file with status 404 (`topic.manifest`).
* Each flat is its own origin (`<slug>` host), so root-relative asset paths
  work. Static flats need no runtime and also work on hosts started with
  `--runtime=false`.

### Save

`save_draft` and `save_version` accept `slug`, `files: [{path, content,
encoding}]`, optional `expected_revision`, `message`, `git_sha`, `git_dirty`
and `deploy` (default false). Send the **complete** build every time: the
Draft is replaced, not patched. Use `utf8` for text and RFC 4648 `base64` for
binary files. A missing flat is created on the first save. A minimal call:

```json
{"slug":"hello","files":[{"path":"index.html","content":"<h1>Hello</h1>","encoding":"utf8"}],"deploy":true}
```

Saving never publishes: `deploy: true` only requests publish approval after
the save (`topic.approvals`). `expected_revision` is the Draft revision you
expect (0 when no Draft exists); a mismatch is refused as `conflict` and
overwrites nothing. Read `get_draft`, reconcile, then save again.

A successful save may return `warnings: [{path, message, fix}]`: a missing
title or viewport, an unpinned external script, a reference to a file that is
not in the upload, a large image, no favicon or screenshot. They never block
the save; fix them and save again (`topic.design` lists every check).

On the Flats host itself, `save_version_from_dir {slug, dir}` uploads an
absolute build directory (loopback callers only; it also skips
`node_modules`). The CLI `flats deploy <dir> --flat <slug> --save-only` saves
the Draft the same way; without `--save-only` it also requests publish
approval. Remote agents send files inline.

### Upload limits

Upload size is operator-configurable (default **20 MiB** uncompressed; the
current value is in the MCP instructions and `get_runtime_reference`); max
**20,000 upload files**, no symlinks, special files or paths outside the root.
`.git` and `.DS_Store` are skipped. Every validation problem is reported at
once with its path and a fix (`refusal.invalid`). Upload encoding is separate
from the text encoding of `env.FILES`.

## Manifest (`flats.json`)

`flats.json` is optional and sits at the bundle root. Unknown fields are
rejected; every problem is reported at once with a fix.

```json
{ "type": "flat", "name": "My blog", "kind": "static", "entry": "index.html",
  "spa": false, "not_found": "404.html", "health": "/", "screenshot": "screenshot.png" }
```

| Field | Meaning |
|---|---|
| `type` | `flat` (default, a website) or `docs` (Markdown documents, `topic.docs`) |
| `name` | display name |
| `kind` | `static` (default) or `server` (`topic.server`) |
| `entry` | static: default `index.html`; server: first of `server.js`, `index.js`, `main.wasm`, `server.wasm` |
| `spa` | static only: serve the entry for unknown paths |
| `not_found` | static only: file served with status 404 |
| `health` | URL path checked before activation, default `/`; use a side-effect-free path such as `/healthz` |
| `screenshot` | image path shown as the thumbnail in the operator's console |

An upload with no manifest `type`, no `kind`, no `index.html` and no server
entry, but with a Markdown entry candidate, is a `docs` upload. `type: "docs"`
has its own rules for `entry`, `name`, `kind`, `spa`, `not_found` and `health`
(`topic.docs`).

## Server flats

A server flat runs your JavaScript ES module (QuickJS on WebAssembly) or a
WASI `.wasm` command on the Flats host. Set `"kind": "server"` in
`flats.json` (`topic.manifest`); entry inference tries `server.js`,
`index.js`, `main.wasm`, `server.wasm`. Save and publish it like a static
site (`topic.static`), with every file in the same complete upload.

```js
export default {
  async fetch(request, env) {
    if (new URL(request.url).pathname === "/healthz") return new Response("ok");
    env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
    env.DB.exec("INSERT INTO hits VALUES (?)", [1]);
    const [{ n }] = env.DB.query("SELECT count(*) AS n FROM hits");
    env.FILES.put("last.txt", String(n));
    return Response.json({ hits: n, last: env.FILES.get("last.txt") });
  }
};
```

Set manifest `health: "/healthz"` for a side-effect-free check. Server handlers
own routing and must explicitly serve their UI/assets; uploading index.html with
a server does not automatically serve it. Pages the handler returns follow the
page contract in `topic.design`; saves check uploaded HTML documents for a
title, viewport and pinned external assets.

Related topics: `topic.server.db` (`env.DB` SQLite), `topic.server.files`
(`env.FILES` text storage), `topic.server.fetch` (outbound HTTP),
`topic.server.limits` (sandbox, globals and limits), `topic.env-secrets`
(`env.NAME` settings) and `topic.rollback-data` (what happens to live data).

### Activation and health

After approval, Flats health-checks GET at the manifest health path on an
isolated copy of the live DB/FILES: 2xx/3xx within 15 seconds. Runtime request
deadline is separately 10 seconds. A failed check or start leaves the previous
**code** live and is reported as `health_check_failed` or
`runtime_start_failed`. Starting the live runtime itself can write live data,
even when activation then fails, and those writes are not undone: keep
startup and health paths free of destructive mutations. Use `get_logs` for
validation, health and runtime errors, and `get_approval` `result_data` for
the reported data impact.

### Handler, request and response

Default ES module export `{ async fetch(request, env) { ... } }` (sync fetch
also works). Uploaded relative module imports are read-only; bundle any
libraries that need npm/Node resolution. Four request VMs by default may run
concurrently and are reused; module globals are neither durable nor shared
across all requests. Store state in DB/FILES. Do not block top-level loading.

`request.method`, absolute `request.url`, `request.headers` (lowercase string
keys plus get/has/forEach/entries/keys/iterator helpers), and `request.body`
(string or null) are provided. Duplicate header values are joined with comma
and space; Cookie uses semicolon and space. `await request.text()` returns
body or empty string; `await request.json()` parses it and throws for invalid
or empty JSON. Body is buffered text, not a ReadableStream: arbitrary binary
requests are not losslessly represented. `await request.arrayBuffer()` UTF-8
encodes this buffered text; it does not recover original binary request bytes.
No multipart/formData or clone API. Incoming body max **10 MiB** (413 if exceeded).

Return `new Response(body, {status, headers})`, `Response.json(value, init)`,
`Response.redirect(url, status = 302)`, a string, or `{status, headers, body}`.
Strings use UTF-8; Response string bodies default to text/plain;charset=UTF-8;
Response.json sets application/json. Plain-object response bodies that are
objects/arrays are JSON-serialized; use Response.json for that behavior with
a Response instance. Null body is empty; ArrayBuffer/typed-array response bodies
are binary and preserve bytes. Plain headers may include arrays for repeated
values (including Set-Cookie). Headers supports append/set/get/getSetCookie/
has/delete/forEach/entries/keys/values/iterator. These are minimal web helpers,
not full browser Fetch implementations; Response has text/json/arrayBuffer,
ok/status/statusText/headers/body/url/redirected, no streaming/clone/blob API.
Body reads are reusable and there is no bodyUsed property.

HTTP status defaults to 200 (serialized zero also becomes 200); final statuses
must be 200–599. HEAD emits no body. Decoded response max **32 MiB**. Invalid
header names, CR/LF/NUL values and hop-by-hop/framing headers are discarded.
Unhandled exception/invalid response/memory exhaustion becomes generic 500;
deadline exceeded becomes 504; details go to runtime logs via get_logs. An
error does not undo already committed DB/FILES writes. console log/info/debug/
trace/warn/error is supported; lines truncate at 8 KiB, rate limit 20/s with
burst 100. Do not log secrets.

### WebSocket

Optional export `websocket: {open(ws, env), message(ws, data, env), close(ws, env)}`
accepts incoming WebSockets. Callbacks may be async, run serially in a separate
VM and share its module state. ws has id/url/headers/readyState; send(data)
converts to text; close(code=1000, reason=""); close callback gets closeCode/
closeReason. Incoming text/binary messages are delivered as strings (not a
lossless arbitrary binary API), max **1 MiB**. No extensions/subprotocols;
send queue 256 messages, closes on overflow. Redeploy closes connections.

### WASI

WASI `.wasm` is a fresh preview1 command per request: stdin JSON
`{method,url,headers,body}`; stdout JSON `{status,headers,body}`, optional
`body_base64: true` for base64-encoded binary response. Environment receives
only the flat's configured environment variables and secrets; clocks and CSPRNG
are available separately. There is no inherited host environment.
It has **no DB/FILES host ABI**,
filesystem mounts, outbound network, JS Web Crypto or WebSocket callbacks.
Use JavaScript for persistent DB/FILES APIs.

## JavaScript env.FILES

A per-flat **local-disk string key/value store**, not S3-compatible storage.
It has no buckets, credentials, remote object URLs, metadata/options objects,
streaming, byte retrieval API, or automatic HTTP serving. All four methods
are **synchronous** (ordinary values, not Promises); errors throw JS exceptions.

| Method | Arguments and result | Missing behavior |
|---|---|---|
| `env.FILES.get(key)` | key coerced with `String(key)`; returns stored string | `null` for absent key/store or directory key |
| `env.FILES.put(key, data)` | key coerced to string; replaces entire value atomically; returns `undefined` | creates key and parent directories |
| `env.FILES.delete(key)` | key coerced to string; returns boolean | `false` if absent or not a regular file; `true` if removed |
| `env.FILES.list(prefix = "")` | string-coerced literal prefix; returns sorted array of full relative keys | `[]` if store absent/no matches |

`put` stores text: strings unchanged, null/undefined as empty string, other
nonbinary values via `String(data)` (objects become `[object Object]`, so use
`JSON.stringify`). ArrayBuffer/typed-array views are decoded as UTF-8 where
possible, otherwise converted to a character string; this is **not** a raw
binary round trip. Values travel through JSON and UTF-8 disk bytes. For binary,
store an application-encoded base64 ASCII string and decode it yourself:

```js
const bytes = new Uint8Array([0, 128, 255]);
const encoded = btoa(Array.from(bytes, b => String.fromCharCode(b)).join(""));
env.FILES.put("attachments/demo.b64", encoded);
const value = env.FILES.get("attachments/demo.b64");
const decoded = value === null ? null : Uint8Array.from(atob(value), c => c.charCodeAt(0));
const keys = env.FILES.list("attachments/");
const removed = env.FILES.delete("attachments/demo.b64");
```

Keys for get/put/delete: nonempty valid UTF-8, at most **512 UTF-8 bytes**,
relative slash-separated paths. No leading/trailing slash, empty segments,
`.` or `..` segments, backslash, U+0000–U+001F or U+007F. Each segment must
not start `.flats-tmp-` (reserved for atomic writes).
Key identity follows the host filesystem. Typical Linux ext4 is case-sensitive;
default macOS APFS is case-insensitive and Unicode-normalization-insensitive.
On such APFS volumes, keys differing only by case or NFC/NFD form name the same
value: put can overwrite it, and get/delete can resolve either spelling. List
returns the spelling stored by the filesystem (on default APFS, the first-created
spelling), and matches its prefix literally: get("CASE/a") can find "case/a"
while list("CASE/") is empty. For portable apps use canonical names such as
lowercase ASCII IDs; do not distinguish keys by case or normalization alone.
The 512-byte check is a whole-key validation limit, not a guarantee that the
host filesystem accepts the filename. Each path segment also obeys the host's
name limits: ext4 typically allows **255 UTF-8 bytes** per segment, while APFS
uses different Unicode name semantics (a multibyte segment can exceed 255 bytes).
Overlong segments can make get/put/delete throw filesystem errors even when the
whole key passes validation. Use short segments for portable keys.
A file cannot also be a parent directory: conflicting writes throw disk errors;
get/delete through a regular-file parent (e.g. `a/b` when `a` is a file) also
throw a filesystem "not a directory" error (underlying `ENOTDIR`) instead of
returning null/false; JS error text need not contain the errno name.

List uses a **literal string prefix**, not glob/path normalization: `notes`
matches `notes.txt` and `notes/a`; `notes/` matches only descendants. Its prefix
validation is intentionally looser: at most 512 bytes, no backslash or NUL;
empty and trailing slash are allowed. It returns regular files only, excludes
reserved temporary filenames, and stops after **10,000 keys** in filesystem
walk order **before sorting** the collected keys, with no cursor or truncation
flag. This need not select the lexicographically first 10,000 matching keys.
Traversal failures are skipped, so do not treat a list as a
transactional/complete snapshot of concurrently changing storage.

Each value is capped at **10 MiB (10,485,760 bytes)** of stored UTF-8 text;
base64 overhead counts. This explicit size limit does not guarantee a put/get
will fit in the **48 MiB JS heap**: strings and serialization copies compete
with other live allocations, so operations near 10 MiB can exhaust the heap
and produce a generic 500 even with a value within the limit.
Per-flat FILES total is **1 GiB (1,073,741,824 bytes)**;
overwriting charges the new size minus old size, deletion releases space.
Quota checks/mutations are serialized within a worker; individual put publishes
by rename. Multi-call sequences are not transactions and have no compare/swap.
There is no FILES TTL or versioning. Invalid keys/prefixes, value/quota overflow,
permissions, disk-full and incompatible file/directory paths throw; catch errors
and choose an HTTP response. Error wording can contain the key/OS details;
do not depend on exact text or expose it blindly. Failed atomic put keeps the
old value, but may leave newly created empty parent directories. get/put/delete
can all throw I/O errors; null/false apply only to the missing/directory cases
above, not to arbitrary failures to resolve a path.

Live DB/FILES belong to the flat, shared by its serving request VMs, and survive
successful redeploy, ordinary rollback and host restart on the same data dir.
Deleting the flat deletes its data. Preview uses its own copy. Activation
snapshots capture DB and FILES together (`topic.rollback-data`); versions are
code, not storage backups.

## JavaScript env.DB

Per-flat local SQLite. `env.DB.query(sql, ...params)` and
`env.DB.exec(sql, ...params)` are **synchronous** and throw on SQL, parameter,
storage or runtime-limit errors. SQL is string-coerced. Pass positional `?`
parameters as separate arguments or one array, e.g. `query("SELECT ? AS x", [7])`.
Do not interpolate untrusted input. No prepared-statement object or ORM API.

`query` returns an array of objects keyed by column names; no rows returns
`[]`. Alias columns uniquely. SQL NULL becomes null; numbers become JS numbers
(large integers can lose precision), text becomes strings, BLOB results become
text through JSON/UTF-8 rather than typed arrays. Use SQL/application encoding
for binary. `exec` returns `{changes, last_insert_id}` numbers from SQLite
RowsAffected/LastInsertId (not meaningful new insert IDs for every statement).

Parameters: null, string, number; booleans become 1/0; objects/arrays become
JSON text (a single array argument is treated as the parameter list, so nest
an array to store one). The JS-to-host JSON transport converts undefined array
entries to null and rejects BigInt/cyclic objects. The Go host then reserializes
compound values with `encoding/json`: object keys are sorted and `<`, `>` and
`&` are escaped as `\u003c`, `\u003e` and `\u0026` (U+2028/U+2029 are also
escaped). Stored text need not equal the original `JSON.stringify` text;
parse it as JSON instead of relying on textual identity. Parameters are not BLOBs.
`CREATE TABLE IF NOT EXISTS` is useful for idempotent setup.

One SQLite connection per JS VM keeps concurrent request transactions apart.
Use `exec("BEGIN")`, parameterized statements, `exec("COMMIT")`, and rollback
on errors within one handler invocation. Any transaction left open at the end
of a handler/callback is rolled back; never span requests with a transaction.
WAL, foreign keys and 5-second busy timeout are enabled. Query result cap is
**16 MiB** while accumulating serialized rows (the closing bracket is added
afterward; add LIMIT); SQL value/row length cap **32 MiB**;
SQLite allocation ceiling **256 MiB**. Runtime deadline also bounds queries.
ATTACH and VACUUM INTO and directory-changing pragmas are blocked. There is
no hard DB disk-size quota: DB growth is reported against host disk usage;
operator disk quota can reject uploads, but does not hard-cap runtime DB growth.
DB and FILES do not participate in a shared transaction.

## JavaScript outbound HTTP

Global `fetch(urlOrRequest, options)` returns a Promise for a buffered Response.
The supported options are `method`, `headers`, `body` and `redirect`; other
options, including `signal`, are rejected. Methods are GET, HEAD, POST, PUT,
PATCH, DELETE and OPTIONS. Bodies are strings, URLSearchParams, ArrayBuffers or
typed-array views; GET/HEAD cannot have a body. Response readers `text()`,
`json()` and `arrayBuffer()` are reusable; response metadata includes `status`,
`ok`, `url`, `redirected`, `headers` and an empty `statusText`. There are no
streams, cookie jar or automatic decompression. `Accept-Encoding`, host,
hop-by-hop, proxy and security headers cannot be supplied. Host I/O blocks its
VM while completing even though the result is a Promise; Promise.all does not
parallelize calls within a VM. Calls outside a request/WebSocket callback fail.

Server fetch defaults to denied. Only an operator can grant up to **32 exact
origins**, through the console or local `flats network set <flat> <origins...>`.
`flats network ls` and MCP `get_network {slug}` read desired grants; `flats
network clear <flat>` clears them. Only HTTP port 80 and HTTPS port 443 are
supported, with no wildcards, URL credentials or arbitrary ports. Private,
loopback, link-local, metadata and other non-public targets are blocked. All
resolved addresses must pass validation; connections use pinned addresses and
the original TLS hostname. Ambient proxy settings are ignored. Redirects
default to an error; `redirect: "manual"` returns the response without following.

Limits: **8 KiB URL**, **16 KiB supplied request headers**, **1 MiB request
body**, **16 KiB response headers** and **4 MiB response body**. Supplied
request headers allow at most **128 header names** and **128 values per name**;
empty value arrays are rejected. Header accounting
counts UTF-8 bytes(name) + bytes(value) + 4 for each value, repeating the name; response header
parsing is also bounded by the HTTP transport. Each call has a **5-second
deadline** within the existing handler deadline. At most **16 calls per
invocation**, **five concurrent calls per worker**, and **20 calls/second with
burst 20** are allowed. Budgets belong to each worker; live and preview
workers have independent allowances. Client disconnect cancels outbound I/O
without immediately terminating the VM; the handler may catch it and continues
under its deadline. Worker shutdown cancels outbound I/O. Host
errors omit request URLs, credentials and upstream bodies.

Grants are captured with env/secrets at approved deploy/redeploy, rollback,
data restoration, host restart and new preview. Health checks and live startup
share the capture. Automatic worker replacement keeps it. Changes and
revocations require activation to affect a running worker: redeploy after
clearing grants. Health checks/previews can call external services, so keep
health paths local and avoid external mutations during checks. WASI still has
no outbound network API. Browser fetch uses its own CORS/CSP/mixed-content
rules, independent of server grants. Never put server secrets in frontend code.

## Server runtime and limits

QuickJS on WebAssembly, default **10-second wall-clock deadline**, **64 MiB
wasm memory per VM**, with JS heap limited below that (48 MiB), four request
VMs plus a separate WebSocket VM when used. These are host runtime settings,
not arbitrary manifest fields. Standard JS language/JSON/typed arrays,
minimal URL/URLSearchParams, btoa/atob, console, Web Crypto randomness
`crypto.getRandomValues` (integer typed arrays, max 65,536 bytes per call)
and `crypto.randomUUID` are available. `TextEncoder`, `TextDecoder`,
`structuredClone`, `Blob`, `AbortController` and `WebAssembly` globals
are absent; the host UTF-8 encodes response strings. QuickJS may expose sandboxed
`os`/`std` helpers, but these have no supported Flats API contract. The capability
list is not an exhaustive inventory of engine globals.
No general Node.js process/fs/require,
subprocesses, host environment, general filesystem access,
TCP/UDP/client WebSocket, browser DOM or Web Crypto subtle API contract.
Timers supplied by the engine are subject to the same deadline, not background
jobs. Global `fetch` provides buffered HTTP(S) requests to exact operator-granted
origins, with public-address enforcement, pinned DNS, no proxy inheritance and
no automatic redirects. Requests default to denied (`topic.server.fetch`).
Browser network APIs retain real CORS, CSP and mixed-content protections;
server secrets never enter static frontend assets automatically.

Server deploys need the host runtime enabled; a host started with
`--runtime=false` refuses them as `runtime_unavailable`.

| Resource | Limit | Topic |
|---|---|---|
| Request body / decoded response | 10 MiB / 32 MiB | `topic.server` |
| WebSocket message | 1 MiB, 256 queued sends | `topic.server` |
| FILES value / per-flat total / list | 10 MiB / 1 GiB / 10,000 keys | `topic.server.files` |
| DB query result / value or row / allocation | 16 MiB / 32 MiB / 256 MiB | `topic.server.db` |
| Outbound fetch | 5 s, 16 calls per invocation, 4 MiB response | `topic.server.fetch` |
| Upload | operator setting (default 20 MiB), 20,000 files | `topic.static` |
| Environment value | 64 KiB, names up to 64 characters | `topic.env-secrets` |

Built-in host internals such as `runtimeGeneration`, `ws.setSendLimits` and
`__flats_docsCodec` serve the built-in docs app; they are explicitly outside
runtime API v1 and are not supported APIs for agent-authored server flats.

## Environment variables and secrets

Ordinary app environment variables and operator-managed secrets are strings
injected as `env.NAME` when the worker starts. `env` is frozen; `env.DB` and
`env.FILES` are reserved host bindings. New variable and secret writes require names matching
`[A-Z_][A-Z0-9_]*`, at most 64 characters; `DB`, `FILES`, `__PROTO__`, `PROTOTYPE` and `CONSTRUCTOR` are rejected for new writes.
New values are at most 64 KiB and must be valid UTF-8 without NUL; empty strings are supported.
A name cannot exist in both namespaces. Missing properties are undefined.
Historical secrets remain stored and keep their runtime behavior: JavaScript
`DB`/`FILES` host bindings take precedence over historical secrets with those
names, while WASI receives their stored strings. Other historical names and
values are preserved, including WASI rejecting NUL values as before. Replacing
a historical secret must pass the current write validation.

Manage ordinary values with MCP `list_env {slug}`, `set_env {slug, name, value}`
and `delete_env {slug, name}`, the CLI `flats env set <slug> <name> <value>`,
`flats env ls <slug>` and `flats env rm <slug> <name>`, or the console Settings
page. `list_env` returns ordinary names, values and update times. Ordinary
values are readable by management clients and stored without secret
encryption: use secrets for credentials. An ordinary variable cannot share a
secret's name; remove the old kind before changing kinds.

Changes leave running handlers and previews on their startup snapshot. Approved
deployment, redeployment, rollback or standalone data snapshot restoration captures current variables and secrets
when activation begins; the health check and live worker share that snapshot.
Settings are not pinned to the approval request or code version. Writes after
capture apply at the next activation. New previews and a Flats host restart load
current settings; automatic worker restarts reuse the captured snapshot. Redeploy
through the existing approval flow to apply changes. Neither
ordinary variables nor secrets enter frontend bundles, static files or build
substitution. There is no inherited host environment.

`list_secrets {slug}` shows names/update times only. Agent MCP cannot set/read
secret values; operators use the console or `flats secret set`. Secrets are
encrypted on disk with local secret.key, but anyone with data-dir access can
recover them; trust your handler, which can itself return secrets. Never log
secret values or put credentials in ordinary variables.

To apply changed settings deliberately, redeploy the live version with
`deploy {slug, version: <live>}` and wait for approval (`topic.approvals`), or
ask the user to use **Redeploy (apply environment)** in the console.

## Preview and verify

### Preview

Before previewing, read the `warnings` of the save result (`topic.design`);
they are cheap static checks, not a render.

`open_preview {slug, version: 0}` serves the current Private Draft; a positive
`version` previews that published version. Live is unchanged. Server and docs
flats get an isolated copy of the live DB/FILES: preview writes never reach
live. New previews capture current environment variables, secrets and network
grants. At most **5 open previews per flat**; a preview closes on the next
activation of that flat, or after the operator setting `preview_ttl_seconds`
(default **24 hours**) without visits. Previews are Private (loopback or the
existing tailnet ACL) and never use Public providers.

### Readiness

A URL can exist before it answers. `get_flat` reports `private_state` and each
preview's `state`: `ready`, or `starting` while a tailnet node joins or waits
for its HTTPS certificate (often 1-2 minutes for a new flat or preview). Check
`get_flat` again before fetching; a TLS error while `starting` is not a failed
deploy. Local mode serves plain HTTP at `<slug>.localhost:<local-port>`. To
verify a local-mode URL from another network namespace, connect to the host's
address and port with the returned URL's Host header; that is not public
exposure.

Give the user the URLs that `get_flat` and `open_preview` return; never build
one from a pattern. A Local link (`*.localhost`) opens on the machine where it
is clicked, so it reaches the flat only from the Flats host itself. On a host
whose private backend is Tailscale, new flats and Draft previews use the
tailnet address, which other allowed devices can open.

For a page, look at the Draft preview once at desktop width and at about
400px, in light and dark, fix what you see in one pass, then publish
(`topic.design`). Do not repeat the look in a loop.

### Verify

Report a version as live only after `get_approval` says `approved`, `get_flat`
shows it as `live_version` with a ready endpoint, and you fetched the URL and
checked the behavior. A save result or a pending request never means live.
Report the verified version, address and observed health. Use
`get_logs {slug, kind?, after?, limit?}` for validation, health, runtime and
approval events (page with `next_after`). For Public flats, repeat the public
notice and recheck shared links over time.

## Rollback and live data

### Live data

Server and docs flats keep live data: `env.DB` and `env.FILES` for server
flats, the collaborative document state for docs flats. Live data belongs to
the flat and survives successful redeploy, ordinary rollback and host restart.
Deleting the flat deletes its data. Previews work on isolated copies. Versions
are code, not data backups.

Before activating a version of a flat that has data, Flats snapshots the live
data: the database and FILES together. It keeps a bounded number of snapshots
per flat (10, plus 3 from failed deploys).

### Rollback

`rollback {slug, version?}` requests approval to activate an earlier
published version (default: the one live before the current one). Code only
by default: live DB and FILES stay as they are.

`restore_data: true` also replaces the live data with the snapshot taken
before the current live version was deployed. Current data is first backed up
as a new snapshot, and the target version is health-checked on a copy of the
snapshot before anything changes. Snapshots from current hosts capture DB and
FILES, and restoring them replaces both; legacy DB-only snapshots from older
releases restore the database and preserve current FILES. Writes made since
the snapshot stop being live. Nothing is restored before operator approval.
Ask the user before requesting it, then poll `get_approval` and report its
`result_data` (`data_impact`, `health_data`, `live_data`).

A rollback to the version that is already live, or with nothing live yet, is
refused (`refusal.conflict`, `refusal.not_deployed`). Restoring a named
snapshot without changing code is an operator console action.

For docs flats, a code rollback merges the target version's Markdown into the
live text: independent human edits are kept and overlapping lines prefer the
target (`topic.docs`).

## Approvals, visibility and exposure

Agents never decide publication, exposure or deletion. These requests return
`status: pending_approval` with `approval_id` and `approval_url`. What they
request changes nothing until the operator approves it in the Flats console:

* `publish`, and the publish request made by `save_draft`/`save_version` with
  `deploy: true` (the save itself updates the Private Draft immediately; only
  the publication waits, and the Draft stays saved if that request fails);
* `deploy {version}` (activation or redeploy of a published version);
* `rollback`, with or without `restore_data`;
* `set_visibility` in **both** directions, private to public and public to
  private;
* `delete_flat`, which permanently removes the flat, its versions and data.

Rules:

* Give the user `approval_url` exactly as returned. Poll
  `get_approval {id}` until `approved`, `rejected` or `failed`; while it is
  `pending` (or `applying`), say you are waiting for operator approval.
* Never approve your own request, call console routes or send console
  headers. MCP has no approval or provider-grant tool. Console routes have
  same-origin CSRF checks but no separate authentication, and a local process
  can send those headers: run only trusted agents on the host. Client
  auto-approval of MCP calls does not bypass Flats approvals.
* Ask the user before requesting `set_visibility`, `delete_flat` or a rollback
  with `restore_data`. `set_visibility` and `delete_flat` accept a `reason`
  that the operator sees; `rollback` has none.
* An approval freezes what it approves (Draft revision and hash, version,
  snapshot, visibility, providers). If that changes before the decision, it
  fails as `stale_approval`; request again. An identical pending request is
  reused rather than duplicated.
* After a decision, `get_approval` `result_data` reports `failure_code` (a
  `refusal.<category>` name, or `apply_failed`), `data_impact` (`none`,
  `runtime_start`, `restore_data` or `unknown`), `health_data` and
  `live_data`. Report them as given.

### Visibility

* Visibility is only `private` or `public`. New flats are Unpublished and
  Private.
* **Private** means Local loopback on the host (plain HTTP at
  `<slug>.localhost:<port>`) or Tailscale for people and devices the existing
  tailnet ACL allows. It is not owner-only access.
* **Public** means anyone on the internet, through a provider the operator
  permitted for this flat: Portal relay or Tailscale Funnel (Funnel visitors
  do not need Tailscale; Tailscale Serve is tailnet-only and is not Public).
  Public is NOT access control: anyone can open a ready Public route.
* An Unpublished flat cannot become Public. Requesting the current
  visibility changes nothing.
* Going Private also waits for approval. Until the approved change applies,
  the public route stays reachable.
* Local is always permitted. Nonlocal providers (`tailscale`,
  `tailscale-funnel`, `portal`) need host configuration and per-flat
  operator permission; agents cannot grant either. One standing exception:
  on a host whose private backend is Tailscale (`network.private_backend:
  tailscale`), a new flat, including one an agent creates, starts with
  `tailscale` permitted, so its private and preview links use the tailnet.
  That grant never covers Funnel or Portal, and the operator can turn it off
  per flat. Read the actual `providers` from `get_flat` instead of assuming. Connecting Tailscale
  grants no Funnel permission, and configuring a provider grants no publish or
  visibility consent. A provider failure never authorizes switching providers.
* Never infer visibility, publication or readiness from a URL or domain.
  `get_flat` reports `publication`, `live_version`, `visibility`, `providers`,
  `connection_state` and `endpoints` as separate fields. Repeat
  `public_notice` whenever you report a public URL.
* `portal_hidden` reports whether a public flat is kept out of Portal relay
  listings (`portal_listing` is the flat's own choice: `default`, `hidden` or
  `listed`). Only the operator changes it, in the console. Hidden is not
  access control: never describe a hidden public flat as private.
