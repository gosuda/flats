# Idle document workers — technical design and validation

Status: design (R4), code (R4 plus cancellation hardening), and test source (R3) approved by independent Claude review; integrated verification passed; final report review pending.
Base: `54f1214` (fresh origin/main, 2026-10-17). Product contract: [docs-worker-idle.md](docs-worker-idle.md).

## Existing architecture and constraints

`core.buildWithEnvironment` maps docs onto the embedded JS server, retains a read-only runtime directory and runtime-generation ownership, wraps assets/revision routes, and starts `runtime.Manager`. Manager owns child stdin/socket/proxy, single-flight crash restart/backoff, and terminal Stop. Ordinary server behavior must remain the default.

`designLoop` schedules `TickDocs` sync/import/budget work every 15 seconds; budget validity is two minutes. `runningDocs()` means a live document route, not a live process. `refreshDocsLocked` syncs, imports up to the 64 MiB text budget, acknowledges durable import cursors, settles holds and pushes budget; it can return `more`. Ack errors are currently logged rather than necessarily propagated. `sync` seals due human edit groups; outbox and receipts live in SQLite. Revision sweep refuses to delete blobs when import is incomplete. These are correctness dependencies, not optional housekeeping.

`runtimeGeneration` prevents an obsolete instance from recovering into authority. `runtimeInstance.Stop` releases runtime directory references and ownership permanently. Sleeping must not call that terminal wrapper. Manager child termination currently waits for confirmed process completion, including after kill; a timeout alone must never mean safe release.

## Minimal ownership boundary

Keep one logical Manager instance for each live docs deployment, including while asleep. Add docs-only lifecycle controls through an optional core interface implemented by Manager and forwarded by `runtimeInstance`; ordinary `Instance` and non-docs behavior remain supported. The controls distinguish acquire-with-wake, acquire-if-running, idle eligibility/quiescing, confirmed suspension, and terminal Stop. A lease pins the physical child for the complete operation. RuntimeSpec marks docs lazy/idle eligibility explicitly; neither argv nor visitor headers select it.

Manager continues to own all physical start/exit/stop synchronization. Its existing `starting` channel currently covers restarts only; extend it to cover first lazy start and **initialization as well as spawn**, and a corresponding quiescing completion blocks new acquisitions until sleep finishes or aborts. Do not duplicate spawn algorithms in core. Add a bounded preparation phase for docs wake, with direct access to the just-started instance that bypasses public acquisition. Do not expose that bypass outside trusted core/runtime code.

Core owns application readiness and the storage lock. Public dispatch first attempts an atomic ready-child lease under the Manager mutex, without taking the storage lock. Only the dormant/failed slow path takes the storage lock, rechecks `lf.cur.Load() == d`, performs wake preparation/acquisition, then releases it before streaming. On a stale captured instance, redispatch once to the current `lf.cur` handler (preserving the request/access context); if that also changes or no live instance remains, return the existing retryable unavailable/not-deployed result. Thus ready visitors and WS joins do not queue behind uncertain-edit waits or save trials. Already-locked operations use a dedicated locked acquisition helper, never reenter the storage lock. Preparation performs generation-authorized sync/recovery, bounded import, and an explicit fresh-budget attempt before publishing the instance as ready. The outcome table below defines recoverable readiness; `refreshDocsLocked` alone cannot prove budget success because it logs that error. The direct preparation path is also used for its `docsHost` calls/checkpoint so it cannot recursively wait on its own `starting` channel. Do not run a callback which acquires the storage lock from a Manager restart initiated while that same lock is held.

To enforce this ordering, docs requests enter through core's acquisition boundary rather than using generic Manager's automatic `ensure` behind its back. An acquired docs dispatch must return unavailable on physical process loss; the next core acquisition owns recovery/preparation. Ordinary server automatic restart stays unchanged. A crash during a pinned operation may fail that operation; existing durable receipt reconciliation resolves uncertain commits, and must acquire/prepare a replacement through the same locked boundary.

## Concrete control contract and implementation ownership

Define in core (names fixed for parallel implementation):

