# Metrics internals

How host metrics and per-flat usage are measured, stored and exposed. The
product rules ("spec rule N" below) are in [Metrics](../spec/metrics.md); the
operator-facing summary is in [design.md](../design.md#metrics) and the routes
in [surfaces.md](../surfaces.md).

## Overview

```
request ─► siteHandler ─► usageWriter ─► flat handler
                │            (status, bytes, start time)
                └─► liveFlat.usage (in memory)
                       ├─ cumulative counters + histogram ───────► /metrics
                       └─ per-day buffer ──Sweep (30 s)──► flats.db usagedays

flats worker ──stderr {"t":"usage"} every 15 s──► instance ──► UsageSink
                                                  (cmd.Wait: final CPU)   │
                                         liveFlat.usage (live) ◄──────────┤
                                         host role counters (preview, check)

Sweep ──► hostSnapshot: flats by type/visibility/running, endpoint states,
          pending approvals, flats.db size; disk walk every 5 min ──► /metrics
```

The scrape path reads only atomics, the usage sinks under their own short
locks, and the last `hostSnapshot`. It never touches SQLite, the file
system, workers or provider backends (spec rule 4).

## Library

`github.com/prometheus/client_golang` (v1.24.1) was already linked into
`flats` through `github.com/oesni/libtunnel` before Flats used it directly.
`go.mod` requires it and `prometheus/common` (the tests parse the exposition
with `expfmt`) directly, at the versions the module graph already selected,
so adding metrics changed no module version. The nested `netbird/go.mod` that
builds the release binary keeps them `// indirect`, since no package in that
module imports them and `go mod tidy` would restore the marker.

Flats uses a private `prometheus.Registry`. It never uses `promauto`,
`promhttp.Handler()` or `prometheus.DefaultRegisterer`, which libtunnel
replaces during startup (`libtunnel/v1alpha1/cloudflare/cloudflare.go`), so
no library metric is exposed (spec rule 8). The registry holds
`collectors.NewGoCollector()`, `collectors.NewProcessCollector()` and one
custom `prometheus.Collector` that reads the current live flats at collect
time, so per-flat series appear, follow renames and disappear without
unregistering anything.

Release builds are CGO-free (`scripts/build-release.sh`). In that mode the
process collector reports no resident memory on macOS
(`process_collector_mem_nocgo_darwin.go`); the Go runtime series still show
the host's Go memory. `docs/design.md` states the gap.

## Request accounting

`siteHandler` (`internal/core/service.go`) wraps the `ResponseWriter` before
its first check, so the public-epoch 503, the rate-limit 429 and the "not
deployed" 503 are counted like handler answers. `previewHandler` is not
wrapped (spec rule 9).

`usageWriter`:

- records the status when it is first sent: on `WriteHeader`, on the first
  `Write` (implicit 200), or on a successful `Hijack` (101). A handler that
  returns without writing anything is counted as 200 when it returns. The
  request's counters and day are taken at that moment, so a WebSocket open
  for days counts on the day it opened (spec rule 9);
- counts response body bytes written through it;
- implements `Unwrap()`, so `http.ResponseController` reaches the underlying
  writer. The server-flat reverse proxy and the design handler use
  `ResponseController` for upgrades, flushes and deadlines
  (`internal/runtime/manager.go`, `internal/design/handler.go`).
  `Flush()` and `Hijack()` delegate through a `ResponseController` on the
  inner writer; the design handler type-asserts `http.Flusher` for its event
  stream, and every writer Flats serves through (net/http HTTP/1 and HTTP/2)
  supports flushing.

A request is **streaming** when it is hijacked or when its response has
`Content-Type: text/event-stream`. Streaming requests are counted with their
status (101, or the event stream's status), raise the per-flat open-streams
gauge until the handler returns, and are not observed in the duration
histogram (spec rule 11). HTTP/2 extended `CONNECT` WebSockets need no branch:
Go 1.27 keeps them disabled unless `GODEBUG=http2xconnect=1` is set, and
Flats sets no such option; a test pins that.

Closed requests (spec rule 10) are not observed in the duration histogram.
Since only the handler's return tells whether a `502` or `503` becomes
`closed`, those two statuses are counted when the handler returns; every other
status is counted when it is sent. The wrapper keeps the request's original
context when it is entered, before the public route replaces it with an
epoch-derived one (`siteHandler`), so a route revocation is never mistaken for
a departed visitor. A request that sent no status is counted as `closed`
instead of its status class when its original context ended with
`context.Canceled` by the time the handler returns. A `502` or `503` is
`closed` when the original context had already been cancelled when that
status was sent, which the wrapper records at that moment; a visitor who
leaves later, while the body is being sent, leaves it an error (spec rule 10).
That covers the runtime
proxy's `ErrorHandler` 502, the 503 of a cancelled `ensure` in
`instance.ServeHTTP` and the 503 that `docsDemand` writes when a visitor stops
waiting for a wake. A panic with `http.ErrAbortHandler` (the reverse proxy
aborting a body copy to a departed visitor) keeps the status already sent and
is re-raised. Any other panic counts as `5xx` if no status was sent and is
re-raised.

Per live flat, `flatUsage` holds:

- cumulative atomic counters: requests by route kind (`private`, `public`) ×
  class (`1xx`…`5xx`, `closed`), rate-limited requests, response bytes, open
  streams;
- a cumulative duration histogram with fixed buckets in seconds: 0.005, 0.01,
  0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 11, 30, `+Inf`. The 11 s bound
  sits just above the 10 s runtime deadline, so requests ending at that
  deadline fall in (10, 11] instead of (10, 30], where they would inflate
  p95;
- a day buffer: a map from UTC day to the same counts, the bucket counts,
  the duration sum in milliseconds, the CPU increase and the peak memory.
  Requests are added under the day their status was sent; a CPU increase and
  a memory sample go to the day of the sweep that saw them. (The page-view
  flush stamps the day at flush time; usage does not copy that.) CPU moves
  into the buffer in whole milliseconds, and a memory peak only when it
  exceeds the highest peak this `flatUsage` already recorded for that UTC
  day, so a worker with steady memory and less than 1 ms of new CPU adds
  nothing and its flat writes no row. After a host restart the day's peak may
  be written once more.

`flatUsage` hangs off `liveFlat`, which renames keep, so counters continue
under the new slug (spec rule 7). `liveFlat` also records the flat's `uid`
(`store.Flat.ID`) the first time it is bound to a stored flat; usage is
flushed under that uid, never under a uid looked up from the slug at flush
time. The day buffer has its own mutex, held only to add or to swap the
buffer out. `Sweep` swaps out the buffers of every live flat and writes them
in one transaction (`store.AddUsageBatch`), so a sweep makes one usage write
however many flats run; if it fails, every buffer is merged back, so nothing
is lost or counted twice. `Service.Close` flushes the same way. The request
path never waits for SQLite (spec rule 15).

Reading a flat's stats never writes: `Service.Usage` reads the stored rows
and adds a read-only copy of the flat's day buffer plus the CPU and memory
peak not captured yet, without moving them. It never binds a `liveFlat`
either: the slug may have been deleted and created again between its lookup
of the flat and of `s.live`, and the uid is bound only once. Only a state
already bound to the flat's uid adds its memory; an unbound state (bound by
its activation or the next sweep) and a state bound to another uid add
nothing, and the report comes from the stored rows alone. A Service-level mutex
(`usageMu`) makes that copy and the read one step for the flush, so a
concurrent sweep is seen either entirely in SQLite or entirely in memory.
Only the flush and stats reads take it; the request path does not.

## Worker CPU and memory

### Samples

Workers already send JSON control lines (`childMsg`) on stderr; control
messages are never rate limited (`internal/runtime/protocol.go`). Flat code
cannot write raw stderr lines: QuickJS console output and WASI stderr reach
the parent only inside the `msg` field of `"log"` lines
(`internal/qjs/options.go`, `internal/runtime/wasi.go`). The type
`"usage"` carries:

```json
{"t":"usage","cpu_ns":1234567890,"mem":52428800}
```

- `cpu_ns`: user + system CPU time of the worker since it started, from
  `syscall.Getrusage(RUSAGE_SELF)` (Linux and macOS, the release targets).
- `mem` (Linux only, omitted elsewhere): `RssAnon` from `/proc/self/status`,
  the resident private anonymous memory. It includes everything the worker
  maps privately: the Go heap and stacks, wazero linear memory (Go heap when
  no allocator is configured), wazero's compiled code and the modernc
  SQLite allocations (`modernc.org/memory` maps them outside the Go runtime).
  It excludes the shared program text (spec rule 12). macOS has no CGO-free
  equivalent; the API reports memory as not measured there.

The worker sends a sample right after `ready` and then every 15 seconds. The
parent validates each one (non-negative, `cpu_ns` not decreasing within one
process); an invalid sample is dropped and logged once per process.

A flat is charged only for CPU its worker used after receiving its spec. The
worker's `"spec"` acknowledgement carries the CPU used so far as `cpu_ns`: for
a pre-started worker ([Fast worker cold start](../spec/worker-cold-start.md))
that is its warm-up, and for an exec'd worker its process start. The parent
subtracts it from every sample and from the final CPU at exit. A worker
adopted before its warm-up finished completes it after the acknowledgement,
and that remainder is the flat's, as the same engine setup is in an exec'd
worker's startup.

### Attribution

`runtime.Manager` holds no per-flat state: instances are owned by core.
Core therefore passes a sink in the spec. `build` and `buildWithEnvironment`
(`internal/core/service.go`) take an explicit `usage UsageSink` argument,
since their callers otherwise differ only by data directory:

```go
// RuntimeSpec
Usage UsageSink // nil: not accounted
```

```go
type UsageSink interface {
	Started(id uint64)                                 // a worker process of this sink started
	Sample(id uint64, cpuNS int64, mem int64, memOK bool) // latest sample of that process
	Exited(id uint64, cpuNS int64, unexpected bool)
}
```

- Live builds pass the `usage` of the `liveFlat` they already hold:
  activation and restore (`service.go`, `lifecycle.go`), host restore at
  startup, and both rename paths (`rename.go`), which hold `lf` while it is
  out of `s.live`. A rename must not look the flat up by slug during the
  rebuild: `delete(s.live, from)` precedes it, so a lookup would create a
  stray `liveFlat`. This covers the live instance, docs wakes and restarts,
  the rebuilt instance after a rename and an old version still draining after
  a switch. A live build whose activation then fails keeps its CPU on the
  flat's live usage.
- Previews (preview opening in `service.go` and the preview paths in
  `lifecycle.go`) pass the host `preview` role sink; activation trials
  (`checkIsolated` in `lifecycle.go`) and the restore check pass the `check`
  role sink (spec rule 12). Role sinks only keep a CPU total per flat and
  role, for host metrics. `TestUsageSinkOfEveryBuild` asserts the sink of
  each call site.
- Each process gets a manager-wide `id` when it starts. `Started` is called
  once an instance owns the process: after `cmd.Start` for a process started
  for the instance, or when a spawn adopts a pre-started worker (under the
  process's owner lock, which also records that the process was waited for:
  a worker that exits while being adopted is not given the instance, so
  every `Started` is followed by exactly one `Exited`). A pre-started worker
  that exits on its own before acknowledging an adopted spec contributes no
  CPU and is not an unexpected exit; the spawn then starts a new process. A
  start the host gives up on (an error, the start timeout) is unexpected in
  either kind of worker.
- Exit accounting runs in the `cmd.Wait` goroutine that every spawned process
  already has (`manager.go`), after `cmd.Wait` returns and **before**
  `close(p.done)`, not in `exited()`, which returns early for superseded or
  stopped processes. `Stop` returns on `<-p.done`, so once `Stop` returns the
  final CPU is in the sink; shutdown relies on that. The final CPU is
  `ProcessState.UserTime() + SystemTime()`, or the last sample when
  `ProcessState` is nil. `unexpected` is true when the process was not
  asked to stop, and also when the host killed it because its startup failed
  (an error reported before ready, the start timeout, or a failed spec
  write); a start cancelled by its caller is not unexpected.
- The sink replaces the process's latest sample with its final CPU in one
  critical section: the exiting process leaves the running set and its final
  CPU joins `exitedCPU` under the same lock the readers take. A flat's CPU
  total is `exitedCPU` plus the latest sample of each running process, so a
  reader sees each process exactly once and the total never decreases.
  A daily CPU increase below zero is clamped to zero.
- Memory is the sum of the latest `mem` of running processes; with none
  running it is 0. Each process keeps the time of its latest sample with
  `mem` separately from its CPU: a sample without `mem` (for example a failed
  read of `/proc/self/status`) updates the CPU only and leaves the previous
  `mem` and its time, and a process that has sent no `mem` yet contributes
  nothing and counts its start as its sample time. The oldest of those times
  is the flat's memory sample time; one older than 60 seconds is reported
  with its time and flagged stale (spec rule 15).

### Worker state

The state lives in runtime: `instance` (process, failures, backoff) and
`docsInstance` (`internal/runtime/docs_lifecycle.go`). Both implement the
optional `core.StateReporter`; core's wrappers `runtimeInstance` and
`managedDocsRuntime` forward it, since they promote only `ServeHTTP` and
`Stop`. It reads state under the instance's lock without waking or messaging
the child. The state shown for a flat is that of `lf.cur`'s instance, not of
an old version still draining. A docs wake is not a restart and is not an
unexpected exit. [Idle document workers](../spec/docs-worker-idle.md) rule 11 allows
this diagnostic state while the console and API `running` keep their logical
meaning (spec rule 13).

| State | Meaning | Console label |
| --- | --- | --- |
| `running` | a ready worker | Running |
| `starting` | spawning or waking | Starting |
| `quiescing` | draining before an idle sleep | Going to sleep |
| `sleeping` | a docs instance with no child | Sleeping |
| `backoff` | waiting to restart after a failure | Restarting |
| `stopped` | stopped or retired, including a stop that set `quiescing` | Stopped |

## Host snapshot

`Sweep` (every 30 s) refreshes `hostSnapshot`:

- every flat's slug, uid, visibility and type: the store query
  `FlatTypes` returning them in one read, with the type taken as `view`
  takes it (live version, else latest version, else Draft). Sweep never
  calls `Service.ListFlats` or `view`, which walk disks and call
  `ExposureStatus` with its permission callback. Joined with `s.live` for the
  running state;
- pending approvals (`SELECT COUNT(*)`);
- the size of `flats.db` plus `flats.db-wal`;
- the sweep's own completion time, exported as
  `flats_snapshot_timestamp_seconds`, since a slow sweep delays the next one
  (spec rule 4).

At scrape time the collector takes a flat's `type` from `lf.cur` for a
running flat and from the snapshot otherwise. Disk series for flats without a
`liveFlat` come from the snapshot alone. The collector emits per-flat series only for
flats in the last snapshot or with a `liveFlat` that has a bound uid and a
current version: `s.state()` can create an empty `liveFlat` for any slug, and
those never become series. A flat whose state exists but is not bound to its
uid yet (for example after a request before its first activation) shows no
disk series until the next sweep binds it, rather than the snapshot's uid,
which may belong to a deleted flat of the same slug. During a rename the flat is briefly out of
`s.live`; `Service.Metrics` then reports it with `Traffic: false` and the
collector exposes only its disk series, so no counter appears to reset. Its
traffic series return under the new slug with the same counters.

Disk usage: `checkQuotas` already walks each live flat every sweep when the
quota is on, and records the result and its time. A walk that could not read
part of the flat (an unreadable subdirectory) records nothing, so the last
figure keeps its value and time instead of a partial total appearing fresh
(spec rule 15); the quota check itself still uses the partial total, as
before. A missing flat directory, and files removed during the walk, hold
nothing and are not errors. Independently,
every flat in the store, live or not, whose figure is missing or at least 4.5
minutes old is walked, oldest first, up to about a ninth of all flats per
sweep (a failed walk counts as an attempt for this schedule, so an unreadable
flat waits its turn instead of taking every sweep's walks), so disk figures exist for every flat and are about 5 minutes old at
most (spec rule 4). This adds about one walk per flat per 5 minutes on hosts
with the quota off. The flat view's `disk_bytes` keeps measuring on
read; the console keeps showing that figure.

Route endpoints by provider and state are not part of the core snapshot.
`provider.Manager.EndpointStates()` computes, for every observed endpoint
in `m.states`, the same fresh state that `ExposureStatus` computes from each
backend's in-memory `Status()` (shared code, `refreshed()`), without the
permission callback (which can read SQLite). The stored `m.states` state alone
is not used: it is written only when a route opens. `internal/metrics` calls
it once at start and then every 30 seconds from its own loop and caches the
counts; a scrape reads the cache. Unknown states are reported as `other`.

## Management server accounting

The management listener's handler (`loopbackGuard(addr, mux, externalOrigins)`
in `internal/app/serve.go`, including declared external origins) and each console domain's handler (`consoleHandler`
in `internal/app/consoledomains.go`) are wrapped outside their guards, so
refused requests are counted too. The middleware labels each request by
surface: `api` (`/api/`), `console` (`/console/api/`), `mcp` (`/mcp`),
`metrics` (`/metrics`), `other`, and counts requests by status class with a
`statusWriter` that records the sent status like `usageWriter` and keeps
`Unwrap`, `Flush`, `Hijack` (the MCP SDK uses `ResponseController` for its
streams) and `ReadFrom`, so files the management server serves keep sendfile.
A panic counts as `5xx` when no status was sent and is re-raised unchanged; a
panic with `http.ErrAbortHandler` (an aborted response, such as a departed
client) keeps the status already sent and is not counted when none was, since
management requests have no `closed` class. There is no management duration
histogram.

