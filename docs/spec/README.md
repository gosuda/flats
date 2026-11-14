# Product specs

These pages hold what Flats must do and why. Development follows them; a
change that needs to break one updates the page first.

- `<name>.md` is the product spec (PRD): principles and requirements,
  independent of any implementation. It may describe a target that is not
  implemented yet.
- `<name>-technical-design.md` is the technical design of a spec whose feature has not
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
| [Config reload on SIGHUP](config-reload.md) | implemented | SIGHUP reloads config.json on a running host: live-applied keys take effect, restart-required keys wait for the next start |
| [Idle document workers](docs-worker-idle.md) | implemented | Automatic idle sleep and demand-driven recovery of built-in document workers |
| [Content data models](content-models.md) | implemented | The Version model (websites, pages) and the Revision model (documents, designs, boards, notebooks) |
| [Design content type](design-type.md) | implemented | `type: "design"`: a canvas of HTML artboards people and agents edit live, with revisions |
| [Board content type](board.md) | partly implemented | `type: "board"`: an infinite-canvas whiteboard of native elements people and agents edit together live, with revisions |
| [Notebook content type](notebook.md) | partly implemented | `type: "notebook"`: a tree of pages with editable blocks and inline databases, live collaboration, structured agent operations, and revisions |
| [Notebook technical design](notebook-technical-design.md) | partly implemented | N1 notebook architecture, item and concurrency model, storage, protocol, browser editor, agent interface, and validation plan |
| [Sheet content type](sheet.md) | planned | `type: "sheet"`: a workbook of worksheets with a cell grid, formulas that follow Excel, xlsx and CSV import and export, live collaboration, structured agent operations, and revisions |
| [Sheet technical design](sheet-technical-design.md) | planned | S1 sheet architecture: row and column identity, formulas that store ids, the deferred dirty mark and lazy calculation, storage, jobs, the xlsx reader and writer, protocol, agent interface, and validation plan |
| [Anonymous public editing for boards](board-public-edit.md) | partly implemented | A Public board level where anyone with the link can edit after typing a name, viewers named automatically, and the Share and Permissions controls |
| [Anonymous public editing for documents](docs-public-edit.md) | partly implemented | The board's "anyone can edit" Public level for documents: guest names, viewers, the guest projection, limits, guest revisions and the shared console controls |
| [Pasting images into documents](docs-image-paste.md) | implemented | Members paste an image into the document editor; it becomes a bundle file and a Markdown image at the caret |
| [Page content type](page-type.md) | implemented | `type: "page"`: one static HTML document with assets, on the website lifecycle |
| [Operator keys and console domains](console-settings.md) | implemented | One model for the keys Flats needs, and the console's Tailscale domains as an add/remove list |
| [Console listener address](console-address.md) | implemented | Rename the listener configuration key to `host.console_addr` with a behavior-preserving schema 5 migration |
| [NetBird peers](netbird-peers.md) | implemented | One NetBird peer per flat and console domain, the stored setup key, NetBird console domains over HTTP |
| [Agent self-update](agent-self-update.md) | implemented | Client compatibility checks and explicit updates of the CLI, connection skill and guidance |
| [Fast worker cold start](worker-cold-start.md) | implemented | Pre-started worker process and saved document images so workers and sleeping documents answer within 50 ms |
| [Guarded JavaScript engine](guarded-js-engine.md) | proposed | One engine type hides QuickJS from the runtime and enforces call, memory and failure rules: one call at a time, poison on fatal failure, mandatory deadlines and size limits |
| [Metrics](metrics.md) | implemented | Configurable Prometheus exposure, optional per-flat series, and independent Analytics usage (requests, errors, response time, worker CPU and memory, disk) |
| [Delete from the flat list](console-row-delete.md) | implemented | A Delete item in each flat's "…" menu that turns into a slide control in place; dragging its handle to the end deletes the flat |
| [Flats behind a reverse proxy](reverse-proxy-sites.md) | implemented | `proxy` and `proxy-public` providers that serve flats through an operator's reverse proxy, address selection by host default, and rename redirects on the arrival network |
| [Agent document routing](agent-document-routing.md) | implemented | `flats agent-setup docs`: an opt-in, previewable and undoable setup that makes Flats the document tool of Claude Code, Codex and Cursor, and blocks the competing Claude Docs route |
| [Autopilot](autopilot.md) | implemented | An opt-in mode in the new Settings › Experiments tab where agents approve or reject pending approvals for the operations the operator allows, with an operation guide for agents |
| [Windows support](windows-support.md) | partly implemented | Flats on Windows: a PowerShell installer, a per-user service, Windows file names and locks, and a release archive |
| [Secret key recovery](secret-key-recovery.md) | implemented | `flats secret backup-key`, `restore-key` and `reset`: a password-encrypted copy of `secret.key`, its restore against the sealed values, and a reset when both are lost |
| [Public instance mode](public-instance.md) | implemented | An opt-in Experiments mode in which every new flat is created Public on the default public network and no private address is handed out or served |
| [Host backup](host-backup.md) | implemented | `flats backup --out` copies a running or stopped host into a directory with a manifest, and `flats backup --verify` checks such a copy: databases as snapshots, write-once blobs in two passes, coherence of the outbox databases of documents, boards and notebooks with `flats.db`, and a self-check that fails to be run again |