```go
type RuntimeLease interface {
    http.Handler // exact pinned child; no automatic restart
    Release()   // idempotent
}
type RuntimeQuiescence interface {
    http.Handler // exact child, transition owner only
    Suspend(clean bool) // commit async termination; nonblocking
    Resume() error     // core has completed fresh-budget preparation
}
type ManagedDocsInstance interface {
    Instance
    TryAcquire(foreground bool) (RuntimeLease, bool)
    Wake(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error)
    TryQuiesce(time.Time, time.Duration) (RuntimeQuiescence, bool)
    CleanDormant() bool
    Retire() // forbid new admission/start, existing leases may drain
    Quiescing() bool // includes drain and asynchronous physical termination
    InvalidateClean() // invalidate an existing or pending clean-sleep certificate
}
```

`RuntimeSpec` gains `ManagedDocs bool`, `Lazy bool` and `Now func() time.Time` (the core clock for idle/cooldown, distinct from real process uptime/backoff); Lazy is legal only with ManagedDocs. Scratch trials use neither flag. Live eager docs are managed and may spawn during build, but must not run activation/sync/readiness before `CommitPublished`/`SetLive` succeeds. Core then swaps the new instance as non-ready and prepares that exact child under the still-held storage lock before releasing the operation; no unprepared child is publicly ready. `TryAcquire` never starts or waits and only succeeds for ready, current, nonretired state. `Wake` requires the caller's storage lock, owns its supplied bounded preparation callback, grants a foreground lease after success only after rechecking under its mutex that the exact prepared child is still current, alive, nonretired and nonterminal, and returns an error otherwise. A recoverable import error caused by child death cannot publish readiness; its original exit/termination owner and retry policy remain authoritative. Core treats recoverable outcomes above as callback success after safe admission is established; all other callback errors trigger owned termination. A contender takes the ready fast path or waits cancellably for the storage lock; it never invokes a second preparation concurrently.

`TryQuiesce` only succeeds for zero leases after idle deadline; it reserves admission and returns a direct handler. Exactly one of Suspend or Resume consumes its transition. A core drain error still must call Resume after fresh readiness or Suspend(false) on fatal protection failure. `CleanDormant` is true only for confirmed no-child and a clean certificate. Resume is not a public escape hatch: only the core transition owner calls it after budget readiness; it rejects retired/terminal/child-lost state. Terminal Stop may invalidate outstanding leases/transitions, whose later Release/Resume must remain safe.

Core implementation owner: optional interfaces/RuntimeSpec in `internal/core/service.go`, core lifecycle/wrappers/caller integration, docs host-only sync proof and budget revoke in `internal/apps/docs/_web/src/{revisions,server}.js` plus regenerated dist and docs. Runtime implementation owner: only `internal/runtime/manager.go` plus a new `internal/runtime/docs_lifecycle.go` (no core files). Agree/freeze interface before delegation. Tests remain gated on development review and are then assigned with separate file ownership. Core's existing test fake runtimes may retain eager legacy behavior unless implementing the optional managed interface; real Manager must implement it.

## State and concurrency invariants

States: dormant, starting/preparing, ready, quiescing, terminal. State, child identity, activity count, last foreground release time, clean-sleep certificate and transition channels are protected by Manager's mutex. Never hold it during process I/O, waits, core callbacks, or storage locking. The full permitted order is per-flat storage lock → short `Service.mu`/`runtimeMu`/`docsMu` access → short Manager state operation. No global core mutex may be held while waiting on Manager transitions or process completion. Refactor `Service.Close` to snapshot live/preview instances under `s.mu`, unlock, then Stop them; preparation may still call `state` safely. Refactor existing Manager `spec.Log` calls out of `in.mu` in `exited` and `ensure`; log/store callbacks are not short mutex operations. Manager never acquires core locks.

Foreground acquisition waits cancellably on a transition and shares a bounded start. Once started, work has an owned bounded lifecycle context independent of an individual visitor cancellation. The controller's terminal cancellation prevents publication after deletion/replacement. All wait channels are closed on every success/failure/terminal path. Child identity checks prevent a late exit callback clearing a newer child. Sleep does not reset crash backoff or fabricate a successful restart.

