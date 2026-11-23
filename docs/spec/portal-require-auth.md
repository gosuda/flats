# Portal require_auth

Status: planned. The request is issue #222. It depends on [Portal sign-in](portal-sign-in.md) (#221), which is
implemented.

## Summary

An operator who runs a host for one company wants one rule: nothing goes out through Portal without sign-in. Per-flat
sign-in is one setting on one flat, and nothing checks it across the host. This spec adds the host setting
`portal.require_auth` (default `false`). While it is on, the host does not serve a flat on Portal unless the flat has
sign-in on. A flat that Portal served without sign-in stops being served when the setting turns on, and the refusal
shows in `flats status`, in the console and in the flat view, with the flat and the reason.

The setting covers the Portal route only. The other public providers stay under `network.permitted`, which turns each
of them off for the whole host.

## Before this change

- `portal.relays`, `portal.discovery`, `portal.max_active_relays` and `portal.hide` are the Portal host settings
  (`docs/configuration.md`). None of them needs sign-in.
- No host setting needs an approval. The approval actions are per flat (`applyApproval` in `internal/core/lifecycle.go`).
  The settings routes are on the console mux only, and `consoleRequest` refuses a request that carries
  `X-Flats-Client` (`internal/api/guard.go`). MCP has no tool for a setting, and `AutopilotNeeds` refuses an action that
  it does not know. A host setting change writes no `audit_events` row and sends no webhook event
  ([audit events](audit-events.md), scope; [event webhook](event-webhook.md), scope).
- `portal.hide` is the model of a setting that reaches every served flat. `applySettingChanges` calls `applyHostListing`
  when it changes (`internal/core/settings.go`, `internal/core/listing.go`). It visits each flat under the flat lock,
  and a failure is a flat event while the setting stays saved. A reload by SIGHUP lists the key as applied
  (`internal/core/reload.go`).
- Every open of the Portal route of a flat carries the stored sign-in (`PortalSignIn` in `internal/core/expose.go`,
  built by `portalSignIn`), or the mark `Redirect` of the rename redirect. The callers are `ensureLifecycle`,
  `reapplyExposure`, `reopenPublic` and `serveRedirect` in package `core`, and `servePublicNet` for a host without a
  provider manager. `Manager.servePortal` and `portal.Net.serve` act on it. A gate that cannot be built gives no route
  for a new entry and stops a served one (sign-in rule 35).
- A failed open of a provider shows as an endpoint with a state and a detail. The state `unavailable` is the state of a
  refusal by host policy, such as `host permission absent` (`refused` in `internal/expose/provider/manager.go`). No
  state `refused` exists. The states are `ready`, `starting`, `error`, `stopped` and `unavailable`
  ([network provider lifecycle](../network-provider-lifecycle.md)). The re-serve sets of `reapplyExposure` and
  `reopenPublic` skip only endpoints with an empty state or `stopped`, so an `unavailable` endpoint is served again at
  the next open.
- `flats status` prints the `system` object of `GET /api/status` as JSON. A Portal backend lists a flat whose gate
  failed as a host with the state `error` (`Net.Status` in `internal/expose/portal/portal.go`). The console shows the
  detail of a Portal endpoint on host Settings, Networks only. The flat page shows the detail of `proxy` and
  `proxy-public` rows, and it shows none for Portal.
- The public site handler reads the flat for each request and refuses a request on the sign-in rule 44 (`serveSite` in
  `internal/core/service.go`). A request on a provider route carries `core.RouteProvider`, which the provider manager
  sets (`tagged` in `internal/expose/provider/manager.go`).

## Scope

In scope: the setting, its storage and its change paths, the refusal of the Portal route, its endpoint, event, log
line and views, the check of each request on the Portal route, the console switch and texts, the amended specs, the
docs and the tests.

Out of scope:

- The other public providers (`tailscale-funnel`, `zrok`, `cloudflare-quick`, `netbird-expose`, `proxy-public`). The
  docs say that `network.permitted` is the host-wide switch for them.
- The private routes of a flat, previews, the console, MCP and the API.
- The rename redirect of an old slug. It sends no content of the flat (sign-in rule 41).
- An audit row and a webhook event for the setting. A host setting writes neither.
- An approval for the setting.

## Decisions

The owner of the repository decided four points:

1. The setting covers Portal only.
2. It changes as `portal.hide` does: the console, `config.json` with SIGHUP, and the offline `flats config set`. A
   change applies at once to the served routes. It needs no approval, and agents cannot change it.
3. A refused Portal route shows the endpoint state `unavailable` with a detail that names the setting and the next
   command. The host writes a flat event and a log line when a served route stops. The setting writes no audit row and
   no webhook event.
