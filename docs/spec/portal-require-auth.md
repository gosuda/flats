# Portal require_auth

Status: implemented. The request is issue #222. It depends on [Portal sign-in](portal-sign-in.md) (#221), which is
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
  it does not know. A host setting change writes no `audit_events` row and sends no webhook event ([audit
  events](audit-events.md), scope; [event webhook](event-webhook.md), scope).
- `portal.hide` is the model of a setting that reaches every served flat. `applySettingChanges` calls `applyHostListing`
  when it changes (`internal/core/settings.go`, `internal/core/listing.go`). It visits each flat under the flat lock,
  and a failure is a flat event while the setting stays saved. A save runs the visit outside the settings lock, and a
  reload by SIGHUP runs it under `publicEditMu`, so two changes can run their visits at the same time. A change
  reaches `applySettingChanges` only when the value changes (`SettingsSource.update`), and a reload lists any key of
  `settingKeys` as applied (`internal/core/reload.go`).
- Every open of the Portal route of a flat that the service makes carries the stored sign-in (`PortalSignIn` in
  `internal/core/expose.go`, built by `portalSignIn`), or the mark `Redirect` of the rename redirect. The callers are
  `ensureLifecycle`, `reapplyExposure`, `reopenPublic` and `serveRedirect` in package `core`, and `servePublicNet` for
  a host without a provider manager. `Manager.servePortal` and `portal.Net.serve` act on it. A gate that cannot be
  built gives no route for a new entry and stops a served one (sign-in rule 35). An open from outside the service
  carries nothing (`Net.Serve`, a nil `SignIn`), and keeps the gate that the route has (sign-in rule 38). The legacy
  redirect of `serveRedirect` uses such a plain `Serve` on a backend without the gate.
- Stopping a Portal route does not end a connection that is open on it. `sdk.RunHTTP` shuts its HTTP server down with a
  5 second limit, and `http.Server.Shutdown` closes no hijacked connection, such as a WebSocket, and cancels no running
  request. Only the route epoch (`revokeRoute`, key `public:<slug>`) ends them, and one epoch covers every public
  route of a flat (`internal/core/route_epoch.go`).
- A failed open of a provider shows as an endpoint with a state and a detail. The state `unavailable` is the state of a
  refusal by host policy, such as `host permission absent` (`refused` in `internal/expose/provider/manager.go`). No
  state `refused` exists. The states that Flats shows are `ready`, `starting`, `error`, `stopped` and `unavailable`,
  and for some providers `needs-login`, `key-expiring` and `idle` ([network provider
  lifecycle](../network-provider-lifecycle.md)). The re-serve sets of `reapplyExposure` and `reopenPublic` skip only
  endpoints with an empty state or `stopped`, so an `unavailable` endpoint is served again at the next open.
- `Manager.servePublic` returns the joined errors of its providers also when one of them opened, and `ensureLifecycle`
  fails on that error, so a failed open of one provider fails the apply of a visibility approval and the approval rolls
  back (`rollbackPublicExposure`).
- `flats status` reads `GET /api/status` only and prints its `system` object as JSON (`internal/cli/ops.go`). The
  console shows the detail of a Portal endpoint on host Settings, Networks only. The flat page shows the detail of
  `proxy` and `proxy-public` rows, and it shows none for Portal.
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

The owner of the repository decided seven points:

1. The setting covers Portal only.
2. It changes as `portal.hide` does: the console, `config.json` with SIGHUP, and the offline `flats config set`. A
   change applies at once to the served routes. It needs no approval, and agents cannot change it.
3. A refused Portal route shows the endpoint state `unavailable` with a detail that names the setting and the next
   command. The host writes a flat event and a log line when a served route stops. The setting writes no audit row and
   no webhook event.
4. Public instance mode gets no special case. A flat that the mode made Public on Portal without sign-in is refused on
   Portal like any other flat.
