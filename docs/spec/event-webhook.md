# Event webhook and audit export

Status: planned.

## Summary

An operator learns about a pending approval only when they open the console or when the agent says so, and no record of
the decisions can leave the host. This spec adds two things. The host sends a signed HTTP POST to a URL that the operator
stored when an approval is created or decided, when the visibility of a flat changes, and when a grant of a flat's network
access changes. `flats audit export` writes the audit log and the approval records as JSON lines, so that the operator can
ship them off the host.

The request is issue #225. It builds on the audit log of [audit events](audit-events.md) (item 4 of #224). Approval
records stay when a flat is deleted, and the export contains them.

## Before this change

- An approval is a row of `approvals` (`internal/store/store.go`) with `id`, `flat` (the slug), `action`, `params`,
  `status` (`pending`, `applying`, `approved`, `rejected` or `failed`), `via`, `reason`, `result`, `requested_at`,
  `decided_at`, `decided_by`, `authorized_at`, `idempotency_key` and `result_data`. It has no column for the ID of the
  flat. A rename rewrites `approvals.flat`. Nothing deletes an approval row, and no retention touches it. A delete that an
  agent requested leaves the row after the flat is gone. A delete from the console applies at once and makes no approval
  row.
- `Service.requestFrozen` inserts every approval. A request that repeats an open one returns the open row and inserts
  nothing. `Service.Decide` and `DecideAsAgent` call `decide`. A rejection is one write (`RejectApprovalAuthorized` or
  `RejectApprovalAutopilot`). An approval claims the row first (`applying`), and `finishApplying` writes `approved` or
  `failed`. Three other code paths set a final status: a flat deletion sets the other pending approvals of that flat to
  `failed`; `resumeApplying` finishes a claim that a crash left, and runs inside `core.New`, before the host holds the
  service; and `AdoptLegacyDraft`, which `migrateLegacyDrafts` runs from `core.New` at every start. `decided_by` is
  `console`, a tailnet login, `netbird:<peer>` or, for an agent under [Autopilot](autopilot.md), `autopilot:` followed by a
  JSON object with the agent name, its address and its login.
- The console link is `Service.ApprovalURL`. It takes the console address of the request. Without one it falls back to
  `Host.ConsoleURL`: the preferred console, else the first console domain that is served, else the loopback address.
- A visibility change that an approval applies is committed in one place, `Store.CommitVisibility`, called from
  `applyVisibilityApproval`. The public instance mode creates flats Public and changes no visibility. When guest editing
  is off, the host lowers the stored public access level of a flat from edit to view (`downgradeEditWhen` in
  `internal/core/publicedit.go`), at start and on reload, with no approval.
- "Grant" has three meanings in the code. This spec uses the first two (decision 1): the per-flat provider permission
  (`Service.SetProviderPermission`, console only), the per-flat outbound origin grants of a flat's `fetch`
  (`Service.SetNetworkPolicy`, the `/flats/{slug}/network` route), and the host-wide `network.permitted` list in
  `config.json`. None of the three writes `audit_events`. The first two write a flat event of kind `provider` or `network`
  after the commit. `PreserveLegacyTailscale` inserts a per-flat grant outside the setters, at the start of a legacy service that names
  `--network tailscale`.
- Operator keys are sealed values of `host_keys`. The table has `CHECK (id IN ('tailscale','zrok','netbird'))`, so a new
  key needs a migration. `flats key set` and `rm` work while the host is stopped. Two lists of key IDs exist in the
  code: `core.HostKeyIDs`, which `flats key` accepts and `flats key ls` iterates, and the console's `hostKeys`, which
  iterates the same list today. The console has routes for the settings, for the three provider keys and for the
  providers. No MCP tool and no `/api` route reads or writes a setting or an operator key, and no route, tool or view
  reads or writes `audit_events`.
- The only outbound client with an address guard, `internal/egress`, serves the `fetch` of flats. It refuses private,
  loopback and CGNAT addresses and every port except 80 and 443, which a receiver of the operator may need.
- `flats audit ls` reads `audit_events`, and `flats` for the slug of a flat ([audit events](audit-events.md), rules 20 to
  24).

## Scope

In scope: the four webhook events and their body, the two operator keys that configure the webhook, the signature, the
delivery rules, the audit kinds of rule 21, the command `flats audit export`, the migration to schema version 26, the
docs.

