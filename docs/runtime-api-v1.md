# Flats runtime API reference v1

Contract version: `1`. MCP resource: `flats://docs/runtime-api/v1`.
Read-only fallback tool: `get_runtime_reference` (no arguments).
This document ships inside the host binary; no checkout, source access, plugin,
or installed skill is needed. The tool returns `documentation_version`,
`uri`, `markdown`, `host_version` and the current `upload_limit_bytes`.
The version identifies the documented contract, separately from the host build.
Changes to this contract require a new reference version; corrections that do
not change behavior may revise v1. Record the document hash with gate evidence.

Non-website content types are described in `flats://docs/content-types/v1`.
Built-in app host internals (`runtimeGeneration`, `ws.setSendLimits` and `__flats_docsCodec`) are
described there and are explicitly outside runtime API v1.

## Deploy from an MCP client

Connect using Streamable HTTP to the operator's console URL plus `/mcp`
(loopback default `http://127.0.0.1:7878/mcp`). Discover tools, then read this
resource with `resources/read` or call `get_runtime_reference` with `{}`.
The management endpoint is privileged; use it only on a trusted host/tailnet.
See README for installation and host/client setup.

`save_version` accepts `slug`, `files: [{path, content, encoding}]`, optional
`message`, and `deploy` (default false). Send the **complete** build each time.
Use `utf8` for text or RFC 4648 `base64` for binary upload bytes. Saving creates
an absent flat and immutable version; it leaves live unchanged unless
`deploy: true`. A static walkthrough call is:

```json
{"slug":"hello","files":[{"path":"index.html","content":"<h1>Hello</h1>","encoding":"utf8"}],"deploy":true}
```

For a server, send `flats.json` with `{"kind":"server","entry":"server.js"}`
and an ES module `server.js`, then deploy the same way:

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

Set manifest `health: "/healthz"` for a side-effect-free check. Server entry
inference tries `server.js`, `index.js`, `main.wasm`, `server.wasm`. Static
entry defaults to `index.html`. Optional manifest fields: `name`, `kind`
(`static` default or `server`), `entry`, `spa`, `not_found`, `health` (default
`/`), `screenshot`. Unknown fields are rejected. `spa` serves the static entry
for unknown routes; `not_found` serves a static 404 file. Server handlers own
routing and must explicitly serve their UI/assets; uploading index.html with
a server does not automatically serve it.

`deploy {slug, version}` health-checks GET at the manifest health path: 2xx/3xx
within 15 seconds. Runtime request deadline is separately 10 seconds. Check
returned health and `private_url`, then fetch that URL and verify behavior.
`get_flat` reports live version/URLs/readiness; on tailnets wait while state is
`starting` (new certificates often take 1–2 minutes). Local mode uses loopback
`http://<slug>.localhost:<local-port>` instead of Tailscale. Remote/container
clients can connect to a configured host address; to verify a local-mode URL
from another network namespace connect to that address/local port with the
returned URL's Host header (do not reinterpret this as public exposure).
Failed uploads save nothing; failed deploys leave previous **code** live.
Candidate startup/health may already write live DB/FILES, and those writes
are not undone. Keep health/startup free of destructive mutations.
Use `get_logs` for validation/health/runtime errors and fix before retrying.

`open_preview {slug, version}` copies current live DB/FILES into isolated
preview storage. Preview writes do not reach live; redeploy closes previews,
and unused previews expire after the operator setting `preview_ttl_seconds`
(default **24 hours**). At most **5 open previews per flat** are allowed.
`rollback {slug}` restores previous code
and keeps current DB/FILES. `restore_data: true` restores the pre-deploy
**database only**, backing up current DB first; FILES is not restored. Ask the
user before discarding newer DB writes. Versions are not storage backups.

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
Deleting the flat deletes its data. Preview uses its own copy. Back up storage
separately; SQLite rollback snapshots do not include FILES.

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

## Handler, request and response

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
requests are not losslessly represented. No multipart/formData, clone or
request.arrayBuffer contract. Incoming body max **10 MiB** (413 if exceeded).

Return `new Response(body, {status, headers})`, `Response.json(value, init)`,
`Response.redirect(url, status = 302)`, a string, or `{status, headers, body}`.
Strings use UTF-8; Response string bodies default to text/plain;charset=UTF-8;
Response.json sets application/json. Plain-object response bodies that are
objects/arrays are JSON-serialized; use Response.json for that behavior with
a Response instance. Null body is empty; ArrayBuffer/typed-array response bodies
are binary and preserve bytes. Plain headers may include arrays for repeated
values (including Set-Cookie). Headers supports append/set/get/getSetCookie/
has/delete/forEach/entries/keys/values/iterator. These are minimal web helpers,
not full browser Fetch implementations; Response has text/json/ok/status,
statusText/headers/body, no streaming/clone/blob API.

