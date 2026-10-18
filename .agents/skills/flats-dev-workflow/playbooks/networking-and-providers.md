# Playbook: network providers, console domains and reverse proxies

Read [Private and public access](../../../../docs/networking.md) and the
[network provider lifecycle](../../../../docs/network-provider-lifecycle.md)
before changing a provider.

## Tailscale (tsnet)

- [ ] In a tailnet with MagicDNS and HTTPS certificates enabled, every tsnet
  node gets its own Let's Encrypt certificate. Let's Encrypt rate-limits new
  accounts per IP address and certificates per name, so keep the shared
  account key and the gate's cached key, and avoid needless reruns. There a
  host is usable only when Flats reports `ready`, not `starting`.
- [ ] Without HTTPS certificates, a node serves plain HTTP inside the tailnet
  and reports `ready` with the detail `HTTPSUnavailable`
  (`internal/expose/tsnet/tsnet.go`, `TestPlainHTTPFallbackAndKeyExpiry`).
  That is the supported fallback, not broken provisioning. See
  [Real tailnet](../../../../docs/gates.md#real-tailnet-scriptstailnet-gatesh-keyfile).
- [ ] A logged-out node stays listed as expired in the tailnet and keeps its
  name, so a new node with that name gets a suffix (for example `flats-1`).
  Reuse a test name only after the old machine is removed, or pick a new name.
- [ ] Clean up test nodes: delete test flats through approvals and log out the
  stopped console node with `go run ./scripts/tsnet-logout <state-dir>`.

## Portal

- [ ] An unreachable explicit relay does not keep a test offline: with
  `portal.discovery` on (the default), Portal still adds discovered relays.
  Set `portal.discovery: false` before making a development flat Public when
  the test must stay off the internet
  ([config.json](../../../../docs/configuration.md#configjson)).
- [ ] A network path that intercepts TLS breaks Portal. Flats reports it as
  `TLS interception detected` (`internal/expose/portal`). Verify public HTTPS
  from an uninspected network; a `ready` relay status shows registration, not
  working HTTPS.
- [ ] Portal SDK shutdown: a context-triggered `Exposure.Close` returns its
  error only to the first caller. Model that in shutdown tests, as
  `internal/expose/portal/sdk_shutdown_test.go` does; independent listener
  fakes miss it.

## NetBird

- [ ] NetBird stays in the nested `netbird/` module. The root `go.mod` has no
  `replace` directive and the root `cmd/flats` must not link NetBird; the CI
  `netbird` job checks both ([CI baseline](../../../../docs/gates.md#pull-request-baseline)).

## Console origins

- [ ] A reverse proxy on loopback does not make its remote clients local.
  Requests through a declared external origin carry that origin in the request
  context (`api.ExternalOrigin`) and are never treated as loopback callers
  (`isLoopback` in `internal/api`, `fromLoopback` in `internal/mcpx`), so
  host-local operations such as `save_draft_from_dir` stay refused. Keep both
  checks on every new route.
- [ ] The MCP SDK has its own localhost Host check. Only the external-origin
  handler disables it, after `HostGuard` has validated the origin.
- [ ] Test through a real loopback reverse proxy: MCP initialization works and
  local-only operations are refused. See
  [Serving the console behind a reverse proxy](../../../../docs/networking.md#serving-the-console-behind-a-reverse-proxy).
