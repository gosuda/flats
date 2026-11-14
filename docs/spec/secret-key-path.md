# Secret key path

Status: planned.

## Summary

Flats seals every flat secret and every stored operator key under one 32-byte
key, `<data>/secret.key`. A copy of the data directory holds the sealed values
and the key that opens them. This spec adds one setting, `host.secret_key_path`,
that puts the key in another place, such as a Docker secret or a systemd
credential. With the setting, the host never creates the key. It reads the file
or it refuses to start.

The request is item 1 of issue #224. Items 2 to 4 (rotation, injected secrets,
audit events) are separate changes, and item 5 (the recovery password) is done
([secret key recovery](secret-key-recovery.md)).

## Current state

- The host reads `<data>/secret.key` at start (`New` in
  `internal/core/service.go`). When the file is missing and `flats.db` holds no
  sealed value, it creates the key (`loadOrCreateKey`). When the file is missing
  and `flats.db` holds sealed values, it refuses to start (`CheckSecretKey`,
  which `Start` in `internal/app/serve.go` calls before any network or worker
  starts).
- Several places build the path `<data>/secret.key` themselves: the start of the
  host, `CheckSecretKey`, the sealing and opening of operator keys
  (`SealHostKey`, `OpenHostKey`), `ReadSecretKey`, `RestoreSecretKey`, the
  commands `flats key`, `flats secret backup-key`, `restore-key` and `reset`,
  and `flats backup` with its check of a copy.
- A wrong-size key at the default path is a plain error of `core.New` and exits
  with status 1. A missing key over sealed values is a refusal with status 78.
- `flats backup` reads the key at that fixed path. Its spec
  ([host backup](host-backup.md), scope and rule 22) says that it must read the
  key through the function that this item adds, and that a change of the key
  during a copy fails the copy. Its walk copies `secret.key` from the data
  directory as a host file unless `--exclude-secret-key` is set.
- The key commands and `flats backup` need the configuration file. A missing or
  invalid file is a refusal with status 78.