4. Public instance mode gets no special case. A flat that the mode made Public on Portal without sign-in is refused on
   Portal like any other flat.

The author of this spec decided these points, and each can change before the implementation:

- A valid sign-in setting means that sign-in is on, which is the sign-in provider of the flat not being empty. The host
  stores only settings that pass `appauth.Validate` (sign-in rule 4). A gate that cannot be built already gives no
  Portal route (sign-in rule 35), so the setting adds no second validity check.
- The Portal route of a refused flat opens nothing, so the flat gets no Portal identity file from an open that the
  setting refuses.
- A failed Portal open is a failed open like any provider failure. A flat that has Portal and another public provider
  granted, and no sign-in, cannot become Public while the setting is on. Its approval fails with the reason, and the
  flat stays Private (rule 19).
- Turning sign-in off for a flat stays possible while the setting is on. The Portal route then stops (rule 20).
- A second check on each request stops a route that a code path opened without the check of the open (rule 15).

## The setting

1. The key is `portal.require_auth` in the section `portal` of `config.json`. It is a boolean with the default `false`.
   The console names it `portal_require_auth`. `flats config set portal.require_auth true|false` stores it, and a value
   that is not `true` or `false` is refused with the allowed values.
2. Three paths change it: the console (Settings, rule 22), an edit of `config.json` that a SIGHUP reloads
   ([config reload](config-reload.md)), and the offline `flats config set` and `flats config unset`. A start reads the
   file, so a restart keeps the value. No service flag pins it.
3. No MCP tool, no `/api` route and no Autopilot operation changes it. The console route refuses a request that carries
   `X-Flats-Client`, as it does for every setting. A program that can reach the console address can still send a console
   request, as it can for every setting ([Autopilot](autopilot.md), rule 2).
4. A change applies at once to a running host (rules 16 to 18). The settings view lists it as applied, not as awaiting
   a restart.
5. A change writes no `audit_events` row and sends no webhook event. The flat event of rule 10 records the effect on a
   flat.

## What the host refuses

6. While the setting is on, the host serves no flat on Portal whose sign-in is off. Such a flat is refused. A flat with
   sign-in on is not affected by the setting. When its gate cannot be built, sign-in rule 35 stops its Portal route as
   before.
7. Every open of the Portal route of a flat carries whether the setting is on, which the host reads under the lock of
   the flat, as it reads the stored sign-in (sign-in rule 38). The open is refused when the setting is on, the open
   carries sign-in off, and it does not carry the mark `Redirect`. The open of the rename redirect is never refused.
8. For a new Portal entry, a refused open registers nothing with a relay, starts no HTTP server, and makes no identity
   file. For a served entry, a refused open stops the route and unregisters it from its relays, as sign-in rule 35 does
   for a gate that cannot be built. The route answers 503 until it stops. A stop that is not confirmed keeps the route
   registered, and it answers 503 and never the flat, as in sign-in rule 35.
9. A Portal backend that cannot run the gate refuses too. The same holds for a host without a provider manager (the
   legacy path of `servePublicNet`). No path serves a flat on Portal without sign-in while the setting is on.
10. The refusal shows in these places:
    - The Portal endpoint of the flat has the state `unavailable`, `ready` false and this detail:
      `portal.require_auth is on and this flat has no sign-in. Run flats signin set <slug> to turn sign-in on, or turn
      the setting off in the console.` The detail names the slug.
    - When a served route stops, the host writes one flat event (level `warn`, kind `exposure`) with the same text and
      one host log line with the slug. A refused open of a route that was not served writes the event only when its
      caller writes one (a restart or a deploy writes the `exposure` event today).
    - The Portal backend lists the slug as a host with the state `unavailable` and the same detail, as it lists a gate
      failure. So `system.public.hosts` of `flats status` holds the flat and the reason.
11. `flats status` adds one note line after the existing note when the setting is on and the Portal backend lists a
    refused flat: `  note: portal.require_auth is on, so Portal does not serve these flats, which have no sign-in:
    <slug>[, <slug>...]. Run flats signin set <slug>, or turn the setting off in the console.` The line names at most
    10 slugs, then `and N more`. Without a refused flat it adds no line.
12. The flat view (`GET /flats/{slug}`, MCP `get_flat` and `list_flats`) holds the endpoint with its state and detail
    (`endpoints[]`) and `connection_state` `unavailable` while the Portal route is the only public route of the flat.
    No MCP text says that the flat is available on its Portal address while the route is refused (public instance
    mode, principle 6).
13. The other routes of the flat stay. A refused flat keeps its private routes, and its other public providers that a
    grant permits keep serving (sign-in rule 12).
14. The refusal never falls back to open. The next open tries again: a deploy, a grant, a restart, a sign-in change or a
    change of the setting. A refused endpoint is among the routes that a re-serve opens again (the re-serve sets skip
    only an empty state and `stopped`), so turning the setting off or turning sign-in on opens the route.

