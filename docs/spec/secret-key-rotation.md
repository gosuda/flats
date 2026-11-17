# Secret key rotation

Status: planned.

## Summary

Flats seals every flat secret and every stored operator key under one 32-byte key, the host key. No command replaces
that key, so a key that may have leaked stays in use. This spec adds `flats secret rotate-key`. The command makes a
new key and seals every stored value again under it, in one transaction. It also gives each sealed value a key ID,
which says which key sealed it. A stop of the command at any point leaves every value readable: a file on the host
always holds the key that opens the values, and the start says what to do.

The request is item 2 of issue #224.

## Before this change

- Flats creates the host key only at the default path, and only when no file is there: at a start, when `flats key
  set` stores the first operator key, and when the import of a legacy host stores a key. A start after `flats secret
  reset` creates a new key in that way. No command replaces a key that exists.
- A sealed value is a nonce and a ciphertext (AES-256-GCM). The associated data is the name of the secret, or
  `host-key`, a zero byte and the key name for an operator key. Nothing in `flats.db` says which key sealed a value.
- The start checks that the default key exists when `flats.db` holds sealed values, and it reads a configured key in
  full (`CheckSecretKey`). It then opens a stored Tailscale key and a stored NetBird key, and refuses when one does
  not open. It does not check the other values: a wrong key shows when a value is used
  ([secret key path](secret-key-path.md), "Scope").
- The recovery copy holds the key of the time of `flats secret backup-key`. The start warns when the copy does not
  match the key ([secret key recovery](secret-key-recovery.md), rule 5).
- The audit log reserves the kind `key.rotate` and nothing writes it ([audit events](audit-events.md), rule 15).
- `flats backup` reads the key two times and fails when the reads differ, and its check fails when the key in the copy
  does not open the sealed values ([host backup](host-backup.md), rules 22 and 28). With `--exclude-secret-key` it
  reads no key.

## Scope

In scope: the key ID on each sealed value, the command, the pending key file, what the start and the other key
commands do in the states that the command can leave, the recovery copy, the audit events, `flats backup`, the docs.

Out of scope:

- A rotation while the host runs. The running host holds the key in memory and gives the opened values to its
  workers. The command needs the stopped host, as `flats secret restore-key` and `reset` do.
- More than one key in use. After a rotation one key opens every value. The old key opens nothing in `flats.db`.
- A copy of the old key. The command does not keep it. The command and the docs say when the operator needs it (rules
  10 and 37).
- A schedule. The operator runs the command.
- A change of the values. A rotation changes how a value is sealed, and not the value, so no flat needs a new
  deployment.
- A key ID for the values of a database from before this change. The migration has no key, so such a value gets its
  ID when it is sealed the next time (rule 33).
