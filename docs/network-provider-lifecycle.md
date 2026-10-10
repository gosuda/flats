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
available with no grant. Tailscale, Tailscale Funnel, Portal and zrok start
only when both of these are true:

1. The host file grants that provider.
2. The exposure request lists it in `Permitted`.

A grant, a tailnet login, or a running node does not publish a flat. `"funnel"`
is not an alias of `tailscale-funnel`. A failed provider is not replaced by
another one. Every requested public provider is attempted independently; that
is not a fallback.

| Request | Providers that may open |
| --- | --- |
| Draft, or visibility private | `local` always. `tailscale` only when both gates allow it. |
| Audience current and visibility public | `tailscale-funnel`, `portal` and/or `zrok`, each only when both gates allow it. |
| Ephemeral preview | Private paths only. Public providers return `provider not permitted`. |

If a public request lists no permitted public provider, or none of the
requested public routes open, `ServeExposure` returns an error. If some open
and some fail, the result and the joined error are both returned.

`StopPublicRoutes` closes Funnel, Portal and zrok for that slug. Local and tailscale
routes stay up. A stop error, or a Funnel state that is still `ready` or
`starting`, is reported in `Unconfirmed` and the route stays tracked so a later
call can retry. Unconfirmed means the caller must not treat the flat as
private. This does not remove an allowed private tailnet ACL, and it does not
implement owner-only authentication. When the stopped Funnel route has no
Private Tailscale sibling, its now-unused node identity is retired after the
listener is confirmed closed. This prevents completed cleanup from leaving a
Funnel-only identity behind. Listener closure and identity retirement
are separate confirmations: if the listener and `AllowFunnel` entry are gone
but control is unavailable for logout, Funnel is reported in `Stopped`, the
visibility transition may complete, and the failed identity cleanup is retained
durably as a non-public obligation across process restart. `StopSlug`, a later Funnel activation, and Funnel
permission-revocation inspection retry it. Revocation remains refused until
that already-authorized cleanup confirms.

Connection states used here are `starting`, `ready`, `error`, `stopped`, and
`unavailable`. A node that is on the tailnet without private HTTP is `idle`
and is not ready. Successful route teardown prunes its observed endpoint;
`stopped` is reserved for stale observations whose registration is already
absent. An endpoint that was refused or failed before registration keeps its
original `unavailable` or `error` detail instead of being mislabeled stopped.
Failed teardown keeps the registration and observation for retry.

## Host file

Host grants now live in `config.json` (`network.permitted` and
`network.private_backend`; see [Configuration and storage](configuration.md)).
`network-provider.json` and the serve flags below are read only by a host
started without `--config` before its one-time migration, which converts them
and moves the file into `backups/`. The rest of this section describes that
legacy input.

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

The console's Settings page can also turn a provider on or off for the host
(`PUT /console/api/providers/{id}` with `{"enabled": bool}` and the settings
ETag in `If-Match`). Turning one on saves `network.permitted` in config.json,
updates the running Manager's grants (`SetGrant`) and attaches the provider's
backend (`AttachTailnet`/`AttachPortal`, nil to set only); constructing a
backend does not contact a tailnet or relay. Portal started this way uses the
Portal settings saved at that moment. Turning one off is refused with
`provider_in_use` while any flat still allows it, and `local` cannot be turned
off. A legacy service whose `--portal`, `--permit` or `--network tailscale`
flag fixes a provider refuses the change, and Tailscale cannot be turned off
while it carries the console and private routes.

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

Manager implements the exact exported core contracts and aliases core DTOs; core does not import expose. Policy tokens include host grants and effective backend configuration, excluding transient readiness. Per-flat permissions are read freshly for status. In-use revocation is refused before any policy write, including starting/error routes, previews and redirects. Public may commit with a registered permitted/configured current route in `starting`; links remain unavailable until ready. A Funnel node waiting for login or device approval remains a registered, permitted route in `needs-login`; it is not reported as an absent provider permission. Teardown uses confirmed Manager stops across delete/rename/expiry, retaining unconfirmed routes for retry. `StopSlug` is the authoritative delete/redirect-expiry operation. It includes the exact slug as a deterministic tsnet identity candidate even when no Funnel or Tailscale route remains in memory, then stops every Funnel listener and confirms every candidate node retirement before it stops Portal or Local. A public listener or node failure therefore leaves Local reachable and keeps the appropriate route or cleanup record for retry; sibling slug routes are untouched. When no tailnet backend exists because neither Tailscale provider is granted, `StopSlug` removes only the validated host's local `tsnet/<host>` directory. It does not start a backend or contact control, and it prevents a later flat with the same slug from loading the historical identity. The old offline machine may remain visible in the tailnet administration plane until it expires or an operator removes it.

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

