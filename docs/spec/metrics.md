# Metrics

Status: implemented.

## Summary

Flats reports how the host and each flat are doing, so an operator can see
load, failures and resource use without reading logs. There are two views of
the same measurements:

- **Host metrics** at `GET /metrics` on the management server, in the
  Prometheus text format, for an operator's existing monitoring
  (Prometheus, Grafana Agent, VictoriaMetrics and similar scrapers).
- **Flat usage** in the console's Analytics screen and the stats API: how
  many requests a flat served, how many failed, how fast it answered, and how
  much CPU and memory its worker used, per day.

Page views stay as they are. Usage adds to them; it does not replace them.

## Host metrics

1. `GET /metrics` answers in the Prometheus text exposition format on the
   management server: the loopback listener, every console domain and declared
   external console origins. It is
   never served on a flat's route, a preview or a public provider.
2. It is read-only and needs no new credential. It follows the existing
   request guards of the management server (the `Host` and `Origin` checks,
   and the refusal of cross-site browser requests that `/api` applies). Like
   the rest of the management server it is not access control: whoever can
   reach the console can read the metrics. They include what no other route
   shows, such as CPU, memory and response times, and the slugs of Private
   and unpublished flats; a system that scrapes them must be trusted like
   someone with console access.
3. It is always on. There is no setting to turn it on, change its path or
   move it to another port.
4. A scrape never wakes a sleeping document, starts a worker, reads SQLite,
   walks a flat's files, or calls a network provider. It reads values the host
   already keeps in memory; its cost grows with the number of flats, not with
   their traffic or size. Traffic counts are always current and worker
   figures are at most 15 seconds old. Host-wide counts and network states are
   refreshed by each host sweep, normally every 30 seconds, and disk usage at
   least every 5 minutes; the time of the last sweep is reported so a stalled
   refresh is visible.
5. The metrics cover:
   - the host: build version, the host process's own CPU and memory, and the
     size of `flats.db`;
   - flats: how many exist by type, visibility and whether they are running
     (running keeps its console meaning: a live document is running even
     while its worker sleeps);
   - traffic per flat: requests by status class and by route kind (private,
     public), requests refused by the rate limit, response bytes, open
     WebSocket and event-stream connections, and a response-time histogram;
   - workers per flat: CPU time, current memory, worker state and unexpected
     exits; and CPU of the flat's preview and validation workers, separately
     from its live usage;
   - storage per flat: disk bytes and when they were measured, for every flat;
   - networks: route endpoints per provider and state (ready, starting,
     error and so on);
   - the management server: requests by surface (`api`, `console`, `mcp`,
     `metrics`, other) and status class;
   - pending approvals.
6. Labels never carry request paths, query strings, client addresses, user
   agents, tailnet or NetBird identities, URLs or domain names. A flat is
   labelled by its slug and type, a network by its provider ID. Every label
   has a bounded set of values.
7. Counters start at zero when the host starts, as Prometheus expects. After a
   rename, a flat's series continue under its new slug; a deleted flat's
   series disappear.
8. Only Flats' own metrics are exposed, plus the standard Go runtime and
   process figures of the host process. Metrics that bundled libraries
   register for themselves are not.

## Flat usage

9. A **request** is every HTTP request that reaches a flat's live route,
   private or public, including the ones Flats answers itself (rate limited,
   not deployed). Preview traffic is not the flat's usage and is not counted.
   A request counts on the UTC day its response status is sent.
10. Requests are counted by status class (`1xx` … `5xx`). An **error** is a
    `5xx` answer. A request whose visitor disconnected before the response
    was sent counts as **closed**, not as an error, even when Flats answered
    the departed visitor with a `502` or `503` (for example while a document
    was waking); a visitor that disconnects while a response is being sent
    does not turn it into one.
11. **Response time** is measured from when Flats receives a request until
    the response is complete. WebSocket connections and event streams count
    as requests with their status but are left out of response times, since
    their duration is the length of the conversation.
12. **CPU** and **memory** are reported for flats whose content runs in its
    own worker process: server flats and documents. Websites, pages and
    designs run inside the host process; for them Flats says that CPU and
    memory are not measured separately, rather than showing zero.
    - A flat's usage covers the workers that serve its live route, including
      an older version still finishing its requests after a switch. Preview
      and validation workers are not part of it; host metrics count their CPU
      separately.
    - CPU is the total processor time (user and system) that those workers
      used, including workers that later stopped, restarted or went to sleep.
    - Memory is the private resident memory those workers hold now,
      excluding shared program code and swapped-out pages. A sleeping
      document holds none. The daily figure is the highest memory seen that
      day; Flats samples memory every 15 seconds, so a short spike between
      samples can be missed.
    - Memory is measured on Linux. On other systems Flats says that worker
      memory is not measured there, rather than showing a partial figure.
13. The **worker state** of a flat with a worker is the state of the
    worker serving its current version: running, starting, going to sleep,
    sleeping, restarting after a failure, or stopped. It is a diagnostic
    detail: it does not change whether the console and API call
    the flat running ([Idle document workers](docs-worker-idle.md), rule 11).
14. Usage is kept per UTC day, like page views: it follows renames, is
    deleted with the flat, and the stats API returns up to 365 days (default
    30). The console shows the same 7 or 30 days as page views, with the
    period's totals: requests, error rate, median and 95th-percentile
    response time, CPU time and peak memory, and the current memory and
    worker state. Disk usage stays where it is today.
15. Measuring must not slow down serving. Counting a request is done in
    memory; persisting and sampling happen off the request path, and a slow
    database write never holds up a request. If a measurement fails, the last
    known value stays and is marked with its time; it never silently becomes
    zero. A host shutdown persists usage after the workers have stopped, so
    their last CPU is kept.
16. Usage has the same privacy rule as page views: no query strings, client
    addresses, user agents or visitor identities are recorded.

## Not in scope

- Per-path or per-visitor response times, traces and profiling endpoints
  (`pprof`).
- Alerting. Operators alert from their own monitoring.
- Authentication for the management server, including `/metrics`; it stays
  the open design item recorded in [surfaces.md](../surfaces.md#request-guards).
- Metrics that a flat's own code defines.
- Bytes received per flat, and bytes inside WebSocket connections.
- An MCP tool for usage. Agents and the CLI read it through
  `GET /api/flats/{slug}/stats`; the console through the same route under
  `/console/api`.

## Open questions

None.