Out of scope:

- A console view, a route or an MCP tool for the webhook, and a test button. The operator sets the keys with `flats key`
  while the host is stopped. A console form is a later change.
- Webhook events for the host-wide `network.permitted` list, for the creation of a flat, for the public instance mode, for
  the downgrade of a stored public access level when guest editing is off, for the grant that `PreserveLegacyTailscale`
  inserts, for the statuses that `resumeApplying` and `AdoptLegacyDraft` write, and for secrets and operator keys. The
  audit log records the secrets and the operator keys.
- A durable queue. An event that the host cannot deliver before it stops is lost (rule 19).
- A webhook for each flat, several URLs, a filter by event type, and a payload that an operator edits.
- A flat-deletion record. A delete from the console makes no approval row and writes no audit row of its own. Such a flat
  shows in the export only through the audit rows of its secrets and through the pending approvals that its deletion
  failed.
- Tamper evidence, trimming and a signature of the export. Whoever writes `flats.db` can change both.
- Retries across restarts, and ordered delivery.

## Decisions

These are choices that the issue leaves open. The repository owner decided 1, 2 and 3 on 2026-11-16, each as the author
proposed it. The author decided 4, 5 and 6, and a reviewer can challenge them. Each has its reason.

1. A "network grant" is a per-flat grant: the provider permission of a flat and the origin grants of a flat. The host-wide
   list is a setting of the host, not a grant of a flat. A webhook event names a flat, and `network.permitted` has none.
   The owner decided that the host-wide list sends no event and that the downgrade of a stored public access level is not a
   visibility change.
2. The URL and the signing secret are sealed operator keys, `webhook-url` and `webhook-secret`. A URL often holds a token
   (a chat webhook URL is one), and `flats config show` prints `config.json` in clear. The key mechanism also gives the
   audit events (`key.set`, `key.delete`), `flats secret backup-key`, `restore-key` and `reset`, and the later rotation.
3. The operator sets the keys with `flats key set`, as for the other keys. That works while the host is stopped. The host
   reads the keys when it sends an event, so a start after the change uses them. There is no console form, and there is no
   new way for an agent to change them, because there is no route.
4. Delivery is in memory with a bounded retry (rules 13 and 14). The audit log and the approval records are the durable
   record, and the export ships them. A table of pending deliveries would make the notification a second source of truth.
5. The export gets the visibility changes and the two kinds of grant through new audit kinds (rule 21), so that every event
   that the webhook sends has a record that the export contains. The approval records carry the other two events.
6. An approval gets the ID of its flat (rule 28), as the audit rows have, so that the export does not mix a flat with an
   older flat that had the same slug.

## The events

1. The webhook sends four types of event.

   | Type | When |
   | --- | --- |
   | `approval.created` | `requestFrozen` inserts a new approval, from the console or from an agent. A request that repeats an open one sends nothing. |
   | `approval.decided` | An approval gets the final status `approved`, `rejected` or `failed` through `decide` (an operator or an Autopilot decision), through `finishApplying`, or through the flat deletion that fails the other pending approvals of the flat. The event follows the commit of that status. `resumeApplying` and `AdoptLegacyDraft` send nothing: they run inside `core.New`, before the host holds the sender (rule 17). |
   | `visibility.changed` | `CommitVisibility` commits a change of the visibility or of the public access level of a flat that an approval applied. |
   | `network.changed` | A provider permission or the origin grants of a flat change in the store. |

2. An operation that changes nothing sends nothing. A set of the origin grants to the list that the flat has, a provider
   permission to the value it has (a provider with no row counts as not permitted), and a visibility to the value it has
   write no audit row and send no event.
3. An event is made after the commit of the change, in the goroutine of the operation, and is handed to the sender
   (rule 15). A crash between the commit and the hand-over loses the event. The audit log keeps its row (rule 21).