5. A refusal by the setting is not a failure of the other public providers of the flat. When a flat has Portal and
   another public provider granted and no sign-in, only Portal is refused and the other provider serves. The apply of a
   visibility approval fails only when Portal is the only public route that would open.
6. When the setting turns on, the host ends the open public connections of a refused flat and serves its other public
   routes again with a new epoch, as sign-in does (sign-in rule 20, steps 3 and 4), because stopping the Portal route
   does not end a connection that is open on it.
7. The two fail-closed paths of rule 18 differ. When the host cannot read one flat, it stops the Portal route of that
   flat and also ends the open public connections of that flat, and the other public routes of the flat answer 503 until
   a deploy, a grant or a restart. When the host cannot list the flats, it stops the Portal routes only. An open Portal
   connection of such a flat stays until it closes or the host restarts. This is a known limit, which the flat event and
   the host log line name. A store fault then does not take every public route of the host down.

The author of this spec decided these points, and each can change before the implementation:

- A valid sign-in setting means that sign-in is on, which is the sign-in provider of the flat not being empty. The host
  stores only settings that pass `appauth.Validate` (sign-in rule 4). A gate that cannot be built already gives no
  Portal route (sign-in rule 35), so the setting adds no second validity check.
- A refused open of a Portal route that is not served makes no identity file.
- `flats signin off` stays possible while the setting is on. The Portal route then stops (rules 21 and 22).
- A second check on each request stops a route that an open without the check serves (rules 7 and 15).
- The list of refused flats in `flats status` is derived from the stored facts when the status is read. No record of a
  refusal outlives its cause.
- A failure of the visit fails closed: it stops a Portal route that it cannot check (rule 18). The slugs for a failed
  list come from the in-memory map of live flats of the service (`Service.live`), which needs no store read.

## The setting

1. The key is `portal.require_auth` in the section `portal` of `config.json`. It is a boolean with the default `false`.
   The console names it `portal_require_auth`. `flats config set portal.require_auth true|false` stores it, and a value
   that is not `true` or `false` is refused with the allowed values.
2. Three paths change it: the console (Settings, rule 24), an edit of `config.json` that a SIGHUP reloads ([config
   reload](config-reload.md)), and the offline `flats config set` and `flats config unset`. A start reads the file, so
   a restart keeps the value. No service flag pins it.
3. No MCP tool, no `/api` route and no Autopilot operation changes it. The console route refuses a request that carries
   `X-Flats-Client`, as it does for every setting. A program that can reach the console address can still send a
   console request ([Autopilot](autopilot.md), rule 2), and a program that can write `config.json` and signal the host
   can change it with no audit row.
4. A change applies at once to a running host (rules 16 to 18). The settings view lists it as applied, not as awaiting
   a restart.
5. A change writes no `audit_events` row and sends no webhook event. The flat event of rule 10 records the effect on a
   flat.

## What the host refuses

6. While the setting is on, the host serves no flat on Portal whose sign-in is off. Such a flat is refused. A flat with
   sign-in on is not affected by the setting. When its gate cannot be built, sign-in rule 35 stops its Portal route as
   before.
7. Every open of the Portal route of a flat that the service makes carries whether the setting is on, which the host
   reads under the lock of the flat, as it reads the stored sign-in (sign-in rule 38). The open is refused when the
   setting is on, the open carries sign-in off, and it does not carry the mark `Redirect`. The open of the rename
   redirect is never refused. An open that carries nothing, which only a call from outside the service makes, is not
   refused, because the backend cannot know the setting. On a host with a provider manager, which marks each request
   with its provider, rule 15 then refuses each request on it while the setting is on, as sign-in rule 44 does for
   sign-in. A host without a provider manager is rule 9.
8. For a new Portal entry, a refused open registers nothing with a relay, starts no HTTP server, and makes no identity
   file. For a served entry, a refused open stops the route and unregisters it from its relays, as sign-in rule 35 does
   for a gate that cannot be built. The route answers 503 until it stops. A stop that is not confirmed keeps the route
   registered, and it answers 503 and never the flat, as in sign-in rule 35.
