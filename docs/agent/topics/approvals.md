## Approvals, visibility and exposure

Agents never decide publication, exposure or deletion. These requests return
`status: pending_approval` with `approval_id` and `approval_url`, and change
nothing until the operator approves them in the Flats console:

* `publish`, and `save_draft`/`save_version` with `deploy: true`;
* `deploy {version}` (activation or redeploy of a published version);
* `rollback`, with or without `restore_data`;
* `set_visibility` in **both** directions, private to public and public to
  private;
* `delete_flat`, which permanently removes the flat, its versions and data.

Rules:

* Give the user `approval_url` exactly as returned. Poll
  `get_approval {id}` until `approved`, `rejected` or `failed`; while it is
  `pending` (or `applying`), say you are waiting for operator approval.
* Never approve your own request, call console routes or send console
  headers. MCP has no approval or provider-grant tool. Console routes have
  same-origin CSRF checks but no separate authentication, and a local process
  can send those headers: run only trusted agents on the host. Client
  auto-approval of MCP calls does not bypass Flats approvals.
* Ask the user before requesting `set_visibility`, `delete_flat` or a rollback
  with `restore_data`; put their reason in `reason`.
* An approval freezes what it approves (Draft revision and hash, version,
  snapshot, visibility, providers). If that changes before the decision, it
  fails as `stale_approval`; request again. An identical pending request is
  reused rather than duplicated.
* After a decision, `get_approval` `result_data` reports `failure_code` (a
  `refusal.<category>` name, or `apply_failed`), `data_impact` (`none`,
  `runtime_start`, `restore_data` or `unknown`), `health_data` and
  `live_data`. Report them as given.

### Visibility

* Visibility is only `private` or `public`. New flats are Unpublished and
  Private.
* **Private** means Local loopback on the host (plain HTTP at
  `<slug>.localhost:<port>`) or Tailscale for people and devices the existing
  tailnet ACL allows. It is not owner-only access.
* **Public** means anyone on the internet, through a provider the operator
  permitted for this flat: Portal relay or Tailscale Funnel (Funnel visitors
  do not need Tailscale; Tailscale Serve is tailnet-only and is not Public).
  Public is NOT access control: anyone can open a ready Public route.
* An Unpublished flat cannot become Public. Requesting the current
  visibility changes nothing.
* Going Private also waits for approval. Until the approved change applies,
  the public route stays reachable.
* Local is always permitted. Nonlocal providers (`tailscale`,
  `tailscale-funnel`, `portal`) need host configuration and per-flat
  operator permission; agents cannot grant either. Connecting Tailscale
  grants no Funnel permission, and configuring a provider grants no publish or
  visibility consent. A provider failure never authorizes switching providers.
* Never infer visibility, publication or readiness from a URL or domain.
  `get_flat` reports `publication`, `live_version`, `visibility`, `providers`,
  `connection_state` and `endpoints` as separate fields. Repeat
  `public_notice` whenever you report a public URL.
