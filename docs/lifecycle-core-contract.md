# Lifecycle core contract

Core owns this document. This is the implemented core/store contract, not an
integrated product acceptance or independent-review verdict. The source base is
`29edc2a6e13e27867603a23ff5b1b5a8d1b84df0`, whose sole parent is
`313ef2557d28350cb91a0daff719aca5bf6813c9`. Transport, production provider wiring,
console integration and the complete acceptance gate have separate owners.

## Content and publication

Each save statically validates the bundle and stores one immutable Draft
revision under `flats/<slug>/draft-revs/<revision>`. `drafts` points at the
current revision; saves do not allocate a published number or run server code.
`SaveMeta.CheckRevision` and `ExpectedRevision` reject competing stale saves.
Draft saves preserve Current, visibility, permissions and live data. A save
whose bytes match Current is clean; publishing identical bytes is rejected
with `ErrUnchangedContent` and does not create a user version for metadata.

Only a validated operator's successful publish creates `vN`, under
`flats/<slug>/versions/<N>`. The version row, live pointer and deployment history
commit in one transaction. Failed health checks consume no number. Existing
published versions can be activated or rolled back without a new number.
Published status persists through connection failures. Visibility is independently
`private|public`; legacy listed/unlisted inputs canonicalize to `public`.
Unpublished flats cannot become Public.

Draft previews always use Private Local and explicitly permitted Tailscale
routes, with isolated data in `flats/<slug>/previews/<host>`. Tailscale access
uses existing tailnet ACLs, not owner-only visitor authentication. Current and
approval-pinned revisions, and open Draft-preview targets, survive revision
pruning. Valid persisted previews resume after restart; expired previews are
closed, and unavailable historical targets get an error event rather than
being rebound to a newly reused numbered version. Successful activation closes
open previews, preserving the existing deployment behavior.

## Callable service surface

```go
SaveVersion(ctx context.Context, slug string, files []bundle.File, meta SaveMeta, via Via) (store.Version, error)
SaveDraft(ctx context.Context, slug string, files []bundle.File, meta SaveMeta, expectedRevision int, via Via) (store.Version, error)
GetDraft(ctx context.Context, slug string) (store.Draft, error)
Publish(ctx context.Context, slug string, revision int, hash string, via Via) (ActionResult, error)
RequestPublish(ctx context.Context, slug string, revision int, hash string, via Via) (ActionResult, error)
Deploy(ctx context.Context, slug string, number int, via Via) (DeployResult, error)
RollbackWithData(ctx context.Context, slug string, number int, restoreData bool, via Via) (DeployResult, error)
RestoreSnapshot(ctx context.Context, slug string, snapshot string, via Via) (DeployResult, error)
OpenPreview(ctx context.Context, slug string, number int) (PreviewView, error)
SetVisibility(ctx context.Context, slug string, visibility store.Visibility, via Via, reason string) (ActionResult, error)
SetProviderPermission(ctx context.Context, slug string, provider string, permitted bool, via Via) error
Decide(ctx context.Context, approvalID string, approve bool) (store.Approval, error)
```

`SaveDraft` always checks the supplied revision. `SaveVersion` supports legacy
transport callers using `SaveMeta`'s optional check. Draft results have
`number:0, published:false, role:"draft", revision:<n>`. Lists of versions contain
only numbered published versions. `Publish` returns an `ActionResult` with
`status:"pending_approval"`; `Deploy`, rollback and snapshot restore return
`*PendingApproval`, which embeds that result and wraps `ErrPending`.
`Deploy(number:0)` requests Current Draft publication; positive numbers request
activation of an existing published version. `OpenPreview(number:0)` targets
Draft. Publish revision zero selects Current; an explicit revision/hash must
match it. No `Via`, including Console, skips approval for these actions.

Flat views add `publication`, `draft`, permitted provider IDs, `connection_state`
and `endpoints`. Endpoint DTOs carry JSON `provider`, `url`, `state`, `detail`,
`configured`, `permitted`, `ready`, `audience`, and `host`. URL existence is not
readiness or proof of Public. The manager supplies fresh concrete status through
the observer contract below. Transport owns HTTP/MCP/CLI serialization and routes;
this core document does not assert those integrations already pass.

