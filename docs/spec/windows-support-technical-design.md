# Windows support technical design

Status: partly implemented. The product rules are in
[Windows support](windows-support.md).

The facts below were measured on Windows 11 Pro 10.0.26200 amd64, in a session
without administrator rights, with a build of this branch from LF sources.

## Measured facts

| Question | Result |
| --- | --- |
| Does a logon-triggered task with an interactive token register without administrator rights? | Yes. |
| Does a task with logon type `S4U` register without administrator rights? | No: `Access is denied`. |
| Does `RestartOnFailure` restart a task whose process exits with an error? | No. A task that exited with 1 did not run again. |
| Does a one-minute repeating trigger with `MultipleInstancesPolicy=IgnoreNew` restart a killed server? | Yes, 44 seconds after the kill. |
| Does `conhost.exe --headless flats.exe serve` run without a window? | Yes: the server answers and no process has a window. |
| Does `schtasks /End` reach `flats.exe`? | No. It ends `conhost.exe`, and `flats.exe` keeps serving. |
| Does a worker run over a Unix socket? | Yes, with the socket bound by the worker. `cmd.ExtraFiles` and `UnixListener.File` do not exist on Windows. |
| What does `os.Root` refuse? | `NUL`, `COM1`, `CONIN$`, `a:b`, `x::$DATA`, `..\x`. It stores `dot.` as `dot` and `space ` as `space`, and it accepts `LONGFI~1.TXT`. |
| Does the NetBird module build for Windows? | Yes: `GOOS=windows CGO_ENABLED=0 go build ./cmd/flats` in `netbird/`, for amd64 and arm64, 140 MB. Its runtime on Windows is not measured. |
| Do workers that start together on an empty compile cache collide? | Yes. Of 1200 compiles in rounds of 8, 551 failed with `rename ...: Access is denied`, and a second `CompileModule` on the same runtime succeeded in all 551. Under load a read collides too: 6 processes of 32 workers failed 41 to 46 of 48 runs with `open ...`: another process has the file. With a bounded retry of both, none of 48 failed. |
| Does a panic in a task-started process reach the log? | Yes, with its stack. A task with the action, token and logon type of the service ran the helper of `logfile_windows_test.go`. |
| What does a wake cost? | With a ready pre-started worker, at p95 of 20 in three logged runs: a tiny JavaScript flat 27 to 29 ms, the first page of a sleeping document 74 to 76 ms, a collaboration hello 62 to 67 ms. Earlier runs beside other work, without a kept log, reached 34, 92 and 76 ms. |
| Does a Unix socket always report the end of its stream? | No. After a reply and an immediate close of the other side, 29 to 40 of 160000 reads stayed blocked in each of four runs: 17 to 29 after `shutdown`, 10 to 14 after `closesocket`. TCP missed none of 87444 and Linux none of 480000. All data arrived. One write on such a socket ended the read, in each of the 29 sockets of the last run. |
| Does a plain HTTP connection meet the same fault? | Yes, when a close follows a reply at once. Of 40000 connections, 1080 and 1086 stayed open in a server without an idle timeout, and none in two runs with one. A wait of 1 ms before the close left 1 of 40000, and waits of 20 ms and 200 ms left none of 24000. Linux left none of 224000. The close of an idle connection after 60 s has no run of its own. |
| What does one hung program cost a broadcast of the new `Path`? | `[Environment]::SetEnvironmentVariable` blocked for about three minutes. `SendMessageTimeout` with `SMTO_ABORTIFHUNG` and a 2 s timeout returned in 7 s, and the whole install took 13 s. |

## Worker socket

On Unix the parent binds the socket and passes it as fd 3, and a pre-started
worker's socket is renamed to the instance's path. On Windows a child cannot
inherit a listening socket, so:

- `workerSpec.Socket` carries the path, and the worker binds it after it reads
  its spec. An empty value means the inherited socket, so Unix stays unchanged.
- `listenFile` returns no file and removes a stale path.
- `moveWorkerSocket` does nothing: a pre-started worker has no socket before it
  gets its spec. Adoption therefore needs no rename, and it removes what an unclean
  exit left at the worker's socket path.
- `workerConn` hides `CloseWrite` from the proxy. When a visitor's WebSocket ends,
  `ReverseProxy` half-closes its side to the worker and waits for the worker to
  close. The worker can miss that end, so the lease of a document lasted until the
  next ping, up to 30 s. The proxy now closes both sides at once, and the lease ends
  with the visitor. A worker that misses the close keeps its socket until its next
  ping, up to 30 s. The sweep of an idle document does not depend on that socket. A
  client that ends its side after its Close frame gets no Close reply from the
  worker on Windows: the proxy closes both sides when that side ends.
- `workerServerIdle` gives the worker's HTTP server an idle timeout of 120 s on
  Windows, twice the host's `workerIdleClose`. A plain connection has no ping. The
  host closes one at once after a reply when it already keeps 64 idle connections or
  when a visitor leaves during a reply, and the worker can miss that close. Without
  the timeout the connection and its goroutine would last until the worker exits. The
  host normally closes first. The exception: a visitor stalls for more than 60 s while
  the last bytes of a large reply wait in the host, so the host's idle period starts
  late. The worker can then close the connection as the host reuses it, and a request
  with a body answers 502. macOS and Linux have no such timeout, because their socket
  reports every close.
