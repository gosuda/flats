# Phase gates: evidence

Measured on the development Mac (Apple Silicon, macOS, Go 1.27.1), 2026-10-03.
"Local network" means `flats serve --network local` (flats at
`http://<flat>.localhost:<port>`), used where a real tailnet was not
available.

## Pull-request baseline

`.github/workflows/ci.yml` runs on every PR and push to main, with read-only
repository permission and no secrets. It uses the Go version from `go.mod`:

```sh
go mod tidy                         # CI requires no go.mod/go.sum diff
go test ./...
go vet ./...
go test -race ./internal/core ./internal/api ./internal/app ./internal/mcpx ./internal/store ./internal/expose/portal
go test -race ./internal/runtime -run 'TestConcurrentFiles|TestWASIRejectsJSHostABI'
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/flats-linux-amd64 ./cmd/flats
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/flats-linux-arm64 ./cmd/flats
```

Tests use disposable hosts/data and local test-control infrastructure. Portal
relay E2E remains opt-in (`FLATS_PORTAL_E2E=1`); ordinary CI never enables it.
Real tailnet/control, Portal relay, Claude/Codex/Cursor agent, launchd/systemd
recovery and reboot gates below remain manual external checks. A Linux cross
build does not establish real Linux service recovery or tailnet behavior.

The new local regressions cover lifetime directory locking (including another
process with different ephemeral listen addresses), complete atomic first-key
publication, concurrent FILES quota/accounting, JS/WASI capabilities, joined
Host shutdown errors, and bounded Portal teardown with stalled SDK/drain/pump
or per-slug locks. The runtime suite shares only a disposable process-local
compilation cache. `scripts/server-flat-gate.sh` can run a disposable local
host with `FLATS_GATE_PORT` and `FLATS_BIN`; it does not need credentials.
Public `go install github.com/gosuda/flats/cmd/flats@latest` must be verified
outside the checkout after the canonical module migration reaches main.

## Phase 0 — spike

| Check | Result | How |
|---|---|---|
| Many tsnet nodes in one process | PASS: 3 and 10 nodes, ~0.33-0.5 s per `Up` | `internal/expose/tsnet` tests against Tailscale's test control server + local DERP |
| Per-node memory | first node ≈14 MB RSS; each further node ≈4.2-6 MB RSS, ≈1 MB live heap, ≈94 goroutines | recon run (10 nodes) and `TestServeHTTPSIdentityStop` (`memory gate:` log line) |
| In-process Portal expose + Hide toggle | PASS: public URL ready in ~0.4-3 s with an explicit relay; `UpdateMetadata` toggles Hide | `FLATS_PORTAL_E2E=1 go test ./internal/expose/portal/` and the gate run below |
| Node key expiry | PASS (test control): expiry detected, node reported `needs-login` with an explanation | `TestPlainHTTPFallbackAndKeyExpiry` |
| wazero + qjs HTTP handler | PASS after forking qjs (v0.0.6 fails on wazero 1.12; see `internal/qjs/NOTICE`) | `internal/runtime` tests |
| **Gate**: 3 flats private and public at once, memory recorded | **PASS**: 3 flats served on the local network and publicly at `https://<flat>.s-h.day` at the same time; RSS 32 MB → 51 MB (≈6 MB per public exposure). Making them private stopped the public URLs. | `flats serve --network local --relays https://s-h.day` |

Against the real Tailscale control server without an auth key: the console
node and a per-flat node both reach `needs-login` and the system status (and
console) shows each node's real `https://login.tailscale.com/a/...` URL; no
node joined the tailnet.

### Real tailnet (`scripts/tailnet-gate.sh <keyfile>`, 2026-10-03)

**PASS** twice on the operator's tailnet (macOS, Tailscale 1.102 client on
the same Mac, reusable untagged auth key), the last run at commit `6771a6c`:

| Check | Result |
|---|---|
| 3 flats + console node, each its own node with a real certificate | ready after 40-41 s; issuer Let's Encrypt; RSS 69 MB with 4 nodes |
| New host reports `starting` until it has its certificate | PASS |
| Preview `<slug>-<8>` (ephemeral node) | ready after 40 s, served the candidate; deploy closed it in under 1 s and the node logged out |
| Delete | the flat's node logged out of the tailnet |
| Management API on a flat host | 404 |
| Identity headers from a real peer (server flat echoing them) | `Tailscale-User-Login` is the peer's real login, `-Name` set, a spoofed `Tailscale-User-Profile-Pic` removed |

Earlier runs failed and found four product bugs, fixed in `c188497` and
`6771a6c`:

1. **Certificates stopped after ~10 new hosts.** Each node registered its
   own Let's Encrypt account; Let's Encrypt answered "too many new
   registrations (10) from this IP address in the last 3h0m0s". All nodes
   now share one account key.
2. **"ready" before HTTPS worked**, and the certificate prewarm gave up
   after 2 minutes without retrying. Hosts are now `starting` until a
   certificate is obtained, and the fetch retries with backoff.
