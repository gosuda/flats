# Flats documentation

Start with the [project README](../README.md) for what Flats is and a first
deploy.

## Running a host

| Page | Covers |
| --- | --- |
| [Releases and installation](release.md) | Installer, service, upgrades and backups; how maintainers cut a release |
| [Configuration and storage](configuration.md) | `config.json`, where each kind of data lives, backups, moving an older installation |
| [Running Flats in a container](container.md) | Image tags, network modes, hardening, upgrades |
| [Private and public access](networking.md) | Local, Tailscale, Tailscale Funnel, Portal, zrok, Cloudflare quick tunnels, NetBird and your own reverse proxy; which address a flat reports; visibility; upgrading provider grants |
| [Calling external APIs](external-api.md) | Outbound `fetch` grants for server flats and browser-side CORS/CSP |

## Building on Flats

| Page | Covers |
| --- | --- |
| [Connecting agents](agents.md) | Plugin, `flats connect`, direct MCP setup, the `guide` tool, llms.txt |
| [Runtime API v1](runtime-api-v1.md) | The complete server-flat contract: handler, FILES, DB, encoding, limits (generated) |
| [Content types](content-types.md) | Websites, HTML pages, Markdown documents, HTML design canvases and whiteboards (generated) |
| [Boards](content-types.md#boards) | Create and edit whiteboards with people and agents; structured operations, imports and revision history (generated) |
| [Server runtime types](agent/types/flats-runtime-v1.d.ts) | TypeScript declarations of runtime API v1, served as guide `topic.server.types` |
| [`agent/`](agent) | Source of the MCP `guide` topics and both generated references |

The two generated references come from [`agent/`](agent). Edit the topics
there and run `go test ./docs -run TestGeneratedReferences -update`.

## Product specs

What Flats must do and why, independent of the implementation; development
follows them. See [the spec index](spec/README.md) for how these pages work and
the full list.

| Page | Covers |
| --- | --- |
| [Content data models](spec/content-models.md) | The Version model (websites, pages) and the Revision model (documents, design); implemented |
| [Design content type](spec/design-type.md) | Canvas of HTML artboards with live editing and revisions; background in [research](research/claude-artifacts-design.md) |
| [Board content type](spec/board.md) | Infinite-canvas whiteboard people and agents edit together, with revisions; [technical design](spec/board-TECH.md) |

## Design and contracts

| Page | Covers |
| --- | --- |
| [Design](design.md) | Architecture and trust model: process layout, manifest, content types, exposure, approvals, secrets, handler ABI |
| [Lifecycle core contract](lifecycle-core-contract.md) | Drafts, versions, deploys, approvals, data and store migrations in the core |
| [Surfaces](surfaces.md) | HTTP API routes, CLI, MCP tools and the web console |
| [Revision store](revision-store.md) | Revisions, pins, quotas and rollback for documents, designs and boards |
| [Network provider lifecycle](network-provider-lifecycle.md) | Provider manager states and teardown |
| [Docs app internals](internal/docs-app.md) | Host integration of the collaborative Markdown editor |
| [Idle document worker internals](internal/docs-worker-idle.md) | How document workers sleep when idle and wake on demand: ownership, leases, preparation, clean sleep, tests |
| [WebSocket handshake](internal/websocket-handshake.md) | Why the RFC 6455 accept hash uses SHA-1 |

## Repository development

| Page | Covers |
| --- | --- |
| [Development protocol](development-protocol.md) | Independent design, implementation and testing skills; model review, correction and completion |
| [Development workflow specification](spec/dev-workflow.md) | Requirements for the repository development process |

## Verification

| Page | Covers |
| --- | --- |
| [Phase gates](gates.md) | CI baseline and recorded gate evidence |
| [Runtime discovery gate](gates/runtime-discovery.md) | A fresh agent authoring a server flat through MCP without source access |
| [Lifecycle regression gate](lifecycle-verification.md) | `scripts/lifecycle-gate.sh` cases and exit codes |
