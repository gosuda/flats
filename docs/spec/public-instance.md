# Public instance mode

Status: planned; see [Current state](#current-state).

## Summary

Today every new flat starts Private. Its first address is a Private URL on
Local, the tailnet, NetBird or the operator's reverse proxy, and it reaches the
internet only after the operator approves a change to Public. That fits a host
that one person runs for themselves. It does not fit a host that exists to
publish, such as a shared server for design mock-ups and notes that anyone with
the link should read. There, every new flat needs the same approval, and the
Private URL an agent hands out opens for nobody outside the operator's own
network.

Public instance mode is an opt-in mode for such hosts. While it is on:

- every new flat, of every content type, is created Public on the host's
  default public network, without a visibility approval;
- private links are disabled: no flat, preview or redirect answers on a
  private link, and Flats hands out no private link.

It is the second entry of Settings › Experiments, after
[Autopilot](autopilot.md), and follows the same rules for experiments: off by
default, changed only in the console, and its behavior and name may still
change.

## Principles

1. **Explicit opt-in, off by default.** A new or upgraded host has the mode
   off. Only the operator turns it on, in the console.
2. **Nothing changes while it is off.** With the mode off, flats, routes,
   addresses, MCP, the CLI, the API and the console behave exactly as today.
3. **Creation is the only new consent.** Turning the mode on is the
   operator's standing consent to create flats Public. It consents to
   nothing else: creating and deploying versions, deleting, rollback, data
   restore, the "anyone can edit" level and every later visibility change
   still need their approvals, as today.
4. **Flats grants no new reach.** The mode uses the public network the
   operator already chose as the default for new flats. It turns on no
   provider and grants no other provider.
5. **Disabled, not removed.** The mode stops private links from answering;
   it does not delete routes, grants, previews or redirects. Turning it off
   brings every one of them back as it was, and changes no flat's
   visibility.
6. **Agents are told the truth.** While the mode is on, nothing Flats tells
   an agent says that a new flat is Private or points to a private link.

## Terms

- **Private link**: an address of a flat, a Draft or version preview, or a
  rename redirect on Local (`*.localhost`), Tailscale, NetBird or the Private
  reverse proxy (`proxy`). This is every route of the Private group in
  [Private and public access](../networking.md#providers), Local included.
- **Public link**: an address on a public provider (Tailscale Funnel,
  Portal, zrok, a Cloudflare quick tunnel, NetBird Expose or `proxy-public`).
- **Default public network**: `network.default_public`, the public provider
  a new flat is allowed on when it is created
  ([configuration](../configuration.md)).

Console addresses, the MCP endpoint and approval links are not flat links.
The mode does not change them.

## Behavior

### Turning the mode on and off

1. Settings › Experiments gets a second section, **Public instance**, after
   Autopilot. It holds one row, Public instance on or off, with a Turn on or
   Turn off button. Autopilot rules 2 and 3 apply to it: only people change
   it, and only through the console. It has no key in `config.json`, no
   environment variable, `flats serve` flag, CLI command or MCP tool, and the
   host refuses changes that are not console requests. It is stored in
   `flats.db`.
2. Turn on is refused while the host has no default public network, or its
   default public network is not turned on for the host. The console then
   disables Turn on, says why and links to Settings › Networks.
3. Turn on asks for confirmation first. The dialog names the default public
   network and says that:
   - every new flat, including flats agents create, will be Public on that
     network, and anyone on the internet can open it: a document, design or
     board at once, a website or page once a version of it is created and
     deployed through their approvals;
   - private links stop working, on the Flats machine too, including Draft
     and version previews, so people can no longer preview a Draft, edit a
     design or open the history of a document, design or board in a
     browser; agents keep these through MCP;
   - how many flats are Private now: they have no working link until they
     are made Public or the mode is turned off.
4. Turn off acts at once, without a dialog.
5. A change takes effect at once, without a restart, and is written to the
   host log with when it changed and who changed it, as Autopilot changes
   are (Autopilot rule 4).
6. Turning the mode on or off changes no flat's visibility, public access
   level, provider grants, versions, revisions, previews, redirects or
   pending approvals.
7. **Ordering.** When Turn on or Turn off returns, every request a private
   link receives from then on is answered according to the new state, and
   every creation that starts from then on follows it. A creation that was
   already running when the mode changed may finish under the state it
   read. Turning the mode on also ends every private-link request and
   connection that is still open, including live editing connections of
   documents and boards.

### New flats

8. While the mode is on, a new flat is created Public with the public access
   level "view only", on every creation path: `create_flat` of any type, the
   first save of `save_draft`, `save_draft_from_dir`, `save_page`,
   `save_document`, `save_design`, `update_board` and `import_board`, and
   the API and CLI paths that reach them.
9. It is allowed on Local and the default public network. It gets no private
   network grant, whatever the default private network is.
10. Creation reads the mode and the default public network once, together,
    and uses what it read for the visibility and the grants. Creation needs
    no visibility approval. The flat's event log records that it was created
    Public by public instance mode and names the network.
11. While the mode is on but the default public network is missing or not
    turned on for the host (Settings › Networks changed after the mode was
    turned on), creation is refused with a conflict that names the cause,
    and nothing is created. Flats never falls back to creating a Private
    flat or to another provider. The Experiments page warns about this
    state.
12. **Documents, designs and boards** run at creation, so their public link
    opens at once. A public link that fails to open does not undo the
    creation: the flat stays Public, its endpoint and `connection_state`
    report the provider's state or error, and an exposure error is recorded
    in its event log, exactly as for a Public flat whose route fails at a
    restart today. The creation call succeeds, as the save of a running
    document whose route fails does today: the API answers with its usual
    success status, the MCP result is not an error and the CLI exits 0. The
    result shows the flat as Public with the provider's state, and its text
    says that the public link is not open yet and why. The same recoveries
    apply: granting the provider again, a restart, or a key change for that
    provider.
13. **Websites and pages** have no live content at creation. They are
    created Public and Unpublished (`visibility` `public`, `publication`
    `unpublished`, `live_version` 0). Until their first deployment they have
    no public link: `public_url` is empty and `connection_state` is
    `unavailable`. Creating and deploying a version keep their approvals.
    When the first deployment is applied, the public link opens on the
    flat's public providers, with rule 12's failure behavior. A restart
    keeps both states.
14. The "anyone can edit" level of documents and boards still needs its own
    approval.

### Private links

15. While the mode is on, every private link answers HTTP 403 with a short
    page saying that private links are turned off on this host, and serves
    no content: flats, previews and rename redirects, on Local, Tailscale,
    NetBird and the Private reverse proxy alike. One exception: a
    Tailscale node's plain-HTTP port redirects to the same private host over
    HTTPS before Flats sees the request; that redirect carries no content,
    and the HTTPS address answers 403. Flats' own operations that do not go
    through a link, such as agent edits, revision history, health checks and
    data snapshots, keep working.
16. Routes, grants, previews and redirects are kept while the mode is on:
    an existing Tailscale or NetBird node of a flat stays up and answers as
    in rule 15. Turning the mode off makes them serve again at once. A
    preview that expires in the meantime closes as usual.
17. No surface hands out a private link while the mode is on:
    - flat and preview results of MCP, the API and the CLI report no
      `private_url` and list no private endpoints;
    - the console shows no Private URL, private open button, private
      address line or Local address, and offers no action that only opens a
      private link, such as Open History; its share links use the public
      link.

    Event log entries written before the mode was turned on are records and
    keep their text, even when it names a private link; that link answers
    403.
18. `open_preview` is refused while the mode is on, with a conflict saying
    that previews are private links and public instance mode is on.
19. A Private flat has no working link while the mode is on, whether it was
    Private before or was made Private through an approval. Its
    `connection_state` is `unavailable`, and the console says so on the
    flat. A Public flat's `connection_state` comes only from its public
    links while the mode is on.
20. Making a flat Private stays possible, through its approval, so the
    operator can take a flat off the internet without deleting it.
21. Console texts that promise private access say instead what the mode
    allows, and return to their usual text when the mode is off:
    - the Make Private confirmation and the approval page of a change to
      Private say that the flat will have no working link while public
      instance mode is on, instead of "the private address keeps working";
    - the Share dialog's "People on your networks" row says that private
      links are turned off, instead of "Can open" or "Can edit";
    - a flat's Private URL line is replaced by a note that private links are
      turned off.

### What agents are told

22. While the mode is on, the server instructions, the descriptions of the
    creation, preview and visibility tools, their result texts and the
    guide topics that describe new flats, Private URLs or previews say that
    new flats start Public on the default public network and that private
    links and previews are disabled. A result that names a flat's address
    names its public link and repeats `public_notice`, as every public link
    result does today. Guide topic `topic.public-instance` holds the rules
    of this mode and names the default public network.
23. `check_client` reports that public instance mode is on.
24. While the mode is off, none of these texts change (principle 2). The
    fixed reference documents (the runtime reference, the content types
    reference and the `llms.txt` documents) keep their text;
    `topic.public-instance` says that its rules take precedence.
25. Autopilot and the mode are independent. With both on, agents get both
    sets of texts, and an agent still requests every approval other than
    creation.

### What does not change

26. The approval flow, approval tools and the console's approval pages,
    apart from the text of rule 21.
27. Public is not access control: every public link result keeps its
    notice.
28. Provider settings, keys, limits and quotas, and the console's Networks
    settings, apart from the help text that says new flats always start
    Private.

## Changes to existing specs

- [Content data models](content-models.md) rule 7 ("an Unpublished flat
  cannot become Public"): while public instance mode is on, a website or page
  is created Public and Unpublished (rule 13). It still cannot be made Public
  through an approval while Unpublished.
- [Private and public access](../networking.md), the
  [configuration](../configuration.md) page, guide `topic.approvals` and the
  console's Networks help say that a new flat is always created Private. They
  gain the exception for this mode.

## Current state

Nothing of this spec is implemented. Today:

- every creation path writes a Private flat and grants the default private
  and public networks;
- private links serve whenever their routes are registered; Local always
  is;
- Public is reached only through an approved `set_visibility`, which
  requires live content;
- `private_url`, private endpoints and previews are always reported.

## Completion criteria

- With the mode off, the existing tests of creation, exposure, MCP texts and
  the console pass unchanged, and the MCP server's instructions, tools and
  guide are byte-identical to a build without the feature.
- On a disposable host with a real public provider, with the mode on:
  a new document, design and board answer on their public HTTPS link at
  once; a new website answers there only after its version is created and
  deployed; every private link of new and existing flats, previews and
  redirects answers 403; no MCP, CLI, API or console surface shows a private
  link, apart from event entries written before the mode was turned on; `open_preview` is refused.
- Turning the mode off on that host makes the existing private links serve
  again, and the flats created Public stay Public.
- Turn on is refused without a usable default public network, and creation
  is refused when that network was turned off afterwards.
- A board editing connection on a private link is closed when the mode is
  turned on.
- A public link that fails at creation returns a successful creation result
  (API success status, non-error MCP result, CLI exit 0) whose flat is
  Public with the provider error reported and whose content is saved;
  granting the provider again opens it.
- With the mode on, the Make Private confirmation, the Share dialog and the
  flat's address lines show the texts of rule 21; with it off, today's.

## Not in scope

- Turning existing Private flats Public in bulk.
- A way for people to edit designs, preview Drafts or open history in a
  browser without a private link.
- Redirecting a private link to the flat's public link.
- A config key, flag or CLI command for the mode.

## Open questions

- None. The scope questions put to the requester were not answered in time;
  this spec chose the literal reading of the request (every private link,
  Local included, is disabled) and kept every approval other than creation.
