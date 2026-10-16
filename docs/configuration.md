# Configuration and storage

This page says which data Flats stores where, how `config.json` is versioned,
and how an existing installation moves to it.

## Where data lives

The question "who decides this value?" picks the place:

| Decided by | Stored in | Examples |
| --- | --- | --- |
| The operator | `config.json` | addresses, system policies, permitted networks, credential file paths |
| Flats while running | `flats.db` | flats, drafts, versions, deployments, approvals, per-flat provider permission, events, analytics, ordinary app environment variables, encrypted secrets, encrypted operator keys, console node records |
| The flat | `flats/<slug>/` | uploaded code, the flat's SQLite database and FILES |
| Nobody may read it in plain text | separate files | `secret.key` |
| A client machine's user | `flats-client/connection.json` | which Flats host the CLI and `flats mcp` use, saved by `flats connect` |

No value is stored in two places. `flats.db` also holds `host_binding`, which
records which `config.json` belongs to this data directory; it is a link, not a
copy of any setting.

By default the configuration and the data share one directory: the OS user
configuration directory plus `Flats` (`~/Library/Application Support/Flats` on
macOS, `$XDG_CONFIG_HOME/Flats` or `~/.config/Flats` on Linux).

The client connection is not part of the data directory. It lives in the same
OS user configuration directory under `flats-client/`, only the CLI and
`flats mcp` read it, and `flats serve` ignores it.

```text
<data-dir>/
  config.json                 operator configuration (the only source)
  config-history/             previous config.json copies, newest 10
  flats.db                    metadata database (+ -wal, -shm)
  secret.key                  key for flat secrets, 32 bytes, mode 0600
  backups/                    flats.db copies taken before a schema migration
                              (newest 3) and the archived network-provider.json
  runtime/docs/               generated read-only docs app modules (recreated as needed)
  flats/<slug>/
    draft-revs/ versions/     draft and published code
    data/db.sqlite data/files/  the flat's own data
    snapshots/                DB+FILES snapshots taken before deploys
    previews/                 preview data, paired with preview rows in flats.db
    restore-journal.json      present only while a data restore is unfinished
    health-check/ restore-trial/  temporary copies
  flats/.lifecycle/           unfinished rename/delete journals, deleted storage
                              being removed, and orphans/ (see below)
  tsnet/ portal/              provider node identities
  netbird/peers/<name>/       one NetBird peer per flat and console domain: identity (mode 0600) and state (builds with NetBird only)
  zrok/                       zrok names this host reserved, until released
  zrok-environment/           the connected zrok account: token and identity (mode 0600)
  cloudflare-quick/           Cloudflare quick tunnel credentials per flat (mode 0600)
  network-retirements/        unfinished tailnet node cleanup
  logs/ cache/ run/ flats.lock  logs, caches, sockets, the host lock
```

Operator keys (the Tailscale auth key, and a NetBird setup key or zrok token
waiting to be used) are kept in `flats.db`, encrypted with `secret.key`; see
[Operator keys](#operator-keys). `config.json` holds no key and no path to one.

A rename or deletion writes a journal under `flats/.lifecycle/` before it moves
or removes a flat directory, and startup finishes or undoes an interrupted one
before any flat runs. If a rename or deletion can neither finish nor undo its
directory move, its journal stays, and until a restart recovers it every
operation that writes that flat's storage (saves, document edits, previews, and
approved publish, deploy, rollback and data restore) is refused with a conflict;
reading the flat still works. A new or renamed flat never takes over a directory that
already exists at its path. At startup, a `flats/<slug>/` directory that no flat
owns is moved, not deleted, to `flats/.lifecycle/orphans/<slug>` (with a numeric
suffix if that name is taken) and the start is logged; inspect it and remove it
when it is no longer needed. A deleted flat's directory that could not be
removed yet stays at its path, and its journal keeps the slug reserved until a
later startup removes it. Startup stops with an error only when it cannot
tell which directory holds a flat's data, and the message names the paths to
resolve by hand.

### What to back up

