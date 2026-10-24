# Idle document worker internals

How the host puts built-in document workers to sleep and wakes them. The
product rules ("spec rule N" below) are in
[Idle document workers](../spec/docs-worker-idle.md). The docs app side, which
is the revision protocol and the host operations the lifecycle calls, is in
[Docs app internals](docs-app.md#host-operations), and the worker states that
operators see are in [Metrics internals](metrics.md#worker-state).

The code is in `internal/core/docs_idle.go` (readiness, callers, the drain),
`internal/runtime/docs_lifecycle.go` (the logical instance, leases,
termination) and, for the optional interfaces and the `RuntimeSpec` fields,
`internal/core/service.go`.

## Overview

```
request ─► docsDemand ─► TryAcquire ─► lease ─► child (ready)
                │ no ready child
                └─► storage lock ─► Wake ─► spawn ─► prepare ─► lease
                                                  (revoke, access, sync, import, budget)

TickDocs (15 s) ─► idleDocsLocked ─► TryQuiesce ─► drain (sync, import, sync, holds)
                        ├─ clean ─► Suspend(true) ─► finishStop ─► sleeping, certificate
                        └─ else ──► prepare again ─► Resume (cooldown) or Suspend(false)
```

A live built-in document is one logical instance for as long as it exists.
Its child process starts and ends. When no foreground request or collaboration
connection has used it for two minutes, the next maintenance tick drains it,
proves that nothing unrecorded is left, and ends the child. The next demand
starts a new child, which recovers and prepares before it serves.

## Background

`core.buildWithEnvironment` maps a docs flat onto the embedded JavaScript
server, keeps its read-only runtime directory and its runtime-generation
ownership, wraps the asset and revision routes and starts `runtime.Manager`.
The Manager owns a child's stdin, socket and proxy, the single-flight crash
restart with backoff, and `Stop`. An ordinary server flat keeps
the Manager's behavior unchanged.

`designLoop` calls `TickDocs` every 15 seconds (`docsTick`). A tick syncs and
imports each running document whose lock is free and pushes its disk budget,
which a worker trusts for two minutes without a refresh (`docsBudgetTTL`).
`runningDocs()` means a live document route, not a live process.
`refreshDocsLocked` syncs, imports up to 64 MiB of text per call
(`docsImportBudget`), acknowledges durable import cursors, settles holds and
pushes the budget; it can report `more`. It logs a budget failure and does not
return it, so it cannot prove that a budget was installed. `sync` closes due
human edit groups, and the outbox and receipts live in SQLite. The revision
sweep refuses to delete blobs while an import is incomplete. These are
correctness dependencies, not housekeeping, so a sleep has to preserve them.

`runtimeGeneration` keeps an obsolete instance from recovering into authority.
`runtimeInstance.Stop` releases the runtime directory references and the
ownership permanently, so a sleep never calls it. Child termination waits for
confirmed process completion, also after a kill (`terminate`: stdin EOF, three
seconds, kill, wait). A timeout alone never means that a resource is safe to
release.

## Ownership

One logical instance stands for a live docs deployment, also while its child
sleeps. In the runtime it is `docsInstance`; core's `managedDocsRuntime`
forwards the lifecycle controls to it. Ordinary `Instance` behavior and non-docs
flats are unaffected. The controls separate acquiring with a wake, acquiring
only if running, idle eligibility with quiescing, confirmed suspension, and
`Stop` as the final shutdown. A lease pins the physical child for a complete operation.
`RuntimeSpec` marks a docs flat as managed and lazy explicitly. Neither the
process arguments nor a visitor header selects it.

The Manager keeps all physical start, exit and stop synchronization. A
transition channel on `docsInstance` covers the first lazy start, spawn and
initialization together, restarts and quiescing. New acquisitions wait for it
cancellably until a sleep finishes or aborts. Core does not repeat the spawn
algorithm. `Wake` runs a bounded preparation phase with direct access to the
child it just started, bypassing public acquisition. That bypass is not exposed
outside trusted core and runtime code.

Core owns application readiness and the storage lock.

- A public request first tries an atomic lease on a ready child under the
  Manager mutex (`TryAcquire`), without the storage lock.
- Only the sleeping or failed path takes the storage lock, checks again that the
  instance it captured is still the current one (`lf.cur`), prepares and
  acquires (`Wake`), and releases the lock before it streams.
- If the captured instance went stale, the request is dispatched once more to
  the current handler with its request and access context and all wrappers of
  the new version (`docsRedispatchKey`). If that changes again, or nothing is
  live, the existing retryable unavailable result is returned. A lookup of a
  route that was deleted or renamed finds no instance and creates no state
  (`currentDeployment`).
- Ready visitors and WebSocket joins therefore do not queue behind
  uncertain-edit waits or save trials.
- An operation that already holds the lock uses `acquireDocsLocked` and never
  enters it again. Preparation reaches the child through its direct handler,
  so it cannot wait on its own transition. No callback that takes the storage
  lock runs from a Manager restart that was started while that lock is held.

Docs requests enter through core's acquisition boundary and not through the
generic Manager `ensure`. An acquired dispatch returns unavailable when the
physical process is lost, and the next acquisition in core owns the recovery
and preparation. A crash during a pinned operation may fail that operation;
the durable receipt reconciliation resolves uncertain commits and acquires and
prepares a replacement through the same locked boundary. Automatic restart of
an ordinary server is unchanged.

## Control interfaces

```go
// internal/core/docs_idle.go
type RuntimeLease interface {
	http.Handler // the exact pinned child; never restarts it
	Release()    // idempotent
}
type RuntimeQuiescence interface {
	http.Handler        // the exact child, for the transition owner only
	Suspend(clean bool) // commits asynchronous termination; does not block
	Resume() error      // core has finished fresh-budget preparation
}
type ManagedDocsInstance interface {
	Instance
	TryAcquire(foreground bool) (RuntimeLease, bool)
	Wake(context.Context, func(context.Context, http.Handler) error) (RuntimeLease, error)
	TryQuiesce(time.Time, time.Duration) (RuntimeQuiescence, bool)
	CleanDormant() bool
	InvalidateClean()
	Retire()
	Quiescing() bool
}
```

`RuntimeSpec` (`internal/core/service.go`) has three fields for it.
`ManagedDocs` marks a live built-in document whose readiness and idle sleep
core owns. `Lazy` restores the logical document without starting its child and
is legal only with `ManagedDocs`; `Manager.Start` refuses it otherwise. `Now`
is the core clock for the idle time and the cooldown, separate from the real
process uptime and the restart backoff; nil means `time.Now`. Scratch trials
use none of them. A live eager document is managed and may spawn while it is
built, but runs no activation, sync or readiness before `CommitPublished` and
`SetLive` succeed. Core then swaps the new instance in as not ready and
prepares that exact child under the storage lock that it still holds, so no
unprepared child is publicly ready.

- `TryAcquire` never starts a child and never waits. It succeeds only for a
  ready, current, non-retired child with no transition in progress.
- `Wake` needs the caller's storage lock and owns the preparation callback it
  receives. After success it grants a foreground lease only if it re-checks,
  under its mutex, that the exact prepared child is still current, alive,
  non-retired and not stopped; otherwise it returns an error. A recoverable
  import error that the child's death caused cannot publish readiness. The exit
  owner of that child and its retry policy stay in charge. Core treats the
  recoverable outcomes of the table in "Wake preparation" as callback success
  once safe admission is established. Any other callback error ends the child
  through its owner. A contender takes the ready fast path or waits for the
  storage lock cancellably; two preparations never run at once.
- `TryQuiesce` succeeds only for a ready, alive child with no transition in
  progress and no lease, after the idle deadline and outside the cooldown. It
  reserves admission and returns a direct handler. `Suspend` or
  `Resume` consumes the transition, exactly one of them. After a drain error core
  calls `Resume` once fresh readiness is proven, or `Suspend(false)` after a
  fatal protection failure. `Resume` is not a public escape hatch. Only the
  transition owner calls it after budget readiness, and it rejects a retired,
  stopped or child-lost state.
- `CleanDormant` is true only for a confirmed absent child with a valid clean
  certificate. `InvalidateClean` invalidates a certificate that exists or is
  pending.
- `Retire` forbids new admission and any start and cancels a preparation in
  flight. Existing leases drain, and a ready child stays up until core stops it.
- `Quiescing` reports a transition that is a drain or an asynchronous physical
  termination. Foreground waits use it to bound their time.
- `Stop` may invalidate leases and transitions that are outstanding;
  their later `Release` and `Resume` stay safe.

## Leases and locks

The Manager mutex protects the state, the child identity, the lease count, the
time of the last foreground release, the clean certificate and the transition
channels. It is never held during process I/O, waits, core callbacks or storage
locking. The permitted order is: the per-flat storage lock, then a short
`Service.mu`, `runtimeMu` or `docsMu` access, then a short Manager state
operation. No global core mutex is held while waiting for a Manager transition
or a process exit. `Service.Close` copies the live and preview instances under
`s.mu`, unlocks, and only then stops them, so a preparation that still calls
`state` cannot deadlock it. The Manager never takes a core lock. A log or
store callback is not a short mutex operation, so the Manager calls `spec.Log`
outside its instance mutex (see `exited`).

A lease covers the whole proxy `ServeHTTP`, including the copying of an
upgraded WebSocket until it disconnects and a slow response until it ends. An
internal operation of several requests holds an outer lease for its logical
instance across the whole budget, edit, receipt and import sequence, or across
stage, restore and settle. A restore that switches to another instance switches
its lease with it, so a lease on the retired source never authorizes calls to the
replacement.

A foreground release records the idle time and clears the cooldown; a
background release (`TryAcquire(false)`) does neither. A successful ready
publication, including the first demand wake and an eager save, sets the last
foreground activity to now. Foreground acquisition waits cancellably for a
transition and shares one bounded start. Once started, the work has a lifecycle
context that belongs to the deployment and not to one visitor, bounded by the
Manager's start timeout (60 seconds when unset). `Stop` and `Retire` cancel it,
which prevents publication after a deletion or replacement. Every wait channel
closes on every success, failure and shutdown path. Child identity checks stop a
late exit callback from clearing a newer child. Every physical spawn, the first
lazy one included, calls the ownership-checked `NextGeneration`.

## Idle sleep

`TickDocs` calls `idleDocsLocked` for each running document whose lock is free,
before it refreshes the document. The idle timeout is two minutes
(`docsIdleTimeout`; tests inject the clock through `RuntimeSpec.Now`). It
exceeds the 30-second idle time of a member's edit group (`GROUP_IDLE`); a
group also closes five minutes after it began (`GROUP_MAX_AGE`), and the tests
keep both boundaries. The guests' group of a Public document at level `edit`
never closes for idleness: `sync` closes it only when its five-minute window
ends, so a document that guests edited cannot sleep cleanly before then unless
an explicit operation closes the group first (leaving `edit`, a safe point, or
another forced boundary such as an agent edit or a rollback). Under the storage
lock `TryQuiesce` reserves the quiescence only if no lease exists and the
deadline has passed. With a foreground operation in flight, sleep is skipped
and tried again at the next tick.

The final drain uses the normal bounded sync and import: sync, import, settle
holds, sync again. It runs through the handler of the transition itself
(`RuntimeQuiescence`), so it does not wait for foreground leases while it holds
the storage lock. Sleep is postponed when the import reports `more`, an
acknowledgement or a sync fails, anything is left pending, or the outbox is not
empty. The host verifies that the worker's outbox is empty after the
acknowledgements and does not infer it from the bytes it imported. The second
sync must report `quiescent` (see [Docs app internals](docs-app.md#host-operations))
and `ready == 0`, and the store must hold no host hold. The drain writes only
what the normal sync and import write. It adds no history of its own and
forces no group boundary early.

After a successful drain core calls `Suspend(true)`: the stopping state commits
under the Manager mutex, the storage lock is released, and one goroutine per
instance (`finishStop`) ends the child. `TickDocs` never waits for the exit, so
a child that does not exit cannot hold up the budget refresh of other documents.
The goroutine keeps the existing graceful then kill behavior, publishes the
instance as sleeping only after the process is confirmed gone, and holds one
child identity and a completion signal. `Stop` joins that owner instead
of terminating the child twice.

When the drain fails, the sleep aborts. Core runs the preparation again (see
"Wake preparation") before `Resume` reopens admission, so admission never opens
on a partly drained child. An aborted sleep with no foreground use keeps the
earlier idle deadline and backs off the next attempt: 30 seconds first, doubling
to a five-minute cap (`30s << min(failures-1, 4)`). Foreground use clears it.
The normal no-wake refresh still runs during the cooldown. This avoids
repeated drains and budget revocations for a persistent backlog or hold.

New foreground demand that meets a quiescence waits at most four seconds, which
are three seconds for a graceful exit and one second for the kill and the reap.
The four seconds start when the request enters `docsDemand`. The wait polls for
the storage lock that the drain holds (`docsWakeLock`), so the bound covers the
drain as well as the exit. A locked caller that calls `Wake` directly gets four
seconds from that call. The request then gets HTTP 503 with `Retry-After: 1`.
The bound is on the caller's latency and not on physical ownership. It cannot
revive the reserved child or cancel a committed sleep. When the exit stays
unconfirmed, the instance stays quiescing and refuses demand after that bound
without starting another child.

`Stop` cancels a preparation and refuses admission. It never waits for
foreground leases, because a WebSocket can live indefinitely. It then ends and
joins the physical child before it releases ownership permanently. It may wait for
a bounded preparation to finish, but only while it holds no core global mutex.
An unconfirmed OS process exit stays a stated shutdown limitation and is never
reported as a successful cleanup. Ordinary maintenance never waits for that
join. A sleep does not reset the crash backoff and does not fabricate a
successful restart.

## Wake

Demand on a sleeping document takes the storage lock, starts a child (reusing
`Manager`'s spawn, with a fresh socket and transport for each spawn, so a stale
lease cannot reach a replacement) and runs the preparation of the next section.
A failed wake counts a failure and sets a retry time with the Manager's restart
backoff (250 ms doubling to 30 seconds; the failure count resets after a child
ran for more than a minute). A request waits for a backoff of up to two seconds
and gets a "retry in" error for a longer one. The wake does not create a save, a
public-visibility approval, a content revision or a deployment (spec rule 7).

## Wake preparation

Every transition into ready (a first wake, a retry after a failure, an aborted
sleep) runs the same steps under the storage lock (`prepareDocsLocked`): revoke
the stored budget with the host-only `budget/revoke` operation, tell the worker
the flat's stored public access level and epoch with `access`, sync, import,
settle holds and install a fresh budget. Revocation sets the budget aside so
that mutation checks fail closed, and it keeps the reservations. It needs an
acknowledgement before any admission. Budgets persist in SQLite, so a fresh
process is not proof that a budget is missing, and an old TTL is never relied
on after a skipped quota update.

The `access` step sends level `""` for a Private flat and the stored level
(`view` or `edit`) and `flats.access_seq` for a Public one, before any request
reaches the child ([Public editing](docs-app.md#public-editing)). Its failure
does not fail the preparation: core records a `visibility` event, and the
worker keeps the level and epoch it last stored. Guest admission compares that
stored pair with the request's headers. If the stored pair is still current,
guests join as before; if the level changed while the document slept, the
stored epoch is older than the headers' and guests cannot join until a later
wake or level change succeeds.

| Outcome | Admission and cleanup |
| --- | --- |
| Sync succeeds, the import is complete and the budget installs | The instance is published ready; reads and mutations are normal. |
| Sync returns `ErrDocumentCapacity` | The child stays ready for reads and explicit recovery (rollback, intent); ordinary mutations keep the existing capacity refusal. A capacity-limited document therefore does not end in a 503 restart loop. |
| The import reports `more`, or fails after a successful or capacity-limited sync and a confirmed revoke (an acknowledgement refused, an invalid entry or hash, a blob write or store error) | The child is published ready under the budget policy. A failure emits a redacted revision event, and a later no-wake tick retries the bounded import. Worker reservations protect the content that is not imported yet. The backlog is marked unresolved, and clean sleep and source or blob sweep are refused until a later drain proves safety. An import failure alone never blocks reads or repair. |
| The budget install fails | Preparation revokes the budget again. If the revoke is acknowledged, the child is published ready for reads and recovery with mutations refused, and an event says that edits remain disabled. A later successful refresh restores mutations. If the revoke fails, preparation fails as in the next row. |
| Spawn fails, sync fails for a reason other than capacity, a revoke is unconfirmed, or the readiness control protocol fails (errors confined to the revision import above excluded) | `Wake` returns the error and publishes nothing. The termination owner (`finishStop`) closes the child and awaits its exit asynchronously, and the instance goes into backoff. The same logical instance does not spawn again before the exit is confirmed. A first-wake failure emits a redacted lifecycle event. |

Preparation decides readiness from its own revoke, sync, import and install
calls, not from the log-only budget error of `refreshDocsLocked`. A failed
install with a confirmed revoke does not block publication; it leaves mutations
refused. Quota propagation that meets a transition may skip the child, because
no normal mutation is admitted until a refresh succeeds. A public caller cannot
forge a recovery mode with a header.

## Callers

| Caller | Policy |
| --- | --- |
| Isolated `checkIsolated` and `trialRun` scratch instances | Eager, never idle-eligible, raw ready dispatch, never take the live storage lock from a handler (a save invokes `healthCheck(d.handler)` while it holds the lock). |
| Visitor docs HTTP and WebSocket | A foreground lease and full wake readiness. Host-served assets stay outside the acquisition wrapper. |
| `GetDocument`, `UpdateDocument`, polling of an uncertain-edit receipt | A foreground locked acquisition that pins the whole operation. No lease is held across a recursive acquisition; an internal lease context or the direct instance is passed instead. |
| Revision restore, save and activation intent, same-shape rollback | A foreground lease across the full existing locked operation. The 12-second restore and uncertain-commit protocols are unchanged. |
| `catchUpDocs`, `ListRevisions`, `GetRevision`, `PinRevision`, `SetRevisionQuotas`, `RevisionQuotas`, history routes | A no-wake refresh through acquire-if-running. They neither wake nor reset the foreground idle time. A sleeping document's history is its durable imported history, and startup recovery may add pending entries on the next real demand. Ordinary listing, settings and provider work do not wake. |
| Approving level `edit` on a Public document | A foreground locked acquisition (`prepareDocsSafePointLocked`) that wakes a sleeper for the safe point and keeps the lease until `access` has applied the new level to the same child. |
| Leaving level `edit`, and other level changes (`applyLevelLocked`) | Acquire-if-running. A sleeping document is left alone and takes the stored level at its next wake. |
| `TickDocs` and quota propagation | Acquire-if-running. An instance without a child or in a transition is skipped and its idle deadline is not refreshed. Awake documents, open WebSocket included, still get the 15-second refresh and quota updates. |
| Startup `startDocsRevisions`, `retainAllRevisions` and blob sweeps | Restoring does not become a mass startup. If no child can be pinned, the worker-dependent recovery, the retention or source deletion and the blob sweep are deferred. The durable outbox, source and hold protections stay until the first wake. Lazy startup does not mean that pending work was imported. |
| `prepareDocsSweepLocked` | Pin if running, or accept the in-memory clean certificate ("Clean sleep and retention"). It returns false for a lazily restored sleeping instance without a certificate, a transitional, incomplete or crashed instance, and an unconfirmed exit. |
| `runningDocs` | Keeps its meaning of a logical route. Each call site makes its own physical acquisition explicit. |

`docsHost` is acquire-only-if-running by default. For a document without a child or
in a transition it returns `errNotRunningDocs` and never implies a wake. A transition
or outer-lease context targets the exact leased child and bypasses a new
acquisition. These callers acquire explicitly with the storage lock held: the
intent of a changed save (it keeps its label and message), a rollback including
its first refresh, the budget push of `UpdateDocument`, `GetDocument`, and the
receipt lookup and recovery. An unchanged save does not wake the document for
its trailing refresh, which is a no-wake refresh; a changed activation is eager
anyway.

`docsIntentLocked` first tries the explicit locked acquisition, so a healthy
sleeper keeps the save label and message. The intent is a best effort for
repairing a broken document: a fatal preparation, a stopped or retired
instance, a backoff or a failed intent does not reject changed content. A
redacted event records that the label and message could not be recorded, and the
existing isolated validation and activation continue. Cancellation of the
caller propagates. If the old logical instance is quiescing, or still ending
after a failed preparation, a changed save waits at most four seconds. If the
child exits in time, the ordinary wake keeps the attribution; if not, the save
continues without its label and records the event. It never waits indefinitely.

The rule of no second worker applies to a restart or wake of the same logical
instance, which cannot spawn again before its child has exited. A distinct
replacement version follows the existing overlap model. It may start a new
child before the commit without issuing a request that affects activation. After
a successful commit and swap it retires the old instance, prepares itself with a
newer ownership-checked generation, and may serve while the retired child
finishes exiting. Generation guards protect the shared data as they do in the
five-second drain, and each old termination keeps its exact owner until the exit
is confirmed. This permits the first changed-save repair attempt after a failed
wake. It is not permission to respawn the old instance.

## Startup, activation and commit

Only restoring documents that are already live starts without a child (`Lazy`). A fresh
or changed save and an isolated activation trial start eagerly and pass the
existing health checks, and startup's `startSavedDocs` still validates saved
documents that never activated. After a successful live database commit and swap,
the new instance goes through the normal preparation and budget, then becomes
idle-eligible. The materialized runtime references and the data ownership stay
attached to the logical instance through a sleep. A wake uses a new permitted
generation, with the same content and environment, and creates no activation
revision unless ordinary recovery finds a pending content change.

Before the durable live-version commit, `buildWithEnvironment` and `Manager.Start`
may allocate and start the new eager child, but no sync, import, `budget/revoke`
or other request that affects activation may reach it. Scratch trial handlers
still validate only their copied data directory. If `CommitPublished` or
`SetLive` fails, the unused new child is stopped and its ownership undone as
before; the old live child stays authoritative and its active hash and
generation do not change.

After the commit succeeds, the new instance is swapped in as not ready and the
old instance is retired for new admission at once. The retirement comes before
the five-second drain. Existing leases may finish, but nothing new may acquire
or start the old instance. Under the same storage lock, preparation runs against
the direct handler of the new child, not through a lookup in `lf.cur` that could
pick the old one. A request that sees the not-ready swap waits through the
existing bounded and cancellable slow path, and a slow acquisition checks the
logical identity again after it takes the lock. Capacity and import backlog
leave the child usable under the budget policy. A fatal preparation ends that
child and leaves the committed logical instance without a child and in backoff,
with an error event, so a later demand retries. The durable commit is not rolled back
and the old instance is not reported as current. The post-commit readiness
failure is reported with the existing deployment and live-data qualification:
the saved content is committed even if the first serving is unavailable. A later
changed save is still a repair path.

Routing and provider handles stay installed through a sleep. A change of
visibility never wakes a document just to reconcile exposure; approving level
`edit` wakes it for its safe point (see "Callers"). A rename, a
replacement or a deletion retires the logical instance and not only its current
process, so a queued request cannot start old content against renamed or deleted
storage. Rename and restart rebuilds are lazy, so a rebuild does not leave a
child resident forever. Existing lifecycle errors remain errors.

## Clean sleep and retention

A document is cleanly asleep when its final sync reports `quiescent` with
`ready == 0` (the definition of `quiescent` is in
[Docs app internals](docs-app.md#host-operations)), the host holds nothing that
needs worker inspection, and the child is confirmed gone. The runtime records the
certificate in `finishStop`, only after that exit, and core reads it through
`CleanDormant`. The certificate says that no process that could create outbox
entries has run for this instance since the verified drain. It lives in memory
only, with a counter epoch in `docsInstance`. A quiescence records a certificate
only if the epoch is unchanged when its child has exited. A wake that starts and
a published child that is found dead clear the certificate. `Retire`, `Stop` and
`InvalidateClean` clear it and advance the epoch. Core calls `InvalidateClean`
before every replacement build, which also defeats a late clean exit of the old
child after a failed commit. A lazily restored document after a process or host
crash has no certificate.

Under the storage lock, `prepareDocsSweepLocked` can use a matching certificate
to continue into the existing keep-set of the current, unpruned and draft
sources and the reference- and hold-aware blob sweep. Revision retention, unpin
and quota changes therefore reclaim the unreferenced blobs of a clean sleeper
without a wake, and the host inspects no SQLite file directly. A restored sleeper
without a certificate keeps deferring the sweep until a demand-driven recovery
and a later clean drain. The sweep step that uses it is in
[Revision store](../revision-store.md#blob-sweep).

## Errors and events

A first-wake failure is a retryable error and an event (spec rule 11). The
Manager logs actual start and preparation failures through its redacting,
rate-limited callback. Core adds no request-level event of its own, so refusals
during a backoff cannot push the real failure out with one event per visitor. All
locked wake callers (document reads, updates and rollback as well as saves) share
the error normalization of `acquireDocsLocked`. With a live caller context, a
worker failure or a refused backoff becomes a deployment-redacted
`ErrUnavailable`; an error of the caller's context stays as it is. Shutdown
returns that sentinel before `Wake` is invoked. A committed activation keeps the
normalized cause, without wrapping or prefixing it twice.

Readiness and wake events leave out the caller's cancellation and ordinary
quiescence timeouts. An unchanged save on a sleeper emits no false revision
warning. The import of a rollback that wake preparation performed first is kept
in an operation-scoped receipt (`docsOpCapture`), so the caller's lookup of the
receipt that preparation already acknowledged still finds its revision. No
global receipt ledger exists. Uncertainty after a rollback reacquires an exact
child before the receipt lookup, the final refresh and the hold settlement.

A readiness failure after a successful live commit keeps the committed files and
returns `DeployError.Committed` together with the existing `ErrUnavailable`.
`publishRevision` keeps the instance and version directory and finalizes the
saved draft even when it returns that error; only a failed commit triggers the
deletion and `Stop` cleanup. The post-commit preparation ignores the caller's
cancellation, remains bounded and is cancelled by `Stop` and `Retire`. Its
error text separates content that is committed and waiting to be served from an
unchanged old document, and its cause holds only deployment-redacted preparation
text. The REST API reports it in the existing `unavailable` category with HTTP
409. Its message says the content was committed, a later request retries
serving, and the caller must not repeat the save or create another flat. The CLI
and the console pass that message on. MCP replaces every generic failure hint for
a committed error, create and save included, with the committed outcome,
`get_logs` and `get_flat` guidance, and the instruction not to repeat. No new API
category exists.

One inherited limitation stays. An ordinary server's `Stop` already waits for a
crash restart that is in progress, and that wait cannot be cancelled. This
feature does not change it. Managed docs cancel their own preparation and clean
up their transition even when a preparation callback panics.

## What survives a sleep

A sleep ends only the child. The host's logical instance, its proxy and
transport objects, the flat state and provider nodes, the routes, the visibility,
the grants and the materialized runtime files all stay, so the memory that a
sleep frees is the child's. `running` in the console and the API keeps meaning a
logical live document, a lazily restored one before its first physical start
included (spec rule 11). The diagnostic worker state shows the physical side.
Core clears the public presence counts that the last `sync` reported whenever
it suspends the child, so a sleeping document reports no guests or viewers.

The host also keeps one pre-started worker while it has any server flat or
document, asleep or not ([Fast worker cold start](../spec/worker-cold-start.md)).
It holds no document, and a sleeping document holds no process of its own.

The six worker state names of this page (`running`, `starting`, `quiescing`,
`sleeping`, `backoff`, `stopped`) are the diagnostic states of
[Metrics internals](metrics.md#worker-state). Code fields keep their own
identifiers (`ready`, `retired`, `transition`, `quiescing`). The state table
merges two things the page tells apart. `Retire` forbids new admission and cancels
a preparation but leaves a ready child to drain, while `Stop` also ends the
child. And `starting` covers both a spawn or wake in progress and a child that
has started and waits for core's preparation.

## Verification

### Testing rules

- Lifecycle tests use deterministic barriers and a fake clock, and the real docs
  host harness (`internal/apps/docs/host_test.go`), not only the Manager's
  JavaScript fixtures.
- A real-document human-group case lets the worker's own `Date.now()` advance
  by at least the 30 seconds of `GROUP_IDLE`, or uses an explicit worker clock
  seam. Advancing the host's fake clock alone is not evidence;
  `TestHostIdlePendingHumanGroupRefusesCleanSleep` waits at least 31 seconds of
  real worker time.
- A test of a completed clean suspension waits for `CleanDormant()` and not for
  the PID to disappear (`waitClean` in `host_idle_test.go`; see the
  [worker playbook](../../.agents/skills/flats-dev-workflow/playbooks/worker-and-runtime.md)).
- Core's fake runtimes in tests keep the eager legacy behavior unless they
  implement the optional managed interface. The real Manager implements it.
- The store-backed core regressions carry no build tag, so ordinary CI package
  runs execute them. Long and population cases are opt-in:
  `FLATS_IDLE_REAL_CLOCK=1` (the real two-minute expiry with group maturity, and
  a WebSocket beyond the budget TTL), `FLATS_IDLE_MEASURE=1` (serial 1, 10 and
  100 document process measurements, and 20 cold-compilation wakes) and
  `FLATS_COLDSTART_BUDGET=1` (the 20-wake cold start budget).
- A change to this area runs targeted race tests for the transitions (the
  managed runtime, the core lock and dispatch) and the existing docs revision,
  activation, edit and runtime stop and restart suites, plus the verification the
  repository prescribes for the changed packages.
- The measurement helper `logIdleMemory` in `host_idle_test.go` lists the host's
  child processes in the phases where every document sleeps and fails if any
  child other than the pre-started worker exists. It also fails if that worker
  (identified by `Manager.SparePID`) was given a flat, and it logs that worker's
  memory separately. The socket count ignores the worker's `s*.sock` socket. The
  helper logs memory and asserts no MiB threshold.

### Acceptance scenarios

The validation plan of the feature was written around these scenarios. Each item
states the outcome that the plan expects. Not every item has a test. The
Coverage table below names the tests that exist, and "Coverage limits when the
feature shipped" lists the scenarios that were never run.

Behavior:

- Five real documents and one ordinary server. After the idle time the document
  processes end and stay absent across several maintenance, quota and retention
  ticks. The ordinary server's process survives, and the provider handler
  identity, URL and visibility stay.
- Reopening a real document page. The exact persisted text, attachments, receipt
  ids and history come back, and a fresh edit succeeds.
- Concurrent first requests. One child and one generation serve them, every
  request succeeds, and a cancelled waiter does not hurt the others.
- Injected initialization failure, budget failure, crash and backoff. No
  unchecked edit is possible and the document stays recoverable.
- An HTTP response, an upload, a WebSocket, a multi-step edit or restore and an
  uncertain receipt operation held across the idle time. Nothing terminates,
  nothing deadlocks, and the idle time starts after the last foreground release.
- A request that arrives before or after a quiescence, and `Stop` racing a start
  or a sleep. None of them deadlocks, and none leaves a second child.
- Durable human edits with pending groups and outbox, a bounded multi-page
  import, a lost acknowledgement, an unresolved restore hold, and a corrupted or
  unavailable source. Sleep and sweep are refused until safe, the revision ids
  and text hashes are the same before and after a wake, and no duplicate
  activation revision appears.
- A startup with pending crash recovery. The sweep is deferred, recovery runs on
  demand, and the bytes are preserved.
- A quota change while asleep. No wake happens, and the first API edit and the
  first WebSocket mutation use the current budget.
- A WebSocket held open beyond the budget TTL. The background refresh allows
  valid edits, and errors fail closed.
- A host restart with saved documents. The routes exist and no docs worker runs
  until demand, and the successful and failed health trials of a new save
  behave as before.
- Rename, delete or replacement during a waiting wake. No stale process comes
  back.
- At least 20 sleep and wake cycles with real documents. Worker processes, file
  descriptors, goroutines and sockets are cleaned up each time, and text and
  receipts stay intact.

Invariants:

- `Service.Close` holds no `s.mu` across `Stop` when shutdown happens while a
  preparation is blocked in `docsHost`.
- Ready requests and WebSocket joins do not wait for a storage lock that
  another operation holds.
- A changed save's scratch health checks do not take the live storage lock, and
  read, edit and receipt calls made under the lock do not acquire it again.
- A capacity-limited document activates, survives a host restart and wakes with
  reads and rollback available and ordinary edits still refused.
- A bounded import backlog leaves the child ready and blocks clean sleep. After
  a budget failure the document is read-only and recoverable. After a fatal
  preparation failure no new child starts before the exact child has exited.
- A stuck child while many documents qualify on one tick. Other documents with
  an open WebSocket still get their budget refresh within the TTL, and `Stop`
  never waits for a WebSocket lease.
- A quota update during quiescing and an aborted drain. A stale quota does not
  permit a mutation.
- History, list, get, pin and quota calls and unchanged saves on sleepers. No
  worker starts and the idle clock does not change.
- A changed save on a healthy sleeper keeps its label and message.
- Retention, unpin and quota sweeps free the blobs of a clean sleeper without a
  wake, and a lazily restored document without a certificate does not sweep
  pending outbox sources.
- A request that reaches the old instance during the five-second replacement
  drain cannot wake it, and the redispatch of a stale request is bounded to one
  retry.
- A freshly ready saved document gets the full two-minute idle interval.
- A request that meets a quiescence returns within its four-second bound and
  does not allow a second child.
- A persistent import hash or blob-write failure after a crash or a lazy
  restore. Reads and explicit recovery remain possible, clean sleep and sweep
  stay blocked, and revision errors are recorded.
- A failed old-worker sync does not block the first changed save, and a save
  that arrives during quiescence continues.
- A failed `CommitPublished` leaves the old active hash and generation
  unchanged, human groups keep recording, and no uncommitted activation
  revision appears.
- A fatal readiness failure after a successful commit leaves the committed
  content and a retryable unavailable state, and a later demand recovers it.
- A kill of the exact child during a preparation import. Readiness is still
  rejected when the final locked liveness check fails, and one owner cleans up.

### Coverage

| Area | Tests |
| --- | --- |
| Runtime ownership (`internal/runtime/docs_lifecycle_test.go`, `usage_test.go`) | `TestManagedDocsLazySingleFlightCancellation`, `TestManagedDocsEagerPreparationLeasesAndIdleClock`, `TestManagedDocsAbortedDrainCooldown`, `TestManagedDocsStopCancelsPreparingAndRetireRejects`, `TestManagedDocsPreparationPanicCleanup`, `TestManagedDocsSuspendWaitsForPhysicalExitAndInvalidatesCertificate`, `TestManagedDocsQuiescenceWaitBoundAndStopWithHeldLease`, `TestManagedDocsStopOwnsUnconsumedQuiescence`, `TestManagedDocsDeadPreparationNeverPublishes`, `TestManagedDocsCrashBackoffUsesUptime`, `TestManagedDocsStopWithOpenWebSocket`, `TestManagedDocsIdleLeavesOrdinaryServerRunning`, `TestManagedDocsBackoffRefusalsDoNotLog`, `TestManagedDocsUsageAndState` |
| Core lock and dispatch (`internal/core/docs_idle_test.go`) | `TestDocsIdleWakeErrorsAreSafe`, `TestDocsIdleLeasePinsFullHandlerAndMaintenanceNeverWakes`, `TestDocsIdleLockWaitHonorsQuiescenceAndCancellation`, `TestDocsIdleImportedReceiptSurvivesPreparationContext`, `TestDocsIdleCloseDoesNotHoldServiceMutexAcrossPreparationStop` |
| Durable save, recovery, concurrency (`internal/core/docs_idle_integration_test.go`, `docs_idle_commit_test.go`) | `TestDocsIdleCommittedFailurePreservesContentAndDormantOperations`, `TestDocsIdleStaleRequestUsesCurrentWrappedHandler`, `TestDocsIdlePreparationRevokesBeforeRecoveryAndBudget`, `TestDocsIdleUncertainRollbackReacquiresAndKeepsImportedReceipt`, `TestDocsIdleDrainRefusesUnresolvedState`, `TestDocsIdleConcurrentDemandStartsOnce`, `TestDocsIdleBackoffRefusalsDoNotFloodEvents`, `TestDocsIdleCommitFailurePreservesOldAuthority` |
| Transport | `internal/api`: `TestDocsIdleCommittedSaveErrorTransport`; `internal/mcpx`: `TestDocsIdleCommittedHintOverridesEveryRefusal` |
| Real docs host (`internal/apps/docs/host_idle*_test.go`) | `TestHostIdleImageModes`, `TestHostIdleRealWorkerCycles`, `TestHostIdleWebSocketPinsWorkerAndRestoreIsLazy`, `TestHostIdlePendingHumanGroupRefusesCleanSleep`, `TestHostIdleWakeFreshMutationsAndQuota`, `TestHostIdleUnavailableBudgetIsReadOnly`, `TestHostIdleRealRollbackLostCommitReply`; opt-in: `TestHostIdleWakeBudget`, `TestHostIdleWebSocketBeyondBudgetTTL`, `TestHostIdleRealClockAndHumanRevision`, `TestHostIdleStoredDocumentScaling`, `TestHostIdleColdCompilationWakeTimings` |

The host tests use real child processes that run the bundled QuickJS, Wasm and
Yjs app, and synthetic local documents. They cover Yjs durability, repeated
sleep and wake cycles with worker exit, a quiet open WebSocket that pins the
worker, a lazy restore of the service, and a real post-commit crash. In the
crash test the worker answers a restore successfully, the test kills that exact
process and substitutes a 503, and recovery then returns the right revision,
the exact files and hashes, no leaked hold and no duplicate revision. Two
cases run on real time: one acknowledges a human update, waits 125 seconds for
both the revision group and the idle threshold, confirms the child exit, reopens
the document and resends the same update id, and one keeps a WebSocket open
beyond the budget TTL with ping and pong and an edit after the TTL.
`TestHostIdlePendingHumanGroupRefusesCleanSleep` proves that the person's
revision survives a sleep. A background tick may import the group before the
final drain, so it does not prove that the drain itself performed that import. A
real clean sleeper also deletes an orphan blob without a wake; the
retention-triggered reclamation is not covered by a host test.

### Measurements when the feature shipped

These numbers are from the candidate of #132, measured on its source commit
`9cf3c88` with an earlier test harness and not rerun on the final test source.
The squash commit of #132 is `59bae8f`; `9cf3c88` is not in the history of
`main`. That the measured product code equals the squashed code is an inference
from matching hashes of part of the files, and no complete source manifest was
recorded at measurement time. The numbers were taken before the pre-started
worker and saved images of #134 (`d74db12`), so they are not a baseline for
current builds. A sleeping document used no process of its own then, and no
pre-started worker existed, so the sleeping rows show the idle floor without that
spare. Current wake times are in
[Fast worker cold start](../spec/worker-cold-start.md#current-state).

Environment: Linux arm64 in a disposable Alpine container (Docker 29.1.3 on
Rancher Desktop), Go 1.27.1 with CGO disabled and cross-compiled test binaries,
2 CPUs and a 2 GiB cap, network disabled, read-only source and binary mounts.
The SQLite driver is pure Go. The baseline is the code before the feature, whose
workers all run eagerly. Synthetic fake-provider workloads are not production
forecasts.

| Saved docs and state | Total cgroup MiB | Parent PSS MiB | Children |
| --- | ---: | ---: | ---: |
| 1, sleeping | 60.02 | 35.98 | 0 |
| 10, sleeping | 59.56 | 28.20 | 0 |
| 100, sleeping | 84.28 | 28.89 | 0 |
| 100, service reopened, sleeping | 96.07 | 40.53 | 0 |
| 100, 1 active | 157.48 | 29.39 | 1 |
| 100, 5 active | 425.77 | 30.31 | 5 |
| 100, 10 active | 736.42 | 27.73 | 10 |

The baseline with one document used 91.49 MiB of cgroup memory with one child
(49.19 MiB PSS), and with ten documents 450.81 MiB with ten children (380.81 MiB
of combined child PSS). Ten sleeping documents used 59.56 MiB with no child, about
86.8 percent less in total than the baseline in this synthetic comparison. The
baseline with 100 documents was not run for lack of resources, and nothing is
extrapolated. The sleeping rows for 1, 10 and 100 documents had 24.44, 16.94 and
17.31 MiB of anonymous memory and 31.57, 34.96 and 53.52 MiB of file memory. Zero
document processes is the invariant; these sequential snapshots do not show a
constant total host memory. The 100-document reopen recreates the service in the
same parent process and Manager, which keeps the parent's heap and cache, so it
is not a fresh host process.

Active workers are not an equivalent-workload comparison. Candidate workers with
an open WebSocket and loaded Yjs measured about 53 to 73 MiB PSS, and baseline
eager workers without a connection 35.2 to 46.7 MiB for ten documents (up to
49.2 MiB including the one-document run). The active total for ten documents,
736.42 MiB against the baseline's 450.81 MiB, shows neither a regression nor an
improvement for equivalent active workloads. The 86.8 percent reduction applies
only to documents that are stored and idle.

| Compilation cache (20 samples) | First page median / p95 / max ms | WebSocket hello after the page median / p95 / max ms |
| --- | --- | --- |
| Warm | 332.69 / 340.78 / 341.41 | 201.06 / 209.60 / 211.61 |
| Cold | 696.49 / 740.63 / 746.69 | 208.99 / 219.09 / 227.29 |

First-page latency includes creating the document's child and the complete page
response, and does not include browser rendering or the editor assets. The
WebSocket time is the extra join after that page and not an independently cold
wake. "Warm" means a new cold process with a warm compilation cache; no
already-running-worker control exists. "Cold" removes only the test-owned cache
after a confirmed worker exit. The candidate's fixture adds a small attachment,
and the baseline uses the existing text fixture. The 200 ms WebSocket hello after
a page is not attributed to the cost of a wake. Twenty synthetic samples are not
an SLA or a production forecast.

### Coverage limits when the feature shipped

This is a frozen record of the validation run of #132. It is not a list of open
tasks, and later tests are not added to it; the coverage table above names the
tests that exist.

The run was Alpine and musl on arm64 only. linux/amd64 and glibc were not tested,
and the documentation checks were not hash-pinned. No production experiment,
hosted CI run, release or install check, or container-image smoke test was
claimed. First-edit latency, an independently cold WebSocket wake and browser
editor-ready latency were not measured. The bounded 20-cycle check asserts the
cleanup of PIDs, file descriptors, goroutines and sockets, but no numeric counts
were kept.

These acceptance scenarios were not run: a capacity-limited activation followed
by a restart, wake and rollback; a startup with pending crash recovery and a
sweep; a persistent import hash or blob-write failure after a real lazy restore;
a save during idle quiescence; a stuck-child stop while other documents refresh in
the same tick; the complete idle-clock census of get, pin, quota, history and
unchanged-save calls; retention or unpin that physically frees blobs; the full
provider-identity scenario of the validation plan (an ordinary server on the same
Manager was covered); a long-run plateau of descriptors, goroutines and sockets
beyond the bounded cycle test; continued real human-group recording after a
`CommitPublished` failure; and a healthy sleeper that keeps the label and message
of a changed save.