4. An event has this body, a JSON object in UTF-8 of at most 4 KiB. The sender never adds a field to it, and no event has a
   field that this list does not name.

   | Field | Content |
   | --- | --- |
   | `id` | `evt_` and 26 random characters. The same for every attempt to deliver the event. |
   | `type` | One of the four types. |
   | `at` | The time of the event on the clock of the host, an RFC 3339 time in UTC with milliseconds. |
   | `flat` | `{"slug": ..., "id": ...}`. `id` is the ID of the flat (`store.Flat.ID`) and is empty when none is known. |
   | `operation` | The action of the approval (`delete`, `set_visibility`, `publish`, `create_version`, `delete_version`, `activate`, `rollback` or `restore_data`) for the approval types. `set_visibility` for `visibility.changed`. `set_network_origins` or `set_provider_permission` for `network.changed`. |
   | `state` | `pending` for `approval.created`. `approved`, `rejected` or `failed` for `approval.decided`. `applied` for the other two. |
   | `approval_id` | The approval, for the approval types and for a `visibility.changed` that an approval applied. Absent otherwise. |
   | `via` | For `approval.created` the channel that requested the approval, as the row records it. Absent for the others. |
   | `decided_by` | For `approval.decided` and for `visibility.changed`: `operator` for a decision of the operator and `autopilot` for a decision of an agent. Absent when the row has no `decided_by` (a pending approval that a flat deletion failed) and for the other types. |
   | `change` | For `visibility.changed` `{"from": {"visibility": ..., "access": ...}, "to": {...}}`. For `network.changed` `{"grant": "origins", "origins": [...]}` with the list after the change, or `{"grant": "provider", "provider": ..., "permitted": true or false}`. Absent for the approval types. |
   | `console_url` | `<console>/approvals/<id>` for the approval types and `<console>/flats/<slug>` for the others. The console address is the one of the request that caused the event, or, without a request, the one of `Host.ConsoleURL` (the preferred console, else the first console domain that is served, else the loopback address). The address is chosen when the event is made. |

5. The body holds no flat content and no secret value. It holds none of the fields `params`, `reason`, `result` and
   `result_data` of an approval, none of the content, secret names, environment names, file names, version numbers or
   document text of a flat, and no key. The values of `operation`, `state`, `type`, `decided_by` and `grant` come from fixed
   lists. The slug, the origins and the provider name are the operator's own settings. The body names no login and no
   address of a person or an agent.

## The webhook keys

6. `webhook-url` and `webhook-secret` are two operator keys of `host_keys`, sealed as the other keys are. The webhook is on
   while both hold a value, and off otherwise. At its start the host checks whether each key has a row, and logs one line
   when only one has, which says which is missing. It sends nothing then. The default is off.
7. `flats key set webhook-url|webhook-secret`, `flats key ls` and `flats key rm` work as for the other keys, with the value
   on stdin or in `--from-file`. The code keeps two lists. `core.HostKeyIDs` holds all five IDs and is what `flats key`
   accepts and lists. The console lists, sets and removes only the three provider keys, so it does not iterate
   `core.HostKeyIDs`. For the webhook keys, `flats key ls` prints `set (from SOURCE, TIME)` or `none`, never the value.
   `flats key set` prints `stored the webhook URL in PATH; events are sent when both webhook keys are stored` (and the same
   with `secret`). `flats key rm` prints `removed the webhook URL; the webhook is off` (and the same with `secret`). The
   key commands, `flats secret reset`, `backup-key` and `restore-key` already treat every row of `host_keys`, and the audit
   events `key.set` and `key.delete` carry the names `webhook-url` and `webhook-secret` with the same fields as for the
   other keys.
8. A `webhook-url` value passes `core.CheckHostKeyValue` or the set is refused. The scheme is `https`, or `http` when the
   host is `localhost` or an IP address literal in the loopback, private, link-local or CGNAT ranges (a receiver on the
   same machine or network). The URL has a host, no fragment and at most 2048 bytes. User information and a query are
   allowed. The check does not resolve names. A `webhook-secret` value has at least 16 bytes. Both values keep the limits
   of the other keys (at most 4096 bytes, no white space).
9. The console routes, the console views, `/api`, the MCP tools, `/api/status` and the logs of flats never carry the URL, the
   secret or the fact that a webhook is on. The key routes of the console list the three provider keys, as they do today,
   and do not list these two. No route and no tool has `webhook` in its name.
10. The host logs the failure of a delivery with the event ID, the type and the reason (the HTTP status or the error class).
    It never logs the URL, the secret, the signature or a response body. An error text from the HTTP client holds the URL,
    so the log line carries only a class: `connect`, `timeout`, `tls`, `status NNN` or `other`.

## Delivery

