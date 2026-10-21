# Fast worker cold start — technical design

Status: implemented. Design approved by independent Claude review (R2); code approved by independent Claude and Codex review (R2), with the R2 notes corrected; gated budget tests pass on macOS and Linux.
Base: `59bae8f` (origin/main, 2026-10-18). Product contract: [worker-cold-start.md](worker-cold-start.md).

## Measured baseline (macOS arm64, load average ~10, warm compilation cache)

| Path | Time |
| --- | --- |
| Sleeping document, first page after wake (`TestHostIdleRealWorkerCycles`, 20 samples) | median ~345 ms |
| Collaboration hello right after that page (worker already running) | median ~207 ms |
| Tiny JS server flat, `Manager.Start` with the real `flats` binary | ~160 ms |
| Docs server, `Manager.Start` with the real binary | ~330 ms |

Phase breakdown inside one worker (real binary):

| Phase | Time | Cause |
| --- | --- | --- |
| exec + Go runtime and package init + ready report | ~25 ms | 110 MB binary; package inits ~19 ms |
| `qjs.New` → `CompileModule` from the file cache | ~115 ms | wazero v1.12.0 `deserializeCompiledModule` reads the cache file with unbuffered 8-byte reads (thousands of `read` syscalls); fixed upstream in unreleased #2552 |
| `qjs.New` in a process that already loaded the module | ~14 ms | every new wazero runtime decodes and validates the 1 MB qjs.wasm again (~12 ms) |
| prelude evaluation | ~12 ms | |
| docs `server.js` + `content.js` module evaluation | ~170 ms | parsing and running a 900 KB bundle |
| each additional runtime in the docs worker (pool, the WebSocket runtime) | ~200 ms | the full `newJSVM` again |

A throwaway prototype (memory image of the docs runtime after module load,
restored into a new wazero instance) restored in 12.6 ms, 12 ms of which was
the per-runtime `CompileModule`. With wazero main (bufio fix) a fresh
process still needs ~21 ms for the cached compile plus ~25 ms exec and init,
so only a pre-started process keeps a document wake reliably under 50 ms.

## Overview

Two mechanisms, each falling back to today's path:

1. **Pre-started worker** (`runtime.Manager`): the Manager keeps one
   unassigned worker process that has finished exec, Go init, loading the
   compiled qjs module from the file cache, and building one runtime shell.
   A spawn adopts it instead of exec'ing.
2. **Document image** (built-in docs app only): a memory image of a QuickJS
   runtime after the prelude and the docs module loaded, before any
   per-worker data entered the runtime. It is saved next to the document's
   runtime directory and restored by every later runtime of that document
   (first runtime of each worker, pool runtimes, the WebSocket runtime).

User modules are never imaged; every runtime of every other flat evaluates
the prelude and the module as today. (R1 #7: a prelude image would add risk
for ~12 ms that the budget does not need, and would make the non-writable
`performance.timeOrigin` identical across user runtimes.)

Dependency versions do not change (wazero stays v1.12.0; no replace
directives).

## 1. Prelude: per-runtime values move to an init call

Today the prelude fetches `runtime.generation` (prelude.js:287) and `env`
(prelude.js:482) while it loads, so any image of it would contain secrets
and a stale generation (R1 P0). Change:

- `runtimeGeneration` and `env` become `let` bindings of the prelude closure,
  assigned by a new one-shot `globalThis.__flats_init()`, which performs the
  same two host calls, freezes `env` exactly as today, and deletes itself.
- `newJSVM` calls `__flats_init` before evaluating the boot module for
  normal runtimes (same order of observable effects as today: the module
  cannot see either value before its handlers run), and after capture or
  restore for document runtimes.
- Every runtime therefore asks the host for its own generation and env, so a
  restored document runtime acts with its own worker's generation.

## 2. VM images in `internal/qjs`

### Contents

The qjs.wasm module state is its linear memory and one mutable global,
`__stack_pointer` (`exportStackPointer` checks: exactly one global, none
imported). Tables are not written by C code. Flats never sets
`StartFunctionName`, so instantiation runs no start function; the libc and
QuickJS state is created by `New_QJS` and lives in memory. Go-side state:

