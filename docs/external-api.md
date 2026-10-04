# Calling external APIs

Browser code uses the browser's ordinary `fetch`. JavaScript server flats use
Flats' bounded, host-mediated `fetch`. WASI has no outbound network API.
These are separate capabilities: inbound sharing/provider permissions do not
grant server egress, and server origin grants do not change browser protections.

## Server setup

New flats deny outbound requests. An operator grants up to 32 exact origins
in the flat's console settings or on the Flats host:

```sh
flats network set hello https://api.example.com
flats network ls hello
flats env set hello API_BASE https://api.example.com
flats secret set hello API_TOKEN
```

`network set` replaces the desired origin list; `flats network clear hello`
clears it. Only HTTP on port 80 and HTTPS on port 443 are supported. Origins
have no path beyond an optional trailing `/`, query, fragment or user information. Wildcards and arbitrary
ports are unsupported. Agents may read grants with MCP `get_network`, but
cannot grant network access. Ordinary API clients cannot set grants.

Grants are captured alongside env/secrets when activation begins: approved
deploy/redeploy, rollback, data snapshot restoration, host restart and new
previews use the then-current settings. Health checks and live execution use
the same capture. Automatic worker replacement retains that capture. Saved
changes, including revocations, do not change a running worker: redeploy after
`network clear` to revoke its access. The grants are app settings, not code
version history. Preview and deployment health checks can make external calls;
keep `/healthz` local and free of external side effects.

[The server example](../examples/external-api/server.js) calls the configured
origin with a server-only token and returns one public field. Deploy its
directory using the checked-in `flats.json`, which selects `server.js` and
`/healthz` (the default `/` route returns 404). For example, `flats deploy
examples/external-api --flat hello`. Configure `API_BASE` and enter your own token through the operator
secret prompt. `api.example.com` is a placeholder, not a configured service.
Do not accept arbitrary visitor URLs or forward incoming authorization headers.
Grant only services you trust to receive any credentials your handler sends.
Use HTTPS for credentials: HTTP grants do not encrypt request headers or bodies.

## JavaScript fetch subset

`await fetch(url, options)` or `await fetch(request, options)` accepts an
absolute HTTP(S) URL, headers and methods `GET`, `HEAD`, `POST`, `PUT`,
`PATCH`, `DELETE` and `OPTIONS`. Bodies may be strings, `URLSearchParams`,
`ArrayBuffer` or typed-array views; GET/HEAD bodies are rejected. Responses expose
`status`, `statusText`, `ok`, `url`, `headers`, `text()`, `json()` and
`arrayBuffer()`. Request and response bodies are buffered; response body reads
are reusable. `statusText` is empty. The call returns a Promise, but host I/O
blocks its VM while it completes; `Promise.all` does not provide parallel
requests within one VM. This is a small runtime API, not a complete browser Fetch API.
Only `method`, `headers`, `body` and `redirect` options are supported.
No streaming, cookie jar, `AbortController` signal, raw TCP, UDP or client
WebSocket API is provided. Redirect statuses 301, 302, 303, 307 and 308 are
rejected by default; `redirect: "manual"` returns them without following.
Statuses 300 and 304 are returned normally. Browser CORS is
not applied to server requests. Calls are permitted only inside a request or
WebSocket callback, not during module initialization.

Host enforcement checks the exact permitted origin and resolves its addresses.
Private, loopback, link-local, metadata, multicast and other non-public IP
targets are rejected, including IP literals and mixed public/private DNS
answers. The connection is pinned to validated addresses while retaining the
original hostname for TLS verification, so a later DNS answer cannot redirect
the connection. Host proxy environment variables are ignored. Redirects do
not create additional connections.

Limits: URL 8 KiB, request headers 16 KiB, request body 1 MiB, response headers
16 KiB and response body 4 MiB. Supplied request headers allow at most
128 names and 128 values per name; empty value arrays are rejected. Each supplied header value counts UTF-8 bytes(name) + bytes(value) + 4,
including the repeated name for each value; response header parsing is also bounded
by the host HTTP transport. Hop-by-hop/host/proxy/security headers cannot be
supplied. `Accept-Encoding` is rejected and automatic decompression is disabled;
a compressed upstream response is returned as compressed bytes. Each call has a 5-second deadline within the
handler's existing 10-second deadline. Client disconnect cancels outbound I/O,
which the handler may catch; it does not immediately terminate the VM. The VM
remains bounded by its handler deadline. Worker shutdown also cancels I/O.
There are at most 16 calls per invocation, five concurrent calls per worker,
and a worker rate limit of 20 calls/second with burst 20. Each live or preview
worker has its own budget; multiple previews multiply the aggregate allowance.
Errors describe the
failure class without including URLs, authorization headers or upstream
response bodies. Application code can still disclose values it reads: do not
log credentials or return raw upstream errors.

## Browser setup

[The browser example](../examples/external-api/browser.html) fetches public
JSON from a user-entered endpoint without sending cookies. Copy that HTML into
a separate directory as `index.html` and deploy it as a default static flat, or
adapt the form into a server flat's UI. Do not copy the server example's
`flats.json` into the static directory. It does
not read server env/secrets. Every value embedded in HTML, JavaScript, browser
requests or responses is visible to the visitor; keep API tokens in the server
handler and expose a narrowly scoped same-origin endpoint when needed.

For a cross-origin browser request, the API must return a suitable
`Access-Control-Allow-Origin` header and handle any required preflight. A CSP
`connect-src` policy can restrict destinations. An HTTPS page should call an
HTTPS API; browser mixed-content protections still apply. Do not use
`mode: "no-cors"` as a workaround: an opaque response cannot provide readable
JSON. See the browser [Fetch/CORS guide](https://developer.mozilla.org/en-US/docs/Web/API/Fetch_API/Using_Fetch)
and [CSP connect-src reference](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Content-Security-Policy/connect-src).
Flats never disables browser CORS, CSP or mixed-content enforcement.

The example's browser test uses disposable local fixtures and actual Chromium:
it verifies a CORS-enabled response is readable, a response without CORS is
rejected, and `connect-src 'self'` blocks the same cross-origin fixture. It
does not contact an external service or assert browser-specific mixed-content
behavior from a loopback test.
