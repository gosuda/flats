# Secret key rotation

Status: planned.

## Summary

Flats seals every flat secret and every stored operator key under one 32-byte key, the host key. Nothing changes that
key after its creation, so a key that may have leaked stays in use. This spec adds `flats secret rotate-key`. The
command makes a new key and seals every stored value again under it, in one transaction. It also gives each sealed
value a key ID, which says which key sealed it. A stop of the command at any point leaves every value readable: a file
on the host always holds the key that opens the values, and the start says what to do.

The request is item 2 of issue #224.

## Before this change

- The host key is created once: at a start, or when `flats key set` stores the first operator key, and only at the
  default path ([secret key recovery](secret-key-recovery.md), "Before this change"). No command replaces it.
- A sealed value is a nonce and a ciphertext (AES-256-GCM). The associated data is the name of the secret, or
  `host-key`, a zero byte and the key name for an operator key. Nothing in `flats.db` says which key sealed a value.
- The start checks that the key exists when `flats.db` holds sealed values (`CheckSecretKey`). It does not check that
  the key opens them: a wrong key shows when a value is used ([secret key path](secret-key-path.md), "Scope").
- The recovery copy holds the key of the time of `flats secret backup-key`. The start warns when the copy does not
  match the key ([secret key recovery](secret-key-recovery.md), rule 5).
- The audit log reserves the kind `key.rotate` and nothing writes it ([audit events](audit-events.md), rule 15).
- `flats backup` reads the key two times and fails when the reads differ, and its check fails when the key in the copy
  does not open the sealed values ([host backup](host-backup.md), rules 22 and 28).

## Scope

In scope: the key ID on each sealed value, the command, the pending key file, what the start and the other key
commands do in the states that the command can leave, the recovery copy, the audit events, `flats backup`, the docs.

Out of scope:

- A rotation while the host runs. The running host holds the key in memory and gives the opened values to its
  workers. The command needs the stopped host, as `flats secret restore-key` and `reset` do.
- More than one key in use. After a rotation one key opens every value. The old key opens nothing in `flats.db`.
- A copy of the old key. The command does not keep it. The docs say when the operator needs it (rule 37).
- A schedule. The operator runs the command.
- A change of the values. A rotation changes how a value is sealed, and not the value, so no flat needs a new
  deployment.