Required: `config.json`, `flats.db` (with `VACUUM INTO` or while the host is
stopped, never a plain copy of a live database), `secret.key` together with
`flats.db`, `flats/<slug>/` except the temporary copies, `restore-journal.json`,
`flats/.lifecycle/` and `network-retirements/` when present, and `tsnet/` and `portal/` (without
them new node names and certificates are issued), `zrok/` (without it a
deleted flat no longer releases its zrok name), and `netbird/` (without it every NetBird peer registers again with the stored setup key and NetBird lists a new peer per name). `cloudflare-quick/` is
optional: without it each public flat's quick tunnel gets a new random
address. The operator keys are in `flats.db` and need `secret.key` from the
same backup. Logs, caches, sockets and the lock are rebuilt.

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
  "schema_version": 4,
  "host": {
    "instance_id": "63b57046-582f-48d9-a64f-877ae28d12aa",
    "data_dir": "/path/to/flats-data"
  },
  "network": {
    "permitted": ["tailscale", "portal"]
  },
  "tailscale": {
    "console_hosts": ["flats"]
  },
  "system": {
    "version_quota": 20
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
| `host.server_runtime` | `true` | restart |
| `network.permitted` | `[]` | immediately from the console; otherwise restart |
| `network.private_backend` | `local` | restart |
| `network.default_private` | `""` (follow `private_backend`) | immediately, for flats created afterwards |
| `network.default_public` | `""` (no public provider) | immediately, for flats created afterwards |
| `tailscale.console_hosts` | `[]` (console on loopback only) | immediately from the console; otherwise restart |
| `system.upload_max_bytes` | `20971520` (20 MiB) | immediately |
| `system.version_quota` | `20` per flat, `0` = unlimited | immediately; existing versions are kept |
| `system.disk_quota_bytes` | `32212254720` (30 GiB) per flat, `0` = off | immediately |
| `system.preview_ttl_seconds` | `86400` | next sweep |
| `system.rate_limit_rps` | `50` per flat | immediately |
| `system.redirect_days` | `7` | new renames |
| `system.events_keep` | `5000` per flat | next pruning |
| `portal.relays` | `[]` (Portal defaults) | restart |
| `portal.discovery` | `true`; `false` needs relays | restart |
| `portal.max_active_relays` | `3` | restart |
| `portal.hide` | `false` (public flats appear in relay listings) | immediately; relays follow at their next lease renewal |
| `zrok.environment` | none (Flats manages `zrok-environment/` in the data directory) | restart, or when zrok is turned on from the console |
| `zrok.namespace` | `public` | restart, or when zrok is turned on from the console |
| `netbird.management_url` | `https://api.netbird.io:443` (NetBird Cloud); an https URL | restart, or when NetBird is turned on from the console; applies to peers started afterwards |
| `netbird.console_hosts` | `[]`; lowercase DNS labels, each its own NetBird peer serving the console over HTTP; needs `netbird` in `network.permitted` | immediately when changed from the console (Settings → Console domains); otherwise restart |
| `credentials.operator_file` | none; ignored (the operator credential is no longer used) | — |
| `credentials.tailscale_authkey_file` | deprecated: the next start stores the key in `flats.db` and removes this key; `flats config set` refuses it | next start |

Addresses take any `host:port` that the old `--listen` and `--local-addr`
flags took, and the host logs a warning when the management address is not
loopback. The integer system settings take values up to 2^53−1, so a
JavaScript client reads them exactly.

Only providers listed in `network.permitted` start. Permitting `tailscale`
does not permit Funnel, and Local needs no entry. `private_backend:
"tailscale"` requires `tailscale` in `permitted`, and also makes every new
flat allowed on Tailscale when it is created (the operator can turn that off
per flat); flats that already exist keep their providers.

`default_private` (`local`, `tailscale` or `netbird`) and `default_public`
(`tailscale-funnel`, `portal`, `zrok`, `cloudflare-quick` or `netbird-expose`) choose the one provider per mode that
a new flat is allowed on when it is created, besides Local. Each must be in
`permitted`; other permitted providers are not granted to new flats. An empty
`default_private` follows `private_backend`, and an empty `default_public`
grants no public provider. A new flat is always Private: a public default is a
permission only and opens nothing until the operator approves making the flat
public. Turning a provider off in the console clears a default that names it. Removing a provider from
`permitted` in the file takes its routes down at the next start but keeps each
flat's provider permission and visibility in `flats.db`; it never publishes a
flat or changes visibility. The console's Settings → Network providers turns a
provider on or off while the host runs: turning one on saves the file and
starts its backend at once (a backend contacts nothing until a flat allows the
provider), and turning one off is refused while a flat still allows it.

`portal.hide: true` keeps public flats out of the Portal relays' lists of
sites. It is not access control: anyone with a flat's URL can still open it.
Each flat can override it in the console (Settings → Networks → Portal relay
listing: follow the host setting, hidden or listed); the override is kept in
`flats.db`, survives renames and Private↔Public changes, and is removed with
the flat. Only the operator changes either, from the console; agents read the
result as `portal_listing` and `portal_hidden` on the flat. `portal_hidden` is
what Flats asks the relays for: a served route's lease metadata changes at
once, and relays apply it at their next lease renewal (up to about 90
seconds). If that update fails, the choice stays saved, the flat logs the
error and the next activation of the route retries it.

zrok serves public flats as zrok public shares on the operator's zrok
account. Connect the account in the console (Settings → Network providers →
zrok) with its account token: Flats registers this machine as an environment
of the account, as `zrok2 enable` would, and keeps the token and identity in
`zrok-environment/` in the data directory (mode 0600). The token is never
shown again, logged or written to `config.json`; the console shows only the
environment ID and zrok service. A new token replaces the account (staged
next to the directory, so a refused token keeps the current one; refused
while a flat is public through zrok), and name records whose reservation the
new account holds with the same creation time move to it. Disconnect
unregisters the environment and deletes it. To use an environment managed with the zrok CLI
instead, set `zrok.environment` to its directory; the console then cannot
connect or disconnect it. With `zrok` in `network.permitted` and no connected
account, the host still starts and zrok stays unconfigured until one is
connected. Each flat
served through zrok reserves its slug as a name in `zrok.namespace`, so its
public URL stays the same across restarts and Private↔Public changes; the
share itself exists only while the flat is public and the host runs.
Deleting a flat, or the expiry of a rename redirect, releases the name, also
when the flat went private or lost its zrok permission earlier: `zrok/` in
the data directory records every name the host reserved, and deleting such a
flat while zrok is turned off is refused until zrok is on again. Flats only
uses, removes and releases names and shares it created itself: a name already
reserved in your account by the zrok CLI or another Flats host is refused
(release it with `zrok2 delete name` or rename the flat), and a share you run
under a name Flats reserved is left alone. Changing `zrok.environment` to
another zrok account keeps the old account's records: flats using them report
the conflict until you switch back, or remove the record in `zrok/`. zrok
identifies an account only by its token, so after regenerating the token of
the same account, release such names with `zrok2 delete name` and remove
their records. A name another zrok
account already owns cannot be used; rename the flat. When the account's
name limit is reached, the route reports the zrok controller's reason. The
zrok client also honors its own `ZROK2_*` environment variables, such as
`ZROK2_API_ENDPOINT` for a self-hosted zrok instance.

`cloudflare-quick` has no settings and no credentials to enter: with it in
`network.permitted`, each flat that allows it gets an anonymous Cloudflare
quick tunnel while it is public. The tunnel credentials Flats keeps in
`cloudflare-quick/` are deleted with the flat. `LIBTUNNEL_*` environment
variables are ignored. See [Private and public access](networking.md#publishing-through-a-cloudflare-quick-tunnel).

Parsing is strict: UTF-8, at most 1 MiB, no duplicate or unknown keys, no
`null`, integers written as plain integers. One invalid value rejects the whole
file, and the error names its JSON path.

### Console domains

`tailscale.console_hosts` lists the console's names on the tailnet, in the
order they were added. Each name is its own tailnet node with its own
MagicDNS name and HTTPS certificate, `https://NAME.<tailnet>.ts.net`, open to
the devices the tailnet ACL allows. A non-empty list runs the console on
Tailscale whatever `network.private_backend` says, and needs `tailscale` in
`network.permitted`; Tailscale cannot be turned off in the console while the
list is not empty.

The console's Settings → Console domains adds and deletes names while the
host runs; there is no single name to edit. To change the name, add the new
one, point agents at it (`flats connect`), then delete the old one.

- A name is a lowercase DNS label. It is refused when a flat, an open
  preview, a rename redirect, another console domain or a node still being
  logged out uses it, and no flat or preview can take a console domain's
  name afterwards. `flats` stays reserved for the console even when it is not
  listed.
- Deleting a name saves the list first, then logs its node out and deletes
  its identity. Tailscale keeps a logged-out machine in the tailnet's machine
  list (as expired), so its name stays taken: to use the same name again,
  remove that machine in the Tailscale admin console first; otherwise the
  tailnet names the new node `NAME-1`, and the console says so. A console reached over the tailnet refuses to delete the last
  name; only the console on the loopback address, or `flats config set` while
  the host is stopped, can empty the list. Deleting the name the page is
  open on moves the page to a remaining name.
- `flats.db` records every console node (`console_nodes`), so a name removed
  with `flats config set` or a text editor is logged out at the next start. A
  logout that fails keeps the name reserved; the console lists it and retries
  on request, and the next start retries it.
- A listed name that a flat, preview or redirect already uses is not served;
  the console shows the conflict, and deleting it leaves that owner's node
  alone. A name listed again while its old node is still being logged out is
  served once that logout succeeds.
- Deleting the name the page is open on needs another console domain that is
  ready. Emptying the list needs the console on a loopback address.
- Approval links and `console_url` / `mcp_url` in `flats status` use the
  console domain the request came through, else the first listed name, else
  the loopback address. `console_urls` lists every served name.

`flats config set tailscale.console_hosts flats` sets the list while the host
is stopped, as on a new remote server.

### Operator keys

Flats keeps the keys an operator enters in `flats.db`, encrypted with
`secret.key` like flat secrets, and never writes them to `config.json`, logs,
events or API responses:

| Key | Use | Kept |
| --- | --- | --- |
| Tailscale auth key | logs in every new tailnet node | until removed |
| NetBird setup key | registers every new NetBird peer (one per flat and console domain) | until removed |
| zrok account token | connects the zrok account | until connected; the zrok environment in `zrok-environment/` then holds it |

A key reaches Flats in one of these ways:

- **Console**: Settings → Keys sets, replaces and removes keys while the host
  runs. A new Tailscale key or NetBird setup key is used by nodes and peers
  started afterwards; those already registered keep their identity.
- **`flats key set tailscale|zrok|netbird`** while the host is stopped, with
  the key on standard input (a prompt without echo on a terminal) or
  `--from-file PATH`. `flats key ls` lists the keys without values and
  `flats key rm` removes a stored key or a waiting zrok token.
- **At start**, in this order: the flags `--tailscale-authkey-file`
  (`--authkey-file` for a legacy service), `--zrok-token-file` and
  `--netbird-setup-key-file`, each naming a file; then the environment
  variables `FLATS_TAILSCALE_AUTHKEY`, `FLATS_ZROK_TOKEN` and
  `FLATS_NETBIRD_SETUP_KEY`, or their `_FILE` forms naming a file (setting both
  forms of one key is refused); then the deprecated
  `credentials.tailscale_authkey_file`. A legacy service also reads
  `TS_AUTHKEY` and `TS_AUTH_KEY`. Flats removes these variables from its
  environment after reading them.

A key given at start only fills in what is missing: the Tailscale key and the
NetBird setup key are stored when `flats.db` holds none of that kind, and a
zrok token is used when no account is connected. A key registered in the console or
with `flats key set` therefore stays after restarts even while a service
still passes an older flag or variable; a start value that differs from the
stored Tailscale or NetBird key is ignored, with a warning in the log and the console.
To replace a key, register the new one. An unreadable key file stops the
start with status 78.

A Tailscale key must be an auth key (`tskey-auth-…`); OAuth client secrets and
API tokens are refused. Use a reusable key so every new node can log in, and
leave it untagged so console decisions record who made them.

No MCP tool and no agent API route reads or sets a key. The console's routes
are protected against cross-site requests, not authenticated: a program that
can reach the management listener could imitate the console, as it can for
approvals.

### Editing

- **Console**: the system and Portal keys, `network.permitted`,
  `network.default_private` and `network.default_public` through Network
  providers, and `tailscale.console_hosts` through Console domains. A save
  rereads the file under a lock and fails if it changed on disk since the
  host read it; restart to pick up an outside edit. Host and
  `network.private_backend` keys are read-only there. A service that still
  passes `--relays` pins the relays, one that passes `--portal`, `--permit` or
  `--network tailscale` pins those providers, and one that passes
  `--console-host` pins the console domains: the console refuses to change
  them until the service is reinstalled.
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

Schema 2 replaced `host.console_host` with `tailscale.console_hosts`. The
migration lists the old name only when `network.private_backend` was
`tailscale` and the name was a lowercase DNS label, the only case in which a
console ran on the tailnet, so no console opens that was not open before.

Schema 3 replaces the shared NetBird peer configuration with per-flat peers
and `netbird.console_hosts`. Schema 4 then replaces `system.keep_versions`
with `system.version_quota`; the version-retention migration below applies
after that NetBird migration, preserving its peer and console-domain settings.

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
value for one run; they are not saved. `--console-host NAME` makes the console
domains `[NAME]` for that run and pins them. The key file flags
`--tailscale-authkey-file`, `--zrok-token-file` and `--netbird-setup-key-file`
are accepted too; see [Operator keys](#operator-keys). Other configuration
flags are refused.
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
or any of `flats/`, `secret.key`, `tsnet/`, `portal/`, `zrok/`,
`zrok-environment/`, `cloudflare-quick/`, `backups/`,
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
the embedded tsnet would read them and bypass Flats. Give the auth key as
described under [Operator keys](#operator-keys).

If flats have stored secrets, or operator keys are stored, and `secret.key` is
missing, the host refuses to start instead of creating a new key. Restore the
key from the same backup as `flats.db`, or, when only operator keys are
stored, remove them with `flats key rm` and register them again.

## Moving an existing installation

The first start of a new release without `--config` migrates automatically,
because the installer upgrades a service by replacing the binary and
restarting it with its old arguments. `flats config migrate --data DIR
--dry-run -- <serve flags>` shows the same result without writing anything.

The migration converts exactly what the old release used:

- `--listen`, `--local-addr`, `--runtime` when given.
- `tailscale.console_hosts`: the `--console-host` name (default `flats`) when
  the private backend is Tailscale and the name is a lowercase DNS label,
  else none: the console ran on the tailnet only then.
- `network.permitted`: the grants in `network-provider.json` plus those the
  given `--permit`, `--network tailscale` or `--portal` flags would have
  recorded. An explicit `--portal=false` leaves `portal` out, because Portal
  was not running.
- `network.private_backend` by the old rule.
- The system and Portal settings from the database's `settings` table;
  `--relays` wins over the stored relays, as before. Stored values the old
  release ignored become the value that was in effect, and the migration log
  lists them.
- `--operator-credential-file` (kept but ignored) as a path. The key of
  `--authkey-file` or `--tailscale-authkey-file` is stored in `flats.db`
  (unless a key is stored already); its path is not written, and an
  unreadable file stops the migration.
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

Version history is retained until explicitly deleted. Per-flat overrides are
managed in the console; lowering a quota never deletes versions. Configuration
schema 4 (migration 3→4) migrates an explicitly configured `system.keep_versions` to
`system.version_quota` (including 0 as unlimited); hosts without an explicit
old value use 20. Previously pruned records stay unavailable and do not count.

Migration changes the meaning of an explicit old count: `keep_versions: N`
becomes a quota of **N total available versions**, including the live version,
not N historical versions plus the live one. For example, quota 1 on a deployed
flat leaves no room to create its replacement. Raise the quota or explicitly
delete eligible older versions. Over-quota flats keep all retained versions;
new creation is refused until there is room. Previously pruned rows do not
count.