9. A Portal backend that cannot run the gate refuses too. The same holds for a host without a provider manager (the
   legacy path of `servePublicNet`, which production never uses). No open that the service makes serves a flat on
   Portal without sign-in while the setting is on. On the legacy path a Portal route that is already served keeps
   serving until the next restart, because the visit of rules 16 and 17 needs a provider manager and a request
   there carries no provider mark for rule 15. This is a known limit of a path that is a test seam.
10. The refusal shows in these places:
    - The Portal endpoint of the flat has the state `unavailable`, `ready` false and a detail that names the slug and
      the setting. For a flat that has no grant for another public provider that sign-in excludes, the detail is:
      `portal.require_auth is on and <slug> has no sign-in. Run flats signin set <slug> to turn sign-in on, or turn the
      setting off in the console.` A flat that is refused is always Public. For a flat that has a grant for another
      public provider that sign-in excludes, the host refuses `flats signin set` (sign-in rule 8), so the detail is:
      `portal.require_auth is on and <slug> has no sign-in. Sign-in is refused while a grant for <providers> exists. Make
      the flat Private, revoke <providers> under Networks, then run flats signin set <slug>. Or turn the setting off in the
      console.` Every action in these texts is one that the host accepts for such a flat. When the stop of a served
      route is not confirmed, the result of the open has the state `error` and the detail followed by `; the stop of the
      route is not confirmed: <cause>`, as in sign-in rule 35. The route stays registered, so the flat view then shows the
      row of the Portal backend: the state `error` and the detail `teardown pending confirmation` (`core.TeardownPending`),
      with the reason of the gate when the visit stopped it. The flat event of the visit names the setting and the cause.
    - When a served route stops, the host writes one flat event (level `warn`, kind `exposure`) with the same text and
      one host log line with the slug. A refused open of a route that was not served writes the event only when its
      caller writes one (a restart or a deploy writes the `exposure` event today).
    - `GET /api/status` holds two new members in `system`: `portal_require_auth`, the value of the setting, and
      `portal_refused`, the sorted slugs of the flats that the setting refuses now. A flat is in the list when the
      setting is on, Portal is permitted for the host (`network.permitted`), the flat is Public with a live version, it
      has a Portal grant and its sign-in is off. A flat that Portal does not serve for another cause (Portal is not
      permitted for the host, or the flat has no live version) is not in the list. The host derives the list from the
      stored facts when it answers, so a flat that becomes Private, is deleted, loses its Portal grant or gets sign-in
      leaves the list at once, and the list is empty when the setting is off. When the host cannot read the store, the
      status still answers, because the CLI uses it as its liveness check: `portal_require_auth` has the value that the
      settings hold in memory and `portal_refused` is `null`.
11. `flats status` adds one note line after the existing note when `portal_require_auth` is true: `  note:
    portal.require_auth is on, so Portal serves only flats that have sign-in.` When `portal_refused` is not empty, the
    line goes on with ` Portal does not serve these flats: <slug>[, <slug>...]. The Networks section of each flat in the
    console says what to do, or turn the setting off in the console.` It names at most 10 slugs, then `and N more`. When
    `portal_refused` is `null`, the line goes on with ` The refused flats cannot be read now.` With the setting off it
    adds no line.
12. The flat view (`GET /flats/{slug}`, MCP `get_flat` and `list_flats`) holds the endpoint with its state and detail
    (`endpoints[]`) and `connection_state` `unavailable` while the Portal route is the only public route of the flat.
    No MCP text says that the flat is available on its Portal address while the route is refused (public instance
    mode, principle 6).
13. The other routes of the flat stay. A refused flat keeps its private routes (sign-in rule 12), and its other public
    providers that a grant permits keep serving (sign-in rule 10 excludes them only for a flat with sign-in on).
