# Lifecycle core contract

This is the core/store contract for a flat's lifecycle: Drafts, versions,
activation, approvals, data effects, store migration and the network contract
core expects. The product rules are in [content-models.md](spec/content-models.md),
the trust model in [design.md](design.md#approvals), the transports in
[surfaces.md](surfaces.md), and the history of documents and designs in the
[revision store](revision-store.md).

## Content and publication

Each save of a website or page statically validates the bundle and stores one
immutable Draft revision under `flats/<slug>/draft-revs/<revision>`. `drafts`
points at the current revision; saves do not allocate a version number or run
server code. `SaveMeta.CheckRevision` and `ExpectedRevision` reject competing
stale saves. Draft saves preserve Current, visibility, permissions and live
data. A save whose bytes match the latest saved version is clean; creating a
version from identical bytes is rejected with `ErrUnchangedContent`.

For the Version-model types (websites and pages), creating a version and
making it live are separate approved steps. Only an approved version creation
(`CreateVersion`, approval action `create_version`) creates `vN`, under
`flats/<slug>/versions/<N>`: the version row and the Draft's new base commit
in one transaction (`store.CommitVersion`), without changing the live pointer,
running server code or recording a deployment. An approved activation of a
saved version health-checks it on a copy of the data, snapshots the data,
starts the version on live data, and only then commits the live pointer and
deployment history in one transaction (`store.SetLive`). A failed activation
keeps `vN` saved and the previous version live, but a failure after the start
can leave live DB/FILES already written (see
[Data and result DTOs](#data-and-result-dtos)). Saved versions can be
activated or rolled back without a new number. A rollback keeps live data
unless it was requested with `restore_data`.

Revision-model saves take another path. A changed document save, a document
revision rollback that needs a new bundle, and a design's shell when the
design is created go live through `publishRevision` with kind `save`: it
allocates the next internal version number and commits the version row, live
pointer and deployment together (`store.CommitPublished`), with no approval and
no approval ID. Those internal versions back the running instance only; the
user-facing history of these types is in the [revision store](revision-store.md),
including the document rollback flow.

A flat is Published once one of its versions has been live, and that status
persists through connection failures. Visibility is independently
`private|public`; legacy listed/unlisted inputs canonicalize to `public`.
Unpublished flats cannot become Public.

Previews (`OpenPreview`; number 0 targets the Draft) are Private only: Local,
plus Tailscale when the flat holds that grant. Server previews use isolated
data in `flats/<slug>/previews/<host>`. At most 5 previews are open per flat.
Current and approval-pinned Draft revisions, and open Draft-preview targets,
survive Draft revision pruning. Valid persisted previews resume after restart;
expired previews are closed, and unavailable historical targets get an error
event rather than being rebound to another version. Successful activation
closes open previews.

## Callable service surface

```go
SaveDraft(ctx context.Context, slug string, files []bundle.File, meta SaveMeta, expectedRevision int, via Via) (store.Version, error)
SaveVersion(ctx context.Context, slug string, files []bundle.File, meta SaveMeta, via Via) (store.Version, error)
GetDraft(ctx context.Context, slug string) (store.Draft, error)
CreateVersion(ctx context.Context, slug string, revision int, hash string, via Via) (ActionResult, error)
RequestCreateVersion(ctx context.Context, slug string, revision int, hash string, via Via) (ActionResult, error)
DeleteVersion(ctx context.Context, slug string, number int, via Via) (ActionResult, error)
RequestDeleteVersion(ctx context.Context, slug string, number int, via Via) (ActionResult, error)
SetVersionQuotaOverride(ctx context.Context, slug string, quota *int) error
Deploy(ctx context.Context, slug string, number int, via Via) (DeployResult, error)
RollbackWithData(ctx context.Context, slug string, number int, restoreData bool, via Via) (DeployResult, error)
RestoreSnapshot(ctx context.Context, slug string, snapshot string, via Via) (DeployResult, error)
OpenPreview(ctx context.Context, slug string, number int) (PreviewView, error)
SetVisibility(ctx context.Context, slug string, visibility store.Visibility, via Via, reason string) (ActionResult, error)
Delete(ctx context.Context, slug string, via Via, reason string) (ActionResult, error)
SetProviderPermission(ctx context.Context, slug string, provider string, permitted bool, via Via) error
Decide(ctx context.Context, approvalID string, approve bool) (store.Approval, error)

// Revision model
SaveDocument(ctx context.Context, slug, path, markdown, title string, meta SaveMeta, via Via) (store.Version, error)
UpdateDocument(ctx context.Context, slug, doc string, ops []DocumentEditOp, ifHash *string, via Via) (DocumentEdit, error)
CreateDesign(ctx context.Context, slug, name string, files []DesignFile, message string, via Via) (FlatView, DesignWriteResult, error)
WriteDesign(ctx context.Context, slug string, w DesignWrite, via Via) (DesignWriteResult, error)
ListRevisions(ctx context.Context, slug string, before, limit int) (design.RevisionPage, error)
GetRevision(ctx context.Context, slug string, n int) (store.Revision, []store.RevisionFile, error)
PinRevision(ctx context.Context, slug string, n int, pinned bool) error
RollbackRevision(ctx context.Context, slug string, n int, label string, via Via) (int, error)
SetRevisionQuotas(ctx context.Context, slug string, revisions, pins *int) error
```

`SaveDraft` always checks the supplied revision; `SaveVersion` is the
unchecked save that also carries docs bundles. Internal save results use a
compatibility `store.Version`, while public transports expose only Draft
metadata under `draft` with revision/hash. Lists of versions contain only
numbered versions. `CreateVersion` returns an `ActionResult` with
`status:"pending_approval"`; `Deploy`, rollback and snapshot restore return
`*PendingApproval`, which embeds that result and wraps `ErrPending`. `Deploy`
requires a positive version number of an existing saved version.
`CreateVersion` revision zero selects Current; an explicit revision/hash must
match it. No `Via`, including Console, skips approval for version creation,
activation, rollback, data restore or visibility. `Delete` and `DeleteVersion`
apply directly for `ViaConsole` and request approval otherwise; a live version
cannot be deleted. `Publish` and `RequestPublish` remain only as internal
aliases of `CreateVersion`.

Documents and designs refuse `GetDraft`, `CreateVersion`, `DeleteVersion`,
`SetVersionQuotaOverride`, `Deploy`, rollback, `ListVersions` and
`OpenPreview` (`invalid`, naming the type's own tools).
Docs bundles are saved through `SaveVersion`; a design manifest is refused on
every bundle save. The revision calls refuse websites and pages
(`no_revisions`).

Flat views add `publication`, `draft`, permitted provider IDs,
`connection_state` and `endpoints`. Endpoint DTOs carry JSON `provider`, `url`,
`state`, `detail`, `configured`, `permitted`, `ready`, `audience`, and `host`.
URL existence is not readiness or proof of Public. The manager supplies fresh
concrete status through the observer contract below.

## Operator authority and frozen approvals

### Default grants and decisions

A new flat inherits the host's default provider grants in the transaction that
creates it, with a `provider` event per grant: the private default
(`network.default_private`, or Tailscale when that is unset and
`network.private_backend` is `tailscale`) and the public default
(`network.default_public`), each only when the host permits it. A public grant
is only a permission; the flat stays Private until an approved visibility
change. Flats are created by agents (MCP, CLI or API), never from the console.

`Decide` and per-flat provider-grant mutations are console operations:
transports expose them only on console routes, and provider grants also
require `ViaConsole`. Decisions persist `decided_by` and `authorized_at`
before execution. `WithActor(ctx, actor)` supplies the audit actor (the API
passes the console domain's identity when one is known); otherwise it is
`console`. What a console request does and does not prove is described in
[design.md](design.md#approvals).

### Frozen approvals

Every approval-gated request freezes its candidate, the live version, the
visibility and the sorted per-flat permissions plus the manager's stable
host/configuration policy token (`ExposurePolicy`). Transient connection
readiness is excluded from the policy token. The decision rechecks each
frozen value:

- `create_version` also freezes the latest saved version and the Current Draft
  revision/hash; retaining old bytes does not authorize stale version
  creation. A Draft whose bytes match the latest saved version is refused with
  `ErrUnchangedContent`. A full version quota refuses it at request and at
  approval; the disk quota is checked at approval.
- `activate` freezes the version number and hash; candidate content hashes
  are checked from disk.
- `rollback` freezes the target hash and explicit `restore_data`, with the
  snapshot name and hash it would restore; `restore_data` (snapshot restore)
  freezes snapshot identity/hash.
- `delete_version` freezes the version hash.
- `set_visibility` freezes the source visibility and provider permissions,
  and for a website or page also the live version. A document or design
  freezes the flat's ID instead of the live version: the approval covers the
  flat, not one revision ([content models](spec/content-models.md), Revision
  model rule 2). A changed save of a document moves its internal live
  version, and so does a revision rollback whose target has other
  non-document files or another set of documents
  ([revision store](revision-store.md)); neither makes a pending visibility
  approval stale, and approving it exposes the content running when it
  applies. The flat ID is random, set at creation and kept by renames, so an
  approval never applies to another flat created later under a deleted slug.
  The request reads the flat and records the approval under the flat's lock.
  Requests stored before flat IDs froze a document's live version and keep
  that rule; an identical new request on the same slug reuses one while that
  version still runs, never one a rename moved to another slug.

Pending requests deduplicate across caller surfaces by canonical
action/params. Decisions serialize by approval ID and atomically claim pending
requests. Repeated completed approvals return their result. A restart resumes
only persisted validated applying decisions. Deployment receipts prevent
repeated version/history allocation; visibility changes record a durable
receipt with the policy write. Historical applying rows lacking validated
authority, and old approvals that lack a frozen field, fail closed and require
a new request.

## Data and result DTOs

Health checks run only after approval, on a full data copy at
`flats/<slug>/health-check` or `restore-trial`. Copy failure never falls back to
live data. The healthy trial is stopped; live startup runs on real data without
another health request. A healthy `/health` sentinel remains in the copy.
Runtime initialization can write real DB/FILES, including when startup later
fails. Core reports these effects honestly rather than claiming no live impact.
A snapshot that fails before activation stops it with `snapshot_failed` before
live data is touched. Preview execution is a separate explicit action using
isolated data.

`Approval.result_data` is a persisted JSON `core.ApprovalExecution`:

```json
{"status":"approved","data_impact":"runtime_start","health_data":"isolated_copy","live_data":"runtime_may_write"}
```

`data_impact`: `none|runtime_start|restore_data|unknown`.
`health_data`: `not_run|isolated_copy`.
`live_data`: `untouched|runtime_may_write|restored|unknown`.
Creating or deleting a version reports none, as it starts nothing. Static
activation reports none; server activation reports runtime_start; explicit
restore reports restore_data. Recovery reconstructs the corresponding result.
Failed recovery/start/swap paths conservatively use unknown where the prior
runtime may have restarted and written data. `failure_code` is the failure's
[error category](#error-categories), or `apply_failed` for an unclassified
execution failure. The human-readable `result` describes the specific drift
or error.

New snapshots retain API names `before-v<N>-<milliseconds>.sqlite`; the
matching `<name>.sqlite.data/` sidecar contains a consistent DB copy, FILES and
a completion marker. Snapshots cover FILES-only data too and are kept when a
version is deleted. Ordinary rollback preserves live data. Explicit restore
restores new full DB/FILES snapshots and first preserves the replaced data.
Historical SQLite-only snapshots restore DB and preserve current FILES because
no historical FILES archive exists. A persisted `restore-journal.json` allows
startup to undo an interrupted, uncommitted data swap before starting live
code; a committed deployment prevents that undo. Runtime restart side effects
are not exactly-once application side effects. Documents keep no data
snapshots.

## Migration

The store's schema version is `PRAGMA user_version`. Opening an older database
first copies it to `backups/flats.v<version>.<UTC time>.db` (the newest three
are kept) and then applies each later step in its own transaction together
with its `user_version`, so a failed step leaves no trace:

| Step | Adds |
| --- | --- |
| 5 | legacy baseline: the schema and data migrations of databases at versions 0 to 5, including the Draft/published split |
| 6 | host binding |
| 7 | application environment variables |
| 8 | application network permissions |
| 9 | runtime activation generation |
| 10 | Portal relay listing |
| 11 | content revisions (designs) |
| 12 | document revision import cursor, sources and holds |
| 13 | host keys and console nodes |
| 14 | console nodes per network |
| 15 | version quota and version number history |
| 16 | flat identity: a random ID per flat |

A database newer than the binary is refused untouched.

The Draft/published split keeps historical deployed version numbers, hashes,
metadata, histories, secrets, pageviews and data intact. It marks rows that
were live or named by a deployment as `published=1`. Saved-but-never-deployed
rows become Draft revisions at startup; files copy before the DB
pointer/reference migration, and restart can safely resume between immutable
metadata and mutable pointer commit. Every version created since is stored
with `published=1`, deployed or not; whether a flat is Published comes from
its live version or recorded deployments (`HasDeployment`), not from that
column. For websites and pages, numbers are allocated by approved version
creation, never by activation.

Version numbers are never reused. Step 15 records every saved number in
`version_numbers` (with a trigger for new rows), and the next number is one
above the highest recorded or existing one, so a deleted version's number
stays taken. A legacy saved-but-never-deployed row was never a version: its
number is not recorded, and once it becomes a Draft revision a later version
may take that number. Its old files stay at that path until then, and pending
numeric activation/rollback references to it are failed, preventing aliasing.

Legacy preview numeric references migrate to explicit Draft revision
references. Legacy visibility vocabulary is rewritten; old incomplete frozen
approvals safely fail on decision and require a new request. Migration does
not invent Portal or Funnel permissions: already-public legacy policy remains
Public with unavailable routes until an approved action reconciles explicitly
permitted configuration. Pre-lifecycle flats and their one-time Private
Tailscale choice are described in
[Upgrading provider grants from an older host](networking.md#upgrading-provider-grants-from-an-older-host).

## Network contract

Core never imports expose. Legacy `PrivateNet` and `PublicNet` remain;
`Config.Lifecycle LifecycleNet` is the provider manager. Canonical IDs:
`local`, `tailscale`, `tailscale-funnel`, `portal`, `zrok`,
`cloudflare-quick`, `netbird`, `netbird-expose`.

```go
type LifecycleNet interface {
    ServeExposure(context.Context, ExposureRequest) (ExposureResult, error)
    StopPublicRoutes(context.Context, string) (PublicStopResult, error)
}
type LifecycleObserver interface {
    ExposurePolicy(context.Context) (string, error)
    ExposureStatus(context.Context, string) (ExposureResult, error)
}
type LifecyclePreviewNet interface {
    StopExposure(context.Context, string) error
}
type LifecycleListingNet interface {
    SetPortalHidden(slug string, hidden bool) error
}
type LifecycleSlugNet interface {
    StopSlug(context.Context, string) error
}
type LifecycleRouteInspector interface {
    HasProviderRoute(context.Context, string, ProviderID) (bool, error)
}
type LifecycleProviderStopper interface {
    StopProviderRoutes(context.Context, string, ProviderID) error
}
```

Requests contain `Slug, Host, Visibility, Audience, PrivateHandler, Handler,
Ephemeral, Permitted, Hidden`. `PrivateHandler` keeps the Private routes of a
Public request; `Hidden` keeps a Portal route out of relay listings and is not
access control. `SetPortalHidden` changes a registered route's listing without
reopening it. Public stops return `Stopped, Unconfirmed`. Private requests
filter out all public providers; Public requests preserve Private routes. Only
permitted public providers with a registered current endpoint in starting, or
ready/key-expiring with its readiness bit, allow a Public policy commit.
Starting remains Connecting and not ready; backend readiness advances
independently. Missing current runtime returns `not_deployed`. Failed setup
is closed and its route handler cannot serve public bytes while policy is
Private. Unconfirmed public stops cannot commit Private. The manager owns
persisted host gates and backend readiness; per-flat permissions alone are
insufficient. There is no automatic provider fallback.

Revoking a per-flat grant writes the denied permission only after its routes
are gone, including preview and rename-redirect routes. For Tailscale and
NetBird core first confirms `StopProviderRoutes`; for every provider it then
checks `HasProviderRoute` (or the observed status). A route still in use
returns `provider_in_use` and changes nothing, so a public provider is revoked
only after an approved change to Private. Delete, rename and redirect expiry retire routes through `StopSlug`,
which returns only after teardown is confirmed and keeps unconfirmed
registrations for retry.

## Deterministic crash tests

Production builds include a no-op hook and do not read fault environment
variables. Test artifacts built with `-tags lifecycle_testhooks` can use
`FLATS_TEST_PHASE=before_health|after_health|after_allocation_before_live|after_live_before_finalize`
and `FLATS_TEST_PHASE_MARKER=<private absolute path>`. The selected phase
writes one exclusive JSON marker and pauses. The gate kills its own disposable
process and restarts with the same marker, which prevents a second pause. No
HTTP fault endpoint or SQL status injection exists. A slow-health process can
exercise a kill while health is executing.

## Restore-journal recovery

An unreadable, malformed or incomplete `flats/<slug>/restore-journal.json`
blocks startup. This is intentional: serving after an unverified swap could
use the wrong data. Stop the host and preserve a full copy of the data
directory, metadata DB, journal and snapshots before repair. Correct file
access problems and retry startup with the unchanged journal first. For
malformed/missing-approval journals, reconcile the exact approval against
`DeploymentByApproval` and the referenced backup in the preserved copy; an
uncommitted swap must restore its pre-restore DB/FILES backup, while a
committed deployment keeps the restored data. Do not blindly delete the
journal or overwrite the only backup. There is no automated repair command for
a corrupt journal; recovery is operator-assisted and offline.

## Derived Draft state

Draft `dirty` is derived at read time from its content hash versus the newest
saved version (`published=1`, highest number) in the same SQL query
(`store.GetDraft`). Creating or deleting a version can change that comparison
without changing Draft content or revision; activation and rollback do not,
because they move the live pointer, not the newest saved version. Pending
requests do not change it.

## Error categories

Persisted `failure_code`, HTTP `category` and MCP categories all use
`core.ErrorCategory`. Joined causes follow one precedence regardless of join
order:

1. `stale_approval`
2. `public_stop_unconfirmed` (a possibly reachable Public route outranks the
   provider failure it is joined with)
3. `provider_not_permitted`, `provider_unavailable`, `runtime_unavailable`,
   `unavailable`
4. `provider_in_use`, `not_deployed`, `unchanged_content`,
   `provider_not_ready`
5. `config_overridden`, `config_changed`, `snapshot_failed`
6. a deploy error: `runtime_start_failed` when it has a cause, else
   `health_check_failed`
7. `edit_conflict`, `document_capacity`, `conflict`, `forbidden`,
   `not_docs`, `no_revisions`, `not_design`, `document_not_found`,
   `not_found`, `invalid`

Within each line the order shown is the precedence. Unknown execution
failures remain `apply_failed` in persisted receipts. `core.ErrorCategories`
lists every value; each has a refusal page in the agent guide.
