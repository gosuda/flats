## Server runtime and limits

QuickJS on WebAssembly, default **10-second wall-clock deadline**, **64 MiB
wasm memory per VM**, with JS heap limited below that (48 MiB), four request
VMs plus a separate WebSocket VM when used. These are host runtime settings,
not arbitrary manifest fields. Standard JS language/JSON/typed arrays,
minimal URL/URLSearchParams, btoa/atob, console, Web Crypto randomness
`crypto.getRandomValues` (integer typed arrays, max 65,536 bytes per call)
and `crypto.randomUUID` are available. `TextEncoder`, `TextDecoder`,
`structuredClone`, `Blob`, `AbortController` and `WebAssembly` globals
are absent; the host UTF-8 encodes response strings. QuickJS exposes sandboxed
`os`/`std` helpers, but these have no supported Flats API contract. The capability
list is not an exhaustive inventory of engine globals.
No general Node.js process/fs/require,
subprocesses, host environment, general filesystem access,
TCP/UDP/client WebSocket, browser DOM or Web Crypto subtle API contract.
`setTimeout`, `setInterval`, their clear functions and `queueMicrotask` come
from the engine: they fire only while the current invocation awaits, within
its deadline, and are not background jobs. Extra timer arguments are not
passed to the callback; clear pending timers before returning. Other engine
extras such as `performance` and `navigator` have no contract;
`topic.server.types` lists them. Global `fetch` provides buffered HTTP(S)
requests to exact operator-granted origins, with public-address enforcement, pinned DNS, no proxy inheritance and
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
