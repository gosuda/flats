# Lifecycle regression gate

Run `scripts/lifecycle-gate.sh` from the integrated worktree. Exit 0 means every
selected observable case passed; exit 1 means at least one failed. A partial run
(`--only case-id,...`) is diagnostic and never establishes full acceptance.
The launcher builds `cmd/flats`, a historical binary from the exact archived
pre-lifecycle base, and a test-only provider host. `FLATS_BIN` may supply an
integrated binary; `FLATS_LIFECYCLE_LEGACY_BIN` may supply the historical binary.
Neither variable refers to a running server.

Each case starts a new server with its own disposable data directory and two
free loopback ports. The runner never kills listeners it did not start. It
disables Portal, selects Local, removes inherited `TS_AUTHKEY`, bypasses HTTP
proxy settings, and never opens user data, authkeys, tailnet identities, or
relay configuration. Data and logs remain in the printed temporary evidence
directory for review. Raw execution logs must stay outside the public repo.

## Observable acceptance matrix

| Requirement | Case / observable oracle | Proof boundary |
|---|---|---|
| Repeated saves create no published v1; conflicting writer rejected | `draft-save-conflict`: three validated tar.gz uploads return number 0, revision 3, no history/live pointer; stale expected revision returns 409 and keeps revision | Actual binary, HTTP |
| Unsafe archive validation changes nothing | `archive-validation`: traversal upload rejected 422, prior flat view preserved | Actual binary, HTTP archive validation |
| Draft preview before publication is Private | `private-draft-traffic`: preview serves the exact uploaded marker over loopback, no live version/history/public URL | Actual binary, Local traffic |
| All publish entrypoints await human approval | `api-upload-deploy-pending`, `cli-console-pending`, `mcp-pending`: upload-and-deploy and explicit deploy produce pending approval, API 202/CLI 3/MCP pending, live 0/history empty; console request also pending | Actual integrated binary, CLI subprocess and JSON-RPC MCP |
| Agent cannot approve its request | `publish-human-idempotency-restart`: `/api/.../approve` unavailable; `mcp-pending`: initialized tool inventory has no approve capability | Existing console/API trust boundary; this does not establish owner auth |
| Explicit approval first creates v1/Published/Private | `publish-human-idempotency-restart`: console decision simulation approves once; published history exactly [1], live 1, publication Published; GET serves candidate marker | Actual binary, HTTP and traffic |
| Duplicate pending requests, concurrent decisions, retries do not activate twice | `publish-human-idempotency-restart`: same approval id; concurrent decisions plus repeat approve; one version and one deployment before/after restart | Actual binary, persisted observable history |
| Approval rejects candidate/current policy drift | `frozen-revision-policy`: save newer revision then approve old request => failed/no version/live; provider policy change likewise fails | Actual binary, HTTP |
| Claimed approval resumes once after restart | `restart-claimed-approval`: after stopping the disposable host, test-only SQLite update models crash after persisted pending→applying claim; actual startup must activate bytes, with one deployment across two starts | Deterministic crash boundary; real binary restore/activation |
| v1 stays serving while Draft changes | `publish-human-idempotency-restart` and provider lane: GET remains old marker after new upload; private preview serves new marker | Actual Local traffic and separately labeled simulated public traffic |
| Explicit provider permission; Tailscale never implicitly authorizes Funnel | `provider-permission-failure`: initial provider set empty/local; agent cannot grant; grant tailscale excludes tailscale-funnel; neither permission changes visibility; unavailable/unpermitted Public approve fails, keeps Published v1 and Local traffic | Actual binary, disabled external infrastructure; no real Tailscale connection claim |
| Both visibility directions through API/console/CLI/MCP await approval | `visibility-all-surfaces-teardown`: each surface cycles Private→Public→Private, asserting old visibility before decision and unchanged v1 numbering | Deterministic loopback provider host uses product API/core/CLI/MCP |
| Draft never on Public path | Provider lane: Public current GET still v1, preview GET is draft, simulated public preview host returns 404 | Deterministic route registration and actual loopback traffic, not internet Funnel proof |
| Public→Private teardown failure is not success | Provider lane: injected Stop failure leaves GET answering, approval fails, visibility remains Public; clear fault and reapprove => route 404, Private current still answers | Deterministic provider fault, actual HTTP serving oracle |
| Network failure separate from publication; no fallback permission | Local provider-failure and provider lane: connection/setup failure leaves Published/current version and visibility unchanged, no simulated public route opens | Fault injection and real Local traffic; external readiness remains separately unproved |
| Failed preparation preserves Draft/history/live data | `runtime-health-rollback-data`: real uploaded JS health handler writes sentinel then returns 503 on isolated check; before decision no execution; failure leaves live hits=1/version=1/history=[1] | Actual integrated binary and real runtime/SQLite, not fake DOM |
| Code rollback preserves number/data; restore separately approved | Runtime case: publish v2, increment data; rollback pending then approved to v1 retains hits=2 and history [1,2]; restore_data request freezes true, leaves hits unchanged until decision, rejection keeps data | Actual binary/runtime; restore execution coverage must be recorded separately |
| Historical never-deployed migration preserves files and restarts | `historical-migration`: baseline binary saves historical v1/v2 without deploy; integrated server migrates history empty/Unpublished; both file hashes survive; preview serves latest; restart stable; approved first publish is v1 with legacy bytes | Real historical binary seed and integrated binary migration/traffic |

