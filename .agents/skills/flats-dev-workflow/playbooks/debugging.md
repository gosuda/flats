# Playbook: hangs, freezes and failing gates

## Before blaming the change

- [ ] Run the failing test or gate on main with the same environment and
  compare. A failure that also happens on main is not caused by the change;
  report it separately.
- [ ] Record the exact command, exit status, commit and machine load.

## Hangs and freezes

- [ ] Capture state before restarting anything, starting with evidence that
  leaves the process running: on macOS `sample <pid>`, or a debugger attached
  to the process. Flats serves no profiling endpoint.
- [ ] `SIGQUIT` makes a Go process print every goroutine's stack and then
  exit; `flats serve` handles only interrupt and `SIGTERM`
  (`internal/app/serve.go`). Send it only when ending that process is
  acceptable, such as a hung test binary or a disposable host you started,
  never to a host you do not own. A `go test -timeout` expiry also prints
  all goroutine stacks and ends the test binary.
- [ ] `GOTRACEBACK` triggers nothing. It only sets how much a crash prints; set
  it (for example `GOTRACEBACK=all`) before starting the process.
- [ ] On macOS a forked child can wedge before exec and block the parent's
  networking (golang/go#56784). Flats avoids that fork in tailscale's LocalAPI
  client and `internal/forkwatch` kills a child stuck before exec. Check the
  dump for a stuck fork before assuming a network failure. Background:
  [Real tailnet](../../../../docs/gates.md#real-tailnet-scriptstailnet-gatesh-keyfile).

## Gate environment

- [ ] Unix socket paths are short (about 104 bytes on macOS; see
  `internal/runtime/manager.go`). A long `TMPDIR` can break gates that create
  sockets under it. Run gates with a short temporary directory.
- [ ] Use a free port for each disposable host (`FLATS_GATE_PORT`) and confirm
  that the server you started owns it before sending CLI commands. A failed
  start can leave the port held by another server, and commands then reach
  that one.