14. The refusal never falls back to open. The next open tries again: a deploy, a grant, a restart, a sign-in change or a
    change of the setting. A refused endpoint is among the routes that a re-serve opens again (the re-serve sets skip
    only an empty state and `stopped`), so turning the setting off or turning sign-in on opens the route. On the legacy
    path of rule 9 only a deploy or a restart opens it again.

## Each request

15. While the setting is on, the public site handler answers 403 with the text `This flat has no sign-in, and this host
    serves a flat on Portal only with sign-in.` to a request that arrived on the Portal route (`core.RouteProvider` is
    Portal) of a flat whose sign-in is off. It reads the flat and the setting for each request, as it reads the sign-in
    for sign-in rule 44. The read of the setting does not wait for a settings save, which holds the settings lock
    across its file write: the check keeps an atomic copy of the value. The check passes a request on any other
    provider, a request for a flat with sign-in on (sign-in rule 44 governs it) and a request that carries no provider
    mark. It never sees the rename redirect, which does not go through the site handler.

## Applying a change

16. When the setting changes to on, the host visits every flat under its lock, as `applyHostListing` does. Each step of
    the visit reads the current value of the setting under the lock of the flat and does nothing when it is off again.
    The visit first checks from memory whether the provider manager reports a registered Portal route for the flat. A
    flat without one is left as it is, also when a read of the flat would fail: nothing serves it on Portal, a later open
    is refused by rule 7, and its other public routes keep their epoch (decision 6 applies only to a connection that is
    open on Portal). For a flat with a registered Portal route that is Public and has sign-in off, the visit does these
    steps in order:
    1. It reads what the re-serve needs: the flat, its permitted providers, its observed endpoints and its listing.
       A failure here is rule 18.
    2. For a flat with a runtime, it ends the open connections of the public routes of the flat (`revokeRoute`, as
       sign-in rule 20, step 3), and serves the public routes of the flat again. The Portal open is refused (rules 7
       and 8), and every other public route gets a new epoch.
    3. For a flat without a runtime that has a registered Portal route (a failed rename can leave one), it stops the
       Portal route directly, as sign-in rule 35 stops a route whose gate cannot be built.
    4. It writes the flat event of rule 10.
    A visitor on a connection that was open on Portal gets no more content of the flat. A visitor on another public
    provider of the flat reconnects through the re-served route.
17. When the setting changes to off, the host visits every flat under its lock. Each step reads the current value of
    the setting and does nothing when it is on again. For a flat that is Public, has a runtime and has a Portal grant,
    and whose Portal route is not registered or is registered with a stop that was not confirmed (it answers 503), the
    visit serves the public routes again, so the Portal route opens as a restart opens it. A serve of an entry whose
    teardown is pending finishes that teardown first. This covers a flat whose Portal endpoint is `unavailable` by rule
    10, and a flat that rule 18 stopped, with sign-in on or off. It opens only a route that the approved exposure of the
    flat already used (the flat is Public and has the grant), and it publishes nothing new. When the route opens, the
    visit writes a flat event of level `info`. When the open fails, for example because Portal is not permitted for the
    host, it writes a flat event of level `warn` with the cause and goes on. A read that fails opens nothing for that
    flat: the route stays stopped, the visit writes a flat event of level `error` and goes on, and the next open of the
    route tries again. The visit applies to a host with a provider manager (rule 9).
