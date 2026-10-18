# Playbook: hangs, freezes and failing gates

## Before blaming the change

- [ ] Run the failing test or gate on main with the same environment and
  compare. A failure that also happens on main is not caused by the change;
  report it separately.
- [ ] Record the exact command, exit status, commit and machine load.

## Hangs and freezes

- [ ] Capture state before restarting anything: a goroutine dump (`SIGQUIT`, or
  `GOTRACEBACK=all`) and, on macOS, `sample <pid>`.
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
