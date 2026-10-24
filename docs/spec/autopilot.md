# Autopilot

Status: implemented. The design is in the architecture docs:
[Autopilot](../design.md#autopilot),
[Experiments routes](../surfaces.md#experiments-routes),
[Autopilot tools](../surfaces.md#autopilot-tools) and
[Autopilot settings](../configuration.md#autopilot-settings).

## Summary

Today an agent can create flats, save Drafts and edit documents, but every
operation that publishes, exposes or deletes waits for the operator: the
agent's request returns an approval link, and only a person in the console
can approve it. That keeps people in control, but it stops agents that work
without a person at hand. A typical case is a bot that opens pull requests
and wants to attach a design mock-up or a document as a public link: it can
make the flat, but it cannot make it reachable.

Autopilot is an opt-in mode in which the operator lets agents decide these
approvals themselves, one kind of operation at a time. It changes nothing
about how operations are requested. Agents still request them as today and
still receive an approval; under Autopilot they also get tools to approve or
reject pending approvals, limited to the kinds of operation the operator
allowed. Everything an agent decides is recorded as the agent's decision.

Autopilot is the first entry of a new Settings tab, **Experiments**, for
features whose behavior and names may still change. The tab is changed only
in the console.

## Principles

1. **Explicit opt-in, off by default.** A new or upgraded host has
   Autopilot off and every operation allowed under it off. Only the operator
   turns them on, in the console.
2. **Nothing changes while it is off.** With Autopilot off, agents see
   exactly today's MCP: the same tools, instructions, guide and results.
3. **Additive, not a different path.** Autopilot does not skip approvals or
   apply operations directly. Every operation is still requested, frozen,
   checked and applied by the existing approval flow; Autopilot only adds a
   second decider. Rules that make an approval stale or fail apply to an
   agent's decision exactly as to the operator's.
4. **Allowed per operation.** The operator chooses each kind of operation
   agents may decide. There is no other axis: the choice applies to every
   flat, every content type and every network agents may use.
5. **Flats grants no new reach.** Autopilot decides approvals; it does not
   grant public providers, keys or secrets. A flat made Public by an agent is
   reachable only through the public providers the operator already
   permitted for that flat.
6. **Recorded, not hidden.** Every decision an agent makes is marked as an
   agent decision, with what Flats knows about where it came from. The
   content itself is not marked.
7. **The operator guides the agents.** When agents should approve, how they
   name flats and when they clean up is policy, not Flats behavior. The
   operator writes that policy as an operation guide, which Flats hands to
   agents; Flats does not enforce it.

## Terms

- **Approval**: an operation an agent, the CLI or the API requested that
  waits for a decision (see [surfaces](../surfaces.md) and guide
  `topic.approvals`).
- **Decide**: approve or reject a pending approval.
- **Agent decision**: a decision made through the Autopilot tools of rule 14.

## Behavior

### The Experiments tab

1. Settings gets a fifth section, **Experiments**, after Agents. It lists
   experimental features; at first it has only Autopilot.
2. The tab says at its top that the features may change or be removed, and
   that only people change this tab: agents must never change it, including
   through a browser or by calling the console. This is an instruction, not
   a guard. The console has no login: its requests are checked only to keep
   other websites out (same-origin checks), and a program that can reach the
   console's address can send the same requests as the page. Flats does not
   tell a person from an agent here.
3. Experiments settings are changed only through the console. They have no
   key in `config.json`, no environment variable or `flats serve` flag, no
   CLI command and no MCP tool, and the host's API refuses changes from
   anything that is not a console request (same-origin console requests, as
   for the console's other settings). They are stored in `flats.db`. There
   is no emergency switch outside the console: to stop Autopilot without the
   console, stop Flats.
4. A change takes effect at once, without a restart. Each change is
   written to the host log: what changed, when, and the tailnet login of the
   person who made it when the console knows it. The log records that the
   operation guide changed, not its text.

### Autopilot settings

5. **Autopilot** on or off. Off by default.
6. **Operations agents may decide**, each its own switch, all off by
   default:

   | Operation | Requests | Stored approval action |
   | --- | --- | --- |
   | Create versions | `create_version` | `create_version`, and legacy `publish` |
   | Delete versions | `delete_version` | `delete_version` |
   | Deploy | `deploy` of a saved version (activation or redeploy) | `activate` |
   | Roll back | `rollback` without `restore_data` | `rollback` with `restore_data` false |
   | Roll back with data restore | `rollback` with `restore_data` | `rollback` with `restore_data` true, and `restore_data` |
   | Make Public | `set_visibility` Private to Public | `set_visibility`, see below |
   | Make Private | `set_visibility` Public to Private | `set_visibility`, see below |
   | Public editing | a change of a Public flat's access level, `view` to `edit` or `edit` to `view` | `set_visibility`, see below |
   | Delete flats | `delete_flat` | `delete` |

   The `restore_data` action, which restores the live version's data without
   changing the version, has no request surface today; an approval stored
   with it follows Roll back with data restore.

   The operations an approval needs come from what the approval froze when
   it was requested, not from the flat's current state. For `set_visibility`
   that is the frozen starting visibility and access level and the requested
   ones; a requested Public level is `edit` only when the approval froze an
   access level with its access sequence, as when it is applied, and `view`
   otherwise. Then:
   - Private to Public needs Make Public, and also Public editing when the
     level is `edit`;
   - Public to Private needs Make Private, whatever the level was;
   - Public to Public with a different level needs Public editing.

   Every other action needs only its row's switch: a `delete` approval, for
   example, freezes just the slug and needs Delete flats. An approval whose
   action is not in this table, whose frozen parameters cannot be read, or a
   `set_visibility` approval without its frozen starting visibility (as
   approvals from older Flats versions lack), needs an operation no switch
   allows: agents can neither approve nor reject it. Whether the frozen
   state still matches the flat is checked later, when the approval is
   applied, exactly as today.
7. **Operation guide**: free text for agents, at most 4,000 characters,
   empty by default. Agents receive it as described in rules 22 and 23.
8. With Autopilot off, the operation switches and the guide keep their
   values but have no effect.

### The Experiments screen

9. While Autopilot is off, the tab shows Autopilot with a Turn on button,
   and hides the operation switches and the operation guide.
10. Turn on asks for confirmation first. The dialog lists the operations
    that are allowed now (their values kept from before, or none), says that
    anyone on the internet can open a Public flat, and that agents follow
    the operation guide but Flats does not enforce it. Turn off acts at once,
    without a dialog.
11. While Autopilot is on, the tab shows a Turn off button, the operation
    switches and the operation guide.
12. Each operation switch takes effect as soon as it changes, without a
    save step, and the console confirms it. If the change fails, the switch
    returns to its stored value and the console says why.
13. The operation guide has its own Save button, enabled while the text
    differs from the stored guide. It shows how many of the 4,000
    characters are used. As in the console's other forms, unsaved text is
    not kept when the operator leaves the page.

### Agent tools

14. With Autopilot on, MCP has two more tools: `approve_request` and
    `reject_request`. With Autopilot off, they are not in the tool list.
15. Flats' MCP keeps no sessions: each request is served on its own. A
    change of Autopilot applies to the very next request, but an agent sees
    the changed tool list only when it lists tools again, which most clients
    do when they connect. An agent that still calls `approve_request` after
    Autopilot was turned off is refused (rule 17).
16. Both tools take the approval `id` and `agent`, a required name the
    agent gives for itself (for example the bot's name). Flats records the
    name as given; it is not verified.
17. A call is refused, and nothing changes, when:
    - Autopilot is off;
    - the approval does not exist;
    - an operation the approval needs is not allowed (rule 6). The refusal
      names the missing operations. This holds for `reject_request` as well:
      agents reject only what they could approve, and the operator keeps the
      other approvals;
    - `agent` is empty or longer than 100 characters.

    Autopilot and the switches are checked at the moment the decision takes
    effect, together with claiming the approval: a switch turned off before
    that moment refuses the call. A switch turned off afterwards neither
    stops nor undoes the decision.
18. Within the allowed operations, agents may decide every pending
    approval, whoever requested it (MCP,
    CLI or API) and whether it was requested before or after Autopilot was
    turned on. Flats has no identity for agents, so it does not tell one
    agent's requests from another's.
19. Every network the MCP endpoint is served on may use the tools: the
    console listener on whatever address the operator bound it to,
    Tailscale and NetBird console domains, and the operator's configured
    external origins.
20. Deciding through the tools then behaves like deciding in the console:
    the approval is claimed, the frozen state is checked, the operation is
    applied, and the result is what `get_approval` returns today. Of several
    decisions of one approval, by the operator or agents, only the first
    changes it. A later decision in the same direction (approving an
    approved one, rejecting a rejected one) answers with the approval as it
    is, without a conflict and without changing its record, so an agent may
    retry. A later decision in the other direction, or any decision while
    the approval is being applied, gets a conflict. These are the console's
    rules today.
21. The operator can still approve or reject in the console as before.
    Pending approvals keep showing in the console until someone decides
    them.

### What agents are told

22. With Autopilot on, the server instructions an agent receives when it
    connects say that Autopilot is on and point to guide topic
    `topic.autopilot`. Wherever Flats tells agents that MCP has no approval
    tool, that they never approve their own requests, or that they ask the
    user before visibility changes, deletion or data restore, the text says
    instead what Autopilot allows: in the instructions, in the descriptions
    of the request tools (`set_visibility`, `rollback`, `delete_flat` and the
    others), and in every guide topic that says it (`topic.approvals`,
    `topic.index`, `topic.rollback-data` and any other). Whether to ask a
    person first is left to the operation guide and the agent's own policy.
    The fixed reference documents (the runtime reference, the content types
    reference and the `llms.txt` documents) keep their text;
    `topic.autopilot` says that its rules take precedence over them.
23. With Autopilot on:
    - guide `topic.autopilot` holds the rules of this mode, the operations
      currently allowed and the operation guide in full;
    - `check_client` reports that Autopilot is on, the operations allowed
      and that the agent should read `topic.autopilot`;
    - `get_approval` on a pending approval that agents may decide says
      that the operator or the agent can decide it, with `approve_request`
      or `reject_request`. For any other pending approval its text is
      today's.
24. With Autopilot off, `topic.autopilot` does not exist and none of the
    texts above change (principle 2).
25. The console's Agents page describes what agents can do, and its text
    stays true under Autopilot:
    - its introduction says that requests wait for the operator, except,
      while Autopilot is on, those of the operations agents may decide;
    - while Autopilot is on, the title shows "Autopilot on" and a "What
      agents can decide" group lists the allowed operations with a link to
      the Experiments tab;
    - the list of what the agent tools cannot do no longer says that no
      agent tool decides an approval while Autopilot is on, and it always
      says that no agent tool changes Experiments.

### Records

26. An agent decision is recorded in the approval's `decided_by` as an
    Autopilot decision with:
    - the agent name of rule 16;
    - the network the request arrived on: Local, for a connection from
      the host itself to the console listener; the console listener with
      the caller's IP address, for a connection from another machine when
      the operator bound the listener to a non-loopback address; Tailscale;
      NetBird; or the external origin;
    - over Tailscale, the tailnet login or node name of the calling device,
      as the console records an operator's tailnet login today;
    - over NetBird, the calling peer's name, as the console records an
      operator's NetBird peer today.
27. The approval page in the console shows who decided an approval
    ("Decided by"): the operator's tailnet login or "the console" as
    recorded today, or Autopilot with the details of rule 26. The approvals
    list does not change.
28. The flat's event log says the operation was approved or rejected by
    Autopilot, with the same details, where it says "by the operator" for a
    console decision. The events of the operation it applies say "via
    autopilot" where they say "via console" for a console decision. Deleting a flat deletes its events, as today, so for
    an approved deletion the approval's record of rule 26 is the record:
    approvals are kept after their flat is deleted.
29. Content is not marked: a version, revision or visibility set by an agent
    looks like any other.
30. Turning Autopilot off, or an operation off, changes no flat, version,
    visibility or past record.

### What does not change

31. Requests: every tool that requests an approval keeps its arguments and
    results, including `approval_url`.
32. Operations that need no approval today (creating flats, saving Drafts,
    saving and editing documents) are not affected.
33. Public providers, provider grants, default networks, keys, secrets,
    quotas and limits keep working as today. Autopilot adds no limit of its
    own on how many flats or decisions agents make.

## Open questions

- None yet.