Activity leases cover the entire proxy ServeHTTP, including upgraded WebSocket copying until disconnect and slow response completion. Internal multi-request operations hold an outer lease per logical instance across budget/edit/receipt/import or stage/restore/settle. Non-same-shape restore switches leases when it switches instances; a lease on the retired source must never authorize calls to the replacement. Initial successful ready publication sets last foreground activity to now, including eager saves and first demand wake. A lease release records foreground idle time only for user-demand operations; background release does not. An aborted sleep with no foreground use preserves the prior idle deadline and backs off quiescence attempts: 30 seconds initially, doubling to a five-minute cap. Normal no-wake refresh still runs during cooldown; foreground activity resets it. This avoids repeated drain/revoke churn for a persistent backlog or hold.

Idle eligibility is checked on `TickDocs`, default two minutes (injectable clock/interval only in tests). Under the storage lock, atomically reserve quiescence only if no leases exist and the deadline passed. New foreground demand waits cancellably for at most four seconds from its acquisition attempt for quiescence completion (three-second graceful exit plus one second kill/reap allowance); if still quiescing it returns HTTP 503 with Retry-After: 1. This bounds caller latency, not physical ownership; it cannot revive the reserved child or cancel a committed sleep. If drain fails and sleep is aborted, readiness must pass the fresh-budget policy below before opening admission; it never enters a partially drained child. Drain through a privileged transition-owned lease, without waiting for foreground lease completion while holding storage lock. If a foreground operation exists, skip sleep entirely and retry next tick.

The final drain uses normal bounded sync/import; `more`, failed ack, failed sync, unresolved pending state, or nonempty outbox postpones sleep. Verify worker-reported outbox empty after acknowledgements rather than inferring emptiness from imported bytes or suppressed ack errors. Extend the internal sync result with enough pending-group/dirty/intent state if its existing `ready` count cannot prove quiescence; do not modify persisted history or force an early group boundary. Two minutes exceeds the current 30-second edit-group idle window (`GROUP_IDLE`); `GROUP_MAX_AGE` is five minutes. Tests must preserve both group boundaries. After a successful drain, commit stopping under the Manager mutex, release the storage lock, and terminate asynchronously in one instance-owned goroutine. `TickDocs` never waits for physical exit; an unconfirmed child cannot block other docs budget refreshes. Keep existing graceful/kill behavior and publish dormant only after `proc.done`. The goroutine has one child identity and a completion signal; terminal Stop joins that owner rather than double-terminating. If exit remains unconfirmed, remain quiescing and reject demand after that four-second wait bound with retryable unavailability, without spawning another child. Terminal Stop cancels preparation and prevents admission, never waits for foreground leases (a WS can live indefinitely), then terminates/joins physical ownership before permanent release. Stop may wait for bounded preparation completion only without any core global mutex; unconfirmed OS process death remains an explicit shutdown limitation, never reported as successful cleanup. Ordinary periodic maintenance must not wait on this join.

## Caller census and policy

* Isolated `checkIsolated` and `trialRun` scratch-dataDir instances: eager, never idle-eligible, use raw ready dispatch, never acquire the live storage lock from a handler. This covers saves invoking `healthCheck(d.handler)` while already locked.
* Visitor docs HTTP/WS: foreground lease and full wake readiness. Host-served assets remain outside the acquisition wrapper.
* GetDocument, UpdateDocument and uncertain-edit receipt polling: foreground locked acquisition, pin full operation. Do not keep a lease across a recursive acquisition; pass an internal lease context or direct instance explicitly.
* Revision restore, save/activation intent and same-shape rollback: foreground lease across their full existing locked operation. Preserve 12-second restore and uncertain-commit protocols.
* `catchUpDocs`, ListRevisions, GetRevision, PinRevision, SetRevisionQuotas, RevisionQuotas and history routes: no-wake refresh via acquire-if-running; historical files remain host-only. They neither wake nor reset foreground idle. Dormant history is the durable imported history, and startup recovery can add pending entries on next real demand. Ordinary listing/settings/provider operations do not wake.
* TickDocs and quota propagation: acquire-if-running; skip dormant or transitional instances without waking or refreshing their idle deadline. Awake documents including open WS still receive the 15-second refresh and quota updates before return where currently required.
* Startup `startDocsRevisions`/`retainAllRevisions` and blob sweeps: do not turn restore into mass startup. If no physical child can be pinned, defer worker-dependent recovery, retention/source deletion and blob sweep conservatively. Existing durable outbox/source/hold protections remain until first wake. No claim that lazy startup means pending work has been imported.
* `prepareDocsSweepLocked`: pin-if-running, or use the in-memory clean-sleep certificate described below. Return false for lazy-restored dormant instances without that certificate, transitional/incomplete/crashed instances, and unconfirmed exit. Retention while cleanly asleep may delete old revisions and sweep their unreferenced blobs without waking.
* `runningDocs`: retain its logical route meaning; add explicit physical acquisition checks rather than changing all existing callers implicitly.

