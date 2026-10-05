# Configuration and storage

This page says which data Flats stores where, how `config.json` is versioned,
and how an existing installation moves to it.

## Where data lives

The question "who decides this value?" picks the place:

| Decided by | Stored in | Examples |
| --- | --- | --- |
| The operator | `config.json` | addresses, system policies, permitted networks, credential file paths |
| Flats while running | `flats.db` | flats, drafts, versions, deployments, approvals, per-flat provider permission, events, analytics, encrypted secrets |
| The flat | `flats/<slug>/` | uploaded code, the flat's SQLite database and FILES |
| Nobody may read it in plain text | separate files | `secret.key`, the Tailscale auth key |

No value is stored in two places. `flats.db` also holds `host_binding`, which
records which `config.json` belongs to this data directory; it is a link, not a
copy of any setting.

By default the configuration and the data share one directory: the OS user
configuration directory plus `Flats` (`~/Library/Application Support/Flats` on
macOS, `$XDG_CONFIG_HOME/Flats` or `~/.config/Flats` on Linux).

```text
<data-dir>/
  config.json                 operator configuration (the only source)
  config-history/             previous config.json copies, newest 10
  flats.db                    metadata database (+ -wal, -shm)
  secret.key                  key for flat secrets, 32 bytes, mode 0600
  backups/                    flats.db copies taken before a schema migration
                              (newest 3) and the archived network-provider.json
  flats/<slug>/
    draft-revs/ versions/     draft and published code
    data/db.sqlite data/files/  the flat's own data
    snapshots/                DB+FILES snapshots taken before deploys
    previews/                 preview data, paired with preview rows in flats.db
    restore-journal.json      present only while a data restore is unfinished
    health-check/ restore-trial/  temporary copies
  tsnet/ portal/              provider node identities
  network-retirements/        unfinished tailnet node cleanup
  logs/ cache/ run/ flats.lock  logs, caches, sockets, the host lock
```

The Tailscale auth key stays outside the data directory and outside anything
agents can read; `config.json` holds only its path.

### What to back up

Required: `config.json`, `flats.db` (with `VACUUM INTO` or while the host is
stopped, never a plain copy of a live database), `secret.key` together with
`flats.db`, `flats/<slug>/` except the temporary copies, `restore-journal.json`
and `network-retirements/` when present, and `tsnet/` and `portal/` (without
them new node names and certificates are issued). Back up the credential files
separately. Logs, caches, sockets and the lock are rebuilt.

Restore with the same or a newer Flats release: an older binary refuses a newer
`config.json` or database.

## config.json

The file holds only what the operator set. A missing key uses the default
below. Only `schema_version`, `host.instance_id` and `host.data_dir` are
required; `flats config init`, or the first `flats serve --config` of a new
host, writes them. A value equal to its default stays in the file once
written; `flats config unset KEY` removes it.

```json
{
  "schema_version": 1,
  "host": {
    "instance_id": "63b57046-582f-48d9-a64f-877ae28d12aa",
    "data_dir": "/Users/example/Library/Application Support/Flats"
  },
  "network": {
    "permitted": ["portal"]
  },
  "system": {
    "keep_versions": 20
  },
  "portal": {
    "relays": ["https://relay.example.com"]
  }
}
```

| Key | Default | Applies |
| --- | --- | --- |
| `host.instance_id` | written by init | never edited; `flats config rebind` replaces it |
| `host.data_dir` | written by init | offline: `flats config set host.data_dir` |
| `host.management_addr` | `127.0.0.1:7878` | restart |
| `host.local_addr` | `127.0.0.1:7879` | restart |
| `host.console_host` | `flats` | restart |
| `host.server_runtime` | `true` | restart |
| `network.permitted` | `[]` | immediately from the console; otherwise restart |
| `network.private_backend` | `local` | restart |
| `system.upload_max_bytes` | `20971520` (20 MiB) | immediately |
| `system.keep_versions` | `10` per flat, `0` = never prune | next pruning |
| `system.disk_quota_bytes` | `32212254720` (30 GiB) per flat, `0` = off | immediately |
| `system.preview_ttl_seconds` | `86400` | next sweep |
| `system.rate_limit_rps` | `50` per flat | immediately |
| `system.redirect_days` | `7` | new renames |
| `system.events_keep` | `5000` per flat | next pruning |
| `portal.relays` | `[]` (Portal defaults) | restart |
| `portal.discovery` | `true`; `false` needs relays | restart |
| `portal.max_active_relays` | `3` | restart |
| `credentials.operator_file` | none; ignored (the operator credential is no longer used) | — |
| `credentials.tailscale_authkey_file` | none (interactive login) | restart |

