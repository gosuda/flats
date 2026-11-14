# Configuration and storage

This page says which data Flats stores where, how `config.json` is versioned,
and how an existing installation moves to it.

## Where data lives

The question "who decides this value?" picks the place:

| Decided by | Stored in | Examples |
| --- | --- | --- |
| The operator | `config.json` | addresses, system policies, permitted networks, console domains, provider settings |
| Flats while running | `flats.db` | flats, drafts, versions, deployments, revision metadata of documents, designs, boards and notebooks, approvals, per-flat provider permission, events, analytics (page views, and per-day usage in `usagedays`), ordinary app environment variables, encrypted secrets, encrypted operator keys, console node records, [Autopilot settings](#autopilot-settings), [public instance settings](#public-instance-settings) |
| The flat | `flats/<slug>/` | uploaded code, revision contents, the flat's SQLite database and FILES, a board's or notebook's current content |
| Nobody may read it in plain text | separate files | `secret.key`, or the file that [`host.secret_key_path`](#keeping-the-key-outside-the-data-directory) names |
| A client machine's user | `flats-client/connection.json` | which Flats host the CLI and `flats mcp` use, saved by `flats connect` |

No value is stored in two places. `flats.db` also holds `host_binding`, which
records which `config.json` belongs to this data directory; it is a link, not a
copy of any setting.

By default the configuration and the data share one directory: the OS user
configuration directory plus `Flats` (`~/Library/Application Support/Flats` on
macOS, `$XDG_CONFIG_HOME/Flats` or `~/.config/Flats` on Linux). On Windows it is
`%LOCALAPPDATA%\Flats`, not the roaming profile, which would copy databases and
caches at each logon on a domain machine. `flats serve` gives the data directory
an access list for the user and the system alone, wherever it is: file modes do
not protect the keys and sockets there.

The client connection is not part of the data directory. It lives in the OS
user configuration directory under `flats-client/` (`%APPDATA%` on Windows), only
the CLI and `flats mcp` read it, and `flats serve` ignores it. On Windows the
definition of the service, `service.xml`, lies in `%LOCALAPPDATA%\Flats` whatever
the data directory is.

```text
<data-dir>/
  config.json                 operator configuration (the only source)
  config-history/             previous config.json copies, newest 10
  flats.db                    metadata database (+ -wal, -shm)
  secret.key                  key for flat secrets and operator keys, 32 bytes, mode 0600
                              (not here when host.secret_key_path names another file)
  secret.key.recovery         that key encrypted under a recovery password (optional)
  secret.key.recovery.id      the ID of the key in that copy
  backups/                    flats.db copies taken before a schema migration
                              (newest 3) and the archived network-provider.json
  runtime/docs/               generated read-only docs app modules (recreated as needed)
  flats/<slug>/
    draft-revs/ versions/     saved Draft revisions and versions
    data/db.sqlite data/files/  the flat's own data (db.sqlite + -wal, -shm)
    snapshots/                DB+FILES snapshots taken before a version goes
                              live and before a data restore (not for documents)
    previews/                 preview data, paired with preview rows in flats.db
                              (each preview's db.sqlite + -wal, -shm)
    revisions/                revision contents of a document, board or notebook
                              (blobs/, by SHA-256)
    design/                   a design's revision contents (blobs/, by SHA-256)
    board/board.db            a board's current content and the revisions not yet
                              imported (+ -wal, -shm)
    notebook/notebook.db      a notebook's current content, receipts, undo and
                              unimported revisions (+ -wal, -shm)
    restore-journal.json      present only while a data restore is unfinished
    health-check/ restore-trial/  temporary copies
  flats/.lifecycle/           unfinished rename/delete journals, deleted storage
                              being removed, and orphans/ (see below)
  tsnet/                      tailnet node state: one node per flat, preview
                              and console domain
  portal/                     Portal identities (<slug>.json), the pinned relay
                              (<slug>.relay) and remembered aliases (<slug>.alias)
  netbird/peers/<name>/       one NetBird peer per flat and console domain:
                              identity (mode 0600) and state (builds with NetBird only)
  zrok/                       zrok names this host reserved, until released
  zrok-environment/           the connected zrok account: token and identity (mode 0600)
  cloudflare-quick/<slug>.json  Cloudflare quick tunnel credentials (mode 0600)
  network-retirements/        unfinished tailnet node cleanup
  cache/ run/                 compiled code cache, worker sockets
  logs/ flats.lock            service logs, the host lock
```

Revision contents are content-addressed: a revision adds only the files that
changed, and blobs no revision refers to any more are removed after
retention deletes revisions.

Operator keys (the Tailscale auth key, the NetBird setup key, and a zrok token
waiting to be used) are kept in `flats.db`, encrypted with `secret.key`; see
[Operator keys](#operator-keys). `config.json` holds no key. It holds a path to
the key file only when you set `host.secret_key_path`.

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

Required:

- `config.json`;
- `flats.db`, together with `secret.key` from the same moment: the flat
  secrets and operator keys in `flats.db` need it. With
  [`host.secret_key_path`](#keeping-the-key-outside-the-data-directory) the key
  is a file outside the data directory, and you keep it apart from the copy;
- `flats/<slug>/` except the temporary copies, with `restore-journal.json`,
  revision contents and snapshots, and `flats/.lifecycle/` and
  `network-retirements/` when present;
- `tsnet/` and `portal/`: without them new node names and certificates are
  issued;
- `zrok/` and `zrok-environment/`: without them a deleted flat no longer
  releases its zrok name, and the zrok account must be connected again;
- `netbird/`: without it every NetBird peer registers again with the stored
  setup key, and NetBird lists a new peer per name.

`cloudflare-quick/` is optional: without it each public flat's quick tunnel
gets a new random address. Logs, caches, sockets and the lock are rebuilt.

To back up, run this on the machine of the data directory, with the host running
or stopped:

```bash
flats backup --out PATH [--exclude-secret-key] [--config PATH | --data DIR]
```

`PATH` must not exist, and its folder must. The command writes `PATH/manifest.json`
(the Flats version, and the size and SHA-256 of every file), `PATH/data/` (the data
directory) and, when `config.json` lives outside the data directory,
`PATH/config.json` and the `config-history/` folder beside it. It writes into a
temporary folder and renames it to `PATH` at the end, so a failed or interrupted
backup leaves nothing at `PATH`. Details are in the [Host backup](spec/host-backup.md)
spec.

The command does not pause the host. It copies each live database as a snapshot
(`VACUUM INTO`, which holds every commit up to its moment), copies the revision
contents, which the host writes once, in two passes, and checks its own copy. A change during the copy
that could make the copy inconsistent, such as a flat that was created, renamed or
deleted, or a board that kept changing while its database and `flats.db` were
copied, makes the command fail with "run the backup again". Under constant writes
it can fail again and again. Stop the host then, and run the command again: the
copy of a stopped host is exact.

A copy of a running host does not come from one moment. Two flats' databases come
from different moments, and so do a flat's `data/db.sqlite` and its `data/files/`.
A file that a flat writes all the time can be partial. The command lists such
files as unstable and keeps the last read. Each database, each outbox database
against `flats.db`, and every blob that a database needs are consistent in the
copy.

The copy holds every secret that the host holds, including `secret.key`, the
operator keys and the sealed values in `flats.db`. Keep it as protected as the data
directory, in a folder that only you can write. With `--exclude-secret-key`, the
copy has no `secret.key`, so you can keep the key apart from the data. The
recovery copy `secret.key.recovery` stays in it, and anyone who holds the copy can
guess its password offline
([Secret key recovery](spec/secret-key-recovery.md#the-recovery-copy)).

With [`host.secret_key_path`](#keeping-the-key-outside-the-data-directory) the key
is not in the data directory, so it is not in the copy. The manifest records
`secret_key: "external"`, and the command prints where the key is: back that file
up apart from the copy. The command reads the key, checks that it opens every
sealed value of the copy, and fails if the key changed during the copy. It
refuses before it writes anything when the key is missing or unreadable, and the
message names `--exclude-secret-key`; with the flag it does not read the key at
all. It also scans every file of the copy for the 32 bytes of the key, under any
name, and fails the backup when it finds them: run it again after you remove the
second copy of the key from the data directory.

The command works only on a data directory that a host of this Flats release wrote.
After an upgrade, start the host once so that it migrates `flats.db`. To back up a
stopped host of an older release before you start the new one, copy the data
directory by hand ([Copying by hand](#copying-by-hand)).

To check a copy, run `flats backup --verify PATH`. It needs no host and no
configuration, and it writes nothing. It compares every file with the manifest,
checks each database, checks that the outbox databases of documents, boards and
notebooks fit `flats.db`, checks that every blob that a flat needs is in the copy,
and checks that `secret.key` opens every sealed value. For a copy with an
external key it has no key to use: it says in a note that the key is outside the
copy and that it did not check the sealed values. It prints `ok` or each
failure, and exits with status 1 on a failure. It refuses, with status 78, a copy
whose `flats.db` has another schema version than this Flats expects, and says
which Flats version made it. Run `--verify` before every restore.

To restore, stop the host, run `flats backup --verify PATH`, move the old data
directory away and copy `PATH/data` to the same path. If `PATH/config.json` and
`PATH/config-history/` exist, copy them to the place of the configuration file and
the folder beside it. Then start the host. A copy made with `--exclude-secret-key`
needs the key first: put `secret.key` back from where you kept it, or run
`flats secret restore-key` with the recovery password
([Recovering a lost secret.key](#recovering-a-lost-secretkey)). A copy with an
external key brings back the `config.json` that holds `host.secret_key_path`:
put the key at that path before you start the host. Copying over the old directory can
leave a `-wal` file beside a restored database and corrupt it. A copy at another
path is not used, because `host.data_dir` in its `config.json` still names the
old path and a service started with `--config` still reads the old file.

Restore with the same or a newer Flats release: an older binary refuses a newer
`config.json` or database.

#### Copying by hand

You can still copy the data directory yourself with the host stopped, plus
`config.json` when it lives outside that directory. That covers all of it and
takes every file from one moment.

While the host runs, these are live SQLite databases, with recent commits in a
`-wal` file beside each one:

- `flats.db`;
- `flats/<slug>/data/db.sqlite`, a flat's own data;
- `flats/<slug>/previews/<host>/db.sqlite`, while that preview is open;
- `flats/<slug>/board/board.db`, a board;
- `flats/<slug>/notebook/notebook.db`, a notebook.

Never take a plain copy of a live database: it can lose recent commits or come
out corrupt. A host that crashed or was killed also leaves commits in the
`-wal` file, so copy each `-wal` with its database.

SQLite's backup API (`.backup` in the `sqlite3` shell) copies one live database,
not the host. The databases and revision contents depend on each other and must
come from the same moment, and a write between two copies can break that. For
boards and notebooks:

- With a board or notebook database that lacks revisions `flats.db` already
  recorded, that flat does not start, and its log says why. It starts again once
  both come from one moment.
- With `flats.db` older than a board or notebook database, the newest revisions
  are gone and later revisions reuse their numbers.
- With `revisions/` older than the databases, the host stops importing. Writes
  still report success but no new revision appears, and a restart does not
  clear it.

### Autopilot settings

The [Autopilot](spec/autopilot.md) settings live only in `flats.db`. They
have no `config.json` key, environment variable, `flats serve` flag, CLI
command or MCP tool; only the console's Experiments page changes them
([routes](surfaces.md#experiments-routes)). Database step 21 creates two
tables on new and upgraded hosts, with Autopilot off, no operation allowed
and an empty guide:

| Table | Holds |
| --- | --- |
| `autopilot` | One row (`id` 1): `enabled` (0 or 1), `ops` (the bit set of operations agents may decide, below 512; the bits are listed in [Autopilot](design.md#autopilot)), `guide` (the operation guide as the operator saved it; the host refuses more than 4,000 code points) and `updated_at` (Unix milliseconds of the last change, 0 when never changed) |
| `autopilot_changes` | The shared Experiments change log, including public instance changes from step 22: the host log line of each committed change (`seq`, `at` in Unix milliseconds, `line`, `logged`) until it is logged; the newest 100 logged lines stay |

Nothing in them is secret, so they are not encrypted. Because every database
from step 21 on has the `autopilot` row, that row does not count when Flats
checks whether a database already holds data. A release older than step 21
refuses the database; going back means restoring a full backup taken before
the upgrade, which loses every change made since, including agent decisions. To
stop Autopilot without going back, turn it off in the console, or stop Flats.

### Public instance settings

[Public instance mode](spec/public-instance.md) lives only in `flats.db`,
independently of Autopilot. It has no `config.json` key, environment variable,
`flats serve` flag, CLI command or MCP tool. Only Settings › Experiments
changes it, through the [console routes](surfaces.md#experiments-routes).
Changes apply without a restart.

Database step 22 creates `public_instance` on new and upgraded hosts, with
one row, `id` 1. Its `enabled` is 0 or 1 and starts at 0; `updated_at` is the
Unix-millisecond time of the last change, or 0 when never changed. The row is
not encrypted and does not count when Flats checks whether a database already
holds data. A change writes the row and a `public instance:` line to the
shared `autopilot_changes` table in one transaction. The existing Experiments
log sync writes and marks both kinds of lines, with the same retention.

Turn on requires `network.default_public` to name a public provider in
`network.permitted`, and confirms that provider's current ID. This checks
permission, not route readiness. The mode grants no provider the host has not
already permitted. While on, a new flat gets only that public grant besides
implicit Local, starts Public with view-only access and needs no visibility
approval. Every private link, Local included, is disabled. If the default is
later cleared or no longer permitted, the mode stays on, the console warns
and new creation is refused without a fallback. Existing flats keep their
visibility and grants. Turn off restores retained private links without
changing visibility.

Database step 23 adds `flats.created_public_instance`, a 0-or-1 mark with a
default of 0 for existing flats. Creation while the mode is on sets it to 1
in the same transaction as visibility and grants. Rename and restart keep
it, and turning the mode off does not clear it. While the mode is on and a
marked website or page is Public, each save stores and deploys the next
version without approval. Older flats, marked Private flats and saves while
the mode is off keep the separate approved version/deploy steps. The page
starter Draft from `create_flat` is not published.

A binary older than database step 23 refuses this database. To run it,
restore a full backup taken before the upgrade, losing every change made since.
To stop the mode without restoring a backup, turn it off in the console or stop
Flats.

## config.json

The file holds only what the operator set. A missing key uses the default
below. Only `schema_version`, `host.instance_id` and `host.data_dir` are
required; `flats config init`, or the first `flats serve --config` of a new
host, writes them. A value equal to its default stays in the file once
written; `flats config unset KEY` removes it.

```json
{
  "schema_version": 5,
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
| `host.console_addr` | `127.0.0.1:7878`; console, HTTP API and MCP listener | restart |
| `host.local_addr` | `127.0.0.1:7879` | restart |
| `host.server_runtime` | `true` | restart |
| `host.secret_key_path` | `""` (the key is `<data-dir>/secret.key`); an absolute path outside `host.data_dir` | offline: `flats config set host.secret_key_path`; a SIGHUP reload lists it as awaiting a restart, and the running host keeps its key |
| `metrics.enabled` | `true` | restart |
| `metrics.expose_on` | `["localhost"]`; unique entries from `localhost`, `console`, `external`; `[]` exposes nowhere | restart |
| `metrics.per_flat` | `true`; set `false` to omit per-flat Prometheus series | restart |
| `network.permitted` | `[]` | immediately from the console; a SIGHUP reload or restart from the file |
| `network.private_backend` | `local` | restart |
| `network.default_private` | `""` (follow `private_backend`); `local`, `tailscale`, `netbird` or `proxy` | immediately, for flats created afterwards |
| `network.default_public` | `""` (no public provider); `tailscale-funnel`, `portal`, `zrok`, `cloudflare-quick`, `netbird-expose` or `proxy-public` | immediately, for flats created afterwards |
| `tailscale.console_hosts` | `[]` (console on loopback only) | immediately from the console; a SIGHUP reload or restart from the file |
| `system.upload_max_bytes` | `20971520` (20 MiB) | immediately |
| `system.version_quota` | `20` per flat, `0` = unlimited | immediately; existing versions are kept |
| `system.revision_quota` | `100` unpinned revisions per document, design, board or notebook, `0` = keep all | immediately; lowering it deletes the oldest unpinned revisions at once |
| `system.pin_quota` | `20` pinned revisions per document, design, board or notebook, `0` = unlimited | immediately; existing pins are kept and new pins are refused while full |
| `system.disk_quota_bytes` | `32212254720` (30 GiB) per flat, `0` = off | immediately; running boards and notebooks measure their flat again |
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
| `netbird.console_hosts` | `[]`; lowercase DNS labels, each its own NetBird peer serving the console over HTTP; needs `netbird` in `network.permitted` | immediately when changed from the console (Settings → Console); a SIGHUP reload or restart from the file |
| `proxy.url_template` | `""` (none); the reverse proxy's address pattern, such as `https://{host}.site.example.org` | immediately from Settings → Networks → Reverse proxy; a SIGHUP reload or restart from the file |
| `proxy.addr` | `127.0.0.1:7880`; the listener the reverse proxy forwards to | offline `flats config set` only; when the listener next binds (at start, or when a reverse proxy kind is turned on) |
| `console.external_origins` | `[]`; explicit HTTP/HTTPS origins with lowercase DNS names and optional ports | immediately from Settings → Console; a SIGHUP reload or restart from the file |
| `console.preferred_host` | `""` (no preference); `local`, `tailscale:NAME` or `netbird:NAME` with NAME in that list | immediately when changed from the console (Settings → Console); a SIGHUP reload or restart from the file |
| `credentials.operator_file` | none; ignored (the operator credential is no longer used) | — |
| `credentials.tailscale_authkey_file` | deprecated: the next start stores the key in `flats.db` and removes this key; `flats config set` refuses it | next start |

`host.console_addr` names the shared management listener, not a console-only
listener. The rename leaves the actual bind address, routes and embedded
Tailscale/NetBird console behavior unchanged. `host.local_addr` still names the
separate flat listener. `--listen` still overrides `host.console_addr` for one
run; existing CLI and installer flag names are unchanged. The Settings view
reports `host.console_addr` and labels it "Console address".

Addresses take any `host:port` that the old `--listen` and `--local-addr`
flags took, and the host logs a warning when the management address is not
loopback. The integer system settings take values up to 2^53−1, so a
JavaScript client reads them exactly.

Only providers listed in `network.permitted` start. Permitting `tailscale`
does not permit Funnel, and Local needs no entry. `private_backend:
"tailscale"` requires `tailscale` in `permitted`. While `default_private` is
empty, it also makes every new flat allowed on Tailscale when it is created
(the operator can turn that off per flat); a non-empty `default_private`
decides instead. This private grant is skipped while public instance mode is
on. Flats that already exist keep their providers.

`default_private` (`local`, `tailscale`, `netbird` or `proxy`) and `default_public`
(`tailscale-funnel`, `portal`, `zrok`, `cloudflare-quick`, `netbird-expose` or
`proxy-public`) choose the one provider per mode that
a new flat is allowed on when it is created, besides Local. Each must be in
`permitted`; other permitted providers are not granted to new flats. An empty
`default_private` follows `private_backend`, and an empty `default_public`
grants no public provider. With public instance mode off, a new flat is
Private: a public default is a permission only and opens nothing until an
approved change to Public. While [public instance mode](#public-instance-settings)
is on, only the public default is granted besides Local, and a new flat starts
Public without a visibility approval. Saving a Public website or page
created by the mode publishes it without approval; older flats keep approved
version creation and deployment. Turning a provider off in the console clears a default that names it.
The console's Settings → Networks turns a
provider on or off while the host runs: turning one on saves the file and
starts its backend at once (a backend contacts nothing until a flat allows the
provider), and turning one off is refused while a flat still allows it.
Removing a provider from `permitted` in the file takes its routes down at the
next start but keeps each flat's provider permission and visibility in
`flats.db`; it never publishes a flat or changes visibility. A SIGHUP reload
applies the removal at once only to a provider no flat allows; one that flats
still allow keeps its routes until the next start, and the reload logs that.
While such a reverse proxy kind stays on, the other kind is not turned on,
because the two share one listener; it is turned on once the first is off.

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

zrok keeps the connected account's token and identity in `zrok-environment/`
(mode 0600), never in `config.json`, unless `zrok.environment` names an
environment managed with the zrok CLI; `zrok/` records every name the host
reserved in `zrok.namespace`. `cloudflare-quick` has no settings and no
credentials to enter; its per-flat tunnel credentials in `cloudflare-quick/`
are deleted with the flat. How both providers serve flats is in
[Publishing through zrok](networking.md#publishing-through-zrok) and
[Publishing through a Cloudflare quick tunnel](networking.md#publishing-through-a-cloudflare-quick-tunnel).

### Reverse proxy keys

`proxy.url_template` and `proxy.addr` belong to the reverse proxy providers
`proxy` and `proxy-public`; how they serve flats is in
[Serving flats behind a reverse proxy](networking.md#serving-flats-behind-a-reverse-proxy).
The operator sets them in Settings → Networks → Reverse proxy (the template
only) or with offline `flats config`. MCP, agents and API routes outside the
console cannot change either key or turn a reverse proxy kind on.

The template is an `http` or `https` URL whose host is `{host}`, a dot and a
lowercase DNS name, with an optional port:

- `{host}` appears once and makes up the whole leftmost label. Flats puts a
  flat's slug, a preview's name or an old slug there.
- The domain after `{host}.` is at most 189 characters, so the address of the
  longest route (a 63-character preview label) is still a valid DNS name.
- A port equal to the scheme's default (`:443` for `https`, `:80` for `http`)
  is refused with a hint to omit it, because browsers leave it out of
  `Origin`.
- The same checks as `console.external_origins` refuse a path, query,
  fragment, user information, IP literal, `localhost` or a `.localhost` name.

Checks across keys, applied to the whole document whenever it is saved or
loaded:

- `network.permitted` lists at most one of `proxy` and `proxy-public`.
- Either one needs a template, and the template cannot be cleared while one is
  permitted.
- While either one is permitted, `proxy.addr` differs from `host.console_addr`
  and `host.local_addr` (two ephemeral `:0` ports never collide). While
  neither is, the address is never bound, so a configuration that already uses
  `127.0.0.1:7880` for another listener keeps starting. The check compares
  the addresses as written: `localhost:7880` and `127.0.0.1:7880` pass it, and
  the clash then shows up as a bind error when the listener starts.
- `network.default_private` can be `proxy` and `network.default_public` can be
  `proxy-public` only while the host permits that kind.
- No `console.external_origins` entry may be an address the template can
  produce, that is one DNS label followed by the template's domain. The check
  runs on the whole document, so whichever of the two is saved second is
  refused.

Adding both keys with defaults and widening the defaults' allowed values kept
`schema_version` at 5.

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

`netbird.console_hosts` does the same on NetBird: each name is its own NetBird
peer serving the console over plain HTTP at `http://NAME.<NetBird domain>/` to
the peers the NetBird access policies allow. It needs `netbird` in
`network.permitted`, and NetBird cannot be turned off while it is not empty.
Over HTTP the console accepts a change only from its own page: a same-origin
`http` `Origin` with the console's own header (see
[Serving through NetBird](networking.md#serving-through-netbird)).

The console's Settings → Console adds and deletes names on both
networks while the host runs; there is no single name to edit. To change the
name, add the new one, point agents at it (`flats connect`), then delete the
old one.

- A name is a lowercase DNS label. It is refused when a flat, an open
  preview, a rename redirect, another console domain or a node still being
  logged out uses it, and no flat or preview can take a console domain's
  name afterwards. `flats` stays reserved for the console even when it is not
  listed.
- Deleting a name saves the list first, then retires its node: a tailnet
  node is logged out, a NetBird peer is stopped, and the identity is deleted.
  Tailscale keeps a logged-out machine in the tailnet's machine list (as
  expired), so its name stays taken: to use the same name again, remove that
  machine in the Tailscale admin console first; otherwise the tailnet names
  the new node `NAME-1`, and the console says so.
- The last console domain, counting both lists, can be deleted only from the
  console on the loopback address, or with `flats config set` while the host
  is stopped. Deleting the name the page is open on needs another console
  domain that is ready, and moves the page there.
- `flats.db` records every console node (`console_nodes`), so a name removed
  with `flats config set` or a text editor is retired at a SIGHUP reload or
  the next start. A
  retirement that fails keeps the name reserved; the console lists it and
  retries on request, and the next reload or start retries it.
- A listed name that a flat, preview or redirect already uses is not served;
  the console shows the conflict, and deleting it leaves that owner's node
  alone. A name listed again while its old node is still being retired is
  served once that retirement succeeds.
- Approval links and their event messages, `console_url` / `mcp_url` in
  `flats status` and the page the console moves to after deleting the
  domain it is open on use the console domain the request came through
  while it is served (see [Preferred console](#preferred-console) for the
  other requests). The agent discovery documents (`/llms.txt`,
  `/llms-full.txt`, the agent guide) name the address they were fetched
  from instead ([llms.txt](agents.md#llmstxt)). `console_urls` lists every served name, tailnet
  names first, whatever the preference.
- `--console-host NAME` on `flats serve` replaces `tailscale.console_hosts`
  with `[NAME]` for that run and pins it: the console cannot change the
  tailnet list until the service stops passing the flag.

`flats config set tailscale.console_hosts flats` sets the list while the host
is stopped, as on a new remote server.

#### External origins behind a reverse proxy

Declare the origins used by an existing reverse proxy in Settings → Console,
or while Flats is stopped:

```sh
flats config set console.external_origins 'https://flats.example.org'
```

The JSON value is an array, for example
`"external_origins": ["https://flats.example.org"]` inside `console`.
Values must use `http` or `https`, lowercase DNS names and optional ports
(1–65535, without leading zeros). Paths (including a trailing slash), queries,
fragments, user info, IP literals, localhost names and numeric final DNS labels
are refused. Each authority can select
only one scheme; duplicate default-port aliases are refused too.

This setting accepts the declared Host on the management listener. A present
browser Origin must match that request's declared scheme and authority;
other origins, including other configured origins, are refused. Links and
agent discovery documents use the declared scheme even when the proxy
forwards HTTP. `Forwarded` and `X-Forwarded-*` never select these URLs.
Changes in Settings apply without a restart; removing an origin immediately
blocks subsequent requests through it. The console asks for confirmation before
removing the origin currently in use; cancelling keeps the staged edits. Save
removals from a remaining address
or use offline configuration if you remove the address you are using.

This exposes the whole console, approval, API and MCP surface. **It does not
add authentication.** Protect the proxy with authentication or restrict it to
a trusted private network, and keep the upstream listener inaccessible to
untrusted clients. Flats does not create the proxy or publish a DNS record.
Agent-only origins and external preferred-console addresses are deferred;
the existing fallback order below is unchanged. Agents cannot edit this
setting through MCP or API. Built-in authentication is tracked separately
in [issue #139](https://github.com/gosuda/flats/issues/139).

#### Preferred console

A link that cannot name the console domain its request came through (the
request came over the loopback address, or through a domain that is no longer
served, such as one being deleted) names the preferred console,
`console.preferred_host`:

- `local` is the loopback management address; `tailscale:NAME` and
  `netbird:NAME` name a console domain in `tailscale.console_hosts` or
  `netbird.console_hosts`. It is never a free URL, so a link only ever names
  an address Flats serves itself. The Host header of a request never
  changes a link.
- A preferred domain is used only once its node is ready (a tailnet node
  has logged in and has its certificate, including while its key is about
  to expire; a NetBird peer is up). Unset, or
  while the named domain is not ready (still starting, its network off, or
  being retired), links use the first served tailnet name, then the first
  served NetBird name, then the loopback address, where served means, as
  before the setting existed, that the domain's node has been started. The
  order is fixed, and unset gives exactly the links of hosts without the
  setting.
- The console's Settings → Console chooses it among the loopback
  address and the listed names (`PUT /console/api/console-domains/preferred`)
  and warns when the preferred domain is still starting or not served. `flats config set
  console.preferred_host tailscale:flats` sets it while the host is stopped.
  Both refuse a NAME that is not in its list in `config.json`. Agents cannot
  change it.
- Deleting the preferred name from its list, in the console or with
  `flats config set` or `unset`, unsets the preference in the same change.
  A text edit that leaves it naming an unlisted domain does not stop Flats:
  the preference is ignored, with a warning in the log, in
  `flats config validate` and in the console.
- It is not pinned by `--console-host`, which replaces only the tailnet list:
  a NAME must still be in the list `config.json` holds, and a preference
  naming a domain the flag leaves out is ignored for that run.
- When the console is deleted through the domain it is open on, the page
  moves to the preferred domain if it is ready, else to the first ready one
  in the same order.

### Prometheus metrics

Metrics are enabled by default on the management listener, including per-flat
Prometheus series. These defaults also apply to existing installations when
these new keys are absent. The per-flat controls are unreleased; their earlier
false default never became a released configuration default. Released metrics
always included per-flat series, so this true default preserves that behavior.
Configuration schema 5 already includes the `host.console_addr` rename;
metrics controls need no further schema change or migration:

```json
{
  "metrics": {
    "enabled": true,
    "expose_on": ["localhost"],
    "per_flat": true
  }
}
```

`expose_on` accepts a unique list of `localhost` (the `host.console_addr`
management listener), `console` (managed Tailscale and NetBird console domains),
and `external`
(requests through `console.external_origins`). An external-origin request
remains external when its reverse proxy connects over loopback. Unknown names,
duplicates and values that are not lists are invalid. `[]` exposes nowhere.
The unreleased `loopback` spelling is invalid; it is not an alias.
`localhost` names the listener category, not a DNS-name or client-source-IP
filter. Changing `host.console_addr` to a non-loopback address also changes
where that listener can be reached.

With `enabled: false`, or on an excluded access point, admitted requests
return 404 without `Allow`. Existing Host and Origin guards still reject
invalid requests with 403, and GET/HEAD still reject cross-site browser
requests with 403. An enabled, selected endpoint serves GET/HEAD and returns
405 with `Allow: GET, HEAD` for other methods. These settings add no
authentication and do not change the endpoint path or port.

`per_flat: false` omits every per-flat Prometheus series, including request,
worker, preview/check CPU and storage series. Host process figures, aggregate
flat and network counts, management requests and host-wide board and
notebook metrics remain. By default, or with explicit `per_flat: true`,
metrics export the existing series with flat slugs, including Private and unpublished flats.
Only allow trusted scrapers to reach the endpoint. Analytics, its stats API
and usage collection continue with either setting, even when Prometheus
metrics are disabled.
The unreleased `metrics.flat_metrics` key is invalid; use `metrics.per_flat`.
This name change adds no alias, migration or schema revision.

Stop the host, use `flats config set` or edit `config.json`, then restart it.
For example, these offline commands enable console-domain scraping while
omitting per-flat series:

```sh
flats config set metrics.expose_on localhost,console
flats config set metrics.per_flat false
```

Per-flat series are included by default. To retain the exposure from before
these controls were introduced, explicitly set:

```json
{
  "metrics": {
    "enabled": true,
    "expose_on": ["localhost", "console", "external"]
  }
}
```

See [Scraping metrics](networking.md#scraping-metrics) for scraper setup.

### Operator keys

Flats keeps the keys an operator enters in `flats.db`, encrypted with
`secret.key` like flat secrets, and never writes them to `config.json`, logs,
events or API responses:

| Key | Use | Kept |
| --- | --- | --- |
| Tailscale auth key | logs in every new tailnet node | until removed |
| NetBird setup key | registers every new NetBird peer (one per flat and console domain) | until removed |
| zrok account token | connects the zrok account | until connected; the zrok environment in `zrok-environment/` then holds it |

Each value is sealed with AES-256-GCM under `secret.key`, with
`"host-key\x00"` followed by the key's name as additional data, so a stored
value cannot be moved to another key, and the prefix keeps operator keys
apart from flat secrets sealed under the same `secret.key`. A
value has no white space and at most 4096 bytes. A Tailscale key must be an
auth key (`tskey-auth-…`); OAuth client secrets (`tskey-client-…`) and API
tokens (`tskey-api-…`) are refused. Use a reusable key so every new node can
log in, and leave it untagged so console decisions record who made them. A
NetBird setup key is a UUID; [Serving through NetBird](networking.md#serving-through-netbird)
lists the options it needs.

A key reaches Flats in one of these ways:

- **Console**: each network's page under Settings → Networks sets, replaces
  and removes its key while the host runs. A new Tailscale key or NetBird setup key is used by nodes and peers
  started afterwards; those already registered keep their identity.
- **`flats key`** while the host is stopped. It takes the host lock like
  `flats config set`, refuses while a host runs, and checks that `flats.db`
  belongs to the configuration (`--config PATH` or `--data DIR` select it):
  - `flats key set tailscale|zrok|netbird` reads the key from standard input
    (a prompt without echo on a terminal) or `--from-file PATH`, never from
    the command line, and replaces a stored one. A zrok token waits for the
    next start, which connects it when no account is connected. With
    `host.secret_key_path` it needs the key file there and never creates one.
  - `flats key ls` lists the keys with their state, source and time, never
    their values.
  - `flats key rm` removes a stored key or a waiting zrok token and needs no
    `secret.key`; `flats key ls` needs none either. Disconnecting a zrok
    account stays in the console.
- **At start**, in this order of priority:
  1. the flags `--tailscale-authkey-file` (`--authkey-file` for a legacy
     service), `--zrok-token-file` and `--netbird-setup-key-file`, each naming
     a file;
  2. the environment variables `FLATS_TAILSCALE_AUTHKEY`, `FLATS_ZROK_TOKEN`
     and `FLATS_NETBIRD_SETUP_KEY`, or their `_FILE` forms naming a file
     (setting both forms of one key is refused); a legacy service also reads
     `TS_AUTHKEY` and `TS_AUTH_KEY`;
  3. the deprecated `credentials.tailscale_authkey_file`, which is removed
     from `config.json` once its key is stored.

  A key file is read whole and trimmed of surrounding white space. Every
  input given is read and checked, also one a higher-priority input
  overrides: a missing, unreadable, empty or invalid one stops the start with
  status 78. Flats removes all of these variables from its environment after
  reading them, before any provider or worker process starts.

A key given at start only fills in what is missing: the Tailscale key and
the NetBird setup key are stored when `flats.db` holds none of that kind, and a zrok token is used when no account
is connected. A key registered in the console or with `flats key set`
therefore stays after restarts even while a service still passes an older
flag or variable; a start value that differs from the stored Tailscale or
NetBird key is ignored, with a warning in the log and the console. To
replace a key, register the new one.

A host started with `--config` never reads the `TS_*` variables: it refuses
to start a Tailscale backend, at start or when the console turns Tailscale
on, while `TS_AUTHKEY`, `TS_AUTH_KEY`, `TS_CLIENT_SECRET`, `TS_CLIENT_ID`,
`TS_ID_TOKEN` or `TS_CONTROL_URL` is set, because the embedded tsnet would
read them and bypass Flats.

If operator keys are stored and `secret.key` is missing, the host refuses to
start instead of creating a new key (see [Starting](#starting)). The console,
`flats key set` and the keys given at start read the key file each time they
seal or open a key, so a key file that you replace while the host runs changes
what they open at once. With
[`host.secret_key_path`](#keeping-the-key-outside-the-data-directory) they read
the file that it names.

No MCP tool and no agent API route reads or sets a key. The console's routes
are protected against cross-site requests, not authenticated: a program that
can reach the management listener could imitate the console, as it can for
approvals.

### Editing

- **Console**: the system and Portal keys, `network.permitted`,
  `network.default_private`, `network.default_public` and
  `proxy.url_template` through Networks, and `tailscale.console_hosts` and
  `netbird.console_hosts` through Console.
  A save
  rereads the file under a lock and fails if it changed on disk since the
  host read it; SIGHUP reloads an outside edit (a restart also picks it up). Host,
  `network.private_backend` and `proxy.addr` keys are read-only there. A service that still
  passes `--relays` pins the relays, one that passes `--portal`, `--permit` or
  `--network tailscale` pins those providers, and one that passes
  `--console-host` pins the console domains: the console refuses to change
  them until the service is reinstalled, and a SIGHUP reload refuses a file
  that changes them.
- **`flats config set KEY VALUE` / `unset KEY`**: offline; they take the host
  lock and refuse while a host runs.
- **A text editor**: allowed. Sending SIGHUP to a `flats serve` that runs as
  a service or in a container without a TTY reloads the file: the keys whose
  Applies cell names a live path take effect at once with the same side
  effects as a console save, the others apply at the next start, and an
  invalid file is refused and logged, changing nothing. A `flats serve`
  started in a terminal stops on SIGHUP instead, as when the terminal
  closes, and one started with SIGHUP ignored (as under `nohup`) ignores it.
  Without a reload the change applies at the next start, which fails if the
  file is invalid. Windows has no SIGHUP: run `flats install` to restart the
  service.

Every Flats write keeps the previous file in `config-history/`, writes a
synced temporary file, renames it into place and syncs the directory.

## Versions

These versions are independent:

| Version | Where | Bumped when | Migration |
| --- | --- | --- | --- |
| `schema_version` | `config.json` | the format changes incompatibly | automatic at start |
| `user_version` | `flats.db` | any table, column or data-meaning change | automatic at start, after a backup |
| a flat's database | `flats/<slug>/data/` | the flat decides | Flats never touches it |
| a notebook's `user_version` | `flats/<slug>/notebook/notebook.db` | the notebook schema changes | by the notebook when it opens; a newer version is refused without writes |

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
version. Historical schema 1–4 inputs remain unchanged; expected migration
outputs track schema 5 and the canonical new key.

An older `config.json` is converted at start. The conversions that change
what an operator sees:

- Schema 5 renames `host.management_addr` to `host.console_addr`, preserving
  every explicitly configured address. An absent key stays absent, with the
  same loopback default `127.0.0.1:7878`. A new-key-only file also keeps its
  value. Both keys together are rejected, even when equal. Migration errors
  for schema 1–4 name both keys; schema 5 reports the old key as unknown,
  whether alone or alongside the new key. Rejection leaves the file and
  history untouched. Offline commands and runtime overrides use only the
  new name. The migration requires no confirmation because exposure is unchanged.
- `host.console_host` becomes `tailscale.console_hosts`, listing the old name
  only when `network.private_backend` was `tailscale` and the name was a
  lowercase DNS label, the only case in which a console ran on the tailnet.
  No console opens that was not open before.
- `netbird.device_name` is dropped: every flat and console domain runs its
  own NetBird peer named after it.
- `system.keep_versions` becomes `system.version_quota` with the same number
  (`0` stays unlimited); a file without it gets the default 20. Versions are
  no longer pruned: the quota counts every available version, the live one
  included, so `keep_versions: 1` leaves no room to create a replacement for a
  deployed flat. A flat over its quota keeps all its versions and refuses new
  ones until the operator raises the quota or deletes older versions.
  Versions pruned before the upgrade stay unavailable and do not count.

After successful migration, `config-history/` holds the previous file. A
binary that supports only schema 4 refuses a schema 5 file without changing
it. Before running an older release, restore its compatible configuration and
corresponding data backup. The installer's informational URL discovery reads
`host.console_addr` first and falls back to `host.management_addr` for files
not yet migrated; this does not accept configurations the host rejects.

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
restore a full backup taken before the upgrade
([What to back up](#what-to-back-up)). The copy in `backups/` holds only
`flats.db`. Restored alone, it loses every change recorded there since, and the
flat directories no longer match it, so use it only if nothing changed since it
was taken.

## Starting

`flats serve --config PATH` (or `FLATS_CONFIG`) runs from that file. Only
`--listen`, `--local-addr`, `--console-host` and `--runtime` may override a
value for one run; they are not saved. `--console-host NAME` makes the console
domains `[NAME]` for that run and pins them. The key file flags
`--tailscale-authkey-file`, `--zrok-token-file` and `--netbird-setup-key-file`
are accepted too; see [Operator keys](#operator-keys). Other configuration
flags are refused, including `--operator-credential-file`.
`--operator-credential-stdin` is accepted and ignored, with a warning: the
operator credential is no longer used.

Without `--config` and `FLATS_CONFIG`, `flats serve` keeps accepting the older
flags and uses `<data-dir>/config.json`. This is what an installed service
from an earlier release runs after an upgrade, and what `flats serve` with no
arguments does. Each older flag given must match `config.json`; a different
value, `--operator-credential-file` included, stops the start with status 78
(see [Moving an existing installation](#moving-an-existing-installation)).

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
or any of `flats/`, `secret.key`, `secret.key.recovery`,
`secret.key.recovery.id`, `tsnet/`, `portal/`, `zrok/`,
`zrok-environment/`, `cloudflare-quick/`, `netbird/`, `backups/`,
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
`--config` host refuses to start while a `TS_*` variable tsnet reads is set;
see [Operator keys](#operator-keys).

If flats have stored secrets, or operator keys are stored, and `secret.key` is
missing, the host refuses to start instead of creating a new key. Restore the
key with `flats secret restore-key` or from the same backup as `flats.db`, or,
when only operator keys are stored, remove them with `flats key rm` and
register them again. [Recovering a lost secret.key](#recovering-a-lost-secretkey)
covers the commands.

With `host.secret_key_path` the host never creates a key, with or without
sealed values. Before any listener opens, it checks the file that the setting
names, and a refusal exits with status 78 and names the path and the setting;
[Keeping the key outside the data directory](#keeping-the-key-outside-the-data-directory)
lists the cases.

### Keeping the key outside the data directory

By default `secret.key` lies in the data directory, so a copy of that directory
holds the sealed values and the key that opens them. `host.secret_key_path`
names another file for the key, for example a Docker secret or a file that only
the service user can read. The [Secret key path](spec/secret-key-path.md) spec
has the rules.

```sh
flats config set host.secret_key_path /etc/flats/secret.key --config PATH
```

The value is an absolute path without a NUL byte, and it must not lie inside
`host.data_dir`. Stop the host first, because `flats config set` refuses while
a host runs. The setting applies at the next start. A SIGHUP reload lists it as
awaiting a restart and the running host keeps the key that it holds.

With the setting, the host only reads the key. It never creates, writes,
renames or deletes the file, and it makes no temporary file beside it, so a key
on a read-only mount works. Flats does not check the mode or the owner of the
file. The user that runs the host must be able to read it (uid `65532` in the
container image). It must be a regular file of exactly 32 bytes, and the host
refuses to start otherwise. Flats never creates a key at a configured path.

Make a key with 32 random bytes:

```sh
(umask 077 && head -c 32 /dev/urandom > /etc/flats/secret.key)
```

```powershell
$k = New-Object byte[] 32
[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($k)
[System.IO.File]::WriteAllBytes('C:\ProgramData\flats\secret.key', $k)
```

On Windows, give the file an access list for the service account only. Flats
does not set one.

To move the key of an existing host out of the data directory, stop the host,
move the file, and set the path:

```sh
mv DATA/secret.key /etc/flats/secret.key
flats config set host.secret_key_path /etc/flats/secret.key --config PATH
```

Keep the key in a folder that no link and no mount reaches from the data
directory. The checks below follow links, but they cannot find every name that
a file system gives one file, such as a Windows short name, a hard link or a
bind mount of the folder into the data directory.

The start refuses, with status 78 and before any listener opens, in each of
these cases. The message names the path and the setting, and it holds when
`flats.db` has no sealed value too:

- nothing is at the path, or a link there points to nothing;
- the path is not a regular file once links are followed, or it is not 32
  bytes, or the user cannot read it;
- a file, a directory or a link exists at `DATA/secret.key`: two keys would be in
  use, and a copy of the data directory would hold the one that you wanted to
  keep out. Move that file away;
- the key, after the system resolved every link, lies inside the data
  directory.

`flats key set`, `flats secret backup-key`, `restore-key` and `reset` refuse in
the last two cases too. `flats key ls` and `flats key rm` touch no key and still
work. `flats backup` refuses in them before it writes anything.

`flats config set` and `unset` load the file first, so they cannot repair a
file that the check refuses, for example a key path that lies inside
`host.data_dir`. Edit `config.json` by hand: remove `host.secret_key_path` or
give it a path outside the data directory. For the same reason,
`flats config set host.data_dir` refuses a directory that contains the key path.

To put the key back in the data directory, or to go back to an older Flats,
stop the host, copy the key to `DATA/secret.key`, and clear the setting:

```sh
cp /etc/flats/secret.key DATA/secret.key
flats config unset host.secret_key_path --config PATH
```

Copy before you clear. `flats config unset` works while a file is at
`DATA/secret.key`, but a start between the two steps then refuses instead of
creating a new key. An older release refuses a `config.json` that holds
`host.secret_key_path`, as it refuses any key it does not know, so clear the
setting before you start it.

The key commands and `flats backup` run as processes of their own. A key that
only the service process can see is missing for them. This holds for a systemd
credential, and for a Docker secret that is mounted into the service container
([Docker](container.md#keeping-the-key-outside-the-volume)). For such a key,
`flats backup` needs `--exclude-secret-key`, `flats secret restore-key` needs
`--out FILE` (it writes the recovered key to a new file of mode 0600 and changes
nothing at the path), and the other key commands cannot run unless you give them
the same mount. A plain file that the service user can read, for example
`/etc/flats/secret.key` with mode 0400, works for every command. A drop-in for a
system service that loads a systemd credential:

```ini
# /etc/systemd/system/flats.service.d/key.conf
[Service]
LoadCredential=secret.key:/etc/flats/secret.key
```

```sh
flats config set host.secret_key_path /run/credentials/flats.service/secret.key --config PATH
```

`systemd.exec(5)` names the credentials directory of a service. The path in
`config.json` is fixed, so it cannot use `$CREDENTIALS_DIRECTORY`.

### Recovering a lost secret.key

`secret.key` opens every flat secret and stored operator key, and nothing
else does. Three commands on the host keep a way back when the file is lost.
They work on the data directory, never call the host, and no agent tool or API
route reaches them. With `host.secret_key_path` they use the file that it
names, and the recovery copy stays in the data directory.

```sh
flats secret backup-key [--out PATH] [--password-file PATH] [--config PATH | --data DIR]
flats secret restore-key [--from PATH] [--out FILE] [--password-file PATH] [--config PATH | --data DIR]
flats secret reset [--yes] [--config PATH | --data DIR]
```

- `backup-key` encrypts a copy of `secret.key` under a recovery password and
  writes it to `secret.key.recovery` in the data directory, with the key's ID
  in `secret.key.recovery.id`. It works while the host runs. `--out PATH` also
  writes the copy to a new file to keep apart from the host, in an existing
  folder outside the data directory. On a terminal the command asks for the
  password twice without echo; `--password-file PATH` reads it from a file
  instead, without its trailing line ends. The password has at least 12
  characters and at most 1024 bytes. Run the command again to change it.
  Flats keeps no copy of the password.
- `restore-key` needs the host stopped and nothing at `secret.key`, or, with
  `host.secret_key_path`, nothing at that path. It reads
  `secret.key.recovery`, or `--from PATH`, asks for the password, checks that
  the key opens every sealed value in `flats.db`, and only then writes
  `secret.key`, or the file at `host.secret_key_path`. It never replaces a
  key. When `flats.db` holds no sealed value, there is nothing to check the key
  against: the command says so and writes any key the password opens. It
  refuses a copy written at a work factor above 2^18, the age default that
  `backup-key` and `age -p` use. With `host.secret_key_path` its synced
  temporary file lies beside the configured path. When it cannot write there,
  for example on a read-only mount, it writes nothing and names `--out`.
  `--out FILE` writes the recovered key to a new file of mode 0600 instead,
  after the same checks of the key. It needs no empty path, because the path may
  hold a wrong key, and it changes nothing there. `FILE` must not lie in the data
  directory. Move `FILE` into your secret store and remove it.
- `reset` is for a host that has lost both the key and the password. With the
  host stopped, and nothing at `secret.key` or a key there that opens none of
  the values, it lists the flat secrets and operator keys it will delete,
  asks you to type `reset` (or takes `--yes`), deletes them and leaves an
  event on each flat. The next start creates a new key: run `backup-key`
  after it, set the secrets again with `flats secret set` and redeploy, and
  register the operator keys again with `flats key set`. With
  `host.secret_key_path` the next start creates no key: put a key at that path
  before it, and run `backup-key` after it.
  A key at `secret.key` that opens any of the values makes `reset` refuse.
  Remove only the values it does not open instead: operator keys first, with
  `flats key rm` while the host is stopped, because the start refuses while
  a stored Tailscale or NetBird key does not open, then flat secrets with
  `flats secret rm` once the host runs. Anything else at `secret.key`, such as
  a file of the wrong size or a broken link, must be moved away first.

The copy is the key in the [age](https://age-encryption.org) format, encrypted
with a passphrase, so the `age` tool opens it without Flats:

```sh
(umask 077 && age -d -o secret.key secret.key.recovery)
```

Run it in the data directory with the host stopped and nothing at
`secret.key`, because `age` replaces an existing output file. Without the
`umask`, `age` creates the file readable by other users; with it the file
gets the mode 0600 that Flats uses. This way skips the check against
`flats.db` that `restore-key` makes. With `host.secret_key_path`, name that path
after `-o` instead of `secret.key`. A copy made by hand with `age -p` has no
ID file, and the host warns about it until `backup-key` writes one.

A copy of the data directory includes the recovery file, so anyone holding a
backup can guess passwords offline. Each guess costs one scrypt derivation,
about a second and 256 MiB of memory. Use a long password, and keep it apart
from the host and its backups.

At each start, when `secret.key.recovery` exists, the host compares the key's
ID with `secret.key.recovery.id` and logs a warning that names
`flats secret backup-key` when they differ or the ID file is missing, for
example after `reset` let the start create a new key.

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

To go back to an older release straight after the move, put the
`backups/flats.v<N>.<time>.db` that the move wrote back as `flats.db`. If the
old host had a `network-provider.json`, also put the archived
`network-provider.<time>.json` back under that name. The older binary ignores
`config.json`. After any change, restore a full backup taken before the move.