## Startup, activation and lifecycle

Every docs physical spawn, including first lazy spawn, calls the ownership-checked `NextGeneration`, not just crash recovery. On live swap retire the old instance for new admission synchronously before the five-second drain: existing leases may finish, but no new acquisition or start may revive it. Slow acquisition also rechecks current logical identity after taking storage lock. Only restoration of already-live docs is initially dormant. Fresh/changed saves and isolated activation trials start eagerly and pass existing health checks. After a successful live database commit and swap, the new instance receives normal preparation/budget before use, then becomes idle-eligible; startup's legacy `startSavedDocs` still validates never-activated saved docs. Materialized runtime references and data ownership remain attached to the logical instance through sleep. Wake uses a new permitted generation but the same current content/environment, and must not create an activation revision unless ordinary recovery detects an actual pending content change.

Routing/provider handles stay installed. Visibility changes never wake solely to reconcile exposure. Rename/replacement/deletion retire the logical instance, not just its current PID; queued requests cannot start old content against renamed/deleted storage. Existing lifecycle errors remain errors. No production migration, setting, UI change, telemetry endpoint or app JS engine replacement is required. Runtime implements the logical controller as `docsInstance`, using a fresh raw child instance/socket for each spawn so a released or stale lease cannot target a replacement child. `InvalidateClean` increments a certificate epoch before every replacement build, invalidating even a late old-child clean exit after failed commit.

## Preparation outcomes and default host-call policy

| Outcome | Admission and cleanup |
| --- | --- |
| Sync succeeds; import complete; budget succeeds | Publish ready; normal reads/mutations. |
| Sync returns `ErrDocumentCapacity` | Keep child ready for reads and explicit recovery (rollback/intent); preserve the existing capacity refusal for ordinary mutations. This must not become a permanent 503 restart loop. |
| Import reports `more` or fails after successful/capacity sync and confirmed budget revoke (including ack rejection, invalid entry/hash, blob-write/store errors) | Keep child ready under the budget policy; emit a redacted revision error event for failures and retry bounded import on later no-wake ticks. Existing worker reservations protect unimported content. Mark unresolved backlog; refuse clean sleep and source/blob sweep until a later successful drain proves safety. Import failure alone must not block reads or repair operations. |
| Budget install fails | Keep the one child ready for reads and recovery, with normal mutations blocked. Budgets persist in SQLite, so a fresh process is not proof of a missing budget. Before every wake preparation and aborted-sleep reopening, explicitly revoke the stored budget via a new trusted host-only `budget/revoke` operation (set/remove the budget so mutation checks fail closed; preserve reservations). Require acknowledgement before any admission, then attempt a fresh installation. If revocation cannot be confirmed, do not reopen admission: terminate and wait for `proc.done`. Never rely on an old TTL after a skipped quota update. Successful refresh restores normal mutation admission. |
| Spawn failure, non-capacity sync failure, unconfirmed budget revoke, or readiness-control protocol failure (excluding errors confined to revision import above) | No readiness publication. Close the child through the single termination owner, await confirmed exit asynchronously, then enter dormant with backoff. A second spawn for this same logical instance is forbidden until exit is confirmed; distinct committed replacement follows the generation-guarded overlap rule below. First-wake failure emits a redacted lifecycle event. |