- `Runtime.handle` (QJSRuntime*), `Context.handle` (JSContext*),
  `Context.global`, `Context.intrinsics` (name → JSValue*),
  `stackLow`/`stackTop`;
- `Context.goFunctions` must be empty at capture (else refused);
- the `ProxyRegistry`: ids 1..n mapping to Go `Function`s or the `*Context`;
  any other entry kind makes the runtime non-imageable.

An image records the memory (the 64 KiB pages that differ from a fresh
instantiation, plus total size), the
stack pointer, the raw handles, the registry kind sequence, the
`random_state` offset (below), the guest monotonic time at capture, and
caller raw values (the jsVM keeps the native `gc` function).

`Runtime.Image(extra map[string]uint64) (*Image, error)` captures between
calls. `New(Option{Image: img, Functions: [...]})` instantiates as today,
grows memory to the image size, writes every page that differs from the
freshly instantiated memory (data segments included), sets the stack pointer, rebuilds the wrappers
from raw values and re-registers the registry in the recorded order with the
caller's new functions and the new Context. Any mismatch is an error.

### Key

Restore is refused unless the key matches. The key covers: an image format
constant; SHA-256 of the patched qjs.wasm; the qjs options that shape memory
(`MemoryLimit`, `MaxStackSize`, `MaxExecutionTime`, `GCThreshold`,
`StackSize`, `MemoryLimitPages`); and a caller-supplied binding key. The
runtime package's binding key covers SHA-256 of `prelude.js`, a
`newJSVM` binding-order constant, the build identity (`buildinfo` VCS
revision and modified flag, plus the executable's size, mtime and inode,
so a replaced binary never restores an older image), and a fingerprint of
the version directory (sorted relative path, size, mtime, mode of every
regular file).

### Per-runtime entropy and clocks

QuickJS-ng 0.10.1 sets two per-context values in `JS_NewContext`, adjacent
in `struct JSContext`: `double time_origin = js__hrtime_ns()/1e6` (WASI
monotonic clock, also copied into the non-writable `performance.timeOrigin`)
and `uint64_t random_state = js__gettimeofday_us()` (WASI wall clock, the
xorshift64* state of `Math.random`). No other per-context or per-runtime
time or entropy state exists in quickjs.c, quickjs-libc.c or the qjswasm glue
(debug-only `randomize_address_space` is compiled out).

- **Monotonic clock.** Every runtime gets its own guest monotonic clock via
  `ModuleConfig.WithNanotime`: the value wazero uses today
  (`platform.Nanotime`, i.e. the Go runtime monotonic clock) plus a
  per-runtime offset, exactly 0 for fresh runtimes so user flats see the
  same values as today. A restored runtime's offset
  continues its clock from the capture time, so `time_origin`,
  `performance.timeOrigin` and any std timer deadlines stay consistent.
  Wall time stays the real host clock.
- **Random state.** While creating the template runtime, the wrapper records
  the wall and monotonic values handed to the guest during `New_QJS` and
  searches `[ctx, ctx+1024)` (8-byte aligned) for a slot holding a recorded
  `wall_ns/1000` whose preceding 8 bytes hold a recorded `mono_ns/1e6`
  double. Exactly one match is required, else the runtime is not imageable.
  Every restore writes a fresh nonzero `crypto/rand` value there before any
  guest code runs.

Documented, accepted differences for the docs app only: restored document
runtimes share `performance.timeOrigin` with the image's template, and
lib0 logging's module-scope `lastLoggingTime` (the one module-scope wall
clock read) is the capture time, so the first log time delta after a wake
can be large. The docs app uses neither for behavior.

### Eligibility

A capture is refused unless, over the runtime's whole life since `New_QJS`:

- no `__flats_host` operation ran at all (`__flats_docs_codec` is pure and
  allowed), so no environment, generation, DB, files or log data was ever
  written into the memory, including freed chunks;
- nothing was written to stdout/stderr;
- `random_state` is unchanged since `New_QJS`;
- `goFunctions` is empty and the registry holds only function/context
  entries;
