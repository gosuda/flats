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
