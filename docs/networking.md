# Private and public access

This page is for operators: which networks a Flats host serves on, how to turn
on Tailscale, Tailscale Funnel, Portal, zrok, Cloudflare quick tunnels and
NetBird, and how visibility works. The
provider state machine is in [Network provider lifecycle](network-provider-lifecycle.md).

## Providers

| Provider | Group | Reaches |
| --- | --- | --- |
| Local | Private | `*.localhost` origins on the Flats machine. Always on. |
| Tailscale | Private | Devices your tailnet ACLs allow |
| NetBird | Private | Peers your NetBird access policies allow |
| Tailscale Funnel | Public | Anyone on the internet |
| Portal | Public | Anyone on the internet, through a Portal relay |
| zrok | Public | Anyone on the internet, through a zrok public share on your zrok account |
| Cloudflare quick tunnel | Public | Anyone on the internet, through an anonymous Cloudflare quick tunnel on a random `trycloudflare.com` address |
| NetBird Expose | Public | Anyone on the internet, through NetBird's reverse proxy |

The default network is Local; every other provider is off. A provider serves a
flat only when both of these are on:

1. **Host permission.** Turn the provider on in **Settings → Network
   providers**. This saves `network.permitted` in `config.json` and starts the
   provider's backend without a restart. Turning a provider off is refused
   while a flat still allows it.
2. **Flat permission.** Allow the provider under **Networks** on the flat's
   Settings tab.

Permitting Tailscale does not permit Funnel, and permitting NetBird does not
permit NetBird Expose. Neither permission publishes a
version or changes visibility.

On a running Public flat, allowing another public provider starts its route
immediately, while existing addresses keep serving. Its endpoint shows
connecting, ready or the provider's error without a redeploy or restart.
If startup fails, the permission stays allowed; allowing it again retries.
On a Private flat or one without live content running, allowing a public provider
only records the permission. It never makes the flat Public.

Each mode has a **default for new flats** in Settings → Network providers. A
new flat starts allowed on Local plus the private and public defaults only,
even when more providers are on for the host, and it is always created
Private. Allow more providers on a flat to serve it on several networks. The console
shows a private open button (lock icon) and, while the flat is public, a
public one (globe icon); a button lists every address when its kind has
several.

## Running the console on Tailscale

The console runs on your tailnet under one or more console domains. Turn
Tailscale on (Settings → Network providers) and add a name under Settings →
Console domains; it is served at `https://NAME.<tailnet>.ts.net`. On a new
remote server, whose console is not reachable yet, set it up while the host
is stopped:

```sh
flats config set network.permitted tailscale
flats config set tailscale.console_hosts flats
flats key set tailscale < tailscale-authkey    # optional: or follow the login links
```

