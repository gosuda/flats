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
