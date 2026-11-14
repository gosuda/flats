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

## Before this change

- The host reads `<data>/secret.key` at start (`New` in
  `internal/core/service.go`). When the file is missing and `flats.db` holds no
  sealed value, it creates the key (`loadOrCreateKey`). When the file is missing
  and `flats.db` holds sealed values, it refuses to start (`CheckSecretKey`).
- Several places build the path `<data>/secret.key` themselves: the start of the
  host, `CheckSecretKey`, the sealing and opening of operator keys
  (`SealHostKey`, `OpenHostKey`), `ReadSecretKey`, `RestoreSecretKey`, the
  commands `flats key`, `flats secret backup-key`, `restore-key` and `reset`,
  and `flats backup` with its check of a copy.
- `flats backup` reads the key at that fixed path. Its spec
  ([host backup](host-backup.md), scope and rule 22) says that it must read the
  key through the function that this item adds, and that a change of the key
  during a copy fails the copy.
- [Configuration](../configuration.md#configjson) lists no key setting.

## Scope

In scope: the setting and its checks, the start of the host, every command that
reads or writes the key, `flats backup` and its check of a copy, the docs.

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
- Anything for the recovery copy. It stays at `<data>/secret.key.recovery`.

## The setting

1. `host.secret_key_path` is a string. It is empty by default, and empty means
   the key is `<data>/secret.key` as before. A value must be an absolute path
   without a NUL byte, as `host.data_dir` must. It is not a `flats serve` flag.
   It applies at the next start, and `flats config set` and `unset` change it
   offline, like the other host keys. Adding a key with a default leaves
   `schema_version` at 5.
2. A value must not lie inside the data directory, and the default path
   `<data>/secret.key` is such a value, so it is refused too: the setting exists
   to keep the key out of the data directory, and a key at another name inside it
   would be copied by `flats backup` and kept by `--exclude-secret-key`. The
   check compares the two cleaned absolute paths, and on Windows it ignores
   case. It does not resolve links. It runs when the configuration is loaded, so
   it also stops `flats config set host.data_dir` when the new directory contains
   the key path. The message says to use a path outside `host.data_dir`, or to
   leave the setting empty.
3. The location of the key is one function of the data directory and the setting.
   Every place that the list under "Before this change" names gets the path from it. It
   returns the path and whether the setting chose it.

## The host

4. With the setting empty, nothing changes.
5. With the setting, the host reads the file at the path and follows links, as
   `ReadSecretKey` does for `secret.key`. It refuses to start, before any network
   or worker starts and before it writes anything, when the path does not exist
   (a link to nothing is that case), is not a regular file once links are
   followed, is not 32 bytes, or cannot be read. The refusal names the path and
   the setting. It holds when `flats.db` has no sealed value too.
6. The refusal for a missing file says that Flats never creates a key at a
   configured path. It names three ways out: put a key there (the docs show
   `head -c 32 /dev/urandom > PATH`), restore it with `flats secret restore-key`,
   or clear the setting with `flats config unset host.secret_key_path`. It is a
   refusal that exits with status 78, like `CheckSecretKey`.
7. With the setting, the host never creates, writes, renames or deletes the key
   file, and it creates no temporary file beside it. A key on a read-only mount,
   in a read-only folder or with mode 0444 works. The host does not check the mode
   or the owner of the file. It must be readable by the user that runs the host,
   which is uid 65532 in the container image.
8. The host reads the key once at start and keeps it in memory, as now. A change
   of the file during the run has no effect until the next start.
9. Everything that seals or opens an operator key uses the same location: the
   console, `flats key set`, the key inputs of `flats serve`
   (`--tailscale-authkey-file`, `FLATS_TAILSCALE_AUTHKEY`) and the import of a
   legacy key file. With the setting and no file, none of them creates a key;
   each fails with the message of rule 6.
10. The warning at start that the recovery copy no longer matches the key
    compares the copy with the key at this location.

## The commands

`flats key set`, `rm` and `ls`, and `flats secret backup-key`, `restore-key` and
`reset` find the data directory with `--config PATH` or `--data DIR`. Both ways
load the configuration file that they resolve, when it exists, so the setting
applies in both. With no configuration file, the key is `<data>/secret.key`.

11. `backup-key` reads the key at the location. The recovery copy and its ID file
    stay in the data directory.
12. `restore-key` keeps its rules ([secret key recovery](secret-key-recovery.md),
    rules 18 to 24): it runs with the host stopped, needs a missing key, checks
    that the recovered key opens every stored value, writes only then, and never
    replaces a file. With the setting it writes the key to the configured path.
    When it cannot write there, for example on a read-only mount, it writes
    nothing and says so, and names `--out`.
13. `restore-key --out FILE` writes the recovered key to the new file FILE
    instead of the location, with mode 0600, after the same checks, and never
    replaces a file. It makes no change at the location. The output says where
    the host must find the key. FILE may not lie inside the data directory.
    The operator moves FILE into the secret store and removes it.
14. `reset` keeps its rules (rules 25 to 31 there), and finds the key at the location. It refuses while
    the key at the location opens the stored values. With the setting, it says
    after it deleted the values that the next start needs a key at the path,
    because Flats will not create one.
15. A key at the location that is not a usable key (a directory, a wrong size)
    gets the message of the existing commands, with the path of the location.

## Backup

16. `flats backup` takes the location from the configuration file that it
    resolves, as the commands above do. With the setting, the key is not in the
    data directory, so the copy never holds it. The manifest records
    `secret_key: "external"`, and the output says that the key is kept at the
    path and must be backed up apart from the copy. The manifest keeps `format:
    1`: `external` is a new value of an existing field, and a `--verify` of an
    older release reports any value other than `included` as a copy without a
    key.
17. With the setting, the command reads the key at the location at step 13 and
    again at step 18 of [host backup](host-backup.md), and fails the backup when
    the two reads differ or the file is gone (rule 22 there). It runs the key
    check of rule 28 there at step 20 against the sealed values of the copy.
    A key that cannot be read before step 13 refuses the backup with a message
    that names `--exclude-secret-key`.
18. With the setting, `--exclude-secret-key` means that the command does not
    read the key: it skips the reads of rule 17 and the key check. The manifest
    still records `external`. Without the setting the flag keeps its meaning
    (rule 24 of host backup).
19. `flats backup --verify` needs no configuration. For a copy with `secret_key:
    "external"` it reports, as a note and not as a failure, that the key is
    outside the copy and that the sealed values were not checked against a key.
20. A restore (rule 35 of host backup) of a copy with `secret_key: "external"`
    brings back `PATH/config.json`, which holds the setting, and the operator
    puts the key at the configured path before the start. The docs say so.

## Docs

21. `docs/configuration.md` has the row of the setting, the key lifecycle (what
    the key protects, what to back up and what to store apart, how to move an
    existing key out of the data directory and back, and what a refusal means),
    and the effect on a downgrade (rule 22). `docs/container.md` shows a Docker
    secret and a Compose secret with `config set host.secret_key_path`.
    `docs/design.md` ("Secrets") says where the key may be. The spec index lists
    this page. The out-of-scope notes of [host backup](host-backup.md) and the
    rules named above change as rules 16 to 20 say, and
    [secret key recovery](secret-key-recovery.md) says that the commands use the
    location.

## Compatibility

22. No database change, and `schema_version` stays 5. A host without the setting
    behaves as before. An older Flats that reads a `config.json` with the new key
    refuses it as an unknown key, as for any key it does not know. To go back, the
    operator runs `flats config unset host.secret_key_path` with the new Flats
    and puts the key at `<data>/secret.key` first. The docs say so.
23. The key file format is unchanged: 32 raw bytes.

## Completion criteria

- Configuration tests: a relative path, a NUL byte and a path inside the data
  directory are refused, with the value and the reason; the default path
  `<data>/secret.key` is refused; an empty value is accepted; `config set`,
  `get` and `unset` round trip; `config set host.data_dir` refuses a directory
  that contains the key path; every example file of an earlier schema still
  migrates; `schema_version` stays 5.
- Host tests: with the setting, a missing file, a link to nothing, a directory,
  a file of 31 and of 33 bytes and an unreadable file each stop the start, name
  the path and the setting, and leave the data directory and the folder of the
  key as they were, also when `flats.db` has no sealed value. A valid key starts
  the host, in a read-only folder and with mode 0444, with no file created. The
  host opens a sealed flat secret and a stored operator key with that key. With
  the setting empty, the first start creates the key as before.
- A test shows that `flats key set` with the setting and no file refuses and
  creates nothing.
- Command tests with `--config` and with `--data` and a configuration file:
  `backup-key` reads the configured key; `restore-key` writes to the configured
  path, never replaces a file, and with `--out` writes a new 0600 file and
  changes nothing at the location, and refuses an `--out` inside the data
  directory; with a read-only location it writes nothing and names `--out`;
  `reset` refuses while the configured key opens the values, and after a reset
  the next start refuses until a key is put at the path.
- Backup tests: the manifest has `external` and the copy holds no key; the output
  names the path; a key that changes between step 13 and step 18 fails the copy;
  `--exclude-secret-key` skips the reads and the check; an unreadable key refuses
  the backup and names the flag; `--verify` reports the note; a copy restored
  with the key put back at the path starts a host that opens every sealed value.
- Docs: `docs/configuration.md`, `docs/container.md` and `docs/design.md` say
  what rules 21 and 22 list, and the documentation checks pass.

## Open questions

None.