18. The visit fails closed:
    - A read that fails for a flat with a registered Portal route (step 1 of rule 16) first ends the open public
      connections of that flat (`revokeRoute`, an in-memory cancel that needs no read), so that its running requests
      end, and then stops the Portal route as sign-in rule 28 does (`closePortal`) (decision 7). Its other public
      routes answer 503 on the cancelled epoch until a deploy, a grant or a restart serves them again. The host writes
      a flat event of level `error`: `portal.require_auth turned on, but the host cannot read <slug> to check its
      sign-in: <cause>. Its Portal route is stopped and its public connections are ended. Its other public routes
      answer 503 until a deploy, a grant or a restart.` The visit goes on with the other flats.
    - A list of the flats that fails stops the Portal route of every flat in the in-memory map of live flats of the
      service (`Service.live`) that the provider manager reports with a registered Portal route, each under the lock
      of its flat, which the stop needs. A slug that `Service.redir` holds as the source of a redirect is skipped,
      because `Service.live` can hold an old slug after a rename, and the route of an old slug is a redirect that
      rule 7 never refuses. The path ends no connection (decision 7). The stop of a flat with a request that is still
      running does not confirm: its route stays registered and answers 503 and never the flat. The flat event for each
      flat says: `... its Portal route is stopped. A Portal connection that is open stays until it closes or the host
      restarts.` The host writes one host log line that gives the number of flats that it stopped and their slugs (at
      most 20, then `and N more`). A flat with sign-in on loses its Portal route the same way.
    - A flat that these two paths stopped, or whose stop was not confirmed, is opened again by a deploy, a grant, a
      restart, a sign-in change, or the visit of rule 17 after the setting is turned off. A stop that waits for a running
      request is confirmed only after that request has ended: each earlier attempt waits for the shutdown limit of the
      backend and fails, and the flat event says so.
    - When the write of a flat event fails, as it does with a store fault, the host log holds the text of the event with
      the slug. So the log names every flat that a path of this rule stopped.
    - A failure of the re-serve after `revokeRoute` leaves the Portal route stopped and the other public routes of the
      flat answering 503 on the cancelled epoch until a deploy, a grant or a restart serves them again. The flat event
      names the failure and says that a public route that did not open again answers 503.
    - A save of the same value runs no visit, because only a change of the value does.
    - The visit uses a context that the request does not cancel, as `applyHostListing` does. A client that drops the
      request does not leave a flat served. A reload by SIGHUP does the same visit and logs a failure as a save does.
    - Two changes that run at the same time end with the state of the last stored value. Each open and each step of a
      visit read the current value under the lock of the flat (rules 7, 16 and 17), and the stops of the failure paths
      run under the same lock, so a late visit of the earlier change does nothing or is undone by the visit of the later
      one.

## Other changes of state

19. Making a flat Public with Portal granted and sign-in off, while the setting is on, refuses the Portal route only
    (decision 5). When another public provider opens, the visibility approval applies, and its result says that Portal
    is refused, with the text of rule 10. When Portal is the only public route that would open, the application fails
    with the text of rule 10, as it fails when no public route opens, and the flat stays Private. The text names only
    actions that the host accepts for the flat (rule 10).
20. A grant of Portal to a Public flat without sign-in, while the setting is on, is recorded. Its route is refused, and
    the result of the grant says so with the text of rule 10, in place of the text that tells the operator to allow the
    provider again or to deploy or restart.
21. `flats signin off` stays possible while the setting is on. The approval applies as before, and its step 4 refuses the
    Portal open, so the Portal route stops. The title and the warning of the approval page read the setting when the
    page renders: while it is on, the title is `Turn off sign-in for <slug>: its Portal address stops serving it` and the
    page does not say that anyone can open the address. With the setting off, the texts of sign-in rule 64 stay as they
    are. The result of the approval reads the setting at the apply. While the setting is on and the flat is Public, it says
    that the Portal route of the flat is stopped; otherwise it is as before. When the setting changes between the render and the decision, the result tells the truth and the approval is
    not stale.
22. A flat created by public instance mode with Portal as the default public network is Public with no sign-in. While
    the setting is on, its Portal route is refused, and the flat is reachable on no route that serves it, because the
    mode turns private links off. The endpoint detail of rule 10 says what to do. The mode and the setting stay
    independent, and neither changes the other. The agent guide of the mode says that Portal refuses such a flat and that
    the agent must not tell the user that it is available.

## Surfaces

23. The console Settings tab gets the toggle `Serve flats on Portal only with sign-in` after `Hide public flats from
    relay listings`, with the hint `While on, Portal serves a flat only when the flat has sign-in. Other public
    providers are not covered: turn them off with their own switch under Networks.` It saves as `portal_require_auth`
    through `PUT /console/api/settings`.