## `/metrics`

Registered on the management mux as `/metrics` (not `GET /metrics`, which
would send other methods to the console's `/` handler) with
`promhttp.HandlerFor` over the private registry (`EnableOpenMetrics: true`;
promhttp negotiates the format and gzip). The handler serves `GET` and `HEAD`
and answers other methods 405 with `Allow: GET, HEAD`. It is reached
through the same host guards as the rest of the mux and applies the `/api`
browser rule from `agentRequest` (`internal/api/guard.go`): `Sec-Fetch-Site: cross-site` or `same-site`, or an
`Origin` other than the requested host, is 403. The console page does not
answer `/metrics`.

Metric names (per-flat series carry `flat` = slug and `type`):

| Metric | Type | Extra labels |
| --- | --- | --- |
| `flats_build_info` | gauge (1) | `version`, `commit` |
| `flats_db_bytes` | gauge | |
| `flats_flats` | gauge | `type`, `visibility`, `running` |
| `flats_requests_total` | counter | `route`, `code` (`1xx`…`5xx`, `closed`) |
| `flats_rate_limited_total` | counter | |
| `flats_response_bytes_total` | counter | |
| `flats_open_streams` | gauge | |
| `flats_request_duration_seconds` | histogram | |
| `flats_worker_cpu_seconds_total` | counter | `role` (`live`, `preview`, `check`) |
| `flats_worker_memory_bytes` | gauge | (Linux) |
| `flats_worker_memory_sampled_timestamp_seconds` | gauge | (Linux) |
| `flats_worker_state` | gauge (1) | `state` |
| `flats_worker_unexpected_exits_total` | counter | |
| `flats_disk_bytes` | gauge | |
| `flats_disk_measured_timestamp_seconds` | gauge | |
| `flats_route_endpoints` | gauge (host) | `provider`, `state` |
| `flats_pending_approvals` | gauge (host) | |
| `flats_snapshot_timestamp_seconds` | gauge (host) | |
| `flats_management_requests_total` | counter (host) | `surface`, `code` |

plus `go_*` and `process_*` of the host process. Label values are slugs and
the fixed type, route, code, role, state, provider and surface enums, and the
build strings; nothing else reaches a label (spec rule 6). A host with 100
flats exposes roughly 2500 series.

## Storage

Migration 17 in `internal/store/migrate.go` creates the table:

```sql
CREATE TABLE IF NOT EXISTS usagedays (
  flat_uid TEXT NOT NULL,
  day TEXT NOT NULL,
  requests INTEGER NOT NULL DEFAULT 0,
  c1xx INTEGER NOT NULL DEFAULT 0,
  c2xx INTEGER NOT NULL DEFAULT 0,
  c3xx INTEGER NOT NULL DEFAULT 0,
  c4xx INTEGER NOT NULL DEFAULT 0,
  c5xx INTEGER NOT NULL DEFAULT 0,
  closed INTEGER NOT NULL DEFAULT 0,
  rate_limited INTEGER NOT NULL DEFAULT 0,
  timed INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  latency TEXT NOT NULL DEFAULT '',
  cpu_ms INTEGER NOT NULL DEFAULT 0,
  memory_peak INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (flat_uid, day)
);
```

- Rows are keyed by the flat's `uid` (`store.Flat.ID`), which renames keep
  and a deleted and recreated slug does not. The flush writes under the uid
  bound on `liveFlat` and only `WHERE EXISTS (SELECT 1 FROM flats WHERE
  uid=?)`, so a late flush after a delete is discarded instead of attaching
  old usage to a new flat with the same slug. `DeleteFlat` deletes the usage
  rows by uid in the same transaction, before the `flats` row.
- `timed` is the number of requests in the histogram. `latency` is
  `{"v":2,"b":[14 counts]}`; the version names the bucket bounds (version 1
  had 13 counts, without the 11 s bound), and percentiles sum only rows of the
  current version. When a day's stored row and a flush differ in version, the
  newer one wins: its buckets, `timed` and `duration_ms` replace the older
  ones for that day, and a newer write is never dropped because the stored
  row is older. A mid-day upgrade therefore keeps the rest of that day's
  response times and drops the earlier part of the day's.
- The flush reads each day's row, adds counts, takes `MAX` of
  `memory_peak`, merges bucket counts in Go and writes it back. One
  transaction holds every flat of a sweep; the existence check runs per uid
  inside it, and rows of a uid with no flat are discarded.
- One row per flat per day with traffic or CPU; rows are kept until the flat
  is deleted, like page views. `docs/configuration.md` lists the table among
  the `flats.db` contents.

Shutdown: `Service.Close` flushes page views before it stops workers, but
usage only after all workers have stopped and their exit accounting has
run, so the last CPU reaches `flats.db` (spec rule 15).

## Stats API

`GET /flats/{slug}/stats` (both prefixes) adds a `usage` object; existing
fields are unchanged:

```json
{
  "usage": {
    "days": [{"day": "2026-10-18", "requests": 120,
              "status": {"2xx": 110, "4xx": 7, "5xx": 2, "closed": 1},
              "rate_limited": 0, "cpu_seconds": 4.2, "memory_peak_bytes": 52428800}],
    "totals": {"requests": 120, "errors": 2, "error_rate": 0.0167,
               "p50_ms": 25, "p95_ms": 250, "cpu_seconds": 4.2,
               "memory_peak_bytes": 52428800},
    "now": {"cpu": "measured", "memory": "measured", "state": "running",
            "workers": 1, "memory_bytes": 50331648,
            "memory_sampled_at": "2026-10-18T09:00:00Z", "memory_stale": false}
  }
}
```

- `now.cpu` and `now.memory` are `measured`, `not_separate` (websites, pages,
  designs: the CPU, memory, `state` and `workers` fields are omitted) or
  `unsupported` (memory on non-Linux hosts).
- Percentiles are interpolated within a bucket from the summed bucket
  counts, as `histogram_quantile` does, and are `null` when no request was
  timed. They use only rows of the current latency version, so days recorded
  before an upgrade of the buckets count their requests but give no
  percentiles. `error_rate` is `null` with zero requests and counts `5xx`
  only.
- `days` covers the same `?days=` window as `page_views`; days without
  usage are omitted. The figures include usage not yet flushed, read from
  memory.
- If usage cannot be read, `stats` still returns `page_views` and
  `top_pages` and omits `usage`; the console then hides its Usage section.

## Console

Analytics (`internal/console/static/assets/js/analytics.js`) shows a
**Usage** section under Page Views, from the same `stats` response:

- tiles for the period: Requests, Error rate, Median and p95 response time,
  CPU time, Peak memory;
- a "now" line: worker state (labels in the table above) and current memory,
  with the sample age when stale;
- a daily requests chart stacked by status class.

For websites, pages and designs the CPU and memory tiles are replaced by one
note: "CPU and memory are not measured separately for this type; it runs
inside the Flats process." On a non-Linux host the memory tiles say "Memory
is not measured on this system", and the stats API omits
`memory_peak_bytes`. The Storage tile is the flat view's `disk_bytes`.
[surfaces.md](../surfaces.md) lists the Analytics labels.
