# Flats behind a reverse proxy: technical design

Status: planned. Product rules are in [reverse-proxy-sites.md](reverse-proxy-sites.md);
rule numbers below refer to that page. The design follows the existing
provider Manager ([network provider lifecycle](../network-provider-lifecycle.md))
and changes no stored data, so it needs no `flats.db` migration and no
`schema_version` bump.

## Delivery

One pull request for the feature, with the general corrections in their own
commits ahead of the proxy work so each can be reviewed alone:

1. Request provider tagging, one address-selection function and
   per-provider redirects (rules 25, 26 for the existing providers, 29).
2. Configuration, the listener package, Manager, app and core wiring.
3. Console and documentation.

## Configuration (`internal/config`)

- Add `proxy` and `proxy-public` to the canonical provider list that
  `network.permitted` accepts, after `netbird-expose`.
- `checkDefaultPrivate` also accepts `proxy`; `checkDefaultPublic` also
  accepts `proxy-public`. Widening an allowed set keeps the schema version
  ([schema rules](../configuration.md#configjson-schema-rules)).
- New keys:

  | Key | Kind | Default | Applies |
  | --- | --- | --- | --- |
  | `proxy.url_template` | string | `""` | immediately |
  | `proxy.addr` | string, `checkAddr` | `127.0.0.1:7880` | restart |

- `proxy_template.go` holds `ParseTemplate(s string) (Template, error)`.
  It requires the host to start with `{host}.`, then validates the URL with
  `{host}` replaced by a fixed placeholder label, using the host checks that
  `checkExternalOrigins` uses today (scheme, lowercase DNS name, optional
  port, no path, query, fragment, user information, IP literal or
  `.localhost`). That check moves into a shared helper so both keys refuse
  the same forms. It also requires the suffix to be at most 189 characters
  and refuses a port equal to the scheme's default (rule 9). `Template` has `URL(label string) string`,
  `Label(host string) (string, bool)`, `Authority(label string) string`
  (label, suffix and the template's port, if any) and `Scheme() string`. `Label`
  lowercases, removes a port and a trailing dot, requires the suffix, and
  requires the rest to be one label in the slug character set (lowercase
  letters, digits and single hyphens).
- Cross-field validation in `Config.validate`, next to the existing
  `default_private` checks (`config.go:906`):
  - not both `proxy` and `proxy-public` in `network.permitted`;
  - either one requires a template;
  - a default naming a proxy provider requires it in `network.permitted`;
  - while either kind is permitted, `proxy.addr` differs from
    `host.console_addr` and `host.local_addr` (ephemeral ports excepted, as
    for the existing pair, `config.go:889`);
  - no `console.external_origins` host equals one label followed by the
    template's suffix (rule 10). Because the check runs on the whole
    document, it refuses the second of the two writes in either order.
- Settings writes go through `core.UpdateSettings` with new setting names,
  as `external_origins` does. The settings routes are console-only
  ([HTTP routes](../surfaces.md#http-routes)). The test that pins
  `external_origins` as console-only (`TestExternalOriginsOnlyConsoleSettings`)
  gains the template key.

## Provider IDs (`internal/core`, `internal/store`)

- `core.ProviderProxy = "proxy"`, `core.ProviderProxyPublic = "proxy-public"`,
  and the same strings in `store` (`store/lifecycle.go:14`).
- `PublicProvider` includes `proxy-public`.
- New predicate `PrivateRouteProvider(id)`: Tailscale, NetBird or `proxy`,
  meaning a provider that carries a flat's Private route besides Local.
  `PrivateMeshProvider` keeps its meaning (tailnet-like overlays). Each of
  its call sites is checked and switched where it means "Private route":
  - `service.go:862` (`view`) and `lifecycle.go:1320` (private-route
    lookup): switch.
  - `lifecycle.go:1674` (`privateProviders`): switch, so private and preview
    requests list `proxy` when the flat allows it.
  - `lifecycle.go:469`, `:1981` and `:2043` (grant paths): switch, so a
    `proxy` grant reconciles the Private route at once and a revocation runs
    the confirmed stop of rule 22, as for Tailscale.
- The literal list of public providers at `lifecycle.go:2043` is replaced by
  `PublicProvider`, which includes `proxy-public`.
- `newFlatProviders` (`service.go:1045`) gains a `default_private == proxy`
  case, and `DefaultProviders` (`service.go:1064`) reports it. The existing
  `default_public` branch already grants any permitted ID and needs no change.

## Listener (`internal/expose/proxy`)

A new package, modeled on `internal/expose/local`:

```go
type Net struct { /* listener, server, template func, routes map */ }

func Listen(addr string, template func() config.Template) (*Net, error)
func (n *Net) Serve(ctx context.Context, host string, h http.Handler) (string, error)
func (n *Net) Stop(host string) error
func (n *Net) URL(host string) string
func (n *Net) Status() core.NetStatus
func (n *Net) Close() error
```

- `template` is read on every request and every `URL` call, so a template
  change applies to the next request (rule 12). The app passes a function
  backed by the live settings source.
- `ServeHTTP`:
  1. resolves `template().Label(r.Host)` and looks the label up in the
     routes map; otherwise 404 with a fixed body (rule 17);
  2. strips `Tailscale-User-*` headers, including underscore spellings, as
     `cfquick` does (rule 21);
  3. sets `r.Host` to `template().Authority(label)` and
     `X-Forwarded-Proto` to `template().Scheme()`, replacing any values
     (rule 20). `requestOrigin` (`core/docsroutes.go:223`),
     `board/ws.go:1118` and `design/handler.go:248` then compute the
     external origin from them, and server flats receive both through
     `runtime/worker.go:255` and `runtime/manager.go:195`;
  4. calls the handler.
- One `Net` serves either the Private or the Public kind, never both
  (rule 1), so one route map keyed by host label is enough. The Manager
  decides which handler is registered.
- `Status` reports every route `ready` with the rule 6 detail.
- The package imports no console, API or MCP package; only the Manager
  registers handlers on it (rule 19).

## Manager (`internal/expose/provider`)

- `Options.Proxy ProxyNet` (an interface over `proxy.Net`) and two IDs,
  `Proxy` and `ProxyPublic`, with host grants from `network.permitted`.
- `AttachProxy(ProxyNet)` and `DetachProxy() bool`. `DetachProxy` refuses
  while a proxy route is registered, like `DetachZrok` (`manager.go:406`).
- `servePrivate` (`manager.go:773`) opens a `proxy` route when the request
  lists `Proxy`, including ephemeral previews (unlike NetBird). `servePublic`
  retains it among the Private routes of a Public flat, as it retains
  Tailscale and NetBird.
- `publicIDs` (`manager.go:1069`) gains `ProxyPublic`. `servePublic` opens
  it like the other public providers; the existing ephemeral refusal keeps
  previews off it. `StopPublicRoutes` stops it. A proxy stop has no remote
  side, so it is confirmed once the route leaves the map.
- `StopProviderRoutes(…, Proxy)`, `StopExposure` and `StopSlug` remove the
  matching routes. There is no identity or state on disk to retire.
- `ExposurePolicy` adds `proxy:<kind>:<template>` while a proxy provider is
  granted (rule 13). The listener address is left out: it does not change
  any address an approval shows. Hosts without a proxy keep their policy
  token unchanged.
- `ExposureStatus` reports proxy endpoints with `Configured` true while the
  backend is attached.

## App (`internal/app`)

- **Turning on** (`providers.go:173`, `attachLocked`): for `proxy` or
  `proxy-public`, first validate the candidate settings document (the new
  `network.permitted` with the current template), which refuses the other
  kind and a missing template (rules 1, 11). Then bind
  `proxy.Listen(addr, …)` with the `proxy.addr` saved at that moment and
  attach it. `SetPermittedNetworks` saves afterwards; if it fails (for
  example an ETag conflict), the handler detaches and closes the listener
  it just bound before returning the error (rule 14).
- **Turning off**: the existing refusal while a flat allows the provider
  (`providers.go:194`) runs first. Revocation already stopped every proxy
  route of those flats, so `DetachProxy` cannot find a registered route.
  The handler saves the change, then detaches and closes the listener. If
  `DetachProxy` refuses anyway, the handler reports an internal error and
  keeps the listener until restart rather than closing a live route.
- **Start** (`serve.go`, before `provider.New` at `:482`): when a proxy
  provider is permitted, bind the listener and pass it in `Options.Proxy`.
  Core restores flat routes after the Manager exists, so the listener is
  bound first (rule 15). A bind error is logged and the backend is attached
  in a failed state whose `Serve` returns the bind error, so each proxy route
  reports `error` with it and the host keeps starting.
- A non-loopback `proxy.addr` logs a warning at start, with the same check
  used for the management address.

## Request provider tagging (commit 1)

- `core.WithRouteProvider(ctx, id)` and `core.RouteProvider(ctx) (ProviderID, bool)`.
- The Manager wraps every handler it registers in one helper used by
  `openLocal` (`manager.go:898`), `openTailscale` (`:908`), `openNetbird`,
  the public openers (their `Serve` calls at `:992`, `:1014`, `:1032`) and `openProxy`, so every
  route request carries the provider it arrived on. The legacy core path
  without a Manager leaves requests untagged; untagged requests use the
  fallback of rule 29.

## Address selection (commit 1)

One function replaces `privateRank` (`service.go:2545`), the preview loop in
`previewView` (`service.go:2133`) and `previewLocation`
(`lifecycle.go:1568`):

```go
// preferredRoute picks the reported endpoint among candidates of one kind.
func preferredRoute(eps []ExposureEndpoint, def ProviderID, order []ProviderID) (ExposureEndpoint, bool)
```

- Private candidates: allowed endpoints with a URL, whatever their state
  (as `service.go:862` does now). Order: Tailscale, NetBird, `proxy`, Local.
  Preview candidates: the same over the preview-capable providers.
- `def` is the effective private default (rule 25): `default_private`, or
  Tailscale when it is empty and `private_backend` is `tailscale`. Local or
  empty maps to no preference.
- Public candidates: allowed, ready public endpoints; `def` is
  `default_public`. Without the default among them, the current selection in
  `view` stays.
- The Tailscale placeholder rule of `previewLocation` (a URL containing
  `<tailnet>` is reported as an address still pending) is kept.
- Core reads the defaults through `s.settings.network()`, as
  `newFlatProviders` does.

## Redirects (commit 1)

`redirectHandler` (`rename.go:243`) takes a target function with the
arrival provider:

```go
func (s *Service) redirectHandler(target func(p ProviderID, public bool) string) http.Handler
```

`serveRedirect` builds it from `GetFlat(cur)`: the endpoint of the new slug
with the same provider and `AudienceCurrent`, else `private_url` or
`public_url` as today. While neither exists it still answers 503 with
`Retry-After`.

## Console

- Settings → Networks gains a Reverse proxy section: the template field, the
  listener address (read-only), the kind (Private or Public, mapped to
  turning on `proxy` or `proxy-public`), the rule 5 warning for the
  Private kind, the bind error of rule 15, and a confirmation before a
  template or kind change that says pending approvals become stale
  (rule 13).
- A flat's Networks list shows the proxy provider under Private or Public,
  with the rule 6 detail.
- `SERVER_ONLY` in `ui.js` names the reverse proxy while a proxy provider is
  on; the console reads that from `GET /console/api/providers`. Proxy URLs
  are not loopback, so `extLink` already renders them as links.

## Documentation

- `networking.md`: a "Serving flats behind a reverse proxy" section with
  Caddy and nginx examples that forward the original `Host`, the rule 5
  warning, the shared-parent-domain cookie limit and the note for operators
  moving from a Local-listener shim. The providers table gains both rows.
- `configuration.md`: the two keys, the widened defaults and the cross-key
  checks.
- `network-provider-lifecycle.md`: the proxy backend, attach and detach,
  start order, its Manager paths and the policy token.
- `surfaces.md`: the provider IDs.
- `design.md` Exposure section, and the `docs/agent/` topics that list
  providers (regenerate the references).
- `docs/spec/README.md`: this spec's row.

## Tests

| Area | Test |
| --- | --- |
| Template | Accepted and refused templates, including the suffix length limit and `:443` with `https` and `:80` with `http`; `Label` with port, case, trailing dot, nested label, wrong suffix, empty label |
| Config | Mutual exclusion; template required; defaults require the grant; address conflicts only while a proxy kind is permitted (a no-proxy config using `127.0.0.1:7880` for another listener validates; turning a kind on there is refused; changing another listener's address to `proxy.addr` while a kind is permitted is refused); template and console origin overlap refused in both save orders; offline `flats config` and Settings agree |
| Listener | Served label; unknown label; nested label; console name; `/metrics`; 404 body without the `Host`; identity headers stripped; `Host` and `X-Forwarded-Proto` replaced, including a template port the proxy dropped; template change applies to the next request |
| App lifecycle | Turn on binds; bind conflict, missing template and failed save each refuse, save nothing and leave the port free; turn off refused while allowed, closes the listener after; second kind refused; full kind switch with a Public flat; changed `proxy.addr` used on the next turn-on; start with a taken address keeps the host up with proxy routes in `error`; non-loopback warning |
| Manager | Private, preview and Public paths for both IDs; preview refused on `proxy-public`; stops; policy token with and without a proxy |
| Core grants | Grant reconciles at once; revoke stops routes before saving; `proxy-public` revoke refused on a Public flat; new flats under each proxy default get the grant and event; existing flats do not |
| Approvals | A pending approval is stale after a template or kind change and not after a `proxy.addr` change |
| Address selection | The cases of the completion criteria, for flats and previews |
| Redirects | Arrival on Local, Tailscale, proxy and a public provider; fallback; 503 while no target |
| No-proxy golden | A fixture host without a proxy: flat views, previews, endpoints and redirects equal the recorded current output except the rule 30 cases |
| End to end | `httputil.ReverseProxy` on loopback that keeps the original `Host`, in front of the proxy listener, with and without TLS termination, `X-Forwarded-Proto` and the port in `Host`, driving MCP `get_flat`, `list_flats`, `open_preview`, a document edit and a server flat that echoes `X-Forwarded-Proto` |
| Console | `ui_test.mjs`: proxy links render as links from another device; `SERVER_ONLY` text with and without a proxy; the Private kind warning; the confirmation for a template or kind change |

## Risks

- **Template changes and caches.** Browsers and agents may hold old proxy
  addresses after a template change. Flats does not redirect old template
  hosts.
- **Private kind exposure.** A proxy without authentication exposes the
  Private flats the operator allowed, including editing. Flats limits this
  to granted flats and states it where the operator turns the kind on; it
  cannot detect the proxy's configuration.
