# Configuration and storage

This page says which data Flats stores where, how `config.json` is versioned,
and how an existing installation moves to it.

## Where data lives

The question "who decides this value?" picks the place:

| Decided by | Stored in | Examples |
| --- | --- | --- |
| The operator | `config.json` | addresses, system policies, permitted networks, console domains, provider settings |
| Flats while running | `flats.db` | flats, drafts, versions, deployments, document and design revisions, approvals, per-flat provider permission, events, analytics, ordinary app environment variables, encrypted secrets, encrypted operator keys, console node records |
| The flat | `flats/<slug>/` | uploaded code, revision contents, the flat's SQLite database and FILES |
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
  secret.key                  key for flat secrets and operator keys, 32 bytes, mode 0600
  backups/                    flats.db copies taken before a schema migration
                              (newest 3) and the archived network-provider.json
  runtime/docs/               generated read-only docs app modules (recreated as needed)
  flats/<slug>/
    draft-revs/ versions/     saved Draft revisions and versions
    data/db.sqlite data/files/  the flat's own data
    snapshots/                DB+FILES snapshots taken before a version goes
                              live and before a data restore (not for documents)
    previews/                 preview data, paired with preview rows in flats.db
    revisions/                a document's revision contents (blobs/, by SHA-256)
    design/                   a design's revision contents (blobs/, by SHA-256)
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

Required:

- `config.json`;
- `flats.db`, copied with `VACUUM INTO` or while the host is stopped, never
  as a plain copy of a live database, together with `secret.key` from the
  same moment: the flat secrets and operator keys in `flats.db` need it;
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
Stopping the host and copying the whole data directory covers all of it.

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
| `system.revision_quota` | `100` unpinned revisions per document or design, `0` = keep all | immediately; lowering it deletes the oldest unpinned revisions at once |
| `system.pin_quota` | `20` pinned revisions per document or design, `0` = unlimited | immediately; existing pins are kept and new pins are refused while full |
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
| `netbird.console_hosts` | `[]`; lowercase DNS labels, each its own NetBird peer serving the console over HTTP; needs `netbird` in `network.permitted` | immediately when changed from the console (Settings → Console); otherwise restart |
| `console.preferred_host` | `""` (no preference); `local`, `tailscale:NAME` or `netbird:NAME` with NAME in that list | immediately when changed from the console (Settings → Console); otherwise restart |
| `credentials.operator_file` | none; ignored (the operator credential is no longer used) | — |
| `credentials.tailscale_authkey_file` | deprecated: the next start stores the key in `flats.db` and removes this key; `flats config set` refuses it | next start |

Addresses take any `host:port` that the old `--listen` and `--local-addr`
flags took, and the host logs a warning when the management address is not
loopback. The integer system settings take values up to 2^53−1, so a
JavaScript client reads them exactly.

Only providers listed in `network.permitted` start. Permitting `tailscale`
does not permit Funnel, and Local needs no entry. `private_backend:
"tailscale"` requires `tailscale` in `permitted`. While `default_private` is
empty, it also makes every new flat allowed on Tailscale when it is created
(the operator can turn that off per flat); a non-empty `default_private`
decides instead. Flats that already exist keep their providers.

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
flat or changes visibility. The console's Settings → Networks turns a
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

zrok keeps the connected account's token and identity in `zrok-environment/`
(mode 0600), never in `config.json`, unless `zrok.environment` names an
environment managed with the zrok CLI; `zrok/` records every name the host
reserved in `zrok.namespace`. `cloudflare-quick` has no settings and no
credentials to enter; its per-flat tunnel credentials in `cloudflare-quick/`
are deleted with the flat. How both providers serve flats is in
[Publishing through zrok](networking.md#publishing-through-zrok) and
[Publishing through a Cloudflare quick tunnel](networking.md#publishing-through-a-cloudflare-quick-tunnel).

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
  with `flats config set` or a text editor is retired at the next start. A
  retirement that fails keeps the name reserved; the console lists it and
  retries on request, and the next start retries it.
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
    next start, which connects it when no account is connected.
  - `flats key ls` lists the keys with their state, source and time, never
    their values.
  - `flats key rm` removes a stored key or a waiting zrok token and needs no
    `secret.key`. Disconnecting a zrok account stays in the console.
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
start instead of creating a new key (see [Starting](#starting)).

No MCP tool and no agent API route reads or sets a key. The console's routes
are protected against cross-site requests, not authenticated: a program that
can reach the management listener could imitate the console, as it can for
approvals.

### Editing

- **Console**: the system and Portal keys, `network.permitted`,
  `network.default_private` and `network.default_public` through Networks,
  and `tailscale.console_hosts` and `netbird.console_hosts` through Console.
  A save
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

An older `config.json` is converted at start. The conversions that change
what an operator sees:

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
or any of `flats/`, `secret.key`, `tsnet/`, `portal/`, `zrok/`,
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