The preparation readiness check deliberately distinguishes capacity/backlog from fatal initialization. Explicit budget installation, not the log-only error in `refreshDocsLocked`, determines readiness. All transitions back into ready, including aborted quiescence, remeasure/install current budget under storage lock; the degraded-read-only fallback above applies if this fails. Quota propagation that sees a transition may skip the child only because no normal mutation is admitted until this refresh. No public caller can forge a recovery mode using headers.

`docsHost` defaults to acquire-only-if-running and returns `errNotRunningDocs` for dormant/transitional state. It does not imply wake. Internal transition/outer-lease contexts target the exact leased child and bypass reacquisition. Explicit locked foreground acquisition must be added before: changed-save intent (preserve label/message), rollback including initial refresh, UpdateDocument budget push, getDocument, and receipt lookup/recovery. The unchanged-save path must not wake just to attempt its trailing refresh. The existing post-save refresh is no-wake; changed activation is already eager. `docsIntentLocked` first attempts explicit locked acquisition so a healthy sleeper preserves the save label/message. Intent remains best effort for repairing a broken document: fatal preparation, terminal/unavailable source, backoff, or failed intent does not reject changed content. Record a redacted event that the save label/message could not be recorded and continue existing isolated validation/activation. Propagate caller cancellation. If the old logical instance is idle-quiescing or still terminating after failed preparation, a changed save may wait at most four seconds; if it exits in time, the ordinary wake preserves attribution. Otherwise the changed save continues without its intent label and records the event. It never waits indefinitely on that child. The no-second-worker rule applies to restart/wake of the **same logical instance**: it cannot spawn again before its child exits. A distinct replacement version follows the existing overlap model: it may start a new child before commit without issuing activation-affecting requests; after a successful commit/swap it retires the old logical instance, prepares using a newer ownership-checked generation, and may serve while the retired old child finishes exiting. Generation guards protect shared data exactly as in the existing five-second drain. Each old termination retains its exact owner until confirmed completion. This explicitly permits the first changed-save repair attempt after a failed wake; it is not permission to respawn the old instance.

## Live commit boundary and preparation failure

`buildWithEnvironment`/Manager.Start may allocate and start the new eager live child but must not send sync, import, budget/revoke or any other activation-affecting request before the durable live-version commit. Scratch trial handlers still validate only their copied DataDir. If `CommitPublished`/`SetLive` fails, stop the unused new child and undo its ownership as today; the old live child remains authoritative and its DB active hash/generation must not change.

After commit succeeds, swap to the non-ready new instance and retire the old instance for new admission. Under the same storage lock, call preparation with a direct handler for the new child, not a lookup through `lf.cur` that could accidentally select an old one. Requests seeing the non-ready swap wait through the existing bounded/cancellable slow path. Apply the outcome table: capacity/import backlog stays usable with the budget policy; fatal preparation terminates that child and leaves the committed logical instance dormant with backoff and an error event, so later demand retries. Do not roll back the durable live-version commit or report the old instance as current. Report a post-commit readiness failure with the existing deployment/live-data qualification; the saved content is committed even if initial serving is unavailable. A later changed save remains a repair path.

## Clean sleep and retention

On successful final sync, require no pending dirty groups/intent, an empty outbox after confirmed ack, and no unresolved holds requiring worker inspection. Record a non-durable certificate `(logical identity, generation, dataDir)` only after the corresponding child is confirmed gone. It certifies that this instance has had no process capable of creating new outbox entries since the verified drain. Invalidate it before any spawn, on crash/failed preparation, retirement, rename/replacement or state uncertainty. Do not persist it; lazy restore after process/host crash has no certificate.

Under the storage lock, `prepareDocsSweepLocked` may use a matching clean dormant certificate to proceed through the existing current/unpruned/draft source keep-set and reference/hold-aware blob sweep. Revision retention/unpin/quota changes therefore reclaim clean sleepers' unreferenced blobs without a wake. No direct SQLite inspection is introduced. An uncertified restored sleeper continues to defer sweep until demand-driven recovery and a later clean drain; report this conservative case separately.

## Contract documentation and observability

