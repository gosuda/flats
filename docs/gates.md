# Phase gates: evidence

Measured on the development Mac (Apple Silicon, macOS, Go 1.27.1), 2026-10-03.
"Local network" means `flats serve --network local` (flats at
`http://<flat>.localhost:<port>`), used where a real tailnet was not
available.

## Phase 0 — spike

| Check | Result | How |
|---|---|---|
| Many tsnet nodes in one process | PASS: 3 and 10 nodes, ~0.33-0.5 s per `Up` | `internal/expose/tsnet` tests against Tailscale's test control server + local DERP |
| Per-node memory | first node ≈14 MB RSS; each further node ≈4.2-6 MB RSS, ≈1 MB live heap, ≈94 goroutines | recon run (10 nodes) and `TestServeHTTPSIdentityStop` (`memory gate:` log line) |
| In-process Portal expose + Hide toggle | PASS: public URL ready in ~0.4-3 s with an explicit relay; `UpdateMetadata` toggles Hide | `FLATS_PORTAL_E2E=1 go test ./internal/expose/portal/` and the gate run below |
| Node key expiry | PASS (test control): expiry detected, node reported `needs-login` with an explanation | `TestPlainHTTPFallbackAndKeyExpiry` |
| wazero + qjs HTTP handler | PASS after forking qjs (v0.0.6 fails on wazero 1.12; see `internal/qjs/NOTICE`) | `internal/runtime` tests |
| **Gate**: 3 flats private and public at once, memory recorded | **PASS**: 3 flats served on the local network and publicly at `https://<flat>.s-h.day` at the same time; RSS 32 MB → 51 MB (≈6 MB per public exposure). Making them private stopped the public URLs. | `flats serve --network local --relays https://s-h.day` |

Not yet verified on the operator's real tailnet (needs an auth key): ACME
certificates, node removal on logout, re-login URL after key expiry.

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
