## Autopilot

Autopilot is on: the operator lets agents decide pending approvals of the
operations listed below. Operations are still requested as before
(`topic.approvals`), and every request still returns `approval_url`.
Autopilot adds two tools:

* `approve_request {id, agent}` approves a pending approval. Flats checks
  its frozen state and applies it exactly as when the operator approves it
  in the console, and returns what `get_approval` returns.
* `reject_request {id, agent}` rejects a pending approval. Nothing is
  applied.

`id` is the `approval_id` a request returned. `agent` is your name for
yourself, such as the bot's name, 1 to 100 characters. Flats does not verify
it. Flats records it with the decision, together with the network the call
arrived on: Local, the console listener with the caller's IP address,
Tailscale with the tailnet login or node name of the calling device, NetBird
with the calling peer's name, or an external origin. The approval and the
flat's event log show the decision as made by Autopilot.

### Operations agents may decide now

{{operations}}

The operator can turn Autopilot or any operation on or off at any moment.
`get_approval` on a pending approval says whether agents may decide it now.

### Which operations an approval needs

An approval needs the operations of what it froze when it was requested,
not of the flat's current state:

| Approval action | Needs |
|---|---|
| `create_version` (and legacy `publish`) | Create versions |
| `delete_version` | Delete versions |
| `activate`, from `deploy` | Deploy |
| `rollback` without `restore_data` | Roll back |
| `rollback` with `restore_data`, and `restore_data` | Roll back with data restore |
| `set_visibility` from Private to Public | Make Public, and Public editing for access `edit` |
| `set_visibility` from Public to Private | Make Private |
| `set_visibility` from Public to Public with another access level | Public editing |
| `delete`, from `delete_flat` | Delete flats |

Both tools refuse an approval that needs an operation the operator does not
allow (`refusal.autopilot_not_allowed`). They also refuse one whose action or
frozen parameters Flats cannot read, such as a `set_visibility` approval from
an older Flats version that did not freeze its starting visibility. The
operator decides those in the console: give the user its `approval_url` and
poll `get_approval`.

### Decisions

* The first decision of an approval, by the operator or an agent, is the one
  that counts. Approving an approved request or rejecting a rejected one
  returns it unchanged, so you may retry. A decision in the other direction,
  or any decision while the approval is `applying`, is refused as
  `refusal.conflict`.
* An approved operation can still fail. Then the result has `status: failed`
  and `result_data.failure_code`. A failure Flats reports as a conflict, as for
  the operator (`topic.approvals`), is instead a refused call naming its
  category, for example `refusal.stale_approval` when what the approval froze
  changed before the decision, `refusal.public_edit_blocked` or
  `refusal.public_stop_unconfirmed`. The approval is still recorded `failed`:
  read it with `get_approval`.
* Flats checks Autopilot and the operation's switch when the decision takes
  effect. If the operator turned it off before that, the call is refused and
  nothing changes.
* Agents may decide every pending approval of an allowed operation, whoever
  requested it: an agent over MCP, the CLI or the API. Flats has no identity
  for agents.

### Limits

* Autopilot grants no public provider, key or secret. A flat made Public is
  reachable only through the public providers the operator permitted for
  that flat. Public is NOT access control: anyone on the internet can open a
  ready Public route.
* Never change the Experiments settings of the console, where the operator
  controls Autopilot, also not through a browser or by calling the console.
  Never call console routes or send console headers.
* Where the runtime reference (`flats://docs/runtime-api/v1`), the content
  types reference (`flats://docs/content-types/v1`) or `llms.txt` say that
  agents cannot decide approvals, the rules on this page take precedence
  while Autopilot is on.

### Operation guide from the operator

The operator's policy for agents, such as when to approve, how to name flats
and when to clean up. Flats does not enforce it. Whether to ask a person
before a decision is up to this guide and your own policy.

{{guide}}

## autopilot_off

Autopilot is off, so `approve_request` and `reject_request` decide nothing.
Nothing changed. List the tools again for the current set. Give the user
`approval_url` exactly as returned and poll `get_approval`. Only the
operator turns Autopilot on, in the console.

## autopilot_not_allowed

The approval needs an operation the operator does not allow agents to
decide, or Flats cannot read which operations it needs. The error names the
missing operations or the reason. Nothing changed. Give the user
`approval_url` and poll `get_approval`: the operator decides it in the
console. If the error says that Autopilot changed during the call, call
again.