- the stack canary is intact.

Wall/monotonic clock reads and module file opens are allowed for the docs
app. A census test pins the docs app's module-scope events (exactly one wall
clock read, no host operations, no output, no `Math.random`, every file the
module loader opened is closed again; the census uses a counting FS hook in
the test only), so a future docs change that adds module-scope
nondeterminism fails CI. A second test scans a captured image file for
sentinel values placed in env, the DB and FILES and requires none.

### Serialization

`Image.MarshalBinary`/`UnmarshalImage`: magic, format, key, fields, pages,
then a CRC-32C of everything before it (corruption detection in a private
directory, cheap on every CPU). Corruption or key mismatch is an error.

### Runtime shells

`qjs.NewShell(opts)` performs everything independent of a flat: wazero
runtime with the compilation cache, WASI, the `env` host module and
`CompileModule`. `New(Option{Shell: s, ...})` consumes a shell once and only
instantiates the module with the flat's FS mount and writers. Later runtimes
in a process are created as today (~12 ms each); a shared wazero runtime is
out of scope.

## 3. Images in `internal/runtime`

`workerSpec` gains `image_path` (a file path, set only for live managed
documents).

`newJSVM` for a spec with `image_path`:

1. If this process holds the document image, restore it; else try to read
   `image_path`; restore on success. A failed read, key mismatch or restore
   error deletes the file (best effort) and falls through.
2. Else create a runtime as today, evaluate the prelude and the module
   (without `__flats_init`); on success check eligibility and capture. The
   capture is kept in memory for later runtimes of this process and written
   after the worker reported ready: temporary file in the same directory
   (mode 0600), rename to `image_path`. The worker never creates
   directories, so a write after the document's state directory was removed
   fails. Errors are logged once and ignored.
3. Call `__flats_init`, then continue as today (`__flats_info` on the first
   runtime).

Startup errors cannot come from an image: an image is captured only after a
successful module load, and its key includes the version files.

Everything else (non-docs flats, previews, scratch trials) is unchanged
apart from the prelude init split and the shell.

## 4. Pre-started worker in `runtime.Manager`

- The spare exists only while the Manager has at least one instance that is
  not stopped (server flats and logical documents, dormant included). The
  first `Start` requests one; stopping the last instance stops it without
  waiting for it to exit (a spare still preparing exits when it is done).
- A spare is a `worker` child (same exe, args, env, cwd `/`, fd 3 listener)
  whose socket is bound at a fresh `s<random>.sock` path (`m.init` removes
  stale `s*.sock` as it does `w*.sock`). Its first stdin line is
  `{"prewarm":{"cache_dir":…,"warm_dir":…}}`. The worker builds a shell
  in the background, then creates `warm_dir`, a path the Manager chose for
  this spare (`s<random>` in its `run/prewarm`, or in the temp directory
  shared by Managers without a private `run`), opens a throwaway database in
  it through the `env.DB` code path, runs one statement and removes it, so
  the flat's first database open does not pay SQLite's once-per-process
  setup; a failure (also an existing `warm_dir`) only skips that. The
  Manager removes that exact directory after the process exited, whatever
  the reason, so a spare killed or crashed during the warm-up leaves
  nothing; it never removes other entries. `m.init` also clears a private
  `run/prewarm` like stale sockets. It then reports
  `{"t":"warm"}` on stderr and blocks reading the spec line. A spec that arrives during prewarm waits for it to finish
  instead of compiling a second time. A stdin EOF before the spec exits
  quietly with status 0.
- `instance.spawn` first tries to adopt the spare: it checks the spare is
  alive, detaches it from the Manager, renames its socket file to the
  instance socket path (the bound socket follows the inode; verified on
  macOS and to be tested on Linux), points its stderr reader at the
  instance's log and ready channel, sets `proc.started` to the adoption time,
  clears its stderr tail, and writes the spec line. Existing
  ready/fatal/timeout/exit handling then applies.
