## Public instance mode

Public instance mode is on. The default public network is {{network}}.
These rules take precedence over the fixed runtime and content types
references and the `llms.txt` documents while this mode is on. Read
`topic.approvals` for approvals and `topic.autopilot` if Autopilot is also on.

### Creation and publication

* Every new flat starts Public, with public access set to "view only", on
  the default public network. This includes `create_flat` of every type and
  the first save through `save_draft`, `save_draft_from_dir`, `save_page`,
  `save_document`, `save_design`, `update_board` and `import_board`.
  Creation needs no visibility approval and grants no private network.
* Documents, designs and boards run at once. Everyone on the internet can
  see their content and subsequent saves as soon as their public link is
  ready. Repeat `public_notice` whenever you give a public URL.
* Each save of a Public website or page created by this mode publishes the
  saved Draft as the next numbered version and deploys it without approval.
  This includes `save_draft`, `save_draft_from_dir` and `save_page`, and does
  not depend on Autopilot. The flat keeps this creation mark after restart,
  rename and mode changes. The mode must still be on and the flat must still
  be Public at the save. Older flats, marked flats made Private and saves
  while the mode is off only change the Draft and keep version/deploy approvals.
* Until the first published save, websites and pages are Public and
  Unpublished, with `live_version: 0`, an empty `public_url` and
  `connection_state: unavailable`. The starter Draft from
  `create_flat {type: "page"}` is not published.
* A published save uses the same health check, data snapshot and runtime
  start as an approved deployment. A full version quota, version disk quota, failed
  health check, snapshot or runtime start fails publication but keeps Draft
  revision N saved, without a new version or a changed live version. Report
  the cause and the saved revision. A failed server start can have changed
  live data. Explicit `create_version` and `deploy` still request approvals.
* Inspect files before saving: a successful save can be public immediately.
  Check the returned version and public endpoint before sharing the link,
  and repeat `public_notice`. Saves of one flat publish in their saved order.
* If the default public network is missing or not turned on for the host,
  creation is refused with a conflict naming the cause and creates nothing.
  Never fall back to a Private flat or another provider. Ask the operator
  to fix Settings > Networks.
* A public route that fails to open does not undo a successful creation or
  save. The content stays saved and the flat stays Public. Report that the
  public link is not open yet, with `connection_state` and its detail, and
  read the endpoints and event log. Do not invent an address or switch
  providers. Provider permission, restart or key repair may restore the link.

### Private links and previews

* All private links are disabled, including Local loopback on the Flats
  host, Tailscale, NetBird and the Private reverse proxy. Flats, previews
  and rename redirects serve no content there and answer HTTP 403. A
  Tailscale plain-HTTP redirect carries no content; its HTTPS target is
  also blocked. Never offer a Private or Local link.
* `open_preview` is refused with `public_instance_on`: previews are private
  links. There is no public Draft preview. Inspect files before saving and
  read save warnings afterward. Browser verification uses the deployed
  public link. Do not claim a Draft render check you could not do.
* Browser editing of designs and browser history of documents, designs and
  boards require private links and are unavailable. MCP edits, revision
  reads and restores still work. Documents and boards remain view only to
  public visitors unless their "anyone can edit" approval is applied.
* Existing Private flats stay Private and have no working link, with
  `connection_state: unavailable`. Make Private remains possible through
  `set_visibility` and its approval to take a flat off the internet, but
  does not leave a working private address while this mode is on.

### What stays unchanged

Only the operator changes this mode, in Settings > Experiments. Turning it
on or off changes no existing flat's visibility, public access, grants,
versions, revisions or pending approvals. Turning it off restores private
links that still exist, not previews that expired in the meantime; flats
created Public stay Public. Historical event entries keep their text, but
an old private link does not work while the mode is on.

Only initial Public creation and publication on save of marked Public
websites and pages bypass approvals. Later visibility changes, "anyone can
edit", explicit version creation and deployment, version deletion, rollback,
data restore and deletion keep approvals. Autopilot is independent:
if enabled it decides only the approvals its operation guide allows.
Public is NOT access control. Anyone on the internet can open a ready
Public route. Console, MCP and approval addresses are unchanged.

## public_instance_on

Previews are private links and public instance mode is on. `open_preview`
is refused without opening a preview. There is no public Draft preview.
Read `topic.public-instance` and `topic.preview-verify`. Inspect files before
saving, then check warnings. Saving Public websites and pages
created by the mode publishes them; older flats still need approved
`create_version` and `deploy`. Verify the deployed public URL only
when `get_flat` reports it ready, and repeat its `public_notice`. Only the
operator can turn the mode off in Settings > Experiments; do not call
console routes or try another private provider to bypass it.