- Injected `fetch` secrets (#224 item 3).
- The console, the API and MCP. No route and no agent tool reaches the command or shows a key ID.

## Decisions

The issue leaves four points open. The owner of the repository decided them on 2026-11-16:

1. The host is stopped during a rotation. Every other command that writes the key or the sealed values without the
   host needs the stopped host, and a swap of the key under a running host has no defined effect.
2. The start does not finish a rotation that stopped. It refuses and names the command, which finishes the rotation.
   The start never writes a key file for a rotation.
3. The command does not keep the old key.
4. With a key that Flats cannot write where it is (a read-only secret store), `rotate-key --out FILE` writes the new
   key to a new file, and the operator puts it in place. This is the pattern of `flats secret restore-key --out`.

The author of this spec decided these points, and each can change before the implementation:

- The command asks for a confirmation on a terminal (rule 10). Decision 3 makes the old key go away, and the
  operator must know that before the step that cannot be reversed.
- No flag rotates without the recovery password. Item 5 of the issue requires that the recovery password opens the
  new key after a rotation, so a recovery copy that exists is always made current (rule 20). An operator who lost the
  password removes the copy first.
- The start also refuses in the second state of rule 22: a valid key that sealed none of the values, while no
  pending key waits. Decision 4 needs that refusal, because a host would else seal new values under the old key
  after a rotation with `--out`. The first state of rule 22 is decision 2 of the owner.
- `flats key set`, `flats secret backup-key` and `flats backup --out` refuse in the two states of rule 22 (rules 22,
  25 and 26).

## Key IDs

1. Each flat secret and each operator key record that holds a value has a key ID: the ID of the key that sealed it,
   computed as rule 2 of [secret key recovery](secret-key-recovery.md) says (32 lowercase hex digits). The ID is
   empty for a value that was sealed before this change. An operator key record without a value has an empty ID,
   also when it replaces a record that held a value.
2. Each write of a sealed value sets the key ID in the same row write. The writers are a set of a flat secret (the
   console and the CLI), each store of an operator key value (`flats key set`, the console, an input of the start,
   the import of a legacy host), and the rotation. Nothing else writes or copies a sealed value: a rename of a flat
   keeps its rows, and no code copies the secrets of a flat. A later migration that builds `host_keys` again must
   copy the key IDs.
3. The key ID is a label. It is not part of the associated data, and the host never uses it to decide whether it
   tries to open a value: it tries the key. When a value does not open and its key ID is not empty and differs from
   the ID of the key, the error names both IDs. The labels decide what the commands and the start do with key files:
   whether a pending key waits (rule 17), and the rules 19, 21, 22, 24, 25 and 26 that follow from it. They also
   choose
   the messages of rules 9, 14 and 23.
4. The operator sees key IDs in the output and the refusals of the key commands and of the start, in the audit log,
   and in `secret.key.recovery.id`. `flats key ls`, the console, the API and MCP show no key ID.

## The command

```text
flats secret rotate-key [--out FILE] [--password-file PATH] [--yes] [--config PATH | --data DIR]
```

5. Rules 7 to 11 of [secret key recovery](secret-key-recovery.md) apply: the selection of the configuration, the
   sources of a password, the exit codes, what a refusal can write, and an interrupt. The password of this command is
   the password of the existing recovery copy. The command uses it as it is, also when it is shorter than rule 14 of
   that spec asks for a new password, because the operator chose it for the copy that exists. Rule 9 of that spec
   (at most 1024 bytes) applies.
6. The command takes the host lock like `flats key`, refuses while a host runs, and checks that `flats.db` belongs to
   the configuration.
7. The command works in this order, and nothing before the last point writes a file or a row:
   1. rule 6;
   2. the pending key and `FILE`: when a rotation that stopped can be finished, the command finishes it (rule 19),
      and the points below do not apply;
   3. the key (rules 8 and 9);
   4. the place of the new key: the checks of rules 12 and 13, or of rule 14 with `--out`;
   5. the recovery copy and its password (rule 20);
   6. the confirmation (rule 10);
   7. the new key (rule 11), and the new recovery copy, encrypted in memory;
   8. the four steps of rule 16.
8. The key path must hold a valid key. The command makes the checks of rule 3 of [secret key path](secret-key-path.md)
   for a configured path. When nothing is at the key path, it refuses:
   - at the default path, without sealed values: there is no key yet, and the next start creates one;
   - at the default path, with sealed values: the message of the start for a missing key
     ([secret key recovery](secret-key-recovery.md), rule 32, and rule 23 below);
   - at a configured path: the message of rule 6 of [secret key path](secret-key-path.md), and a sentence that says
     that a key that only the service process sees (a systemd credential, a Docker secret in the service container)
     is missing for this command, and that the docs give the steps (rule 36). The message does not say that no key
     exists.

   Anything at the path that is not a valid key is refused with the advice to move it away, as rule 13 of
   [secret key recovery](secret-key-recovery.md) says. Another failure to read the key stops the command with that
   error, status 1.
9. The key must open every sealed value in `flats.db`: every flat secret and every operator key record with a value.
   When it does not, the command refuses. A rotation would seal the other values under a new key and leave those
   under a key that no file holds. The message gives the numbers ("opens N of M") and lists the values that do not
   open, operator keys apart from flat secrets, without a value. It names the ways out that work in that state:
   - the right key, with `flats secret restore-key`;
   - when the key opens some values: `flats key rm` for an operator key, and for a flat secret a start of the host
     and `flats secret rm`;
   - when the key opens no value: `flats secret reset`.

   When the values that do not open carry a key ID, the message names it, and says that the file of an earlier
   `rotate-key --out` can hold that key. With no sealed value there is nothing to check, and the command still makes
   a new key.
10. The command asks for a confirmation, because it cannot be reversed: the old key is gone after it. On a terminal
    it shows the number of values that it will seal again, the file that gets the new key, and the note of rule 29,
    and asks the operator to type `rotate`. When a pending key is left over, it also says that the file is left over
    from a rotation that stopped before it changed a value, and that the rotation removes it. Any other answer
    changes nothing and exits with status 0. Without a
    terminal the command needs `--yes`, and without it the command stops with a usage error and changes nothing.
    `--yes` also skips the question on a terminal. The finish of a rotation that stopped (rule 19) asks no
    confirmation, because the first run had it.

## The new key

11. The new key is 32 bytes from the random source of the operating system.
12. Without `--out`, the new key goes into the key file where it is (step 3 of rule 16). The command writes into the
    file and does not replace it, so the file keeps its owner, its mode and its access list. When the key path is a
    link, the file that the link names gets the new key. Every other name of that file gets the new key too: a hard
    link, and the key path of a second host that names the same file. Before its first write, the command opens the
    key file for
    writing and closes it again without a change, as a check. When that fails (a read-only mount, a read-only
    attribute on Windows, a mode that does not let the user write, a program that holds the file), the command
    refuses and names `--out`. On Unix a user other than root needs the write permission on the file for this check:
    a key with mode 0400 is refused until its owner makes it writable for the time of the rotation, or uses `--out`.
13. Without `--out`, the command keeps the new key in the pending key file until the key file holds it. The pending
    key file is `<key path>.next`, which is `<data>/secret.key.next` for the default path. The command creates it
    with a call that fails when the name exists, with mode 0600, writes the 32 bytes, syncs the file and then syncs
    the folder. It makes no temporary file and no hard link. When it cannot create the file, it refuses, names
    `--out`, and has changed nothing but the removal of rule 21. For a configured path, the folder of the pending key
    must not lie inside the data directory after the system resolved every link. Such a folder is a refusal at
    point 4 of rule 7, which names `--out` and writes nothing. On Unix the pending key belongs to the user that runs
    the command. The command removes it at the end of step 3.
14. With `--out FILE`, the command writes the new key to `FILE` and does not open the key path for writing, so the key
    path can be read-only. `FILE` passes the checks of `flats secret restore-key --out`: it does not exist, its
    folder exists, and it does not lie inside the data directory, also through a link. The command creates it with a
    call that fails when the name exists, writes the 32 bytes, syncs the file and then syncs the folder. On Unix
    `FILE` has mode 0600 and belongs to the user that runs the command. On Windows it takes the access list of its
    folder. The operator gives the key at the key path the owner and the access that the host needs, and the docs say
    so. While `flats.db` holds sealed values, the start refuses until the operator puts the new key at the key path
    (rule 22). With no sealed value the host starts with the old key, and the new key has no effect until it is at
    the key path. When `FILE` exists, the command refuses, with one exception, the finish of rule 19. The refusal says
    which case it found:
    - `FILE` holds the key that the key path holds too: the key is in place, and `FILE` must be removed;
    - in every other case, such as a file that is no valid key, or a key whose ID no sealed value carries: remove
      it, or name another file.

## The re-seal

15. One transaction (`BEGIN IMMEDIATE`) reads each sealed value, opens it with the old key, seals it with the new key
    under a new random nonce and the same associated data, and writes the nonce, the ciphertext and the key ID of the
    new key. The time of the last change and the source of an operator key do not change, because the value did not
    change. An operator key record without a value does not change. The transaction also writes the `key.rotate`
    event (rule 30). When one value does not open inside the transaction, the transaction changes nothing. A
    transaction that fails for any reason, also in its `COMMIT`, stops the command with status 1. The command then
    removes neither the pending key nor `FILE`: the process cannot always know whether a commit exists. The message
    names the file that stays, and the next run decides by rule 17 what it is.
16. The four steps have this order, and each is durable before the next one starts:
    1. The new key is in the pending key file (rule 13), or in `FILE` (rule 14). The removal of a pending key that
       was left over (rule 21) is the start of this step.
    2. The transaction of rule 15 commits, and the command makes it durable with a complete checkpoint, as
       `flats secret reset` does. When the checkpoint fails, the command stops with status 1 and does not write the
       key file. Without `--out` the message says to run the command again, which finishes the rotation. With
       `--out` it says to run the command again with the same `FILE`, and to keep the old key at the key path until
       that run succeeds, because a loss of power can still take the re-seal back.
    3. Without `--out`, the command writes the 32 bytes of the new key into the key file, syncs it, reads it again
       and compares. It then removes the pending key and syncs the folder. When the write fails, or the bytes that
       it reads differ, the command stops with status 1, and the state is C. The message names the cause and says
       that the pending key holds the key of the values. It gives two ways to finish: to remove the cause and run
       the command again, or to copy the bytes of the pending key into the key file by hand and run the command
       again, which then removes the pending key (rule 19). The copy by hand is safe at this point, because step 2
       made the re-seal durable. When the write is good and the removal of the pending key fails, the key is in
       place and the state is D. The command then does step 4, exits with status 1, and names the pending key,
       which the next run removes.
    4. The command writes the recovery copy (rule 20).

    An interrupt (Ctrl-C or SIGTERM) before step 1 writes nothing. After step 1 has begun, the command completes the
    four steps, also when an interrupt arrives. A finish (rule 19) follows the same rule: an interrupt before its
    first write writes nothing, and after that write has begun the finish completes.

    On Windows a folder cannot be synced, and Flats does nothing there (`internal/durable`). The command syncs each
    file that it creates after it wrote it. On NTFS that sync also makes the creation of the file durable, because
    NTFS writes its journal in order. Step 3 needs no rename. So the order of the steps holds on NTFS. On another
    file system of Windows (ReFS, FAT, exFAT, a network share) Flats cannot give that order, and the docs tell the
    operator
    to keep the key and `FILE` on an NTFS volume for a rotation.

## A rotation that stopped

17. The pending key file is the file `<key path>.next` of the key path that the configuration names now. A change of
    `host.secret_key_path` while a rotation is stopped hides the file, and the second case of rule 22 then applies.
    Every reader looks at the name without following a link. A regular file at that name that the reader can read
    is one of three things:
    - It *waits* when it is a valid key, at least one sealed value carries its key ID, and the key path does not
      hold the same key.
    - It is a *duplicate* when it is a valid key and the key path holds the same 32 bytes.
    - It is *left over* in every other case: a valid key whose ID no sealed value carries, or a file that is not 32
      bytes, which a stop during step 1 can leave. A pending key that is left over
      is never the only file with the key of a value, because the transaction of rule 15 starts only when the
      pending key is complete and durable. Flats treats a valid key at this name as its own, whatever wrote it, and
      the docs say not to use the name.

    Something at the name that is no regular file (a folder, a link), or a file that the reader cannot read, is none
    of the three. It stops every reader with an error that names the file: the command, the start, `flats key set`,
    `flats secret reset`, `flats secret backup-key` and `flats backup --out`. The error says to move the file away,
    or to make it readable for the user. No reader treats such a file as left over, because it can be a pending key
    that waits: a run of the command as another user leaves a pending key that the user of the host cannot read.
18. After a stop at any point (a crash, a kill, a loss of power), the files are in one of these states. In each of
    them one file holds the key that opens every sealed value: the key path in A, B, D, E, F and H, the pending key
    in C, and `FILE` in G.

    | State | Key path | Pending key, or `FILE` | Sealed values | The start | The next `rotate-key` |
    | --- | --- | --- | --- | --- | --- |
    | A | the old key | none | under the old key | starts | a new rotation |
    | B | the old key | a pending key that is left over | under the old key | starts | a new rotation, which removes the pending key first (rule 21) |
    | C | the old key, nothing, or a file that a stop in step 3 damaged | a pending key that waits | under the new key | refuses (rule 22) | finishes (rule 19) |
    | D | the new key | a pending key that is a duplicate | under the new key | starts, and warns while the recovery copy holds the old key | finishes: removes the pending key (rule 19) |
    | E | the new key | none | under the new key | starts, and warns while the recovery copy holds the old key | a new rotation |
    | F | the old key | `FILE` is incomplete, or holds a key that sealed nothing | under the old key | starts | refuses `FILE` because it exists (rule 14) |
    | G | the old key | `FILE` holds the new key | under the new key | refuses (rule 22) | with the same `FILE`: finishes (rule 19); else refuses by rule 9 and names the key ID of the values |
    | H | the new key, which the operator put there | `FILE`, until the operator removes it | under the new key | starts | a new rotation; the same `FILE` is refused (rule 14) |

    A stop before step 1 ends leaves A, B or F. A stop after step 1 and before the commit leaves B or F. A stop after
    the commit and before the write of step 3 ends leaves C or G, also when the checkpoint did not run yet. A stop
    after that write and before the removal of the pending key leaves D. A stop after step 3 leaves E. A loss of power
    can take back a commit that no checkpoint made durable: C then becomes B, and G becomes F. Step 3 starts only
    after a complete checkpoint, so a state D or E that the command made never has values under the old key. An
    operator who copies a pending key into the key file by hand makes state D too. That is safe only after a run
    reported the re-seal durable, and the messages and the docs name the copy by hand only then (rules 16, 19 and
    36). In G the old key must stay at the key path until a run of the command reported the re-seal durable (rules
    16 and 19).

    With no sealed value in `flats.db`, a pending key never waits. A stop inside the write of step 3 can then leave
    a damaged key file beside a pending key that is left over. No value is sealed, so nothing is lost: the start and
    the command refuse the damaged file with the advice to move it away, as before this change.
19. The command finishes a rotation that stopped, and makes no new key, in three cases.
    - **A pending key waits.** `--out` is a refusal that says to run the command without it. The pending key must
      open every sealed value. When it does not, the command refuses with the numbers of rule 9 and writes nothing.
      The checks of rule 3 of [secret key path](secret-key-path.md) apply to a configured path. The command asks for
      the recovery password as rule 20 says. It then makes `flats.db` durable with a complete checkpoint, as step 2
      of rule 16 does, and stops without a change when that fails. Then it makes the check of rule 12 on the key
      file. When that check fails, or when something is at the key path that is no file after links are followed,
      the command stops with status 1 and gives the two ways of step 3 of rule 16. It does not name `--out`, which a
      finish refuses. Then it does steps 3 and 4 of rule 16. When nothing is at the key path, step 3 creates the key
      file with mode 0600, and syncs the file and its folder before it removes the pending key. That file belongs to
      the user that runs the command. The output says that the operator must give it the owner and the access that
      the host needs, as for a key that `flats secret restore-key` wrote.
    - **A pending key is a duplicate.** The key is in place. `--out` is the same refusal. The key must open every
      sealed value, else the command refuses as rule 9 says and writes nothing. The command asks for the recovery
      password as rule 20 says. It then makes `flats.db` durable with a complete checkpoint, and stops without a
      change when that fails, because an operator can have made the duplicate by hand before the re-seal was
      durable. Then it removes the pending key, syncs the folder, and does step 4 of rule 16.
    - **`--out FILE` names a file that exists, is a valid key, and is not the key at the key path, and every sealed
      value carries its key ID** (at least one value). The key in `FILE` must open every sealed value, else the
      command refuses with the numbers of rule 9. The command asks for the recovery password as rule 20 says, makes
      `flats.db` durable with a complete checkpoint, and does step 4 of rule 16. When the checkpoint fails, the
      command gives the message of step 2 of rule 16 again. When it succeeds, the output says that the re-seal is
      durable, that the key in `FILE` must be put at the key path, and that `FILE` must then be removed.

    A finish writes no second `key.rotate` event and asks no confirmation. The output says that it finished a
    rotation that had stopped.
20. The recovery copy. When something is at `<data>/secret.key.recovery`, the command makes the copy current, so that
    the recovery password opens the new key:
    - At point 5 of rule 7 it reads the copy (at most 64 KiB), asks for the recovery password once, and opens the copy
      with it by rule 21 of [secret key recovery](secret-key-recovery.md). A wrong password, or a copy that does not
      open, is a refusal that writes nothing. The refusal says that an operator who lost the password must remove
      the recovery copy, which opens nothing without its password, and run `flats secret backup-key` after the
      rotation. The key in the copy does not have to be the host key: a copy that was out of date is made current
      too.
    - Without a terminal and without `--password-file`, the command stops with a usage error that names
      `--password-file`, and writes nothing.
    - At point 7 of rule 7 it encrypts the new key under the same password, at the work factor of
      `flats secret backup-key`.
    - At step 4 of rule 16 it writes the copy and then its key ID as rule 15 of that spec says, and appends the
      `key.backup` event (rule 31). With `--out`, step 4 comes after step 2.
    - When a write of step 4 fails, the rotation is complete and the command exits with status 1. The message says
      what is true at that point: either the copy was not written and holds the old key, or the copy holds the new
      key and its ID file is out of date. In both cases it names `flats secret backup-key`, which writes both files.
      With `--out` it says to run that command after the new key is at the key path, because rule 25 refuses it
      before. The start warns until then.

    When nothing is at `<data>/secret.key.recovery`, the command asks for no password and creates no copy, and the
    output names `flats secret backup-key`. `--password-file` is then a refusal that says that there is no recovery
    copy to make current. A copy that the operator keeps apart from the host (`backup-key --out`) is not changed. It
    holds the old key.

    `flats secret backup-key` takes no host lock, so it can run at the same time as this command. In that case the
    data directory can end with a copy of the old key and the ID of the new key, and the start does not report it.
    This is a limit. The docs say to run one key command at a time.
21. A pending key that is left over is removed at the start of step 1 of a new rotation, with or without `--out`,
    and the output says so. Nothing removes it earlier: a refusal of points 3 to 6 of rule 7 leaves it where it is.
    The command never removes a pending key in the run that wrote it, except at the end of step 3.

## The start and the other commands

22. The start refuses in two states. The check comes with the first check of the key, before the start reads an
    input for an operator key, and before it opens or stores an operator key.
    - **A pending key waits.** The refusal names `flats secret rotate-key`, which finishes the rotation, whatever the
      key path holds: the old key, nothing, or a file that is no valid key.
    - **No pending key waits, the key path holds a valid key, `flats.db` holds sealed values, each of them carries a
      key ID, and none carries the ID of that key.** The refusal names the ID of the key, the key IDs of the values,
      and the ways out, in this order. After a rotation with `--out`: run `flats secret rotate-key --out FILE` again
      with the same file, and put the key of that file at the key path only after that run reported the re-seal
      durable. Else: restore the key with the ID of the values with `flats secret restore-key`, or, with no such key
      left, delete the stored values with `flats secret reset`.

    A host in one of these states would seal new values under a key that does not open the others. `flats key set`
    and `flats secret backup-key` refuse in the same states with the same message. Every other state starts as
    before this change: a database with a value that has no key ID, or with a value that carries the ID of the key,
    has no check of this rule, and a value that the key does not open fails when it is used. A database from before
    this change has no key ID on its values, so the second state cannot apply to it until a rotation, or a new set
    of each value, gave every value an ID. The docs say so.
23. The start never moves, writes or removes a pending key. It reads the pending key only for rule 22. A pending key
    that is left over or a duplicate does not change a start. When nothing is at the key path, no pending key waits,
    and every sealed value carries one key ID, the refusal of the missing key names that ID, for the default path
    ([secret key recovery](secret-key-recovery.md), rule 32) and for a configured path
    ([secret key path](secret-key-path.md), rule 6).
24. `flats secret reset` refuses when a pending key waits, and names `flats secret rotate-key`: that key opens the
    values, so nothing is lost. In every other state the command works as before, with one addition. When every
    sealed value carries one key ID, and the key path holds nothing or a key with another ID, the command names that
    ID before it asks for the confirmation. It says that a key file with that ID opens the values, for example the
    file that `rotate-key --out` wrote.
25. `flats secret restore-key`, `flats key ls` and `flats key rm` do not change, and they are safe in every state of
    rule 18. `flats secret backup-key` refuses in the states of rule 22, because a copy of a key that sealed none of
    the values restores nothing, and in state G it would replace the copy of the new key. It reads the key IDs for
    that check without the host lock and without a migration, and makes no check when `flats.db` is not of the
    schema of the binary.
26. `flats backup`:
    - It never copies `<data>/secret.key.next`, and never a temporary key file `<data>/.secret-key-*`, which the
      creation of a key at the first start and `flats secret restore-key` can leave after a crash. The manifest
      lists them in `excluded`, with and without `--exclude-secret-key` ([host backup](host-backup.md), rule 3).
    - `flats backup --out` refuses in its preflight in the states of rule 22, with the message of that rule, and
      writes nothing. This covers the states C and G. It reads the key IDs as `flats secret backup-key` does (rule
      25). With `--exclude-secret-key` the command copies no key and reads none for the second state of rule 22, so in
      state G such a copy is made: it holds `flats.db` under the new key and the recovery copy that step 4 wrote,
      when one exists. For the first state the command needs the bytes of two files, for the default path and for a
      configured path alike. It reads the pending key file when one exists, also in this mode. When a sealed value
      carries the ID of that key, it reads the key file too, only to compare the two, and a key file that is missing
      or cannot be read counts as different. So in this mode the command refuses in state C and makes a copy in
      state D.
    - In the states B and D a copy is made as in A and E. It holds no pending key.
    - A write of the key file during a copy fails the copy by rule 22 of that spec.
    - While the recovery copy holds the old key (after step 3 and before step 4, or after a failed step 4), a copy
      holds a recovery copy that does not open its `flats.db`. A copy with its `secret.key` is still complete. A
      copy with `--exclude-secret-key` cannot be restored with the password. This is the limit of every recovery copy
      that is out of date: the start and the message of rule 20 report it, and `flats backup` does not.
27. Bootstrapping a new host treats `secret.key.next` as Flats data, like `secret.key`
    ([secret key recovery](secret-key-recovery.md), rule 6).

## What the command says

28. The output of a rotation gives the number of flat secrets and of operator keys that were sealed again, the key ID
    of the old key and of the new key, each file that the command wrote, and what happened to the recovery copy.
    With `--out` it says that the new key must be put at the key path, that the start refuses until then while
    `flats.db` holds sealed values, and that `FILE` must then be removed. It never shows a key or a value.
29. The confirmation and the output say that a copy of `flats.db` from before the rotation opens only with the old
    key, and that the operator must copy the old key away first when such a copy must stay readable without a key of
    its own. They say that this must be a real copy and not a link, because every name of the key file gets the new
    key (rule 12).

## Audit

30. `key.rotate` is written by the transaction of rule 15, also when `flats.db` holds no sealed value. It has no flat
    and no name, the `via` is `command`, and `data` is
    `{"from": <ID of the old key>, "to": <ID of the new key>, "secrets": N, "keys": M, "out": true or false}`, where
    N and M count the flat secrets and the operator key values that were sealed again. The event records the re-seal
    in `flats.db`, after which the old key opens nothing there. It does not record when the new key became the host
    key: the finish of rule 19 and the placement of `FILE` by the operator write no event. After a rotation with
    `--out`, or after a stop before the end of step 3 for a database without a sealed value, the log can hold a `to`
    key that
    never became the host key.
31. `key.backup` is also written when the command has written the recovery copy and its ID file (rule 20), with the ID
    of the new key and `"out": false`. A failure to append it follows rule 17 of [audit events](audit-events.md): the
    copy stays, and the error says so.
32. The event webhook sends nothing for these events. `flats audit ls` and `flats audit export` show them as they
    show every audit event.

## Compatibility

33. Schema 27 adds the column `key_id` (text, not null, default empty) to `secrets` and to `host_keys`. The migration
    writes no key ID, because it has no key: a value gets its ID when it is sealed the next time. An older release
    refuses the database, as for schemas 25 and 26: its start, `flats key`, `flats backup` and `flats audit` refuse,
    and its `backup-key` writes the copy and fails the audit event with status 1. The opens that demand the exact
    schema (`OpenImmutable`, `OpenAuditReader`, `AppendAuditBeside`) refuse a database of schema 26 with this
    release until a start or a key command migrated it.
34. The key file stays 32 bytes, and the recovery copy keeps its format. A value sealed by this change differs from
    an older one only in its label.
35. No new dependency.

## Docs

36. `docs/configuration.md` gets a section on the rotation:
    - when to rotate, and what the confirmation means;
    - the steps for a key in the data directory;
    - the steps for a key at `host.secret_key_path`: a plain file (in place, or `--out` when the file or its folder
      cannot be written, as for the file with mode 0400 that the docs show); a Docker or Compose secret (a one-off
      container that mounts the secrets of the service, with `--out`
      to a mounted folder outside the data volume, then a new secret); a key that the command cannot see, such as a
      systemd credential (the operator makes a copy of the key in a folder of the service user outside the data
      directory, points `host.secret_key_path` at the copy, rotates in place, puts the new key into the secret store,
      and sets the path back);
    - the owner and the access of a key file that the operator puts in place;
    - what each state of rule 18 looks like to the operator and how to go on;
    - the recovery password, and the lost password;
    - one key command at a time, and an NTFS volume on Windows;
    - the copy of a pending key into the key file by hand, only after the command named it or reported the re-seal
      durable;
    - a key file with more than one name: every hard link gets the new key, and a rotation of a key file that two
      hosts use changes the key of both.

    "What to back up", "Recovering a lost secret.key" and "Keeping the key outside the data directory" name the
    rotation where a key changes, and `docs/container.md` names it for a Docker secret. `docs/design.md` ("Secrets")
    describes the life of the key: its creation, the key ID, the order of rule 16, what to back up, what to store
    apart (the recovery password, a recovery copy from `backup-key --out`, a key at a configured path), and when to
    use `reset`. `docs/surfaces.md` and the help of `flats secret` list the command. The sentence of "Keeping the
    key outside the data directory" that Flats never creates a key at a configured path gets its limit: the host
    never does, and `flats secret restore-key` and the finish of a rotation write a key there for the operator.
37. The docs say what the old key still opens: a copy of `flats.db` from before the rotation. A copy that
    `flats backup` made with its `secret.key` stays complete. A copy without its key, and a recovery copy that is
    kept apart from the host, need the old key or hold it. The operator copies the old key away before the rotation
    when such copies must stay readable. That copy is a real copy and not a link. After the rotation the operator
    stores the new key apart again: a new
    `flats secret backup-key --out`, and a new backup.

## Changes to other specs

The implementation of this spec changes these implemented specs, and not before:

- [Audit events](audit-events.md): the scope names this spec as the writer of `key.rotate`; rule 4 lists the command
  under `command`; rule 10 and the summary say that a rotation replaces every value with one `key.rotate` event and
  writes no `key.set`; rule 12 names rule 31; rule 15 and the row of `key.rotate` get the fields of rule 30; rule 16
  lists the transaction of rule 15.
- [Host backup](host-backup.md): the scope names this spec; rule 3 lists `secret.key.next` and `.secret-key-*`; the
  preflight (rule 12 there) gets the refusal of rule 26; rule 22 says that it holds for the key file, and names the
  limit of rule 26 for the recovery copy.
- [Secret key recovery](secret-key-recovery.md): the scope names this spec; rule 6 lists `secret.key.next`; rule 12
  gets the refusal of rule 25 and the limit of rule 20; rule 27 gets rule 24; rule 32 gets rules 22 and 23.
- [Secret key path](secret-key-path.md): the scope names this spec and the check of rule 22; rule 5 says that the
  first state of rule 22 comes before the refusals of a configured key; rule 6 gets rule 23; rule 7 names
  `rotate-key` and its finish as commands that write a key at a configured path; rule 21 names the steps of rule 36
  for this command.

## Current state

Nothing of this spec is implemented. `key.rotate` is a reserved kind that nothing writes.

## Completion criteria

The tests stop the command at a named step through a hook, as the tests of `flats backup` do. A test that needs a
file that the user cannot write, or a folder in which the user cannot create a file, skips for the user root, and the
folder test skips on Windows. A test that needs a symbolic link skips on Windows without the privilege. Each skip
names its reason. No criterion depends on time, apart from the checkpoint test, which can wait for the busy timeout.

- A test rotates a host that holds flat secrets of two flats and operator keys. It shows that the key file holds a
  new key and is the same file as before, with the same mode and owner on Unix; that every value opens with the new
  key and has its key ID; that no value opens with the old key; that the opened values are the same bytes as before;
  that an operator key record without a value did not change; that no pending key and no temporary file stays; and
  that a host that starts afterwards gives a flat its secrets.
- Tests stop the command after each step of rule 16 and inside steps 1 and 3, and show the state of rule 18 that each
  stop leaves, by its letter: which file holds the key that opens every value, what the start does, and what the next
  `rotate-key` does. The tests of state C run it with the old key at the key path, with nothing there and with a
  damaged file there. They show that the start, `flats key set` and `flats secret backup-key` refuse and name the
  command, that `flats secret reset` refuses, and that the finished rotation gives a host that starts and opens every
  value. The tests of state C at the start run it with an input for an operator key and with a stored Tailscale key,
  and show that `host_keys` did not change. The test of state D shows that the next run removes the pending key and
  makes no new key.
- A test shows that a value that does not open inside the transaction changes nothing: every value opens with the old
  key, no value has a new key ID, no `key.rotate` event exists, and the pending key stays. A test with an injected
  error of `COMMIT` shows that the pending key and `FILE` stay.
- A test shows that the key file is not written when the checkpoint of step 2 fails, without and with `--out`. With
  `--out`, a second run with the same `FILE` reports the re-seal durable only after its checkpoint succeeded.
- Tests show a failed write of step 3 and a failed comparison: status 1, the message of rule 16, state C, and a
  finish by a second run and by a copy by hand (state D). A test shows a failed removal of the pending key after a
  good write: step 4 runs, the status is 1, and the state is D. A test shows a finish with nothing at the key path:
  the key file exists with mode 0600 before the pending key is removed, and the output names its owner.
- Tests show each refusal of rules 6, 8, 9, 12, 13, 14, 17, 19 and 20, and a declined confirmation and the missing
  `--yes` of rule 10. A test shows the sentence of rule 10 for a pending key that is left over. Each creates no
  pending key and no `FILE`, writes nothing that rule 11 of [secret key recovery](secret-key-recovery.md) excludes,
  and leaves a pending key or a `FILE` that existed before the same, byte for byte. The one exception is the refusal
  of rule 13 at step 1, which can come after the removal of a pending key that was left over. A test shows that an
  interrupt before step 1 writes nothing, and that an interrupt after step 1 began does not stop steps 2 to 4. A
  test shows the same for a finish.
- A test shows that a pending file that the user cannot read, and a link at the pending name, stop the command, the
  start, `flats key set`, `flats secret reset`, `flats secret backup-key` and `flats backup --out` with an error that
  names the file. The test of the file that cannot be read skips for root and on Windows.
- Tests show `flats secret backup-key` in state G, and `--out` and `flats secret reset` in state D. A test rotates a
  key path that is a link, and the file that the link names gets the new key. A test shows that the finish of a
  duplicate does not remove the pending key when its checkpoint fails. A test changes `host.secret_key_path` while a
  pending key waits, and the start then gives the second refusal of rule 22.
- A test shows that the recovery password opens the new key after a rotation, through `flats secret restore-key`, and
  that the start does not warn. Tests show the wrong password with its advice, a password shorter than 12
  characters that opens a copy made by hand, the usage error without a terminal, `--password-file` without a
  recovery copy, and each of the two failures of step 4 with its message.
- Tests show a rotation with `--out`: the key path is not written, `FILE` holds the key that opens every value, the
  start refuses with the key IDs until that key is at the key path, and then starts. They show the three cases of an
  existing `FILE` (rules 14 and 19). One test uses a key file that cannot be opened for writing.
- Tests show rule 22 for its excluded states: a database with a value without a key ID starts with a wrong key as
  before, and so does one with values of two keys of which one is the key at the key path. Tests show rule 23 (the
  ID in the refusal of a missing key, for both kinds of path) and rule 24 (the ID before the confirmation of
  `reset`).
- A test shows that an error for a value that does not open names both key IDs (rule 3). Tests show that each writer
  of rule 2 writes the key ID of the host key: a set of a flat secret, `flats key set`, the console, an input of the
  start, and the import of a legacy host. A test shows that a record without a value has an empty key ID after it
  replaced a record with a value.
- Tests show the `key.rotate` event with its fields, in the transaction of the re-seal, the `key.backup` event of
  rule 31 and a failed append of it, and that a finish writes no second `key.rotate`. A test shows that no route,
  no agent tool and not `flats key ls` gives a key ID (rule 4).
- Tests show that `flats backup` leaves `secret.key.next` and `.secret-key-*` out and lists them, that
  `flats backup --out` refuses in the states C and G with the message of rule 22, that it makes a copy in the states
  B and D, and that with `--exclude-secret-key` it refuses in state C and makes a copy in the states D and G. A test
  shows that a new host treats `secret.key.next` as Flats data (rule 27).
- A migration test shows schema 27 on a database of schema 26 that the registry builds, with values sealed under a
  known key: the values keep their bytes, have an empty key ID, and open. The tests that pin the schema version
  move to 27, and a test shows that `OpenImmutable`, `OpenAuditReader` and `AppendAuditBeside` refuse a database of
  schema 26.
- Tests show the output of rules 28 and 29, and that it holds no key and no value.
- On the real binary: a host with a flat secret and an operator key, a stop, `rotate-key --yes` with a recovery copy,
  a start whose flat secret opens, and `flats audit ls --kind key.rotate` with the event.
- `docs/configuration.md`, `docs/container.md`, `docs/design.md` and `docs/surfaces.md` describe the command as rules
  36 and 37 say, and `doccheck` passes.

## Open questions

None.
