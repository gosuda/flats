# NetBird peers per flat and console domain

Status: target. Technical design:
[netbird-peers-TECH.md](netbird-peers-TECH.md).

## Summary

NetBird works the way Tailscale does in Flats: every name Flats serves on
NetBird is its own NetBird peer with its own DNS name.

- **One peer per flat.** A flat that allows NetBird or NetBird Expose runs on
  a peer named after its slug, reachable at `http://<slug>.<NetBird domain>/`.
- **Console domains.** The console's NetBird names are a list the operator
  adds to and deletes from, like the Tailscale console domains in
  [console-settings.md](console-settings.md). Each name is its own peer.
- **One stored setup key.** Flats keeps the NetBird setup key in `flats.db`
  and uses it for every new peer, as it keeps the Tailscale auth key.

This replaces the single shared peer that served every flat on its own port.

## Peers

1. **Naming.** A peer's requested name is the flat's slug or the console
   domain. NetBird derives the DNS label from it and adds `-1`, `-2`, … when
   another peer in the account already holds that label. Flats shows the
   name NetBird actually assigned and warns when it differs from the
   requested one. Flat slugs and console domains never collide in Flats
   (rule 14 of console-settings.md), so one name has at most one peer.
2. **Address.** A peer serves plain HTTP on port 80 of its overlay address.
   The URL is `http://<assigned FQDN>/`, or the overlay IP when the peer has
   no FQDN. NetBird issues no certificates for peer names, so NetBird routes
   are never HTTPS (see [HTTP](#http)).
3. **Which flats.** A peer starts when a flat that allows `netbird` or
   `netbird-expose` is served, and only then. Previews and drafts do not use
   NetBird. NetBird Expose publishes the flat from the flat's own peer.
4. **Identity and restart.** Each peer keeps its own WireGuard identity in the
   data directory. A restart logs in with it, so the peer keeps its name and
   address.
5. **Re-registration.** When NetBird no longer knows a peer (an ephemeral peer
   NetBird removed after it was offline, or one deleted in the dashboard),
   the next start registers it again with the stored setup key and the same
   identity. Its name stays the same unless another peer took it meanwhile.
6. **Retirement.** Deleting or renaming a flat, or removing NetBird from it,
   stops its peer and deletes its local identity. Flats cannot delete a peer
   in NetBird: an ephemeral peer disappears from the dashboard about 10
   minutes after it goes offline; a non-ephemeral one stays until the
   operator deletes it there. The console says which applies.
7. **Stopping.** A route that stops without retiring (the provider is turned
   off, the flat goes Private) stops its peer once the peer serves nothing
   else, and keeps its identity for the next start.
8. **Cost.** Every peer is a full embedded NetBird client with its own
   management, signal and relay connections. The console shows how many
   peers run.

## Setup key

9. **Stored like the Tailscale key.** The NetBird setup key follows rules 1 and
   3–6 of console-settings.md: entered by flag, environment, offline CLI or
   console; stored in `flats.db`, encrypted; an external value seeds and does
   not override; a console registration wins at once. Rule 7 (enrollment keys)
   no longer applies to NetBird.
10. **Recommended key.** The console recommends a setup key that is
    **reusable** (every flat and console domain registers with it),
    **ephemeral** (NetBird removes peers Flats no longer runs, so deleted
    flats do not pile up in the dashboard) and assigns an **auto group**
    (access policies then apply by group, as peers come and go). It should
    not expire or be limited in uses, because re-registration (rule 5) needs
    it later. Flats cannot tell these properties from the key; the operator
    is responsible for them.
11. **Check on entry.** A NetBird setup key is a UUID; anything else is
    refused. It is not otherwise verified until a peer uses it.
12. **Changing or removing the key.** A new key is used for peers that register
    from then on; peers already registered keep working. Removing the key
    keeps running peers, but a peer that must register (new flat, new console
    domain, re-registration) fails with an error that asks for a key.
13. **Approvals.** The approval policy token names the management server and
    a fingerprint of the setup key (a one-way hash, not the key), so an
    approval granted under one key does not carry over to another.

## Console domains

14. **A list.** `netbird.console_hosts` is an ordered list of DNS labels, like
    `tailscale.console_hosts`. Each name is a peer serving the console. Rules
    12–16 of console-settings.md apply with "NetBird" in place of
    "Tailscale": the NetBird provider must be on to add a name and cannot be
    turned off while the list is not empty; names are reserved against
    flats, previews and redirects; deleting retires the peer.
15. **Not locking the operator out.** A console reached through a console
    domain of either provider refuses a delete that would leave no console
    domain at all; only the loopback console or offline `flats config` can
    empty both lists. Deleting the domain the console is open on needs
    another ready domain, of either provider, to move the browser to.
16. **The same name on both providers** is allowed: they are different
    networks.
17. **Who decided.** A console decision made through a NetBird domain records
    the requesting device as `netbird:<peer FQDN>`, as a Tailscale domain
    records the tailnet login.

## HTTP

18. **Traffic.** NetBird routes are HTTP inside the NetBird overlay: traffic
    between peers is encrypted by WireGuard, but the browser treats the page
    as not secure. Browser features that need a secure context (clipboard
    writes, `crypto.subtle`, service workers, `Secure` cookies) do not work on
    NetBird routes. The console says so next to the NetBird providers.
19. **Console protection over HTTP.** Browsers send `Sec-Fetch-Site` only to
    secure origins. On a NetBird console domain, a console change is accepted
    without it when it carries the console's custom header and an `Origin`
    equal to the address it was sent to. Every other console guard is
    unchanged. This is CSRF protection, not authentication, as before.

## Upgrading

20. **The shared peer is dropped.** A host upgraded from the shared-peer design
    deletes that peer's local identity and port records at its first start,
    and the console asks the operator to delete the peer (its device name is
    shown) in the NetBird dashboard. A setup key waiting for enrollment
    becomes the stored key. Flats that allowed NetBird keep their grants and
    get their own peers once a key is stored. Their URLs change from
    `http://<device FQDN>:<port>` to `http://<slug FQDN>/`.
21. `netbird.device_name` is removed from `config.json`.

## Open questions

- Per-provider console domain identity beyond the requesting peer's FQDN
  (NetBird user identity) is not available from the embedded client.