- Every worker reports `{"t":"spec"}` on stderr right after it parsed its
  spec, before it prepares the flat. If an adopted spare exits before that
  acknowledgement, or the rename or spec write fails, spawn retries once
  with today's exec path, so a spare that died or was killed while idle
  never causes a startup failure. An exit after the acknowledgement is the
  flat's startup failure, reported as for a new process; the flat's
  top-level code never runs twice for one start.
- After the worker's engine is prepared, a prewarmed shell it did not take
  (a WASI flat) is closed.
- After every adoption a new spare is requested 1 s after the adoption
  finished (the adopted worker reported ready, or the adoption failed), so
  that its exec, Go init and preparation do not compete for CPU with the
  woken worker and the requests that prepare it; at most one request is
  pending at a time. A spare that exits before
  adoption is discarded and replaced with exponential backoff (1 s doubling
  to 10 min); never disabled permanently.
- Spare logs before adoption go to the Manager log.
- `Manager.Close()` stops the spare, waits until every spare it or a last
  instance's stop stopped has exited, and prevents new ones. Callers: app
  serve shutdown, the lifecycle-check adapters, and test helpers. A spare
  also exits when its parent dies (stdin EOF).
- `Manager.NoSpare` disables spares for tests sensitive to process counts or
  timing of the exec path; `Manager.SparePID()` lets idle tests identify it,
  and `Manager.SpareReady()` (a spare is waiting, alive and has reported
  `warm`) lets budget tests wait for the fast path.
  The docs idle tests change deliberately: "dormant host retains no
  children" becomes "no child other than the spare, and the spare holds no
  flat" (it has not received a spec), and the socket check ignores
  `s*.sock`. The idle memory measurement reports the spare separately.

## 5. Core: document image lifecycle

`core.RuntimeSpec` gains `ImagePath string`. `docsRuntime` creates (under
`docsMu`) `dir + ".state"` (0700) for a materialized runtime directory and
returns `dir + ".state/image"`; release removes both directories after the
references drop to zero; `cleanupDocs` removes `.state` directories whose
runtime directory is not referenced. Only live managed documents get an
image path. Release runs only after `Instance.Stop`, which returns only
after the instance's worker exited, so no worker of a released document can
write an image afterwards.

The image is a function of the runtime directory contents only (no env,
generation or data), so documents sharing a runtime directory may share it.

## Implementation notes from review (R2 P3)

- Build identity: read the executable identity once at worker start; on
  Linux stat `/proc/self/exe` (the running inode), elsewhere
  `os.Executable()`.
- Shell failure: any error on the prewarmed-shell path falls back to a fresh
  `qjs.New` in the same process before reporting `fatal`.
- Registry: capture requires ids exactly 1..n without gaps; restore sets the
  next id to n.
- `.state` housekeeping: `docsRuntime` creates `.state` on both the new and
  the existing-directory branch; `cleanupDocs` keeps `.state` directories of
  referenced runtime directories and removes unreferenced ones; acquisition
  removes leftover temporary image files.
- Docs: [Idle document worker internals](../internal/docs-worker-idle.md)
  states the spare, and the new spec is listed in `docs/spec/README.md`.
- Scratch-trial-only hosts: a trial's Start creates a spare that its Stop
  removes again; spare creation is delayed 250 ms after an instance became
  ready (a server flat's successful Start, or a registered document, lazy
  ones included), so short-lived trials, whose own start and health checks
  outlast 250 ms from Start, do not churn processes.
- Not counted elsewhere: each docs worker keeps its ~5 MB image in the Go
  heap for later runtimes; the version fingerprint is taken by
  `checkVersionDir`'s walk (one walk, at worker init).
- Measure the core wake rows (`NextGeneration`, preparation commits) on Linux
  disk-backed storage before judging budgets; if they exceed it, narrow
  requirement 2 explicitly.
- Additional tests: delete/replace a document while its worker writes the
  image (no `.state` remains); a changed prelude hash or build identity
  invalidates an image; runtime and docs suites in forced-image mode as well
  as forced-off.

## Expected cost on the new wake path (to be measured per phase)