11. The sender reads and opens the two keys from `flats.db` when it makes the first attempt of an event, and uses those
    values for every attempt of that event. It does not cache them across events. An event that finds either key missing
    is dropped with no log line (the webhook is off), so while the webhook is off an event costs one hand-over and one read
    of `host_keys` in the goroutine of the sender, and nothing else. An event that finds the keys unreadable (the key file
    is gone, or a value does not open) is dropped with one log line.
12. Each attempt is a `POST` of the body with these headers:

    | Header | Value |
    | --- | --- |
    | `Content-Type` | `application/json` |
    | `User-Agent` | `flats/<version>` |
    | `X-Flats-Event` | the type |
    | `X-Flats-Event-Id` | the `id` of the body |
    | `X-Flats-Timestamp` | the Unix time in seconds of this attempt |
    | `X-Flats-Signature` | `v1=` and the lower-case hex of HMAC-SHA256, with the secret as the key, of the timestamp text, a `.`, and the exact bytes of the body |

    For the secret `whsec_0123456789abcdef0123456789abcdef`, the timestamp `1700000000` and the body
    `{"id":"evt_test","type":"approval.created"}`, the signature is
    `v1=edecf23a0b88e1b94a9e5c0e5534118dcd61eeaf578b04fa0696a8a3f9b79919`. The author computed it with the `crypto`
    module of Node.js, and a test computes it again with `crypto/hmac`. The docs give the check for a receiver: compute it,
    compare in constant time, refuse a timestamp that is more than five minutes from the receiver's clock, and ignore a
    second event with the same ID.
13. An attempt succeeds on a status 200 to 299. It does not follow a redirect: a 3xx is a failure. It verifies the
    certificate of an `https` URL and has no way to skip that. It has a time limit of 10 seconds for the whole attempt. It
    reads and drops at most 4 KiB of the response. The retry rules are:
    - a network error (a refused connection, a failed TLS handshake or certificate check, a timeout), a status 408, a
      status 429 and a status 5xx are retried;
    - any other status, and a redirect, are permanent: the event is not retried and the failure is logged.
14. An event is tried at most 4 times, with a wait of 5 seconds, then 30 seconds, then 2 minutes after the previous failed
    attempt. The waits are fields of the sender that a test replaces. After the last attempt the failure is logged and the
    event is dropped.
15. A failed delivery, a slow receiver and a full queue never block, delay or fail the operation that made the event. The
    hand-over is a short critical section that does no I/O, and nothing in the path of an operation waits for the sender.
    The sender reads the database and calls the receiver only in its own goroutines, and holds no transaction while it
    waits for the receiver.
16. The sender keeps at most 256 events that are not yet delivered, and runs at most 4 deliveries at once. When a new
    event finds the queue full, the sender drops the oldest event that no delivery has started, and logs the ID of the
    dropped event. Deliveries start in the order in which the events were made. Four run at once and a retry waits, so a
    receiver can see the events in another order. An agent that creates many approvals can push earlier events out of the
    queue, and the docs say so.
17. The sender is owned by the host. The host builds it after `core.New` returns and sets it on the service before the
    management server accepts a request, so an event that `core.New` makes (rule 1) has no sender and is not sent. The host
    starts its goroutines with its other background work and stops them when it closes: an attempt in flight is cancelled,
    waiting events are dropped, and `Close` waits for the goroutines of the sender before it closes the store, as it does
    for its other goroutines. A hand-over after the sender stopped drops the event, does not block and does not panic.
18. A delivery is tried at most four times within one run of the host. A receiver can see an event twice (a timeout after
    the receiver processed it, or a retry after a restart of the receiver) and deduplicates on `X-Flats-Event-Id`. It can
    also see an event zero times (rules 16 and 19).
19. The retry and the limits of rules 13, 14 and 16 are the whole of the guarantee. An event that is waiting when the host
    stops, or that exhausted its attempts, is lost. The export (rules 22 to 27) is how an operator recovers the record.
20. A test replaces the sender's HTTP client, its clock, its waits and its attempt time limit. A receiver in a test is an
    `httptest` server on the loopback address, which rule 8 allows.

## The audit kinds

