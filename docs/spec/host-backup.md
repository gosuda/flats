# Host backup

Status: implemented.

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
host is never paused. The copy checks itself, and it fails, to be run again,
when a change during the copy could make it inconsistent.

## Before this change

- [Configuration](../configuration.md#what-to-back-up) says to stop the host and
  copy the whole data directory, and that Flats has no supported way to take a
  backup from a running host.
- A plain copy of a live SQLite database can lose recent commits. SQLite's
  backup API copies one database, not the host.
- A document, a board and a notebook each keep an outbox in a database of their
  own, and `flats.db` keeps a cursor into it (`content_state.import_store` and
  `import_seq`). The outbox databases and `flats.db` must come from one moment,
  or a flat does not start, loses its newest revisions, or stops importing
  ([configuration](../configuration.md#what-to-back-up),
  [boards](../revision-store.md#boards)).
- Revision blobs are written once. The sweep deletes a blob that nothing
  references and that is older than 10 minutes
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
  fail the copy (rule 22).

## The copy

1. The copy is a new directory `PATH` that holds `manifest.json`, `data/` and,
   only when the configuration file lies outside the data directory,
   `config.json` and the `config-history/` folder that the host keeps beside it.
   `data/` mirrors the data directory. A configuration file inside the data
   directory is copied once, as `data/config.json`, and so is its
   `config-history/`.
2. The copy holds everything that
   [What to back up](../configuration.md#what-to-back-up) names, including the
   optional `cloudflare-quick/`, and `config-history/`. Each path belongs to one
   class, and the class decides the step and the way of copying:

   | Class | Paths below the data directory | Steps | Copy |
   | --- | --- | --- | --- |
   | Write-once files | `flats/<slug>/revisions/blobs/`, `flats/<slug>/design/blobs/`, `versions/`, `draft-revs/`, and `snapshots/` | 14 and 17 | rule 6 |
   | Databases | `flats.db`, and for each flat `data/db.sqlite`, `previews/<host>/db.sqlite`, `board/board.db` and `notebook/notebook.db` | 15 and 16 | rule 4 |
   | Guest files | `flats/<slug>/data/files/`, and `previews/<host>/` apart from its database | 18 | rule 7 |
   | Host files | everything else that rule 3 does not leave out: `config.json`, `config-history/`, `secret.key`, `secret.key.recovery` and its `.id`, `tsnet/`, `portal/`, `netbird/`, `zrok/`, `zrok-environment/`, `cloudflare-quick/`, `network-retirements/`, `flats/.lifecycle/` and each `restore-journal.json` | 18 | rule 6 |

3. The command leaves out, and the manifest lists in `excluded`:
   - `logs/`, `cache/`, `run/`, `runtime/`, `backups/` and `flats.lock`;
   - the `-wal`, `-shm` and `-journal` file of each SQLite database, because the
     snapshot of rule 4 holds its commits;
   - below a flat's directory: `health-check/`, `restore-trial/` and the
     temporary directories of a restore (`data.restore`, `data.restore-old`);
   - below `revisions/` and `design/`: `tmp/`;
   - a snapshot that is being written (rule 14).
4. Each SQLite database is copied as a snapshot made with `VACUUM INTO` from a
   read-only connection with a busy timeout of 5 s. The snapshot holds every
   commit up to its moment, has no `-wal` or `-shm`, and is consistent by itself.
   The command snapshots one database at a time, so that the host's own
   checkpoints resume between two snapshots, with one exception: the group of
   rule 15 starts together. A snapshot that cannot start within the busy timeout
   is tried again, up to 3 times. The command syncs each snapshot file and
   computes its SHA-256 after the last snapshot, so that this work does not
   lengthen the time between two snapshots. The manifest lists each database in
   `databases`, with its path and kind (`flats`, `data`, `preview`, `board` or
   `notebook`).
5. A flat's `data/db.sqlite` is the database of a server flat, of a document
   (the docs worker's) or of a flat with no database yet. It is an outbox
   database when its flat is a document. A `board.db` and a `notebook.db` are
   outbox databases. Rule 16 treats the three the same way.
6. A file of the classes write-once and host is copied as it is, with its content
   read while the host may write it. It is stable when its size and modification
   time are the same before and after the copy. An unstable file is copied again,
   up to 3 times. A host file that is still unstable fails the backup. A
   write-once file that is still unstable is skipped and listed in `skipped`,
   because the host writes such a file once and a partial copy of one that is
   being written is not part of a finished state. On Windows the command opens a
   file with sharing for reading, writing and deleting, as `durable.ReadFile`
   does. The copy computes the SHA-256 of each file from the bytes it reads. A
   write-once file that the first pass skipped and the second pass copies is
   listed in `files` and not in `skipped`.
7. A guest file is copied once with the same read. When it changes during the
   copy, the command reads it again up to 3 times and keeps the last read. It
   then lists the path in `unstable`, and the backup does not fail. A flat may
   write its files all the time, and rule 33 says what that means.
8. A file or directory that is gone when the command opens it is skipped and
   listed in `skipped` when the host removes it on purpose while it runs: a
   write-once or guest file (pruning, the sweep, the flat itself), a flat's
   `previews/` directory and anything below it (a preview expires, directory and
   database with it), a file of
   `config-history/` (the host keeps the newest 10), and an entry of
   `network-retirements/` (a finished retirement). For `flats.db`, the
   `data/db.sqlite`, `board.db` and `notebook.db` of a flat, and every other host
   file, the command waits 100 ms and tries again, up to 3 times, because a
   restore of a legacy snapshot replaces `db.sqlite` by a rename. It fails the
   backup when the path is still gone.
9. The command copies regular files and directories. It does not follow a
   symbolic link or a Windows junction, and it does not open a FIFO, a socket or
   a device file. It lists each such entry in `skipped` with its path and kind.
   The two exceptions are `secret.key` and `secret.key.recovery`, which the host
   follows when they are links (rule 13 of the
   [secret key recovery](secret-key-recovery.md) spec). The command copies the
   bytes of their targets. This rule applies to entries below the data directory.
   The data directory itself may be a link, and the command follows it.
10. On Unix the command creates files with mode 0600 and directories with mode
    0700. On Windows a new file takes the access list of its folder, and the
    command sets none. A copy holds every secret that the host holds, apart from
    `secret.key` when it is excluded: the sealed values and operator tokens in
    `flats.db`, and the identities in `tsnet/`, `portal/`, `netbird/`,
    `zrok-environment/` and `cloudflare-quick/`. The docs say to keep a copy as
    protected as the data directory and to use a folder that only the operator
    can write. The command does not check who can write the folder.
11. The command changes no content of a flat. It opens databases read-only and
    files for reading, and it takes no lock of the host. SQLite's read lock does
    not block the host's writes in WAL mode. Opening a database that a stopped
    host left with a `-wal` file and no `-shm` creates an empty `-shm`. That needs
    a writable folder, and the command stops with that error when it has none. A
    copy of a stopped host is exact, because nothing changes during the copy.

## Order and checks

The order matters, because the parts depend on each other. The command runs
these steps and no other:

12. **Preflight.** It selects the configuration and the data directory with
    `--config PATH` or `--data DIR`, as `flats secret backup-key` does. It does
    not call the host, so `--url` does not apply. It refuses before writing
    anything when `--out` names something that exists, even a link; when its
    folder does not exist or is not a folder; when it is inside the data
    directory; when the data directory is inside it; or when `flats.db` is not the
    database of a host of this release, which means a file that is no Flats
    database or a `user_version` other than the one this binary expects (rule 25).
    A host of an older release migrates its `flats.db` when it starts, and the
    refusal says so and names the copy by hand. This check comes last, because
    it is the only refusal that opens a database, and opening the database of a
    stopped host that left a `-wal` file and no `-shm` creates an empty `-shm`
    (rule 11). It creates a new
    directory `PATH.partial-<random>` in the same folder, with mode 0700. All
    writes go there.
13. **Survey.** For each flat directory below `flats/` it lists the directory and
    those of its subdirectories `data/`, `board/` and `notebook/` that exist.
    Each has the file identity that the system gives (the inode on Unix, the file
    index on Windows) and its creation time where the system has one. It lists
    the entry names of `flats/.lifecycle/` and each `restore-journal.json` with
    its size and modification time. It reads `secret.key`. A journal that exists
    now is data and is copied. It is not a failure.
14. **Write-once files, first pass.** It copies the write-once class. A snapshot
    is the file `<kind>-<ms>.sqlite` and, in the current format, the directory
    `<kind>-<ms>.sqlite.data/`, which is complete when it holds
    `.flats-snapshot-complete`. The host restores the directory of a complete
    snapshot and otherwise the file alone (`installSnapshot`). The command copies
    the file of every snapshot, and the directory of every complete one. A
    snapshot with a directory and no marker is being written or was abandoned by
    a crash. The command leaves it out whole and lists it in `excluded`. A
    snapshot with no directory is a legacy snapshot, and the command copies its
    file.
15. **Databases.** It snapshots, one at a time, every database of the flats that
    is not in the group below: `data/db.sqlite`, each preview database,
    `board.db` and `notebook.db`. It then snapshots `flats.db` and the outbox
    databases (rule 5) of the flats that have a cursor in the live `flats.db`
    (`content_state.import_store` is not empty) at the same time. Each snapshot of
    this group opens its connection first, and all of them start together. A
    member that fails before its start does not hold the others. The group holds
    at most 64 outbox databases, in the order of the flat names, so that the
    command holds a bounded number of files open on a host with many flats. The
    other outbox databases are snapshotted one at a time before the group. A flat
    that gets its cursor after the command read the live `flats.db` is found by
    rule 16.
16. **Coherence.** An outbox database (rule 5) fits the cursor of its flat in the
    copy of `flats.db` when the cursor lies between what the outbox has
    acknowledged and what it has created: `acked <= import_seq <= last`. `last` is
    the highest outbox sequence (`sqlite_sequence` of the table `outbox`, or
    `flats_docs_outbox`). `acked` is the lowest sequence that is still in that
    table, minus one, or `last` when the table is empty. A running host keeps this
    true at every moment: it creates an entry, `flats.db` imports it and moves the
    cursor, and then the host acknowledges the entry by deleting it
    ([boards](../revision-store.md#boards) and
    [notebooks](../revision-store.md#notebooks), importing, step 1; for a document,
    [Importing](../revision-store.md#importing), step 1). When the store id of the
    database (the `store` row of its `meta` table, or `flats_docs_meta` for a
    document) differs from `import_store`, there is nothing to compare. A flat with
    no cursor, and a flat whose outbox database does not exist, have nothing to
    compare. The host runs the upper half of this test when it opens a board or a
    notebook (`checkImportCursor`) and no test for a document. The command applies
    the whole test to all three.

    Two snapshots that do not start at one moment can break the test in two ways:

    - `import_seq > last`: an entry was created and imported after the snapshot of
      the database and before the snapshot of `flats.db`. A restored board or
      notebook in that state does not start. A restored document starts, and its
      next import acknowledges through `import_seq`, which deletes every outbox
      entry up to that number, so the new entries that reuse those numbers are
      dropped unimported ([docs app](../internal/docs-app.md)).
    - `acked > import_seq`: entries were imported and acknowledged after the
      snapshot of `flats.db` and before the snapshot of the database. Their
      revisions are in no database of the copy, and the restored host does not
      notice that.

    An entry that is only created in the gap, or that the `flats.db` snapshot
    imported and the database snapshot still holds, keeps the test true. The second
    entry is at or before the cursor, and the host acknowledges it again.

    The group of rule 15 starts together to make the gap short, and the test holds
    for either order of two snapshots. When the test fails, the command snapshots
    the group again as a whole. A database that the check found and that was not in
    the group joins it. Then it checks every outbox database again, up to 3
    attempts in all. After the third it fails the backup. Under constant writes to
    several outbox databases the attempts can fail again and again, and the docs
    advise a stopped host then.
17. **Write-once files, second pass.** It copies the write-once files that are new
    since the first pass. This pass starts after the last snapshot. A blob that a
    snapshot references is in the copy, unless the host created the blob after the
    first pass, unreferenced it, and the sweep deleted it before this pass. The
    sweep deletes only a blob that is older than 10 minutes, so that needs a copy
    that runs longer than that. Rule 27 finds such a loss.
18. **Guest files and host files.** It copies the guest class and the host class.
19. **Survey again.** It lists what step 13 listed. The backup fails when a flat
    directory appeared or disappeared, when a directory of step 13 that existed
    then has another identity or is gone, when an entry of `flats/.lifecycle/`
    changed, or when a `restore-journal.json` appeared, disappeared or changed. A
    `data/`, `board/` or `notebook/` directory that did not exist at step 13 and
    exists now is not a failure, because a flat creates `data/` at its first use of
    a database. A create, rename, delete or data restore ran during the copy
    otherwise.
20. **Verification.** It runs the checks of rules 25 to 28 on the copy in
    `PATH.partial-<random>`, with the manifest that it holds in memory. Its first
    check reads every written file again from the destination and compares it with
    the hash that rule 4, 6 or 7 computed, so a failed write to the destination is
    found.
21. **Publish.** It writes `manifest.json` into the partial directory, syncs the
    files and the directory, and moves the partial directory to `PATH` in a way
    that fails when anything exists there, an empty directory included. When
    something exists by then, the command refuses and removes the partial
    directory.
22. `secret.key` is read at step 13 and again at step 18. When the two reads
    differ, the command fails the backup, so a key change during the copy, such as
    a rotation, never pairs one key with the other key's `flats.db`.
23. On any failure, and on an interrupt (Ctrl-C or SIGTERM) before step 21, the
    command removes the partial directory and exits with a failure. `PATH` never
    holds an incomplete copy. After a crash a `PATH.partial-<random>` can stay. It
    has no claim to be a copy, and it can be removed.

## `flats backup --out`

```text
flats backup --out PATH [--exclude-secret-key] [--config PATH | --data DIR] [--json]
```

24. The command works with the host running and with the host stopped. It runs
    on the machine of the data directory and does not use the management API.
    With `--exclude-secret-key`, `data/secret.key` is not copied and the manifest
    records `secret_key: "excluded"`. The recovery copy `secret.key.recovery` and
    its `.id` file stay, so a copy without the key can still be restored with
    `flats secret restore-key`. A holder of the copy can guess the recovery
    password offline, as
    [secret key recovery](secret-key-recovery.md#the-recovery-copy) says. The
    output names the folder, the number of files and bytes, whether a secret key
    is in the copy, the number of retries that it needed, the skipped and unstable
    paths, and the text that the copy holds secrets and must be protected. It
    prints text. With `--json`, a failure prints as JSON, as for every command.

## `flats backup --verify`

```text
flats backup --verify PATH [--json]
```

25. The command needs no host, no configuration and no data directory. It reads
    `PATH` and writes nothing. It opens every database with `mode=ro` and
    `immutable=1`, which creates no file and changes no byte, and it does not use
    `store.Open`, which migrates an older database. A `flats.db` whose
    `user_version` is not the one that this binary expects stops the command with
    a refusal that names the Flats version in the manifest and says to run
    `--verify` with that version. A `-wal`, `-shm` or `-journal` file beside
    `flats.db` fails the check, and the command then does not open `flats.db` in
    a way that could create a `-shm`. A copy made by this command holds none of
    them. This failure exits with status 1, not with the refusal of status 78,
    because the command does not read the schema version of such a database.
26. **Manifest.** `manifest.json` must exist, name a format that this version
    knows (`format` is `1`), and list every file below `PATH` with its size and
    SHA-256. A listed file that is missing, a file below `PATH` that is not listed
    apart from `manifest.json`, and a size or hash that differs fail the check.
    The manifest also records the number of attempts that rule 16 needed
    (`attempts`).
27. **Databases and parts.**
    - Each database that the manifest lists in `databases` passes
      `PRAGMA quick_check` with the result `ok`.
    - Every outbox database is coherent with `flats.db` (rule 16).
    - Every blob that the flat needs exists in the copy, under that flat's
      `revisions/blobs/` or `design/blobs/`. A flat needs the hashes in
      `content_head`, `revision_files` and `content_sources` of `flats.db`, and
      the `hash` of every row of the table `assets` of its `board.db` and its
      `notebook.db`. The blob sweep keeps more than this, such as the blobs that
      live undo state names, and the check does not read them.
28. **Key.** When the copy holds `secret.key` and `flats.db` holds sealed values,
    every flat secret and every operator key record with a stored value opens with
    that key. The code is the one of `flats secret restore-key` (rule 22 of its
    spec), reading the immutable connection of rule 25. When the copy has no
    `secret.key`, the command says so and skips this check.
29. The output lists each failure with its path and reason, and ends with the
    count. It prints `ok` and the Flats version that made the copy when nothing
    failed.

## Exit codes and failures

30. A usage mistake, such as both `--out` and `--verify`, or neither, exits with
    status 2. A refusal exits with status 78, like `flats key`: an existing
    `PATH`, a missing or unusable folder, a `PATH` inside the data directory, a
    data directory with no `flats.db`, a `flats.db` that is no Flats database, and
    a `user_version` that rule 25 refuses.
    Any other failure exits with status 1: an unstable host file after 3 copies, a
    database that stays incoherent after 3 attempts, a change of the layout during
    the copy (rule 19), a changed key (rule 22), a failed verification, and an
    input or output error such as a full disk. A failure that a second run can
    clear says so: "run the backup again".
31. A refusal writes nothing at `PATH`, and the only file that it can create
    anywhere is the empty `-shm` that the schema refusal of rule 12 can leave
    beside the database of a stopped host. A failure leaves nothing at `PATH`.

## Consistency and its limits

32. Each database in the copy is consistent by itself. Each outbox database is
    coherent with `flats.db` (rule 16). The blobs that the copied databases need
    are in the copy (rule 27).
33. The copy does not hold one moment for the whole host. Two flats' databases
    come from different moments. A flat's `data/db.sqlite` and its `data/files/`
    come from different moments, as in a snapshot that `flats rollback` takes, and
    a guest file that a flat writes all the time can be partial. The docs say so.
34. Rule 19 detects the creation, the removal and the rename of a flat directory,
    a data restore that replaces a surveyed directory, and a rollback whose journal
    still exists at step 19 or differs. It does not detect a rename that returns the
    old name, a restore of a legacy snapshot that replaces only `db.sqlite` and
    whose journal appears and disappears between the two surveys, and an operation
    that fails and leaves every surveyed entry as it was. The command has no host
    pause, so the docs advise a stopped host when an exact copy of a host that
    changes constantly matters.

## Restore

35. To restore, the operator stops the host, runs `flats backup --verify PATH`,
    moves the old data directory away, copies `PATH/data` to the same path, copies
    `PATH/config.json` and `PATH/config-history/`, when they exist, to the place of
    the configuration file and the folder beside it, and starts the host. A copy at another path is not used, as
    [configuration](../configuration.md#what-to-back-up) says. A copy made with
    `--exclude-secret-key` needs `secret.key` put back from where the operator
    kept it, or `flats secret restore-key` with the recovery password, before the
    start.

## Compatibility

36. No schema change and no new dependency. The manifest has `format: 1`. A later
    Flats that changes the format raises it, and this command refuses a format it
    does not know.
37. A host that is not backed up this way behaves as before.

## Completion criteria

- A test starts a host with a board, a notebook, a server flat with SQLite data
  and files below `data/files/`, a flat secret and an operator key. It runs
  `flats backup` while writes to the board and the notebook run, and restores the
  result into a new data directory at the old path. The host starts, serves every
  flat, reads the files, opens the secret and the operator key, holds at least
  the board elements and the notebook pages that the host had acknowledged before
  the backup began, accepts a new write to the board and to the notebook, and the
  board continues its sequence above the one that the copy had reached.
  `flats backup --verify` passes on the copy before the restore. This test has no
  document and no running server flat: they need the docs worker and a flat
  process, which the unit tests do not start. The package tests cover the outbox
  database of a document and the database of a server flat with fixture
  databases. A backup of a host with a running document and a running server flat
  under write load has no automatic test, and this is a known gap.
- For each kind of outbox database (a document, a board, a notebook) and for each
  order of the two snapshots, a test shows that a change between the snapshot of
  the database and the snapshot of `flats.db` that breaks the test of rule 16
  fails it on the first attempt, passes on the next, and fails the backup when it
  happens on three attempts. The tests set the order, which production code
  leaves to the group of rule 15. A test shows that an entry that is only created
  in the gap, and an entry that is imported but not yet acknowledged, do not fail
  the test. A test shows the three states of rule 16 on a copy directly: the
  cursor below `acked`, between `acked` and `last`, and above `last`. A test shows
  that a group of snapshots does not wait for a member that failed, that a damaged
  database fails the backup with nothing at `PATH`, and that a host with more
  outbox databases than one group holds is copied and verified.
- Tests show that these fail the backup with nothing at `PATH`: a flat created,
  renamed and deleted during the copy, a restore journal that appears, a changed
  `secret.key`, a host file that stays unstable, and a database that stays
  incoherent. A test shows that the first use of the database of a flat that has
  no `data/` yet does not fail the backup. A test shows that a preview that
  expires during the copy, a file of `config-history/` that the host removes, and a
  finished retirement marker do not fail the backup and are listed in `skipped`,
  and that a `db.sqlite` that is missing for the whole retry fails it.
- Tests show the result for a skipped entry (a symbolic link, a FIFO, a link to a
  file outside the data directory), a write-once file and a guest file that vanish
  during the copy, a guest file that stays unstable (the backup succeeds and lists
  it), a snapshot with no directory (copied), a complete snapshot (copied) and a
  snapshot with a directory and no marker (left out). A restore that uses the
  legacy snapshot of a copy has no test: the copy holds its file byte for byte,
  and the restore is the host's own.
- Tests show that `--verify` fails on each of: a changed byte in a copied file, a
  missing file, an extra file, a format it does not know, a `user_version` it does
  not expect, a database that fails `quick_check`, an outbox database below or
  above its cursor (a document, a board and a notebook), a needed blob that is
  missing, a `flats.db` with sealed values and a key that does not open them, and a
  `-wal` file beside `flats.db`. A test runs `--verify` twice on a copy, once on a
  copy with another schema version and once on a copy with a `-wal` file, and shows
  that `PATH` keeps the same files with the same sizes and modification times.
- Tests show that each refusal of rules 12 and 30 writes nothing at `PATH`, that a
  refusal for `--out` creates no `-shm` file beside the database of a stopped host,
  that the exit codes are 2, 78 and 1 as rule 30 says, that an interrupt at each of
  the steps 12 to 20 leaves no `PATH` and removes the partial directory, and that a
  `PATH` that is an empty directory is refused. Step 21 has no interrupt test: the
  rename is the last act, and an interrupt after it leaves the complete `PATH`.
- A test shows that `--exclude-secret-key` leaves out `secret.key`, keeps the
  recovery copy, and that a restore of that copy plus `restore-key` starts the
  host. A test shows that the copy has the modes of rule 10 on Unix, and a test
  shows the output of rule 24 and rule 29.
- A test shows a backup of a stopped host with a `-wal` file and no `-shm`, and
  that the copy holds the commits of the `-wal` file. The error for a folder that
  is not writable has no test, because a test owns its folder and may run as the
  superuser. A test shows that a snapshot that cannot start within the busy timeout
  is tried again and succeeds on a later try, and fails the backup after the third.
- The Windows job runs the same tests, including the backup of a running host,
  which holds its databases open.
- `docs/configuration.md` names this command as the supported way, no longer says
  that Flats has no supported way, and gives the restore of rule 35.
  `docs/surfaces.md` lists the command. The spec index lists this page.

## Open questions

None.