## Operator authority and frozen approvals

`Config.ValidateOperatorDecision func(context.Context) error` is mandatory for
`Decide` and per-flat provider-grant mutations: nil denies. A Console label or
same-origin headers confer no decision authority. The transport must validate
its operator credential/session and construct the private proof context consumed
by this callback. App must wire the same authority instance to API and core.
`Config.OperatorIdentity func(context.Context) string` supplies a nonsecret audit
identity; validated decisions persist `decided_by` and `authorized_at` before
execution. Without an identity callback, the audit actor is `validated operator`.

The concrete transport authority is separately implemented and must be tested
against forged-header HTTP decisions and MCP/CLI approval surfaces. Core tests
prove callback enforcement; they do not prove a human was present or that a local
process with operator credentials cannot decide. Operator credential/session and
OS account protection remain privileged boundaries, separate from visitor ACLs.
No operator credentials belong in source, logs or this document.

All critical requests freeze the candidate, live version, visibility and sorted
per-flat permissions plus the manager's stable host/configuration policy token.
Publish additionally rechecks that Current Draft revision/hash still equal the
candidate at decision time: retaining old bytes does not authorize stale Draft
publication. Candidate content hashes are checked from disk. Rollback freezes
target hash and explicit `restore_data`; restores freeze snapshot identity/hash.
Transient connection readiness is excluded from the policy token.

Pending requests deduplicate across caller surfaces by canonical action/params.
Decisions serialize by approval ID and atomically claim pending requests.
Repeated completed approvals return their result. A restart resumes only
persisted validated applying decisions. Deployment receipts prevent repeated
version/history allocation; visibility changes record a durable receipt with
the policy write. Historical applying rows lacking validated authority fail
closed and require a new request.

## Data and result DTOs

Health checks run only after approval, on a full data copy at
`flats/<slug>/health-check` or `restore-trial`. Copy failure never falls back to
live data. The healthy trial is stopped; live startup runs on real data without
another health request. A healthy `/health` sentinel remains in the copy.
Runtime initialization can write real DB/FILES, including when startup later
fails. Core reports these effects honestly rather than claiming no live impact.
Preview execution is a separate explicit action using isolated data.

`Approval.result_data` is a persisted JSON `core.ApprovalExecution`:

```json
{"status":"approved","data_impact":"runtime_start","health_data":"isolated_copy","live_data":"runtime_may_write"}
```

`data_impact`: `none|runtime_start|restore_data|unknown`.
`health_data`: `not_run|isolated_copy`.
`live_data`: `untouched|runtime_may_write|restored|unknown`.
Static publication reports none; server publication reports runtime_start;
explicit restore reports restore_data. Recovery reconstructs the corresponding
result. Failed recovery/start/swap paths conservatively use unknown where the
prior runtime may have restarted and written data. `failure_code` is additive:
`stale_approval`, `health_check_failed`, `runtime_start_failed`,
`provider_not_permitted`, `provider_not_ready`, `provider_unavailable`,
`public_stop_unconfirmed`, `provider_in_use`, `runtime_unavailable`, `unavailable`, `not_deployed`, `unchanged_content`, or `apply_failed`.
The human-readable `result` describes the specific drift or error.

New snapshots retain API names `before-v<N>-<milliseconds>.sqlite`; the matching
`<name>.sqlite.data/` sidecar contains a consistent DB copy, FILES and a completion
marker. Snapshots cover FILES-only data too. Ordinary rollback preserves live
data. Explicit restore restores new full DB/FILES snapshots and first preserves
the replaced data. Historical SQLite-only snapshots restore DB and preserve
current FILES because no historical FILES archive exists. A persisted
`restore-journal.json` allows startup to undo an interrupted, uncommitted data
swap before starting live code; a committed deployment prevents that undo.
Runtime restart side effects are not exactly-once application side effects.

## Migration

Store migration uses `PRAGMA user_version=5` and idempotent column additions.
Historical deployed version numbers, hashes, metadata, histories, secrets,
pageviews and data stay intact. Live/deployment references mark published rows.
Saved-but-never-deployed rows become Draft revisions; files copy before DB
pointer/reference migration, and their source files remain until number reuse.
Restart can safely resume between immutable metadata and mutable pointer commit.
The first successful publish of a never-deployed flat is v1.

