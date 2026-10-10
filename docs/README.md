# Flats documentation

Start with the [project README](../README.md) for what Flats is and a first
deploy.

## Running a host

| Page | Covers |
| --- | --- |
| [Releases and installation](release.md) | Installer, service, upgrades and backups; how maintainers cut a release |
| [Configuration and storage](configuration.md) | `config.json`, where each kind of data lives, backups, moving an older installation |
| [Running Flats in a container](container.md) | Image tags, network modes, hardening, upgrades |
| [Private and public access](networking.md) | Local, Tailscale, Tailscale Funnel and Portal; visibility; upgrading provider grants |
| [Calling external APIs](external-api.md) | Outbound `fetch` grants for server flats and browser-side CORS/CSP |

## Building on Flats

| Page | Covers |
| --- | --- |
| [Connecting agents](agents.md) | Plugin, `flats connect`, direct MCP setup, the `guide` tool, llms.txt |
| [Runtime API v1](runtime-api-v1.md) | The complete server-flat contract: handler, FILES, DB, encoding, limits (generated) |
| [Content types](content-types.md) | Websites and Markdown documents (`type: "docs"`) (generated) |
| [`agent/`](agent) | Source of the MCP `guide` topics and both generated references |

The two generated references come from [`agent/`](agent). Edit the topics
there and run `go test ./docs -run TestGeneratedReferences -update`.

## Design and contracts

| Page | Covers |
| --- | --- |
| [Design](design.md) | Process layout, manifest, exposure, secrets, approvals, handler ABI, HTTP API |
| [Lifecycle core contract](lifecycle-core-contract.md) | Drafts, publication, versions, approvals and data in the core store |
| [Lifecycle transport](lifecycle-transport.md) | Console decisions and the API/CLI/MCP surfaces |
| [Lifecycle console](lifecycle-console.md) | Routes and screens of the web console |
| [Network provider lifecycle](network-provider-lifecycle.md) | Provider manager states and teardown |
| [Docs app internals](internal/docs-app.md) | Host integration of the collaborative Markdown editor |
| [WebSocket handshake](internal/websocket-handshake.md) | Why the RFC 6455 accept hash uses SHA-1 |

## Verification

| Page | Covers |
| --- | --- |
| [Phase gates](gates.md) | CI baseline and recorded gate evidence |
| [Runtime discovery gate](gates/runtime-discovery.md) | A fresh agent authoring a server flat through MCP without source access |
| [Lifecycle regression gate](lifecycle-verification.md) | `scripts/lifecycle-gate.sh` cases and exit codes |