When implemented, update `docs/revision-store.md` import/catch-up/sweep rules and `docs/internal/docs-app.md` background sync/readiness statements. Document that docs `running` means a logical live route even when lazy restoration has not yet proved physical startup; first-wake failure is reported as an error event and retryable request failure. Do not describe sleeping as unpublished. Physical sleep releases child memory, while host logical instances, proxy/transport objects, flat state/provider nodes and materialized files remain.

## Validation and report

Tests are written/run only after design PASS and development review PASS as requested. Keep independent source and final test-report review gates distinct. Use focused lifecycle tests with deterministic barriers/fake clock, plus existing real docs host harness (`internal/apps/docs/host_test.go`), not only Manager JS fixtures.

1. Baseline five real docs and one ordinary server. Exercise real first-document page and API reads. Advance idle eligibility; prove docs PIDs terminate and remain absent across multiple maintenance/quota/retention ticks, ordinary server PID survives, and provider handler identity/URL/visibility remains.
2. Reopen a real docs page: verify exact persisted live text, attachments, receipt IDs, history and successful fresh edit. Record measured cold-start wall time for first page and first edit, then warm comparison; no generic JS historical latency substitution or unsupported SLA.
3. Concurrent first requests: barrier start, one child/generation, all successful responses; cancel one waiter and prove others succeed. Inject initialization failure, budget failure, crash and backoff; prove no unchecked edit and recoverability.
4. Hold HTTP response, upload, WS, multi-step edit/restore and uncertain receipt operations across idle eligibility. Prove no termination, no deadlock, and idle begins after final foreground release. Exercise request arriving before/after quiescence and terminal Stop racing start/sleep.
5. Durable human edits with pending groups/outbox, bounded multi-page import, lost ack, unresolved restore hold and corrupted/unavailable source: refuse sleep/sweep until safe. Compare exact revision IDs/text hashes before/after wake; no duplicate activation revision. Startup with pending crash recovery must defer sweep, recover on demand, and preserve bytes.
6. Change quota while asleep; prove no wake, then first API edit and first WS mutation use current budget. Keep WS open longer than budget TTL; show background refresh allows valid edits and errors fail closed when required.
7. Restart host with saved docs: routes exist with no docs worker until demand; successful and failed new-save health trials retain existing behavior. Test rename/delete/replacement during waiting wake and verify no stale process resurrection.
8. Repeat at least 20 real docs sleep/wake cycles, asserting worker/FD/goroutine cleanup and text/receipt integrity; run targeted race tests for transitions and existing docs revision, activation, edit, runtime stop/restart, lifecycle suites. Run repository-prescribed verification appropriate to changed packages plus main build and required nested-module compile checks.
9. Disposable Linux measurement when available: before/load/idle/wake process PSS/anonymous plus cgroup memory, worker PID counts, same fixtures. Report hardware/OS/commit/test commands/exits/sample times and whether page cache accounts for residual memory. Assert process disappearance; record memory reduction rather than arbitrary exact MiB threshold. No production restart/load/configuration changes.

Write the actual results in `docs/spec/docs-worker-idle-TEST-REPORT.md`, clearly separating PASS, failure and unperformed checks, with exact source commit and commands. Independent Claude report review must PASS before one feature PR is published; no merge authorization is implied.

### Review-derived mandatory scenarios

Add shutdown while preparation is blocked in `docsHost`, proving `Service.Close` holds no `s.mu` during Stop; ready request/WS fast paths while a storage lock is held; changed-save scratch health checks; live read/edit/receipt already-locked dispatch. Test capacity-failed activation → host restart → wake → rollback, bounded import backlog readiness, budget failure read-only recovery, and fatal prep failure with exact child exit before retry.

Use a termination barrier to simulate a stuck child while many docs qualify on one tick: other open-WS docs must receive budget refresh within TTL; terminal Stop never waits for WS lease release. Test quota update during quiescing and aborted drain; stale quota must not permit a mutation. All history/list/get/pin/quota APIs and unchanged saves on sleepers must preserve zero worker starts and idle time; changed-save intent must retain its label/message.