- The host's own read can miss the end when a worker exits. The proxy then keeps the
  visitor's connection until the visitor leaves. The page of a document closes its
  socket after 45 s of silence and connects again.

The parent connects only after the worker reported `ready`, so the later bind
does not change the startup order. The socket directory is `<data>\run`, or
`%TEMP%\flats--1` when that path is too long (`os.Getuid` is -1 on Windows). Before
a worker binds a socket, `privatedir.Restrict` gives the directory the list of
"Private data". It refuses a directory that another user owns, because an owner can
change the list later, so a second user with the same shared `TEMP` gets an error.

## File locks

`internal/filelock` replaces the direct `flock` calls of `internal/app`,
`internal/notebook` and `internal/selfupdate`. On Windows it uses `LockFileEx`
on one byte at the highest file offset, because those locks are mandatory: a lock
over the content would block a reader. The system releases the lock when the
handle closes or the process ends, as with `flock`.

## Durability and open files

- `internal/durable.SyncDir` replaces the directory flushes. It does nothing on
  Windows, where a directory handle cannot be flushed. Rule 16 states what that
  costs. File contents still get their `Sync`.
- Windows refuses to rename or remove what has an open handle. A directory held
  by `os.Root` is closed before its rename. `spill.go` closes an owner lock before
  it removes the directory: this holds on Unix too, and it is safe because every
  acquirer of an owner lock first holds the namespace lock (`openOwner`), and
  `Close` removes only its own directory.
- `putBlob` treats a failed rename as success when the blob exists, because
  Windows refuses a rename onto a read-only file, which is the race of two writers
  of one blob. The bytes are the same by their hash.
- `wasmcompile.Module` calls wazero again after a failed read or rename of a cache
  entry, up to ten times with a short wait. wazero keeps a compiled module in memory
  and then stores it in the file cache, and that store fails while another worker
  has the entry open. A read fails while another worker replaces the entry. The next
  call finds the module in memory or in the file.
- The self-update lock opens without a check for a reparse point, where Unix passes
  `O_NOFOLLOW`. It lives in the user's own cache directory, so a link there already
  needs that user's rights.
- `internal/sqliteuri.File` builds `file:///C:/dir/name.db`. `url.URL` with a
  plain Windows path gives an invalid URI authority.
- `cleanDataDir`, `readKeyFile` and the scratch `config.New` calls assumed a
  path that starts with `/`.

## Private data

`internal/privatedir.Protect` gives the data directory a protected access list
with the user and `SYSTEM` alone, and children inherit it. `lockDataDir` calls it,
so it holds before any key or database opens. Outside the profile a directory
otherwise inherits access for other local users.

A change walks the whole tree and takes access from everything below, so:

- A list that is up to date stays. The check compares the entries as a set, because
  the system stores them in its own order and adds the `AI` mark.
- A volume root, the user's profile and a non-empty directory that shows no sign of
  Flats are refused with `ErrNotOwnDirectory`, and nothing changes. The signs are
  names that only Flats uses (`flats.db`, `flats.lock`, `secret.key`,
  `zrok-environment`) and a `config.json` with `schema_version`. A list that is already
  up to date is not checked again, so a data directory may gain other files later.
- `%LOCALAPPDATA%\Flats` is never refused. It holds `service.xml`, also for a service
  on another data directory, and Flats chose that folder itself.
- A volume without `FILE_PERSISTENT_ACLS` is left alone with a log line.

## Service

Package `internal/winservice` holds the Windows counterpart of `launchd` and
`systemd`. It keeps the definition in `%LOCALAPPDATA%\Flats\service.xml`, which
plays the part of the plist and the unit file: `flats update` and the install
restore read it. It registers the task `Flats` with `schtasks /Create /XML`:

- Principal: the current user's SID, `InteractiveToken`, `LeastPrivilege`.
- Triggers: `LogonTrigger` for that user, and a `TimeTrigger` that repeats every
  minute. With `MultipleInstancesPolicy=IgnoreNew` the time trigger starts the
  task only when it does not run, which gives rule 9.
- Settings: no execution time limit, not stopped on battery, hidden.
- Action: `conhost.exe --headless "<flats.exe>" serve --config <path>
  --log-file <path> --watch-parent`. The headless console gives the server and its
  workers a console without a window.

Nothing parses `schtasks` text, which is localized. `/Query /TN Flats` exits with
1 for a missing task and for a failed query, so a second `/Query` of all tasks tells
the two apart. The data directory lock says whether a server runs. `flats.lock`
holds the process ID, and `service` after it for a server with `--watch-parent`.

