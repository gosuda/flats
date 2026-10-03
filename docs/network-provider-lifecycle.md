# Network provider lifecycle

This document is the network-provider candidate on `oesni/flats-lifecycle-network`.
It covers host permission, selectable private and public providers, and the
Tailscale Funnel path. Core, store, API, CLI, MCP, bundle, and the console are
owned elsewhere and are not wired to `ServeExposure` on this branch.

## Revisions

- Verified `origin/main` and the branch start: `29edc2a6e13e27867603a23ff5b1b5a8d1b84df0`.
- The brief's observed `313ef2557d28350cb91a0daff719aca5bf6813c9` is a child of that commit and was not the live tip after fetch.
- Candidate HEAD is the commit that adds this file. The worker result repeats `git rev-parse HEAD`.

## Contract

Provider choice stays separate from visibility and version. Local loopback is
available with no grant. Tailscale, Tailscale Funnel, and Portal start only
when both of these are true:

1. The host file grants that provider.
2. The exposure request lists it in `Permitted`.

A grant, a tailnet login, or a running node does not publish a flat. `"funnel"`
is not an alias of `tailscale-funnel`. A failed provider is not replaced by
another one. Both requested public providers are attempted independently; that
is not a fallback.

| Request | Providers that may open |
| --- | --- |
| Draft, or visibility private | `local` always. `tailscale` only when both gates allow it. |
| Audience current and visibility public | `tailscale-funnel` and/or `portal`, each only when both gates allow it. |
| Ephemeral preview | Private paths only. Public providers return `provider not permitted`. |

If a public request lists no permitted public provider, or none of the
requested public routes open, `ServeExposure` returns an error. If some open
and some fail, the result and the joined error are both returned.

`StopPublicRoutes` closes Funnel and Portal for that slug. Local and tailscale
routes stay up. A stop error, or a Funnel state that is still `ready` or
`starting`, is reported in `Unconfirmed` and the route stays tracked so a later
call can retry. Unconfirmed means the caller must not treat the flat as
private. This does not remove an allowed private tailnet ACL, and it does not
implement owner-only authentication.

Connection states used here are `starting`, `ready`, `error`, and
`unavailable`. A node that is on the tailnet without private HTTP is `idle`
and is not ready.

## Host file

`network-provider.json` (mode `0600`, version 1) lives in the data directory.
A missing file is written with an empty `permitted` list and a migration
record. Existing `tsnet/<node>/` directories and `portal/*.json` files are
recorded in `migration` and are not grants. `grants_from_state` must stay
empty; validation rejects a non-empty list. `private_backend` is `local` or
`tailscale` and is not itself a grant. Auth keys are not stored in this file.

## Flags

Defaults changed so a process does not opt in by omission:

- `--network` defaults to `local`. The previous default was `tailscale`.
- `--portal` defaults to `false`. The previous default was `true`.
- `--permit` accepts a comma-separated list of canonical ids.

`--network tailscale` grants `tailscale` only and sets `private_backend`. It
does not grant Funnel. `--network local`, when the flag is present, sets
`private_backend` to `local` and does not revoke other grants. `--portal`
grants `portal`. `--portal=false` does not revoke a stored portal grant and
does not attach the legacy public network. `--relays` does not grant portal.

On a later start, if `--network` is omitted, `private_backend` is `tailscale`,
and that grant exists, the legacy private path is tsnet again. A backend is
constructed only for a granted provider. Local is always constructed.

`scripts/tailnet-gate.sh` already passes `--network tailscale --portal=false`
on a fresh data directory, so those explicit flags still grant tailscale and
not portal. That script was not run here.

## Adapters

`provider.New(dir, provider.Options)` loads the host file and keeps the
backends. It does not serve a flat.

- `Options.Local` is required.
- `Options.Tailscale` is the `Tailnet` interface. `provider.TSNet` adapts `*tsnet.Net` and adds `FunnelState`.
- `Options.Portal` is the `PortalNet` interface. `*portal.Net` satisfies it.

A non-nil backend is not permission. Tests inject fakes through these
interfaces. There is no permit-skipping constructor.

