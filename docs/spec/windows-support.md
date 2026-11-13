# Windows support

Status: partly implemented; see [Current state](#current-state).
The technical design is in
[Windows support technical design](windows-support-technical-design.md).

## Summary

Flats runs natively on Windows with the experience it has on macOS and Linux:
one command installs it without administrator rights, it runs in the background
from login, and the console opens at `http://127.0.0.1:7878`.

## Scope

Windows 11 and Windows Server 2022 or newer, on amd64. The measurements ran on
Windows 11, and CI runs on Windows Server 2025. Windows Server 2022 and Windows 10
22H2 should work and are not tested.
Windows on arm64 can run the amd64 build through its emulation; a native arm64
archive is a later step.

WSL is not part of this spec. `install.sh` inside WSL is the Linux path.

## Rules

### Install

1. One PowerShell command installs Flats:
   `irm https://raw.githubusercontent.com/gosuda/flats/main/install.ps1 | iex`.
   It needs no administrator rights and works in Windows PowerShell 5.1 and in
   PowerShell 7.
2. The installer downloads the release archive for the machine, verifies its
   SHA-256 against the release's `checksums.txt`, and stops without a change on
   a mismatch.
3. The binary goes to `%LOCALAPPDATA%\Programs\Flats\flats.exe`. The installer
   adds that directory to the user's `Path`, not the machine's, so that a new
   terminal finds `flats`. It adds the entry only when it is missing and keeps
   every existing entry as written, including `%VAR%` forms. With `-NoPath` it
   leaves `Path` alone. `flats uninstall` removes the service and not the binary,
   as on Unix, so it leaves `Path` alone too.
4. On an existing installation the installer replaces the binary and keeps the
   service's settings and data, as `install.sh` does: it restarts a running
   service and leaves a stopped one stopped, and says how to start it.
5. The installer offers the options of `install.sh` that apply: the install
   directory, the release, no service, a download mirror or local directory, and
   `flats serve` flags for a new service. A piped `iex` takes no parameters, so
   options use this form:
   `& ([scriptblock]::Create((irm <url>))) -Version v1.2.3`.

### Service

6. `flats install` registers a per-user service, and `flats uninstall` stops it,
   removes it and keeps the data. Neither needs administrator rights.
7. The service starts at login, and starts at once after `flats install`. In a
   session without a desktop, such as SSH, `flats install` registers the service
   and says that it starts at the next login.
8. No window appears for the service or its workers, and closing a terminal
   does not stop it.
9. The service comes back within two minutes after the server process dies.
10. A stop lets `flats serve` shut down in order, as `SIGTERM` does on Unix: it
    closes workers and databases and releases the data directory lock. A server
    that does not exit in time is ended. `flats install` and `flats uninstall`
    stop the service this way, and a service stopped by them stays stopped until
    it is started again. They leave a foreground `flats serve` alone, as on macOS
    and Linux.
11. Ending the task from Task Scheduler is an orderly stop, and the service comes
    back by rule 9. Disabling the task keeps it stopped.
12. `flats status` reports whether the service is installed and running, on a
    Windows of any display language.
13. The service writes its log to `<data>\logs\serve.log`, including a crash of
    the Go runtime, and `flats install` names that file. The log rotates at
    10 MiB while the server runs, and keeps one older file.

### Server

14. Every content type works: static and server flats, pages, documents,
    designs, boards and notebooks, including worker sleep, wake and the
    pre-started worker.
15. A second `flats serve` on one data directory is refused with the message
    it has on Unix.
16. File contents reach the disk as they do on Unix. The renames that publish
    them do not: Windows has no directory flush. A process crash loses nothing,
    but a power loss or a system crash can undo the last renames. NTFS keeps its
    metadata consistent, and Flats recovers from the state before a rename as it
    recovers from any interrupted operation.
17. The default data directory is `%LOCALAPPDATA%\Flats`, not the roaming profile,
    which would copy databases and caches at each logon on a domain machine.
18. The data directory is private to the user: `flats serve` gives it an access
    list for the user and the system alone, and what is mode 0600 on Unix, such as
    keys and sockets, relies on that list. That list reaches every file below it,
    so the directory must be a directory of its own. `flats serve` refuses a volume
    root, the user's profile, and a non-empty directory that shows no sign of Flats,
    and it changes nothing there. The default data directory is never refused: it
    holds the service definition, also of a service on another data directory. A
    volume without access lists, such as FAT or
    exFAT, cannot be private: `flats serve` runs there and logs that.

### Names

19. A save never writes outside the flat, and a name that Windows cannot store
    exactly is refused with an error that names the file: device names such as
    `NUL` or `COM1`, also with an extension, names with `:` or `<>"|?*`, control
    characters, names that end in a dot or a space, names in the form of an 8.3
    alias such as `LONGFI~1.TXT`, and two names that differ only in case. An
    `env.FILES` key follows the same rules, except for case: two keys that differ
    only in case name one file on Windows, and the last write sets the spelling.
20. A request answers only for the exact stored name. A stream
    (`/flats.json::$DATA`), a short name, a device name, a trailing dot or space
    and a case variant (`/FLATS.JSON`, `/INDEX.HTML`) answer 404. So a protection
    that compares a name cannot be passed by another spelling of it.
21. A slug that is a device name (`con`, `nul`, `aux`, `prn`, `com0` to `com9`,
    `lpt0` to `lpt9`) is refused on Windows. A data directory does not move
    between systems: the paths, modes and locks in it are not portable.

### Networks

22. The Local provider works. `*.localhost` links open in the browsers that
    resolve them to loopback.
23. Tailscale, zrok, Portal, Cloudflare Quick, NetBird and the reverse proxy
    providers run in user space and are part of the Windows build. A provider that
    cannot start reports that as it does elsewhere.
24. Windows Firewall blocks an inbound listener of a background process without a
    prompt. The documentation says so where a non-loopback address is configured.

### Updates

25. `flats update` replaces the running `flats.exe` on Windows from the release
    zip. It refuses the service's own binary, as on Unix. The old binary moves
    aside under a unique name, because a process may still run it; a later update
    removes the ones that no longer run.
26. Saving `config.json` by hand takes effect after a restart of the service, and
    messages that mention `SIGHUP` on Unix name that restart instead. Windows has
    no `SIGHUP`. Running `flats install` again restarts the service; changes from
    the console apply as they do elsewhere.

### Delivery

27. Each release publishes `flats_windows_amd64.zip` and lists it in
    `checksums.txt`.
28. CI builds and tests on Windows and runs `scripts/install-smoke.ps1`: install
    from an archive of the same run, run a flat, kill the server and see it come
    back, reinstall, stop, uninstall, and see that the data stays.
29. "No administrator rights" cannot be checked on CI, whose Windows runners are
    administrators. Each release checks it by hand as a standard user, and
    `docs/release.md` holds the procedure.
30. A checkout on Windows has LF line endings. The docs, hashes and embedded
    assets depend on them.
31. macOS and Linux behavior does not change.

## Non-goals

A real Windows service, which needs administrator rights. Running without a
login. A desktop or tray application. Native arm64 builds in the first release.
A signed binary: the first release is unsigned. Moving a data directory between
systems.

## Current state

Every rule above is in the code. Each content type was deployed and used in a
browser on Windows. The technical design holds the measurements. The status is
`partly implemented` because these differ from the target:

- A sleeping document misses [Fast worker cold start](worker-cold-start.md) on
  Windows: its first page took 74 to 76 ms and a collaboration hello 62 to 67 ms
  at p95 in three logged runs, against 50 ms. A tiny JavaScript flat meets it, at 27
  to 29 ms. Each call from the worker to the host takes 3 to 18 ms at p50 on Windows,
  and a wake makes several.
- No provider other than Local has run on Windows, so rule 23 is a build-time
  statement for now. NetBird and Tailscale need an account to measure.
- On the machine of the measurements, `TestGuestRoomsLeaveMemberRoomMemory` reaches
  the 10 s limit of one guest call at a first `hello` on a 0.9 MiB document: on
  Windows in each run, and on Linux in 7 of 24 runs at `main` and in 7 of 24 on
  this branch, interleaved. With a higher limit it passes on both, in 26 to 27 s on
  Windows and in 25 s on Linux. It is a wall-clock test, so it runs in the `docs-budget` shard,
  which the Windows job leaves out.
- `TestRejectedTextMutationReloadsWithoutDisconnectingPeers` failed in 3 of 4 runs with
  16 tests in parallel on Windows: its first `hello` reached the 10 s limit of one
  guest call after a slow worker start, 3.8 s in the last run. It passed 6 of 6 runs on
  four processors. The CI job is the first run of the docs tests on a Windows runner.
- A guest call that writes waits for the drive, and on a drive with slow flushes it
  fails. The worker's database flushes at each commit. The document app also truncates
  the write-ahead log inside the call that commits, when the log holds more than 64
  frames, so that the log at rest stays inside the disk quota (`commit` in
  `storage.js`). That copies the log into the database and flushes again. When each
  flush takes seconds, such a call reaches the 10 s limit of one guest call: the request
  answers 504, the limit closes the JavaScript runtime of the call, and a WebSocket
  whose message was in work closes with code 1011. The `windows` CI job met this in 4 of
  its first 32 completed runs, each time in tests of the `docs-soak` shard, with one to
  four failed tests in a run. A measurement on other runners gives the system drive of
  the runner as the cause. About 11 minutes after a hosted runner boots, about 1.2 GB
  are read from that drive in 36 to 50 s, through page faults. Seventeen runners showed
  the reads in the counters. On an eighteenth the counters began after the time at which
  the reads come on the others, and recorded none. On the three where a watch looked,
  the process of the Windows Storage Service used a full processor in those seconds, and
  nothing measured attributes the reads to it. A job started 1 min 44 s to 4 min 55 s
  after the boot of its runner on seventeen of the eighteen runners, and 11 min 1 s
  after it on one. On that one the reads would fall, by the clock of the others, into
  the setup steps of the job, before its tests. The shard runs from about 9 to 10 min
  after the job starts. Each of the four events began 9 min 9 s to 9 min 43 s after its
  job started, which is the time of the reads on a runner that booted about 2 min before
  its job. The boot times of those four runners are not recorded, so the events fit the
  clock of the reads, and no record shows the reads in them. Six of the measured runners
  repeated the shard 25 times each. On the three that kept the test data in the default
  temp directory, which is on that drive, the run that met the reads had a guest call of
  7.9 to 11.2 s and a flushed write of 4 KiB of 1.1 to 2.8 s, and one of the three
  failed two tests with the errors of the four events. The stacks of the stalled calls
  end in `FlushFileBuffers`, for the commit in one sample and for the truncation in a
  later one, and the timers of each process were on time. The longest guest call of the
  other 72 runs on those runners took 2.7 s, although single flushed writes took 1.4 and
  2.0 s there in the first run of two jobs. On the three that kept the test data on the
  temporary drive, the longest guest call of 75 runs took 2.7 s and the longest flushed
  write 0.13 s, the runs during the reads included. The job now keeps its test data on
  that drive, below `RUNNER_TEMP`, except for the package `internal/site`, which runs
  first with the default temp directory ([the gates](../gates.md#pull-request-baseline)
  have the reason). The product is as before: the limit, the flush of each commit and
  the truncation inside the commit. A host whose data directory is on a drive with such
  delays fails the calls that write during them. The place of the truncation is an
  [open question](#open-questions).
- Beside CPU and disk load on four processors, `TestHostIdleRealWorkerCycles` saw a
  worker start stall in two events of 252 runs, with both copies of the test at the
  same moment: for more than 15 s, which failed those two runs, and for more than
  4 s. The first began in the second that a Windows task took a volume snapshot, and
  the second had no snapshot. The host waited for the worker to report ready, and the
  cause is not identified. The slowest of 2379 traced starts took 3.2 s.
- A worker on Windows can miss the end of a visitor's connection, because a Unix
  socket there does not always report the end of its stream. The worker then keeps
  that socket until its next ping, up to 30 s, as it does for a client that lost its
  network. Until then the visitor still counts: its guest slot, its presence and its
  place as a viewer last up to 30 s longer. The lease of the document ends with the
  visitor, so a sleeping document does not wait for that socket. A plain connection
  has no ping, and it ends at the worker's idle timeout of 120 s. In a rare case that
  timeout meets a request, which then answers 502: the technical design has the
  condition.
- The host can also miss the end of a worker that exits. The proxy then keeps the
  visitor's connection until the visitor leaves. An open document page shows no
  updates for up to 45 s before it connects again.
- If another program keeps a notebook spill file open for more than 2 s while Flats
  removes it, every later read that needs a spill answers `unavailable` until the
  server restarts. The code does that on every system after a failed removal, and
  Windows refuses a removal more often. No run has shown it. The 2 s retry runs under
  the lock of the spill manager, so a held file can pause spill admission for every
  notebook on the host for that long.
- The resource check of `TestHostIdleRealWorkerCycles` counts goroutines on Windows,
  where Unix also counts open file descriptors, so it cannot catch a handle leak
  across sleep and wake on Windows.
- Rule 29 has no measurement as a separate standard user. The measurements ran
  without elevation, in an account of the administrators group.

## Pages that change

`README.md` (install, platforms), `docs/design.md` (process layout),
`docs/release.md` (archives, install, updating a client, the manual check),
`docs/configuration.md` (the default data directory, `SIGHUP`, the firewall),
`docs/internal/metrics.md` (CPU time), `docs/spec/config-reload.md`,
`docs/spec/agent-self-update.md` and `docs/spec/agent-document-routing.md`.

## Open questions

- Whether the document app can truncate its write-ahead log outside the call that
  commits. The truncation keeps the log at rest inside the disk quota. Inside the call
  it flushes again, and the call has a limit of 10 s ([Current state](#current-state)).