`--log-file` and `--watch-parent` exist only on Windows and are accepted with
`--config`, because they say how this process runs and hold no host configuration.
`--log-file` moves the log and the standard error handle with `SetStdHandle`, so a
Go panic reaches the file; launchd and systemd capture output themselves, and a
scheduled task does not. The log rotates while the server runs, by a copy and a
truncation, because the standard error handle must stay valid. A rotation that
fails writes a line to the log, and the next attempt comes after another 10 MiB. `--watch-parent` waits on the parent process, the headless
console, and shuts down in order when it ends. A parent that is already gone when
the watch starts is an error, so `flats serve` exits and the next run of the task
starts over. If the console ends between the start of `flats serve` and
`OpenProcess` and its ID goes to a new process at once, the server would watch that
process. The design accepts that: it needs an ID reused within milliseconds.

Stop works in this order, and `flats uninstall` runs it before it removes the task:

1. If the data directory lock is free, or a foreground `flats serve` holds it, stop
   here and change nothing, so that a failed `flats install` never leaves a task
   disabled that the user did not stop. If `service.xml` is gone, no lock says
   whether a server runs: run `schtasks /End`, which reaches one through its console,
   and stop here.
   A server that is starting and has no lock yet counts as stopped. If it is such a
   server, or one that the trigger starts during `flats install`, the install goes on
   or fails on the lock, and the user runs it again.
2. Disable the task, so that the trigger does not start the server again.
3. Run `schtasks /End`. That ends the console, and the server sees its parent end
   and shuts down in order.
4. Wait up to 20 seconds for the lock to come free.
5. End the process through the ID that `lockDataDir` records in `flats.lock`. Its
   workers exit when stdin closes. The lock proves that the ID is the holder's.

This is meant to work from any session of the user, also over SSH; only a fake
runner has tested that. Start registers the task
from its definition, which enables it, and runs it. `flats install` reuses the
flow of `internal/cli/ops.go`: stop, prepare the config, write the definition,
start, wait for the server, and restore the previous definition on failure.

`flats install` without `--config`, `--data` or serve flags stays on the host of the
installed definition, whatever `FLATS_DATA` and `FLATS_CONFIG` say, because it is how
the installer upgrades and how a user restarts the service. If that `config.json` no
longer exists, the install creates a new host at that path.

A status check takes the data directory lock for a moment to see whether a server
runs. `tryLockDataDir` therefore retries for 250 ms, so that a check never makes a
starting server fail.

## Update of a running binary

Windows allows a rename of a running executable but not a replacement.
`commitExecutable` removes the asides that no longer run, renames the target to
`<name>.old-<time>`, renames the staged file to the target, restores the aside if
that fails, and tries to remove the aside. `Update` reads the zip with its entry
`flats.exe`, and `checkService` reads the task's definition to refuse the service's
binary. `samePath` ignores case on Windows, because `EvalSymlinks` can return
another case than the path that started the process. `install.ps1` uses the same
aside.

## Names

`internal/winname` holds the rules of rules 19 to 21. It has no build tag, so
its tests run on every platform, and its callers apply it only on Windows, so a
bundle that is valid on Unix stays valid there.

- `internal/bundle` refuses such a name on save and finds case collisions.
- `internal/slug` refuses device names.
- `internal/site.Static` refuses such a name in a request, and after it opens a file
  it compares the name that the system reports (`GetFinalPathNameByHandle`) with
  the requested path, byte by byte. Case variants and short names fail this check.

The check is on the relative path, because the data directory may itself have a
short name. `env.FILES` goes through `os.Root`, which refuses devices and streams,
and `checkKey` applies the same name rules on Windows.

## Reload

`SIGHUP` does not exist on Windows, so `serve` never gets it. `syscall.SIGTERM`
still arrives for `CTRL_CLOSE_EVENT`, `CTRL_LOGOFF_EVENT` and
`CTRL_SHUTDOWN_EVENT`, which Go maps to it. The two messages that mention `SIGHUP`
come from a constant per platform.

## Release and CI

`scripts/build-release.sh` writes `flats_windows_amd64.zip` with `flats.exe`.
`.github/workflows/release.yml` uploads it. A `windows` CI job builds, runs the
tests that apply and runs `scripts/install-smoke.ps1` against an archive of the
same run.

A new `.gitattributes` holds `* text=auto eol=lf`. Every text blob in the
repository is LF already, so it renormalizes nothing. Without it, `flats` built
from a CRLF checkout panics at start: `runtimeref: missing public_instance_on
refusal` was reproduced, because the embedded docs are parsed by line.

Tests that need `*.localhost` to resolve use a dialer, because the Go resolver on
Windows does not map those names. Tests of POSIX modes, symlinks (a privilege on
Windows) and other Unix semantics skip on Windows, each with its reason.

## Verification

- `go test ./...` on Windows, with the skipped set explained.
- `scripts/install-smoke.ps1` on this machine and on CI.
- The manual check of rule 29 as a standard user.
- A test that each spelling of rule 20 answers 404 from `site.Static`.
- A measured panic in a task-started server reaches `serve.log`.
- Each content type deployed and used in a browser on Windows.
- `GOOS=linux` and `GOOS=darwin` builds, and the unchanged Linux test suite in CI.

The Windows CI job leaves out `internal/devreview`: its `run` command needs process
groups and reports that it is Unix only, and it is a development tool.
