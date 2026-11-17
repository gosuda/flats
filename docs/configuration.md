# Configuration and storage

This page says which data Flats stores where, how `config.json` is versioned,
and how an existing installation moves to it.

## Where data lives

The question "who decides this value?" picks the place:

| Decided by | Stored in | Examples |
| --- | --- | --- |
| The operator | `config.json` | addresses, system policies, permitted networks, console domains, provider settings |
| Flats while running | `flats.db` | flats, drafts, versions, deployments, revision metadata of documents, designs, boards and notebooks, approvals, per-flat provider permission, events, the [audit log](#audit-log) of secret, key, visibility and grant changes, analytics (page views, and per-day usage in `usagedays`), ordinary app environment variables, encrypted secrets, encrypted operator keys, console node records, [Autopilot settings](#autopilot-settings), [public instance settings](#public-instance-settings) |
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
  secret.key.next             the pending key of a key rotation, removed when the
                              rotation ends; flats backup never copies it
                              (beside the key when host.secret_key_path names it)
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

Operator keys (the Tailscale auth key, the NetBird setup key, a zrok token
waiting to be used, and the URL and signing secret of the event webhook) are kept in `flats.db`, encrypted with `secret.key`; see
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
  is a file outside the data directory, and you keep it apart from the copy.
  A [key rotation](#rotating-the-key) replaces the key. A copy of `flats.db`
  from before it opens only with the old key, so take a new backup after a
  rotation;
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

The command never copies the pending key of a
[key rotation](#rotating-the-key), `secret.key.next`, or a temporary key file
`.secret-key-*`, and the manifest lists them in `excluded`. `flats backup --out`
refuses, before it writes anything, in the two states in which the start refuses
after a rotation that did not end
([If the command stops](#if-the-command-stops)). With `--exclude-secret-key` it
refuses only in the first of them, while a pending key waits. A rotation can
leave a recovery copy that still holds the old key, for example when it could
not write the new one. A backup from that time holds a recovery copy that does
not open its `flats.db`. With its `secret.key` the backup is complete all the
same. Made with `--exclude-secret-key`, it cannot be restored with the recovery
password. The start warns about such a recovery copy, and `flats backup` does
not.

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
| Webhook URL (`webhook-url`) | the address that the [event webhook](#event-webhook) posts to | until removed |
| Webhook signing secret (`webhook-secret`) | signs every event of the webhook | until removed |

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
  - `flats key set tailscale|zrok|netbird|webhook-url|webhook-secret` reads the key from standard input
    (a prompt without echo on a terminal) or `--from-file PATH`, never from
    the command line, and replaces a stored one. A zrok token waits for the
    next start, which connects it when no account is connected. With
    `host.secret_key_path` it needs the key file there and never creates one.
    It refuses in the two states in which the start refuses after a key
    rotation that did not end ([If the command stops](#if-the-command-stops)).
  - `flats key ls` lists the keys with their state, source and time, never
    their values.
  - `flats key rm` removes a stored key or a waiting zrok token and needs no
    `secret.key`; `flats key ls` needs none either. Disconnecting a zrok
    account stays in the console.
  - The two keys of the [event webhook](#event-webhook), `webhook-url` and
    `webhook-secret`, have no console page and no start input. `flats key` is
    the only way to set, list and remove them.
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

`flats secret rotate-key` replaces the key while the host is stopped and seals
every stored key value again under the new key
([Rotating the key](#rotating-the-key)). No value changes, and `flats key ls`
shows the same source and time as before.

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
or any of `flats/`, `secret.key`, `secret.key.next`, `secret.key.recovery`,
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
covers the commands. When every sealed value carries the same key ID, the
refusal names that ID, which is the ID of the key that sealed them.

The host also refuses to start in two states that a
[key rotation](#rotating-the-key) can leave. In the first, a pending key of a
rotation waits: the sealed values are under the new key, and the key file does
not hold it yet. The refusal names `flats secret rotate-key`, which finishes
the rotation. In the second, the key file holds a valid key, every sealed value
carries a key ID, and none carries the ID of that key. The refusal names the ID
of the key and the key IDs of the values. A host in one of these states would
seal new values under a key that does not open the others.
[If the command stops](#if-the-command-stops) says what to do. The start never
writes, moves or removes a key file for a rotation.

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
refuses to start otherwise. The host never creates a key at a configured path.
Two commands write a key there for you. `flats secret restore-key` writes the
recovered key when nothing is at the path. `flats secret rotate-key` writes the
new key into the file, and when it finishes a rotation that stopped and nothing
is at the path, it creates the file ([Rotating the key](#rotating-the-key)).

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

`flats key set`, `flats secret backup-key`, `restore-key`, `rotate-key` and
`reset` refuse in the last two cases too. `flats key ls` and `flats key rm` touch
no key and still work. `flats backup` refuses in them before it writes anything.

With a key at the default path or at a configured path, the start also refuses
in two states that a [key rotation](#rotating-the-key) can leave, and
`flats key set`, `flats secret backup-key` and `flats backup --out` refuse in
the same two states ([If the command stops](#if-the-command-stops)).

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
`/etc/flats/secret.key` with mode 0400, works for every command.
`flats secret rotate-key` reads such a file and cannot write it, so it needs
`--out FILE` there. [Rotating the key](#a-key-at-hostsecret_key_path) has the
steps for each kind of key file.

A systemd credential suits a system service that you wrote and that runs as its
own user. `flats install` makes a user unit, `flats.service`, that runs as your
own user, and a credential adds no protection there over a file with mode 0400,
so use the plain file for it. A drop-in for a system service:

```ini
# /etc/systemd/system/flats.service.d/key.conf
[Service]
LoadCredential=secret.key:/etc/flats/secret.key
```

```sh
flats config set host.secret_key_path /run/credentials/flats.service/secret.key --config PATH
```

A user unit has its drop-in in `~/.config/systemd/user/flats.service.d/key.conf`,
and its credentials directory lies under `$XDG_RUNTIME_DIR/credentials/flats.service/`,
for example `/run/user/1000/credentials/flats.service/secret.key` for uid 1000.
`systemd.exec(5)` names both directories. The path in `config.json` is fixed, so
it cannot use `$CREDENTIALS_DIRECTORY`.

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
  Flats keeps no copy of the password. The command refuses in the two states
  in which the start refuses after a key rotation that did not end
  ([If the command stops](#if-the-command-stops)), because a copy of a key that
  sealed none of the values restores nothing.
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
  `reset` also refuses while a pending key of a rotation waits, and names
  `flats secret rotate-key`: that key opens the values, so nothing is lost.
  When every sealed value carries the same key ID, and the key file is missing
  or holds a key with another ID, `reset` names that ID before it asks. It says
  that a key file with that ID opens the values, for example the file that
  `rotate-key --out` wrote.

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

A [key rotation](#rotating-the-key) replaces the key. `flats secret rotate-key`
asks for the recovery password and writes `secret.key.recovery` again for the
new key, so the same password still opens the key of the host. A copy that
`backup-key --out` wrote is not changed and holds the old key. Run
`backup-key --out` again after a rotation.

### Rotating the key

Rotate the key when it may have leaked, or when a person who had access to it
has left. `flats secret rotate-key` makes a new key and seals every flat secret
and every stored operator key value again under it, in one transaction. No
value changes, so no flat needs a new deployment. The
[Secret key rotation](spec/secret-key-rotation.md) spec has the rules.

```sh
flats secret rotate-key [--out FILE] [--password-file PATH] [--yes] [--config PATH | --data DIR]
```

The command needs the host stopped. It takes the host lock like `flats key`,
refuses while a host runs, and checks that `flats.db` belongs to the
configuration. It works on the data directory and never calls the host, and no
agent tool or API route reaches it.

A rotation cannot be reversed, because the command does not keep the old key.
After it, the old key opens nothing in `flats.db`. The command therefore asks
before it writes. On a terminal it shows how many values it will seal again and
which file gets the new key, and asks you to type `rotate`. Any other answer
changes nothing. Without a terminal the command needs `--yes`, and `--yes` also
skips the question on a terminal.

The key path must hold a valid key, and that key must open every sealed value.
When it does not open them all, the command refuses and changes nothing,
because a rotation would leave the other values under a key that no file holds.
The message says how many values the key opens, lists the others without their
values, and names the ways out. The first is the right key, with
`flats secret restore-key`. When the key opens some values, `flats key rm`
removes an operator key that it does not open, and `flats secret rm` removes a
flat secret once the host runs. When the key opens no value,
`flats secret reset` deletes them all. When the values that do not open carry a
key ID, the message names it before these ways out. When the key also opens no
value and the values carry one key ID, the message names one more way first,
for the state after a rotation with `--out`. The file of that rotation holds
that key: run `rotate-key --out FILE` again with the same file, and put the key
of that file at the key path only after that run reported the re-seal durable.
With no sealed value there is nothing to check, and the command still makes a
new key.

The output gives the number of flat secrets and of operator keys that the
command sealed again, the key ID of the old key and of the new key, each file
that it wrote, and what happened to the recovery copy. It never shows a key or
a value. A key ID is 32 hex digits that name a key and do not help to open
anything. Each sealed value carries the ID of the key that sealed it, so the
key commands and the start can say which key file belongs to the values.
`flats key ls`, the console, the API and MCP show no key ID.

An interrupt before the command starts to write changes nothing. Once the
command has started to write, it completes its steps. A usage mistake exits
with status 2, a refusal with status 78 and any other failure with status 1.

#### Before you rotate

After a rotation the old key still opens one thing, a copy of `flats.db` from
before the rotation:

- A copy that `flats backup` made with its `secret.key` stays complete. It
  holds the key of its own moment.
- A copy without its key needs the old key. That is a copy made with
  `--exclude-secret-key`, or a copy of a host whose key is at
  `host.secret_key_path`.
- A recovery copy that you keep apart from the host (`backup-key --out`) holds
  the old key.

When such copies must stay readable, copy the old key away before you rotate.
Make a real copy and not a link. The command writes the new key into the key
file, so every name of that file gets the new key, a hard link included. For
the same reason, a rotation of a key file that two hosts use changes the key of
both. The confirmation and the output of the command say that every name of
the key file gets the new key.

After the rotation, store the new key apart again. Run
`flats secret backup-key --out PATH` again, and take a new backup.

#### A key in the data directory

Stop the host, run the command, and start the host again:

```sh
flats secret rotate-key
```

The new key goes into `secret.key`. The command writes into the file and does
not replace it, so the file keeps its owner, its mode and its access list.
Until `secret.key` holds the new key, the command keeps the key in the pending
key file `secret.key.next`. It removes that file at the end.

#### The recovery password

When `secret.key.recovery` exists in the data directory, the command asks for
its password once and opens the copy with it. At the end it writes the copy
again for the new key, under the same password. The password then opens the new
key, and the start does not warn. A copy that was out of date is made current
too. `--password-file PATH` reads the password from a file, and the command
needs the flag when it has no terminal. The command takes the password as it
is, also when it is shorter than `backup-key` accepts for a new password. A
wrong password, or a copy that does not open, is a refusal that changes
nothing.

If you lost the password, remove `secret.key.recovery` first. It opens nothing
without its password. Then rotate, and run `flats secret backup-key` to write a
new copy.

Without a recovery copy the command asks for no password and creates no copy,
and its output names `flats secret backup-key`. It refuses `--password-file`
then, because there is no copy to make current.

The command does not change a copy that you keep apart from the host
(`backup-key --out`). That copy holds the old key, so run
`flats secret backup-key --out PATH` again.

#### A key at `host.secret_key_path`

The command must read the key, and without `--out` it writes the new key into
the same file. When the key path is a link, the file that the link names gets
the new key. Until the key file holds the new key, the command keeps it in a
pending key file beside the key, `<key path>.next`. What you do depends on what
the user that runs the command can see and write.

**A file that the user can write, in a folder where the user can create a
file.** Rotate in place, as for a key in the data directory:

```sh
flats secret rotate-key --config PATH
```

**A file or a folder that the user cannot write.** This holds for the file with
mode 0400 above, for a read-only mount, and for a file with the read-only
attribute on Windows. The command refuses and names `--out`. With `--out FILE`
it writes the new key to the new file `FILE` and does not open the key path for
writing. You then put the key of `FILE` at the key path and remove `FILE`:

```sh
flats secret rotate-key --out ~/flats-new.key --config PATH
sudo cp ~/flats-new.key /etc/flats/secret.key
rm ~/flats-new.key
```

`FILE` must not exist, its folder must exist, and it must not lie inside the
data directory. After the command, `flats.db` holds the values under the new
key, and the key path still holds the old one. While `flats.db` holds sealed
values, the start refuses until the new key is at the key path. The `cp` above
writes into the existing key file, which keeps its owner and its mode. The
owner of a key with mode 0400 can also make it writable for the time of the
rotation and rotate in place, when the folder lets the user create the pending
key file.

**A Docker or Compose secret.** The secret is read-only, and only a container
that mounts it sees it
([Keeping the key outside the volume](container.md#keeping-the-key-outside-the-volume)).
Run the command in a one-off container that has the mounts of the service, and
give it a folder outside the data volume for `--out`. `docker compose run`
mounts the secrets of its service. The container user `65532` must be able to
write the folder:

```sh
docker compose stop flats
mkdir -m 700 newkey && sudo chown 65532 newkey
docker compose run --rm --no-deps -v "$PWD/newkey:/newkey" flats \
  secret rotate-key --out /newkey/flats.key
sudo cp newkey/flats.key flats.key
docker compose up -d --force-recreate --wait
sudo rm -r newkey
```

`flats.key` is the file of the secret in `compose.yaml`. The `cp` puts the new
key into it and keeps its owner and its mode, and the next line creates the
container again with the new secret. With a secret that an orchestrator stores,
create a new secret from the file and update the service with it. With plain
`docker run`, give the one-off container the volume and the key mount of the
host container:

```sh
docker stop flats
mkdir -m 700 newkey && sudo chown 65532 newkey
docker run --rm -it -v flats-data:/data \
  -v "$PWD/flats.key:/run/secrets/flats.key:ro" -v "$PWD/newkey:/newkey" \
  ghcr.io/gosuda/flats:latest secret rotate-key --out /newkey/flats.key
sudo cp newkey/flats.key flats.key
docker start flats
sudo rm -r newkey
```

Both forms give the one-off container a terminal, so the command can ask for
the confirmation and for the recovery password. Copy the new key over the old
one only after the command ended with success.
[If the command stops](#if-the-command-stops) says what to do otherwise.

**A key that the command cannot see.** A systemd credential exists only for the
service process, so the command finds nothing at the key path. It refuses and
says that a key that only the service process sees is missing for it. Give the
command a copy of the key for the time of the rotation, in a folder that
belongs to the service user and lies outside the data directory. Point
`host.secret_key_path` at the copy, rotate in place, put the new key into the
source file of the credential, and set the path back. These lines are for the
drop-in above and a service that runs as the user `flats`. Run them as root:

```sh
systemctl stop flats
install -d -o flats -m 0700 /var/lib/flats-rotate
install -o flats -m 0600 /etc/flats/secret.key /var/lib/flats-rotate/secret.key
sudo -u flats flats config set host.secret_key_path /var/lib/flats-rotate/secret.key --config PATH
sudo -u flats flats secret rotate-key --config PATH
cp /var/lib/flats-rotate/secret.key /etc/flats/secret.key
sudo -u flats flats config set host.secret_key_path /run/credentials/flats.service/secret.key --config PATH
rm -r /var/lib/flats-rotate
systemctl start flats
```

The `cp` writes into the source file of the credential, which keeps its owner
and its mode. Go on with the `cp` only after the command ended with success. If
the command stopped, run it again first. The pending key lies beside the key
path that the configuration names at that moment, so a change of
`host.secret_key_path` hides it from the command and from the start.

#### The owner and the access of a key file

Give a key file that you put in place the owner and the access that the host
needs. Flats sets none. On Unix, `FILE` has mode 0600 and belongs to the user
that ran the command. On Windows it takes the access list of its folder. The
user of the host must be able to read the file at the key path, for example
after `chown 65532` and `chmod 0400` for the container image. On Windows, give
the file an access list for the service account only. The same holds for a key
file that the command creates when it finishes a rotation and nothing is at the
key path. That file has mode 0600 and belongs to the user that ran the command,
and the output says that you must give it the owner and the access that the
host needs.

#### If the command stops

After a stop at any point, such as a crash, a kill or a loss of power, one file
on the host holds the key that opens every sealed value: the key file, the
pending key file, or `FILE`. The start and the command say which step is
missing.

| What you see | What it means | What to do |
| --- | --- | --- |
| The start refuses and names `flats secret rotate-key` | A pending key waits. The values are under the new key, and the pending key file holds it. The key file holds the old key, nothing or a damaged file | Run `flats secret rotate-key` again, without `--out`. It finishes the rotation, makes no new key and asks no confirmation. It asks for the recovery password when a recovery copy exists. `flats key set`, `flats secret backup-key`, `flats backup --out` and `flats secret reset` refuse until then |
| The host starts, and a file `secret.key.next` (`<key path>.next`) is left | The rotation stopped before it changed a value, and the old key is still in use. Or the new key is in place, and only the removal of the pending key is missing | Nothing is lost. Run `flats secret rotate-key` again. In the first case it starts a new rotation, which removes the file first. In the second it removes the file, writes the recovery copy when one exists, and makes no new key. Do not use that name for a file of your own, because Flats treats a valid key there as its own |
| After a rotation with `--out`, the start refuses and names the ID of the key at the key path and the key IDs of the values | The values are under the new key, which `FILE` holds. The key path still holds the old key | When the command told you to put the new key at the key path, do that and remove `FILE`. When it stopped before, run `flats secret rotate-key --out FILE` again with the same file. Put the key of `FILE` at the key path only after that run reported the re-seal durable, and keep the old key there until then, because a loss of power can still take the re-seal back |
| The start refuses with the same message, and no file of `--out` holds the key of the values | The key at the key path is valid, and it sealed none of the values | Restore the key that has the ID of the values with `flats secret restore-key`. When no such key is left, delete the stored values with `flats secret reset` |
| The command stops with status 1 at the re-seal, and names a file that stays or says to run it again | The re-seal failed, or the command could not make it durable. The file is the pending key or `FILE` | Run the command again, with the same `FILE` when you used `--out`. It reads the key IDs of the values to tell whether the re-seal took place. Then it finishes the rotation, starts a new one, or refuses a `FILE` that sealed no value, which you remove. With `--out`, keep the old key at the key path until that run succeeds |
| The command fails while it writes the key file | The re-seal is durable, and the pending key file holds the key of the values. The key file can be damaged, and the start refuses as in the first row | Remove the cause that the message names and run the command again. Or copy the bytes of the pending key into the key file by hand and run the command again, which then removes the pending key. Make a real copy, and not a symbolic link to the pending key file, because the command removes that file. Copy by hand only when the command named this way or reported the re-seal durable. Before that, a loss of power can still take the re-seal back |
| The command says that it could not write the recovery copy, or the start warns that the recovery copy holds another key | The rotation is complete. The recovery copy still holds the old key, or it holds the new key beside an ID file that is out of date | Run `flats secret backup-key`, which writes both files. After `--out`, run it when the new key is at the key path, because it refuses before |
| `rotate-key --out FILE` refuses because `FILE` exists | A run that stopped left `FILE`, the key of `FILE` is in place already, or `FILE` is a file that the command must not use | The message says which case it found. When `FILE` is the key file, also through a link, or lies inside the data directory, name a new file and do not remove `FILE`. When `FILE` is a second file with the key that the key path holds, the key is in place, and you can remove `FILE`. In every other case name a file that does not exist, and remove `FILE` only when it is a key file that an earlier run of the command left |
| The start or a key command stops with an error that names the pending key file | Something at that name is no regular file, or the user cannot read it. A run of `rotate-key` as another user leaves a pending key that the user of the host cannot read. Or the key path is a symbolic link to the pending key file | Make a file that can be a pending key readable for the user of the host, or run `flats secret rotate-key` as the user that wrote it. Such a file can hold the key of the values. Move away anything else at that name, such as a folder or a link. When the key path is a link to the pending key file, put a real copy of that file at the key path in place of the link, and run `flats secret rotate-key` |

On a host without a sealed value, a stop while the command writes the key file
can leave a damaged key file. No value is sealed, so nothing is lost. The start
and the command refuse the file and say to move it away.

#### Limits of a rotation

- Run one key command at a time. `flats secret backup-key` takes no host lock,
  so it can run during a rotation. The data directory can then end with a
  recovery copy of the old key beside the ID of the new key, and the start does
  not report it.
- On Windows, keep the key and the `--out` file on an NTFS volume during a
  rotation. On another file system (ReFS, FAT, exFAT, a network share) Flats
  cannot make each step durable before the next one.
- The pending key lies beside the key path that the configuration names. A
  change of `host.secret_key_path` while a rotation waits for its finish hides
  the pending key, and the start then refuses for a key that sealed none of the
  values. Finish the rotation before you change the setting.
- A `flats.db` from before this release has no key ID on its values. A value
  gets its ID when it is sealed the next time, at a rotation or when you set it
  again. The start refuses a valid key that sealed none of the values only when
  every value carries an ID. That check does not apply to such a database until
  a rotation, or a new set of every value, gave each value an ID. Until then a
  wrong key shows when a value is used, as before.

### Audit log

Every change of a sealed value, every key backup, key restore, key rotation and reset, every change of the visibility
of a flat that an approval applies, and every change of the origin grants or of a provider permission of a flat that
the console or `flats network set` makes writes one event to the audit log in
`flats.db` (the table `audit_events`, schema version 25). It is separate from the log of a flat, which Flats
trims to `system.events_keep` events and removes with the flat. The audit log is append-only, is never trimmed,
and the deletion or rename of a flat does not change it. The grants that a new flat starts with, and the Tailscale grant
that the upgrade of a legacy service adds, write no event. Agents cannot read it: no MCP tool and no API route
returns it, and `get_logs` shows only the log of the flat. The value of a secret or key is never recorded, only
its name.

| Kind | Written when | `data` |
| --- | --- | --- |
| `secret.set` | a flat secret is stored or replaced | `created`: true for a new name |
| `secret.delete` | a secret is deleted, by `flats secret reset`, or with its flat | `approval`: the approval that deleted the flat, when there is one |
| `key.set` | an operator key value is stored or replaced | `replaced`; `from` (`flag`, `env` or `config`) for a start input or a migration |
| `key.delete` | an operator key value is removed, or a waiting zrok token is used up | `reason`: `removed` or `connected` |
| `key.backup` | `flats secret backup-key` wrote the recovery copy, or `flats secret rotate-key` wrote it again for the new key | `key_id`, and `out` when `--out` wrote a second copy |
| `key.restore` | `flats secret restore-key` wrote the key | `key_id`, `out`, and how many sealed values the key `opened` of the `total` |
| `key.reset` | `flats secret reset` deleted the stored values | how many `secrets` and `keys`; each value also has its own delete event |
| `key.rotate` | `flats secret rotate-key` sealed every stored value again under a new key | `from` and `to`, the IDs of the old and the new key; how many `secrets` and `keys` it sealed again; `out` when the command ran with `--out` |
| `visibility.set` | an approval applied a change of the visibility or of the public access level of a flat | `from` and `to`, each with `visibility` and `access`, and the `approval` |
| `network.set` | the list of origins that the server code of a flat may fetch changed | `count` and `origins`: the list after the change. A list whose JSON text has more than 3000 bytes is cut to its first origins, with `truncated` |
| `provider.set` | the permission of a flat to use a network provider changed (the provider is the name of the event) | `permitted` |

Each event has a time, the slug and the ID of the flat when it concerns one, the name of the secret or key, the
channel (`via`) and the actor. The channel is `console`, `cli` (`flats secret set` or `rm`, or `flats network set`, through the loopback
API), `command` (a command that you ran on the host), `start` (an input that the host read at start, or the zrok
enrolment after it), `migration` (the import of a legacy key), `flat-delete`, or `autopilot` (an agent decided the approval of a visibility
change under [Autopilot](#autopilot-settings); the actor is then the Autopilot record of that agent). The actor is the identity that the
channel knows. For the console it is the text that the console records for approvals: a tailnet user or node, a
NetBird peer, the external origin, or the address of the caller. For a command it is the name of the operating-system
user, which is `root` under `sudo`. For `cli`, `start`, `migration` and `flat-delete` it is empty: the CLI header is
a claim of the client and not an identity. The log holds these names for as long as the host keeps `flats.db`, so
treat it as you treat the approval records.

```sh
flats audit ls [--since TIME] [--kind KIND] [--flat SLUG] [--flat-id ID] [--limit N] [--json] [--config PATH | --data DIR]
```

`flats audit ls` lists the events oldest first, the newest 100 by default (`--limit 0` lists all). `--since` takes
an RFC 3339 time or a duration such as `24h`. `--flat SLUG` follows a flat through its renames, because it selects by
the flat's ID; when no flat has that slug any more, it lists the events that were written under it, which is how you
find a deleted flat. `--flat-id` selects one flat by its ID, which `--json` prints with every event. `--json` prints one JSON object per
line. The command reads `flats.db` without a host lock and never calls the host, so it works while the host runs
and while it is stopped. It writes no row. A stopped host that crashed and left a `-wal` file makes the read create an
empty `-shm` file beside it, which needs a writable folder. The command refuses, with status 78, a data directory
without `flats.db` and a database of another schema version, and says whether to start the host once so that it migrates
or to use the release that wrote it.

`flats secret backup-key` writes its event with a write of its own to `flats.db`, without the host lock, so run it as
the user that runs the host, as you run `flats key`, `restore-key`, `reset` and `rotate-key`. When it cannot record the event, it
exits with status 1 and says that the recovery copy was written and the event was not.

A [key rotation](#rotating-the-key) writes one `key.rotate` event for all the values that it seals again, in the
transaction of the re-seal, also when `flats.db` holds no sealed value. It writes no `secret.set` and no `key.set`,
because no value changes. The event records the re-seal in `flats.db`, after which the old key opens nothing there. It
does not record when the new key became the key of the host. A run that finishes a rotation that stopped writes no
second event, and the step in which you put the file of `--out` at the key path writes none. After a rotation with
`--out`, or after a stop before the key file was written on a host without a sealed value, the log can hold a `to` key
that never became the key of the host. When the command wrote the recovery copy again, it also appends a `key.backup`
event with the ID of the new key. When it cannot record that event, the copy stays and the error says so. The event
webhook sends nothing for these two kinds.

A restore of the host from `flats backup` replaces `flats.db`, so it drops the audit events that were written after
the copy. Anyone who can write `flats.db` can change the log. To keep a record off the host, run
[`flats audit export`](#audit-export) from time to time.

The audit log is a schema change: an older Flats refuses a `flats.db` of schema 25. Schema 26 added the two webhook keys and the
flat ID of an approval, and an older Flats refuses it too. Schema 27 added the key ID of each sealed value
([Rotating the key](#rotating-the-key)), and an older Flats refuses it too. To go back to an older release,
restore a backup of the whole data directory that you took before the upgrade, as [flats.db](#flatsdb) says.

### Audit export

```sh
flats audit export [--since TIME] [--config PATH | --data DIR]
```

`flats audit export` writes the audit log and the approval records as JSON lines, oldest first, so that you can ship
them off the host. It prints nothing else to standard output. It reads `flats.db` as `flats audit ls` does: without a
host lock, without calling the host, while the host runs or is stopped, and with the same refusals and exit statuses.
It writes no row.

There are three kinds of line. Each has `record` and `at`, an RFC 3339 time in UTC with milliseconds.

```json
{"record":"approval","event":"created","at":"2026-11-16T09:29:40.002Z","id":"apr-3765ym6bd21w","flat":"notes","flat_id":"754f…","action":"delete","via":"mcp","reason":"the project ended"}
{"record":"audit","id":12,"at":"2026-11-16T09:30:00.118Z","kind":"secret.delete","flat":"notes","flat_id":"754f…","name":"API_KEY","via":"flat-delete","actor":"","data":{"approval":"apr-3765ym6bd21w"}}
{"record":"approval","event":"decided","at":"2026-11-16T09:30:00.120Z","id":"apr-3765ym6bd21w","flat":"notes","flat_id":"754f…","action":"delete","state":"approved","decided_by":"ann@example.org","result":"flat notes was permanently deleted"}
```

- An `audit` line is one event of the [audit log](#audit-log), with the fields of `flats audit ls --json`.
- A `created` line is written for every approval, at the time of the request.
- A `decided` line is written for every approval that is `approved`, `rejected` or `failed`, at the time of the decision.

An approval record stays when its flat is deleted, so the decision of an approved delete is in the export after the flat
is gone, together with the `secret.delete` events of the secrets that the flat held. A delete from the console makes no
approval. Such a flat shows only through those `secret.delete` events and through the pending approvals that its
deletion failed. `flat_id` joins the lines of one flat: a rename changes the slug of its approval lines and not the slug
of its older audit lines, and a flat that is created again under a deleted slug has another ID. `flat_id` is empty for an
approval that was made before schema 26 and whose flat was gone at the upgrade.

`decided_by` is the stored record of who decided: `console`, a tailnet login, a NetBird peer, or the Autopilot record of
an agent with its address. `reason` is text that an agent wrote, and `result` can be an error text of the host with a path
in it. Flats does not filter them. It cuts `reason` and `result` at 1 KiB and `decided_by` at 512 bytes. No line holds the
parameters of an approval or a secret value. Treat the export as you treat the approval records.

`--since` takes an RFC 3339 time or a duration such as `24h` and keeps the lines whose `at` is that time or later.
The command stops at a cutoff, 30 seconds before it started, because a record that the host is writing can still be on
its way to the disk. It prints the cutoff on standard error:

```text
exported 1820 lines through 2026-11-16T09:31:10.456Z
```

Run the next export with `--since` and that time. The line at the boundary can appear in both exports, so a reader
ignores a second line with the same `record`, `event` and `id`. A write of the host that takes more than 30 seconds
between its time stamp and its commit can be missed. To be safe against that, give `--since` a time a few minutes before
the printed one and ignore the repeated lines. Lines are ordered by `at`. Lines with the same `at` come as audit
lines by `id`, then `created` lines, then `decided` lines.

Anyone who can write `flats.db` can change what the export shows. A restore of the host from `flats backup` drops the
records that were written after the copy.

### Event webhook

Flats can send a signed HTTP POST to one URL when an approval is created or decided, when the visibility of a flat
changes, and when a grant of a flat's network access changes. The webhook is off until you store its two
[operator keys](#operator-keys) while the host is stopped:

```sh
umask 077
openssl rand -hex 32 > webhook-secret.txt      # the receiver needs this value too
flats key set webhook-secret --from-file webhook-secret.txt
flats key set webhook-url --from-file webhook-url.txt   # a file that holds only the URL
```

Flats never shows a stored value again, so keep the secret for the receiver, and delete the two files when both sides
have it. A value on a command line can stay in the shell history, which is why the URL comes from a file or from
standard input. The next start sends events. `flats key rm webhook-url` or `flats key rm webhook-secret` turns the webhook off, and a
host that has only one of the two keys logs that at its start and sends nothing. There is no console page for the
webhook, and no MCP tool, API route or log of a flat shows the URL, the secret or that a webhook is on. Both values are
sealed in `flats.db` like the other operator keys, because a webhook URL often holds a token.

The URL uses `https`. `http` is accepted only for `localhost` and for an IP address in the loopback, private,
link-local or CGNAT ranges, for a receiver on the same machine or network. It has no fragment and at most 2048 bytes.
The secret has at least 16 bytes.

| Event | Sent when |
| --- | --- |
| `approval.created` | an approval is requested, from the console or by an agent. A request that repeats an open one sends nothing |
| `approval.decided` | an approval is approved, rejected or failed, also when the deletion of its flat fails it |
| `visibility.changed` | an approved change of the visibility or of the public access level of a flat is applied |
| `network.changed` | the provider permission of a flat, or the list of origins that its server code may fetch, changes |

A change that alters nothing sends nothing. The host-wide `network.permitted` list, the creation of a flat, the public
instance mode, the downgrade of "anyone can edit" when guest editing is off, an approval that a restart finishes, and
secrets and operator keys send no event. The [audit log](#audit-log) records the changes of secrets and operator keys.

The body is a JSON object of at most 4 KiB:

```json
{
  "id": "evt_mfrggzdfmztwq2lknnwg23tpob",
  "type": "approval.decided",
  "at": "2026-11-16T09:30:00.120Z",
  "flat": {"slug": "notes", "id": "754f4ee971ef128d6ce458c59c730fbb"},
  "operation": "set_visibility",
  "state": "approved",
  "approval_id": "apr-3765ym6bd21w",
  "decided_by": "operator",
  "console_url": "https://console.example.ts.net/approvals/apr-3765ym6bd21w"
}
```

| Field | Content |
| --- | --- |
| `id` | the ID of the event, the same for every attempt to deliver it |
| `type`, `at` | the event and its time on the clock of the host |
| `flat` | the slug and the ID of the flat. The ID is empty when Flats does not know it |
| `operation` | the action of the approval, or `set_visibility`, `set_network_origins` or `set_provider_permission` |
| `state` | `pending`, then `approved`, `rejected` or `failed`; `applied` for a visibility or grant change |
| `approval_id` | the approval, when there is one |
| `via` | for `approval.created`, the channel that asked (`mcp`, `cli`, `api` or `console`) |
| `decided_by` | `operator` or `autopilot`. Absent for an approval that nobody decided |
| `change` | for `visibility.changed`, `from` and `to`, each with `visibility` and `access`. For `network.changed`, `{"grant":"origins","count":2,"origins":[…]}` or `{"grant":"provider","provider":…,"permitted":true}`. A list of origins whose JSON text has more than 3000 bytes is cut to its first origins, with `"truncated":true`; `count` is the size of the whole list |
| `console_url` | the page of the approval or of the flat in the console |

The body never holds the content of a flat, a secret or its name, the reason, parameters or result of an approval, a
login or an address. The export carries the reason, the result and the stored record of who decided.

Each request has these headers:

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `flats/<version>` |
| `X-Flats-Event` | the type |
| `X-Flats-Event-Id` | the `id` of the body |
| `X-Flats-Timestamp` | the Unix time in seconds of this attempt |
| `X-Flats-Signature` | `v1=` and the HMAC-SHA256, in lower-case hex, of the timestamp, a `.` and the exact body, with the secret as the key |

A receiver checks a request in this order. It computes the signature from the header timestamp and the bytes it
received, and compares it with the header in constant time. It refuses a timestamp that is more than five minutes from
its own clock. It ignores an event whose `X-Flats-Event-Id` it has handled already. With the body in `body.json`:

```sh
printf '%s.' "$TIMESTAMP" | cat - body.json | openssl dgst -sha256 -hmac "$SECRET" -hex
```

`body.json` must hold the exact bytes of the request body, with no added line end. `openssl` prints a label and then the
hex digits. The digits must equal the header value after `v1=`.

For the secret `whsec_0123456789abcdef0123456789abcdef`, the timestamp `1700000000` and the body
`{"id":"evt_test","type":"approval.created"}`, the signature is
`v1=edecf23a0b88e1b94a9e5c0e5534118dcd61eeaf578b04fa0696a8a3f9b79919`.

A delivery succeeds on a status 200 to 299. Flats does not follow a redirect and always verifies the certificate of an
`https` receiver. It connects to the receiver itself and ignores `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`, so a host
that reaches the network only through a proxy cannot deliver the webhook. An attempt has 10 seconds. Flats tries an event at most 4 times, and waits 5 seconds, 30 seconds and
2 minutes between the attempts, after a network or certificate error, a timeout, and a status 408, 429 or 5xx. Any other
status, and a redirect, end the event at once. A failed event leaves one line in the host log with the ID of the event,
its type and a class (`connect`, `timeout`, `tls`, `status NNN` or `other`), never the URL or the secret.

A delivery never delays the operation that caused it. Flats keeps at most 256 events that wait and runs 4 deliveries at
once, so a receiver can see events in another order than they happened, can see an event twice, and can miss one: an
event that waits when the host stops, that used its 4 attempts, or that a burst of newer events pushed out of the queue
(an agent that requests many approvals can cause that) is not sent again. The webhook is a notification. The record is
the audit log and the approvals, which [`flats audit export`](#audit-export) ships.

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