3. **The server froze.** A forked child wedged in Network.framework's
   atfork handler (golang/go#56784, seen with `sample`); the parent held
   `syscall.ForkLock`, which darwin socket creation also takes, so
   certificate requests never returned and one deploy waited 10 minutes for
   its preview to close. The fork was tailscale's LocalAPI client running
   `lsof` on every request; Flats now gives it in-process credentials so it
   never forks, and `internal/forkwatch` kills any child stuck before exec
   for over 10 s.
4. **Unbounded Stop.** Stop and Close now wait at most 30 s.

Notes: the gate reuses a cached Let's Encrypt account
(`~/.cache/flats-gate/`) so repeated runs do not hit the registration limit.
Logged-out nodes stay listed as expired in the admin console until
Tailscale removes them or the operator does.

## Phase 1 — MVP

| Check | Result | How |
|---|---|---|
| 10 MB static upload → live | **PASS**: 33 ms (limit 30 s) | `TestMVPGate` (incompressible 10 MB archive over the HTTP API) |
| Rollback | **PASS**: 1.4 ms (limit 10 s) | `TestMVPGate` |
| No shared origins | **PASS**: every flat and preview has its own host | `TestMVPGate`, `internal/core` tests |
| Failed deploy keeps live | **PASS** | `TestFailedHealthCheckKeepsLive` |
| Claude Code deploys after one setup | **PASS** (19 s) | `scripts/agent-gate.sh` — Claude Code 2.1.287, `--mcp-config` |
| Codex deploys after one setup | **PASS** (49 s) | codex-cli 0.160.0, `mcp_servers.flats.url` + `default_tools_approval_mode="approve"` (needed for `codex exec`; without it Codex refuses MCP calls in non-interactive mode) |
| Cursor deploys after one setup | **PASS** (32 s) | cursor-agent 2026.10.01, `.cursor/mcp.json` |
| launchd install and recovery | **PASS**: installed, `kill -9` → launchd restarted the server after its ~10 s throttle, the flat was restored and served; uninstalled cleanly | manual run with a temporary data dir |
| Reboot recovery | Covered by the same restore path (state is rebuilt from SQLite at start); a real reboot was not performed | |

## Phase 2 — server flats

| Check | Result | How |
|---|---|---|
| JS flat with SQLite deploys, keeps data across versions | **PASS** | `scripts/server-flat-gate.sh` (real binary, worker processes) |
| Rollback restores code, keeps data | **PASS** | same |
| Rollback with `--restore-data` restores the pre-deploy DB snapshot | **PASS** | same; `TestPreDeploySnapshotAndDataRollback` |
| Secrets: operator-set, visible to the flat, never in logs/API | **PASS** | same; `TestSecretsOperatorOnlyAndNeverReturned` |
| Sandbox: host files, parent env/credentials, network, ATTACH / VACUUM INTO, FILES traversal | **PASS**: all blocked; JS has no `fetch`/sockets, so the management API is unreachable | same; `internal/runtime` `TestSandbox`, `TestSQLiteHardening`, `TestVersionSymlinkRefused` |
| Timeouts / memory | infinite loop → 504 after 10 s and the worker keeps serving; memory bomb → 500 in ~0.9 s and recovers; sleeps/timers are bounded | `internal/runtime` tests |
| WebSocket through the parent proxy; WASI `.wasm` handler | **PASS** | `TestWebSocketEcho`, `TestWebSocketSlowClient`, `TestWASIHandler` |
| Latency | JS cold start 139 ms (warm compile cache); request p50 0.37 ms, with DB 0.6-0.8 ms; WASI p50 4.4 ms | `TestLatencyAndConcurrency` |
| Retention, disk quota, skills, console search/grid | **PASS** | core/api/console tests; `claude plugin validate .` |

Known limits (documented in `docs/design.md`): no OS-level sandbox around the
worker (isolation is wasm plus the host API); the 10 s limit is wall clock;
SQLite growth is reported against the disk quota, not hard-capped.

## Phase 3 — polish

| Check | Result | How |
|---|---|---|
| Slug rename with 7-day redirect (path and query kept) | **PASS** | `TestRenameRedirect`, `internal/store` tests |
| Pre-deploy DB snapshot | **PASS** | `TestPreDeploySnapshotAndDataRollback`, server-flat gate |
| Request-count page views | **PASS** (HTML documents only; visitor IPs are not available through Portal) | `TestPageViewsCountHTMLRequests` |
| Thumbnails: agent screenshot, else favicon, else initials | **PASS** | `TestThumbnailFallsBackToFavicon`, console |
| Linux | **PASS**: linux/amd64 and linux/arm64 build CGO-free. On Linux 6.6 aarch64 (Debian 12 containers, non-root user): all 16 package test binaries pass; the server-flat gate passes; `flats install` installs a real systemd user service (systemd 252, lingering user) that comes back after `kill -9` and after a container reboot with the flat served; `flats uninstall` removes it. | cross-compiled `go test -c` binaries and `scripts/server-flat-gate.sh` (`FLATS_BIN`) in containers, run with Rancher Desktop |

## Final review (2026-10-03)

A four-lens adversarial review (security, correctness, spec compliance,
interfaces; every finding checked by two independent verifiers) confirmed 37
findings. All were addressed; independent re-verification confirmed 32 fixed
and the rest either fixed later in this session (snapshot eviction, repeated
restore_data, over-redaction of short values, wording) or documented because
they cannot be enforced without accounts:

- an agent that can send arbitrary HTTP from the operator's own devices can
  imitate the console's browser headers and approve its own request;
- `flats secret set` on the Flats host is available to any process running
  as the operator.

After the fixes: `go test ./...` and `-race` on core/api/app/mcpx/store pass;
the MVP gate (10 MB upload→live 28 ms, rollback 2 ms), the server-flat gate
and the three-agent gate (Claude Code 18 s, Codex 60 s, Cursor 31 s) pass
again.
