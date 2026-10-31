# Guarded JavaScript engine

Status: proposed

## Summary

All JavaScript execution in a Flats worker goes through one guarded engine
type. The engine hides the QuickJS runtime, the wasm module and every
engine handle. It enforces the call, memory and failure rules in one place.
Callers cannot skip a rule, because they cannot reach the raw runtime.
Observable behavior of flats does not change.

## Requirements

1. The package `internal/jsengine` MUST be the only package that imports
   `internal/qjs`. A repository check MUST fail when another package imports
   it. Test files of `internal/qjs` are not covered by this rule.
2. The engine interface MUST NOT expose a JavaScript value, a handle, an atom
   or a wasm address. It MUST pass only strings, byte slices, errors,
   contexts and the named types of this spec across the boundary. One escape
   hatch is allowed: the document codec, whose typed-value semantics cannot
   be carried by byte slices. It is exposed as an opaque `Value` type and
   `RawCodec CodecFunc` on `Spec`.
3. Spec carries the whole construction surface: `Dir`, `Entry`, `TimeoutMS`,
   `MaxInputBytes`, `MaxOutputBytes`, `MaxHostArgBytes`, `CacheDir`,
   `MemoryPages` (0 = default), `LogSink`, `Stdout`, `Stderr`, `Prelude`, `Boot` and `BootSource` startup
   sources, `ImagePath` (empty = no image support), `ImageBinding`, `Census` and the host function.
4. An engine runs at most one call at a time. A call that starts while
   another call runs MUST return `ErrBusy` and MUST NOT touch the runtime.
5. The engine has the states Ready, Busy, Poisoned and Closed. A fatal
   failure moves it to Poisoned. A Poisoned engine MUST refuse every call
   with `ErrPoisoned`. Only `Close` is allowed from Poisoned.
6. These failures are fatal: a Go panic raised by the runtime, a wasm trap,
   a deadline that closed the module, an out-of-memory failure, an
   `InternalError`, a damaged stack canary, an exception left pending after a
   call returns, and a count of live handles that is not zero after a call
   returns. A guest exception that is none of these is a script error. A
   script error leaves the engine in Ready.
7. Every call MUST take a context with a deadline. A call without a deadline
   MUST fail with `ErrNoDeadline`. The deadline MUST NOT exceed the worker's
   configured timeout.
8. The engine MUST check these limits before any byte crosses into the wasm
   module: the size of a call argument, the size of a call result, the number
   and size of host-call arguments, and the size of one log line. A call over
   a limit MUST fail with `ErrTooLarge` and MUST NOT poison the engine.
9. The engine owns the memory policy: the wasm page cap, the QuickJS heap
   limit and the collection of reference cycles. The rules in the playbook
   "QuickJS and wazero" stay in force. Callers MUST NOT call `gc` directly.
10. Host functions have the form
    `func(ctx context.Context, op string, args []byte) ([]byte, error)`.
    An error from a host function becomes a JavaScript exception. A panic in
    a host function is a fatal failure.
11. Prewarm crosses the boundary as an opaque `Shell`. A caller MAY create a
    shell outside a worker and hand it to `Open`. A shell exposes only
    compatibility and close.
12. Images cross the boundary as an opaque `Image` type with compatibility,
    binary marshal and eligibility. `Snapshot`, `Restored`, `ImageEligible`
    and `GCDrained` exist on the engine. `ImageEligible` returns an error describing
    capture eligibility (nil when eligible). Requirement 13 governs who may use
    them.
13. Snapshot and image restore MUST exist only for the constructor of the
    built-in document app. User module code MUST NOT be saved in an image.
14. `Close` MUST be safe to call more than once and from any state. It MUST
    close the native module even when JavaScript teardown fails or panics.
15. The engine MUST check at `Open` that the embedded `qjs.wasm` has the
    pinned SHA-256. A mismatch MUST fail `Open`.
16. The script `scripts/build-qjs-wasm.sh` MUST rebuild `qjs.wasm` from the
    pinned quickjs-ng commit. It MUST stop with a clear message when a tool
    is missing. The rebuilt binary MUST pass the engine test suite.
17. Observable behavior of flats MUST NOT change. This includes isolation,
    resource limits, timeouts, error reports, crash restart, idle sleep, the
    cold-start budgets of [worker-cold-start.md](worker-cold-start.md) and
    the per-runtime clocks and random sequences.

## Verification

- A parallel call test shows `ErrBusy` under the race detector.
- One test per fatal class in requirement 6 that is reachable in-process
  shows the Poisoned state.
- Boundary tests use the sizes N-1, N and N+1 for each limit in
  requirement 8.
- A test calls `Close` twice and calls the engine after `Close`.
- The existing `internal/runtime` suites pass without change to their
  expectations.
- The import check is shown to fail on a temporary forbidden import.