21. `audit_events` gets three kinds. They follow rules 1 to 7 of [audit events](audit-events.md) and are written in the
    transaction of the change (its rule 16): when the audit write fails, the change does not happen.

    | Kind | `flat` | `name` | `data` | `via` |
    | --- | --- | --- | --- | --- |
    | `visibility.set` | the flat | none | `{"from": {"visibility": ..., "access": ...}, "to": {...}, "approval": ID}` | `console` (decided in the console) or `autopilot` (decided by an agent) |
    | `network.set` | the flat | none | `{"origins": [...]}` with the list after the change | `console` or `cli` |
    | `provider.set` | the flat | the provider | `{"permitted": true or false}` | `console` |

    `actor` of `visibility.set` is the actor text of the console when `via` is `console`, as for every console row, and
    `core.DeciderText` of the approval's `decided_by` when `via` is `autopilot`. Both are cut at 512 bytes on a character
    boundary. The actor is empty when the change is applied without a console request, as `resumeApplying` does. The `at` of
    each new row is taken on the clock of the service immediately before the call of the store function that writes it,
    not when the caller built the change. For `network.set` and `provider.set` the actor is the actor text of the console for `console` and empty for
    `cli`. `visibility.set` is written by `Store.CommitVisibility`. `network.set` and `provider.set` are written by the
    store functions that set the origin grants and the provider permission. Each writes its row only when its value
    changes (rule 2). `Service.SetProviderPermission` stops the routes of a revoked provider before it calls the store,
    and that order does not change. A failed audit write leaves the permission as it was and returns the error, as a failed
    write of the permission does today. The migration, the public instance mode, the creation of a flat, the legacy
    Tailscale grant and the downgrade of a stored public access level write none. [Audit events](audit-events.md) lists
    these kinds in its table, in rule 4 (`via` `autopilot`) and in rule 5 (the `actor` of `visibility.set`), and its scope no
    longer excludes them.

## The export

```text
flats audit export [--since TIME] [--config PATH | --data DIR]
```

22. The command writes JSON lines to stdout, oldest first, and nothing else to stdout. `--since` takes an RFC 3339 time or a
    duration such as `24h`, compares it with the `at` of each line, and includes a line whose `at` equals the time. Without
    it the command writes everything. A value that does not parse is a usage mistake with status 2. There is no limit
    flag. The command opens `flats.db` and reads it in the ways that rules 22 and 23 of [audit events](audit-events.md) say
    (read-only, no host lock, no call to the host, pages of at most 500 rows each in its own transaction, no transaction
    while it waits for its output, status 78 for no database and for a database of another schema, status 1 for another
    failure). It creates no row.
23. Every line has `record` and `at` (an RFC 3339 time in UTC with milliseconds). There are three kinds of line.
    - `{"record": "audit", ...}` with the fields of `flats audit ls --json` (`id`, `at`, `kind`, `flat`, `flat_id`, `name`,
      `via`, `actor`, `data`), one for each row of `audit_events`.
    - `{"record": "approval", "event": "created", "at": <requested_at>, "id", "flat", "flat_id", "action", "via",
      "reason"}`, one for each approval row.
    - `{"record": "approval", "event": "decided", "at": <decided_at>, "id", "flat", "flat_id", "action", "state",
      "decided_by", "result"}`, one for each approval row whose status is `approved`, `rejected` or `failed`. When such a
      row has no `decided_at`, the line uses `requested_at`.

    `decided_by` is the stored value, cut at 512 bytes on a character boundary. It can hold a login, a peer name or the
    address of an agent. `reason` is text that an agent wrote, and `result` can be an error text of the host (a path, a
    runtime message). The host does not filter either, and both are cut at 1 KiB on a character boundary. The docs say so.
    No line has `params`, `result_data`, `authorized_at` or a secret value. Each line holds the `id` of its approval and
    the `flat_id` of the row, which joins it to the audit rows. A rename rewrites `approvals.flat`, so an old approval line
    carries the new slug while an old audit row keeps the old one. `flat_id` is empty for a row that was written before
    schema 26 and whose flat was already gone at the migration (rule 28).
24. The lines are ordered by `at`, and lines with the same `at` come in the order: `audit` rows by `id`, `created` lines by
    approval `id`, `decided` lines by approval `id`. The implementation merges three ascending streams (the audit rows, the
    `created` lines and the `decided` lines), each read in pages of at most 500 rows by `(at, id)`.