24. The Networks section of the Settings tab of a flat shows the Portal row with the badge of the endpoint state and the
    detail, as it shows the detail of a `proxy` row, whenever the endpoint has a detail and is not ready. So the reason
    of rule 10 is on the flat page. This also shows the detail of a Portal endpoint in the state `error`, as for a
    gate that cannot be built, with the setting on or off.
25. The share dialog keeps its texts. The state `unavailable` already says `Link is unavailable. Check this flat's
    Networks settings, then refresh.`
26. `GET /console/api/settings` lists `portal_require_auth` with the current value and the config key, as it lists
    `portal_hide`. It is not `restart_required`. The settings routes exist on the console mux only, so `/api` has none.

## Compatibility

27. With the setting off, no behavior changes. The one display change is rule 24, which shows the detail of a Portal
    endpoint that is not ready, also with the setting off. A new host and an upgraded host have the setting off. There
    is no schema change.
28. An older release refuses a `config.json` that holds `portal.require_auth`, because it parses the file strictly
    ([config reload](config-reload.md), requirement 2). The docs say to run `flats config unset portal.require_auth`
    before a return to an older release. `flats backup` copies `config.json`, so a restore brings back the value of the
    time of the backup. The docs say so.
29. Rules 8, 15 and 16 add no behavior for a flat with sign-in on, except the stop that rule 18 gives when a read fails.

## Changes to other specs

30. These specs change with the implementation:
    - [Portal sign-in](portal-sign-in.md): the out-of-scope line on host-wide sign-in names this spec, rule 64 gets the
      title of rule 21 here, and the known limits get one line that the setting can refuse the Portal route of a flat
      that sign-in rule 7 allows, and the lists of occasions that open a Portal route (rules 35 and 38) gain the visit of
      rule 17 here;
    - [config reload](config-reload.md): requirement 4 lists `portal.require_auth` as a key with immediate effect;
    - [public instance mode](public-instance.md): a note on rule 22 here;
    - [event webhook](event-webhook.md): the scope line that names the host-wide settings gains this setting as one that
      sends nothing ([audit events](audit-events.md) already excludes every change of `config.json`);
    - `docs/spec/README.md`: the row of this spec.

## Docs

31. These docs and texts change:
    - `docs/configuration.md`: a row in the `config.json` table (default `false`, applies immediately), a paragraph
      after the `portal.hide` text that says what the setting refuses, that it covers Portal only, that
      `network.permitted` is the switch for the other providers, that agents cannot change it, the downgrade note and the
      restore note of rule 28, the known limits of the fail-closed paths (rule 18) and of the end of open connections
      (decision 6), and the list of changes that send no webhook event;
    - `docs/networking.md`: the Portal sign-in section says that a host can require sign-in for every flat on Portal;
    - `docs/network-provider-lifecycle.md`: a Portal refusal by the setting is an `unavailable` endpoint, and the grant
      result and the joined error of an open follow rules 19 and 20;
    - `docs/surfaces.md`: the console toggle, the Networks row, the `flats status` note and members, and the title and
      the warning of the sign-in off approval;
    - `docs/design.md`: the Portal sign-in design text and Exposure gain the setting;
    - `docs/agent/topics/approvals.md`: the agent guide for Portal sign-in says that the setting can stop a Portal route
      that sign-in off would open;
    - the agent guide of public instance mode, `docs/agent/public-instance.md`, as rule 22 says;
    - the CLI help of `flats status`, if it lists its notes;
    - the tests that pin changed texts change with them.

## Known limits

- The setting covers Portal only. A flat can be Public on another public provider without sign-in, because sign-in
  itself covers Portal only (sign-in decision 1).
- Turning the setting on ends the open connections of the other public providers of a refused flat that has a registered
  Portal route, and they reconnect (decision 6). When the re-serve fails, those routes answer 503 until a deploy, a
  grant or a restart (rule 18).