## Each request

15. While the setting is on, the public site handler answers 403 with the text `This flat has no sign-in, and this host
    serves a flat on Portal only with sign-in.` to a request that arrived on the Portal route (`core.RouteProvider` is
    Portal) of a flat whose sign-in is off. It reads the flat and the setting for each request, as it reads the sign-in
    for sign-in rule 44. The check passes a request on any other provider, a request for a flat with sign-in on (sign-in
    rule 44 governs it) and a request that carries no provider mark. It never sees the rename redirect, which does not
    go through the site handler.

## Applying a change

16. When the setting changes to on, the host visits every flat under its lock, as `applyHostListing` does. For a flat
    that is Public, has a runtime, has sign-in off and has a Portal route or a Portal endpoint, it does these steps in
    order: it ends the open connections of the public routes of the flat (`revokeRoute`, as sign-in rule 20, step 3),
    serves the public routes of the flat again, so the Portal open is refused (rules 7 and 8) and every other public
    route gets a new epoch, and writes the flat event of rule 10. A visitor on a connection that was open on Portal
    gets no more content of the flat.
17. When the setting changes to off, the host visits every flat under its lock. For a flat that is Public, has a
    runtime, has sign-in off and has a refused Portal endpoint, it serves the public routes again, so the Portal route
    opens as a restart opens it. It opens only a route that the approved exposure of the flat already used, and it
    publishes nothing new. It writes a flat event of level `info`.
18. A failure for one flat writes a flat event of level `error`, leaves the setting saved, and does not stop the visit of
    the other flats. The next open of the route tries again. The host does the visit with a context that the request
    does not cancel, as `applyHostListing` does, so a client that drops the request does not leave a flat served. A
    reload by SIGHUP does the same visit, and it logs a failure of one flat as the console save does.

## Other changes of state

19. Making a flat Public needs an open Portal route when Portal is the public provider that the flat is granted. While
    the setting is on and the flat has sign-in off, the application of the visibility approval fails with the text of
    rule 10, as it fails when no public route opens, and the flat stays Private. A flat that has Portal and another
    public provider granted fails the same way, because a failed open of one provider is a failure of the open. The text
    adds: `Revoke the Portal grant under Networks, or turn sign-in on, or turn portal.require_auth off.`
20. `flats signin off` stays possible while the setting is on. The approval applies as before, and its step 4 refuses the
    Portal open, so the Portal route stops. The approval page and the result say so: while the setting is on, the title
    of such an approval is `Turn off sign-in for <slug>: its Portal address stops serving it` and the page does not say
    that anyone can open the address. With the setting off, the texts of sign-in rule 64 stay as they are.
21. A flat created by public instance mode with Portal as the default public network is Public with no sign-in. While
    the setting is on, its Portal route is refused, and the flat is reachable on no route that serves it, because the
    mode turns private links off. The endpoint detail of rule 10 says what to do. The mode and the setting stay
    independent, and neither changes the other.

## Surfaces

22. The console Settings tab gets the toggle `Serve flats on Portal only with sign-in` after `Hide public flats from
    relay listings`, with the hint `While on, Portal serves a flat only when the flat has sign-in. Other public
    providers are not covered: turn them off with their own switch under Networks.` It saves as `portal_require_auth`
    through `PUT /console/api/settings`.
23. The Networks section of the Settings tab of a flat shows the Portal row with the badge of the endpoint state and the
    detail, as it shows the detail of a `proxy` row, whenever the endpoint has a detail and is not ready. So the reason
    of rule 10 is on the flat page. This also shows the detail of a Portal endpoint in the state `error`, as for a
    gate that cannot be built.
24. The share dialog keeps its texts. The state `unavailable` already says `Link is unavailable. Check this flat's
    Networks settings, then refresh.`
25. `GET /console/api/settings` lists `portal_require_auth` with the current value and the config key, as it lists
    `portal_hide`. It is not `restart_required`. The settings routes exist on the console mux only, so `/api` has none.

## Compatibility

26. With the setting off, nothing changes. A new host and an upgraded host have it off. There is no schema change.
27. An older release refuses a `config.json` that holds `portal.require_auth`, because it parses the file strictly
    ([config reload](config-reload.md), requirement 2). The docs say to run `flats config unset portal.require_auth`
    before a return to an older release.
28. Rule 8 and rule 15 add no behavior for a flat with sign-in on.

## Changes to other specs