Enable MagicDNS and HTTPS certificates in your tailnet. Without an auth key,
each new node shows a login link in the console and the log. Other ways to
give the key are under [Operator keys](configuration.md#operator-keys);
adding, renaming and deleting console names is under
[Console domains](configuration.md#console-domains).

To also put new flats' private routes on the tailnet, set the private backend
while the host is stopped:

```sh
flats config set network.private_backend tailscale
```

With the private backend on Tailscale and no other private default chosen
(`network.default_private` empty), a new flat is allowed on Tailscale from the
start, so its private and preview links use the tailnet:

- Choosing that backend is your standing permission for the Private Tailscale
  routes of every flat created afterwards, including flats an agent creates.
- Each grant is logged as a `provider` event. Turn it off under the flat's
  Networks to keep a flat on the host only.
- It never allows Funnel, Portal, zrok, quick tunnels or NetBird Expose. Flats created earlier keep their
  providers.

Flats embeds tsnet. Permitted flat and preview routes, and each console
domain, use separate nodes, so each flat and preview gets its own
origin. Tailnet ACLs decide which devices can reach those origins. Code and
data stay on your machine.

Local loopback also binds in Tailscale mode: concurrent hosts need distinct
`host.management_addr`, `host.local_addr` and data directories.

## Removing a permission

Removing a flat's Tailscale permission stops its Private Tailscale current,
preview and redirect routes before saving the revocation. Local stays
available, and a separately permitted Public Tailscale Funnel route stays up.
An unconfirmed stop keeps the permission allowed so the operator can retry.

A currently Public flat must complete an approved change to Private before its
Public provider can be revoked.

## Local links from another device

A Local link (`*.localhost`) opens on the machine where you click it. When you
open the console from another device, such as over the tailnet, it shows Local
links as "server only" text instead of links, because they would open that
device rather than the Flats host.

## Publishing through zrok

zrok serves public flats as zrok public shares on your zrok account.

**Connecting the account.** Under **Settings → Network providers → zrok**,
paste your zrok account token and choose **Connect**. The token is on
myzrok.io: open the zrok API console and copy it from the getting-started
wizard. Flats registers this machine as an environment of the account, as
`zrok2 enable` would, and keeps the token and identity in
`zrok-environment/` in its data directory (mode 0600); no zrok CLI is needed.
The token is never shown again, logged or written to `config.json`: the panel
shows only the environment ID and zrok service. **Turn on** then makes zrok
available; allow it on a flat to use it. With `zrok` permitted and no account
connected, the host still starts and zrok stays unconfigured. A token can
also be given at start or with `flats key set zrok`; see
[Operator keys](configuration.md#operator-keys).

- **Replacing** (under **Replace with a new account token**, for example
  after regenerating it) stages the new environment beside the current one,
  so a refused token keeps the current account. It is refused while a flat is
  public through zrok. A new token of the same account unregisters the old
  environment, and name records the new account holds move to it.
- **Disconnect account** unregisters the environment and deletes the token;
  turn zrok off first. If zrok refuses the stored token, the console says so
  and offers to forget the account on this machine; remove the environment it
  names in the zrok console.
- To use an environment managed with the zrok CLI instead, set
  `zrok.environment` to its directory; the console then cannot connect or
  disconnect it. The zrok client also honors its own `ZROK2_*` environment
  variables, such as `ZROK2_API_ENDPOINT` for a self-hosted zrok instance.

**Names.** Each flat served through zrok reserves its slug as a name in
`zrok.namespace` (default `public`), so its URL survives restarts and
Private↔Public changes; the share itself exists only while the flat is public
and the host runs. `zrok/` in the data directory records every name the host
reserved:

- Deleting a flat, or the expiry of a rename redirect, releases its name,
  also when the flat went Private or lost its zrok permission earlier.
  Deleting such a flat while zrok is turned off is refused until zrok is on
  again.
- Flats uses, removes and releases only names and shares it created. A name
  already reserved in your account by the zrok CLI or another Flats host is
  refused (release it with `zrok2 delete name` or rename the flat), and a
  share you run under a name Flats reserved is left alone. A name another
  zrok account owns cannot be used; rename the flat. When the account's name
  limit is reached, the route reports the zrok controller's reason.
- Switching `zrok.environment` to another account keeps the old account's
  records: flats using them report the conflict until you switch back or
  remove the record in `zrok/`. zrok identifies an account only by its
  token, so after switching to a regenerated token of the same account,
  release such names with `zrok2 delete name` and remove their records.

On the free zrok plan, a visitor's browser first sees a zrok warning page
with a **Visit Share** button, once per visitor; programs can skip it with a
`skip_zrok_interstitial` request header.

## Publishing through a Cloudflare quick tunnel

A Cloudflare quick tunnel needs no Cloudflare account, no domain and no token.
Under **Settings → Network providers → Cloudflare Quick Tunnel**, choose
**Turn on**, then allow it on a flat. While the flat is public, Flats opens an
anonymous tunnel for it through Cloudflare's own quick-tunnel service
(`api.trycloudflare.com`) and serves the flat at a random
`https://<words>.trycloudflare.com` address. The tunnel runs inside the Flats
process; no `cloudflared` binary is needed.

- **The address is random and can change.** Flats keeps each flat's tunnel
  credentials in `cloudflare-quick/<slug>.json` (mode 0600), so a restart or
  a later Public transition reuses the same tunnel and address while
  Cloudflare still has it. Cloudflare removes idle quick tunnels; when the
  tunnel is gone, Flats opens a new one and the flat gets a new address. Read
  the current address from the console or `get_flat`, not from a bookmark.
- **It is meant for testing and development.** Cloudflare gives quick tunnels
  no uptime guarantee, allows up to 200 concurrent requests per tunnel
  (further requests get `429`) and does not support server-sent events.
- **Cloudflare terminates TLS**, so it sees the traffic in clear text, as with
  any reverse proxy in front of the flat.
- Going Private closes the tunnel and keeps its credentials. Deleting the flat
  closes the tunnel and deletes them, even with the provider turned off.
- Flats sends no network-error reports to the mint service and ignores
  `LIBTUNNEL_*` environment variables.

## Serving through NetBird

The release archives, and so `install.sh` and the container images, include
NetBird. `go install` does not: NetBird's client needs `replace` directives
that Go rejects for `go install …@version`, so NetBird lives in a separate Go
module (`netbird/`) with its own entry point, and `build-release.sh` builds the
release binary from it. To build the same binary from a checkout:

```sh
git clone https://github.com/gosuda/flats && cd flats/netbird
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o flats ./cmd/flats
```

Including NetBird makes the binary noticeably larger, and NetBird's `replace`
directives apply to that binary too. A build without NetBird (`go install`, or
`go build ./cmd/flats` at the repository root) shows NetBird under Settings → Network providers with a
note that this build does not include it, and it cannot connect a peer or turn
NetBird on.

NetBird gives Flats two providers: **NetBird** (Private) and **NetBird
Expose** (Public). Like Tailscale, every flat runs on its own NetBird peer,
named after its slug, and the console can have NetBird domains of its own
(Settings → Console domains → NetBird). The embedded client runs in userspace,
so Flats needs no root rights or tunnel interface.

**Setup key.** A setup key is a UUID. In the NetBird dashboard, open Setup
Keys and create a key with
**Allow extra DNS labels** on: Flats asks NetBird for each flat's name as an
extra DNS label, and NetBird refuses the registration otherwise. Make it also

- **reusable**: every flat and console domain registers a peer with it;
- **ephemeral**: NetBird removes a peer about ten minutes after it goes
  offline, so peers of deleted flats do not pile up in the dashboard;
- assigned to an **auto group** your access policies use, because peers come
  and go;
- without an expiry or usage limit, because Flats registers a peer again with
  it when NetBird has removed it.

Register the key under **Settings → Keys**; the other ways are under
[Operator keys](configuration.md#operator-keys). Flats keeps it in `flats.db`,
encrypted, like the Tailscale auth key, and never shows it again. Peers talk
to the management server in `netbird.management_url` (default NetBird Cloud).
**Turn on** NetBird and/or NetBird Expose, then allow them on a flat. Without a
key, NetBird routes report that one is missing; registering one opens them and
the unserved NetBird console domains in the background, without a restart.

- **NetBird (Private)** serves a flat as plain HTTP on port 80 of its own peer,
  at `http://SLUG.<NetBird domain>/` (for NetBird Cloud, `netbird.cloud`), to
  the peers your NetBird access policies allow. Previews and drafts do not use
  NetBird. NetBird gives each peer a primary name of its own as well (for an
  ephemeral peer it carries the overlay IP, such as `blog-12-34`); Flats uses
  the extra label. NetBird does not keep extra labels unique: if a peer it
  still lists asks for the same name, for example a flat deleted and created
  again within ten minutes, both answer it until NetBird removes the old one.
- **NetBird Expose (Public)** exposes a public flat through NetBird's reverse
  proxy from the flat's own peer; the proxy must be enabled on your NetBird
  account or server. Flats asks for an expose named after the slug and keeps
  the session alive; the URL NetBird returns is the flat's public URL. If
  NetBird ends the session Flats exposes again, and the URL changes: NetBird
  adds a random suffix to each expose, so a flat that goes Private and Public
  again, or whose session is exposed again, gets a new address (read the current
  one from the console or `get_flat`). Public is not access control: Flats sets
  no PIN or password, and anyone with the URL can open the flat.

**HTTP only.** NetBird issues no certificates for peer names, so NetBird routes
and NetBird console domains are plain HTTP. Traffic between peers is encrypted
by WireGuard, but browsers treat the page as not secure: clipboard writes,
`crypto.subtle`, service workers and `Secure` cookies do not work there. The
console accepts changes over HTTP only on its NetBird domains, and only from
its own page: the request must carry the console's own header and a
same-origin `http` `Origin`, and a `Sec-Fetch-Site` header, when sent, must be
`same-origin`. A page on another origin, including another flat on the same
NetBird domain, cannot pass that check. On the loopback listener and tailnet
console domains a change also needs `Sec-Fetch-Site: same-origin`.

**Lifecycle.** Each peer keeps its identity in `netbird/peers/<name>/` (mode
0600) and logs in with it after a restart. When NetBird has removed it (an
ephemeral peer offline for ten minutes, or one deleted in the dashboard), Flats
registers it again with the stored key, asking for the same name. Removing
NetBird from a flat stops its peer and keeps its identity; deleting the flat
deletes it. A renamed flat's old peer serves the redirect until it expires.
Flats cannot delete a peer in NetBird: an ephemeral one disappears by itself,
a non-ephemeral one stays in the dashboard until you delete it there.
Replacing the key affects peers that register afterwards; removing it keeps
running peers but stops new ones from registering. Peers start one at a time,
so an unreachable management server delays every peer start behind it.

## Visibility

Visibility is Private or Public, independently of publication and connection
state.

- **Private** routes use Local, or permitted Tailscale or NetBird with the
  existing tailnet ACLs or NetBird access policies.
- **Public** routes use explicitly permitted Portal, zrok, Tailscale Funnel,
  Cloudflare quick tunnels or NetBird Expose.
  Public is not access control: anyone with the URL can open the flat.

Configuration alone never publishes a version or changes visibility. Public
requires an approved transition with a registered, configured and permitted
public route. Websites and pages also need a deployed version; documents and
designs run without publication. Both directions, Private → Public and
Public → Private, wait for an approval in the console.

A visibility approval of a website or page freezes the live version it was
requested for: a deployment or rollback while it is pending makes it stale,
and it has to be requested again. A visibility approval of a document or
design covers the flat, not its content: saves, live edits and revision
rollbacks while it is pending do not make it stale, and approving it exposes
the content as it is when the change applies. Both kinds still freeze the
source visibility and provider permissions.

A route may still be Connecting after approval; open its link only after it
reports ready. Legacy listed/unlisted inputs map to Public. Relay availability
and Tailscale connectivity are external dependencies.

Portal relay listing (whether a Public flat appears in relay listings) is a
separate setting, described under Exposure in the
[design document](design.md#exposure).

## Upgrading provider grants from an older host

Historical network configuration does not create provider permissions. An
existing Public policy can remain Public while its route is unavailable; do
not report a link as reachable until its endpoint is ready and has been
checked. [Configuration and storage](configuration.md#moving-an-existing-installation)
describes how the first start of a new release moves the old serve flags into
`config.json`.

Restore intended host grants explicitly:

- `--network tailscale` grants Tailscale.
- `--portal=true` grants Portal.
- `--permit tailscale,tailscale-funnel,portal` records only the listed grants.
- `--portal=false` disables Portal even if its stored grant remains.

A Tailscale grant does not grant Funnel. Configure the relevant backend and
allow each desired provider separately under Networks on each flat's page.

### Pre-lifecycle flats

Database schema 5 (`flats.db` `user_version` 5, not the `config.json`
`schema_version`) records pre-lifecycle flats as eligible for a one-time Private
Tailscale choice:

- An old default-Tailscale host with historical tsnet state must explicitly
  choose `--network tailscale` to preserve eligible flats' Private Tailscale
  opt-in, or `--network local` to consume eligibility as Local-only. An
  ambiguous startup stops before networking.
- An already persisted explicit Tailscale backend or grant also preserves
  eligible flats once.
- Hosts with neither historical tsnet state nor a persisted Tailscale choice
  consume eligibility as Local-only on their first upgraded startup; a later
  host Tailscale grant never opts those flats in.

Existing explicit denials win; new flats and later revocations are never opted
in by restart. Neither choice grants Funnel, Portal, zrok, quick tunnels or NetBird.
