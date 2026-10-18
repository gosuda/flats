# Playbook: performance, disk, memory and cold start

## Measure

- [ ] Check for competing load before timing anything: other tests, builds or
  containers on the same machine. Record load with the result.
- [ ] Explain a number before reporting it: what limits it, and what else it
  might be measuring (disk, cache state, a shared VM, the measurement itself).
- [ ] Compare against a baseline from the same machine and the same setup,
  interleaving runs when the machine is shared.

## Disk and SQLite

- [ ] Count syscalls and fsyncs, not only bytes written. Small writes with many
  syncs cost more than their byte count suggests.
- [ ] `PRAGMA wal_checkpoint(PASSIVE)` is a real checkpoint: it copies frames
  and syncs. `wal_checkpoint(NOOP)` only reads the frame counts; check that the
  SQLite bundled by `modernc.org/sqlite` in `go.mod` supports it before relying
  on it. The docs worker's policy is in
  [Disk quota](../../../../docs/internal/docs-app.md#disk-quota) and the
  [revision store](../../../../docs/revision-store.md).
- [ ] Do not predict write-ahead log growth from the SQL statements. Measure
  the real log on a file database, not an in-memory one.
- [ ] Worker migrations and backlog reads page through rows: a query result is
  capped at `MaxQueryResult` (16 MiB, `internal/runtime/protocol.go`). See
  [Initialization and migration](../../../../docs/internal/docs-app.md#initialization-and-migration).

## Memory

- [ ] Summed RSS double-counts shared binary pages. Use cgroup memory
  (`memory.current`, `memory.stat`) for a container and per-process PSS or
  anonymous memory (`/proc/<pid>/smaps_rollup`) for processes.
- [ ] Go runtime metrics miss memory mapped outside the Go runtime, such as the
  SQLite allocations of `modernc.org/memory` and wazero's compiled code. Worker
  memory is reported as `RssAnon`, on Linux only; see
  [Worker CPU and memory](../../../../docs/internal/metrics.md#worker-cpu-and-memory).
- [ ] Compare like with like: an active WebSocket workload, compilation-cache
  state and retained file cache all change the numbers.

## Cold start

- [ ] The cold-start budget tests skip unless `FLATS_COLDSTART_BUDGET=1` is set
  (`internal/runtime/budget_test.go`, `internal/apps/docs/host_idle_budget_test.go`).
  Measure with a ready pre-started worker, as the
  [worker cold start spec](../../../../docs/spec/worker-cold-start.md) defines.
