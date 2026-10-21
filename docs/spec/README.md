# Product specs

These pages hold what Flats must do and why. Development follows them; a
change that needs to break one updates the page first.

- `<name>.md` is the product spec (PRD): principles and requirements,
  independent of any implementation. It may describe a target that is not
  implemented yet.
- `<name>-TECH.md` is the technical design of a spec whose feature has not
  shipped yet. When the feature ships, its durable design moves into the
  architecture docs (see below) and the TECH page is removed. Principles that
  span several features, such as the content data models, have no TECH page.
- Contracts and references elsewhere in `docs/` (`design.md`,
  `surfaces.md`, the generated references, `agent/`) describe how the
  implemented system behaves now.

Every spec starts with a status line: `implemented`, `partly implemented`, or
`planned` when none of its target is implemented yet. A spec whose target is
not fully implemented keeps a "Current state" section
listing the differences, and lists undecided points under "Open questions".
When a question is decided, its answer moves into the spec and leaves that
list.

| Spec | Status | Covers |
| --- | --- | --- |
| [Development workflow](dev-workflow.md) | implemented | Instruction-based design, implementation and testing stages with cross-model adversarial review, and a review evidence helper |
| [Idle document workers](docs-worker-idle.md) | implemented | Automatic idle sleep and demand-driven recovery of built-in document workers |
| [Content data models](content-models.md) | implemented | The Version model (websites, pages) and the Revision model (documents, designs) |
| [Design content type](design-type.md) | implemented | `type: "design"`: a canvas of HTML artboards people and agents edit live, with revisions |
| [Board content type](board.md) | partly implemented | `type: "board"`: an infinite-canvas whiteboard of native elements people and agents edit together live, with revisions |
| [Anonymous public editing for boards](board-public-edit.md) | partly implemented | A Public board level where anyone with the link can edit after typing a name, viewers named automatically, and the Share and Permissions controls |
| [Page content type](page-type.md) | implemented | `type: "page"`: one static HTML document with assets, on the website lifecycle |
| [Operator keys and console domains](console-settings.md) | implemented | One model for the keys Flats needs, and the console's Tailscale domains as an add/remove list |
| [Console listener address](console-address.md) | implemented | Rename the listener configuration key to `host.console_addr` with a behavior-preserving schema 5 migration |
| [NetBird peers](netbird-peers.md) | implemented | One NetBird peer per flat and console domain, the stored setup key, NetBird console domains over HTTP |
| [Agent self-update](agent-self-update.md) | implemented | Client compatibility checks and explicit updates of the CLI, connection skill and guidance |
| [Fast worker cold start](worker-cold-start.md) | implemented | Pre-started worker process and saved document images so workers and sleeping documents answer within 50 ms |
| [Metrics](metrics.md) | implemented | Configurable Prometheus exposure, optional per-flat series, and independent Analytics usage (requests, errors, response time, worker CPU and memory, disk) |

## Where the technical designs live

The technical designs of shipped features are folded into the architecture
docs:

| Feature | Architecture docs |
| --- | --- |
| Revision store shared by documents and designs: storage, import, quotas, pins, retention, rollback | [revision-store.md](../revision-store.md) |
| Document revisions in the docs worker: capture, activation, intents, host operations, the History panel | [internal/docs-app.md](../internal/docs-app.md#document-revisions) |
| Design and page types: type handling, bundles, serving, editing model | [design.md](../design.md) |
| Routes, MCP tools, CLI commands and console screens of every type | [surfaces.md](../surfaces.md) |
| Operator keys, console domains, configuration schema and migrations | [configuration.md](../configuration.md) |
| Host metrics and flat usage: request and worker accounting, attribution, storage, `/metrics` | [internal/metrics.md](../internal/metrics.md) |
| NetBird peers and the other network providers | [networking.md](../networking.md), [network-provider-lifecycle.md](../network-provider-lifecycle.md) |
| Client checks, CLI updates and skill sync | [release.md](../release.md#updating-a-client), [agents.md](../agents.md#keeping-clients-current) |
