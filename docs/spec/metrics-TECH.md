# Metrics: technical design

Technical design for [Metrics](metrics.md). It moves into the architecture
docs when the feature ships.

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

`github.com/prometheus/client_golang` v1.24.1 is already in the module graph
and linked into `flats` through `github.com/oesni/libtunnel` (`go list -deps
./cmd/flats` lists `prometheus` and `promhttp`). Using it directly turns the
`// indirect` requirement into a direct one at the same version, in `go.mod`
and in the nested `netbird/go.mod` that builds the release binary; module
selection does not change. The new `prometheus/collectors` package and Flats'
own code are new in the binary.

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

Closed requests (spec rule 10): the wrapper keeps the request's original
context when it is entered, before the public route replaces it with an
epoch-derived one (`siteHandler`), so a route revocation is never mistaken for
a departed visitor. When the handler returns, a request whose original context
ended with `context.Canceled` before its response was complete, and that sent
no status or a `502` or `503`, is counted as `closed` instead of its status
class; a 502 or 503 that was fully sent stays an error. That covers the runtime
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
  0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, `+Inf`;
- a day buffer: a map from UTC day to the same counts, the bucket counts,
  the duration sum in milliseconds, the CPU increase and the peak memory.
  Requests are added under the day their status was sent; a CPU increase and
  a memory sample go to the day of the sweep that saw them. (The page-view
  flush stamps the day at flush time; usage does not copy that.)

`flatUsage` hangs off `liveFlat`, which renames keep, so counters continue
under the new slug (spec rule 7). `liveFlat` also records the flat's `uid`
(`store.Flat.ID`) the first time it is bound to a stored flat; usage is
flushed under that uid, never under a uid looked up from the slug at flush
time. The day buffer has its own mutex, held only to add or to swap the
buffer out. `Sweep` swaps it out, writes it, and merges it back on failure, as
`flushViews` does for page views; the request path never waits for SQLite
(spec rule 15). Reading a flat's stats flushes that flat first, as `TopPages`
does.

## Worker CPU and memory

### Samples

Workers already send JSON control lines (`childMsg`) on stderr; control
messages are never rate limited (`internal/runtime/protocol.go`). Flat code
cannot write raw stderr lines: QuickJS console output and WASI stderr reach
the parent only inside the `msg` field of `"log"` lines
(`internal/qjs/options.go`, `internal/runtime/wasi.go`). A new type
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

### Attribution

`runtime.Manager` holds no per-flat state: instances are owned by core.
Core therefore passes a sink in the spec. `build` and `buildWithEnvironment`
(`internal/core/service.go`) gain an explicit `usage UsageSink` argument,
since their callers differ today only by data directory:

```go
// RuntimeSpec
Usage UsageSink // nil: not accounted
```

