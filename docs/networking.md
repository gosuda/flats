# Private and public access

This page is for operators: which networks a Flats host serves on, how to turn
on Tailscale, Tailscale Funnel and Portal, and how visibility works. The
provider state machine is in [Network provider lifecycle](network-provider-lifecycle.md).

## Providers

| Provider | Group | Reaches |
| --- | --- | --- |
| Local | Private | `*.localhost` origins on the Flats machine. Always on. |
| Tailscale | Private | Devices your tailnet ACLs allow |
| Tailscale Funnel | Public | Anyone on the internet |
| Portal | Public | Anyone on the internet, through a Portal relay |

The default network is Local; every other provider is off. A provider serves a
flat only when both of these are on:

1. **Host permission.** Turn the provider on in **Settings → Network
   providers**. This saves `network.permitted` in `config.json` and starts the
   provider's backend without a restart. Turning a provider off is refused
   while a flat still allows it.
2. **Flat permission.** Allow the provider under **Networks** on the flat's
   Settings tab.

Permitting Tailscale does not permit Funnel. Neither permission publishes a
version or changes visibility.

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
- It never allows Funnel or Portal. Flats created earlier keep their
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

## Visibility

Visibility is Private or Public, independently of publication and connection
state.

- **Private** routes use Local, or permitted Tailscale with existing tailnet
  ACLs.
- **Public** routes use explicitly permitted Portal or Tailscale Funnel.
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
in by restart. Neither choice grants Funnel or Portal.
