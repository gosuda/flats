# Lifecycle console

The console separates four questions: what is being edited (Draft), what is published (current vN), who may open that current version (Private or Public), and which provider path is allowed. Connection state is shown on its own. A connection failure does not make a flat Unpublished, and a URL is not treated as proof that the path is ready.

This page is the console binding to the core contract. Core owns the store and HTTP behavior. The console calls the routes below and does not invent `/access`, `/activate`, `/provider`, or `/publish/retry`.

## Routes the console calls

All paths are under `/console/api`. Mutations other than provider permission return `pending_approval`. The console then calls the existing approve or reject endpoint. Nothing is marked published, rolled back, or made Public or Private until that approve call returns.

| Action | Call |
|---|---|
| Save draft files | `POST /flats/{slug}/draft` with the archive body and `expected_revision` when a draft already exists. Optional `message` is metadata on that upload, not the flat content. A save message typed alone is not uploaded. |
| Publish | `POST /flats/{slug}/publish` `{revision, hash}` then approve |
| Serve an existing published version again | `POST /flats/{slug}/deploy` `{version}` then approve. This does not allocate a new number. |
| Roll back | `POST /flats/{slug}/rollback` `{version, restore_data}` then approve. Restoring data is explicit in that approval. |
| Private or Public | `POST /flats/{slug}/visibility` `{visibility, reason}` then approve. Public is refused in the UI until `publication` is `published`. |
| Draft preview | `POST /flats/{slug}/previews` `{target:"draft", version:0}` |
| Published-version preview | `POST /flats/{slug}/previews` `{version}` |
| Provider permission | `POST /flats/{slug}/providers` `{provider, permitted}` |

`local` stays permitted. `tailscale`, `tailscale-funnel`, and `portal` start unpermitted until the operator allows them. The visible name for `tailscale-funnel` is Tailscale Funnel. Serve is not a public option. Allowing Funnel is separate from Tailscale being connected, and neither action publishes or changes Private/Public.

## Fields the console reads

On each flat:

- `publication`: `unpublished` or `published`
- `live_version`: the current published version, `0` when none
- `visibility`: `private` or `public`
- `draft`: `null` or `{revision, hash, base_version, dirty, updated_at, kind, message, size, files, number:0, role:"draft"}`
- `providers`: permitted ids. `local` is treated as permitted even when omitted
- `connection`: a state string or `{state, detail}` using `starting`, `ready`, `error`, `needs-login`, `key-expiring`, `unavailable`
- Existing `private_url`, `private_state`, `public_url` for the current version’s address. The current-version link is enabled only when `publication` is `published`, a version exists, and the matching connection state is `ready` or `key-expiring`

List rows use those fields. An optional `draft_preview` object with `url`, `state`, `target:"draft"` supplies the list’s Open draft link. Without it, the list does not invent a draft URL.

Previews may include `target` and `revision`. A draft preview is labeled private. Approval `params` are shown when they include `revision`, `hash`, `base_version` or `expected_live`, `version`, `from`, `to` / `visibility`, and `providers`.

`data_impact` on an approval or error is shown as reported. `none` means the check did not use live data. A missing impact is not described as “no effect”. The publish check is described as running, after approval, on a copy of the flat data.

A Private approval that leaves `visibility` as `public`, or whose result says the public route was unconfirmed, stays off the Private badge.

## Screen

The flat list links the name to the management page. Open current version and Open draft are separate controls, each named with its target. Rows do not repeat a public-access banner.

The flat page keeps the current version and the draft side by side (stacked on a narrow screen). Tabs are Overview, Version history, Access, and Operations. Logs, secrets, database, and analytics stay reachable from Operations and from the existing settings, analytics, and database pages.

Publishing, both access directions, rollback, and provider permission each use a dialog that states the candidate or the policy being changed. Cancel sends no request. The dialog returns focus to the control that opened it. Approval results are announced in the assertive status region. Pending, rejected, failed, and stale approvals stay visible; a stale or failed publish is not shown as a new vN.

Private Tailscale copy follows the tailnet ACL, including other people and devices that ACL allows. Local is this device through localhost. Public Funnel and Portal are internet paths. Funnel visitors do not need Tailscale.
