# HTTP API, CLI, MCP and console

The management server serves these surfaces from one mux:

| Path | Surface | `via` recorded in events and approvals |
| --- | --- | --- |
| `/api/` | HTTP API for agents and the CLI | `api`, or `cli` when the request sends `X-Flats-Client: cli` |
| `/mcp` | MCP (Streamable HTTP, stateless) | `mcp` |
| `/console/api/` | the web console's API | `console` |
| `/llms.txt`, `/llms-full.txt`, `/docs/…` | agent discovery documents ([Connecting agents](agents.md#llmstxt)) | none |
| `/metrics` | host metrics in the Prometheus text format ([Metrics](design.md#metrics)) | none |

Everything else under `/` is the console page itself. Core behavior behind
these surfaces (Draft, versions, frozen approvals, result DTOs, provider IDs)
is in the [lifecycle core contract](lifecycle-core-contract.md); who can
decide approvals, and why, is in [Approvals](design.md#approvals).

## Request guards

Every request to the management server must name it in `Host`, and any
`Origin` must be one of those names (see [Approvals](design.md#approvals)).
The management listener also accepts operator-declared
[`console.external_origins`](configuration.md#external-origins-behind-a-reverse-proxy).
For these requests, any Origin must match the declared scheme and the request
hostname/effective port exactly; even another declared origin is refused.
Forwarded headers do not influence admission or link generation.
On top of that:

- **`/api`** refuses browser requests with `Sec-Fetch-Site: cross-site` or
  `same-site`, or an `Origin` other than the requested host (403). Requests
  other than GET/HEAD/OPTIONS must send `Content-Type: application/json`, an
  archive type (`application/gzip`, `application/x-tar`, `application/zip`,
  `application/octet-stream` and their `x-` variants) or an `X-Flats-Client`
  header (415 otherwise). None of these is CORS-safelisted, so a web page
  cannot send them without a preflight, which Flats never grants.
- **`/console/api`** requires `X-Flats-Console: 1`, refuses any
  `X-Flats-Client` header (the CLI and the MCP relay send one), and needs a
  same-origin `Origin` when one is present. Mutations also need
  `Sec-Fetch-Site: same-origin` and an `Origin` header.
- **`/metrics`** answers `GET` and `HEAD`; other methods are 405 with
  `Allow: GET, HEAD`. It
  applies the `/api` browser rule: `Sec-Fetch-Site: cross-site` or
  `same-site`, or an `Origin` other than the requested host, is 403. A
  scraper sends neither header and needs no credential. It is never served on
  a flat's route, a preview or a public provider.
- **NetBird console domains and declared HTTP external origins** are plain HTTP, where browsers send no Fetch
  Metadata. Only their handlers mark requests as plain-HTTP console requests;
  on those, a mutation without `Sec-Fetch-Site` is accepted when `Origin` is
  present, uses `http`, and matches the requested host and port (a missing port
  is 80). A present `Sec-Fetch-Site` must still be `same-origin`. The custom
  header still needs a preflight from any other origin, including another flat
  on the same NetBird domain. The loopback listener and Tailscale console
  domains keep the strict rule.

These checks are CSRF protection, not authentication: a local process that
sends the same headers to the loopback listener can act as the console,
including deciding approvals. A separate operator credential is an open design
item. Likewise whoever can reach the console can read `/metrics`, which shows
what no other route does (CPU, memory and response times, and the slugs of
Private and unpublished flats): trust a system that scrapes it like someone
with console access.

The approver recorded with a decision is the tailnet login on a Tailscale
console domain, `netbird:<peer FQDN>` on a NetBird console domain, and none on
the loopback listener (the audit actor is then `console`).

## Errors and status codes

Errors are JSON: `{"error", "category", "problems": [{"path","message","fix"}], "health"}`.
`category` is the typed cause; guide topic `refusal.<category>` explains each.

| Status | Meaning |
| --- | --- |
| 202 | A pending approval was created; the body carries `status: "pending_approval"`, the approval and `approval_url` |
| 400 | Invalid input, including malformed or repeated `expected_revision` |
| 403 | Not allowed from this surface, or a cross-site browser request |
| 404 | Not found, or the host does not manage that feature |
| 409 | Conflict: stale Draft revision, unpublished flat made Public, not deployed, unavailable, stale approval, provider not permitted or not ready, unconfirmed public stop, unchanged content, provider in use |
| 412 | `config_changed`: `config.json` changed since the host read it, or the console page is stale |
| 415 | A mutating `/api` request without a JSON or archive type or `X-Flats-Client` |
| 422 | Bundle validation, a failed health check or runtime start, or a board write or import over the disk quota (`board_capacity`) |
| 500 | Server fault |

A failed decision (`POST approvals/{id}/approve`) answers with the error body
plus the persisted `approval`, whose `result_data` carries the typed
`failure_code` (see [Data and result DTOs](lifecycle-core-contract.md#data-and-result-dtos)).

## HTTP routes

Paths are relative to `/api` or `/console/api`. **Both** means the route is
mounted under both prefixes; **console** routes exist only under
`/console/api`; **api** routes only under `/api`.

| Method | Path | Mounted | Purpose |
| --- | --- | --- | --- |
| GET | `/status` | both | `{ok, system, upload_max_bytes}`: host, networks, limits, build version |
| GET | `/client` | both | Client manifest: server build, client contract, guide and bootstrap revisions, `server_url` ([Updating a client](release.md#updating-a-client)) |
| GET | `/flats` | both | List; `?q=` filters by slug or name |
| POST | `/flats` | api | `{slug, name}` creates an empty Private flat (201). The console never creates flats |
| GET | `/flats/{slug}` | both | Flat view |
| GET | `/flats/{slug}/document` | both | `?doc=`: live Markdown of a docs flat, or its saved content when not running |
| GET | `/flats/{slug}/draft` | both | Current Private Draft metadata, or 404 |
| POST, PUT | `/flats/{slug}/draft` | both | Complete archive (tar, tar.gz, zip); query `expected_revision`, `git_sha`, `git_dirty`, `message`. 201 `{draft}`; never a version or approval. Documents are refused here |
| POST | `/flats/{slug}/document` | both | Complete docs archive; replaces the whole live bundle without approval. 201 `{flat}` |
| GET | `/flats/{slug}/board` | api | `get_board` read of a board: `?revision=&frame=&rect=x,y,w,h&ids=a,b&format=full\|outline&cursor=&limit=`; answers `{seq, revision, pending, meta, elements, next?}` |
| POST | `/flats/{slug}/board` | api | An `update_board` body `{op_id, ops, layout?, assets?, if_seq?, message?, replace?}` (assets' `content` in base64; the body at most `upload_max_bytes × 4/3` plus 16 MiB, so one call carries images of about one upload limit in total; send more over several calls or in a data file through the import route); applies at once without approval and creates a missing board. Answers `{revision, seq, ids, layout, results, assets?, created?, warning?}` |
| POST | `/flats/{slug}/board/import` | api | A board data file as the raw body (`.zip`, up to `upload_max_bytes × 4`, streamed to a temporary file); query `mode` (`add`, the default, or `replace`) and `op_id` (random when absent; resending with the same `op_id` imports once); applies at once without approval and creates a missing board |
| POST | `/flats/{slug}/versions` | both | `{revision, hash?}` (revision 0 selects current): 202 `create_version` request. Creates the version without deploying it |
| GET | `/flats/{slug}/versions` | both | Retained versions, `[]` when none |
| GET | `/flats/{slug}/versions/{n}` | both | One version |
| GET | `/flats/{slug}/versions/{n}/files/{path}` | both | One file of a version (thumbnails), served sandboxed |
| DELETE | `/flats/{slug}/versions/{n}` | both | Non-live version: agents get a 202 `delete_version` request; the console deletes directly (200) |
| POST | `/flats/{slug}/deploy` | both | `{version}` (`version > 0`): 202 `activate` request for an existing version |
| POST | `/flats/{slug}/rollback` | both | `{version, restore_data}` (0 = previous live): 202 `rollback` request; data is kept unless `restore_data` |
| GET | `/flats/{slug}/deployments` | both | Deployment history |
| POST | `/flats/{slug}/previews` | both | `{target: "draft", version: 0}` or `{version: n}`: 201 Private preview. There is no Public Draft route |
| GET | `/flats/{slug}/previews` | both | Open previews |
| DELETE | `/previews/{host}` | both | Close a preview |
| POST | `/flats/{slug}/visibility` | both | `{visibility, reason}`: 202 `set_visibility` request in either direction; the same value answers 200 `status: "done"`, `visibility unchanged`; Public for an unpublished flat is 409 |
| POST | `/flats/{slug}/rename` | both | `{slug}` changes the slug; the old one redirects |
| DELETE | `/flats/{slug}` | both | `?reason=`: agents get a 202 `delete` request; the console deletes directly (200) |
| GET | `/flats/{slug}/logs` | both | Events; `?kind=&after=&limit=` |
| GET | `/flats/{slug}/env` | both | `{env: [{name, value, updated_at}], note}`; ordinary values only |
| PUT, DELETE | `/flats/{slug}/env/{name}` | both | `{value}` (any string, including empty); applies at the next activation |
| GET | `/flats/{slug}/network` | both | `{origins, updated_at, note}`: server HTTP(S) origin grants |
| PUT | `/flats/{slug}/network` | both | `{origins: [...]}`, `[]` clears. Console, or the CLI on loopback; 403 otherwise |
| GET | `/flats/{slug}/secrets` | both | Secret names; values are never returned |
| PUT, DELETE | `/flats/{slug}/secrets/{name}` | both | `{value}`. Console, or the CLI on loopback; 403 otherwise |
| GET | `/flats/{slug}/stats` | both | `?days=` (1–365, default 30): daily page views, top paths and `usage` (below) |
| GET | `/flats/{slug}/snapshots` | both | Data snapshots taken before deploys |
| POST | `/flats/{slug}/providers` | both | `{provider, permitted}`: per-flat network permission. Console only: other surfaces get 403 |
| GET | `/approvals` | both | `?status=pending\|approved\|rejected\|failed` |
| GET | `/approvals/{id}` | both | Poll one approval |
| POST | `/approvals/{id}/approve`, `/approvals/{id}/reject` | console | Decide. Answers the approval with `decided_by` |
| POST | `/flats/{slug}/name` | console | `{name}`: display name |
| PUT | `/flats/{slug}/listing` | console | `{listing}`: `default`, `hidden` or `listed` relay listing of the Portal route |
| GET, PUT | `/flats/{slug}/version-quota` | console | GET is the flat view; PUT `{version_quota: n\|null}` (null follows the host) |
| GET, PUT | `/flats/{slug}/revision-quota` | console | Documents, designs and boards: `{revision_quota, pin_quota}` overrides (`n\|null`) with the host defaults and pin count |
| GET | `/providers` | console | Host providers: grant, backend status, hosts and provider settings |
| PUT | `/providers/{id}` | console | `{enabled}` with the settings ETag in `If-Match` |
| PUT, DELETE | `/providers/zrok/account` | console | `{token}` connects or replaces the zrok account; DELETE disconnects (`?force=1` removes a stale environment locally) |
| GET | `/keys` | console | Operator keys: state, source, time and identity, never values |
| PUT, DELETE | `/keys/tailscale`, `/keys/netbird` | console | `{key}` stores, replaces or removes the Tailscale auth key or NetBird setup key |
| DELETE | `/keys/netbird/legacy-peer` | console | Dismiss the notice about an earlier version's shared NetBird peer |
| GET | `/console-domains` | console | Per provider: domains, retiring names, whether the network is on, pin; plus the domain the request came through, whether it came over loopback, pending approvals, the preferred console (`preferred`, `preferred_state`, `link_url`) and the ETag |
| POST | `/console-domains` | console | `{provider, name}` (provider defaults to `tailscale`) with `If-Match` |
| DELETE | `/console-domains/{provider}/{name}` | console | With `If-Match`; answers `{etag, redirect}` |
| PUT | `/console-domains/preferred` | console | `{preferred}`: `local`, `tailscale:NAME`, `netbird:NAME` or `""` for none, with `If-Match`; answers `{etag}` |
| GET, PUT | `/settings` | console | Read or save `config.json` settings (below) |
| POST | `/settings/impact` | console | What the next pruning would remove with the candidate settings; writes nothing |

Approval outcomes, frozen parameters and the `result_data` enums are defined in
the [lifecycle core contract](lifecycle-core-contract.md). Version creation,
activation, rollback, visibility in both directions, deletion and version
deletion requested by an agent wait for a console decision. The console does
not skip the approval request: for those same actions it creates the request
and approves it at once (its confirm dialog is the decision), except flat
deletion and version deletion, which it applies directly. Previews, provider
permissions and the Portal listing take effect immediately.

### Response details

- **Flat view.** Adds to the stored flat `publication`, `live_version`,
  nullable `draft`, `providers`, `connection_state`, `endpoints` (see
  [Network contract](lifecycle-core-contract.md#network-contract)),
  `private_url`, `public_url`, `versions`, `version_quota` (override),
  `effective_version_quota`, `version_count`, `next_version`, `latest_version`,
  `latest_deployed`, `disk_bytes`, `running` and `portal_listing` /
  `portal_hidden` (the listing Flats asks the relays for, not what they show).
  Visibility outputs are `private` or `public`; legacy listed/unlisted inputs
  are stored as public. URLs are never authorization or proof of publication.
- **Draft saves.** The receipt describes the exact saved content: `type`,
  `flat`, `revision`, `hash`, `size`, `files`, `kind`, git metadata, `message`,
  `saved_at`. GET `draft` reads the current Draft with `base_version`,
  `dirty`, `created_at` and `updated_at`. A stale `expected_revision` is 409
  and leaves the Draft unchanged; an invalid archive is 422 with problems and
  also keeps the Draft. A `deploy` query parameter on a save is refused.
- **Version deletion.** Can answer `status: "cleanup_pending"` and
  `cleanup_pending: true` (also in an approved deletion's `result_data`) when
  the version is already deleted but storage cleanup is incomplete. Do not
  repeat the deletion: repair the reported storage issue and restart the host.
  Guarded storage changes are blocked while the `.version-delete-N` staging
  folder remains; a repeated cleanup failure can prevent host startup.
- **Stats.** Besides `page_views` and `top_pages`, `stats` returns `usage`
  for the same `?days=` window ([Metrics](spec/metrics.md#flat-usage)):
  - `days`: one entry per UTC day with usage (days without it are omitted):
    `day`, `requests`, `status` (counts by `1xx` … `5xx` and `closed`),
    `rate_limited`, `cpu_seconds` and `memory_peak_bytes`.
  - `totals` for the period: `requests`, `errors` (`5xx` only),
    `error_rate` (`null` with no requests), `p50_ms` and `p95_ms`
    (interpolated from the response-time histogram, `null` when no request
    was timed; days recorded before the histogram's buckets changed give no
    percentiles), `cpu_seconds` and `memory_peak_bytes`. In `days` and
    `totals`, `cpu_seconds` and `memory_peak_bytes` are omitted where that
    figure is not measured, never reported as zero.
  - `now`: `cpu` and `memory` are `measured`, `not_separate` (websites,
    pages, designs and boards, which run inside the host process; the other `now`
    fields are then omitted) or `unsupported` (memory on hosts other than
    Linux). With a worker: `state` (`running`, `starting`, `quiescing`,
    `sleeping`, `backoff` or `stopped`; diagnostic only, `running` in the
    flat view keeps its meaning), `workers`, and with measured memory
    `memory_bytes`, `memory_sampled_at` and `memory_stale` (no sample for 60
    seconds).

  A `closed` request is one whose visitor left before the response was sent;
  it is not an error. WebSocket connections and event streams count with
  their status but are left out of response times. Reading `stats` never
  wakes a sleeping document and never writes usage; it includes usage not yet
  persisted. When usage cannot be read, `stats` omits `usage` and still
  returns page views.
- **Settings.** `GET settings` returns the settings, defaults,
  `apply_on_restart` and `config` (mode, ETag, `changed_on_disk`, read-only
  host, network and credential values without paths, per-key sources and flag
  pins). `PUT settings` saves to `config.json` and returns `applied` and
  `restart_required`. Changes send `If-Match`; 412 means `config.json` changed
  or the page is stale, and 409 means a flag of a service started without
  `--config` pins the key.

## CLI

`flats <command>` calls the HTTP API (the CLI sends `X-Flats-Client: cli`).
`flats serve` runs the host itself (`flats worker` is the process it starts
for server flats); `flats config` and
`flats key` edit the host while it is stopped
([Configuration and storage](configuration.md)).

| Command | Effect |
| --- | --- |
| `save-draft <slug> <dir\|archive> [--expected-revision n] [--message m]` | Save complete output as the Private Draft |
| `draft <slug> [<dir\|archive>] …` | Read Draft metadata, or save like `save-draft` |
| `get-draft <slug>` | Read Draft metadata |
| `save-document <slug> <dir\|archive> [--message m]` | Replace a whole live docs bundle |
| `board get <slug> [--outline] [--revision n]` | Read every page of a board's elements, or of an earlier revision |
| `board apply <slug> <ops.json> [--message m]` | Send an `update_board` body to a board, creating it when missing; a body without `op_id` gets a random one |
| `board import <slug> <file.zip> [--replace] [--op-id id]` | Import a board data file; adds with new ids unless `--replace`. Prints the operation id it used (random unless `--op-id`); resending with the same `--op-id` imports once |
| `create-version <slug> [--revision n] [--hash h]` | Request a new version from the Draft |
| `deploy --flat <slug> --version <n>` | Request activation of a saved version; a directory argument is refused with guidance to save and create a version first |
| `rollback <slug> [--to n] [--restore-data]` | Request activation of an earlier version |
| `delete-version <slug> --version n` | Request deletion of a non-live version |
| `versions`, `list`, `info`, `logs [--follow]` | Read |
| `preview <slug> [--version n]` | Open a Private preview; 0 (default) is the current Draft |
| `visibility <slug> private\|public [--reason r]` | Request a visibility change |
| `rename <slug> <new-slug>`, `delete <slug> [--reason r]` | Rename; request deletion |
| `env`, `network`, `secret` | Manage server settings; changing `network` origins or `secret` values works only from the host itself (loopback) |
| `approvals [--status s]` | List approvals (read-only) |
| `status`, `install`, `uninstall`, `connect`, `mcp`, `mcp-config` | Host and client setup ([Connecting agents](agents.md)) |
| `client check`, `update`, `skill sync`, `version` | Client freshness ([Updating a client](release.md#updating-a-client)) |

Exit codes: 0 success, 1 error, 2 usage, 3 pending approval, 78 configuration
the operator must fix. Every request that waits for approval exits 3 in text
and JSON modes, with the approval URL, its status and how to poll it. A pending
Private-to-Public request says that internet access begins only after approval;
it never describes the flat as already Public. The CLI has no approve, reject,
provider-permission or key command against a running host.

## MCP

Tools (names are the registered `Name` values):

| Group | Tools |
| --- | --- |
| Client | `check_client`, `get_client_skill`, `guide` |
| Read | `list_flats`, `get_flat`, `list_versions`, `get_draft`, `get_logs`, `get_approval`, `get_network`, `list_env`, `list_secrets` |
| Websites and pages | `create_flat`, `save_draft`, `save_draft_from_dir` (callers on the host's loopback only), `save_page`, `create_version`, `deploy`, `rollback`, `delete_version`, `open_preview` |
| Documents | `save_document`, `get_document`, `update_document` |
| Designs | `save_design`, `get_design` |
| Boards | `get_board`, `update_board`, `get_board_share`, `import_board` |
| Revisions (documents, designs and boards) | `list_revisions`, `get_revision`, `pin_revision`, `rollback_revision` |
| Flat settings | `set_visibility`, `delete_flat`, `set_env`, `delete_env` |
| References | `get_runtime_reference`, `get_content_types` (also resources `flats://docs/runtime-api/v1` and `flats://docs/content-types/v1`) |

Saves contain complete inline files or validated host build-directory content.
Typed output names Draft content only as `draft`; there is no version 0.
Document saves return `flat` without a Draft or version. Version operations use
positive numbers and return the pending approval, its status and link. There is
no decision, provider-permission, settings or key tool, and the MCP handler
never gains console authority. The server instructions describe Private URLs as
Local loopback or explicitly permitted Tailscale routes, and pending visibility
notices use the future tense. Removed tools (`save_version`,
`save_version_from_dir`, `publish`) have no aliases.

Revision management of documents, designs and boards also happens inside the
flat's own Private route; see [Revision store](revision-store.md).
`rollback_revision` on a board answers `{status, revision}`, or `{status,
warning}` without a revision when the restore applied but its revision is not
recorded yet (import pending, or already deleted by retention); it must not be
resent.

### Board tools

| Tool | Input | Result |
| --- | --- | --- |
| `create_flat {type: "board"}` | `slug`, `name` | An empty Private board (revision 1, `created`) |
| `get_board` | `slug`, `revision?`, `frame?`, `rect?`, `ids?`, `format?` (`full` or `outline`), `cursor?`, `limit?` | `{seq, revision, pending, elements, next?}`; each element carries `ver` for guards |
| `update_board` | `slug`, `op_id`, `ops`, `assets?` (`[{ref, content}]`, `content` in base64; image elements give the ref as `asset`; images of about one `upload_max_bytes` in total per call, as the request is at most `upload_max_bytes × 1.5` plus 1 MiB), `layout?`, `if_seq?`, `message?`, `replace?` | `{revision, seq, ids, layout, results, assets}`; all operations apply or none, as one revision. `revision` is null with a `warning` while its import is pending |
| `get_board_share` | `slug`, `n?` | One selection a person sent to the agent, or the recent list; deleted elements are `missing` |
| `import_board` | `slug`, `op_id`, `archive` (base64, at most `upload_max_bytes`), `mode` (`add` or `replace`) | `{revision, seq, ids}`; creates a missing board |

Board writes and imports apply at once without approval. Board tools refuse
other types (`not_board`); version tools refuse a board (`invalid`). A guard
mismatch, a missing or locked target, an expired read cursor or a reused
`op_id` with a different request is `conflict`; the per-flat disk quota is
`board_capacity`. A write or import that brings new images is checked
against the quota before its images are stored, so a refused call stores
none. `get_revision` on a board returns its file list; read an
earlier board's elements with `get_board {revision}`. The agent guide is
`topic.board`.

### Board routes inside the flat

A board serves its canvas on its own routes. The board trusts only the
access level core decides for the route; a missing or unknown one counts as
Public. Every POST and WebSocket upgrade needs `Sec-Fetch-Site: same-origin`
(or, without it, an `Origin` equal to the request's scheme and host), and
POST also needs `X-Flats-Board: 1`.

| Route | Access | Use |
| --- | --- | --- |
| `GET /` | everyone | Canvas app |
| `GET /_board/app/<file>` | everyone | Built-in app files |
| `GET /_board/ws` | everyone; writes, presence and sharing are Private | Live updates |
| `GET /_board/a/<hash>` | Private: any asset of the board or a retained revision; Public: only assets a current element uses | Images |
| `POST /_board/api/assets` | Private | Image upload, answers `{hash, w, h}` |
| `GET /_board/api/revisions[/{n}]`, `POST …/{n}/pin`, `POST …/{n}/rollback` | Private | History |
| `GET /_board/api/revisions/{n}/elements` | Private | Elements of an earlier revision, for element recovery |
| `POST /_board/api/recover` | Private | Copy elements of an earlier revision into the board with new ids |
| `GET /_board/export.zip` | everyone | Data file of the current board; Public gets the public projection |
| `POST /_board/api/import` | Private | Import a data or recovery file |

A Public route receives only the current board: no cursors, names,
selections, shares, history or past-only assets. Open Public connections
close when the board becomes Private, its route stops, or it is renamed or
deleted. Every frame a board socket sends counts toward its send queue cap;
a socket over it is closed with 1008 and the canvas reconnects.

## Console

### Words

The console uses two words for the two approved steps of websites and pages.
**Save** turns the Draft into a new numbered version and puts nothing live
(`Save as vN`, approval action `create_version`). **Publish** puts one saved
version live (`Publish vN`, approval action `activate`); publishing an earlier
version again is `Roll back to vN`. Save buttons are drawn quieter than Publish
buttons. Documents, designs and boards have no Draft, versions or publishing:
they run once saved.

### Screens

- **List.** Rows show `Live vN`, `Not published`, `vN saved, not published`
  (the newest saved version was never live, `latest_deployed` false) and
  `unsaved draft changes`; documents, designs and boards show `Running` or
  `Not running`. `Save as vN` appears for flats with Draft changes, otherwise
  `Publish vN` when the newest saved version was never live. A row's menu holds
  the open links, Share, Deployments (Overview for documents, designs and
  boards), Analytics and Settings. Each row has its type's label and icon
  (Flat, Document, Page, Design, Board); a running board's open link goes to
  its canvas. Rows do not repeat a public-access banner.
- **Flat page.** Tabs: Deployments (Overview for documents, designs and boards) at
  `/flats/{slug}`, Analytics, Database (server flats only) and Settings.
  Deployments shows the addresses and live state, the Versions table, open
  previews and logs; the Versions table starts with a Draft row (Preview,
  `Save as vN`) while `draft.dirty` is true, and each version has a `Live`,
  `Previously live` or `Not published` badge with `Publish vN` above the live
  version and `Roll back to vN` below it. The Overview of a document, design
  or board has the addresses, running state and logs, and no Versions or
  previews. Database lists data snapshots. Settings holds the name, slug,
  sharing, networks, version history quota (websites and pages), revision
  quotas (documents, designs and boards), environment variables, secrets,
  server HTTP(S) permissions and deletion.
- **Networks** on a flat's Settings: Local (always on), Tailscale and
  NetBird under Private; Tailscale Funnel, Portal, zrok, Cloudflare Quick
  Tunnel and NetBird Expose under Public. A provider that is off for the host
  cannot be newly allowed. A flat that allows Portal also has its relay
  listing choice (follow the host setting, hidden or listed), described as the
  listing requested from the relays, which follow at their next lease renewal,
  and not as access control.
- **Share.** Private or Public, with Visit and Copy link. Making a flat Public
  is refused in the console until it has a live version and allows a public
  network. The public URL is `public_url`, or the URL of a current Funnel,
  Portal, zrok, Cloudflare quick tunnel or NetBird Expose endpoint.
- **Settings.** Four pages behind a left nav. Networks: the defaults for new
  flats, then Private and Public providers, each opening its own page with
  Turn on / Turn off, its key (Tailscale auth key, NetBird setup key, zrok
  account token: state and source, never the value), its hosts, its own
  settings and the flats that allow it. Console: the console's addresses on
  every network, the address for links, external reverse-proxy origins and the
  read-only host values. Limits:
  storage, history and traffic. Agents: MCP setup and what the agent tools
  cannot do (the console itself is not authenticated yet). Fields stage changes.
  Console has separate inline Save/Discard controls for Links and Reverse proxy,
  always visible and disabled until that section changes; saving one preserves
  edits in the other. Other settings pages use one floating save bar. Each
  form counts its changes, blocks invalid values and asks again before a save
  that removes data. Removing the external origin currently in use also asks
  for confirmation because that address stops accepting new requests;
  buttons act at once. A disabled action shows its reason next to it or on
  hover and keyboard focus.
- **Approvals.** A top-bar item with the number of pending requests opens the
  queue.
- **Approval page.** Names version creation (including legacy `publish`
  requests), version deletion, activation, rollback, data restore, visibility
  changes and deletion, so requests made by agents can be reviewed and decided
  there.

Legacy `public-listed` and `public-unlisted` values are shown as Public.
Permission for a network does not publish a flat or change its visibility, and
Tailscale being connected does not permit Funnel. Removing a public network
while the flat is Public is refused (`provider_in_use`) and the console shows
that error.

### Labels

Labels match the data the server returns:

- Limits uses the shared labels **Revisions to keep** and **Pinned revisions
  allowed** for the revision and pin quotas of documents, designs and boards, and
  **Version quota** for websites and pages.
- Analytics shows **Page Views** (requests for pages), with the note that
  unique visitors are not tracked, and under it **Usage** (every request to
  the live route): **Requests**, **Error rate**, **Median response time**,
  **p95 response time**, **CPU time** and **Peak memory** for the period; a
  "Now" line with the worker state (**Running**, **Starting**, **Going to
  sleep**, **Sleeping**, **Restarting**, **Stopped**) and current memory,
  with the sample age when the sample is stale; and daily requests stacked by
  status class (2xx, 3xx, 4xx, 5xx, Closed, and 1xx when there were
  upgrades). A missing value is shown as an em dash, never as zero.
- For websites, pages, designs and boards the CPU and memory tiles are replaced by the
  note "CPU and memory are not measured separately for this type; it runs
  inside the Flats process." On a host that does not measure worker memory
  (not Linux), Peak memory says "Memory is not measured on this system." A
  server that returns no `usage` shows no Usage section.
- For documents, designs and boards, Analytics hides the Versions count and shows disk
  usage as a plain byte value. Websites, pages and an unknown type show the
  Versions count and the disk value with its "(versions, data and previews)"
  description.

### Console addresses

Approval links, their event messages and `console_url` / `mcp_url` in
`flats status` follow the request: the console domain the request came
through while it is still listed and served. Otherwise (the loopback
listener, or a domain being deleted) they use the preferred console,
`console.preferred_host`, once its node is ready, else the first served Tailscale
domain, then the first served NetBird domain, then the loopback address
([Preferred console](configuration.md#preferred-console)). Unset, the
preference changes nothing. A Host matching an operator-declared external origin
uses that origin and scheme; any other Host never selects an external link. MCP
requests carry the same context, so an agent's approval links use the domain
it connected to. `console_urls` lists every served domain, Tailscale first,
whatever the preference. The agent discovery documents (`/llms.txt`,
`/llms-full.txt`, `/docs/agent-guide.md`) do not follow this rule: they name
the MCP endpoint at the address the agent fetched them from (a Host the
listener accepts), which is the address that agent can reach.

Console domains are managed in Settings → Console
([Console domains](configuration.md#console-domains)). At the surface:

- Adding needs the settings ETag, a lowercase DNS label not already used by a
  flat, preview, redirect or another console domain of the same network, and
  the network turned on. The same name may serve the console on both
  Tailscale and NetBird. A domain pinned by `--console-host` cannot be changed
  here.
- Deleting saves the list first, then retires the node. A console reached
  through a console domain refuses to delete the last domain of both lists;
  only the console on the loopback listener (judged by the connection's local
  address) can empty them.
- Deleting the domain the page is open on needs another console domain that is
  ready (Tailscale first). The response names it in `redirect` and is sent
  before the node is retired, a couple of seconds later, because retiring it
  closes the connection.
- Turning Tailscale or NetBird off is refused while that network still has
  console domains.
- The preferred console is `local` or a listed domain, set with the settings
  ETag; a NAME not in its list is refused (400 `invalid`), and deleting the
  preferred domain unsets it in the same change. The view reports it as
  `preferred`, with `preferred_state` (`served` while links use it,
  `starting` while its node is not ready yet, `not_served`, or `unlisted`
  for a file edit naming a domain that is not listed) and `link_url`, where
  links go when they cannot follow the request.

### Data-model and client links

Version-model rules: [Lifecycle core contract](lifecycle-core-contract.md).
Default provider grants of new flats: [Operator authority](lifecycle-core-contract.md#operator-authority-and-frozen-approvals).
Upgrading pre-lifecycle grants: [Private and public access](networking.md#upgrading-provider-grants-from-an-older-host).
Client and bootstrap revisions: [Updating a client](release.md#updating-a-client).
