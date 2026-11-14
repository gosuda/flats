# Audit events

Status: planned.

## Summary

Flats records secret and key operations only in the log of a flat, and that log is trimmed to its newest events and
deleted with the flat. Operator keys and the host key have no record at all. This spec adds a host-level audit log in
`flats.db`. It is append-only, it is not trimmed, and a deleted or renamed flat does not change it. Every operation that
stores, replaces or removes a sealed value, and every key backup, key restore and reset, writes one event: what
happened, to which flat and name, when, through which channel, and by whom when the channel knows. The value is never
recorded. `flats audit ls` reads the log.

The request is item 4 of issue #224. Item 2 (rotation) writes its own event later. The webhook and `flats audit export`
of #225 build on this log.

## Current state

- The `events` table (`internal/store/store.go`) holds the log of each flat. `events.flat` has no foreign key.
  `DeleteFlat` runs `DELETE FROM events WHERE flat=?`, and a rename runs `UPDATE events SET flat=?`. `Service.Event`
  trims a flat's log to `system.events_keep` (5000) events after every 200 new events, and the runtime log lines of a
  flat share the table, so a noisy flat can push other rows out.
- `SetSecret` and `DeleteSecret` (`internal/core/secrets.go`) write the flat event `secret NAME set via VIA; redeploy to
  apply` (or `deleted via`) after the change and outside its transaction. It does not tell a new secret from a replaced
  one, and a delete of a missing name records nothing. The secrets of a flat disappear without a trace when the flat is
  deleted, because `secrets` cascades.
- `flats secret reset` writes one flat event for each flat secret it deletes and nothing for operator keys, in the
  transaction of `DeleteSealedValues`. `backup-key` and `restore-key` write nothing.
- Operator keys have no event. The console routes (`internal/app/keys.go`), `flats key set` and `rm`
  (`internal/app/keycmd.go`), the start-time inputs (`resolveStoredKey`), the import of a legacy key
  (`internal/app/setup.go`) and the zrok enrolment write a line of the host log at most. That log is stderr or a file
  that is not queryable and that a backup leaves out.
- No reader lists the events of a deleted flat or of no flat. The readers (`GET /flats/{slug}/logs`, the MCP tool
  `get_logs`, the console, `flats logs`) all need the flat to exist.
- `flats.db` has schema version 24 (`internal/store/migrate.go`). `Inspect` ignores the tables `host_binding`,
  `autopilot` and `public_instance` when it decides whether a database holds data.
- Approvals survive the deletion of a flat and record `decided_by` and `via`. Decisions on the loopback listener have
  no identity, and #139 covers that part.

## Scope

In scope: the table and its migration, the events listed in rules 8 to 15 and where each is written, the command
`flats audit ls`, the docs.

Out of scope:

