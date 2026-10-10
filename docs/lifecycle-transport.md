# Lifecycle transport and console decisions

## Console decisions

Approval decisions and per-flat provider grants are console routes only. Console
routes require `X-Flats-Console: 1`, no `X-Flats-Client`, and for mutations a
same-origin browser request with an Origin header. These checks are CSRF
protection, not authentication: a local process that sends the same headers to
the loopback listener can decide. Agent API, CLI and MCP surfaces offer no
decision or grant operation; on a host whose `network.private_backend` is
`tailscale`, a flat they create starts with the Tailscale permission that host
configuration grants (see the core contract). Deploy only trusted agents on the host and restrict
the console node with tailnet ACLs. A separate operator credential is an open
design item; the earlier session-based design was removed from the console.

## Ownership and verification

Transport owns `internal/api`, `internal/mcpx`, `internal/cli` and this document.
App configuration, console UI, providers and acceptance gate are integration
owned; core/store/bundle are core owned. The prerequisite network contract was
adopted only from the exact coordinator-granted commit. Lifecycle methods are bound to exact coordinator-granted committed core exports.
Final app/provider/gate integration and independent Claude review remain separate.

Disposable tests cover console CSRF/client rejection and agent-route refusal of
decisions and provider grants.
CLI tests cover pending exit 3 and approval links for upload/deploy/rollback/both
visibility directions, plus read-only approvals and absent decision commands.
These tests do not prove live internet exposure or visitor ACL behavior.

## HTTP lifecycle routes

Both prefixes expose read/save/request operations; the console prefix keeps CSRF
checks. Console requests never skip the core approval request. Existing unrelated routes remain available.

| Method / flat-relative path | Input | Response |
| --- | --- | --- |
| POST/PUT `draft` | Complete archive; optional `expected_revision`, git metadata, message query | 201 Draft plus compatibility `version` with number 0, role draft, revision and published false |
| GET `draft` | None | Current Draft metadata, or 404 |
| POST `versions` | Same complete archive | Draft save compatibility; no published vN |
| POST `versions?deploy=1` | Same archive | 202 pending publish, top-level approval/status/link and nested deploy request |
| POST `publish` | `{revision, hash?}`; revision 0 selects current | 202 frozen current-candidate publish request |
| POST `deploy` | `{version}`; 0 selects current Draft, positive selects existing published version | 202 pending request |
| POST `rollback` | `{version, restore_data?}` | 202 frozen request; default retains data |
| POST `visibility` | `{visibility, reason?}` | Both directions 202; same value 200 with status done and message visibility unchanged; Unpublished Public is 409 |
| POST `previews` | `{target:"draft",version:0}` or positive published version | 201 Private preview; no Public Draft route |
| GET `versions` | None | Published versions only, including empty `[]` |
| POST console `providers` | `{provider, permitted}` | Console only; 200 flat view, no publish or visibility change |
| GET/PUT console `/providers[/{id}]` (host, not flat-relative) | `{enabled}` | Console only; host grants and backend status per provider; turning off is refused while a flat allows it |
| PUT console `listing` | `{listing}`: `default`, `hidden` or `listed` | Console only; 200 flat view. Requested relay listing of the Portal route; the served route's lease metadata is updated at once and relays apply it at their next lease renewal; no publish, visibility or access change |

The concrete flat response exposes `publication`, `live_version`, nullable
`draft`, `providers`, `connection_state`, `endpoints`, `portal_listing` and
`portal_hidden` (the requested relay listing, not the relays' observed
state). Each endpoint uses
exact lowercase JSON tags: `provider`, `url`, `state`, `detail`, `configured`,
`permitted`, `ready`, `audience`, `host`. Provider IDs are `local`, `tailscale`,
`tailscale-funnel`, `portal`. Visibility outputs are `private`/`public`; legacy
listed/unlisted inputs normalize to public. The provider manager supplies actual
configuration/readiness; URLs are never treated as authorization or publication.

Stale/unknown expected revisions return 409/category conflict and do not replace
Draft content. Malformed/negative/duplicate expected_revision parameters return
400. Invalid archives return 422 with actionable problems and preserve Draft.
Publish drift leaves the prior serving version/history intact and reports typed
`approval.result_data.failure_code`; a fresh request is required.

