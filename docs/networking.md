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

Each mode has a **default for new flats** in Settings → Network providers. A
new flat starts allowed on Local plus the private and public defaults only,
even when more providers are on for the host, and it is always created
Private. Allow more providers on a flat to serve it on several networks. The console
shows a private open button (lock icon) and, while the flat is public, a
public one (globe icon); a button lists every address when its kind has
several.

## Running the console on Tailscale

To run the console and private routes on your tailnet as well, set the private
backend while the host is stopped:

```sh
flats config set network.permitted tailscale
flats config set network.private_backend tailscale
```

Then enable MagicDNS and HTTPS certificates in your tailnet, and either follow
the node login links in the console or put a reusable, untagged Tailscale auth
key in a protected file and set its path as
`credentials.tailscale_authkey_file`. A host started with `--config` refuses
`TS_AUTHKEY` and the other `TS_*` variables, which would bypass `config.json`.

With the private backend on Tailscale, a new flat is allowed on Tailscale from
the start, so its private and preview links use the tailnet:

- Choosing that backend is your standing permission for the Private Tailscale
  routes of every flat created afterwards, including flats an agent creates.
- Each grant is logged as a `provider` event. Turn it off under the flat's
  Networks to keep a flat on the host only.
- It never allows Funnel, Portal, zrok, quick tunnels or NetBird Expose. Flats created earlier keep their
  providers.

Flats embeds tsnet. Permitted flat and preview routes, and the console in
Tailscale mode, use separate nodes, so each flat and preview gets its own
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

Under **Settings → Network providers → zrok**, paste your zrok account token
and choose **Connect**. The token is on myzrok.io: open the zrok API console
and copy it from the getting-started wizard. Flats registers this machine as
an environment of the account and keeps the token in its data directory; no
zrok CLI is needed. The panel then shows the connected environment's ID and
zrok service, and **Turn on** makes zrok available; allow it on a flat to use
it. To switch to another token, for example after regenerating it, enter it
under **Replace with a new account token**: the current account stays
connected if the new token is refused, and replacing is refused while a flat
is public through zrok. **Disconnect account** unregisters the environment
and deletes the token; turn zrok off first. If zrok refuses the stored token
(for example after you regenerated it), the console says so and offers to
forget the account on this machine; remove the environment it names in the
zrok console. Replacing works after a regenerated token: the new token
unregisters the old environment of the same account. Each flat served through zrok reserves its slug as a name in
`zrok.namespace` (default `public`), so its URL survives restarts, and the
share exists only while the flat is public. A name already reserved in your
zrok account by something other than this host is refused. On the free zrok
plan, a visitor's browser first sees a zrok warning page with a **Visit
Share** button, once per visitor; programs can skip it with a
`skip_zrok_interstitial` request header. See
[Configuration and storage](configuration.md) for `zrok.environment` and
`zrok.namespace`.

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

Including NetBird makes the binary 10–16 MB larger (about 80 MB to 95 MB on
linux/amd64) and its archive about 5 MB larger. Its `replace`
directives apply to that binary too; among them,
`github.com/cloudflare/circl` is replaced by NetBird's pinned 2023 fork
(`codeberg.org/cunicu/circl`). A build without NetBird (`go install`, or
`go build ./cmd/flats`) shows NetBird under Settings → Network providers with a
note that this build does not include it, and it cannot connect a peer or turn
NetBird on.

NetBird gives Flats two providers that share one NetBird peer: **NetBird**
(Private) and **NetBird Expose** (Public). Under **Settings → Network
providers → NetBird**, paste a setup key from the NetBird dashboard (Setup
Keys) and choose **Connect**. Flats uses the key once to register this machine
as a peer, keeps only the peer identity in `netbird/` in the data directory
(mode 0600), and does not store or show the key again. The peer talks to the
management server in `netbird.management_url` (default NetBird Cloud) and is
named `netbird.device_name` (default `flats`). The embedded client runs in
userspace, so Flats needs no root rights or tunnel interface. **Turn on** NetBird
and/or NetBird Expose, then allow them on a flat.

- **NetBird (Private)** serves each flat on the peer's overlay address, on its
  own TCP port (21000–21999, kept across restarts), over plain HTTP. Reach it
  from another peer that your NetBird access policies allow, at the URL the
  flat's page shows. Previews and drafts do not use NetBird. **Flats on the
  same peer share one browser cookie jar.** The peer has a single host name and
  the flats differ only by port, and browsers do not scope cookies by port, so
  a cookie one flat sets is also sent to the others on that peer, which can read
  or overwrite it. This matters only for server flats that set cookies, such as
  a session; static flats are unaffected. Use NetBird for flats you trust
  together, and serve a flat that needs its own cookies through NetBird Expose,
  Tailscale (each flat has its own host name) or another provider.
- **NetBird Expose (Public)** exposes a public flat through NetBird's reverse
  proxy, which must be enabled on your NetBird account or server. Flats asks
  for an expose named after the slug and keeps the session alive; the URL NetBird
  returns is the flat's public URL. If NetBird ends the session Flats exposes again, and
  the URL changes: NetBird adds a random suffix to each expose, so a flat that
  goes Private and Public again, or whose session is exposed again, gets a new
  address (read the current one from the console or `get_flat`). Public is not access control: Flats sets no PIN or
  password, and anyone with the URL can open the flat.

**Disconnect peer** deletes the peer identity from this machine; turn both
providers off first. NetBird's embedded client cannot remove a peer, so remove
it in the NetBird dashboard (Peers) and revoke a reusable setup key. If the peer
was removed in the dashboard, connect again with a new key. Deleting a flat
ends its expose session and releases its port; a public flat's port stays
out of use for 24 hours, because NetBird only logs a failed attempt to end an
expose session and its proxy might still forward to that port.

## Visibility

Visibility is Private or Public, independently of publication and connection
state.

- **Private** routes use Local, or permitted Tailscale or NetBird with the
  existing tailnet ACLs or NetBird access policies.
- **Public** routes use explicitly permitted Portal, zrok, Tailscale Funnel,
  Cloudflare quick tunnels or NetBird Expose.
  Public is not access control: anyone with the URL can open the flat.

Configuration alone never publishes a version or changes visibility. Public
requires a published version and an approved transition with a registered,
configured and permitted public route. Both directions, Private → Public and
Public → Private, wait for an approval in the console.

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

Schema 5 records pre-lifecycle flats as eligible for a one-time Private
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