## Where the technical designs live

The technical designs of shipped features are folded into the architecture
docs:

| Feature | Architecture docs |
| --- | --- |
| Revision store shared by documents, designs, boards and notebooks: storage, import, quotas, pins, retention, rollback | [revision-store.md](../revision-store.md) |
| Document revisions in the docs worker: capture, activation, intents, host operations, the History panel | [internal/docs-app.md](../internal/docs-app.md#document-revisions) |
| Design and page types: type handling, bundles, serving, editing model | [design.md](../design.md) |
| Routes, MCP tools, CLI commands and console screens of every type | [surfaces.md](../surfaces.md) |
| Operator keys, console domains, configuration schema and migrations | [configuration.md](../configuration.md) |
| Host metrics and flat usage: request and worker accounting, attribution, storage, `/metrics` | [internal/metrics.md](../internal/metrics.md) |
| Idle document workers: ownership, lease and lock rules, wake preparation, clean sleep, error contract, verification | [internal/docs-worker-idle.md](../internal/docs-worker-idle.md) |
| NetBird peers and the other network providers | [networking.md](../networking.md), [network-provider-lifecycle.md](../network-provider-lifecycle.md) |
| Reverse proxy providers: template and listener, Manager and app paths, address selection by host default, rename redirects on the arrival network | [networking.md](../networking.md#serving-flats-behind-a-reverse-proxy), [network-provider-lifecycle.md](../network-provider-lifecycle.md#reverse-proxy), [configuration.md](../configuration.md#reverse-proxy-keys) |
| Client checks, CLI updates and skill sync | [release.md](../release.md#updating-a-client), [agents.md](../agents.md#keeping-clients-current) |
| Agent document routing: the targets, the settings and block edits, the plan, confirmation, backups and undo | [agents.md](../agents.md#use-flats-for-documents) |
| Autopilot: operations, what an approval needs, agent decisions and their records, arrival networks, settings and host log, MCP on and off texts, the Experiments screen, storage | [design.md](../design.md#autopilot), [surfaces.md](../surfaces.md#autopilot-tools), [configuration.md](../configuration.md#autopilot-settings) |
| Public instance mode: creation policy, private-link gate and request ordering, MCP texts, console, storage and migration | [design.md](../design.md#public-instance-mode), [surfaces.md](../surfaces.md#public-instance-texts), [configuration.md](../configuration.md#public-instance-settings), [lifecycle-core-contract.md](../lifecycle-core-contract.md#migration) |
