# Operator keys and console domains: technical design

Technical design of [console-settings.md](console-settings.md). "Rule N"
refers to that spec's numbered rules.

## Keys

### Storage

A new `flats.db` migration (user_version 12) adds:

```sql
CREATE TABLE host_keys (
  id TEXT PRIMARY KEY CHECK (id IN ('tailscale', 'zrok', 'netbird')),
  nonce BLOB,
  ciphertext BLOB,
  source TEXT NOT NULL CHECK (source IN ('console', 'cli', 'flag', 'env', 'config')),
  updated_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  CHECK ((nonce IS NULL) = (ciphertext IS NULL))
);
```

- `tailscale`: the auth key in use; the value is always present.
- `zrok`, `netbird`: the record of the account or peer connection. While an
  enrollment waits (written by the offline CLI), the value is present; once
  enrollment succeeds the value is cleared and the row keeps only `source`
  and `updated_at` (rule 9). The NetBird setup key is thus kept only until
  the peer is enrolled; the zrok token keeps living in `zrok-environment/`
  after connecting (rule 2). Disconnecting deletes the row. A connection made
  before this feature has no row and shows source "unknown".
- `last_error` holds the last failed enrollment, for the console.

Values are sealed with AES-256-GCM under `secret.key`, the key flat secrets
use, with the additional data `"host-key\x00" + id`, so a value cannot be
moved to another id. The code sits beside the flat secret code in
`internal/core` as functions over a `*store.Store` and the data directory,
because the Tailscale key is needed before `core.Service` exists.

`core.CheckSecretKey` also counts `host_keys` rows that hold a value: a
missing `secret.key` with such rows stops the start (status 78) with two
fixes: restore `secret.key`, or remove the stored keys with `flats key rm`,
which needs no `secret.key`.

### Inputs at start

`app` resolves the keys once, before any provider backend is built:

| Priority | Tailscale | zrok | NetBird |
|---|---|---|---|
| flag (a file path) | `--tailscale-authkey-file` (legacy alias `--authkey-file`) | `--zrok-token-file` | `--netbird-setup-key-file` |
| environment | `FLATS_TAILSCALE_AUTHKEY`, `FLATS_TAILSCALE_AUTHKEY_FILE`; legacy mode also `TS_AUTHKEY`, `TS_AUTH_KEY` | `FLATS_ZROK_TOKEN`, `FLATS_ZROK_TOKEN_FILE` | `FLATS_NETBIRD_SETUP_KEY`, `FLATS_NETBIRD_SETUP_KEY_FILE` |
| config.json (deprecated) | `credentials.tailscale_authkey_file` | — | — |
| database | `host_keys` | `host_keys` (waiting enrollment) | `host_keys` (waiting enrollment) |

- Setting both the value and the `_FILE` variable of one key is refused
  (status 78).
- Every variable in the table, including the legacy `TS_AUTHKEY` and
  `TS_AUTH_KEY`, is read and then removed from the process environment
  before any backend or child process starts, whether or not a
  higher-priority input wins. The embedded Tailscale client falls back to
  `TS_AUTHKEY`/`TS_AUTH_KEY` when a node has no key, so a key removed in the
  console would otherwise come back from the environment.
- Config mode keeps refusing the remaining `TS_*` variables
  (`TS_CLIENT_SECRET`, `TS_CLIENT_ID`, `TS_ID_TOKEN`, `TS_CONTROL_URL`) when
  Tailscale is permitted; legacy mode is unchanged for those.
- A file input is read whole and trimmed of surrounding white space. A
  missing or unreadable file, or an empty value, stops the start (status 78).
- The new flags are added to the config-mode flag allow-list.

Tailscale (rules 4–6). Every external input present is read and checked
(rule 11) first, so an unreadable deprecated file still stops the start even
when a flag wins; then:

1. Pick the highest-priority external value, if any.
2. No stored key: store it with its source, and use it.
3. A stored key equal to it (constant-time compare): use it. A different
   one: use the stored key, log a warning, and report `external_differs` to
   the console. Nothing about the external value is stored.
4. No external value: use the stored key, or none.

The deprecated `credentials.tailscale_authkey_file` (both startup modes):
`flats config set` refuses the key; a start that finds it treats its file as
the lowest-priority external value above, and after the database step has
committed removes the key from `config.json` through the config writer
(keeping history). A crash between the two steps repeats the same idempotent
import at the next start. A legacy service's `--authkey-file` is handled as
the flag at start. The legacy migration (also run by `flats install`) still
records that path as the deprecated key without reading the file
(`Document.SetDeprecated`), so the first start imports it, and the per-start
flag comparison no longer checks that flag.