HTTP status defaults to 200 (serialized zero also becomes 200); final statuses
must be 200–599. HEAD emits no body. Decoded response max **32 MiB**. Invalid
header names, CR/LF/NUL values and hop-by-hop/framing headers are discarded.
Unhandled exception/invalid response/memory exhaustion becomes generic 500;
deadline exceeded becomes 504; details go to runtime logs via get_logs. An
error does not undo already committed DB/FILES writes. console log/info/debug/
trace/warn/error is supported; lines truncate at 8 KiB, rate limit 20/s with
burst 100. Do not log secrets.

## Capabilities, limits and secrets

QuickJS on WebAssembly, default **10-second wall-clock deadline**, **64 MiB
wasm memory per VM**, with JS heap limited below that (48 MiB), four request
VMs plus a separate WebSocket VM when used. These are host runtime settings,
not arbitrary manifest fields. Standard JS language/JSON/typed arrays,
minimal URL/URLSearchParams, btoa/atob, console, Web Crypto randomness
`crypto.getRandomValues` (integer typed arrays, max 65,536 bytes per call)
and `crypto.randomUUID` are available. `TextEncoder`, `TextDecoder`,
`structuredClone`, `Blob`, `AbortController`, `fetch` and `WebAssembly` globals
are absent; the host UTF-8 encodes response strings. QuickJS may expose sandboxed
`os`/`std` helpers, but these have no supported Flats API contract. The capability
list is not an exhaustive inventory of engine globals.
No general Node.js process/fs/require,
subprocesses, host environment, general filesystem access, outbound fetch,
TCP/UDP/client WebSocket, browser DOM or Web Crypto subtle API contract.
Timers supplied by the engine are subject to the same deadline, not background
jobs. Use browser-side network APIs when appropriate.

Optional export `websocket: {open(ws, env), message(ws, data, env), close(ws, env)}`
accepts incoming WebSockets. Callbacks may be async, run serially in a separate
VM and share its module state. ws has id/url/headers/readyState; send(data)
converts to text; close(code=1000, reason=""); close callback gets closeCode/
closeReason. Incoming text/binary messages are delivered as strings (not a
lossless arbitrary binary API), max **1 MiB**. No extensions/subprotocols;
send queue 256 messages, closes on overflow. Redeploy closes connections.

Secrets are operator-managed strings injected as `env.NAME` at next deploy.
`env` is frozen; injected DB/FILES names are overwritten by host objects.
Operator secret names match `[A-Z_][A-Z0-9_]*`, at most 64 characters; values
are at most 64 KiB. Missing secret properties are undefined. `list_secrets {slug}`
shows names/update times only. Agent MCP cannot set/read secret values;
operator uses console or `flats secret set`. No inherited host credentials.
Secrets are encrypted on disk with local secret.key, but anyone with data-dir
access can recover them; trust your handler, which can itself return secrets.

WASI `.wasm` is a fresh preview1 command per request: stdin JSON
`{method,url,headers,body}`; stdout JSON `{status,headers,body}`, optional
`body_base64: true` for base64-encoded binary response. Environment receives
only the flat's configured secrets; clocks and CSPRNG are available separately.
There is no separate variable configuration or inherited host environment.
It has **no DB/FILES host ABI**,
filesystem mounts, outbound network, JS Web Crypto or WebSocket callbacks.
Use JavaScript for persistent DB/FILES APIs.

Upload size is operator-configurable (default **20 MiB**, current value in MCP
initialization and get_runtime_reference); max **20,000 upload files**,
no symlinks/special files/traversal. One wrapping directory is stripped; .git
and .DS_Store skipped; host-local save_version_from_dir also skips node_modules.
Only directly loopback clients can use save_version_from_dir (absolute path).
Remote agents should use inline save_version. Upload encoding is separate from
FILES text encoding. Static builds need no runtime and still work when the host
starts with `--runtime=false`; server deploys need runtime enabled.

## Visibility and approvals

Default private means tailnet-only in normal Tailscale mode; local test mode
uses loopback origins. Public exposure requires operator approval through
`set_visibility`: public-unlisted means anyone with URL can access (only hidden
from relay listings), public-listed also appears in relay listings. Neither
is authentication. Return approval_url verbatim to the user, poll get_approval;
no exposure changes until approved. Returning private is immediate.
`delete_flat` similarly requires operator approval and permanently removes data.
Repeat public_notice with any public_url. Never approve your own request or
imitate the console. Approvals govern supported tools, not arbitrary same-user
processes or clients that can imitate console headers. Restrict management
access and use trusted agents. Client auto-approval of MCP calls does not bypass
Flats public/delete approval and still permits code/data mutations by those tools.