Funnel registration is asynchronous. `ServeFunnel` records the handler and a
`starting` route, starts at most one background listener attempt, and returns
without waiting for control, login, or certificate readiness. `FunnelStatus`
reports the later ready or error result. Setup and stop are serialized, so a
listener cannot appear after a confirmed stop and concurrent registrations do
not open duplicate listeners.

A successful Funnel-only Public-to-Private transition logs the node out and
removes its certificate cache. A later Private-to-Public transition therefore
creates a new node identity and obtains a new certificate. This deliberately
keeps delete, alias expiry, and permission-revocation cleanup deterministic,
at the cost of additional certificate issuance. Repeated approved visibility
cycles can encounter certificate-authority rate limits and leave Funnel waiting
for certificate readiness. Visibility changes require explicit approval and
are expected to be infrequent; operators should avoid using them as a rapid
on/off control.

`StopFunnel` closes that listener. `cleanupListener.Close` deletes the
`AllowFunnel` entry. It does not log the node out and it does not close the
private listener. If the entry is still set after that close, `StopFunnel`
returns an error so the caller reports the stop as unconfirmed. If Funnel
created a new persistent identity and then fails, `Stop` rolls that identity
back. Identity provenance is the `tailscaled.state` file observed before node
startup: a state that existed before this registration is preserved across
listener failure and process restart. If private HTTP already owned the node,
the failure leaves that node up and closes only the Funnel listener. A failed `ListenFunnel` that introduced `AllowFunnel`
and then returned no listener is cleared the same way; an entry that was
already present is left alone. Funnel stays `starting` until the
certificate fetch succeeds, unless a test certificate source is installed, in
which case it is `ready` immediately. Funnel strips `Tailscale-User-*` headers
and does not set identity.

## zrok

`zrok.Net` (`internal/expose/zrok`) serves a slug as a zrok public share of
the operator's enabled zrok environment. Constructing it reads that
environment from disk and contacts nothing. `Serve` reserves the name
`<slug>` in `zrok.namespace` when the account does not own it yet, removes a
share of this environment that still holds the name (a crash leaves one
behind; unsharing another environment's share fails, so a name is never taken
over), creates the share and returns its frontend URL. Binding the share on
the zrok overlay is asynchronous: the route is `starting` until the listener
is established, then `ready`, or `error` with the reason. A name another
account owns is refused with a rename hint.

`Stop`, used by `StopPublicRoutes`, drains HTTP, closes the overlay listener
and deletes the share; the name stays reserved, so a later Public transition
gets the same URL. A failed unshare keeps the route registered and reports
zrok in `Unconfirmed`. `Retire`, used by `StopSlug` for a deleted flat or an
expired redirect, also releases the name. `Serve` records each name in
`<data>/zrok/<slug>` before reserving it and `Retire` removes the record, so
`StopSlug` releases a name the host still holds even without a route record:
after a restart, while the flat was private or after its zrok permission was
revoked. A failed release keeps the record and fails `StopSlug` for retry;
it is a cleanup obligation, not a public route. With no zrok backend
configured, `StopSlug` refuses while a record exists, so the operator turns
zrok on again before deleting the flat. Records also carry the namespace,
whether Flats created the name and the token of any share Flats created:
`Serve` removes only a share whose token Flats recorded, `Retire` releases
only a name Flats created and leaves a name another share now uses, and a
changed `zrok.namespace` releases the old namespace's name first. The
controller returns frontend endpoints as bare host names; Flats reports them
as `https://` URLs.

`Serve` writes the record with the intent to create a name before asking the
controller, so a name created by a request whose response was lost is still
released later. A share whose token cannot be recorded is deleted at once;
if that also fails, its token is kept in memory so the next `Serve` or
`Retire` of the slug removes it. `Stop` unshares with its own deadline after
the HTTP drain, so a drain timeout does not leave the share behind. A record
that cannot be inspected makes `StopSlug` fail instead of skipping it. The
controller client checks its version once, under the operation's context.