zrok and NetBird (rule 7): the highest-priority value (flag, environment,
then a waiting row) is used only when no account or peer is connected. The
host starts normally and connects in the background afterwards through the
existing `ConnectZrok` and `ConnectNetbird` paths. Success clears the value
and records the source and time; failure keeps a waiting row (an external
value is not stored) and records `last_error`; the next start retries. When
`zrok.environment` names an outside environment, or the binary has no
NetBird, the value is ignored with a warning. Console connections write the
row with source `console`.

### Using a changed Tailscale key

`tsnet.Net` gets `SetAuthKey(string)`; a node reads the current key when it
starts (`ts.Server.AuthKey`) and when deciding whether it needs an
interactive login. Registering or removing the key in the console takes
effect for nodes started afterwards. Logged-in nodes keep their identity.

### Check on entry (rule 11)

A Tailscale key must start with `tskey-auth-`; `tskey-client-` (OAuth client
secrets) and `tskey-api-` are refused with an explanation. The console's help
says the key must be reusable and, so that console decisions record who made
them, should not be tagged.

### Console API

Routes behind `consoleRequest`, so cross-site pages and clients sending
`X-Flats-Client` (the CLI, MCP relay) are refused. No MCP tool or `/api`
route exposes keys. This is CSRF protection, not authentication (rule 10).

- `GET /console/api/keys`: per key `id`, `set`, `waiting`, `source`,
  `updated_at`, `external_differs`, `last_error`, and identity
  (`environment_id`, `device_name`).
- `PUT /console/api/keys/tailscale` `{"key": "..."}`,
  `DELETE /console/api/keys/tailscale`.
- zrok and NetBird keep `PUT/DELETE /console/api/providers/{zrok,netbird}/account`;
  they now also write or delete the row.

### Offline CLI

`flats key ls | set <id> | rm <id> [--config path | --data dir]` runs like
`flats config set`: it takes the data lock (refusing while a host runs),
checks the database binding, and opens the store.

- `set` reads the value from standard input; on a terminal it prompts without
  echo. `--from-file PATH` reads a file instead. It checks the value (rule
  11) and writes the row with source `cli`, replacing an existing one. For
  `zrok` and `netbird` this is a waiting enrollment for the next start.
- `rm` deletes the row and needs no `secret.key`. For zrok and NetBird it
  removes a waiting enrollment only; disconnecting stays in the console.
- `ls` prints id, state, source and time, never values.

## Console domains

### Configuration (schema_version 2)

