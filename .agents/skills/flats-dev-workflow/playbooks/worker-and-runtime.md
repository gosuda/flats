# Playbook: workers, QuickJS/wazero runtime and metrics

Read [Process layout](../../../../docs/design.md#process-layout) and, for
document workers, [Idle worker lifecycle](../../../../docs/internal/docs-app.md#idle-worker-lifecycle)
before changing this area.

## Worker lifecycle tests

- [ ] A worker's process exit comes before the manager removes its socket and
  before a clean-sleep certificate is installed. A test that asserts a
  completed clean suspension waits for `CleanDormant()`, not for the PID to
  disappear (see `waitClean` in `internal/apps/docs/host_idle_test.go`).
- [ ] Long-lived WebSocket tests must behave like a real client: answer
  control-frame pings and send the application `ping` messages, or the
  connection's own timeouts end the test early.
- [ ] Permission-failure tests skip as root (`os.Geteuid() == 0`), because root
  ignores directory permissions. Run them as a non-root user on Linux; a
  root-only run does not cover them.

## QuickJS and wazero (`internal/qjs`, `internal/runtime`)

- [ ] Know who owns each value before freeing it:
  - a JS-owned C string is released with `JS_FreeCString` (`readCString`);
  - a malloc-packed wrapper is released with `free` (`Handle.Free`);
  - an ArrayBuffer backing store is borrowed, so copy it before freeing its
    wrapper (`Value.ToByteArray`);
  - owned atoms and values are freed exactly once.
  `internal/qjs/ownership_test.go` covers these cases; extend it.
- [ ] WASI QuickJS reports zero malloc usable size, so automatic cycle
  collection misses large payloads. The runtime captures the original `gc`
  before guest code runs and collects explicitly after request references are
  released (`collectCycles` in `internal/runtime/js.go`). Keep that order.
- [ ] `Runtime.Close` closes the native wazero module in a `defer`, so the
  native store is released even when JS teardown panics after a deadline, and
  a repeated `Close` is safe. Keep `internal/qjs/close_test.go` passing.
- [ ] Never save a memory image of user module code. Images are for the
  built-in docs app only ([worker cold start spec](../../../../docs/spec/worker-cold-start.md)).

## Metrics

- [ ] Register metrics on the private registry in `internal/metrics`. Never use
  `promauto`, `promhttp.Handler()` or `prometheus.DefaultRegisterer`; see
  [Library](../../../../docs/internal/metrics.md#library).
- [ ] A scrape reads cached state from memory only. It never touches SQLite,
  files, workers or providers (`TestScrapeDoesNotCallEndpointSource`).
