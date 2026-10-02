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