- Injected `fetch` secrets (#224 item 3).
- The console, the API and MCP. No route and no agent tool reaches the command or shows a key ID.

## Decisions

The issue leaves four points open. The owner of the repository decided them on 2026-11-16:

1. The host is stopped during a rotation. Every other command that writes the key or the sealed values without the
   host needs the stopped host, and a swap of the key under a running host has no defined effect.
2. The start does not finish a rotation that stopped. It refuses and names the command, which finishes the rotation.
   The start never replaces a key file.
3. The command does not keep the old key. The docs and the output say when the operator needs a copy of it (rules
   29 and 37).
4. With a key that Flats cannot replace where it is (a read-only secret store, or a link), the command writes the new
   key to a new file that the operator names, as `flats secret restore-key --out` does.

One more point follows from the issue. Item 5 of the issue requires that the recovery password opens the new key
after a rotation, so the command asks for that password when a recovery copy exists (rule 20). The flag
`--skip-recovery-copy` is for an operator who lost the password.

## Key IDs

1. Each flat secret and each operator key record that holds a value has a key ID: the ID of the key that sealed it,
   computed as rule 2 of [secret key recovery](secret-key-recovery.md) says (32 lowercase hex digits). The ID is
   empty for a value that was sealed before this change, and for an operator key record without a value.
2. Each write of a sealed value sets the key ID in the same row write: a set of a flat secret, each store of an
   operator key value (`flats key set`, the console, an input of the start), and the rotation.
3. The key ID is a label. It is not part of the associated data, and the host does not use it to decide whether a
   value opens: it tries the key. When a value does not open and its key ID is not empty and differs from the ID of
   the key, the error names both IDs. Rule 22 is the only rule in which the labels decide something.
4. The audit log is the only place that shows a key ID to a reader (rules 30 and 31, and the `key.backup` and
   `key.restore` events). `flats key ls`, the console, the API and MCP do not show one.

## The command

```text
flats secret rotate-key [--out FILE] [--password-file PATH] [--skip-recovery-copy] [--config PATH | --data DIR]
```

5. Rules 7 to 11 of [secret key recovery](secret-key-recovery.md) apply: the selection of the configuration, the
   sources of a password, the exit codes, what a refusal can write, and an interrupt. The password of this command is
   the existing recovery password, so rule 14 of that spec (the length of a new password) does not apply.
6. The command takes the host lock like `flats key`, refuses while a host runs, and checks that `flats.db` belongs to
   the configuration.
7. The command looks for a pending key first (rule 17). When a pending key waits to become the host key, the command
   finishes that rotation (rule 19) and rules 8 to 16 do not apply.
8. The key path must hold a valid key. The command makes the checks of rule 3 of [secret key path](secret-key-path.md)
   for a configured path. When nothing is at the key path, it refuses and says that there is no key to rotate: the
   next start creates a key at the default path, and a configured path needs a key from the operator. Anything at the
   path that is not a valid key is refused with the advice to move it away, as rule 13 of
   [secret key recovery](secret-key-recovery.md) says. Another failure to read the key stops the command with that
   error, status 1.
9. The key must open every sealed value in `flats.db`: every flat secret and every operator key record with a value.
   When it does not, the command refuses, says how many of how many values open, and lists the values that do not
   open, operator keys apart from flat secrets, without a value. A rotation would seal the other values under a new
   key and leave those under a key that no file holds. The message names `flats secret restore-key` for the right key,
   and `flats key rm` and `flats secret reset` for values that are lost. When the values that do not open carry a key
   ID, the message names it, and says that the file of an earlier `rotate-key --out` can hold that key. With no sealed
   value there is nothing to check, and the command still makes a new key.
10. The command asks no confirmation. It deletes nothing.

## The new key

11. The new key is 32 bytes from the random source of the operating system.
12. Without `--out`, the command replaces the key where it is. The key path itself must be a regular file. When it is
    a symbolic link, the command refuses and names `--out`, because a replacement would put a file in the place of
    the link and leave the target of the link with the old key.
13. Without `--out`, the command first writes the new key as the pending key: the file `<key path>.next`, which is
    `<data>/secret.key.next` for the default path. It writes through a synced temporary file `.secret-key-*` with
    mode 0600 in the folder of the key, publishes it under the pending name with a link that fails when the name
    exists, and syncs the folder. When it cannot create a file in that folder, it refuses, names `--out`, and has
    changed nothing. On Unix the key file belongs to the user that runs the command, so the command must run as the
    user that runs the host, as for `flats key` and `flats secret restore-key`.
14. With `--out FILE`, the command writes the new key to `FILE` and does not touch the key path, which can be
    read-only. `FILE` passes the checks of `flats secret restore-key --out`: it does not exist, its folder exists, and
    it does not lie inside the data directory, also through a link. The file gets mode 0600 on Unix, is synced, and
    its folder is synced. When `FILE` exists, the command refuses. When that file is a valid key whose ID every
    sealed value carries, the refusal says that the rotation to this key is complete and that the key must be put at
    the key path. Until the operator puts the new key at the key path, the start refuses (rule 22).

## The re-seal

15. One transaction (`BEGIN IMMEDIATE`) reads each sealed value, opens it with the old key, seals it with the new key
    under a new random nonce and the same associated data, and writes the nonce, the ciphertext and the key ID of the
    new key. The time of the last change and the source of an operator key do not change, because the value did not
    change. An operator key record without a value does not change. The transaction also writes the `key.rotate`
    event (rule 30). When one value does not open inside the transaction, the transaction changes nothing and the
    command fails. After a transaction that failed for any reason, the command reads the key IDs of the sealed values.
    It removes the pending key or `FILE` only when it could read them and no value carries the ID of the new key. In
    every other case the file stays, and rule 17 decides at the next run.
16. The steps have this order, and each is durable before the next one starts:
    1. The new key is in the pending key file, or in `FILE`, and that file and its folder are synced.
    2. The transaction of rule 15 commits, and the command makes it durable with a complete checkpoint, as
       `flats secret reset` does. When the checkpoint fails, the command stops with an error and does not replace
       the key. The state is then one of rule 18. Without `--out` the message says to run the command again. With
       `--out` it says that the values are sealed under the key in `FILE`, and that the old key must be kept until
       the host has started with the new key and stopped again, because a loss of power can still take the re-seal
       back.
    3. Without `--out`, the command moves the pending key to the key path, which replaces the old key in one rename
       (with the retry of PR #220 on Windows), and syncs the folder.
    4. The command writes the recovery copy (rule 20).
    An interrupt (Ctrl-C or SIGTERM) before step 1 writes nothing. After step 1 has begun, the command completes
    steps 1 to 3, also when an interrupt arrives.

## A rotation that stopped

17. The pending key is the file `<key path>.next`. The command and the start read it as they read a key. A pending
    key *waits* when it is a valid key and at least one sealed value carries its key ID. A valid pending key whose ID
    no sealed value carries is *left over*: the transaction of rule 15 did not commit, or it committed for a database
    without a sealed value.
18. After a stop at any point (a crash, a kill, a loss of power), the files are in one of these states. In each of
    them one file holds the key that opens every sealed value.

    | The command stopped | Key path | Pending key or `FILE` | Sealed values | The start | The next `rotate-key` |
    | --- | --- | --- | --- | --- | --- |
    | before step 1 ended | old key | none, or a temporary file | old key | starts | a new rotation |
    | after step 1, before the commit of step 2 is durable | old key | new key, left over | old key | starts | removes the pending key (rule 21), then a new rotation |
    | after step 2, before step 3 | old key | new key, waits | new key | refuses (rule 22) | finishes (rule 19) |
    | after step 3 | new key | none | new key | starts, and warns while the recovery copy holds the old key | a new rotation |
    | with `--out`, before the commit is durable | old key | `FILE` holds a key that seals nothing | old key | starts | refuses `FILE` because it exists; the operator removes it |
    | with `--out`, after step 2 | old key | `FILE` holds the new key | new key | refuses (rule 22) until the new key is at the key path | refuses (rule 9), and names the key ID of the values |

    A loss of power between the commit and its checkpoint can take the commit back. The state is then the one of the
    second or the fifth row, because step 3 did not start.
19. When a pending key waits, the command finishes that rotation and makes no new key:
    - `--out` is a refusal that says to run the command without it.
    - The pending key must open every sealed value. When it does not, the command refuses with the numbers of rule 9
      and writes nothing.
    - The checks of rule 3 of [secret key path](secret-key-path.md) apply to a configured path. The key path holds
      the old key, or nothing. When it holds a symbolic link, or something that is no regular file, the command
      refuses and says to put the pending key where the host reads its key.
    - The command asks for the recovery password as rule 20 says. It then makes `flats.db` durable with a complete
      checkpoint, as step 2 of rule 16 does, and stops without a change when that fails. It then moves the pending
      key to the key path (step 3 of rule 16) and writes the recovery copy.
    - It writes no second `key.rotate` event. The output says that it finished a rotation that had stopped.
20. The recovery copy. When `<data>/secret.key.recovery` exists and `--skip-recovery-copy` is not given, the command
    keeps the copy current:
    - Before its first write, it reads the copy (at most 64 KiB), asks for the recovery password once, and opens the
      copy with it by rule 21 of [secret key recovery](secret-key-recovery.md). A wrong password, or a copy that
      does not open, is a refusal that writes nothing. The key in the copy does not have to be the host key: a copy
      that was out of date is replaced too.
    - Before its first write, it also encrypts the new key under the same password, at the work factor of
      `flats secret backup-key`.
    - After step 3 of rule 16 (with `--out`: after step 2), it writes the copy and then its key ID as rule 15 of that
      spec says, and appends the `key.backup` event (rule 31).
    - When that write fails, the rotation is complete. The command exits with status 1, and says that the copy holds
      the old key and that `flats secret backup-key` writes a current one. The start warns until then.
    Without a terminal and without `--password-file`, the command stops with a usage error that names
    `--password-file` and `--skip-recovery-copy`, and writes nothing. With `--skip-recovery-copy` the command asks no
    password and leaves the copy as it is: the output says that the copy holds the old key, and the start warns.
    `--password-file` together with `--skip-recovery-copy`, or without a recovery copy in the data directory, is a
    usage error. Without a recovery copy the command creates none, and the output names `flats secret backup-key`.
    A copy that the operator keeps apart from the host (`backup-key --out`) is not changed. It holds the old key.
21. When a pending key is left over, the command removes it after rules 8 and 9 passed, says so, and goes on with a
    new rotation, with or without `--out`. A file at the pending name that is not a valid key is a refusal with the
    advice to move it away, because Flats did not write it: the command publishes a pending key complete.

## The start and the other commands

22. The start refuses when the key path holds a valid key, `flats.db` holds sealed values, each of them carries a key
    ID, and none carries the ID of that key. A host in that state would seal new values under a key that does not
    open the others. The refusal names the ID of the key, the key IDs of the values, and the way out:
    - when a pending key waits: run `flats secret rotate-key`, which finishes the rotation;
    - else: put the key with the ID of the values at the key path (the file that `rotate-key --out` wrote), or
      restore it with `flats secret restore-key`, or, with no such key left, delete the stored values with
      `flats secret reset`.
    `flats key set` refuses in the same state with the same message. Every other state starts as before this change:
    a database with a value that has no key ID, or with a value that carries the ID of the key, has no check, and a
    value that the key does not open fails when it is used.
23. The start never moves, writes or removes a pending key. It reads the pending key only to choose the message of
    rule 22. A pending key that is left over does not change a start. When the key path holds nothing and a pending
    key waits, the refusal of the missing key names `flats secret rotate-key` first, for the default path
    ([secret key recovery](secret-key-recovery.md), rule 32) and for a configured path.
24. `flats secret reset` refuses when a pending key waits, and names `flats secret rotate-key`: that key opens the
    values, so nothing is lost. In every other state the command works as before. When it goes on because the key
    opens none of the values, and every value carries one key ID, the output names that ID and says that the key with
    that ID opens them.
25. `flats secret restore-key` and `flats secret backup-key` do not change. A recovery copy that `backup-key` writes
    while a pending key waits holds the old key, and the finish of the rotation replaces it by rule 20.
26. `flats backup` never copies `<data>/secret.key.next`. The manifest lists it in `excluded`, with and without
    `--exclude-secret-key` ([host backup](host-backup.md), rule 3). A copy that is made while a pending key waits and
    that holds `secret.key` fails its own check, because that key does not open the sealed values (rule 28 of that
    spec). A replacement of the key during a copy fails the copy by rule 22 of that spec.
27. Bootstrapping a new host treats `secret.key.next` as Flats data, like `secret.key`
    ([secret key recovery](secret-key-recovery.md), rule 6).

## What the command says

28. The output of a rotation gives the number of flat secrets and of operator keys that were sealed again, the key ID
    of the old key and of the new key, each file that the command wrote or replaced, and what happened to the
    recovery copy. With `--out` it says that the host does not start until the new key is at the key path, and that
    `FILE` must then be removed. It never shows a key or a value.
29. The output also says that a copy of `flats.db` from before the rotation opens only with the old key.

## Audit

30. `key.rotate` is written by the transaction of rule 15, also when `flats.db` holds no sealed value. It has no flat
    and no name, the `via` is `command`, and `data` is
    `{"from": <ID of the old key>, "to": <ID of the new key>, "secrets": N, "keys": M, "out": true or false}`, where
    N and M count the flat secrets and the operator key values that were sealed again. The event records the re-seal
    in `flats.db`. It does not record the replacement of the key file: after a rotation with `--out`, or one that
    stopped before step 3 of rule 16 for a database without a sealed value, the log can hold a `to` key that never
    became the host key.
31. `key.backup` is also written when the command has written the recovery copy and its ID file (rule 20), with the ID
    of the new key and `"out": false`. A failure to append it follows rule 17 of [audit events](audit-events.md): the
    copy stays, and the error says so.
32. The event webhook sends nothing for these events. `flats audit ls` and `flats audit export` show them as they
    show every audit event.

## Compatibility

33. Schema 27 adds the column `key_id` (text, not null, default empty) to `secrets` and to `host_keys`. An older
    release refuses the database, as for schemas 25 and 26. The migration writes no key ID, because it has no key: a
    value gets its ID when it is sealed the next time.
34. The key file stays 32 bytes, and the recovery copy keeps its format. A value sealed by this change differs from
    an older one only in its label.
35. No new dependency.

## Docs

36. `docs/configuration.md` gets a section on the rotation: when to rotate, the steps for a key in the data
    directory and for a key at `host.secret_key_path`, what a stop of the command leaves and how to go on, and the
    recovery password. "What to back up" and "Recovering a lost secret.key" name the rotation where a key changes.
    `docs/design.md` ("Secrets") describes the key ID and the order of rule 16. `docs/surfaces.md` and the help of
    `flats secret` list the command.
37. The docs say what the old key still opens: a copy of `flats.db` from before the rotation. A copy that
    `flats backup` made with its `secret.key` stays complete. A copy without its key, and a recovery copy that is
    kept apart from the host, need the old key or hold it. The operator copies the old key away before the rotation
    when such copies must stay readable, and runs `flats secret backup-key --out` again after it.

## Changes to other specs

The implementation of this spec changes these implemented specs, and not before:

- [Audit events](audit-events.md): rule 15 and the row of `key.rotate` get the fields of rule 30, rule 12 names rule
  31, and the scope names this spec as the writer.
- [Host backup](host-backup.md): rule 3 lists `secret.key.next`, and the scope names this spec.
- [Secret key recovery](secret-key-recovery.md): the scope names this spec, rule 6 lists `secret.key.next`, rule 27
  gets the refusal of rule 24, and rule 32 gets rule 23.
- [Secret key path](secret-key-path.md): the scope names this spec and the check of rule 22.

## Current state

Nothing of this spec is implemented. `key.rotate` is a reserved kind that nothing writes.

## Completion criteria

- A test rotates a host that holds flat secrets of two flats and operator keys, and then shows that the key file holds
  a new key, that every value opens with it and has its key ID, that no value opens with the old key, that the opened
  values are the same bytes as before, that an operator key record without a value did not change, and that a host
  that starts afterwards gives a flat its secrets.
- Tests stop the rotation after each step of rule 16 and show, for each state of rule 18: which file holds the key
  that opens every value, what the start does, and what the next `rotate-key` does. The tests of the third row show
  that the start and `flats key set` refuse and name the command, that `flats secret reset` refuses, and that the
  finished rotation gives a host that starts and opens every value.
- A test shows that a value that does not open inside the transaction changes nothing: every value opens with the old
  key, no value has a new key ID, and no `key.rotate` event exists.
- A test shows that the key is not replaced when the checkpoint of step 2 fails.
- Tests show each refusal of rules 6, 8, 9, 12, 13, 14, 19, 20 and 21, and that each writes nothing that rule 11 of
  [secret key recovery](secret-key-recovery.md) excludes and leaves no pending key and no `FILE`.
- A test shows that the recovery password opens the new key after a rotation, through `flats secret restore-key`, and
  that the start does not warn. Tests show the wrong password, `--skip-recovery-copy` with its warning at the start,
  the usage errors of rule 20, and a failed write of the copy after a complete rotation.
- Tests show a rotation with `--out`: the key path is not written, `FILE` holds the key that opens every value, the
  start refuses with both key IDs until that key is at the key path, and then starts. One of them uses a key path
  whose folder cannot be written, and one a key path that is a link.
- Tests show rule 22 for its excluded states: a database with a value without a key ID starts with a wrong key as
  before, and so does one with values of two keys of which one is the key at the key path.
- Tests show the `key.rotate` event with its fields, in the transaction of the re-seal, the `key.backup` event of
  rule 31, and that the finish of a stopped rotation writes no second `key.rotate`.
- A test shows that `flats backup` leaves `secret.key.next` out and lists it, and that a copy made while a pending
  key waits fails its check.
- A migration test shows schema 27 on a database of schema 26 with sealed values: the values keep their bytes and
  have an empty key ID, and they open.
- A test shows that a set of a flat secret and a store of an operator key value write the key ID of the host key.
- On the real binary: a host with a flat secret and an operator key, a stop, `rotate-key` with a recovery copy, a
  start whose flat secret opens, and `flats audit ls --kind key.rotate` with the event.
- `docs/configuration.md`, `docs/design.md` and `docs/surfaces.md` describe the command as rules 36 and 37 say, and
  `doccheck` passes.

## Open questions

None.
