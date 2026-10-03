# Network provider lifecycle

This document describes the integrated provider Manager. Production app and disposable gate host wire the exact exported core `LifecycleNet`, observer and preview contracts through `Config.Lifecycle`; legacy `Config.Public` is unset. Historical branch/test checkpoints below are receipts, not the current integration boundary.

## Historical revisions

- `git cat-file -p 29edc2a6e13e27867603a23ff5b1b5a8d1b84df0` shows one parent, `313ef2557d28350cb91a0daff719aca5bf6813c9`. `29edc2a6` is the child. `313ef255` is the parent. `git merge-base --is-ancestor 313ef2557d28350cb91a0daff719aca5bf6813c9 29edc2a6e13e27867603a23ff5b1b5a8d1b84df0` exits 0, and the reverse exits 1.
- This branch started at the child `29edc2a6e13e27867603a23ff5b1b5a8d1b84df0`.
- Grant and Funnel listener commit: `e906e929b8312cdf28d5df15e04bd344573db6b0`.
- AllowFunnel rollback: `992e39afdf57c8dfbd154352b20d8c5cbdcd79bf`.
- The commit that corrects this ancestry note and `--portal=false` is the candidate HEAD when it is the branch tip. The worker result repeats `git rev-parse HEAD`.

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
grants `portal` and starts it. `--relays` does not grant portal.

`--portal=false` keeps a portal grant that is already stored and does not
start Portal for this process. The legacy public network stays unset and the
manager receives no portal backend, so this process cannot publish through
Portal. The next start that omits `--portal` uses the stored grant and starts
Portal again. Omitting `--portal` never creates a grant by itself.

On a later start, if `--network` is omitted, `private_backend` is `tailscale`,
and that grant exists, the legacy private path is tsnet again. A backend is
constructed only for a granted provider that this process is allowed to start.
Local is always constructed.

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

Manager implements the exact exported core contracts and aliases core DTOs; core does not import expose. Policy tokens include host grants and effective backend configuration, excluding transient readiness. Per-flat permissions are read freshly for status. In-use revocation is refused before any policy write, including starting/error routes, previews and redirects. Public may commit with a registered permitted/configured current route in `starting`; links remain unavailable until ready. Teardown uses confirmed Manager stops across delete/rename/expiry, retaining unconfirmed routes for retry.

Legacy schema 5 eligibility is consumed once by an explicit Private Tailscale/Local upgrade choice. An ambiguous historical-default host refuses before networking; the choice never authorizes Public providers. See README for operator credential-file/install and upgrade commands.

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
private listener. If the entry is still set after that close, `StopFunnel`
returns an error so the caller reports the stop as unconfirmed. If Funnel
created the node and then fails, `Stop` rolls the new node back. If private
HTTP already owned the node, the failure leaves that node up and closes only
the Funnel listener. A failed `ListenFunnel` that introduced `AllowFunnel`
and then returned no listener is cleared the same way; an entry that was
already present is left alone. Funnel stays `starting` until the
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

## Historical network-slice tests

Commands run from this worktree. Exit 0 for each.

```text
go test ./internal/expose/provider/ ./internal/app/ ./internal/expose/portal/ ./internal/expose/tsnet/ -count=1 -timeout 300s
```

`provider` 0.709s, `app` 1.219s, `portal` 2.816s, `tsnet` 8.221s.

```text
go test -race ./internal/app ./internal/expose/portal ./internal/expose/provider -count=1 -timeout 300s
```

`app` 4.756s, `portal` 3.187s, `provider` 3.525s.

```text
go test ./internal/expose/tsnet/ ./internal/expose/provider/ -count=1 -timeout 180s
```

After the AllowFunnel rollback: `tsnet` 7.375s, `provider` 0.711s. Exit 0.

`TestServeFunnelRoutesIngressAndLeavesPrivate` uses the existing testcontrol
harness, not a live tailnet. It serves private HTTPS first. `ListenFunnel`
without `FunnelOnly` returns an error containing `listener already open` and
leaves `AllowFunnel` set; the test asserts that entry, then clears it so the
later stop assertion belongs to our listener. A FunnelOnly attempt that
writes `AllowFunnel` and then returns that same listen error is rolled back:
state is `error`, `AllowFunnel` is clear, and private HTTPS still serves.
The successful `FunnelOnly` listener then shares `:443` with the private
listener. Ingress through PeerAPI `/v0/ingress` receives the public handler.
A tailnet client still receives the private handler. `StopFunnel` clears
`AllowFunnel` and ingress fails while private HTTPS still serves. A later
failure after the listener exists closes it, leaves state `error`, clears
`AllowFunnel`, and does not replace the private route. The verbose run of
this test passed in 1.15s, with certificate SNI `flat.tail-scale.ts.net`
twice.