25. The command takes its cutoff when it starts: the host clock time in milliseconds minus 30 seconds. It writes no line
    with a larger `at`. The host takes the `at` of a record before it starts the write, and the write waits at most the busy
    timeout of the store (5 seconds) for the lock (a write that waits longer fails and leaves no row), so a record whose
    `at` is not later than the cutoff has been committed already. A record that a running host writes after the cutoff is left for the next export. A write that
    stalls for more than 30 seconds between taking `at` and its commit can still be missed, and the docs say so. When the
    command ends it prints to stderr one line, `exported N lines through TIME`, where TIME is the cutoff. A later export
    with `--since TIME` continues from there. The boundary line can appear in both exports, and a reader deduplicates on
    `record`, `event` and `id`.
26. An approval that an agent requested for a delete, and that the operator approved, is in the export after the flat is
    gone, as a `created` and a `decided` line with `action` `delete`, `state` `approved`, the `decided_by` of the decision
    and the slug and ID of the flat. The audit rows of the secrets of that flat are there too, with `via` `flat-delete` and
    the `approval` ID. A delete from the console is in the export only as the scope says.
27. No MCP tool, no route and no console view runs the export or reads what it reads (rule 24 of
    [audit events](audit-events.md) holds for it).

## Compatibility

28. The migration to schema version 26 runs in the transaction of the version bump, after the copy in `<data>/backups` that
    a migration of an existing database makes. It rebuilds `host_keys` with the check
    `id IN ('tailscale','zrok','netbird','webhook-url','webhook-secret')` and keeps every row, and it adds
    `approvals.flat_id` (`TEXT NOT NULL DEFAULT ''`). For each existing approval it sets `flat_id` to the `ID` of the flat
    that now has the approval's slug, or leaves it empty when no such flat exists. It adds indexes on
    `approvals(requested_at)`, `approvals(decided_at)` and `audit_events(at, id)`. `InsertApproval` sets `flat_id` for every
    new approval from the flat that the request names, and a rename does not change it. An older Flats refuses a database
    of schema 26 (`ErrSchemaTooNew`), as it refuses 25, and the docs say again that a downgrade needs a backup of the whole
    data directory.
29. `Inspect` and `HasData` are unchanged: both `approvals` and `host_keys` count already. `flats backup` copies the new
    columns and the keys with `flats.db`, and its verifier refuses a copy of another version.
30. An existing flat, approval and key is unchanged. No event is sent and no audit row is written for what happened before the
    migration.
31. The change amends [audit events](audit-events.md): its status and a "Current state" section until the three kinds are
    written, its scope (the webhook and the export are now in this spec, and the three kinds are in scope), its table (three
    rows), rule 3 (the fields of rule 21 are allowed in `data`), rule 4 (`via` `autopilot`), rule 5 (the `actor` of
    `visibility.set`), and the reader's schema version. The spec index gets a row for this page.

## Completion criteria

Tests that need a file mode, a read-only folder or a link run on Unix as a user that is not the superuser, and skip
elsewhere. The receiver of a test is an `httptest` server (rule 20). The failure of an audit write is a `BEFORE INSERT`
trigger on `audit_events`, as for [audit events](audit-events.md). The migration fixture is a database built by the
registry up to version 25. The export reads its clock through a function variable that a test replaces.

- Signature. The vector of rule 12 gives the signature that rule 12 states, computed by a test with `crypto/hmac` from the
  documented recipe and not by the code under test. A body or a timestamp that changes by one byte gives another value.
  An attempt carries the headers of rule 12 and the timestamp of its own attempt, and a retry keeps the event ID.
- Events. Each of the four types is sent for the change that rule 1 names and for no other: a repeated open request, a no-op
  set (rule 2), the public instance mode, a creation, a migration, the legacy Tailscale grant, the downgrade of a stored
  public access level, a secret change, a host-wide grant, `resumeApplying` and `AdoptLegacyDraft` send nothing. A
  visibility approval sends `approval.decided` and `visibility.changed`. A flat deletion that fails the other pending
  approvals of the flat sends one `approval.decided` with state `failed` for each. An approved delete sends
  `approval.decided` with the slug and the ID of the flat that is already gone. A rejection by the operator and an
  Autopilot decision send `approval.decided` with `decided_by` `operator` and `autopilot`.
- Body. A test builds a flat with a distinctive secret value, a distinctive file content, an approval `reason`, `result`
  and `params` that hold a distinctive string, and an operator key value, and decides an approval as an Autopilot agent
  with a distinctive address and login. No body of any type holds any of them. The fields of every body are exactly the
  fields of rule 4 for its type. The console link is the approval link of the console address of the request, and, for an
  event made without a request, the link of the address that `Host.ConsoleURL` gives on a host with a preferred console
  domain and on a host without one.
