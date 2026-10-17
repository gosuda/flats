# Network provider lifecycle

This document describes the provider Manager (`internal/expose/provider`) and
the backends it drives. The app and the deterministic integration host wire it
into core through `Config.Lifecycle`, which carries the exported
`LifecycleNet`, `LifecycleObserver` and `LifecyclePreviewNet` contracts
([Network contract](lifecycle-core-contract.md#network-contract)); legacy
`Config.Public` stays unset. Operator setup is in
[Private and public access](networking.md).

## Contract

Provider choice stays separate from visibility and version. Local loopback is
available with no grant. Tailscale, NetBird, Tailscale Funnel, Portal, zrok,
Cloudflare quick tunnels (`cloudflare-quick`) and NetBird Expose start
only when both of these are true:

1. The host grants that provider (`network.permitted`).
2. The exposure request lists it in `Permitted`.

A grant, a tailnet login, or a running node does not publish a flat. `"funnel"`
is not an alias of `tailscale-funnel`. A failed provider is not replaced by
another one. Every requested public provider is attempted independently; that
is not a fallback.

An explicit per-flat public-provider grant on a live Public flat reconciles
that provider immediately. The request retains the observed non-stopped
permitted public siblings and private routes, using the effective Portal
listing preference. It does not reopen stopped siblings or add other merely
permitted public providers. On a Private flat or without a live runtime, a
public grant records permission only. A failed startup keeps the grant and
endpoint error, logs an exposure error and returns `provider_not_ready`;
repeating the grant retries. Asynchronous registration may return `starting`
and later become `ready` or `error` through ordinary status polling.

| Request | Providers that may open |
| --- | --- |
| Draft, or visibility private | `local` always. `tailscale` and `netbird` only when both gates allow them (`netbird` never for an ephemeral preview). |
| Audience current and visibility public | `tailscale-funnel`, `portal`, `zrok`, `cloudflare-quick` and/or `netbird-expose`, each only when both gates allow it. |
| Ephemeral preview | Private paths only. Public providers return `provider not permitted`. |

If a public request lists no permitted public provider, or none of the
requested public routes open, `ServeExposure` returns an error. If some open
and some fail, the result and the joined error are both returned.

`StopPublicRoutes` closes Funnel, Portal, zrok, quick tunnels and NetBird Expose for that slug. Local and tailscale
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

## Host grants

Host grants live in `config.json` (`network.permitted` and
`network.private_backend`; see [Configuration and storage](configuration.md)).
The app passes them to the Manager as `Options.Grants`, so the Manager neither
reads nor writes a host file, and `Reload` keeps those grants until the process
restarts. `local` needs no grant and cannot be turned off.

The console turns a provider on or off for the host with
`PUT /console/api/providers/{id}` `{"enabled": bool}` and the settings ETag in
`If-Match` ([HTTP routes](surfaces.md#http-routes)). Turning one on saves
`network.permitted`, updates the running Manager (`SetGrant`) and attaches the
provider's backend (`AttachTailnet`, `AttachPortal`, `AttachZrok`,
`AttachCFQuick`, `AttachNetbird`; nil to set only). Constructing a backend does
not contact a tailnet, relay or controller. Portal started this way uses the
Portal settings saved at that moment. Turning one off is refused with
`provider_in_use` while any flat still allows it; Tailscale and NetBird are
also refused while that network carries console domains, and Tailscale while
it carries the private routes.

### Legacy host file and flags

A host started without `--config` (legacy mode) converts its older inputs into
`config.json` once, then moves `network-provider.json` into `backups/`. That
file (mode `0600`, version 1) holds a `permitted` list and `private_backend`
(`local` or `tailscale`, not itself a grant); existing `tsnet/<node>/`
directories and `portal/*.json` files were recorded in its `migration` block
and are not grants, and `grants_from_state` must stay empty. Auth keys were
never stored in it.

The legacy flags map to grants so that a process never opts in by omission:

- `--network` defaults to `local`. `--network tailscale` grants `tailscale` and
  sets `private_backend`; it does not grant Funnel. An explicit
  `--network local` sets `private_backend` to `local` and revokes nothing.
- `--portal` defaults to `false`. `--portal` grants `portal`. An explicit
  `--portal=false` leaves `portal` out of `network.permitted`; flats' own
  Portal permissions stay in the database. Omitting `--portal` never creates
  a grant: pass `--portal` or `--permit portal` once.
- `--permit` takes a comma-separated list of canonical IDs. `"funnel"` is not
  an alias of `tailscale-funnel`.
- `--relays` sets `portal.relays` and does not grant Portal.

After the conversion, each legacy flag given must match `config.json`; a
different value stops the start with status 78. A provider fixed by a legacy
service's `--network tailscale`, `--portal` or `--permit` flag is pinned: the
console refuses to turn it on or off. `scripts/tailnet-gate.sh` passes
`--network tailscale --portal=false` on a fresh data directory, so it grants
Tailscale and not Portal.

Pre-lifecycle flats' grants: see
[Upgrading provider grants from an older host](networking.md#upgrading-provider-grants-from-an-older-host).

## Manager

`provider.New(dir, provider.Options)` keeps the supplied backends. It does not
serve a flat.

- `Options.Local` is required.
- `Options.Tailscale` is the `Tailnet` interface. `provider.TSNet` adapts
  `*tsnet.Net` and adds `FunnelState`.
- `Options.Portal` is `PortalNet` (`*portal.Net` satisfies it),
  `Options.Zrok` is `ZrokNet`, `Options.CFQuick` is `QuickNet` and
  `Options.Netbird` is `NetbirdNet`. The app assigns an interface only when its
  concrete backend is non-nil, so a skipped backend is unconfigured rather than
  a typed nil.
- `TailscaleStateDir`, `ZrokStateDir`, `CFQuickStateDir` and `NetbirdStateDir`
  name the local state the Manager cleans up for a deleted flat when that
  backend is not configured (defaults under the data directory).
- `Configuration` is the canonical, non-secret desired backend configuration
  hashed into approval policy; `Permission` reads per-flat opt-ins for status;
  `Grants` are the host grants above.

A non-nil backend is not permission. Tests inject fakes through these
interfaces; there is no permit-skipping constructor.

Manager implements the core contracts and aliases core DTOs; core does not
import expose. `ExposurePolicy` hashes host grants, the private backend, which
backends are configured and `Configuration`, excluding transient readiness, so
a pending approval does not carry over to changed provider configuration. A
backend skipped by an explicit `--portal=false` is absent from it. Canonical
nonsecret relay origins and settings bind provider configuration changes
without binding secrets.

Routes and observed endpoints are keyed by exact host, audience and provider.
Per-flat permissions are read freshly for status. Confirmed stops prune the
matching endpoint state, and Manager shutdown clears all observations, so
closed previews and routes do not accumulate in status. Preview cleanup
(`StopExposure`) stops only Draft private routes for the requested host;
another preview and the current site remain reachable. Public opening
preserves an independent Local route, and core rebinding refreshes both current
handlers before each public activation.

In-use revocation is refused before any policy write, including starting/error
routes, previews and redirects. Public may commit with a registered
permitted/configured current route in `starting`; links remain unavailable
until ready. A Funnel node waiting for login or device approval remains a
registered, permitted route in `needs-login`; it is not reported as an absent
provider permission. Teardown uses confirmed Manager stops across
delete/rename/expiry, retaining unconfirmed routes for retry.

`StopSlug` is the authoritative delete/redirect-expiry operation. It includes
the exact slug as a deterministic tsnet identity candidate even when no Funnel
or Tailscale route remains in memory, then stops every Funnel listener and
confirms every candidate node retirement before it stops Portal or Local. A
public listener or node failure therefore leaves Local reachable and keeps the
appropriate route or cleanup record for retry; sibling slug routes are
untouched. When no tailnet backend exists because neither Tailscale provider
is granted, `StopSlug` removes only the validated host's local `tsnet/<host>`
directory. It does not start a backend or contact control, and it prevents a
later flat with the same slug from loading the historical identity. The old
offline machine may remain visible in the tailnet administration plane until it
expires or an operator removes it.

New flats' default provider permissions come from host configuration; see
[Operator authority](lifecycle-core-contract.md#operator-authority-and-frozen-approvals).

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

`TestServeFunnelRoutesIngressAndLeavesPrivate` (`internal/expose/tsnet`)
checks these listener rules against the testcontrol harness, not a live
tailnet: listen-on-both collides with private HTTPS, a failed FunnelOnly
attempt is rolled back, ingress reaches the public handler while tailnet
clients reach the private one, and `StopFunnel` leaves private HTTPS serving.

## Cloudflare quick tunnels

`cfquick.Net` (`internal/expose/cfquick`) serves a slug through an anonymous
Cloudflare quick tunnel minted at `api.trycloudflare.com`, using libtunnel
(`github.com/oesni/libtunnel`, an in-process cloudflared; no binary). `New`
installs libtunnel's isolated environment, so `LIBTUNNEL_*` variables are
ignored and no tunnel secret is exported to processes Flats starts, and the
mint client sends no network-error reports. Each slug gets its own tunnel
and a loopback origin server that strips `Tailscale-User-*` identity
headers (including underscore spellings) before the flat's handler.

`Serve` resolves the tunnel's hostname before it returns, so an approved
Public transition always commits with an address; the edge connection then
comes up in the background (`starting`, then `ready` when libtunnel has
verified the public URL from this host). The tunnel's credentials are
recorded in `<data>/cloudflare-quick/<slug>.json` (mode 0600, written
atomically) and replayed by the next `Serve`, so the address survives
restarts and Private/Public changes while Cloudflare keeps the tunnel. A
record that cannot be replayed is replaced by a fresh mint. When the edge
reports the tunnel gone, or the credentials are rejected, the record is
deleted and a new tunnel is minted on a new hostname; a tunnel that ends for
another reason is retried from the record with backoff (2 s to 60 s). The
route's URL is read from the backend each time, so a new hostname shows up
in the flat's `public_url` without a new approval.

`Stop` closes the tunnel, waits for it to let go of the edge, then drains
the origin; the record is kept. An unconfirmed stop keeps the entry so a
retry finishes it, and `StopPublicRoutes` reports `cloudflare-quick` in
`Unconfirmed`. `Retire` (used by `StopSlug`) stops the tunnel and deletes
the record. With no quick tunnel backend, `StopSlug` deletes the record
directly, so a deleted flat leaves no credentials behind. Quick tunnels
reserve nothing at Cloudflare, so there is no remote cleanup obligation. The
approval policy token counts the quick tunnel backend only while
`cloudflare-quick` is granted, so hosts without it keep their token.

## zrok

`zrok.Net` (`internal/expose/zrok`) serves a slug as a zrok public share of
the operator's enabled zrok environment: by default the one Flats manages in
`<data>/zrok-environment`, which `zrok.Connect` creates from the account token
the operator enters in the console (`PUT /console/api/providers/zrok/account`)
and `zrok.Disconnect` unregisters and removes (`DELETE`, only while zrok is
off; the backend is detached first). When zrok refuses the stored token,
`Disconnect` keeps the environment and returns `ErrStale`, because it may
still be registered; `DELETE ...?force=1` removes it locally anyway. The same `PUT` with an account already
connected calls `zrok.Replace`: the new environment is enabled in
`<dir>.next`, then the old one is unregistered, with its own token or else
the new one (a regenerated token of the same account), and the new one moved
into place. If neither token can unregister it, the replacement fails with
`ErrStale`, the staged environment is unregistered and the current one
stays. The running backend is detached first, which `Manager.DetachZrok`
refuses while a zrok route is registered (it waits for zrok opens in flight,
which hold a read lock from `Serve` through route registration), and restarted on the new account,
or on the old one if the replacement fails. `Net.Adopt` then moves name
records to the new account fingerprint when the new account holds the same
reservation (same name and creation time). Constructing a `Net` reads that
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
zrok exposes no account identity apart from the token, so a regenerated token
is treated like another account. `Serve` uses an
existing name only with this host's record: a name reserved in the account by
the zrok CLI or another Flats host is refused. A creation attempt is recorded
as pending. An `errNameExists` answer or another refusal drops the record.
If the request got no answer and the name exists later, zrok cannot tell
whether this attempt or another client created it: `Serve` refuses it and
asks the operator to release it, and `Retire` leaves the name and drops only
the record. A created name is recorded with the controller's creation time;
if it cannot be recorded, the name is released again. A name deleted and
reserved again under the same spelling has another creation time, so it is
neither used nor released. A share the controller returns without a
frontend endpoint is deleted and the open fails. A refused creation drops
any record, and an existing name is used only with a record of its creation
by this host. zrok reports creation times in whole seconds and offers no
other identity for a name, so a name deleted and reserved again by another
client of the same account within the same second cannot be told apart;
that window is a known limit. A share whose creation
failed, or whose rollback failed, is kept as unsettled: `Stop` and process
shutdown remove it when this host owns it. `StopPublicRoutes` calls zrok's
`Stop` even without a registered zrok route, so the rollback of a failed
Public approval settles such a share and reports zrok unconfirmed if it
cannot. The approval policy token includes the zrok environment directory,
account fingerprint and namespace when zrok is permitted, so a pending
approval does not carry over to another zrok account. Turning zrok on or off
from the console sets the configuration the next start computes, and the
token counts the zrok backend only while zrok is granted. An
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

## Private Tailscale revocation

An operator-validated revocation confirms Manager teardown of that flat’s Tailscale current, preview and redirect registrations before writing the denied permission. Local remains registered. The real tsnet backend uses `StopPrivate`: with a sibling Funnel request it closes only Private HTTP and retains the node and approved Funnel listener; without Funnel it retires the node normally. Private listener setup and teardown are serialized so asynchronous startup cannot reopen a revoked route or race a second teardown. An in-progress or terminally failed retirement remains fail-closed. A terminal logout failure closes the consumed backend and preserves its persistent state; the next `StopPrivate` call starts a fresh backend over that state and performs a real logout retry in the same process. Absence from the active-node map does not prove teardown completed. Legacy backends without Private-only stop support refuse if a Funnel registration shares the host. Public provider revocation continues to require an approved Public-to-Private transition.

## NetBird

`netbird.Net` (`netbird/`, a separate Go module linked only by
`netbird/cmd/flats`, which `build-release.sh` builds for the release
archives; `cmd/flats`, which `go install` builds, has a nil
`provider.NetbirdDriver` and reports `ErrNetbirdNotBuilt`) runs one embedded
NetBird peer per name, as tsnet runs one node per name
([spec](spec/netbird-peers.md)). A name is a flat's slug or a console domain.
Each peer asks NetBird for its name as an extra DNS label and keeps its state
under `<data>/netbird/`:

```
peers/<name>/netbird.json    embed config: the peer's WireGuard identity (0600)
peers/<name>/dns.json        embed state file
legacy-peer.json             notice about an earlier version's shared peer
```

A peer plays up to three roles: private (`netbird`, a mesh provider like
`tailscale`) and console listen on port 80 of the peer's overlay address and
report `http://<name>.<NetBird domain>/` (the extra label; the overlay IP when
the peer has no FQDN); public (`netbird-expose`) listens on overlay port 8080
of the flat's peer, asks NetBird to expose it (`Expose` with the slug as name
prefix, HTTP, no PIN or password) and keeps the session alive. A keep-alive
error or lost listener exposes again with backoff, so the URL can change and
`PublicURL` reports the current one. `ServePublic` returns once the first
expose succeeded; a first failure removes the entry and fails the open. Console
host guards accept both the extra-label FQDN and the primary FQDN.

**Start.** The first role of a name starts its peer under the name's lock.
Every peer registers with the setup key stored in `flats.db` (host key
`netbird`), read through `Config.SetupKey` at each peer start, so a key
registered in the console applies to the next peer without a restart. With a
key, embed logs in with the stored identity and registers it again with the key
only when management no longer knows it. Without one, a peer that has an
identity still logs in, and a rejection is reported as `ErrPeerRejected`; a
peer with neither fails with `ErrNoSetupKey`. A key without "Allow extra DNS
labels" is reported as `ErrKeyNoExtraLabels`, and a later start with the same
key fails at once. The key is scrubbed from every returned error.

**Process-wide start lock.** `embed.New` sets `NB_DNS_STATE_FILE` in the
process environment and the engine reads it while it connects, so two peers
starting at once could swap state files. Peer starts therefore take one
package-wide semaphore before `embed.New` and release it only when NetBird's
own start returns. That start may outlive Flats' start timeout (60 seconds):
the caller then gets an error saying NetBird keeps trying, the abandoned start
keeps the semaphore and the name busy until it ends, and a start that connected
late is stopped so the next route retries. A waiter gives up when its own
context ends. A management outage therefore delays peer starts but never lets
two engines read the variable at once.

**Backend.** The app creates the NetBird backend whenever `netbird` or
`netbird-expose` is permitted and the binary links NetBird, with or without a
key: routes then report the missing key until one is stored.

**Legacy shared peer.** At start, before the backend is built,
`CleanupLegacy` looks for the files of the single shared peer earlier versions
ran (`account.json`, `netbird.json`, `netbird-dns.json`, `ports.json`),
deletes them, and the app writes `legacy-peer.json` (device name, management
URL) so the console can ask the operator to delete that peer in the NetBird
dashboard. A `netbird` host key row without a value (the old connection
record) is deleted; one with a value (a waiting enrollment) becomes the stored
key. The notice stays until the operator dismisses it
(`DELETE /console/api/keys/netbird/legacy-peer`).

**Approval policy.** While `netbird` or `netbird-expose` is granted, the
policy configuration includes `management_url + "\x00" + fingerprint`, where
the fingerprint is the first 16 bytes of `sha256("flats-netbird-key:" + key)`
in hex (empty without a key). It is recomputed when the key changes, so an
approval does not carry over to another key.

**Key change.** Registering a key in the console retries in the background:
core's `RetryProviderRoutes(ctx, netbird, netbird-expose)` reapplies the
exposure of every flat that allows `netbird`, or, for a Public flat that does
not, its `netbird-expose` route, with `publicGrant` false. A public route is
retried only where the flat's approved exposure already used it, so the retry
never publishes anything new; failures are recorded as flat events. Every
listed NetBird console domain not yet served is served again.

**Stop and retire.** `StopPublicRoutes` calls `StopPublic` even without a
registered route. `StopProviderRoutes(…, netbird)` and `StopExposure` call
`StopPrivate`; a peer stops once it serves nothing, keeping its identity.
`StopSlug` calls `RetirePublic` then `RetirePrivate`, which delete
`peers/<name>/` once no role uses it; without a backend it calls the driver's
`RetireStored(dir, name)`. A failed retirement keeps the routes registered for
retry. Retirement never contacts NetBird: the embedded client cannot
deregister a peer, and an ephemeral one is removed by NetBird after ten minutes
offline. Renames keep the old name's routes, NetBird included, for the
redirect and retire them through `StopSlug` when it expires. Public and private
routes strip `Tailscale-User-*` identity headers.

## Console domains

The console runs on one tsnet node per Tailscale console domain and one
NetBird peer (console role) per NetBird console domain
([Console domains](configuration.md#console-domains); routes in
[Console addresses](surfaces.md#console-addresses)).

**Ownership record.** `flats.db` table `console_nodes` has one row per
`(provider, name)` with `retiring` and `last_error`. A row is written before a
console node is first served, marked `retiring` when the name leaves its list,
and deleted once the node is retired (`tsnet` `Stop` or NetBird
`RetireConsole`). Because the record outlives the list, a name removed offline
is still retired at the next start. A failed retirement keeps the row, marked
retiring with its error, so the name stays reserved.

**Reservation.** Core's reserved names are the fixed reserved names plus a
function the app supplies: both console lists and every `console_nodes` row.
`ReserveConsoleName(ctx, name, commit)` runs under the per-slug operation lock
that flat creation and rename take: it refuses a name held by a flat, an open
preview, a redirect or another console name, marks it as being added, runs
`commit`, and drops the mark afterwards (the saved list or the row then holds
it). `ReserveSharedConsoleName` is the same for a name that is a console domain
only of the other network, which may share it. Preview hosts are reserved the
same way against console additions until the preview is registered; preview
host generation retries a random name that is reserved. The fixed name `flats`
stays reserved for flats but may be a console domain.

**Start.** After core restores flat routes, under the console-domain mutex:
every row whose name is no longer listed, or still marked retiring, is marked
retiring and retired in the background (failures are recorded and retried at
the next start; without that network's backend the row waits). A name listed
again while its retirement is unfinished is served, with a new identity, only
after the retirement succeeds. Each other listed name is served unless a flat,
preview or redirect already holds it, in which case the console shows the
conflict.

**Preferred console.** `console.preferred_host` is saved through the same
settings source, under the console-domain mutex, so it never names a domain
a concurrent deletion removes. `config.Document.Set` refuses a NAME its list
does not hold and unsets the preference whenever a list write (`Set` or
`Unset` of `tailscale.console_hosts` or `netbird.console_hosts`) removes the
name it names; every writer (console, offline `flats config`, legacy
upgrade) goes through it. Links are resolved on each use: the request's
served console domain, else the preferred console once its node reports
`ready` or `key-expiring` (the readiness check the self-delete redirect uses;
both mean the node serves with its certificate; a node counts as
served as soon as it is started, while a tailnet node may still be logging
in), else the first served domain in the fixed order, unchanged.

**Adding**, under the console-domain mutex:

1. Refuse a pinned list, a stale settings ETag, an invalid label, a duplicate,
   a name still being retired, or a network that is off.
2. Reserve the name and, inside the reservation, write the `console_nodes`
   row, serve the node (a synchronous error retires it and ends here), then
   save the list through the settings source. If the save fails the node is
   retired; if that fails too, the row stays as retiring, keeping the name
   reserved until a later retirement succeeds.

**Deleting**, under the console-domain mutex:

1. Refuse a pinned list. Refuse emptying both lists unless the request reached
   the management listener on a loopback address (judged from the connection's
   local address; a console domain never counts). When the request came through
   the name being deleted, refuse unless another listed domain is served and
   ready, `ready` or `key-expiring` (the preferred console first, then
   Tailscale, then NetBird) and use
   it as the redirect.
2. Save the list without the name (ETag-guarded); the same write unsets
   `console.preferred_host` when it names the deleted domain. Nothing has
   changed if this fails. A name that was never served (a conflict) has no
   console node: it is only removed from the list, and the owner of the name
   keeps its node.
3. Mark the row retiring and retire the node. On success delete the row; on
   failure keep it with the error and return it; deleting the same name again,
   or the next start, retries.
4. When the request came through the name being deleted, the response is sent
   first and step 3 runs in the background about two seconds later, because
   retiring the node closes the connection that carries the response.