Only POST `/console/api/approvals/{id}/approve` and `/reject` accept decisions.
The audit actor is the trusted tailnet login when the console node supplies one
(`core.WithActor`), otherwise `console`. HTTP retains core's persisted `decided_by`, `authorized_at` and `result_data`,
including repeat decisions. Core owns persisted actor/recovery semantics.

`result_data` is a typed execution object with `status`, optional `failure_code`,
`data_impact` (none/runtime_start/restore_data/unknown), `health_data`
(not_run/isolated_copy), and `live_data`
(untouched/runtime_may_write/restored/unknown). New restore snapshots capture DB
and FILES; legacy DB-only snapshots preserve current FILES. Transport never
converts a setup/read/provider connection into consent to run these actions.

## CLI and MCP

`flats draft <slug>` reads metadata; adding a build directory/archive saves
complete content. `--expected-revision` detects conflicts. `flats publish
<slug> [--revision n] [--hash h]` requests publication. Existing `deploy` uploads
Draft and requests publish unless `--save-only`; explicit `--version 0` selects
current Draft and positive versions select already published content. Default
`preview` opens current Draft; positive `--version` previews published content.
Publish, activation, rollback and both visibility directions return exit 3 while
pending in text and JSON modes, with approval URL/status and read-only poll
instructions. A pending Private-to-Public response says that internet access will
begin only after approval succeeds; it never describes the flat as already Public.
`approvals` only reads; no approve/decide/reject/operator/provider
commands are added.

MCP adds `save_draft`, `get_draft`, `publish` and retains existing save/deploy/
rollback/preview tools. Saves contain actual inline files or validated host
build-directory content, optional expected_revision, git metadata and message.
Typed output separates Draft revisions from published versions, includes pending
approval/status/link, and exposes execution/audit DTOs. There is no
decision or provider grant tool. MCP handler
never imports an authenticated console context. Every listed tool is behaviorally
censused with a pending approval, checking its unchanged status and current served
bytes/version/visibility/provider policy. Publication requests include positive
controls through console HTTP decisions.

MCP server instructions describe Private URLs as Local loopback or explicitly
permitted Tailscale routes. Pending visibility notices use future tense. Approval
reads distinguish a request waiting for an operator decision from an approved
request whose operation is still applying. Approval action metadata uses
`activate` for serving an existing published version.

## Verification limits

Transport tests use actual HTTP/CLI/MCP/core and disposable loopback listeners.
They verify repeated Draft saves without vN, conflict/validation preservation,
Private Draft previews, pending requests, agent-route decision
rejection, console decisions, frozen Draft drift and fresh controls,
and actual current bytes. Public publish/rollback tests hit both the Public and
retained private current URLs and distinguish Draft preview bytes.

The PublicNet adapter in transport tests is a disposable Local double. Production
provider-manager host+flat grants, backend faults, crash recovery, migration,
complete restore data populations and rendered console/bootstrap integration are
separate core/integration gates. These tests claim neither live internet/Funnel
reachability nor visitor ACL/owner-only behavior. The installed operator server,
real flats/data/authkeys and real network providers are untouched.

## Typed decision failures

Failed decision responses retain the persisted `approval` and its `result_data`
(including `failure_code`), alongside `error` and typed `category`. Provider
permission, readiness, availability, unconfirmed public-stop, stale approval and
unchanged-content causes return HTTP 409, with categories matching their core
execution cause. Health/runtime deployment failures retain HTTP 422; unrelated
internal storage failures remain HTTP 500. A published visibility change requests
approval even when a provider is unavailable or unpermitted; only authorized
application checks those prerequisites. Same visibility remains unchanged HTTP
200, and an unpublished flat cannot request Public visibility.

## Host provisioning

Local is default; Portal is off. Legacy schema 5 records eligible pre-lifecycle flats. On hosts with historical tsnet state, explicit `--network tailscale` (or a stored explicit Tailscale backend/grant) preserves only those flats’ Private Tailscale opt-in once, honoring explicit denials. Explicit `--network local` consumes that choice as Local-only; an ambiguous historical-default startup refuses before constructing backends. With neither historical tsnet state nor a persisted Tailscale choice, the first upgrade consumes eligibility as Local-only; a later host grant cannot opt those flats in. Neither choice grants Funnel or Portal. See README upgrade instructions.

`get_flat` retains its publication/visibility/current-version summary when appending Draft preview text. `save_draft` saves without allocating a number; its compatibility `deploy:true` option requests pending publication, whose successful approval later allocates vN. Tool and CLI guidance never treats pending as live.