- When the host cannot list the flats, it stops the Portal routes and ends no connection (decision 7). A Portal
  connection that was open stays until it closes or the host restarts. A flat with sign-in on also loses its Portal route
  until one of the opens of rule 18 serves it again.
- On the legacy path without a provider manager, a Portal route that is already served keeps serving until the next
  restart (rule 9).
- An open that is not the visit, such as a grant or a key change, can stop the Portal route of a flat between the change of
  the setting and the visit of that flat. It ends no connection that is open on Portal, because only the visit revokes the
  epoch. Such a connection stays until it closes or the host restarts. That stop writes no flat event and no host log line of
  rule 10. Rule 7 keeps every later open refused.
- A Portal route whose unregistration the relay refused stays registered, in the state `error`, because the Portal SDK closes
  once. The backend counts the stop as confirmed after the lease of the relay has expired (2 minutes), so the visit that
  turns the setting off opens such a route only after that time. A route that a running request holds is confirmed only after
  the request has ended, and each attempt before that waits for the shutdown limit of the backend. A deploy, a grant, a restart, a sign-in change, or the next
  visit that turns the setting off, opens it then.
- A short request that the Portal route accepted before the change can finish after it. Rule 16 ends the open
  connections, and rule 15 refuses the next request.
- The rename redirect of an old slug stays on Portal, and its target is the Portal address of the flat. While the flat
  is refused, that address answers nothing.
- A program that can reach the console address, and a program that can write `config.json` and signal the host, can
  change the setting (rule 3).
- A relay can learn that a flat existed from an earlier registration. The setting does not change what a relay holds.

## Completion criteria

Test seams are the ones of sign-in: the fields `expose` and `runHTTP` of `portal.Net`, and a fake provider manager net
in package `core`. A failure of a read uses a store that a test makes fail. No test needs the network.

- The issue, a new flat: with the setting on, a flat with sign-in off that is Public and granted Portal has no Portal
  route. The fake exposure is never called, the fake HTTP run never gets the handler of the flat, no identity file
  exists, and the endpoint has the state `unavailable` and the detail of rule 10. For a flat that is also granted a
  second public provider whose route is registered, the detail is the second form of rule 10 and names only actions
  that the host accepts: the test makes each named action and sees it accepted.
- The issue, a served flat: a flat that Portal serves without sign-in stops when the setting turns on. The route
  unregisters, the open connections end (a WebSocket that is open on Portal closes), an endpoint with the detail exists,
  the flat event and the host log line are written, and a second public provider route of the flat serves again with a
  new epoch. With the setting off again, the Portal route opens. A flat without a runtime that has a registered Portal
  route is stopped too. A flat with a Portal grant and no registered Portal route keeps its epoch, and its other public
  routes keep their connections.
- The issue, a restart: with the setting on in `config.json`, a new `Service` and a new `Net` on the same folder open no
  Portal route for the flat. A value that a running host stored through the console is read by the next start. With the
  setting off, they open the route.
- Change paths: a console save applies at once. A SIGHUP reload lists the key as applied and does the visit of rule 16.
  `flats config set` and `unset` store and remove it while the host is stopped. A value that is not a boolean is refused.
  A console request with `X-Flats-Client` is refused. MCP has no tool and `/api` has no route for it, and
  `AutopilotNeeds` has no operation for it.
- Fail closed, one flat: with a store that fails the read of one flat, the Portal route of that flat stops, a WebSocket
  that is open on its Portal URL closes, its other public routes answer 503 on the cancelled epoch, the flat event of
  rule 18 is written, and the visit goes on with the other flats. A flat that is Public on another provider only, with
  no registered Portal route, keeps its epoch and its route when its read fails. A running request that is not a
  WebSocket ends first, because the revoke comes before the stop, and the stop confirms.