The test adapter is a separate executable under `internal/lifecyclecheck/adapter`.
Its fault endpoint exists only there and does not approve actions or write product
state. It links the real product core, HTTP API, MCP server, CLI and runtime;
only the external provider is doubled. The current adapter uses the historical
`PublicNet` fallback, not the production network manager. It must be reconciled
to the final optional `LifecycleNet` interface before claiming integrated provider
acceptance. Portal is represented by a loopback route, not a real relay. No result in
this lane proves Funnel internet access, provider identity/ACL behavior, or remote
teardown. Private remains existing loopback/tailnet ACL policy; owner-only auth is
deferred. Console decisions are simulated with the existing same-origin headers,
which agents can forge under the intentionally retained admin trust boundary.

## Baseline execution receipt

Verified `git ls-remote origin refs/heads/main` and local `git rev-parse HEAD` both
returned `29edc2a6e13e27867603a23ff5b1b5a8d1b84df0` before implementation. The proposed
core contract was read as a proposal, not implemented guarantees. The coordinator
confirmed that later current Draft revision/hash drift invalidates an earlier
pending approval, and authorized only test-harness loopback Public adapters.

Executed argv: `scripts/lifecycle-gate.sh`, exit **1**. At the pre-lifecycle base,
13 cases ran: archive validation **passed**, other 12 **failed**. The baseline
returned number 1 on the first save, rejected draft preview input, lacked the
publish endpoint, and immediately activated CLI/API/MCP deploy requests. These
are expected baseline failures, not acceptance passes. Several dependent cases
stop at missing publish and therefore do not establish their later oracles.
The temporary receipt is `evidence.json` in the printed gate directory; its
request/response traces and exact CLI argv/exits are local review artifacts.
The executed baseline binary SHA256 is
`5247e8d999ba4b395c3125f82dabba2384cb7857c49e673410d94d419583d065`;
the local JSON evidence SHA256 is
`d775c20ed1a28bf97b1d9207a788dc4d3b75a39f8b7f2740af90d64fafb3c159`.
`bash -n scripts/lifecycle-gate.sh`, Python AST parsing, and
`CGO_ENABLED=0 go build ./internal/lifecyclecheck/adapter` each completed with
exit 0. No generated bytecode was committed. The binary and test-host build
inside the full launcher also succeeded before the failing behavioral checks.

Integrated execution and the exact prerequisite commit list are still pending;
the coordinator requested this committed preparation handoff rather than an idle
worker awaiting adoption. All post-publish oracles remain unexecuted against the
candidate because no exact prerequisite adoption was granted in this Dispatch.

The next Codex integration/gate task must additionally implement and execute these
concrete populations, rather than infer them from the current narrower cases:

* Historical mixed migration: seed already-deployed legacy versions alongside
  never-deployed saved versions, live runtime data and disposable secrets, version
  previews and pending approval references through the baseline binary. After
  migration and repeated starts, assert exact deployed numbers/hashes, live data
  values and secret use via runtime responses, preview/reference preservation,
  newest working snapshot selection, and correct next published number. The
  current migration case only proves the never-deployed flat population.
* Runtime initialization effects: include a failing candidate whose initialization
  or top-level evaluation writes persistent data where that runtime allows it,
  separate from the existing failing `/health` handler mutation. Verify live data
  before/after approval and after failure/restart. This receipt makes no blanket
  `data_impact:none` guarantee for untested startup stages, nor does it assert a
  success response reports the correct impact field.
* Successful explicit restore: create known data on both sides of an observed
  deployment snapshot, request `restore_data:true`, verify no mutation pending,
  approve it, and assert the exact intended restored value, backup preservation,
  existing version numbers and serving bytes. The current case tests code-only
  rollback and rejection of a separately frozen restore request, not execution of
  an approved restore.
* Reconcile test provider wiring to the final integrated `LifecycleNet` and test
  both Funnel permission/failure and Portal teardown without opening the internet.
  Current Local failure checks do not model an established real Tailscale node or
  prove tailnet ACL outcomes; no owner-only guarantee is inferred.

Do not use the baseline receipt as approval to merge. Claude review must inspect
the final integrated tree, execute the full gate there, and reconcile every case
and the stated proof boundaries before a success claim.