Legacy preview numeric references migrate to explicit Draft revision references.
Pending numeric activation/rollback references to removed saved-only versions
are classified failed, preventing aliasing when the number is reused. Legacy
visibility vocabulary is rewritten; old incomplete frozen approvals safely fail
on decision and require a new request. Migration does not invent Portal or
Funnel permissions. Already-public legacy policy remains Public with unavailable
routes until explicitly permitted configuration is reconciled by an approved
action; it is not silently published onto a new provider.

## Network contract

Core never imports expose. Legacy `PrivateNet` and `PublicNet` are unchanged;
`Config.Lifecycle LifecycleNet` is the dedicated manager field. Canonical IDs:
`local`, `tailscale`, `tailscale-funnel`, `portal`. The manager cannot implement
both legacy conflicting `Serve` methods.

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
```

Requests contain `Slug, Host, Visibility, Audience, Handler, Ephemeral, Permitted`.
Public stops return `Stopped, Unconfirmed`. Private requests filter out all
public providers; Public requests preserve Private routes. Only permitted public
providers with a registered current endpoint in starting, or ready/key-expiring with its readiness bit, allow a Public policy commit. Starting remains Connecting and not ready; backend readiness advances independently. Missing current runtime returns `not_deployed`. Failed setup is
closed and its route handler cannot serve public bytes while policy is Private.
Unconfirmed public stops cannot commit Private. Manager owns persisted host gates
and backend readiness; per-flat permissions alone are insufficient. There is no
automatic provider fallback. LifecycleObserver and LifecyclePreviewNet must be
wired for complete policy/status/preview coverage; legacy fallback alone does
not establish the production-manager acceptance lane.

## Deterministic crash tests

Production builds include a no-op hook and do not read fault environment variables.
Explicit test artifacts built with `-tags lifecycle_testhooks` can use
`FLATS_TEST_PHASE=before_health|after_health|after_allocation_before_live|after_live_before_finalize`
and `FLATS_TEST_PHASE_MARKER=<private absolute path>`. The selected phase writes
one exclusive JSON marker and pauses. The gate kills its own disposable process
and restarts with the same marker, which prevents a second pause. No HTTP fault
endpoint or SQL status injection exists. A slow-health process can exercise a
kill while health is executing. Integration owns the real-process gate and
independent Claude review; scoped core tests are not their substitute.

## Restore-journal recovery

An unreadable, malformed or incomplete `flats/<slug>/restore-journal.json` blocks startup. This is intentional: serving after an unverified swap could use the wrong data. Stop the host and preserve a full copy of the data directory, metadata DB, journal and snapshots before repair. Correct file access problems and retry startup with the unchanged journal first. For malformed/missing-approval journals, reconcile the exact approval against `DeploymentByApproval` and the referenced backup in the preserved copy; an uncommitted swap must restore its pre-restore DB/FILES backup, while a committed deployment keeps the restored data. Do not blindly delete the journal or overwrite the only backup. There is no automated repair command for a corrupt journal; operator-assisted offline recovery remains a limitation.

Schema 5 records eligible pre-lifecycle flats for a one-time explicit Private Tailscale upgrade choice without granting providers. Existing explicit denials win. Host grants are still required; neither the choice nor a Tailscale grant authorizes Funnel or Portal. In-use per-flat revocation returns `provider_in_use` before writing policy. Delete, rename and redirect expiry use Manager stop confirmation and retain unconfirmed registrations for retry.

### Derived Draft state and joined failures

Draft `dirty` is derived at read time from its content hash versus the current live version in the same SQL query. An approved activation or rollback changes that comparison immediately without changing Draft content or revision. Pending requests do not change it.

Core persisted `failure_code`, HTTP `category`, and MCP categories use `core.ErrorCategory`. Joined causes follow the same precedence regardless of join order: stale approval, permission, provider unavailable, runtime unavailable, generic unavailable, provider in use, not deployed, unchanged content, provider readiness, unconfirmed Public stop, runtime/health failure, then generic conflict/forbidden/not-found/invalid. Unknown execution failures remain `apply_failed` in persisted receipts.