- Fail closed, the list: with a store that fails the list of flats, the Portal route of every live flat with a registered
  Portal route stops, including a flat with sign-in on, and each flat event and the host log line name the open
  connections that stay. A WebSocket that was open on Portal stays open (the stated limit of decision 7). The route of a
  rename redirect still answers. After the setting turns off, the visit of rule 17 opens the stopped routes again, for a
  flat with sign-in off and for one with sign-in on, whose gate builds again. A slug that `Service.live` holds and `Service.redir` holds as a
  redirect source is not stopped. A flat with a running request that is not a WebSocket stays registered and answers
  503, and the visit of rule 17 opens it after the setting turns off and that request has ended. With a store that also fails the write of flat
  events, the host log holds the text of each event with the slug, and its one line names the stopped flats. A failed
  read on the off-visit leaves that flat stopped and writes the `error` event.
- Fail closed, other: a failure of the re-serve after `revokeRoute` leaves the Portal route stopped, and the flat event
  names the failure. A save whose request is cancelled after the commit still completes the visit. Two changes that
  run at the same time, many times, end with the state of the last value, also when the visit of the earlier change
  runs late.
- A stop that was not confirmed: with the real provider manager and the real Portal backend, a route whose unregistration the
  relay refused stays registered in the state `error`, with the detail of the backend, on the path of the visit and on the path
  of a failed list. After the lease has expired, the visit that turns the setting off opens it again. A registered route in the
  state `error` for another cause, and a failed read of the routes, are not taken for it: the first is left alone, and the second
  writes the `error` event of rule 17.
- Not affected: a flat with sign-in on serves through its gate with the setting on. A flat with sign-in on whose gate
  cannot be built is stopped by sign-in rule 35 as before. The rename redirect of an old slug opens with the setting
  on, on a backend with the gate and on one without it (the plain `Serve` of `serveRedirect`). A private route and a
  preview are unchanged. Another public provider route of a refused flat serves.
- Each request: a request marked with the Portal provider for a flat with sign-in off gets 403 with the text of rule
  15, also when a test opened the route without the check of the open, and when an open that carries nothing served a
  new entry (rule 7) through a provider manager. The same request marked with another provider passes. A request for a
  flat with sign-in on gets the answers of sign-in rule 44.
- The backends: a Portal backend without the gate and the legacy path without a provider manager refuse the open. On
  the legacy path, a Portal route that is already served keeps serving until the next restart (the stated limit of rule
  9), and turning the setting off opens an unserved route at the next deploy.
- Races: a change of the setting and an open of the Portal route of one flat that run at the same time, many times, give
  one of the two orders and never a served flat without sign-in after both end.
- Visibility: the application of a visibility approval for a flat with Portal granted and sign-in off fails with the text
  of rule 10 when Portal is the only public route, and leaves the flat Private. With a second public provider granted, it
  applies, the other provider serves, Portal is refused, and the result says so. A grant of Portal to a Public flat
  without sign-in says that the route is refused.
- Sign-in off: the approval of `flats signin off` with the setting on stops the Portal route, and its title and result
  are the texts of rule 21, also for a flat that is not Public at the apply. A change of the setting between the render
  and the decision gives a result that tells the truth.
- Public instance mode: a flat that the mode created with Portal as the default public network has a refused Portal route
  with the setting on, and no MCP text promises its Portal address.
- Views: the flat view, `get_flat` and `list_flats` hold the endpoint and its detail. `GET /api/status` holds
  `portal_require_auth` and `portal_refused`. The list loses a flat that becomes Private, is deleted, loses its Portal
  grant or gets sign-in, and is empty with the setting off. It does not hold a flat when Portal is not permitted for the
  host or the flat has no live version. With a store that fails, the status still answers with `portal_refused` `null`,
  and `flats status` says that the refused flats cannot be read. `flats status` holds the note of rule 11, and no note
  with the setting off. The console UI test shows the toggle with its hint, the Portal row with the badge and the
  detail on the Networks section, and the settings view with the key.
- No audit row and no webhook event follow a change of the setting.
- The docs: `docs/configuration.md` has the row and the paragraph. The doc checks pass, and the generated references
  match their sources.

## Open questions

None.
