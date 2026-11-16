# Secret key recovery

Status: implemented.

## Summary

Flats seals every flat secret and every stored operator key under one 32-byte
key, `<data>/secret.key`. When that file is lost while `flats.db` still holds
sealed values, the host refuses to start, and nothing in Flats can open the
values again. This spec adds three commands under `flats secret` that run on
the host:

- `backup-key` writes a copy of the key, encrypted under a recovery password
  the operator chooses, to `<data>/secret.key.recovery`, and optionally to a
  second file kept apart from the host.
- `restore-key` decrypts that copy with the password, checks the key against
  the sealed values in `flats.db`, and writes `secret.key` again.
- `reset` deletes the sealed values when neither the key nor the password is
  left, so the host can start with a new key.

The host also warns at start while the copy holds another key than
`secret.key`. The request is item 5 of issue #224.

## Before this change

- At start, `CheckSecretKey` (`internal/core/secrets.go`) stops the host when
  `secret.key` is missing and `flats.db` holds flat secrets or operator keys
  with a stored value. It does not create a new key over them. Its message
  says only to restore the key from the backup of the database.
- The docs give one way out: restore `secret.key` from the same backup as
  `flats.db` ([configuration](../configuration.md#starting)). `flats key rm`
  removes operator keys with the host stopped. `flats secret rm` calls the
  running host's API, so a host with flat secrets and no key has no supported
  way to start again.
- The key changes only when Flats creates it, and Flats creates it only when
  no file is at `secret.key`: at a start, or when `flats key set` stores the
  first operator key. Without rotation, a copy goes stale only when the key
  is lost and a new one is created, or when someone puts another key file in
  its place.

## Scope

In scope: the three commands, the recovery copy and its key ID, the start-time
warning, the start-time refusal message, and the docs.

Out of scope:

- Key rotation (#224 item 2). When rotation ships, it must keep the recovery
  copy and its key ID current. The key path setting (#224 item 1) is
  implemented: the commands find the key through the location that
  [secret key path](secret-key-path.md) adds, and rules 19, 23 and 31 below apply
  to that location. The recovery copy and its key ID stay in the data directory.
- Audit records (#224 item 4). `reset` leaves an event on each flat whose
  secrets it deletes, as `flats secret rm` does today.
- The console and MCP. No agent tool or API route reaches these commands.

## The recovery copy

1. `<data>/secret.key.recovery` holds the 32 bytes of `secret.key`, encrypted
   in the age v1 format (`age-encryption.org/v1`) with one passphrase (scrypt)
   recipient, in binary form, at age's default work factor of 2^18. The
   plaintext is exactly the key file's 32 bytes, so the reference `age` tool
   turns the file back into a key with the password and without Flats.
2. `<data>/secret.key.recovery.id` holds the key ID of the key in the copy, as
   32 lowercase hex digits and a line feed. The key ID is the first 16 bytes of
   HMAC-SHA-256 with the key as the HMAC key and, as the message, the 19 ASCII
   bytes of the text `flats secret key id` with nothing after them. For the
   key of the 32 bytes `00` to `1f` it is `5be1a8cb8352ce199e248eadeb89a6b4`.
   It helps neither to open the copy nor the sealed values, because the key is
   256 random bits. Only `backup-key` writes the ID file, so a copy made by
   hand with `age -p` has none until `backup-key` runs once.
3. On Unix, Flats writes both files with mode 0600. On Windows the data
   directory's access list protects them, as it protects `secret.key`.
4. A copy of the data directory includes the files. Without the password the
   copy opens nothing, but anyone holding it can guess passwords offline at
   the cost of one scrypt derivation per guess, which takes about a second
   and 256 MiB of memory (N = 2^18, r = 8). The docs say so and ask for a long
   password that is kept apart from the host.
5. At each start, once the host has its key, and only when
   `secret.key.recovery` exists, the host compares the key's ID with
   `secret.key.recovery.id`, ignoring surrounding white space. When the ID
   file is missing, cannot be read, or names another key, the host logs a
   warning that names `flats secret backup-key`, and starts. A host without a
   recovery copy logs nothing. A start that overlaps a `backup-key` can read
   the ID file before the new one is in place and warn once; the next start
   is quiet.
6. Bootstrapping a new host treats both files as Flats data, like
   `secret.key`.

## Shared rules

7. Each command selects the configuration and the data directory with
   `--config PATH` or `--data DIR`, as `flats key` does. None of them calls the
   host: `--url` does not apply, and they print text, not JSON. With
   `--json`, a failure prints as JSON, as for every command.
8. A password never comes from a command-line argument or an environment
   variable. `--password-file PATH` names a file whose content, without its
   trailing carriage returns and line feeds, is the password; a file larger
   than 4096 bytes is refused. Without it the command prompts without echo on
   a terminal, and echo is on again when the command ends, also when an
   interrupt ends it at the prompt. With neither a file nor a terminal, the
   command stops with a usage error and writes nothing.
9. A password longer than 1024 bytes is refused by both commands.
10. A usage mistake exits with status 2. A refusal exits with status 78, like
    `flats key`. Any other failure exits with status 1.
11. A refusal never writes `secret.key`, the recovery copy, its key ID or the
    sealed values. `restore-key` and `reset` open `flats.db` as `flats key`
    and the next start do, which may migrate a database from an older release,
    writing the `backups/flats.v<N>.<time>.db` copy a migration takes, and
    switch it to WAL. Those are the only writes a refused command can leave.
    An interrupt (Ctrl-C or SIGTERM) that arrives before a command starts to
    write, also after the password or the confirmation was entered, writes
    none of these either. A write that has begun completes.

## `flats secret backup-key`

```text
flats secret backup-key [--out PATH] [--password-file PATH] [--config PATH | --data DIR]
```

12. The command works whether or not the host runs. It reads `secret.key`,
    writes files, and does not open `flats.db`. The key does not change while
    a host runs, so a copy taken from a running host is current.
13. `secret.key` must be a regular file of 32 bytes, the check the host makes
    at start. When nothing is at the path, not even a link, the command
    refuses and says to start the host once, which creates the key, or to run
    `restore-key` when sealed values need the lost key. Anything else that is
    not a valid key, such as a file of the wrong size or a link that leads to
    no file (a missing target, a loop, or a file on its path), is refused with
    the host's own message and the advice to move it away, because the next
    start would stop on it. When reading `secret.key` fails for another
    reason, such as permissions, the command stops with that error, status 1.
14. A new password has at least 12 characters (Unicode code points), from
    either source. On a terminal the command asks twice, and two answers that
    differ are a refusal.
15. The command writes `<data>/secret.key.recovery`, then
    `<data>/secret.key.recovery.id`. Each goes through a new temporary file in
    the data directory (`.secret.key.recovery-*` and
    `.secret.key.recovery.id-*`), synced before it is renamed over the old file
    with the retry that PR #220 added for Windows, and the directory is synced
    after. A crash can leave such a temporary file; it holds only the
    encrypted copy or the ID and can be removed.
    Writing the copy first means a crash between the two leaves an ID that
    does not match, which the next start reports, never an ID that matches an
    older copy. Replacing an existing copy is how the password changes, and the
    output says that it replaced one.
16. `--out PATH` also writes the same copy to `PATH`, without the key ID. The
    command refuses before writing anything when `PATH` exists, when its
    folder does not exist or is not a folder, and when its folder is the data
    directory, which holds the copy and its key ID. It creates the file so
    that it never replaces one, also on a volume without hard links. The file
    gets mode 0600 on Unix. On Windows it takes the access list of its folder.
    If writing it fails, the command removes what it wrote there and exits
    with status 1; the files in the data directory stay.
17. The output names each file written and says that Flats keeps no copy of
    the password.

## `flats secret restore-key`

```text
flats secret restore-key [--from PATH] [--password-file PATH] [--config PATH | --data DIR]
```

18. The command takes the host lock like `flats key`, refuses while a host
    runs, and checks that `flats.db` belongs to the configuration.
19. It refuses when anything is at `<data>/secret.key`: a file, a directory or
    a link, also a link that points nowhere. It never replaces a key. A wrong
    key must be moved away first. With `host.secret_key_path`, the same holds for
    the configured path, and a file at `<data>/secret.key` is a refusal too. With
    `--out FILE`, the command does not look at the configured path
    ([secret key path](secret-key-path.md), rules 12 and 13).
20. It reads `--from PATH`, by default `<data>/secret.key.recovery`. A file
    larger than 64 KiB is refused by its size before any password is asked.
    The command then asks for the password once; rule 14 does not apply.
21. It accepts the binary form and the ASCII-armored form (`age -a`). A wrong
    password, a damaged file, or a file without exactly one passphrase
    recipient is refused, and so is a plaintext that is not exactly 32 bytes.
    A file whose scrypt work factor is above 2^18, the factor `backup-key` and
    `age -p` use, is refused before any key is derived, so a crafted file
    cannot make the command use more memory or time than `backup-key` does.
22. The command checks the recovered key against every sealed value in
    `flats.db`: every flat secret and every operator key record with a stored
    value must open with it. When any does not, it refuses and says how many
    of how many opened. When `flats.db` holds no sealed value, there is
    nothing to check against: the command says so and continues, so in that
    case it installs any key the password opens.
23. It writes `secret.key` through a synced temporary file `.secret-key-*`
    with mode 0600 in the folder of the key (the data directory, or the folder of
    `host.secret_key_path`), published under the final name with
    a link that fails when the name exists, as the host does at its first
    start, and then syncs the directory. When something created `secret.key`
    after the check in rule 19, the command refuses and removes its temporary
    file. A crash between the write and the link can leave the temporary file,
    as one can at the host's first start. It holds the key the command
    recovered, which the recovery copy also holds, and can be removed.
24. The output says that the host can start.

## `flats secret reset`

```text
flats secret reset [--yes] [--config PATH | --data DIR]
```

25. The command takes the host lock like `flats key`, refuses while a host
    runs, and checks that `flats.db` belongs to the configuration.
26. It collects every flat secret, by flat and name, and every operator key
    record with a stored value, by key name. When there are none, it says
    there is nothing to reset and exits with status 0.
27. It continues only when nothing is at `<data>/secret.key`, or when a valid
    key there opens none of the collected values. "Valid" is judged as the
    host judges it, following links, as in rule 13. Otherwise:
    - A key that opens every value: nothing is lost, and the command refuses.
      It names `flats secret rm` and `flats key rm` for single values.
    - A key that opens some values: the command refuses and lists the values
      it does not open, operator keys apart from flat secrets. Operator keys
      come first: the start refuses while a stored Tailscale or NetBird key
      does not open, so `flats key rm` removes them while the host is
      stopped. The host then starts with that key, and `flats secret rm`
      removes the flat secrets one by one. The other values stay.
    - Anything at `secret.key` that is not a valid key (the wrong size, not a
      regular file, a link that leads to no file): the command refuses and
      says to move it away first, because the next start would stop on it.
    - Reading `secret.key` fails for another reason, such as permissions: the
      command stops with that error, status 1.
28. It lists what it will delete: each secret's flat and name, and each
    operator key's name. It never shows a value.
29. On a terminal it asks the operator to type `reset`. Any other answer
    changes nothing and exits with status 0. Without a terminal it needs
    `--yes`, and without it the command stops with a usage error and changes
    nothing. `--yes` also skips the question on a terminal.
30. One transaction deletes the collected values and adds an event to each
    affected flat that names the deleted secret, and the command then makes
    the transaction durable. Operator key records without a stored value stay,
    such as the record of a connected zrok account.
31. The command does not touch `secret.key`, the recovery copy or its key ID.
    With `host.secret_key_path` the next start creates no key, and the output
    says that it needs a key at the path. When nothing is at `secret.key` and the
    setting is empty, the next start creates a new key, and the
    host then warns about an existing recovery copy (rule 5). The output says
    to run `backup-key` after that start, and to set the secrets and operator
    keys again with `flats secret set`, followed by a redeploy, and
    `flats key set`.

## Start-time refusal

32. When `secret.key` is missing over sealed values, the start's refusal
    always names `flats secret restore-key` and the backup of the database.
    When flat secrets are stored, it also names `flats secret reset` for a host
    with neither the key nor the password. With operator keys alone it names
    `flats key rm` instead, as today. `flats key set` refuses with the same
    message. When a stored operator key does not open with the key that is
    there, the start's refusal names `flats secret restore-key` and
    `flats key rm`.

## Compatibility

33. No schema change. A host without a recovery copy behaves as today apart
    from the refusal message, and an older Flats ignores the files.
34. The commands add `filippo.io/age` and its dependency `filippo.io/hpke`,
    both BSD-3-Clause.

## Completion criteria

- Tests show that a key written by `backup-key` and restored by `restore-key`
  is byte for byte the original, and that the host then opens every flat
  secret and operator key.
- Tests show that each refusal in rules 8, 9, 13, 14, 16, 18, 19, 20, 21, 22,
  25 and 27, and a declined confirmation (rule 29), writes nothing that rule
  11 excludes. The test of rule 27's partial case checks that the output
  lists operator keys apart and names `flats key rm` before the start.
- Tests show that `restore-key` refuses a copy from another host while
  `flats.db` holds sealed values, and continues with a note when it holds
  none.
- Tests show that `reset` deletes every flat secret and stored operator key,
  keeps operator key records without a value, adds one event per deleted
  secret, and that the host then starts with a new key.
- Tests show the start-time warning when the key changes after `backup-key`,
  when the ID file is missing, and no warning when the copy matches or no copy
  exists. A test asserts the start-time refusal message.
- Tests show that a copy above 2^18 is refused and that one at 2^18 is
  derived, and that an interrupt after the answers writes nothing.
- On Linux, a test on a pseudo-terminal shows echo on after a password prompt
  that an interrupt ends before it is shown or while it waits, and after an
  answer.
- The tests lower the work factor to keep the suite fast. One test checks
  that the commands use 2^18, and another reads the work factor a copy was
  written at from its header, without deriving a key at 2^18.
- On the real binary: a host with a flat secret and an operator key, a
  `backup-key` while it runs, a stop, the key removed, a refused start that
  names `restore-key`, a `restore-key`, and a start whose flat secret opens.
  Then the key and the recovery copy removed, a `reset --yes`, and a start
  with no stored values.
- A copy that `backup-key` wrote opens with the reference `age` command built
  from `filippo.io/age`, and yields the key's 32 bytes.
- `docs/configuration.md`, `docs/design.md` and `docs/surfaces.md` describe
  the commands, the files, what to keep apart from the host, and the mode step
  of a manual restore with `age` (its output is not mode 0600).

## Open questions

None.