- Failing endpoint. Against a receiver that answers 500, that does not answer within the limit (the replaced time limit),
  that refuses the connection, that fails the certificate check, that answers 400 and that answers 302: the operation that
  made the event (`Decide` of an approval, a visibility change, a grant change) returns its normal result before the
  receiver is released, and the approval is `approved`. The receiver that answers 500, the refused connection and the
  certificate failure see 4 attempts with the replaced waits, and the events with 400 and 302 see one. The failure is in
  the log with the event ID and a class (`tls` for the certificate), and the log holds neither the URL nor the secret.
- Sender. A start with only one of the keys logs one line that names the missing key (rule 6). Keys that do not open drop
  the first event with one log line and make no attempt (rule 11). With the webhook off, a decision makes no attempt and
  no log line. A hand-over after `Close` returns at once and sends nothing, and no goroutine of the sender is left after
  `Close` (rule 17).
- Queue. With a receiver that never answers and 300 events made, at most 4 are in flight and at most 256 wait, the oldest
  ones are dropped with their IDs in the log, and no operation waited. `Close` returns without waiting for a delivery.
- Configuration. With only one key, with a refused URL (`ftp:`, a name with `http:`, a fragment, 2049 bytes) and with a short
  secret, `flats key set` refuses or the webhook stays off, and nothing is sent. Both keys on: an event arrives. The audit
  rows `key.set` and `key.delete` name the keys and hold no value. `flats key ls`, `set` and `rm` print the texts of
  rule 7 and no value. `flats secret reset` turns the webhook off. A test stores a distinctive URL and secret and shows
  that no console route, no `/api` route, no MCP tool, `/api/status` and no flat log contains them, that no tool or route
  has `webhook` in its name, and that the console key list is unchanged while `flats key` accepts the two IDs.
- Audit kinds. A visibility change decided in the console, one decided by Autopilot, an origin grant change through the
  console and through the CLI header, and a provider permission change write the rows of rule 21 with the right `via`
  and `actor`, in the transaction of the change: with a failing audit write the change does not happen, and the
  revocation of a provider keeps the order of rule 21. A change that alters nothing writes no row. With a replaced clock, the `at` of a `visibility.set` row is the time of its
  write and not the time at which the approval applied its change.
- Export. After an approved agent delete of a flat with two secrets, `flats audit export` prints the `created` and
  `decided` lines of the approval and the two `secret.delete` rows, and the flat is gone. After a console delete of a flat
  with a pending approval, the `decided` line with state `failed` and the secret rows are there. `--since` with a time and
  with a duration cuts by `at` and includes the boundary. The order follows rule 24 with rows of the same `at`. A
  database of 1200 audit rows and 600 approvals is printed in full and in order, and a stalled reader of the output between
  two pages shows that no transaction is held. With a replaced clock, a row whose `at` is later than the cutoff is not in
  the export and is in the next one that starts at the printed time, and a row at the cutoff is. The stderr line has the
  cutoff. A usage mistake is status 2, no `flats.db` and another schema are status 78. A flat that was renamed or
  recreated under a slug is separated by `flat_id`. The command writes nothing: the data directory is the same before and
  after for a stopped host that left no `-wal`, and `-wal` and `-shm` rules as for `flats audit ls`. `reason` and `result`
  of 2 KiB are cut at 1 KiB.
- Migration. A database of version 25 migrates to 26: `host_keys` accepts the two new ids and keeps its rows,
  `approvals.flat_id` is filled for approvals whose flat exists and is empty for the others, the indexes exist, and a
  reader of version 25 refuses version 26. The tests that pin the latest version change with it.
- Docs. `docs/configuration.md` describes the two keys, the events and their body, the headers, the signature check with an
  example a receiver can run, the retry and the limits (what is lost, the duplicate, the order, the flood of approvals),
  the privacy of `decided_by`, `reason` and `result` in the export, the export lines, the `--since` continuation and the
  30 second lag, and the downgrade; `docs/design.md` (Approvals), `docs/surfaces.md` (the CLI table) and the spec index
  change; `flats help audit` lists `export`; the checks of the documentation pass.

## Open questions

None.