- [Configuration](../configuration.md#configjson) lists no key setting.

## Scope

In scope: the setting and its checks, the start of the host, every command that
reads or writes the key, `flats backup` and its check of a copy, the SIGHUP
reload, the docs.

Out of scope:

- Key rotation, key IDs, audit events and injected `fetch` secrets (items 2 to
  4 of #224). A rotation that replaces the file during a run has no defined
  effect until a restart.
- A check at start that the key opens the stored values. The default path has
  none either, and a wrong key shows when a value is used.
- An environment variable, a `flats serve` flag or a console field for the
  path. The setting is in `config.json` and is changed with `flats config`.
- A command that moves an existing key or makes a new one. The docs give the
  steps and a shell line.
- The installer and its systemd unit. The docs show a drop-in file.
- Anything for the recovery copy. It stays at `<data>/secret.key.recovery`.
- Changes to the default path, including its exit status for a wrong-size key.

## The setting

1. `host.secret_key_path` is a string. It is empty by default, and empty means
   the key is `<data>/secret.key` as before. A value must be an absolute path
   without a NUL byte, as `host.data_dir` must. It is not a `flats serve` flag.
   `flats config set` and `unset` change it offline, like the other host keys,
   and `flats config show` lists it. It applies at the next start (rule 8).
   Adding a key with a default leaves `schema_version` at 5.
2. A value must not lie inside the data directory. The default path
   `<data>/secret.key` is such a value and is refused too, because the setting
   exists to keep the key out of the data directory. The check compares the two
   cleaned absolute paths, ignores case on Windows, and does not resolve links.
   It runs when the configuration is loaded, so it also stops
   `flats config set host.data_dir` when the new directory contains the key
   path. `flats config set` and `unset` load the file first, so they cannot
   repair a file that this check refuses. The message says to use a path outside
   `host.data_dir` or to leave the setting empty, and to edit `config.json` by
   hand.
3. The location of the key is one function of the data directory and the
   setting. Every place that "Current state" names gets the path from it. It
   returns the path and whether the setting chose it. With the setting, it
   refuses in two more cases that the check of rule 2 cannot see.
   - A file, a directory or a link exists at `<data>/secret.key`. Two keys would
     be in use, and a copy of the data directory would hold the one that the
     setting was meant to keep out. The message says to move the file away.
   - The key exists, and its path after the operating system resolved every link
     lies inside the data directory after the same resolution. A data directory
     that is a link, or a key path that is a link into the data directory, ends
     here. The resolution does not unify every name that a file system can give
     one file. A Windows short name, a hard link and a bind mount of the
     directory of the key into the data directory escape it. The backup closes
     that gap with its own scan (rule 16), and the docs say to keep the key in a
     directory that no link and no mount reaches from the data directory.

## The host

4. With the setting empty, nothing changes.
5. With the setting, `Start` runs the checks of rule 3 and of the file at the
   place where it calls `CheckSecretKey` today, before any network listener or
   worker starts. It refuses when the path does not exist (a link to nothing is
   that case), is not a regular file once links are followed, is not 32 bytes,
   or cannot be read, and in the cases of rule 3. It holds when `flats.db` has no
   sealed value too. Each refusal is an action error that exits with status 78,
   so a service manager that does not restart on 78 does not loop on it. The
   refusal names the path and the setting.
6. The refusal for a missing file says that Flats never creates a key at a
   configured path. It names three ways out. The first is to put a key there; the
   docs show `head -c 32 /dev/urandom > PATH`. The second is to restore it with
   `flats secret restore-key`. The third is to clear the setting with
   `flats config unset host.secret_key_path`. For a link to nothing, the message
   names the link and says to repair the link or the mount, or to clear the
   setting, and it does not name `restore-key`, which refuses anything at the
   path (rule 12).
7. With the setting, the host never creates, writes, renames or deletes the key
   file, and it creates no temporary file beside it. A key on a read-only mount,
   in a read-only folder or with mode 0444 works. The host does not check the mode
   or the owner of the file. It must be readable by the user that runs the host,
   which is uid 65532 in the container image. What `Start` writes before the
   check is what it writes before `CheckSecretKey` today. That is the host lock
   and the Windows access list, the migration of `config.json` and `flats.db`
   with their copies, the move of `network-provider.json` into `backups/`, and the
   writes of an interrupted init or legacy migration, as rule 11 of
   [secret key recovery](secret-key-recovery.md) lists them and as `prepare` makes
   them. The setting adds no write to them.
8. Flat secrets use the key that `core.New` read at start and keeps in memory,
   as now. Operator keys do not. `SealHostKey` and `OpenHostKey` read the key
   file at each use, as now, for the console, `currentTailscaleKey` and the
   enrolment of a waiting key. With the setting, those reads go through the
   location of rule 3 and refuse in its cases. A file that is replaced during the
   run changes what operator-key actions open at once, and a file that appears at
   `<data>/secret.key` during the run makes them refuse. A replacement of the key
   during a run has no other defined effect (see "Scope"). The start reads the
   file twice, in the check of rule 5 and in `core.New`, and `core.New` makes the
   same checks through the same function and returns the same kind of refusal.
   A SIGHUP reload adopts a changed `host.secret_key_path` into its document and
   lists it as awaiting a restart (`AwaitRestart`), like the other restart keys,
   and the running host keeps the key that it holds. The key joins `restartKeys`
   in `internal/core/reload.go` and rule 8 of [config reload](config-reload.md).
9. Everything that seals or opens an operator key uses the same location. That
   means the console, `flats key set`, the key inputs of `flats serve`
   (`--tailscale-authkey-file`, `FLATS_TAILSCALE_AUTHKEY`) and the import of a
   legacy key file. With the setting and no file, none of them creates a key, and
   each fails with the message of rule 6.
10. The warning at start that the recovery copy no longer matches the key
    compares the copy with the key at this location.

## The commands

`flats key set`, `rm` and `ls`, and `flats secret backup-key`, `restore-key` and
`reset` find the data directory with `--config PATH` or `--data DIR`. Both ways
load the configuration file that they resolve. A missing or invalid file is a
refusal with status 78, as today, so the setting applies in both ways. Every
command of this list that reads or writes the key (`key set`, `backup-key`,
`restore-key` and `reset`) gets the location from the function of rule 3 and
refuses in the cases of rule 3. `key ls` and `key rm` touch no key and do not
call it, so they still work when the location is in one of those states.

11. `backup-key` reads the key at the location. The recovery copy and its ID file
    stay in the data directory.
12. `restore-key` keeps its rules ([secret key recovery](secret-key-recovery.md),
    rules 18 to 24). It runs with the host stopped, refuses when anything is at the
    location (a file, a directory or a link, a dangling link included), checks that
    the recovered key opens every stored value, writes only then, and never
    replaces a file. With the setting it writes the key to the configured path.
    Its synced temporary file `.secret-key-*` lies beside the configured path, not
    in the data directory, which changes the place that rule 23 there names. When
    the command cannot write there, for example on a read-only mount, it writes
    nothing, removes any temporary file, and says so and names `--out`.
13. `restore-key --out FILE` writes the recovered key to the new file FILE instead
    of the location, with mode 0600, after the checks of rule 12 that concern the
    recovered key. It does not require the location to be empty, because the
    location may hold a wrong key, and it makes no change at the location. It
    never replaces a file. The output says where the host must find the key. FILE
    may not lie inside the data directory. The operator moves FILE into the secret
    store and removes it.
14. `reset` keeps its rules (rules 25 to 31 there), and finds the key at the
    location. It refuses while the key at the location opens the stored values.
    With the setting, it says after it deleted the values that the next start
    needs a key at the path, because Flats will not create one.
15. A key at the location that is not a usable key (a directory, a wrong size)
    gets the message of the existing commands, with the path of the location.

## Backup

16. `flats backup` takes the location from the configuration file that it
    resolves, as the commands above do, and refuses in the cases of rule 3 in the
    preflight, before it writes anything. With the setting, the key is therefore not
    in the data directory. While the setting is on, the walk leaves out
    `<data>/secret.key` as it does with `--exclude-secret-key` and lists it in
    `excluded`, so a file that appears there during the copy is not copied. When
    the command has read the key (rule 17), step 20 also scans every file of the
    copy for the 32 bytes of the key, and a hit fails the backup with the kind of
    failure that a second run cannot clear. That scan holds for every name that
    a file system can give the key file, which the checks of rule 3 do not. The
    manifest records
    `secret_key: "external"`, and the output says that the key is kept at the
    path and must be backed up apart from the copy. The manifest keeps
    `format: 1`. `external` is a new value of an existing field, and a `--verify`
    of an older release reports any value other than `included` as a copy without
    a key.
17. With the setting, the command reads the key at the location at step 13 and
    again at step 18 of [host backup](host-backup.md), and fails the backup when
    the two reads differ or the file is gone (rule 22 there). At step 20 it checks
    the key it read against the sealed values of the copy, as rule 28 there
    does. `Run` passes the key bytes to that check, and the standalone `--verify`
    has none (rule 19). A key that does not open the sealed values fails the
    backup with the kind of failure that a second run cannot clear. A key that
    cannot be read, because it is missing or unreadable, refuses the backup in the
    preflight (step 12), before the partial directory exists, and the message
    names `--exclude-secret-key`. Step 13 reads the key again for rule 22.
18. With the setting, `--exclude-secret-key` means that the command does not
    read the key. It skips the reads of rule 17 and the key check. The manifest
    still records `external`. Without the setting the flag keeps its meaning
    (rule 24 of host backup).
19. `flats backup --verify` needs no configuration. For a copy with
    `secret_key: "external"` it reports, as a note and not as a failure, that the
    key is outside the copy and that the sealed values were not checked against a
    key.
20. A restore (rule 35 of host backup) of a copy with `secret_key: "external"`
    brings back `PATH/config.json`, which holds the setting, and the operator
    puts the key at the configured path before the start. The docs say so.
21. The key commands and `flats backup` run as processes of their own. A key
    that only the service process can see, such as a systemd `LoadCredential=`
    file under `$CREDENTIALS_DIRECTORY`, or a Docker secret that is mounted into
    the service container, is missing for a process outside the service. A
    command that runs inside the container with `docker exec` sees the secret. A
    one-off container and a process on the host do not. For such a key, `flats backup`
    needs `--exclude-secret-key`, `restore-key` needs `--out`, and the other key
    commands cannot run unless the operator gives them the same mount. The docs
    say this, and they show a deployment that works for every command, a plain
    file that the service user can read, for example `/etc/flats/secret.key` with
    mode 0400.

## Docs

22. `docs/configuration.md` has the row of the setting, the key lifecycle (what
    the key protects, what to back up and what to store apart, how to move an
    existing key out of the data directory and back, what a refusal means, and the
    hand edit that rule 2 asks for), and the effect on a downgrade (rule 24).
    `docs/container.md` shows a Docker secret and a Compose secret with
    `config set host.secret_key_path`. It also shows a systemd drop-in with
    `LoadCredential=` and the limits of rule 21. `docs/design.md` ("Secrets") says
    where the key may be. The spec index lists this page. The out-of-scope notes
    and rules 12, 22, 24, 28, 30, 31 and 35 of [host backup](host-backup.md), rule
    8 of [config reload](config-reload.md), and rules 19, 23 and 31 of
    [secret key recovery](secret-key-recovery.md) change as this spec says. Rule
    12 here changes rules 19 and 23 there, and rule 14 here changes rule 31.

## Compatibility

23. No database change, and `schema_version` stays 5. A host without the setting
    behaves as before. An older Flats that reads a `config.json` with the new key
    refuses it as an unknown key, as for any key it does not know.
24. To go back to an older Flats, the operator stops the host and copies the key
    to `<data>/secret.key`. Then the operator runs
    `flats config unset host.secret_key_path` with the new Flats, and then starts
    the older one. The order matters, because the new Flats refuses a file at
    `<data>/secret.key` while the setting is on (rule 3), and `config unset` is
    not one of the commands that apply that refusal.
25. The key file format is unchanged: 32 raw bytes.

## Completion criteria

Tests that need a file the user cannot read, a read-only folder or mode 0444 run
on Unix as a user that is not the superuser, and skip elsewhere, as the existing
tests of `internal/app` do.

- Configuration tests. A relative path, a NUL byte and a path inside the data
  directory are refused, with the value and the reason. The default path
  `<data>/secret.key` is refused. An empty value is accepted. `config set` and
  `unset` round trip, and `config show` lists the key. `config set host.data_dir`
  refuses a directory that contains the key path. Every example file of an
  earlier schema still migrates, and `schema_version` stays 5. The parser refuses
  an unknown key in the `host` object, which is the rule that makes an older
  release refuse the new key. The tests build no older binary.
- Host tests with the setting. A missing file, a link to nothing, a directory, a
  file of 31 and of 33 bytes, an unreadable file, a file at `<data>/secret.key`,
  and a key that is a link into the data directory each stop the start before any
  listener opens, exit with status 78, name the path and the setting, and leave no
  `.secret-key-*` file and no change in the folder of the key. They hold when
  `flats.db` has no sealed value. A data directory that is a link to the directory
  that holds the key is refused as well. A valid key starts the host, in a
  read-only folder and with mode 0444, with no file created. The host opens a
  sealed flat secret and a stored operator key with that key. With the setting
  empty, the first start creates the key as before. A SIGHUP reload with a changed
  value lists the key as awaiting a restart, and the running host keeps its key.
- A test shows that `flats key set` with the setting and no file refuses and
  creates nothing.
- Command tests with `--config` and with `--data` and a configuration file. A
  missing configuration file is a refusal. `backup-key` reads the configured key.
  `restore-key` writes to the configured path, creates its temporary file beside
  that path, never replaces a file, and with `--out` writes a new 0600 file,
  changes nothing at the location, works when the location holds a wrong key,
  and refuses an `--out` inside the data directory. With a read-only location it
  writes nothing, leaves no temporary file and names `--out`. `reset` refuses
  while the configured key opens the values, and after a reset the next start
  refuses until a key is put at the path. Each key command refuses in the cases
  of rule 3.
- Backup tests. The manifest has `external`, and no file of the copy holds the 32
  bytes of the key (a test scans every file of the copy). The output names the
  path. A leftover `<data>/secret.key` and a key that resolves into the data
  directory refuse the backup before it writes anything. A hard link to the key
  under another name in the data directory fails the backup at step 20 through
  the scan of rule 16, with the kind of failure that a second run does not clear.
  A `<data>/secret.key` that appears during the copy is not copied and is listed
  in `excluded`. A key that changes between step 13 and step 18 fails the copy. A
  key that does not open the sealed values fails the backup and says that a
  second run does not help. `--exclude-secret-key` skips the reads, the scan and
  the check. A missing or unreadable key refuses the backup in the preflight,
  makes no partial directory, and names the flag. `--verify` reports the note. A
  copy restored with the key put back at the path starts a host that opens every
  sealed value.
- Operator-key tests with the setting. The console and `flats key set` open and
  seal with the configured file at each use. Replacing the file between two uses
  changes what they open, and a file that appears at `<data>/secret.key` makes
  them refuse (rule 8). `flats key ls` and `flats key rm` work while the location
  is in a state that rule 3 refuses.
- Docs. `docs/configuration.md`, `docs/container.md` and `docs/design.md` say
  what rules 21, 22 and 24 list, and the documentation checks pass.

## Open questions

None.