Addresses take any `host:port` that the old `--listen` and `--local-addr`
flags took, and the host logs a warning when the management address is not
loopback. The integer system settings take values up to 2^53−1, so a
JavaScript client reads them exactly.

Only providers listed in `network.permitted` start. Permitting `tailscale`
does not permit Funnel, and Local needs no entry. `private_backend:
"tailscale"` requires `tailscale` in `permitted`. Removing a provider from
`permitted` in the file takes its routes down at the next start but keeps each
flat's provider permission and visibility in `flats.db`; it never publishes a
flat or changes visibility. The console's Settings → Network providers turns a
provider on or off while the host runs: turning one on saves the file and
starts its backend at once (a backend contacts nothing until a flat allows the
provider), and turning one off is refused while a flat still allows it.

Parsing is strict: UTF-8, at most 1 MiB, no duplicate or unknown keys, no
`null`, integers written as plain integers. One invalid value rejects the whole
file, and the error names its JSON path.

### Editing

- **Console**: the system and Portal keys, and `network.permitted` through
  Network providers. A save rereads the file under a lock and fails if it
  changed on disk since the host read it; restart to pick up an outside edit.
  Host, `network.private_backend` and credential keys are read-only there. A
  service that still passes `--relays` pins the relays, and one that passes
  `--portal`, `--permit` or `--network tailscale` pins those providers: the
  console refuses to change them until the service is reinstalled.
- **`flats config set KEY VALUE` / `unset KEY`**: offline; they take the host
  lock and refuse while a host runs.
- **A text editor**: allowed. A running host does not reload the file; the
  change applies at the next start, which fails if the file is invalid.

Every Flats write keeps the previous file in `config-history/`, writes a
synced temporary file, renames it into place and syncs the directory.

## Versions

Three versions are independent:

| Version | Where | Bumped when | Migration |
| --- | --- | --- | --- |
| `schema_version` | `config.json` | the format changes incompatibly | automatic at start |
| `user_version` | `flats.db` | any table, column or data-meaning change | automatic at start, after a backup |
| a flat's database | `flats/<slug>/data/` | the flat decides | Flats never touches it |

### config.json schema rules

| Change | `schema_version` |
| --- | --- |
| Add a key with a default | unchanged |
| Widen an allowed range | unchanged |
| Rename, move or remove a key | bumped, with a migration |
| Change a type or unit | bumped, with a migration |
| Change an existing key's default | bumped; the migration writes the old default into files that relied on it |
| Narrow an allowed range or change meaning | bumped, with a conversion or an operator confirmation |

Defaults never change silently: an installation that never set a key keeps
the behavior it had. Migrations run on the raw JSON document in memory, in
order; the original is kept in `config-history/` before the result is saved.
A migration that could widen network or credential authority is not applied
automatically; `flats config migrate` shows it and asks for `--yes`.

Every released schema version keeps example files under
`internal/config/testdata/config/v<N>/`; tests migrate each to the current
version.

A newer `schema_version` than the binary supports is refused without touching
the file. So is a key this binary does not know: it may come from a newer
release.

### flats.db

Opening the database first inspects it read-only. A newer `user_version`, or a
SQLite file that is not a Flats database, is refused before anything is
written. Before migrating an existing database Flats writes
`backups/flats.v<N>.<time>.db` with `VACUUM INTO`; if that fails, it does not
migrate. Each migration step commits its changes and the new `user_version` in
one transaction. There are no down migrations: to run an older release,
restore the backup.

## Starting

`flats serve --config PATH` (or `FLATS_CONFIG`) runs from that file. Only
`--listen`, `--local-addr`, `--console-host` and `--runtime` may override a
value for one run; they are not saved. Other configuration flags are refused.
`--operator-credential-stdin` and `--operator-credential-file` are accepted and
ignored, with a warning: the operator credential is no longer used.

Without `--config` and `FLATS_CONFIG`, `flats serve` keeps accepting the older
flags and uses `<data-dir>/config.json`. This is what an installed service
from an earlier release runs after an upgrade, and what `flats serve` with no
arguments does.

Start-up writes nothing until it holds the host lock, has reread the file and
has checked read-only that the database belongs to this configuration:

| config.json | flats.db | `--config` | without `--config` |
| --- | --- | --- | --- |
| present | bound to it | start | compare the given flags, then start |
| present | bound elsewhere | refuse (`flats config rebind` for a copy) | refuse |
| present | not bound | refuse (`flats config migrate`) | finish an interrupted migration |
| present | absent | `host.data_dir` beside the file and without Flats data: finish the init; otherwise refuse (maybe the wrong directory) | finish an interrupted init |
| absent | absent | no Flats data in the file's directory: bootstrap; otherwise refuse | new directory: init; existing data: migrate |
| absent | present | refuse (`flats config migrate`, or the config it is bound to) | new directory: init; existing data: migrate |

Every refusal that needs the operator exits with status 78. The systemd unit
sets `RestartPreventExitStatus=78` so it does not restart in a loop.

### Bootstrapping a new host

When the file named by `--config` does not exist, `flats serve` starts a new
host instead of refusing, so a first start needs no separate `flats config
init`. The data directory is the directory of that file. Flats writes
`config.json` with a new `host.instance_id` and the defaults, creates and binds
an empty `flats.db`, and logs `initialized a new host in DIR`. Per-run
`--listen`, `--local-addr`, `--console-host` and `--runtime` values are not
written to the file.

It refuses, writing nothing but the lock, when the directory already holds
Flats data: a `flats.db` (convert a legacy one with `flats config migrate`),
or any of `flats/`, `secret.key`, `tsnet/`, `portal/`, `backups/`,
`network-retirements/` or `network-provider.json` without a database (restore
the database and config from the same backup). Other entries, such as a new
volume's `lost+found`, are fine. A bootstrap interrupted after writing
`config.json` finishes on the next start with the same instance id.

A mistyped path or an unmounted volume therefore starts an empty host rather
than failing. Check the log line on a first start, and keep the data directory
where `--config` expects it.

Existing files are upgraded and checked as described under
[Versions](#versions): an older `schema_version` or database `user_version` is
migrated at start (the database after a backup), and a newer one stops the
start with status 78 without changing anything.

When `network.permitted` includes `tailscale` or `tailscale-funnel`, a
`--config` host refuses to start while `TS_AUTHKEY`, `TS_AUTH_KEY`,
`TS_CLIENT_SECRET`, `TS_CLIENT_ID`, `TS_ID_TOKEN` or `TS_CONTROL_URL` is set:
the embedded tsnet would read them and bypass `config.json`. Put the auth key
in a file and set `credentials.tailscale_authkey_file`.

If flats have stored secrets and `secret.key` is missing, the host refuses to
start instead of creating a new key. Restore the key from the same backup as
`flats.db`.

## Moving an existing installation

The first start of a new release without `--config` migrates automatically,
because the installer upgrades a service by replacing the binary and
restarting it with its old arguments. `flats config migrate --data DIR
--dry-run -- <serve flags>` shows the same result without writing anything.

The migration converts exactly what the old release used:

- `--listen`, `--local-addr`, `--console-host`, `--runtime` when given.
- `network.permitted`: the grants in `network-provider.json` plus those the
  given `--permit`, `--network tailscale` or `--portal` flags would have
  recorded. An explicit `--portal=false` leaves `portal` out, because Portal
  was not running.
- `network.private_backend` by the old rule.
- The system and Portal settings from the database's `settings` table;
  `--relays` wins over the stored relays, as before. Stored values the old
  release ignored become the value that was in effect, and the migration log
  lists them.
- `--operator-credential-file` (kept but ignored) and `--authkey-file` paths.
  File contents are not read.
- Pre-lifecycle flats waiting for the one-time Private Tailscale choice are
  decided by the old rules. If the choice is ambiguous (old Tailscale state but
  no `--network`), nothing is written and the start stops with the same
  instructions as before.

Steps: back up and migrate `flats.db` and apply that choice; write
`config.json` with a new `instance_id`; record `host_binding`. The binding row
is the only sign that the migration finished. Afterwards
`network-provider.json` moves into `backups/` (retried at later starts if that
fails), and neither it nor the `settings` table is read again.

| Interrupted after | Next start |
| --- | --- |
| nothing written | starts over |
| the database step | starts over; repeating it changes nothing |
| writing config.json | reuses its `instance_id` if it matches the recomputed plan, else stops and shows the difference |
| the binding | done |

Until the service is reinstalled, it still passes its old flags. Each start
compares only the flags actually given with `config.json`: equal flags start
with a warning, different ones stop with status 78 instead of overriding the
file. Run `flats install` to switch the service to `serve --config`; the
installer prints that command after an upgrade. `flats install` stops a
running Flats service while it prepares `config.json` and starts the old
service definition again if that fails.

To go back to an older release, restore `backups/flats.v<N>.<time>.db`; the
older binary ignores `config.json`.