- New key `tailscale.console_hosts`: a list, default `[]`, kept in order,
  each item a lowercase DNS label (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`),
  no duplicates. A non-empty list needs `tailscale` in `network.permitted`.
- `host.console_host` is removed. Migration 1→2: when the effective
  `network.private_backend` is `tailscale` and the effective
  `host.console_host` (default `flats`) is already a valid label as written,
  the list is `["name"]`; otherwise the list is empty (such a name never
  served a console: tsnet refused it). No name is rewritten, so the migration
  never opens a console that was not open; it needs no confirmation.
- `--console-host NAME` stays a per-run override and means
  `tailscale.console_hosts = [NAME]` for that run. While it is given, the
  console cannot change the list (a pin, like `--relays`).
- `flats config init --console-host` is removed; add domains after init with
  `flats config set tailscale.console_hosts NAME` once Tailscale is permitted.
- Legacy services: `--network tailscale --console-host X` migrates to
  `["X"]`; with `--network local` the list is empty. The per-start flag
  comparison treats `--console-host X` as matching a list `["X"]`, or an empty
  list while `private_backend` is not `tailscale`, and a legacy service that
  passes `--console-host` pins the list in the console so the next start's
  comparison keeps passing.

### Ownership record

A second table in the same migration records which tailnet nodes are console
nodes, so nodes removed from the list are retired even when the list changed
offline:

```sql
CREATE TABLE console_nodes (
  name TEXT PRIMARY KEY,
  retiring INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);
```

A row is written before a console node is first served, marked `retiring`
when the name leaves the list, and deleted once `tsNet.Stop` succeeds.

### Reservation

Core's reserved names gain a function (`Config.ReservedNames`): the effective
console list plus every `console_nodes` row, and core adds names whose
addition is in progress. The fixed name `flats` stays reserved for flats but
may be added as a console domain.
`core.ReserveConsoleName(ctx, name, commit)` runs under the per-slug
operation lock flat creation and rename use: it refuses a name held by a
flat, a preview, a redirect or the reserved set, adds it to the in-progress
set, runs `commit`, and drops it from that set afterwards (the committed list
or the `console_nodes` row then holds it). Preview host generation retries a
random name that is reserved.

### Start

After core restores flat routes and before the console nodes are served:

1. For each `console_nodes` row not in the effective list: mark it retiring
   and call `tsNet.Stop` (in the background; failures are recorded and
   retried at the next start). Without a Tailscale backend the rows wait.
2. For each listed name: if a flat, preview or redirect holds it, it is not
   served and the console shows the conflict; otherwise write its row and
   serve it with `tsNet.Serve(name, handler, false)`, whatever
   `private_backend` is (rule 13).

Each node's handler is `HostGuard` (the node's configured, MagicDNS and short
names) → a middleware that records the console domain in the request context
→ `TailnetIdentity` → the management mux.

### Adding (rule 14)

Under one console-domain mutex:

1. Check the ETag and the pin, validate the label, refuse duplicates, and
   refuse when Tailscale is not on.
2. `ReserveConsoleName` with a commit that writes the `console_nodes` row,
   calls `tsNet.Serve` (a synchronous error, such as an unfinished retirement
   of the same name, ends here and deletes the row), and saves the list
   through the settings source. If the save fails, the node is stopped; if
   stopping fails too, the row stays as retiring so the name stays reserved
   until a later retirement succeeds.

### Deleting (rule 15)

1. Check the ETag and the pin. Refuse the last name when the request came
   through a console domain; the loopback console may empty the list.
2. Save the list without the name. Nothing has changed if this fails.
3. Mark the row retiring and call `tsNet.Stop`. On success delete the row; on
   failure keep it, record the error and return it; a later delete of the same
   name, or the next start, retries.
4. When the request came through the name being deleted, the response
   (with the URL of a remaining name) is sent before step 3, which then runs
   in the background, because stopping the node closes the connection that
   carries the request.

Turning the Tailscale provider off is refused while the list is not empty
(the config rule refuses it). The provider is shown locked while the console
runs on it.

`GET /console/api/console-domains` returns each name with URL, node state
(from `tsNet.Status()`), conflict and retirement error; the domain the
request came through; the pin; whether the request came over loopback; and
the number of pending approvals. `POST /console/api/console-domains`
`{"name": ...}` adds, `DELETE /console/api/console-domains/{name}` deletes;
both take the settings ETag.

### Links (rule 16)

`core.Config` gains `ConsoleURLFor func(context.Context) string` next to
`ConsoleURL`, and `ApprovalURL` takes the request context. The host returns,
in order: the URL of the console domain recorded in the context if
it is still listed and served; the URL of the first listed, served name; the
loopback console address. MCP requests carry the same context, so approval
links, their event messages, and status `console_url` / `mcp_url` follow the
domain an agent connected to. Status adds `console_urls`. The startup log
prints every console URL.

## Console UI

- A **Keys** card lists the three keys with the same row layout: state,
  source and time, identity, warnings (external value differs, enrollment
  error), and the set/replace and remove (or disconnect) actions. The zrok and
  NetBird forms move there; their provider panels keep the connection facts
  and point to Keys.
- A **Console domains** card lists the Tailscale names with URL, state,
  conflicts and retirement errors, an add form, and delete buttons with the
  confirmations of rule 15. Deleting the current domain navigates to the URL
  the response names.

## Documentation

`docs/configuration.md` (schema 2, key list, storage table, inputs),
`docs/networking.md`, `docs/container.md` (secret file as flag or `_FILE`
variable; seeding), and the spec's current state.

## Tests

- config: migration 1→2 for every combination of backend and console host
  (valid, uppercase, invalid), `testdata/config/v2/`, list validation,
  override, deprecated key refusal.
- store: migration 12 and the table constraints.
- core: seal/open round trip, wrong id refused, `CheckSecretKey` with host
  keys only, reservation against flats, previews, redirects and additions in
  progress.
- app: the input table (each priority, differing value warning, both env
  forms refused, every listed variable removed after reading, deprecated
  config key imported and then removed, unreadable deprecated file,
  legacy `--authkey-file` and `TS_AUTHKEY`), offline `flats key`, console
  domain add/delete/last/pin/ETag/self-delete/conflict at start/offline
  removal retired at start, links per request context.
- tsnet: `SetAuthKey` used by a node started after it.
- api: the key and domain routes refuse agent requests.
- console UI tests for both cards.
- A live check on a dev host with a real tailnet: add a second console
  domain, open it, delete the first, register a key from the console and see
  a new node log in with it.
