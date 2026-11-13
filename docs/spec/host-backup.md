# Host backup

Status: planned.

## Summary

Flats has no command that copies a whole host. An operator must stop the host
and copy the data directory, or write a script that knows every live database.
This spec adds `flats backup`, which copies a host into a new directory while
the host serves, and `flats backup --verify`, which checks such a copy without a
host.

- `flats backup --out PATH` writes the copy and a manifest with the Flats
  version, and the size and SHA-256 of every file. `--exclude-secret-key` leaves
  `secret.key` out, so an operator can keep the key apart from the data.
- `flats backup --verify PATH` opens each database of a copy, compares the
  files with the manifest, and checks that the parts of the copy fit together.

The request is issue #223. The requester chose on 2026-11-14 that a running
host is never paused: the copy checks itself and fails, to be run again, when a
change during the copy could make it inconsistent.

## Before this change

- [Configuration](../configuration.md#what-to-back-up) says to stop the host and
  copy the whole data directory, and that Flats has no supported way to take a
  backup from a running host.
- A plain copy of a live SQLite database can lose recent commits. SQLite's
  backup API copies one database, not the host.
- The databases and the revision contents depend on each other. A board or
  notebook database and `flats.db` must come from one moment, or a flat does not
  start, loses its newest revisions, or stops importing
  ([revision store](../revision-store.md#boards)).
- Revision blobs are written once and deleted only by the sweep, which deletes
  a blob that nothing references and that is older than 10 minutes
  ([blob sweep](../revision-store.md#blob-sweep)).
- `snapshotAs` and `vacuumInto` in `internal/core/data.go` copy one flat's data
  database through a read-only connection with `VACUUM INTO`. Nothing copies the
  other stores or the host.
- Nothing pauses the whole host. Each flat has a storage lock that rename,
  delete, rollback, publish and the sweep take, and a separate process cannot
  take it.

## Scope

In scope: the two modes of the command, the layout and manifest of a copy, the
order in which the command copies and checks, the failures and their exit
codes, and the docs.

Out of scope:

- A restore command. A restore is the documented manual step, with a
  `--verify` before it. A command that restores is a later change.
- Scheduling, upload to a remote store, compression, encryption of the copy and
  incremental copies.
- A pause of the running host. The requester chose a copy that checks itself.
- The key path setting and key rotation (#224 items 1 and 2). The command reads
  `secret.key` at its fixed path. When those items ship, the command must read
  the key through the function that they add, and a rotation during a copy must
  fail the copy (rule 20).

## The copy

1. The copy is a new directory `PATH` that holds `manifest.json`,
   `config.json` and `data/`. `data/` mirrors the data directory. `config.json`
   is the host's configuration file, wherever it lives. When the host has no
   configuration file, `config.json` is absent and the manifest says so.
2. `data/` holds everything that
   [What to back up](../configuration.md#what-to-back-up) names, including the
   optional `cloudflare-quick/`, with these exceptions, which the manifest lists
   in `excluded`:
   - `logs/`, `cache/`, `run/`, `runtime/`, `backups/` and `flats.lock`;
   - the `-wal`, `-shm` and `-journal` file of each SQLite database, because the
     snapshot of rule 4 holds its commits;
   - below a flat's directory: `health-check/`, `restore-trial/` and the
     temporary directories of a restore (`data.restore`, `.restore-old`);
   - below `revisions/` and `design/`: `tmp/`;
   - below `snapshots/`: a snapshot that is not complete (rule 10).
3. With `--exclude-secret-key`, `data/secret.key` is not copied. The recovery
   copy `secret.key.recovery` and its `.id` file stay, so a copy without the key
   can still be restored with `flats secret restore-key`. The manifest records
   `secret_key: "excluded"`.
4. Each SQLite database in the data directory is copied as a snapshot made with
   `VACUUM INTO` from a read-only connection with a busy timeout of 5 s. The
   snapshot holds every commit up to its moment, has no `-wal` or `-shm`, and is
   consistent by itself. The databases are the metadata database `flats.db`, and
   below `flats/<slug>/`: `data/db.sqlite`, `previews/<host>/db.sqlite`,
   `board/board.db` and `notebook/notebook.db`. The command snapshots one
   database at a time, so that the host's own checkpoints resume between two
   snapshots. A snapshot that cannot start within the busy timeout is tried
   again, up to 3 times.
5. Every other file is copied as it is, with its content read while the host may
   write it. A file counts as stable when its size and modification time are the
   same before and after its copy. An unstable file is copied again, up to 3
   times, and then fails the backup. The copy computes the SHA-256 of each file
   from the bytes it reads.
6. On Unix the command creates files with mode 0600 and directories with mode
   0700. On Windows a new file takes the access list of its folder. A copy holds
   every secret that the host holds, apart from `secret.key` when it is
   excluded: the sealed values and operator tokens in `flats.db`, and the
   identities in `tsnet/`, `portal/`, `netbird/`, `zrok-environment/` and
   `cloudflare-quick/`. The docs say to keep a copy as protected as the data
   directory.
7. The command never changes a flat. It opens databases read-only and files for
   reading, and it takes no lock of the host. A database that a stopped host left
   with a `-wal` file is read through that file. A copy of a stopped host is
   exact, because nothing changes during the copy.

## Order and checks

The order matters, because the parts depend on each other. The command runs
these steps and no other:

8. **Preflight.** It selects the configuration and the data directory with
   `--config PATH` or `--data DIR`, as `flats secret backup-key` does. It does
   not call the host, so `--url` does not apply. It refuses before writing
   anything when `--out` names something that exists, even a link; when its
   folder does not exist or is not a folder; when it is inside the data
   directory; or when the data directory is inside it. It creates a new
   directory `PATH.partial-<random>` in the same folder, with mode 0700. All
   writes go there.
9. **Survey.** It lists, for each flat directory below `flats/`, the directory
   and its subdirectories `data/`, `board/` and `notebook/`, each with the file
   identity that the system gives (the inode on Unix, the file index on
   Windows) and its creation time where the system has one. It lists the entry
   names of `flats/.lifecycle/` and every `restore-journal.json` with its size and
   modification time. It reads `secret.key`. A journal that exists at this
   point is data and is copied. It is not a failure.
10. **Write-once files, first pass.** It copies every blob under
    `revisions/blobs/` and `design/blobs/`, and `versions/`, `draft-revs/` and the
    complete snapshots under `snapshots/` of every flat, with the rule 5 retry.
    These files do not change after they are written. A snapshot is a file
    `<kind>-<ms>.sqlite` and a directory `<kind>-<ms>.sqlite.data/`, and it is
    complete when that directory holds `.flats-snapshot-complete`, which the host
    writes last. The command copies a snapshot only when the marker exists at the
    time of the copy.
11. **Board and notebook databases.** It snapshots each `board.db` and each
    `notebook.db`.
12. **`flats.db`.** It snapshots `flats.db` after step 11. A board or notebook
    whose `content_state` row holds a cursor (`import_store` and `import_seq`) is
    coherent with `flats.db` when its snapshot has the outbox id that
    `import_store` names and its highest outbox sequence is not below
    `import_seq`. When it is below, an entry was created and imported between the
    two snapshots, and the restored flat would not start (the opening rules of
    [boards](../revision-store.md#boards) and
    [notebooks](../revision-store.md#notebooks)). The command then repeats steps 11
    and 12, up to 3 attempts in all, and fails the backup after the third. This
    order cannot lose a revision. An outbox entry that the `flats.db` snapshot has
    already imported is at or before its cursor, and the host acknowledges such an
    entry again (the importing steps of the same two sections, step 1). The
    reverse order could lose the revisions that were imported and acknowledged
    between the two snapshots.
13. **Other databases.** It snapshots every other database of rule 4.
14. **Write-once files, second pass.** It copies the files of step 10 that are new
    since the first pass. This pass starts after the last snapshot, so a blob that
    a snapshot references is in the copy, unless the sweep removed it after the
    snapshot and before the pass or the first pass. The sweep removes only a blob
    that nothing references and that is older than 10 minutes, so that needs a
    blob that a snapshot references and the host unreferences and sweeps within
    the time that the copy takes after the blob's creation. Rule 25 finds such a
    loss.
15. **Mutable files.** It copies the remaining files of rule 2 with the rule 5
    retry: `config.json`, `config-history/`, `secret.key`, `secret.key.recovery`
    and its `.id`, `tsnet/`, `portal/`, `netbird/`, `zrok/`, `zrok-environment/`,
    `cloudflare-quick/`, `network-retirements/`, `flats/.lifecycle/` and each
    `restore-journal.json`.
16. **Survey again.** It lists what step 9 listed. When a flat directory or one
    of the three subdirectories was created, removed, renamed or replaced (another
    identity), when an entry of `flats/.lifecycle/` changed, or when a
    `restore-journal.json` appeared, disappeared or changed, a create, rename,
    delete or rollback ran during the copy. The command fails the backup.
17. **Verification.** It runs the checks of rules 23 to 26 on the copy in
    `PATH.partial-<random>`. It reads every written file again from the
    destination, so a failed write to the destination is found.
18. **Publish.** It writes `manifest.json` into the partial directory, syncs the
    files and the directory, and renames the partial directory to `PATH`. When
    `PATH` exists by then, the command refuses and removes the partial directory.
19. On any failure, and on an interrupt (Ctrl-C or SIGTERM) before step 18, the
    command removes the partial directory and exits with a failure. `PATH` never
    holds an incomplete copy. After a crash a `PATH.partial-<random>` can stay. It
    has no claim to be a copy, and it can be removed.
20. `secret.key` is read at step 9 and again at step 15. When the two reads
    differ, the command fails the backup, so a key change during the copy, such as
    a rotation, never pairs one key with the other key's `flats.db`.

## `flats backup --out`

```text
flats backup --out PATH [--exclude-secret-key] [--config PATH | --data DIR]
```

21. The command works with the host running and with the host stopped. It runs
    on the machine of the data directory and does not use the management API.
    The output names the folder, the number of files and bytes, whether a
    secret key is in the copy, the number of retries that it needed, and the text
    that the copy holds secrets and must be protected. It prints text. With
    `--json`, a failure prints as JSON, as for every command.

## `flats backup --verify`

```text
flats backup --verify PATH [--json]
```

22. The command needs no host, no configuration and no data directory. It reads
    `PATH` only and writes nothing.
23. **Manifest.** `manifest.json` must exist, name a format that this version
    knows (`format` is `1`), and list every file below `PATH` with its size and
    SHA-256. A listed file that is missing, a file below `PATH` that is not
    listed apart from `manifest.json`, and a size or hash that differs fail the
    check.
24. **Databases.** Each SQLite database in the manifest opens read-only and
    `PRAGMA quick_check` returns `ok`.
25. **Parts.** Every board and notebook is coherent with `flats.db` (rule 12). A
    blob counts as referenced under the definition of the sweep (blob sweep,
    step 4 of the revision store): every referenced blob of a flat exists under
    that flat's `revisions/blobs/` or `design/blobs/` in the copy.
26. **Key.** When the copy holds `secret.key` and `flats.db` holds sealed values,
    every flat secret and every operator key record with a stored value opens with
    that key. This is the check of `flats secret restore-key` (rule 22 of its
    spec). When the copy has no `secret.key`, the command says so and skips this
    check.
27. The output lists each failure with its path and reason, and ends with the
    count. It prints `ok` and the Flats version that made the copy when nothing
    failed.

## Exit codes and failures

28. A usage mistake, such as both `--out` and `--verify`, or neither, exits with
    status 2. A refusal exits with status 78, like `flats key`: an existing
    `PATH`, a missing or unusable folder, a `PATH` inside the data directory, and
    a data directory with no `flats.db`. Any other failure exits with status 1:
    an unstable file after 3 copies, a pair that stays incoherent after 3
    attempts, a change of structure during the copy (rule 16), a changed key
    (rule 20), a failed verification, and an input or output error such as a full
    disk. A failure of the kind that a second run can clear says so: "run the
    backup again".
29. A refusal writes nothing. A failure leaves nothing at `PATH`.

## Consistency and its limits

30. Each database in the copy is consistent by itself. The copy of `flats.db`
    and the copy of a board or notebook database are coherent, checked by rule 12.
    The blobs that the copied databases reference are in the copy, checked by rule
    25.
31. The copy does not hold one moment for the whole host. Two flats' databases
    come from different moments. A flat's `data/db.sqlite` and its `data/files/`
    come from different moments, as in a snapshot that `flats rollback` takes. The
    docs say so.
32. A create, rename, delete or rollback that runs during the copy fails it. The
    command finds it at step 16 by comparing the two surveys. Such a run leaves
    unseen only when it changes none of the names and none of the identities that
    the surveys list, for example an operation that fails and restores the old
    state. The command has no host pause, so the docs advise a stopped host when
    an exact copy of a host that changes constantly matters.

## Restore

33. To restore, the operator stops the host, runs `flats backup --verify PATH`,
    moves the old data directory away, copies `PATH/data` to the same path, copies
    `PATH/config.json` to the place of the configuration file, and starts the
    host. A copy at another path is not used, as
    [configuration](../configuration.md#what-to-back-up) says. A copy made with
    `--exclude-secret-key` needs `secret.key` put back from where the operator
    kept it, or `flats secret restore-key` with the recovery password, before the
    start.

## Compatibility

34. No schema change and no new dependency. The manifest has `format: 1`. A later
    Flats that changes the format raises it, and this command refuses a format it
    does not know.
35. A host that is not backed up this way behaves as before.

## Completion criteria

- A test starts a host with a document, a board, a notebook, a server flat with
  SQLite data and a flat secret and an operator key. It runs `flats backup`
  while writes to the board, the notebook and the server flat's database run, and
  restores the result into a new data directory at the old path. The host starts,
  serves every flat, opens the secret and the operator key, and holds at least
  the writes that were acknowledged before the backup began. `flats backup
  --verify` passes on the copy before the restore.
- Tests show that the board and notebook coherence check (rule 12) fails the
  attempt when an import falls between the two snapshots and passes on the next
  attempt, and fails the backup after 3 attempts.
- Tests show that each of these fails the backup with nothing at `PATH`: a flat
  renamed during the copy, a restore journal that appears, a changed `secret.key`,
  and a file that stays unstable.
- Tests show that `--verify` fails on each of: a changed byte in a copied file,
  a missing file, an extra file, a format it does not know, a database that fails
  `quick_check`, a board snapshot behind its cursor, a referenced blob that is
  missing, and a `flats.db` with sealed values and a key that does not open them.
- Tests show that each refusal of rule 8 and rule 28 writes nothing, and that an
  interrupt at each step leaves no `PATH` and removes the partial directory.
- A test shows that `--exclude-secret-key` leaves out `secret.key`, keeps the
  recovery copy, and that a restore of that copy plus `restore-key` starts the
  host.
- The Windows job runs the same tests, including a copy of a database that a
  process holds open.
- `docs/configuration.md` names this command as the supported way, no longer says
  that Flats has no supported way, and gives the restore of rule 33.
  `docs/surfaces.md` lists the command. The spec index lists this page.

## Open questions

None.