Provider tests cover historical directories, the rejected `funnel` alias, a
grant that does not serve, draft and private requests that do not call public
backends, a Funnel failure that does not call Portal, and a public stop that
leaves local HTTP up and reports Funnel unconfirmed when `StopFunnel` fails.
App tests cover default flags, a data directory whose old tsnet and portal
files do not construct those backends, and `TestExplicitPortalFalseKeepsGrantAndDoesNotStart`:
`--portal` stores the grant and starts Portal, `--portal=false` leaves that
grant stored and starts nothing, and a later start that omits the flag starts
Portal from the stored grant. That test passed in 0.03s (`go test ./internal/app/ -count=1 -run TestExplicitPortalFalseKeepsGrantAndDoesNotStart -timeout 60s`, exit 0).

Not run, because this task does not authorize live publication or a new
tailnet node: `FLATS_PORTAL_E2E`, `scripts/tailnet-gate.sh`, and any command
against an operator data directory or auth key. `cmd/flats` has no test files;
flag parsing lives in `internal/app`.

## Limitations

- Core integration uses the exact exported `LifecycleNet`, `LifecycleObserver`
  and `LifecyclePreviewNet` contracts through `Config.Lifecycle`. The app and
  deterministic integration host leave legacy `Config.Public` unset. Private and
  public current routes are rebound through Manager before public activation.
- Portal SDK end-to-end publication was not repeated. The manager calls
  `PortalNet`; policy tests use a loopback double for call counts.
- Owner-only authentication is deferred. Private access remains the tailnet ACL.
- A restart that omits `--portal` starts Portal only when a grant is already
  stored. `--portal=false` does not delete that grant and does not start
  Portal for that process. Operators who relied on the old default must pass
  `--portal` or `--permit portal` once; after the grant is stored, omitting
  the flag starts Portal again.

## Historical interface-only checkpoint

The purpose integration worktree adopted the exact frozen network, console and
gate artifacts without conflicts, followed by core prerequisite
`d0d4aaf07a86024469e4ad43034230d15a33f7d7`. Manager aliases the exact exported
core DTOs and has a compile-time `core.LifecycleNet` assertion. App and the test
host now assign `core.Config.Lifecycle`; the legacy Public fallback is unset.
At this checkpoint the prerequisite defined interfaces only; later exact
committed grants supplied core lifecycle behavior and operator authority.

Startup assigns a Portal interface only when its concrete backend is non-nil.
The extended `TestExplicitPortalFalseKeepsGrantAndDoesNotStart` calls Manager
with an explicitly permitted Portal request after runtime disable and requires
`ErrNotConfigured` plus an unavailable endpoint, while retaining the host grant.
This avoids the typed-nil interface panic and preserves `--portal=false`.

The integrated Manager implements `LifecycleObserver` and
`LifecyclePreviewNet` from the granted core exports. Host permission plus
effective runtime configuration is hashed into approval policy; readiness is
observed separately. Explicit `--portal=false` changes effective policy while
retaining the grant. Canonical nonsecret relay origins and settings bind provider
configuration changes without binding secrets or transient readiness.

Routes and observed endpoints are keyed by exact host, audience and provider.
Preview cleanup stops only Draft private routes for the requested host; another
preview and the current site remain reachable. Public opening preserves an
independent Local route on a fresh Manager, and normal core rebinding refreshes
both current handlers before each public activation.

### Per-flat Private Tailscale revocation

An operator-validated revocation confirms Manager teardown of that flat’s Tailscale current, preview and redirect registrations before writing the denied permission. Local remains registered. The real tsnet backend uses `StopPrivate`: with a sibling Funnel request it closes only Private HTTP and retains the node and approved Funnel listener; without Funnel it retires the node normally. Private listener setup and teardown are serialized so asynchronous startup cannot reopen a revoked route. Unconfirmed node retirement is retained across retries; absence from the active-node map does not prove teardown completed. Legacy backends without Private-only stop support refuse if a Funnel registration shares the host. Public provider revocation continues to require an approved Public-to-Private transition.