A share is Flats' own when its token is recorded or when the controller
reports it belongs to this environment with this host's target
`flats:<instance id>:<slug>`, which also covers a share whose creation
response was lost; another Flats host on the same zrok environment never
takes it. Records name the
account (a hash of its token, not the token): after `zrok.environment` moves
to another account, `Serve` and `Retire` refuse and keep the record until the
original account is configured again or the operator removes the record.
A different fingerprint whose account still holds the recorded name, as after
a regenerated account token, updates the record instead. `Serve` uses an
existing name only with this host's record: a name reserved in the account by
the zrok CLI or another Flats host is refused. A creation attempt is recorded
as pending; the next `Serve` adopts the name if the account holds it, an
`errNameExists` answer drops the record (another account owns the name), and
`Retire` releases a pending name the account holds. A share whose creation
failed, or whose rollback failed, is kept as unsettled: `Stop` (for example
when a failed Public approval rolls back) and process shutdown remove it
when this host owns it. An
unshare the controller answers with "not found" is confirmed through the
account-wide share detail: a share another environment of the account still
holds is reported, not treated as gone. A `Serve` after a failed `Stop`
finishes that stop before opening a fresh share.

A listener the overlay closes is rebound with backoff (2 s, doubling to
60 s). Requests that the lost listener already accepted are drained before
rebinding, and `Stop` drains every server that may still hold connections
before deleting the share. Process shutdown deletes every share and keeps
the names.

## Shutdown

`Host.Close` drains the management HTTP server before it closes networks. It
then closes core, forgets Manager registrations, closes the legacy public
backend or the portal backend, the private backend, and any tailnet or local
backend that was not that private pointer. `Manager.Close` does not call
destructive per-route stop methods. The backend owner performs process
shutdown: `tsnet.Close` preserves persistent node state and identity, while
explicit delete, revocation, and preview close continue to use logout. Portal
SDK shutdown inside `portal.Net.Close` is unchanged. Startup-failure cleanup
uses the same identity-preserving `Host.Close` path.

Explicit retirement is confirmed only after control accepts logout. If that
request fails, tsnet closes the now-consumed backend but retains its disk state
or ephemeral memory store and keeps the retirement registered. A later
`Stop` or `StopPrivate` creates a fresh backend without an auth key, reloads
that retained node key, and retries logout once LocalBackend has installed its
control client. It does not require `Server.Up`: expired and unapproved keys
can be retired from `NeedsLogin`, `NeedsMachineAuth`, or the post-start state.
The last state observed while Running is restored after a failed logout so a
later same-process attempt still has the identity needed for the control-plane
logout. Full `Stop` also checks the deterministic host state directory when no
node is running. If `tailscaled.state` exists after a restart, it installs the
same fail-closed retirement ledger and uses a fresh backend to confirm logout;
if no identity state exists, it returns without starting a backend that could
mint a node accidentally. A successor cancelled before its backend starts is confirmed retired
when its predecessor has finished and no `tailscaled.state` remains. State is
deleted only after confirmation. `Net.Close` waits for all in-flight retirement attempts,
closes every remaining backend, and reports any retained failure.

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
Confirmed stops prune the matching endpoint state, and Manager shutdown clears
all observations, so closed previews and routes do not accumulate in status.
Preview cleanup stops only Draft private routes for the requested host; another
preview and the current site remain reachable. Public opening preserves an
independent Local route on a fresh Manager, and normal core rebinding refreshes
both current handlers before each public activation.

### Default Private Tailscale permission

On a host whose `network.private_backend` is `tailscale`, creating a flat (by
an agent, or by its first save) stores its Tailscale permission in the same
transaction as the flat and logs a `provider` event. Choosing the tailnet for
the console and private routes is the operator's grant for Private routes of
new flats; Funnel and Portal still need their own per-flat permission. A host
on the `local` backend keeps new flats Local-only. Existing flats are not
changed.

### Per-flat Private Tailscale revocation

An operator-validated revocation confirms Manager teardown of that flat’s Tailscale current, preview and redirect registrations before writing the denied permission. Local remains registered. The real tsnet backend uses `StopPrivate`: with a sibling Funnel request it closes only Private HTTP and retains the node and approved Funnel listener; without Funnel it retires the node normally. Private listener setup and teardown are serialized so asynchronous startup cannot reopen a revoked route or race a second teardown. An in-progress or terminally failed retirement remains fail-closed. A terminal logout failure closes the consumed backend and preserves its persistent state; the next `StopPrivate` call starts a fresh backend over that state and performs a real logout retry in the same process. Absence from the active-node map does not prove teardown completed. Legacy backends without Private-only stop support refuse if a Funnel registration shares the host. Public provider revocation continues to require an approved Public-to-Private transition.