| Phase | Expected |
| --- | --- |
| core wake: storage lock, `NextGeneration` store write | measure |
| adoption: rename, spec write | < 1 ms |
| worker init: shell instantiate, read image (~5 MB) + CRC, restore, `__flats_init`, `__flats_info` | ~5 ms |
| core preparation requests (sync/import/budget; SQLite commits) | measure |
| first page request | ~1–2 ms |
| WebSocket hello (WebSocket runtime: ~12 ms compile + restore) | ~15 ms |

Requirements 1 and 2 hold when the spare is ready; a wake within ~1 s plus
the spare's preparation (~150 ms) after a previous adoption finished (spare
refill) takes the exec path.

## Risks and fallbacks

| Risk | Handling |
| --- | --- |
| Wrong reconstruction of Go-side qjs state | restored document runtimes run the full docs suite; forced-image and forced-fallback modes |
| Secrets or stale generation in images | init split; whole-life eligibility; sentinel scan test |
| Old binary's image | key includes prelude hash and build identity |
| Orphaned image after delete | `.state` directory removed after worker exit; workers never create it |
| Adopted spare dies | one exec retry |
| Backoff skew | `started` at adoption |
| Spare memory | one process per host while any instance exists; reported |

## Validation plan

- qjs: capture/restore round trip, key/CRC refusal, random offset discovery
  against the shipped qjs.wasm, independent `Math.random` across restored
  runtimes (in one process and across processes), monotonic continuity,
  eligibility refusals (host op, output, Math.random, goFunctions), shell use.
- runtime: prelude init split (generation and env per runtime, identical
  startup error messages with images on and off), document image create /
  load / corrupt / stale-key / unwritable paths, image sentinel scan,
  docs census, generation correctness after a restore (activation with a
  newer generation, `removedByNewer`).
- spare: adoption, fallback when the spare died before and after the spec
  write, socket rename (macOS and Linux), backoff with adopted spares, Close,
  last-instance stop, concurrent spawns, log routing.
- Suites: `internal/qjs`, `internal/runtime` (normal and race), all
  `internal/apps/docs` shards including idle lifecycle, `internal/core`,
  docs-check; runtime and docs suites also with images forced off.
- Budgets (environment-gated; macOS and the Linux container on disk-backed
  storage): sleeping-document first page and WebSocket hello p95 < 50 ms over
  20 wakes with a ready spare, per-phase timings logged; tiny server flat
  `Start` + first GET p95 < 50 ms with the real binary; spare PSS/footprint.
  `FLATS_COLDSTART_BUDGET=1 go test ./internal/apps/docs -run
  TestHostIdleWakeBudget -v -p 1` and, with `FLATS_WORKER_EXE` set to a
  built `flats` binary, `FLATS_COLDSTART_BUDGET=1 go test ./internal/runtime
  -run TestColdStartBudget -v`. Test Managers forward `FLATS_NO_IMAGES` and
  `FLATS_FORCE_IMAGES` to their workers; a woken document worker that
  restored the image logs `document runtime restored from image`.

## Implementation ownership

- Images owner: `internal/qjs/*` (including NOTICE),
  `internal/runtime/js.go`, `internal/runtime/prelude.js`, new
  `internal/runtime/image.go` and `internal/runtime/prewarm.go`,
  `internal/runtime/protocol.go`, their tests.
- Spare/core owner: `internal/runtime/manager.go`, `docs_lifecycle.go`,
  `worker.go`, new `internal/runtime/spare.go`, `internal/core/service.go`
  (`RuntimeSpec.ImagePath`), `internal/core/contenttype.go`,
  `internal/app/serve.go`, lifecycle-check adapters, test helpers, the docs
  idle tests, their tests.
- Frozen interface: `protocol.go` adds `prewarmSpec{CacheDir string
  "cache_dir"; WarmDir string "warm_dir"}` and `workerSpec.ImagePath "image_path"`; `prewarm.go`
  provides `startPrewarm(prewarmSpec)` (non-blocking; sends `{"t":"warm"}`)
  and `waitPrewarm()`; `WorkerMain` reads an optional prewarm line, then the
  spec; `newJSEngine` uses the prewarmed shell when compatible.
