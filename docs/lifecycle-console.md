# Lifecycle console

The console keeps the layout it had before the lifecycle work (commit `0dfc1be`) and adds only what the Draft/Published lifecycle, operator approvals and per-flat networks need. Core owns the store and HTTP behavior; the console calls the routes below.

## Routes the console calls

All paths are under `/console/api`. Publish, activation, rollback, deletion and visibility changes return `pending_approval`. Each is made only after the operator confirms it in a console dialog, and that confirmation is the decision: the console then calls `POST /approvals/{id}/approve`. Nothing is shown as published, rolled back, deleted or made Public or Private until that call returns. Cancel sends no request. Previews and provider permissions take effect immediately.

| Action | Call |
|---|---|
| Save the draft as a new version | `POST /flats/{slug}/versions` `{revision, hash}` of the draft shown, then approve. Allocates `vN` without deploying it. |
| Deploy or redeploy a saved version | `POST /flats/{slug}/deploy` `{version}` (`version > 0`), then approve. Activates `vN` as live. |
| Roll back | `POST /flats/{slug}/rollback` `{version, restore_data}`, then approve. Restoring data is an explicit checkbox. |
| Private or Public | `POST /flats/{slug}/visibility` `{visibility, reason}`, then approve |
| Delete | `DELETE /flats/{slug}` after typing the slug, then approve |
| Draft preview | `POST /flats/{slug}/previews` `{target:"draft", version:0}` |
| Version preview | `POST /flats/{slug}/previews` `{version}` |
| Network permission for a flat | `POST /flats/{slug}/providers` `{provider, permitted}` |
| Host providers | `GET /providers`; `PUT /providers/{id}` `{enabled}` |

Agents save drafts through MCP or the CLI; the console does not upload archives.

## Screen

- **Save and Publish:** the console uses two words for the two approved steps. **Save** turns the draft into a new numbered version and puts nothing live (`Save as vN`, approval action `create_version`). **Publish** puts one saved version live (`Publish vN`, approval action `activate`); publishing an earlier version again is `Roll back to vN`. Save buttons are drawn quieter than Publish buttons.
- **List:** rows show `Live vN`, `Not published`, `vN saved, not published` when the newest saved version was never live (`latest_deployed` false), and `unsaved draft changes`. `Save as vN` appears for flats with draft changes; otherwise `Publish vN` appears when the newest saved version was never live. Rows do not repeat a public-access banner. A row's menu opens the flat's Deployments, Analytics and Settings tabs and the Share dialog.
- **Flat page:** one page per flat with tabs. Deployments (`/flats/{slug}`) shows the addresses and live state, the Versions table, open previews and logs; the Versions table starts with a Draft row (Preview, `Save as vN`) while `draft.dirty` is true. Each saved version shows a `Live`, `Previously live` or `Not published` badge (from its `deployed` field) and offers `Publish vN` for versions above `live_version` and `Roll back to vN` for earlier versions. Analytics adds page views and storage, Database lists data snapshots. Settings (`/flats/{slug}/settings`) holds the name, slug, sharing, networks, environment variables, secrets, server HTTP(S) permissions and delete. Sharing offers Private and Public; Networks lists the per-flat permissions: Local (always on), Tailscale (private, tailnet ACL), Tailscale Funnel and Portal (public). A flat that allows Portal also has a Portal relay listing choice (follow the host setting, hidden or listed). It is described as the listing requested from the relays, which follow at their next lease renewal, and as not access control. Making a flat Public is refused in the console until it has a live version and a permitted public network. The public URL is `public_url`, or the URL of a current Funnel endpoint.
- **Settings:** a Network providers card groups Private (Local, Tailscale) and Public (Tailscale Funnel, Portal). Local is always on; each other provider has Turn on / Turn off, the flats that allow it, its hosts, and its own settings (whether a Tailscale auth key is stored; Portal relays, active relays and discovery, used when Portal starts, and whether public flats are hidden from relay listings by default, sent to served routes at once and followed by relays at their next lease renewal). Turning a provider on or off saves `network.permitted` in config.json, guarded by the settings ETag. On a flat's Settings tab, a provider that is off for the host cannot be newly allowed. A Console domains card adds and deletes the console's tailnet names (`tailscale.console_hosts`) in its Tailnet (Tailscale) section and its NetBird names (`netbird.console_hosts`, plain HTTP) in its NetBird section, and disabled actions show their reason on hover and keyboard focus; and a Keys card sets, replaces and removes the Tailscale auth key and the NetBird setup key and connects the zrok account token, showing each key's state and source but never its value.
- **Approval page:** names publish, activation, rollback and data-restore requests, so requests made by agents can be reviewed and decided there.

Legacy `public-listed` and `public-unlisted` values are shown as Public. Permission for a network does not publish a flat or change its visibility, and Tailscale being connected does not permit Funnel. The server refuses removing a public network while the flat is Public (`provider_in_use`); the console shows that error.

## Decision authority

Console routes check the console header and a same-origin browser request. That is CSRF protection, not authentication: a local process that sends the same headers to the loopback listener can also decide. A separate operator credential was tried and removed pending a better design.