`*provider.Manager` does not implement `core.PrivateNet` or `core.PublicNet`.
`internal/expose` already imports `internal/core`, so core must not import this
package. `ExposureRequest`, `ExposureResult`, and `PublicStopResult` match the
core contract field layout. This branch's core does not yet export those types
or a lifecycle field on `core.Config`, so `app.Host.Providers` holds the
manager directly. Legacy `Config.Private` and `Config.Public` stay in place
for the core that is on this branch.

## Funnel

Public Tailscale is `tsnet.Server.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())`
on tailscale.com v1.104.0. It is not Serve.

Pinned `registerListener` keys a listener by network, address, port, and a
funnel boolean. Private HTTPS uses `Listen("tcp", ":443")`, which stores
`funnel=false`. `ListenFunnel` without `FunnelOnly` is listen-on-both and
returns `listener already open for tcp, :443` when that key exists. That
failed call can still write `AllowFunnel` before listen returns. Production
always passes `FunnelOnly`, which stores only `funnel=true`, so the two
listeners share port 443 and stay independent. Ingress is dispatched with
`funnel=true` and does not enter the private handler.

`StopFunnel` closes that listener. `cleanupListener.Close` deletes the
`AllowFunnel` entry. It does not log the node out and it does not close the
private listener. If Funnel created the node and then fails, `Stop` rolls the
new node back. If private HTTP already owned the node, the failure leaves that
node up and closes only the Funnel listener. Funnel stays `starting` until the
certificate fetch succeeds, unless a test certificate source is installed, in
which case it is `ready` immediately. Funnel strips `Tailscale-User-*` headers
and does not set identity.

## Shutdown

`Host.Close` still drains the management HTTP server before it closes
networks. It then closes core, provider routes, the legacy public backend or
the portal backend, the private backend, and any tailnet or local backend that
was not that private pointer. `Manager.Close` stops routes only. It does not
call `portal.Net.Close` and it does not cancel the Portal parent context.
Portal SDK shutdown inside `portal.Net.Close` is unchanged.

## Tests

Commands run from this worktree. Exit 0 for each.

```text
go test ./internal/expose/provider/ ./internal/app/ ./internal/expose/portal/ ./internal/expose/tsnet/ -count=1 -timeout 300s
```

`provider` 0.709s, `app` 1.219s, `portal` 2.816s, `tsnet` 8.221s.

```text
go test -race ./internal/app ./internal/expose/portal ./internal/expose/provider -count=1 -timeout 300s
```

`app` 4.756s, `portal` 3.187s, `provider` 3.525s.

`TestServeFunnelRoutesIngressAndLeavesPrivate` uses the existing testcontrol
harness, not a live tailnet. It serves private HTTPS first, asserts that
`ListenFunnel` without `FunnelOnly` collides on `:443`, clears the `AllowFunnel`
leak that collision leaves behind, then opens Funnel with `FunnelOnly`.
Ingress through PeerAPI `/v0/ingress` receives the public handler. A tailnet
client still receives the private handler. `StopFunnel` clears `AllowFunnel`
and ingress fails while private HTTPS still serves. A later Funnel setup
failure does not replace the private route.

Provider tests cover historical directories, the rejected `funnel` alias, a
grant that does not serve, draft and private requests that do not call public
backends, a Funnel failure that does not call Portal, and a public stop that
leaves local HTTP up and reports Funnel unconfirmed when `StopFunnel` fails.
App tests cover default flags and a data directory whose old tsnet and portal
files do not construct those backends.

Not run, because this task does not authorize live publication or a new
tailnet node: `FLATS_PORTAL_E2E`, `scripts/tailnet-gate.sh`, and any command
against an operator data directory or auth key. `cmd/flats` has no test files;
flag parsing lives in `internal/app`.

## Limitations

- Core on this branch cannot call `ServeExposure` yet. Until root adopts the
  exported core types, already-public flats on a granted Portal still use the
  legacy `Config.Public` path. New publication must go through `ServeExposure`
  and a per-flat permit list. The manager itself never starts an unlisted
  provider.
- Portal SDK end-to-end publication was not repeated. The manager calls
  `PortalNet`; policy tests use a loopback double for call counts.
- Owner-only authentication is deferred. Private access remains the tailnet ACL.
- A restart that omits `--portal` will not attach Portal unless a previous
  explicit grant exists, and `--portal=false` still withholds the legacy public
  attachment. Operators who relied on the old default must pass `--portal` or
  `--permit portal`.
