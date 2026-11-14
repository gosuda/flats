# Audit events

Status: implemented.

## Summary

Flats records secret operations only in the log of a flat, and that log is trimmed to its newest events and deleted
with the flat. Operator keys and the host key have no record at all. This spec adds a host-level audit log in
`flats.db`. It is append-only, it is not trimmed, and the deletion or rename of a flat does not change it. Every
change of a sealed value (a flat secret or an operator key value), and every key backup, key restore and reset, writes
one event: what happened, to which flat and name, when, through which channel, and by whom when the channel knows.
The value is never recorded. `flats audit ls` reads the log.

The request is item 4 of issue #224. Item 2 (rotation) writes its own event later. The webhook and `flats audit
export` of #225 build on this log.

## Before this change

- The `events` table (`internal/store/store.go`) holds the log of each flat. `events.flat` has no foreign key.
  `DeleteFlat` runs `DELETE FROM events WHERE flat=?`, and a rename runs `UPDATE events SET flat=?`. `Service.Event`
  trims the log of a flat to `system.events_keep` (5000) events after every 200 new events of that flat. The runtime
  log lines of a flat share the table, so a noisy flat pushes out its own older events.
- `SetSecret` and `DeleteSecret` (`internal/core/secrets.go`) write the flat event `secret NAME set via VIA; redeploy to
  apply` (or `deleted via`) after the change and outside its transaction. The event does not tell a new secret from a
  replaced one, and a delete of a missing name records nothing. The secrets of a flat disappear without a trace when the
  flat is deleted, because `secrets` cascades.
- `flats secret reset` writes one flat event for each flat secret it deletes and nothing for operator keys, in the
  transaction of `DeleteSealedValues`. `backup-key` and `restore-key` write nothing.
- Operator keys have no event. The console routes (`internal/api/api.go`, implemented by `KeyAdmin` in
  `internal/app/keys.go`), `flats key set` and `rm` (`internal/app/keycmd.go`), the start-time inputs
  (`resolveStoredKey`), the import of a legacy key (`internal/app/setup.go`) and the zrok enrolment write a line of the
  host log at most. That log is stderr or a file that is not queryable and that a backup leaves out.
- The recovery spec ([secret key recovery](secret-key-recovery.md)) says that `backup-key` does not open `flats.db`
  (rule 12), lists the writes that a command can leave (rule 11), and puts audit records out of scope.
- No reader lists the events of a deleted flat or of no flat. The readers (`GET /flats/{slug}/logs`, the MCP tool
  `get_logs`, the console, `flats logs`) all need the flat to exist.
- `flats.db` has schema version 24 (`internal/store/migrate.go`). `Inspect` ignores the tables `host_binding`,
  `autopilot` and `public_instance`, which always hold a row, when it decides whether a database holds data. A row of
  `autopilot_changes`, the change log of the Experiments settings, counts as data (`TestPublicInstanceHasData`).
- A flat has a slug, a display name and an `ID` (`store.Flat.ID`) that is random, set when the flat is created, kept by
  renames, and different for a flat that is created again under a deleted slug.
- Approvals survive the deletion of a flat and record `decided_by` and `via`. Decisions on the loopback listener have
  no identity, and #139 covers that part.

## Scope

In scope: the table and its migration, the events of rules 8 to 15 and where each is written, the command
`flats audit ls`, the changes to the recovery spec that rule 29 lists, the docs.

Out of scope:

- The webhook, `flats audit export` and the approval records in an export (#225).
- The writer of the rotation event. This spec reserves the kind `key.rotate` (rule 15). The rotation spec gives its
  fields and writes it.
- A console view, an MCP tool or an API route for the log. Agents cannot read it.
- Tamper evidence (a hash chain or a signature), a way to trim the log, and shipping it off the host. A person who can
  write `flats.db` can edit the log. Shipping the events off the host is the purpose of #225.
- Events for the decryption of secrets at an activation or at a console use of an operator key, for the check of a key
  against the stored values, for `flats backup` reading the key, for a change of `host.secret_key_path`, and for
  changes of `config.json`. They change no sealed value.
- A downgrade path that keeps data (rule 25).
- Changes to the per-flat events, to `system.events_keep` or to what the log of a flat shows.

## The log

1. `audit_events` is a new table of `flats.db`, created by the migration to schema version 25. A row has `id` (an
   autoincrement integer, which is the order), `at` (milliseconds since the Unix epoch, as `store.unix` gives for the
   other tables, from the clock of the host), `kind`, `flat` (the slug of the flat when the event was written, or
   empty), `flat_id` (the `ID` of that flat, or empty), `name` (the secret name or the operator key name, or empty),
   `via`, `actor` (empty when unknown) and `data` (a JSON object, or empty). The table has no foreign key and no
   trigger. Indexes on `(kind, id)`, `(flat, id)` and `(flat_id, id)` serve the filters of `flats audit ls`.
2. The code only appends. No function updates or deletes an audit row. That holds for retention, for `Sweep`, for
   `DeleteFlat`, for a rename, for the restore of the data of a flat and for `flats secret reset`. The deletion of a
   flat leaves its rows. A rename leaves them under the old slug and the same `flat_id`. A restore of the host from
   `flats backup` replaces `flats.db` and so drops the rows that were written after the copy; the docs say so. A person
   who can write `flats.db` can still change the rows, as "Scope" says.
3. No row holds a secret value, an operator key value, key bytes, a recovery password or the recovery copy. A name is
   not a value. `data` holds only the fields that rules 8 to 15 name. The key ID of rules 12 and 13 is the one that
   [secret key recovery](secret-key-recovery.md) defines. It reveals nothing that opens the key.
4. `via` says which channel made the change. It is one of
   - `console`: a route of the console API (the web console);
   - `cli`: `flats secret set` or `rm` through the loopback API with the header `X-Flats-Client: cli`;
   - `command`: a command that the operator ran on the host and that works on the data directory
     (`flats key set` and `rm`, `flats secret backup-key`, `restore-key` and `reset`);
   - `start`: an input that the host read at its start (a key file flag, a `FLATS_*` variable, or the deprecated
     `credentials.tailscale_authkey_file`), and the zrok enrolment and clean-up that the host runs after its start;
   - `migration`: the import of a legacy key file when an older installation moves to a configuration file;
   - `flat-delete`: the deletion of a flat removed its secrets.
5. `actor` is the identity that the channel knows. For `console` it is the actor text that `consoleActor`
   (`internal/api/experiments.go`) derives from the request: a tailnet user or node, a NetBird peer, the external
   origin, or the socket address of the caller. For `command` it is the name of the operating-system user that ran the
   command, or empty when the system gives none. Under `sudo` that user is `root`, and the docs say so. For `cli`,
   `start`, `migration` and `flat-delete` it is empty: the `cli` header is a client's claim and not an identity
   ([design](../design.md), "Secrets"), and the others have no person. #139 may fill `actor` for `cli` later. The log
   holds these identities and is never trimmed. The docs say so, because #225 ships the log off the host.
6. A row never changes the meaning of an existing per-flat event. The events `secret NAME set via VIA` and the events of
   `flats secret reset` stay as they are.
7. The log has no limit and no trimming in this change. A set or delete of a secret is an action of the operator, so
   the log grows with what the operator does. `actor` is cut at 512 bytes, on a character boundary, before it is
   stored. `data` holds at most 4 KiB. The fields of rules 8 to 15 are far below that, so no event fails for its size.

## What each event records

| Kind | `flat` | `name` | `data` | `via` |
| --- | --- | --- | --- | --- |
| `secret.set` (8) | the flat | the secret | `{"created": true or false}` | `console`, `cli` |
| `secret.delete` (9) | the flat | the secret | `{"approval": ID}` for `flat-delete` when an approval deleted the flat, else none | `console`, `cli`, `command` (reset), `flat-delete` |
| `key.set` (10) | none | `tailscale`, `netbird` or `zrok` | `{"replaced": true or false, "from": "flag", "env" or "config"}`, with `from` only for `start` and `migration` | `console`, `command`, `start`, `migration` |
| `key.delete` (11) | none | the key | `{"reason": "removed" or "connected"}` | `console`, `command`, `start` |
| `key.backup` (12) | none | none | `{"key_id": ..., "out": true or false}` | `command` |
| `key.restore` (13) | none | none | `{"key_id": ..., "out": true or false, "opened": N, "total": M}` | `command` |
| `key.reset` (14) | none | none | `{"secrets": N, "keys": M}` | `command` |
| `key.rotate` (15) | none | none | reserved | reserved |

8. `secret.set` is written for each successful set of a flat secret, whether the name is new (`created` true) or
   replaced. A set that fails validation or the name check changes nothing and writes nothing. `flat` and `flat_id`
   are those of the flat that the secret belongs to.
9. `secret.delete` is written for each secret that is removed: by `DeleteSecret` (a delete of a name that does not
   exist changes nothing and writes nothing), by `flats secret reset`, and by the deletion of a flat, one event for each
   secret the flat held, with `via` `flat-delete`. The names are read in the transaction that deletes the flat. When an
   approval deleted the flat, `data` carries its ID (`applyDelete` has it), so that #225 can join the decision to the
   deletion.
10. `key.set` is written whenever a sealed operator key value is stored or replaced. `from` is the kind of input that a `start`
    or `migration` event came from (`flag`, `env` or `config`, the `source` of the key input), not the name of the
    flag or variable. A start input that is ignored because a key of that kind is stored already (the
    seed rule of [operator keys](../configuration.md#operator-keys)) changes nothing and writes nothing.
11. `key.delete` is written whenever a sealed operator key value is removed. The paths are
    - the console routes and `flats key rm` (`reason` `removed`, `via` `console` and `command`);
    - `flats secret reset` (`reason` `removed`, `via` `command`);
    - the consumption of a stored zrok token, which the host connects the account with, and which `recordConnection`
      replaces by a record without a value, `clearConnected` removes when the account is connected already, and
      `forgetConnection` removes with the record of a disconnected account (`reason` `connected` for the first two and
      `removed` for the last). Their `via` is `console` when the console connects or disconnects the account, and
      `start` for the enrolment and the clean-up after a start.
    A record that holds no value writes nothing when it is created or removed, such as the record of a connected zrok
    account. A token that a person types in the console and that is never stored writes nothing, because no sealed
    value changes.
12. `key.backup` is written when `flats secret backup-key` has written the recovery copy and its ID file. `key_id` is
    the ID of the key in the copy. `out` says that `--out` wrote a second copy. The path is not recorded.
13. `key.restore` is written when `flats secret restore-key` has written the key to the key location or, with `--out`,
    to the new file. `key_id` is the ID of the restored key, and `opened` and `total` are the numbers of sealed values
    that the key opened and that `flats.db` holds.
14. `key.reset` is written by `flats secret reset` once, in the transaction that deletes the values, together with one
    `secret.delete` or `key.delete` for each value (rules 9 and 11). `secrets` and `keys` count them.
15. `key.rotate` is reserved for `flats secret rotate-key`. This change writes it nowhere. The readers accept the kind and
    show its fields as they are.

## When an event is written

16. A change of `flats.db` and its event are one SQLite transaction: a set or delete of a flat secret, a store or removal
    of an operator key value, the deletion of a flat (with the `secret.delete` events of rule 9), and `flats secret
    reset`. When the event cannot be written, the change does not happen and the operation returns the error. There is
    no change of a sealed value without an event. The consumption of a zrok token (rule 11) has a change outside
    `flats.db`, the connected account, and that is not undone. Its row change and its event are one transaction too.
    When the transaction fails, the host logs the failure, the stored token stays, and the next start removes it with
    its event (`clearConnected`).
17. `key.backup` and `key.restore` change no row. The command does its work and then appends the event. When the append
    fails, the command exits with status 1, says that the work was done and the event was not recorded, and names the
    file it wrote. It does not undo the file.
18. `flats secret backup-key` works while the host runs and takes no host lock (rule 12 of
    [secret key recovery](secret-key-recovery.md)). It appends its event with a write of its own to `flats.db`: one
    `BEGIN IMMEDIATE` transaction that reads `user_version`, checks that it is the one of this binary, and inserts the
    row, with the busy timeout of the store (5 s). It does not migrate, and its open does not create `flats.db`. When the
    database is missing, the version is another one, or the transaction cannot start, it follows rule 17. The write creates `flats.db-wal` and `-shm` when a stopped host left
    none, so the docs say to run the command as the user that runs the host, as for `flats key`, `restore-key` and
    `reset`.
19. The other commands that write an event (`flats key set` and `rm`, `restore-key`, `reset`) already open `flats.db`
    under the host lock. They write the event through that connection.

## Reading the log

```text
flats audit ls [--since TIME] [--kind KIND] [--flat SLUG] [--flat-id ID] [--limit N] [--json] [--config PATH | --data DIR]
```

20. The command lists events oldest first. `--since` takes an RFC 3339 time or a duration such as `24h` (that long
    ago) and compares it with `at`. `--kind` takes a kind of the table. `--flat-id` selects the events of one flat by
    its ID. `--flat SLUG` selects the events of the flat that has that slug now, by its ID, so a rename does not split
    its history. When no flat has the slug, it selects the rows that were written under that slug, which is how the
    events of a deleted flat are found. That fallback can return the rows of more than one flat, when a slug was used by
    two flats that were both deleted, and `--flat-id` separates them. A flat that exists under a slug that an older flat
    used has another ID, and `--flat SLUG` does not return the older flat's rows. `--kind` is free text that is compared
    with the column: a kind that no row has gives an empty result. `--limit` defaults to 100 and returns the newest events that match, and
    `--limit 0` returns every match. A flag value that does not parse is a usage mistake with status 2.
21. The text output is one line for each event with the time, the kind, the slug and name, `via`, `actor` and `data`.
    With `--json` it is one JSON object per line (JSON lines) with the fields of rule 1 (`data` as an object, `at` as an
    RFC 3339 time in UTC with milliseconds), which is the format that `flats audit export` of #225 extends. The ID of a
    flat is shown to the operator here, as it is in no other output.
22. The command opens `flats.db` read-only, takes no host lock, never calls the host and writes no row. The open is
    never immutable while a host may write: it is a plain read-only open, which sees the write-ahead log, whenever a
    `flats.db-wal` or `-journal` file exists, whatever its size (a host that started and has not written yet has a
    `-wal` file of 0 bytes), and it is immutable only when neither file exists. The store's `openReadOnly` decides by
    the size of the file, so the implementation adds the condition or an open of its own. The open creates no file when
    the host runs or when the host stopped cleanly. A stopped host that left a `-wal` file and no `-shm` makes the open
    create an empty `-shm`, as for `flats backup` (rule 11 of [host backup](host-backup.md)); that needs a writable
    folder, and the command stops with status 1 and the error when it has none. The command reads the log in pages of at most 500 rows
    by `id`, each in its own short read transaction, and writes a page to its output between two transactions. It holds
    no transaction while it waits for a slow reader of its output, so the checkpoints of the host (`Sync`) are not
    blocked.
23. The command refuses, with status 78, a data directory without `flats.db`, a `flats.db` of an older schema ("start the
    host once with this Flats so that it migrates it") and a `flats.db` of a newer schema ("this Flats is older than the
    database; use the release that wrote it").
24. No MCP tool, no route of `/api` or `/console/api` and no console view reads or writes `audit_events`. The
    implementation registers no route and no tool with `audit` in its name.

## Compatibility

25. The migration to version 25 creates the table and its indexes in the transaction of the version bump, after the
    copy of `flats.db` in `<data>/backups` that a migration of an existing database makes. An older Flats refuses a
    `flats.db` of schema 25 (`ErrSchemaTooNew`). Going back to an older Flats needs the data directory of the time before
    the upgrade. The copy in `<data>/backups` holds only `flats.db`, only the newest three such copies are kept, and it does
    not hold what changed in `flats/<slug>/` since, so restoring it loses everything that happened after the upgrade and leaves the
    files of the flats newer than the database. No supported path keeps that data. This holds for every earlier schema
    migration too. The docs say to back up the whole data directory before an upgrade (`flats backup` of the old
    release, or a copy of the stopped host) and to restore that backup to go back.
26. `Inspect` counts `audit_events` like any other table: a database with an audit row holds data, as a database with a
    row of `autopilot_changes` does. A bound, otherwise empty `flats.db` that `restore-key` wrote an event to is then
    one that the bootstrap and migration code (`setup.go`, `configcmd.go`) treats as holding data, which is the
    conservative reading (it refuses to overwrite).
27. `flats backup` copies the table with the rest of `flats.db`. `flats backup --verify` refuses a copy of another
    schema version, as it does today, so a copy from before this change needs the migration first.
28. An existing flat, secret or key is unchanged. No event is written for what happened before the migration.
29. The change amends [secret key recovery](secret-key-recovery.md): the scope note on audit
    records (they are in scope now), rule 11 (a second write that a command can leave, the append of `backup-key`), rule
    12 (`backup-key` opens `flats.db` for one append after its work, and does not otherwise), and rule 30 (the
    transaction of `reset` also writes the events of rules 9, 11 and 14). The spec index row of this page changes with the
    status.

## Completion criteria

Tests that need a file mode, a read-only folder or a link run on Unix as a user that is not the superuser, and skip
elsewhere. The audit write fails in a test through a `BEFORE INSERT` trigger on `audit_events` that raises an error,
which needs no seam in the product code. A second process that writes to a live database is a second `*sql.DB` of the
test, as the host tests of `internal/app` already do. The migration fixture is a database built by the registry up to
version 24, as `TestHistoricalFixturesMigrateToLatest` does. The zrok calls that the enrolment path makes (`zrok.Describe`,
`Connect`, `New` and `Adopt` in `internal/app/providers.go`) need a seam, function variables that a test replaces,
because no test builds a zrok account. The seam must let a test drive both the consume through `recordConnection`
(no account is connected yet) and the clean-up through `clearConnected` (`Describe` reports a connected account).

- Migration. A `flats.db` of version 24 migrates to 25, and the table has the columns and indexes of rule 1 and the
  `at` unit of the other tables. The tests that pin the latest version change with it. A database with an audit row
  holds data (rule 26). A reader of version 24 refuses version 25 (rule 25).
- Secrets. A set of a new and of an existing secret through the console and through the CLI header writes `secret.set`
  with `created` true and false, the right `via`, the slug and the `flat_id`, and for the console an `actor`. A set that
  fails validation, and a delete of a missing name, write nothing. A delete writes `secret.delete`. A test stores a
  distinctive value and shows that no column of any `audit_events` row holds it. An `actor` of 600 bytes is stored in
  512 (rule 7).
- Flat deletion and rename. After the deletion of a flat with two secrets, the log holds two `secret.delete` events with
  `via` `flat-delete`, the slug and the ID, and the events of the flat's own log are gone. With an approval the events
  carry its ID. A rename leaves the rows under the old slug and the same `flat_id`, and `flats audit ls --flat NEWSLUG`
  finds the history from before the rename. A flat created again under a deleted slug is not mixed with the old rows,
  and `--flat OLDSLUG` finds the rows of a deleted flat. A trimming of the log of the flat to a small `system.events_keep`
  and a `Sweep` never remove an audit row. A restore of a flat's data leaves the rows.
- Operator keys. The console sets, replaces and removes the Tailscale and NetBird keys. `flats key set` and `rm` write
  the events with `via` `command` and the operating-system user. A start input stores a key once, with `from` and `via`
  `start`, and writes nothing when a key of that kind is stored already. The import of a legacy key writes `key.set` with
  `via` `migration`. `flats key set zrok` stores a waiting token, and the enrolment at the next start, with the seam
  of the connect call, consumes it and writes `key.delete` with `reason` `connected` and `via` `start`. A console
  connect with a waiting token stored does the same with `via` `console`. A disconnect that removes a record with a
  value writes `key.delete` with `reason` `removed`, and a record without a value writes nothing. With an injected failure
  of the audit write, a consume leaves the token stored, logs the failure and the next start removes it with its event.
- Key commands. `backup-key` while the host runs appends `key.backup` with the key ID and `out`, takes no host lock, and
  a host write at the same moment still succeeds. When the schema version is another one, it writes the copy, exits with
  status 1 and gives the message of rule 17. `restore-key` writes `key.restore` with `opened` and `total`, also with
  `--out`. `reset` writes one `key.reset` and one event for each value it deleted, in one transaction.
- Atomicity. With an injected failure of the audit write, a set, a delete, a key store, a key removal, a reset and the
  deletion of a flat each leave the old state (the old value, the flat and its secrets) and return the error.
- `flats audit ls`. The filters (`--since` in both forms, `--kind`, `--flat`, `--flat-id`), the limit (default, a number,
  0), the text and the JSON lines, the empty result, and the exit statuses 2 and 78 with the three messages of
  rule 23. It works with a host that runs and writes at the same moment, and it sees a commit that sits in the
  write-ahead log. For a host that runs, the set of entries of the data directory and the bytes of every file other than
  `flats.db-wal` and `-shm` are the same before and after, because the host and the reader may change those two. For a
  host that stopped cleanly, every entry and every byte is the same. A `-wal` file of 0 bytes does not make the open
  immutable (rule 22): a commit that the host writes during a listing shows in a later page. `--kind key.rotate` lists
  a row of that kind that the test inserted by hand. For a stopped host that left a `-wal` file and no `-shm`, only an empty `-shm` appears, and an unwritable folder
  gives status 1. With `--limit 0` and an output that is stalled, a host `Sync` succeeds (rule 22).
- Agents. The test starts a host, writes audit events, and checks that `GET /flats/{slug}/logs` and the MCP `get_logs` of an
  existing flat return none of the audit rows, that the MCP tool list has no tool with `audit` in its name, and that
  `/api/audit` and `/console/api/audit` answer 404. The agent guide (`docs/agent/`) needs no change, and its tests still
  pass.
- Spec amendments. The changed rules of the recovery spec (rule 29) and the index row are in the same change, and
  `doccheck` passes.
- Docs. `docs/configuration.md` (the list of what `flats.db` holds, a section on the audit log with the kinds, the
  fields, the identity text and its privacy, the host restore and the upgrade and downgrade rule of rule 25),
  `docs/design.md` ("Secrets" and the events), and `docs/surfaces.md` (the CLI table) say what rules 1 to 29 say, and
  the documentation checks pass.

## Open questions

None.
