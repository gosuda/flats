# Lifecycle console

The console separates four questions: what is being edited (Draft), what is published (current vN), who may open that current version (Private or Public), and which provider path is allowed. Connection state is shown on its own. A connection failure does not make a flat Unpublished, and a URL is not treated as proof that the path is ready.

This page is the console binding to the core contract. Core owns the store and HTTP behavior. The console calls the routes below and does not invent `/access`, `/activate`, `/provider`, or `/publish/retry`.

## Routes the console calls

All paths are under `/console/api`. Publish, activation, rollback, deletion and visibility transitions return `pending_approval`; Draft saves, previews, settings and provider permission have their own immediate results. The console then calls the existing approve or reject endpoint. Nothing is marked published, rolled back, or made Public or Private until that approve call returns.

| Action | Call |
|---|---|
| Save draft files | `POST /flats/{slug}/draft` with the archive body and `expected_revision` on every save (`0` for the first save). Optional `message` is metadata on that upload, not the flat content. A save message typed alone is not uploaded. |
| Publish | `POST /flats/{slug}/publish` `{revision, hash}` then approve |
| Serve an existing published version again | `POST /flats/{slug}/deploy` `{version}` then approve. This does not allocate a new number. |
| Roll back | `POST /flats/{slug}/rollback` `{version, restore_data}` then approve. Restoring data is explicit in that approval. |
| Private or Public | `POST /flats/{slug}/visibility` `{visibility, reason}` then approve. Public is refused in the UI until `publication` is `published`. |
| Draft preview | `POST /flats/{slug}/previews` `{target:"draft", version:0}` |
| Published-version preview | `POST /flats/{slug}/previews` `{version}` |
| Provider permission | `POST /flats/{slug}/providers` `{provider, permitted}` |

`local` stays permitted. `tailscale`, `tailscale-funnel`, and `portal` require host and per-flat grants. The explicit legacy upgrade choice can preserve eligible Private Tailscale opt-ins once; it never grants Public providers. The visible name for `tailscale-funnel` is Tailscale Funnel. Serve is not a public option. Allowing Funnel is separate from Tailscale being connected, and neither action publishes or changes Private/Public.

## Fields the console reads

On each flat:

- `publication`: `unpublished` or `published`
- `live_version`: the current published version, `0` when none
- `visibility`: `private` or `public`
- `draft`: `null` or `{revision, hash, base_version, dirty, updated_at, kind, message, size, files, number:0, role:"draft"}`
- `providers`: permitted ids. `local` is treated as permitted even when omitted
- `connection_state`: the server aggregate, using `starting`, `ready`, `error`, `needs-login`, `key-expiring`, `unavailable`, or `stopped`
- `endpoints`: current and preview provider routes with `provider`, `audience`, `host`, `state`, `detail`, `configured`, `permitted`, `ready`, and an optional `url`
- Existing `private_url`, `private_state`, `public_url` for the current version’s address. The current-version link is enabled only when `publication` is `published`, a version exists, and the matching connection state is `ready` or `key-expiring`

List rows and the flat page derive the displayed connection from current endpoints for the flat's actual visibility. Ready routes take priority, followed by connecting and actionable failure states; a stopped route cannot hide another current route that is ready, connecting, failed, unavailable, or unconfigured. The server aggregate is the fallback when no matching endpoint is reported. Flat DTOs do not contain `connection` or `draft_preview`. The flat page reads `/flats/{slug}/previews` to find an open Private Draft preview.

Previews may include `target` and `revision`. The console labels Draft previews and numbered-version previews as Private. An endpoint with `state:"stopped"` is labeled as a stopped current or preview route. A refused route remains unavailable/not permitted/not configured and keeps the server's explanatory `detail`; it is not relabeled as stopped. Approval `params` are shown when they include `revision`, `hash`, `base_version` or `expected_live`, `version`, `from`, `to` / `visibility`, and `providers`. Provider policy values are rendered as provider names; the internal policy fingerprint is not operator-facing confirmation copy.

`data_impact` on an approval or error is shown as reported. `none` means the check did not use live data. A missing impact is not described as “no effect”. The publish check is described as running, after approval, on a copy of the flat data.

A Private approval that leaves `visibility` as `public`, or whose result says the public route was unconfirmed, stays off the Private badge.

## Screen

The flat list links the name to the management page. Open current version and Review draft are separate list controls; the flat page offers Open draft only from actual preview data. Clean Drafts do not offer Publish. Rows do not repeat a public-access banner.