```go
type UsageSink interface {
	Started(id uint64)                  // a worker process of this sink started
	Sample(id uint64, cpuNS, mem int64) // latest sample of that process
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
  role, for host metrics. The PR lists every call site with its sink, and a
  test asserts each one's sink.
- Each process gets a manager-wide `id` at spawn. `Started` is called after
  `cmd.Start` succeeds.
- Exit accounting runs in the `cmd.Wait` goroutine that every spawned process
  already has (`manager.go`), after `cmd.Wait` returns and **before**
  `close(p.done)`, not in `exited()`, which returns early for superseded or
  stopped processes. `Stop` returns on `<-p.done`, so once `Stop` returns the
  final CPU is in the sink; shutdown relies on that. The final CPU is
  `ProcessState.UserTime() + SystemTime()`, or the last sample when
  `ProcessState` is nil. `unexpected` is true when the process was not asked
  to stop.
- The sink replaces the process's latest sample with its final CPU in one
  critical section: the exiting process leaves the running set and its final
  CPU joins `exitedCPU` under the same lock the readers take. A flat's CPU
  total is `exitedCPU` plus the latest sample of each running process, so a
  reader sees each process exactly once and the total never decreases.
  A daily CPU increase below zero is clamped to zero.
- Memory is the sum of the latest `mem` of running processes; with none
  running it is 0. A latest sample older than 60 seconds is reported with its
  time and flagged stale (spec rule 15).

### Worker state

The state lives in runtime: `instance` (process, failures, backoff) and
`docsInstance` (`internal/runtime/docs_lifecycle.go`). Both implement a new
optional `core.StateReporter`; core's wrappers `runtimeInstance` and
`managedDocsRuntime` forward it, since they promote only `ServeHTTP` and
`Stop`. It reads state under the instance's lock without waking or messaging
the child. The state shown for a flat is that of `lf.cur`'s instance, not of
an old version still draining. A docs wake is not a restart and is not an
unexpected exit. [Idle document workers](docs-worker-idle.md) rule 11 allows
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

- every flat's slug, uid, visibility and type: a new store query
  (`FlatTypes`) returning them in one read, with the type taken as `view`
  takes it (live version, else latest version, else Draft). Sweep never
  calls `Service.ListFlats` or `view`, which walk disks and call
  `ExposureStatus` with its permission callback. Joined with `s.live` for the
  running state;
- route endpoints by provider and state: a new
  `provider.Manager.EndpointStates()` computes, for every observed endpoint in
  `m.states`, the same fresh state that `ExposureStatus` computes from each
  backend's in-memory `Status()`, without the permission callback (which can
  read SQLite). The stored `m.states` state alone is not used: it is written
  only when a route opens;
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
those never become series. During a rename the flat is briefly out of
`s.live`; a scrape in that window may miss its traffic series, which return
under the new slug with the same counters.

Disk usage: `checkQuotas` already walks each live flat every sweep when the
quota is on; it now also records the result and its time. Independently,
every flat in the store, live or not, is walked at most once every 5
minutes, spread over sweeps, so disk figures exist for every flat and are at
most 5 minutes old (spec rule 4). This adds one walk per flat per 5 minutes on
hosts with the quota off. The flat view's `disk_bytes` keeps measuring on
read; the console keeps showing that figure.

## Management server accounting

The loopback listener's handler (`loopbackGuard(mux)` in
`internal/app/serve.go`) and each console domain's handler (`consoleHandler`
in `internal/app/consoledomains.go`) are wrapped outside their guards, so
refused requests are counted too. The middleware labels each request by
surface: `api` (`/api/`), `console` (`/console/api/`), `mcp` (`/mcp`),
`metrics` (`/metrics`), `other`, and counts requests by status class with the
same `usageWriter` (the MCP SDK uses `ResponseController` for its streams).
There is no management duration histogram.

## `/metrics`

Registered on the management mux as `GET /metrics` with `promhttp.HandlerFor`
over the private registry (`EnableOpenMetrics: true`; promhttp negotiates the
format and gzip). It is reached through the same host guards as the rest of
the mux and applies the `/api` browser rule from `agentRequest`
(`internal/api/guard.go`): `Sec-Fetch-Site: cross-site` or `same-site`, or an
`Origin` other than the requested host, is 403. Other methods are 405. The
console page no longer answers `/metrics`.

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

New table, created by the next migration in `internal/store/migrate.go`:

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
  `{"v":1,"b":[13 counts]}`; the version names the bucket bounds, so the
  bounds can change later and percentiles sum only rows of one version.
- The flush reads the day's row, adds counts, takes `MAX` of
  `memory_peak`, adds bucket counts in Go and writes it back, in one
  transaction per flat.
- One row per flat per day with traffic or CPU; rows are kept until the flat
  is deleted, like page views. `docs/configuration.md` lists the table among
  the `flats.db` contents.

Shutdown: `Service.Close` today flushes page views before it stops workers.
Usage is flushed after all workers have stopped and their exit accounting has
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
  timed. `error_rate` is `null` with zero requests and counts `5xx` only.
- `days` covers the same `?days=` window as `page_views`; days without
  usage are omitted.

## Console

Analytics (`internal/console/static/assets/js/analytics.js`) gains a
**Usage** section under Page Views, from the same `stats` response:

- tiles for the period: Requests, Error rate, Median and p95 response time,
  CPU time, Peak memory;
- a "now" line: worker state (labels in the table above) and current memory, with the sample age when stale;
- a daily requests chart stacked by status class.

For websites, pages and designs the CPU and memory tiles are replaced by one
note: "CPU and memory are not measured separately for this type; it runs
inside the Flats process." On a non-Linux host the memory tiles say "Memory
is not measured on this system." The existing Storage tile is unchanged.
`docs/surfaces.md` updates the Analytics labels ("Page Views is the only
traffic metric" no longer holds).

## Documentation

- `docs/spec/docs-worker-idle.md`: rule 11 allows the diagnostic worker state.
- `docs/surfaces.md`: `/metrics` in the surface table and request guards; the
  `stats` response; Analytics labels.
- `docs/design.md`: Analytics covers usage; a Metrics subsection with the
  metric table, the Linux-only memory and the macOS host memory gap.
- `docs/configuration.md`: `usagedays` in the `flats.db` contents.
- `docs/networking.md`: a short note on scraping through a console domain.
- `docs/agent/`: the stats route if the guide lists it; regenerate the
  references.

## Testing

- `usageWriter`: status capture at `WriteHeader`, first `Write` and `Hijack`;
  implicit 200; bytes; `ResponseController` Flush/Hijack/deadlines through
  `Unwrap`; WebSocket upgrade through the server-flat proxy and the design
  instance; event stream excluded from timing; `ErrAbortHandler` keeps the
  sent status; other panics count `5xx`; a cancelled visitor counts `closed`,
  including the proxy's 502.
- `siteHandler`: 429 and both 503 paths counted; previews not counted.
- Usage flush: a blocked SQLite write does not block a concurrent request
  (the request completes while the flush waits); a failed flush is merged
  back and written once by the next sweep.
- Worker sampling with a real `flats worker`: `cpu_ns` rises under a
  CPU-bound handler; on Linux `mem` rises after allocating wasm memory and
  after a large SQLite cache; a `console.log` of a usage-shaped JSON line is
  not parsed as a sample.
- Attribution: exit during a scrape is seen exactly once (no dip, no double);
  a restart, a docs sleep and wake, a rename and a version switch keep the
  flat's CPU monotonic and on the flat; preview and trial CPU land in their
  roles only; shutdown persists the final CPU.
- Sleeping documents: scraping `/metrics` and reading stats do not start a
  worker (spawn count unchanged), and the flat stays `running` in the flat
  view while its worker state is `sleeping`.
- Endpoint states: a route that moves from starting to ready shows `ready`
  within one sweep.
- Store: migration from the previous schema; delete and recreate of a slug
  starts empty usage; bucket version handling.
- `/metrics`: exposition parses with `expfmt`; no series from libtunnel or
  other libraries; cross-site browser request refused; a label census
  contains no path, host or URL values; a scrape performs no store query
  (store spy) and no file walk.
- Console: Usage section for a server flat and a document, and the note for a
  website, checked in a real browser on a disposable dev host.
