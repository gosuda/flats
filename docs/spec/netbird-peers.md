# NetBird peers per flat and console domain

Status: implemented. The design is in the architecture docs:
[serving through NetBird](../networking.md#serving-through-netbird),
[network provider lifecycle](../network-provider-lifecycle.md) and
[configuration](../configuration.md).

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

1. **Naming.** A peer is named after the flat's slug or the console domain
   and asks NetBird for that name as an extra DNS label, so it is reachable
   as `<name>.<NetBird domain>`. NetBird gives every peer a primary name of
   its own as well: for an ephemeral peer it carries the overlay IP
   (`blog-12-34`), and for another peer it is the name, or the name with a
   suffix when it is taken. Flats uses the extra label. Flat slugs and
   console domains never collide in Flats (rule 14 of console-settings.md),
   so one name has at most one peer on this host.
2. **Address.** A peer serves plain HTTP on port 80 of its overlay address.
   The URL is `http://<name>.<NetBird domain>/`, or the overlay IP when the
   peer has no FQDN. NetBird issues no certificates for peer names, so
   NetBird routes are never HTTPS (see [HTTP](#http)).
3. **Duplicate names.** NetBird does not keep extra labels unique: two peers
   asking for the same label both answer it. That happens only when a peer
   NetBird still lists asks for a name a new peer of this host also uses,
   for example a flat deleted and created again with the same slug within
   ten minutes while its old ephemeral peer is not yet removed, or a peer
   of another host using the same key. The console warns that such a name
   may also reach another peer until NetBird removes the old one.
4. **Which flats.** A peer starts when a flat that allows `netbird` or
   `netbird-expose` is served, and only then. Previews and drafts do not use
   NetBird. NetBird Expose publishes the flat from the flat's own peer.
5. **Identity and restart.** Each peer keeps its own WireGuard identity in the
   data directory. A restart logs in with it, so the peer keeps its name and
   address.
6. **Re-registration.** When NetBird no longer knows a peer (an ephemeral peer
   NetBird removed after it was offline, or one deleted in the dashboard),
   the next start registers it again with the stored setup key and the same
   identity, asking for the same extra label, so its URL stays the same.
   Its overlay IP and primary name may change.
7. **Retirement.** Deleting a flat stops its peer and deletes its local
   identity. Renaming one keeps the old name's peer while the old name
   serves its redirect, and retires it when the redirect expires, as other
   providers' old routes are. Flats cannot delete a peer in NetBird: an
   ephemeral peer disappears from the dashboard about 10 minutes after it
   goes offline; a non-ephemeral one stays until the operator deletes it
   there.
8. **Stopping.** A route that stops without retiring (NetBird removed from
   the flat, the provider turned off, the flat going Private) stops its peer
   once the peer serves nothing else, and keeps its identity until the flat
   is deleted, so allowing NetBird again brings the same peer back.
9. **Cost.** Every peer is a full embedded NetBird client with its own
   management, signal and relay connections. The console shows how many
   peers run.

## Setup key

10. **Stored like the Tailscale key.** The NetBird setup key follows rules 1 and
   3–6 of console-settings.md: entered by flag, environment, offline CLI or
   console; stored in `flats.db`, encrypted; an external value seeds and does
   not override; a console registration wins at once. Rule 7 (enrollment keys)
   no longer applies to NetBird.
11. **Required and recommended key options.** The setup key must **allow
    extra DNS labels** (rule 1); NetBird refuses the registration otherwise,
    and Flats says which option is missing. The console recommends that it
    also be **reusable** (every flat and console domain registers with it),
    **ephemeral** (NetBird removes peers Flats no longer runs, so deleted
    flats do not pile up in the dashboard) and assign an **auto group**
    (access policies then apply by group, as peers come and go). It should
    not expire or be limited in uses, because re-registration (rule 6) needs
    it later. Flats cannot tell these properties from the key; the operator
    is responsible for them.
12. **Check on entry.** A NetBird setup key is a UUID; anything else is
    refused. It is not otherwise verified until a peer uses it.
13. **Changing or removing the key.** A new key is used for peers that register
    from then on; peers already registered keep working. Registering a key
    retries, at once, the NetBird routes and console domains that failed for
    want of one. Removing the key keeps running peers, but a peer that must
    register (new flat, new console domain, re-registration) fails with an
    error that asks for a key.
14. **Approvals.** The approval policy token names the management server and
    a fingerprint of the setup key (a one-way hash, not the key), so an
    approval granted under one key does not carry over to another.

## Console domains

15. **A list.** `netbird.console_hosts` is an ordered list of DNS labels, like
    `tailscale.console_hosts`. Each name is a peer serving the console. Rules
    12–16 of console-settings.md apply with "NetBird" in place of
    "Tailscale": the NetBird provider must be on to add a name and cannot be
    turned off while the list is not empty; names are reserved against
    flats, previews and redirects; deleting retires the peer.
16. **Not locking the operator out.** A console reached through a console
    domain of either provider refuses a delete that would leave no console
    domain at all; only the loopback console or offline `flats config` can
    empty both lists. Deleting the domain the console is open on needs
    another ready domain, of either provider, to move the browser to.
17. **The same name on both providers** is allowed: they are different
    networks.
18. **Who decided.** A console decision made through a NetBird domain records
    the requesting device as `netbird:<peer FQDN>`, as a Tailscale domain
    records the tailnet login.

## HTTP

19. **Traffic.** NetBird routes are HTTP inside the NetBird overlay: traffic
    between peers is encrypted by WireGuard, but the browser treats the page
    as not secure. Browser features that need a secure context (clipboard
    writes, `crypto.subtle`, service workers, `Secure` cookies) do not work on
    NetBird routes. The console says so next to the NetBird providers.
20. **Console protection over HTTP.** Browsers send `Sec-Fetch-Site` only to
    secure origins. On a NetBird console domain, a console change is accepted
    without it when it carries the console's custom header and an `http`
    `Origin` equal to the address it was sent to. Every other console guard is
    unchanged. This is CSRF protection, not authentication, as before.

## Upgrading

21. **The shared peer is dropped.** A host upgraded from the shared-peer design
    deletes that peer's local identity and port records at its first start,
    and the console asks the operator to delete the peer (its device name is
    shown) in the NetBird dashboard. A setup key waiting for enrollment
    becomes the stored key. Flats that allowed NetBird keep their grants and
    get their own peers once a key is stored. Their URLs change from
    `http://<device FQDN>:<port>` to `http://<slug FQDN>/`.
22. `netbird.device_name` is removed from `config.json`.

## Open questions

- Per-provider console domain identity beyond the requesting peer's FQDN
  (NetBird user identity) is not available from the embedded client.
