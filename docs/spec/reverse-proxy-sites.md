# Flats behind a reverse proxy

Status: implemented. Requested in
[gosuda/flats#146](https://github.com/gosuda/flats/issues/146). The design is
in the architecture docs:
[serving flats behind a reverse proxy](../networking.md#serving-flats-behind-a-reverse-proxy),
[network provider lifecycle](../network-provider-lifecycle.md#reverse-proxy)
and [configuration](../configuration.md#reverse-proxy-keys).

## Summary

An operator who runs Flats behind a reverse proxy can already serve the
console through it ([external console origins](console-settings.md), rule 19).
Flats themselves cannot be served that way. The Local provider answers only
`<host>.localhost`, and every address Flats reports for a flat names that
loopback host. An agent that reads `private_url` or `endpoints` gets a link
that opens only on the Flats machine.

This spec adds the reverse proxy as a network provider. The operator declares
the address pattern their proxy serves, such as
`https://{host}.site.example.org`, and turns the provider on. For each flat
that allows the provider, Flats then:

- serves the flat, its previews and its rename redirects on a separate
  listener that the proxy forwards to;
- reports the proxy address in `endpoints`, `private_url` or `public_url`,
  in the console and in every MCP tool that returns those fields;
- keeps the Local route and its `*.localhost` address unchanged.

The proxy comes in two kinds, as Tailscale and NetBird do: `proxy` is a
Private provider for a proxy that controls who gets in, and `proxy-public`
is a Public provider for a proxy open to the internet.

The spec also changes two behaviors that the proxy exposes but that exist
already: which address Flats reports when a flat has several routes, and
where a rename redirect sends a visitor.

## Providers

1. **Two provider IDs.** `proxy` is a Private provider. `proxy-public` is a
   Public provider. They share one listener and one address template, so the
   host turns on at most one of them at a time. Turning on the other one is
   refused until the first is off.
2. **Same rules as the other providers.** A proxy provider serves a flat only
   when the host permits it (`network.permitted`) and the flat allows it, as
   for every non-local provider ([networking](../networking.md#providers)).
   Neither permission publishes a version or changes visibility.
3. **What `proxy` serves.** For a flat that allows `proxy`, the proxy
   listener serves the same routes Local serves for it: the current site with
   the Private handler, every open Draft or version preview, and the redirect
   from an old slug after a rename. A Public flat that allows `proxy` keeps
   this Private route, as it keeps its Tailscale route.
4. **What `proxy-public` serves.** For a Public flat that allows
   `proxy-public`, the proxy listener serves the current site with the Public
   handler: the same content, access level and public-access notice as Portal
   or Funnel. It never serves a Private flat, a Draft, a preview or the
   Private handler. A rename redirect of a Public flat is served on it like on
   the other public providers.
5. **Visitors through `proxy` have Private access.** Whoever the proxy lets
   through can open Drafts through previews and edit documents, designs and
   boards, exactly as on Local or Tailscale. Flats does not check who the
   visitor is. The proxy's own authentication or network restrictions are
   the only access control. Settings and the docs say this where the operator
   turns `proxy` on.
6. **Reachability is the proxy's.** Flats cannot see whether the proxy,
   its DNS or its certificate work. A registered proxy route reports `ready`
   with the detail "Served through your reverse proxy. Flats cannot check
   that the proxy reaches it." The console shows that detail next to the
   proxy address in the flat's Networks list. For a Public flat whose only
   public route is `proxy-public`, the flat's connection state is that
   route's `ready`, and the console shows the same detail beside it.

## Configuration

7. **Keys.** Two new keys, both with defaults, so `schema_version` stays 5
   ([schema rules](../configuration.md#configjson-schema-rules)):
   - `proxy.url_template`, default `""`. The address pattern, for example
     `https://{host}.site.example.org`.
   - `proxy.addr`, default `127.0.0.1:7880`. The listener the proxy forwards
     to.
8. **Who sets them.** The operator, in Settings → Networks or with offline
   `flats config`. MCP, the API outside console routes, and agents cannot
   change either key or turn a proxy provider on.
9. **Template validation.** The template is an `http` or `https` URL whose
   host is `{host}` followed by a dot and a lowercase DNS name, with an
   optional port. A port equal to the scheme's default (`:443` for `https`,
   `:80` for `http`) is refused with a hint to omit it, because browsers
   leave it out of `Origin` and rule 20 would otherwise expect it.
   `{host}` appears exactly once and makes up the whole
   leftmost label. A path, query, fragment, user information, IP literal,
   `localhost` or a `.localhost` name is refused, with the same checks as
   `console.external_origins`. The host must stay a valid DNS name for the
   longest route label (63 characters, a preview of a 54-character slug), so
   the suffix is at most 189 characters.
10. **No overlap with the console.** A template is refused when it can
    produce the host of a `console.external_origins` entry, that is when an
    origin's host is one DNS label followed by the template's suffix. Adding
    such an origin while that template is set is refused the same way,
    whichever of the two is saved first.
11. **Cross-key checks.** Turning on `proxy` or `proxy-public` requires a
    valid template; clearing the template is refused while either is on.
    While a proxy provider is permitted, `proxy.addr` must differ from
    `host.console_addr` and `host.local_addr`. Without one the address is
    never bound, so an existing configuration that already uses the default
    port for another listener keeps starting (rule 30).
    A host default may name a proxy provider only while the host permits it,
    as for the other defaults.
12. **When changes apply.** A template change applies at once, to routing
    and to every reported address. A changed `proxy.addr` applies the next time
    the listener binds: at start, or when a proxy provider is turned on. The
    console shows it read-only and offline `flats config` changes it. A non-loopback `proxy.addr` logs a warning at
    start, because a client that reaches that port directly skips the proxy.
13. **Approvals.** The approval policy covers the proxy configuration while
    a proxy provider is on: the provider kind and the template, which
    decide the addresses an approval shows. Changing either therefore makes
    every pending approval stale, including version
    deployments and visibility changes, as a changed zrok account does. That
    is intended: an approval shows the addresses it was granted for. The
    Settings confirmation for a template or kind change says so.

## Listener lifecycle

14. **On and off.** Turning a proxy provider on first checks the change
    against rules 1 and 11, then binds the listener at `proxy.addr`, then
    saves. A refused check, a taken address or a failed save refuses the
    change, saves nothing and leaves no listener bound. Turning it off is
    refused while any flat allows it, as for every provider. Revoking a
    flat's grant has already stopped its current, preview and redirect
    routes on the proxy (rule 22), so no proxy route remains; Flats saves
    the change and then closes the listener.
15. **Start.** At start a permitted proxy provider binds its listener before
    Flats restores flat routes. If binding fails, the host still starts, and
    every proxy route reports `error` with the bind error. Settings shows the
    error. A restart retries the bind; while flats allow the provider it
    cannot be turned off and on to retry.
16. **Switching kinds.** Because turning a kind off needs no flat to allow
    it (rule 14), switching from `proxy` to `proxy-public` or back means
    removing the old kind from every flat, turning it off, turning the new
    kind on and allowing it per flat. No grant is left waiting on a kind
    that is off. Removing `proxy-public` from a Public flat needs an
    approved change to Private first (rule 22), so leaving the Public kind
    takes one approval per Public flat that allows it.

## Routing

17. **Host matching.** The proxy listener accepts a request only when its
    `Host`, compared without case and with any port and trailing dot removed,
    is `<label>` followed by the template's suffix, and `<label>` is a route
    host the listener serves. The port is ignored because proxies commonly
    drop it; rule 20 restores the template's port. Any other request gets 404 with a fixed body that does not repeat
    the `Host`.
18. **One label.** `<label>` is a single DNS label: a slug for the current
    site and redirects, `<slug>-<8 random>` for a preview. A single-label
    wildcard certificate therefore covers every route.
19. **Nothing else on that listener.** The console, the management API, MCP
    and `/metrics` are never served on the proxy listener, whatever the
    `Host`.
20. **External host and scheme.** Flats does not use forwarded headers to
    route a request or to build a reported address. It does compare the
    browser's `Origin` with the request's scheme and `Host` before accepting
    document, design and board edits, taking the scheme from
    `X-Forwarded-Proto`, and it passes both to server flats. On the proxy
    listener Flats therefore replaces, on every matched request, the `Host`
    with the route's address from the template (label, suffix and the
    template's port, if any) and `X-Forwarded-Proto` with the template's
    scheme, whatever the proxy or the client sent. Edits then work through a
    proxy that terminates TLS or drops the port without extra proxy
    configuration, a client cannot change either value, and a server flat
    sees the address the browser used. Flats passes other headers through
    unchanged.
21. **Identity headers.** The proxy listener strips `Tailscale-User-*`
    headers, as Local and the public providers do.

## Grants and defaults

22. **Per-flat grant.** Each flat allows a proxy provider under Networks on
    its Settings tab, like any other provider. Revoking `proxy` stops the
    flat's current, preview and redirect routes on the proxy before it saves
    the revocation, as revoking Tailscale does. Revoking `proxy-public` from
    a Public flat needs an approved change to Private first, as for every
    public provider.
23. **Defaults for new flats.** `network.default_private` accepts `proxy`
    and `network.default_public` accepts `proxy-public`. A new flat then
    starts allowed on the proxy, with a `provider` event, under the existing
    default-grant rules
    ([operator authority](../lifecycle-core-contract.md#operator-authority-and-frozen-approvals)).
    As for every default, existing flats keep their providers: the operator
    allows the proxy on each existing flat that should use it.

## Reported addresses

24. **Endpoints.** A flat's `endpoints` list a `proxy` or `proxy-public`
    entry for each registered proxy route, next to the Local entry. The Local
    entry and its `*.localhost` address stay.
25. **Which address a flat reports.** Among a flat's Private routes, Flats
    considers the ones the flat allows that have a registered address, as it
    does today; readiness does not matter. `private_url` is the route on the
    host's private default if it is among them, otherwise the first of
    Tailscale, NetBird, `proxy`, Local. Among a Public flat's public routes,
    Flats considers the allowed ones that are ready. `public_url` is the
    route on the host's public default if it is among them, otherwise the
    result of the current order. The private default is
    `network.default_private`, or Tailscale when that is empty and
    `network.private_backend` is `tailscale`. A private default of Local
    means no preference, so a flat with a Tailscale grant on a host whose
    default is Local still reports Tailscale. This applies to every flat,
    with or without a proxy.
26. **Previews.** A preview is served on Local, on Tailscale when the flat
    allows it, and on `proxy` when the flat allows it. NetBird and every
    public provider still never serve previews. The preview address that
    `open_preview` returns and the preview list report follow rule 25 over
    those routes.
27. **Where these addresses appear.** Rules 24 to 26 apply to every response
    that carries a flat or preview address: the API and MCP flat views
    (`get_flat`, `list_flats` and the results of lifecycle tools), the
    preview responses, and the console. No transport rewrites addresses on
    its own.
28. **Console links.** The console renders proxy addresses as ordinary
    links. Local addresses keep the existing "server only" rule. When a proxy
    provider is on, the "server only" explanation names the reverse proxy as
    another way to open a flat from other devices.

## Rename redirects

29. **Redirect on the network the visitor used.** After a rename, a request
    to the old host is redirected to the new flat's address on the same
    provider the request arrived on. When the new flat has no route on that
    provider, Flats falls back to `private_url` for a Private route and to
    `public_url` for a Public route, as today. A visitor on the Flats host
    who follows an old `*.localhost` link therefore stays on Local, and a
    visitor through the proxy stays on the proxy. This applies with or
    without a proxy.

## Unchanged behavior

30. **No proxy, no other change.** While neither proxy provider is on,
    Flats opens no proxy listener, reports no proxy endpoint, and serves
    and reports everything as before, except for rules 25 and 29. Preview
    addresses do not change: without a proxy only Local and Tailscale serve
    previews, and both the current order and rule 25 prefer Tailscale,
    since a NetBird default cannot serve a preview. Rules 25 and 29 change
    only:
    - `private_url` of a flat that has both Tailscale and NetBird routes on
      a host whose private default is NetBird;
    - `public_url` of a Public flat with several ready public routes, one of
      them the host's public default;
    - the redirect target for a visitor who arrives at an old slug on a
      network other than the one the flat's reported address uses.

## Relation to issue #146

| Issue item | Resolution |
| --- | --- |
| Flat URLs reported through the API, MCP and console name `*.localhost` | Rules 24 to 28 |
| A proxy cannot route a public name to a flat without rewriting `Host` | Rules 17 and 18 |
| Key `sites.external_url_template` with `{slug}` | Renamed `proxy.url_template` with `{host}`, because previews and redirects use route hosts that are not slugs (rule 18) |
| Operator-only setting | Rule 8 |
| "The site listener accepts the declared host" | Deviation: a separate listener (rules 14 to 19). Flats then knows a request came through the proxy from the port it reached, not from a `Host` header that any local process can send to the Local listener, and Local stays loopback-only |
| "The template is not access control" | Kept, with a per-flat grant added (rule 22): only flats the operator allows are served through the proxy, as for every other network |
| Acceptance 1: templated URL and no `*.localhost` entry | Deviation in part: `private_url` and `public_url` use the proxy (rule 25); the Local entry stays in `endpoints` so users on the Flats host keep a working link (rule 24) |
| Acceptance 2: declared Host served; refused without the key | Rules 14 and 17; without a proxy provider there is no proxy listener and Local refuses the name as today |
| Acceptance 3: no output change with the key unset | Deviation in part: rule 30 holds except for the address and redirect corrections of rules 25 and 29, which the maintainer chose to apply to every host |
| Per-flat override `flat.site_url` | Out of scope |
| Question: accept the host on the management listener too? | No (rule 19); console addresses stay with `console.external_origins` |
| Question: validate as strictly as `external_origins`? | Yes, plus the label and length rules (rule 9) |
| Question: what happens to the `*.localhost` link? | It stays (rule 24) |
| Question: per-network templates? | The proxy is its own provider; Tailscale and NetBird already report their own addresses |

Operators running a shim in front of the Local listener point the proxy at
`proxy.addr` instead, allow the proxy on the flats it should serve, and can
remove the response rewriting.

## Decisions

- The host default decides the reported address for every flat, not only
  when it names a proxy (rule 25).
- Rename redirects stay on the arrival network for every host (rule 29).
- No action allows the proxy on every existing flat at once; existing flats
  are allowed one by one, consistent with defaults never applying to
  existing flats (rule 23).
- One proxy at a time (rule 1).

## Out of scope

- Authentication of visitors. Built-in authentication is
  [gosuda/flats#139](https://github.com/gosuda/flats/issues/139).
- A per-flat address that is not derived from the route host, such as
  `blog.example.com` for one flat. It would fit later as a per-flat setting
  of the proxy provider.
- Two proxies at once, for example one Private and one Public.
- TLS, DNS and the proxy itself. Flats does not configure or probe them.
- Browser isolation between flats under one parent domain. Flats under
  `*.site.example.org` share a registrable domain unless the operator's
  domain is on the Public Suffix List, so a flat can set cookies that its
  sibling flats receive. The networking docs state this limit.

## Completion criteria

The feature is complete when tests or recorded runs show each of these:

- **Validation.** The template table accepts the forms in rule 9 and refuses
  each refused form, including a suffix too long for a 63-character label.
  A template and a console origin that overlap (rule 10) are refused in both
  save orders. The cross-key checks of rule 11 hold in Settings and in
  offline `flats config`. MCP and non-console API calls cannot change the
  keys or turn a proxy provider on.
- **Address checks.** A configuration that permits no proxy and uses
  `127.0.0.1:7880` for `host.local_addr` or `host.console_addr` validates
  and starts with no proxy listener. Turning a proxy kind on in it is
  refused by rule 11, saves nothing and binds nothing. Changing
  `host.local_addr` or `host.console_addr` to equal `proxy.addr` while a
  kind is permitted is refused. Such a collision is refused when the
  configuration is validated, so it never reaches the bind failure of
  rule 15.
- **Lifecycle.** Turning `proxy` on binds the listener; turning it on with
  the address taken, or with no template, is refused, saves nothing and
  leaves the port free; turning it off is refused while a flat allows it and
  closes the listener once none does; turning on the second kind while one
  is on is refused. A full switch from `proxy-public` to `proxy` on a host
  with a Public flat succeeds only after that flat is made Private, the
  grant is removed, `proxy-public` is turned off and `proxy` is turned on.
  A changed `proxy.addr` is used when the provider is next turned on. A restart with granted flats
  serves them through the proxy, and a restart with the address taken starts
  the host with proxy routes in `error`. A non-loopback `proxy.addr` logs
  the warning.
- **Routing.** Through a real HTTP reverse proxy that forwards the original
  `Host` to the proxy listener:
  - with `proxy`, a granted Private flat, its preview and its rename redirect
    are served; an ungranted flat, an unknown label, a nested label, a
    console name and `/metrics` get 404, and the 404 body does not contain
    the request `Host`;
  - with `proxy` and an `https` template, a document edit through a proxy
    that terminates TLS succeeds both with and without the proxy sending
    `X-Forwarded-Proto`, and a server flat sees `X-Forwarded-Proto: https`
    even when the client sent `http`;
  - with a template on a non-default port, such as `:8443`, behind a proxy
    that drops the port from `Host`, a document edit succeeds and a server
    flat sees the `Host` with that port;
  - with `proxy-public`, only a granted Public flat is served, with its
    public access level; a preview of it, a granted Private flat and its
    Draft are not;
  - `Tailscale-User-*` headers sent through the proxy do not reach the flat;
  - after a template change, the next request is routed by the new template
    and the old name gets 404.
- **Grants.** Revoking `proxy` from a flat stops its proxy routes before the
  revocation is saved. Revoking `proxy-public` from a Public flat is refused
  until it is Private. A flat created under `default_private=proxy` or
  `default_public=proxy-public` holds that grant with its `provider` event,
  and existing flats do not gain it.
- **Approvals.** A pending approval created before a template change is
  refused as stale after it; one created before a `proxy.addr` change is
  not.
- **Addresses.** `get_flat` and `list_flats` over MCP, the API flat views,
  and `open_preview` report the proxy address under rules 24 to 26, and the
  Local entry remains in `endpoints`. Rule 25 picks the expected address in
  each case: private default proxy, default Local with a Tailscale grant,
  Tailscale and NetBird with each default, and several public routes with
  and without a ready public default. A proxy endpoint carries the detail of
  rule 6.
- **Console.** A proxy address renders as a link from another device, a
  Local one as "server only", and the "server only" text names the proxy
  only while a proxy provider is on. The Private kind's Settings section
  shows the rule 5 warning, and a template or kind change asks for
  confirmation naming the pending approvals it makes stale.
- **Redirects.** A renamed flat's old Local link redirects to the new Local
  address and its old proxy link to the new proxy address; a provider
  without a route falls back as rule 29 says.
- **No proxy.** With neither proxy provider on, the flat views, preview
  responses, endpoint lists and redirects of a fixture host match the
  current output except for the cases listed in rule 30.

## Open questions

None.