29. These specs change with the implementation:
    - [Portal sign-in](portal-sign-in.md): the out-of-scope line on host-wide sign-in names this spec, rule 64 gets the
      title of rule 20 here, and the known limits get one line that a flat of sign-in rule 7 can be refused on Portal by
      the setting;
    - [config reload](config-reload.md): requirement 4 lists `portal.require_auth` as a key with immediate effect;
    - [Operator keys and console domains](console-settings.md): the settings list and the console toggle;
    - [public instance mode](public-instance.md): a note on rule 21 here;
    - [audit events](audit-events.md) and [event webhook](event-webhook.md): the scope lines that name host settings gain
      this setting as one that writes nothing;
    - `docs/spec/README.md`: the row of this spec.

## Docs

30. These docs and texts change:
    - `docs/configuration.md`: a row in the `config.json` table (default `false`, applies immediately), a paragraph
      after the `portal.hide` text that says what the setting refuses, that it covers Portal only, that
      `network.permitted` is the switch for the other providers, that agents cannot change it, and the downgrade note of
      rule 27;
    - `docs/networking.md`: the Portal sign-in section says that a host can require sign-in for every flat on Portal;
    - `docs/network-provider-lifecycle.md`: a Portal refusal by the setting is an `unavailable` endpoint;
    - `docs/surfaces.md`: the console toggle, the Networks row and the `flats status` note;
    - the CLI help of `flats status`, if it lists its notes;
    - the tests that pin changed texts change with them.

## Known limits

- The setting covers Portal only. A flat can be Public on another public provider without sign-in, because sign-in
  itself covers Portal only (sign-in decision 1).
- A short request that the Portal route accepted before the change can finish after it. Rule 16 ends the open
  connections, and rule 15 refuses the next request.
- The rename redirect of an old slug stays on Portal, and its target is the Portal address of the flat. While the flat
  is refused, that address answers nothing.
- A program that can reach the console address can change the setting (rule 3).
- A relay can learn that a flat existed from an earlier registration. The setting does not change what a relay holds.

## Completion criteria

Test seams are the ones of sign-in: the fields `expose` and `runHTTP` of `portal.Net`, and a fake provider manager net
in package `core`. No test needs the network.

- The issue, a new flat: with the setting on, a flat with sign-in off that is Public and granted Portal has no Portal
  route. The fake exposure is never called, the fake HTTP run never gets the handler of the flat, no identity file
  exists, and the endpoint has the state `unavailable` and the detail of rule 10.
- The issue, a served flat: a flat that Portal serves without sign-in stops when the setting turns on. The route
  unregisters, the open connections end, an endpoint with the detail exists, the flat event and the host log line are
  written, and a second public provider route of the flat serves again with a new epoch. With the setting off again, the
  Portal route opens.
- The issue, a restart: with the setting on in `config.json`, a new `Service` and a new `Net` on the same folder open no
  Portal route for the flat. With it off, they open the route.
- Change paths: a console save applies at once. A SIGHUP reload lists the key as applied and does the visit of rule 16.
  `flats config set` and `unset` store and remove it while the host is stopped. A value that is not a boolean is refused.
  A console request with `X-Flats-Client` is refused. MCP has no tool and `/api` has no route for it, and
  `AutopilotNeeds` has no operation for it.
- Not affected: a flat with sign-in on serves through its gate with the setting on. A flat with sign-in on whose gate
  cannot be built is stopped by sign-in rule 35 as before. The rename redirect of an old slug opens with the setting
  on. A private route and a preview are unchanged. Another public provider route of a refused flat serves.
- Each request: a request marked with the Portal provider for a flat with sign-in off gets 403 with the text of rule
  15, also when a test opened the route without the check of the open. The same request marked with another provider
  passes. A request for a flat with sign-in on gets the answers of sign-in rule 44.
- The backends: a Portal backend without the gate and the legacy path without a provider manager refuse the open.
- Races: a change of the setting and an open of the Portal route of one flat that run at the same time, many times, give
  one of the two orders and never a served flat without sign-in after both end.
- Visibility: the application of a visibility approval for a flat with Portal granted and sign-in off fails with the text
  of rule 19 and leaves the flat Private, also with a second public provider granted.
- Sign-in off: the approval of `flats signin off` with the setting on stops the Portal route, and its title and result
  are the texts of rule 20.
- Public instance mode: a flat that the mode created with Portal as the default public network has a refused Portal route
  with the setting on, and no MCP text promises its Portal address.
- Views: the flat view, `get_flat` and `list_flats` hold the endpoint and its detail. `flats status` holds the host row
  and the note of rule 11, and no note without a refused flat. The console UI test shows the toggle with its hint, the
  Portal row with the badge and the detail on the Networks section, and the settings view with the key.
- No audit row and no webhook event follow a change of the setting.
- The docs: `docs/configuration.md` has the row and the paragraph. The doc checks pass, and the generated references
  match their sources.

## Open questions

None.