The flat page keeps the current version and the draft side by side (stacked on a narrow screen). Tabs are Overview, Version history, Access, and Operations. Logs, secrets, database, and analytics stay reachable from Operations and from the existing settings, analytics, and database pages.

Publishing, both access directions, rollback, and provider permission each use a dialog that states the candidate or the policy being changed. Cancel sends no request. The dialog returns focus to the control that opened it. Approval results are announced in the assertive status region. Pending, rejected, failed, and stale approvals stay visible; a stale or failed publish is not shown as a new vN.

Private Tailscale copy follows the tailnet ACL, including other people and devices that ACL allows. Local is this device through localhost. Public Funnel and Portal are internet paths. Funnel visitors do not need Tailscale.

## Integration checkpoint

Consecutive archive autosaves use the revision returned by the preceding
successful upload. The editor previously retained its initial revision, causing
the next save to conflict with its own first save. Real conflicts retain the
source archive; an explicit retry first reloads the latest Draft revision and then guards that revision on upload. The console test checks
both source archive bodies and the second request's updated expected revision.

Package and simulated-DOM checks are complemented by actual rendered desktop
and mobile checks against disposable production Local servers and real API DTOs.
Source autosaves, tabs, Access/Operations, operator unlock, separate publish
confirmation, status/audit updates, cancellation focus and logout were verified.
The final frozen binary also rendered persisted actor and data-impact details.

## Operator authority integration

The app provisions one `api.OperatorAuthority` from an explicitly supplied
embedding credential, `--operator-credential-stdin`, or the protected `--operator-credential-file` source documented in README. Interactive input is
hidden; pipe input is bounded. Credentials are not supplied through argv,
environment, HTTP reads or MCP tools. The console unlock dialog creates a
session, then each candidate still requires its own confirmation. The same
instance validates core decisions and provides the nonsecret persisted audit
identity. This separates agent access from operator decision authority; it does
not authenticate an individual person or defend against access to operator
secrets, browser memory or the server OS.

Console status prefers exact core `connection_state` and observed current
endpoints. Access shows backend configuration, host/flat permission and route
readiness separately. Approval pages show actual persisted `result_data` and
`decided_by`. Tabs support arrow keys, Home and End with focus transfer; the
operator toolbar wraps on mobile. New data restores include DB and FILES;
historical DB-only snapshots preserve current FILES.

## Approval disclosure and correction evidence

Canonical `public` uses the globe/Public badge, internet warning, and danger confirmation. Pending rollback with `restore_data:true`, and `restore_data` actions, show the frozen snapshot name/hash and explain that current live data is backed up before replacing DB and captured FILES (historical DB-only snapshots preserve FILES). Activation and publication of server candidates disclose live-data writes at runtime start, separately from isolated health checks; unknown candidate kind uses conditional server copy. Rollback in Version history offers an explicit restore checkbox and a second danger confirmation with the server-frozen snapshot before decision. The API does not expose arbitrary snapshot selection/restore as a console route.

The console maps decision `category` or `approval.result_data.failure_code` for provider permission/readiness/availability, unconfirmed public stop, unchanged content and stale approval. HTTP 409 alone never means drift. Failed Private transitions retain Public until confirmed. Simulated DOM tests use real server-shaped 409 envelopes and exercise pending provider-missing, rejection, disclosure, retry and first-save flows. Rendered correction evidence is external to the repository and binds the exact corrected source/binary in the handoff; combined-head integration must rerun it.

The toolbar reads console-only `GET /console/api/operator/session` on reload, refocus, return to visibility, known expiry and operator-required responses. It reports configured/authorized state and schedules resynchronization at the server-provided expiry; a failed read keeps status unverified. This endpoint returns no credential or cookie and grants no authority. The credential is never stored by this UI.

In-use permission removal is refused with `provider_in_use`; permission remains intact. A currently Public flat must first complete an approved change to Private before removing a Public provider. Removing Tailscale permission confirms teardown of this flat’s Private Tailscale current, preview and redirect routes before persisting revocation. Local stays available, and any separately permitted Public Funnel route stays up. An unconfirmed stop preserves permission for retry. Current links use the selected visibility-matching endpoint: Public Connecting is an approved visibility policy, not a reachable URL. Private URLs come from actually registered permitted Tailscale or Local routes, rather than an unserved ts.net name.