Test clean sleeping retention/unpin/quota sweep frees unreachable blobs without wake, and lazy-restored uncertified state does not sweep pending outbox sources. Test old-instance request in the five-second replacement grace cannot wake it. Real-doc human-group cases must let actual worker `Date.now()` advance at least 30 seconds (or use an explicit worker clock seam); advancing the host fake clock alone is not evidence. The report records remaining host/provider memory and exact unavailable checks.

### Second-review mandatory scenarios

After crash/lazy restore inject persistent import hash/blob-write failure: successful live read and explicit recovery remain possible, while clean sleep/sweep stay blocked and revision errors are recorded. A failed old-worker non-capacity sync must not block the first changed save that passes its own isolated trial, even with the old logical instance still terminating; verify the missing-intent-label event, newer generation, retirement, exact old-child ownership and fresh content. Repeat with a save arriving during idle quiescence, which also continues without intent attribution. Inject `CommitPublished` failure before readiness and prove the old active hash/generation remains unchanged, human groups continue recording, and no uncommitted activation revision appears. Inject fatal readiness after a successful commit and verify committed logical content plus retryable unavailable state and eventual recovery. Verify a freshly ready saved doc gets a full two-minute interval; replacement redispatch is bounded to one retry; quiescence request wait returns within its four-second bound without allowing a second child.

Final review test: kill the exact child during a preparation import so its HTTP failure is classified as import backlog. Wake must nevertheless reject readiness when its final locked child-identity/liveness check fails, retain one cleanup owner, and only retry that logical instance after confirmed exit.

## Code review corrections

A post-commit readiness error carries `DeployError.Committed`. `publishRevision`
must preserve its instance/version directory and finalize the saved draft even
when returning that error; only failed commits trigger deletion/Stop cleanup.
Post-commit preparation ignores caller cancellation but remains bounded and
cancelled by terminal lifecycle actions. Error text distinguishes committed
content awaiting serving from an unchanged old document.

Public demand starts its four-second quiescence budget at handler entry and
checks quiescence while polling the storage lock; the bound includes the drain.
It never creates state while looking up a deleted/renamed slug, and bounded stale
redispatch invokes the replacement's full handler (including asset wrappers).
Readiness/wake events omit caller cancellation and ordinary quiescence timeouts;
an unchanged save on a sleeper emits no false revision warning.

Rollback uncertainty reacquires an exact child before receipt lookup, final
refresh and hold settlement. A single operation-scoped import receipt captures
its revision if wake preparation imported/acknowledged that rollback first; no
global receipt ledger is added. Rename/restart rebuilds are lazy, so a child does
not remain resident forever merely because rebuild bypassed activation.

Ordinary server `Stop` already waited for an in-progress crash restart at base
`54f1214` (`manager.go:600-616`); this feature does not change that inherited
uncancellable-wait limitation. Managed docs cancel owned preparation and recover
transition cleanup even when a preparation callback panics.

### Committed readiness failure transport contract

After the live commit, failed readiness retains the committed files and returns `DeployError.Committed` with the existing `ErrUnavailable` sentinel. The cause includes only deployment-redacted preparation text. REST uses its existing `unavailable` category and HTTP 409; its message explicitly says content was committed, a later request retries serving, and callers must not repeat the save or create another flat. CLI and console propagate this API message. MCP overrides every generic failure hint for a committed error, including create and save, with the committed outcome, `get_logs`/`get_flat` guidance and no-repeat instruction. No new API category is introduced.

Manager logs actual start/preparation failures through the existing redacting, rate-limited runtime callback. Core acquisition adds no duplicate request-level event, so backoff/stopped refusals cannot displace the actual failure with one event per visitor. Acceptance checks include all three caller surfaces and secret-bearing preparation errors, plus repeated backoff refusals without extra events.

All locked wake callers (document reads, updates and rollback as well as saves) share error normalization in `acquireDocsLocked`: with a live caller context, worker failures/refused backoff become deployment-redacted `ErrUnavailable`; caller context errors remain unchanged. Shutdown already returns that sentinel before invoking Wake. Committed activation preserves the normalized cause without wrapping or prefixing it twice. Acceptance checks cover read/update/rollback failed respawn and backoff as well as committed saves.