- The webhook, `flats audit export` and the approval records in an export (#225).
- The writer of the rotation event. This spec reserves the kind `key.rotate` (rule 15). The rotation spec gives its fields
  and writes it.
- A console view, an MCP tool or an API route for the log. Agents cannot read it.
- Tamper evidence (a hash chain or a signature), a way to trim the log, and shipping it off the host. A person who can
  write `flats.db` can edit the log. Shipping the events off the host is the purpose of #225.
- Events for the decryption of secrets at an activation, for `flats backup` reading the key, for a change of
  `host.secret_key_path` and for changes of `config.json`.
- Changes to the per-flat events, to `system.events_keep` or to what a flat's log shows.

## The log

1. `audit_events` is a new table of `flats.db`, created by the migration to schema version 25. A row has `id` (an
   autoincrement integer, which is the order), `at` (Unix seconds, from the clock of the host), `kind`, `flat` (the
   name of the flat when the event was written, or empty), `name` (the secret name or the operator key name, or
   empty), `via`, `actor` (empty when unknown) and `data` (a JSON object, or empty). It has no foreign key and no
   trigger. An index on `(kind, id)` and one on `(flat, id)` serve the filters of `flats audit ls`.
2. The code only appends. No function updates or deletes an audit row. That holds for retention, for `Sweep`, for
   `DeleteFlat`, for a rename, for a data restore and for `flats secret reset`. The deletion of a flat leaves its rows,
   and a rename leaves them under the old name. A person who can write `flats.db` can still change them, as "Scope"
   says.
3. No row holds a secret value, an operator key value, key bytes, a recovery password or the recovery copy. A name is
   not a value. `data` holds only the fields that rules 8 to 15 name. The key ID of rules 12 and 13 is the one that
   [secret key recovery](secret-key-recovery.md) defines. It reveals nothing that opens the key.
4. `via` says which channel made the change. It is one of
   - `console`: a route of the console API (the web console);
   - `cli`: `flats secret set` or `rm` through the loopback API with the header `X-Flats-Client: cli`;
   - `command`: a command that the operator ran on the host and that works on the data directory
     (`flats key set` and `rm`, `flats secret backup-key`, `restore-key` and `reset`);
   - `start`: an input that the host read at its start (a key file flag, a `FLATS_*` variable, or the deprecated
     `credentials.tailscale_authkey_file`);
   - `migration`: the import of a legacy key file when an older installation moves to a configuration file;
   - `flat-delete`: the deletion of a flat removed its secrets.
5. `actor` is the identity that the channel knows. For `console` it is the actor text that `consoleActor`
   (`internal/api/experiments.go`) derives from the request: a tailnet user or node, a NetBird peer, the external origin,
   or the socket address of the caller. For `command` it is the name
   of the operating-system user that ran the command, or empty when the system gives none. For `cli`, `start`,
   `migration` and `flat-delete` it is empty: the `cli` header is a client's claim and not an identity
   ([design](../design.md), "Secrets"), and the others have no person. #139 may fill `actor` for `cli` later.
6. A row never changes the meaning of an existing per-flat event. The events `secret NAME set via VIA` and the events of
   `flats secret reset` stay as they are.
7. The log has no limit and no trimming in this change. A set or delete of a secret is an action of the operator, and
   an event is under 1 KiB, so the log grows with what the operator does. `data` holds at most 4 KiB.

## What each event records

| Kind | `flat` | `name` | `data` | `via` |
| --- | --- | --- | --- | --- |
| `secret.set` (8) | the flat | the secret | `{"created": true or false}` | `console`, `cli` |
| `secret.delete` (9) | the flat | the secret | none | `console`, `cli`, `command` (reset), `flat-delete` |
| `key.set` (10) | none | `tailscale`, `netbird` or `zrok` | `{"replaced": true or false, "source": ...}` | `console`, `command`, `start`, `migration` |
| `key.delete` (11) | none | the key | `{"reason": ...}` | `console`, `command`, `start` |
| `key.backup` (12) | none | none | `{"key_id": ..., "out": true or false}` | `command` |
| `key.restore` (13) | none | none | `{"key_id": ..., "out": true or false, "opened": N, "total": M}` | `command` |
| `key.reset` (14) | none | none | `{"secrets": N, "keys": M}` | `command` |
| `key.rotate` (15) | none | none | reserved | reserved |

8. `secret.set` is written for each successful set of a flat secret, whether the name is new (`created` true) or
   replaced. A set that fails validation or the name check writes nothing, as it changes nothing.
9. `secret.delete` is written for each secret that is removed: by `DeleteSecret` (a delete of a name that does not
   exist changes nothing and writes nothing), by `flats secret reset`, and by the deletion of a flat, one event for each
   secret the flat held, with `via` `flat-delete`. The names are read in the transaction that deletes the flat.
10. `key.set` is written whenever a sealed operator key value is stored or replaced. `source` is the source that
    `host_keys` records (`console`, `cli`, `flag`, `env` or `config`). The values `start` and `migration` of `via`
    belong with `flag`, `env` or `config` as their source. A start input that is ignored because a key of that kind
    is stored already (the seed rule of [operator keys](../configuration.md#operator-keys)) changes nothing and writes
    nothing.
11. `key.delete` is written whenever a sealed operator key value is removed: by the console, by `flats key rm`, by
    `flats secret reset`, and by a zrok token that the host consumes when it connects the account (`reason`
    `connected`). `reason` is `removed` for the other cases. A bookkeeping row of `host_keys` that holds no value, such
    as the record of a connected zrok account, writes nothing when it is created or removed.
12. `key.backup` is written when `flats secret backup-key` has written the recovery copy and its ID file. `key_id` is
    the ID of the key in the copy. `out` says that `--out` wrote a second copy. The path is not recorded.
13. `key.restore` is written when `flats secret restore-key` has written the key to the key location or, with `--out`,
    to the new file. `key_id` is the ID of the restored key, and `opened` and `total` are the numbers of sealed values
    that the key opened and that `flats.db` holds.
14. `key.reset` is written by `flats secret reset` once, in the transaction that deletes the values, together with one
    `secret.delete` or `key.delete` for each value (rules 9 and 11). `secrets` and `keys` count them.
15. `key.rotate` is reserved for `flats secret rotate-key`. This change writes it nowhere and the readers accept it.

## When an event is written

16. A change of `flats.db` and its event are one SQLite transaction: a set or delete of a flat secret, a store or removal
    of an operator key value, the deletion of a flat (with the `secret.delete` events of rule 9), and `flats secret
    reset`. When the event cannot be written, the change does not happen and the operation returns the error. There is no
    change without an event.
17. `key.backup` and `key.restore` change no row. The command does its work and then appends the event. When the
    append fails, the command exits with status 1, says that the work was done and the event was not recorded, and
    names the file it wrote. It does not undo the file.
18. `flats secret backup-key` works while the host runs and takes no host lock (rule 12 of
    [secret key recovery](secret-key-recovery.md)). It appends its event with a short write of its own to `flats.db`,
    with the busy timeout of the store (5 s). It does not migrate. When the schema version of `flats.db` is not the
    one of this binary, it follows rule 17.
19. The other commands that write an event (`flats key set` and `rm`, `restore-key`, `reset`) already open `flats.db`
    under the host lock. They write the event through that connection.

## Reading the log

```text
flats audit ls [--since TIME] [--kind KIND] [--flat SLUG] [--limit N] [--json] [--config PATH | --data DIR]
```

20. The command lists events oldest first. `--since` takes an RFC 3339 time or a duration such as `24h` (that long
    ago). `--kind` takes a kind of the table, `--flat` a flat name. `--limit` defaults to 100 and returns the newest
    events that match, and `--limit 0` returns every match. A flag value that does not parse is a usage mistake with
    status 2.
21. The output is one line for each event with the time, the kind, the flat and name, `via`, `actor` and `data`. With
    `--json` it is one JSON object per line with the fields of rule 1 (`data` as an object, `at` as an RFC 3339
    time in UTC). The line format is the one that `flats audit export` of #225 extends.
22. The command opens `flats.db` read-only on a connection that sees the write-ahead log, takes no host lock, never
    calls the host and writes nothing. It works with the host running and with the host stopped. It refuses, with
    status 78, a `flats.db` whose schema version is not the one of this binary, and a data directory without
    `flats.db`, and the message says to start the host once so that it migrates.
23. No MCP tool, no route of `/api` or `/console/api` and no console view reads or writes `audit_events`. A test
    lists the routes and the tools.

## Compatibility

24. The migration to version 25 creates the table and its indexes in the transaction of the version bump, after the
    copy of `flats.db` in `<data>/backups` that every migration makes. An older Flats refuses a `flats.db` of schema
    25 (`ErrSchemaTooNew`). To go back, the operator restores a copy from before the migration. The docs say to
    back up before the upgrade.
25. `Inspect` ignores `audit_events` when it decides whether a database holds data, as it ignores `host_binding`,
    `autopilot` and `public_instance`. A host with a bound, otherwise empty `flats.db` stays empty after an event of
    `restore-key`.
26. `flats backup` copies the table with the rest of `flats.db`. `flats backup --verify` refuses a copy of another
    schema version, as it does today, so a copy from before this change needs the migration first.
27. An existing flat, secret or key is unchanged. No event is written for what happened before the migration.

## Completion criteria

Tests that need a file mode, a read-only folder or a link run on Unix as a user that is not the superuser, and skip
elsewhere.

- Migration. A `flats.db` of version 24 migrates to 25 and the table has the columns and indexes of rule 1. The
  migration tests that pin the latest version change with it. A database with only audit rows is not "data"
  (rule 25). An older reader refuses version 25.
- Secrets. A set of a new and of an existing secret through the console and through the CLI header writes `secret.set`
  with `created` true and false, the right `via` and, for the console, an `actor`. A set that fails validation, and a
  delete of a missing name, write nothing. A delete writes `secret.delete`. A test stores a distinctive value and shows
  that no audit row and no column of `audit_events` holds it.
- Flat deletion and rename. After the deletion of a flat with two secrets, the log holds two `secret.delete` events with
  `via` `flat-delete` and the flat's name, and the flat's own events are gone. A rename leaves the rows under the old
  name. A trimming of the flat's log to a small `system.events_keep` never removes an audit row.
- Operator keys. The console sets, replaces and removes the Tailscale and NetBird keys, and a zrok token is stored and
  consumed. `flats key set` and `rm` write the events with `via` `command` and the operating-system user. A start input
  stores a key once with the right `source` and `via` `start`, and writes nothing when a key of that kind is stored
  already. The import of a legacy key writes `key.set` with `via` `migration`. A record that holds no value writes
  nothing.
- Key commands. `backup-key` while the host runs appends `key.backup` with the key ID and `out`, takes no lock, and a
  host write that runs at the same time still succeeds. `restore-key` writes `key.restore` with `opened` and `total`,
  also with `--out`. `reset` writes one `key.reset` and one event for each value it deleted, in one transaction. When
  the schema of `flats.db` is another one, `backup-key` writes the copy and exits with status 1 and the message of
  rule 17.
- Atomicity. With an injected failure of the audit write, a set, a delete, a key store, a key removal, a reset and the
  deletion of a flat each leave the old state (the old value, the flat and its secrets) and return the error.
- `flats audit ls`. The filters, the limit (default, a number, 0), both time forms, the JSON lines, the empty result
  and the exit statuses 2 and 78. It works with a host that runs and writes at the same moment, and it sees a commit
  that sits in the write-ahead log. It writes nothing: the data directory is the same before and after.
- Agents. A test lists the MCP tools and the routes and finds none that reads the log. The agent guide
  (`docs/agent/`) needs no change, and a test of the guide still passes.
- Docs. `docs/configuration.md` (the list of what `flats.db` holds, a section on the audit log with the kinds, the fields
  and the upgrade and downgrade rule), `docs/design.md` ("Secrets" and the events), `docs/surfaces.md` (the CLI table),
  and the spec index say what rules 1 to 27 say, and the documentation checks pass.

## Open questions

None.
