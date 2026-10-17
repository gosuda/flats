# Docs worker idle verification report

Status: frozen corrected Linux run passed; final-report review R3: APPROVE for report accuracy and PR publication, not CI, release or deployment sign-off. Additional earlier diagnostic receipts have the source qualifications below. Design Claude R4, implementation Claude R4, cancellation hardening and test-source Claude R3 approved. Production has not been modified.

## Candidate and environment

Source commit: `9cf3c881ff16a15a2d7e93e51de944b5c81ab760`. Base: `54f12147a928e3e58b00bac3b120f634c1fed8db`. Feature branch: `feat/docs-worker-idle`. This report is a subsequent documentation-only commit; its source/test claims refer to the source commit above.

Local evidence directory: workspace `reports/flats-docs-worker-idle-2026-10-17/`. Artifact manifest `final-verification-artifacts.json` hashes 51 selected verification/review artifacts (SHA256 `b7c4449b964529fefadeb330a927b7fc67def37e3086abea592fbe7363ac837c`). The frozen 24 changed source/test hashes were rechecked unchanged after all corrected runs and before committing. The coordinator independently compared those 24 hashes with both `git show` at the source commit and working files, and verified all 51 selected artifact hashes: zero mismatches (`coordinator-final-hash-verification.json`, retained separately). The same coordinator receipt counts 29 changed paths from base to source commit and lists the five documentation paths outside the 24-path manifest. This is tool verification, distinct from Claude’s read-only review, which could not compute hashes.

Cited evidence outside the 51-artifact hashed selection: `core-focused.log`, `core-focused-race.log`, `core-rollback.log`, `corrected-source-before-runs.json`, `code-candidate-4.json`, `coordinator-final-hash-verification.json`, and the candidate/baseline test binaries. Binary hashes below are separately recorded identities, not entries in that selection.

## Evidence coverage

| Area | Test / measurement | Status |
| --- | --- | --- |
| Runtime ownership | Exact-child lease, single-start, stop/cancel/panic, cooldown, process exit and retry | Corrected Linux normal and race PASS |
| Core lock and dispatch | Ready response stays pinned while storage locked; quiescing admission deadline; current wrapped handler redispatch; deleted route unavailable | Focused race PASS |
| Durable save | Actual Service/SQLite committed failure retains live pointer and files; changed save repairs logical deployment; dormant tick/budget no wake; clean sweep certificate; lazy rename/delete | Focused race PASS |
| Recovery | Preparation revoke/sync/import/budget order; failed grant re-revoke; import failure remains recoverable; operation-scoped rollback receipt survives preparation ACK | Focused race PASS; full Service uncertain rollback and actual post-COMMIT process kill PASS |
| Transport | Shared wake error redaction/category/context cancellation; committed REST 409/unavailable; MCP no-repeat hint replaces generic create/save/deploy refusal | Focused race PASS |
| Real docs | Yjs durability, quiet WebSocket pinning, PID exit/wake, cold/warm latency and memory population scaling | Corrected Linux normal and race PASS for durability, quiet WS and PID lifecycle; latency and memory scaling measured once on the earlier harness, without race, and the 127.48s real-clock case also earlier-harness only; none rerun (see below) |
| Integrated regressions | Linux package shards, race lane, docs JS tests, vet and docs checks | Corrected-lane shards, race, vet and docs-check PASS; docs JS tests, runtime soak/codec shards and runtime FILES/WS race passed only in the mixed-source initial run (see qualifications) |

## Measured results

Local macOS focused commands (serial packages):

- `go test ./internal/core ./internal/api ./internal/mcpx -run '^TestDocsIdle' -count=1 -timeout=3m -p=1`: PASS, exit 0; core 1.317s, API 0.547s, MCP 0.584s. This first run preceded the full uncertain-rollback test.
- `go test ./internal/core -run '^TestDocsIdleUncertainRollback' -count=1 -timeout=1m`: PASS, exit 0, 15.138s. Uses real Service/store/revision import with fake physical child and outbox: restore commits then loses response, old child is made unavailable, replacement preparation imports and ACKs, rollback returns revision 2 without stale-child traffic, op lookup or leaked hold. Exact file hash and no-duplicate follow-up assertions were then added for the race run.

- `go test -race ./internal/core ./internal/api ./internal/mcpx -run '^TestDocsIdle' -count=1 -timeout=3m -p=1`: PASS, exit 0; core 18.209s, API 1.696s, MCP 1.722s. Includes final exact restored file hash and no duplicate revision assertions.

Evidence: workspace reports `core-focused.log`, `core-rollback.log`, and `core-focused-race.log`. These early focused runs passed; final corrected Linux evidence is listed below. The new core store-backed tests have no integration build tag, so ordinary CI package tests execute them.

## Actual document worker evidence

The host tests use real child processes running the bundled QuickJS/Wasm/Yjs app and synthetic local documents. The earlier harness passed native correctness (15.578s) and Linux correctness, including 20 worker exit/wake cycles, quiet open WebSocket protection, lazy Service restore and an actual post-COMMIT crash. The crash test receives a successful worker restore response, kills that exact PID, substitutes 503, then verifies replacement recovery returns the right revision, exact files/hashes, no hold leak and no duplicate revision. Its Linux case passed in 16.56s.

A separate real-clock case ACKed a human update, waited 125 seconds for both revision-group maturity and the real idle threshold, confirmed child exit, reopened the document and resent the same update ID. Text and revision history remained correct (127.48s PASS). The 127.48s case, timing samples and memory scaling used the earlier host harness and were not rerun on final test source. These long tests require `FLATS_IDLE_MEASURE=1` / `FLATS_IDLE_REAL_CLOCK=1`; they are not default CI coverage. Final-source human-group durability is covered by `TestHostIdlePendingHumanGroupRefusesCleanSleep`, which waits at least 31 seconds of real worker time and passed the corrected full docs shard (normal) and corrected host-race lane. The later 20-cycle FD/goroutine/socket assertions passed in non-verbose corrected lanes; no final resource counts were retained, and the earlier measurement logs contain no such resource assertions.

Linux measurement environment: Go 1.27.1, linux/arm64, CGO-disabled crosscompiled test binaries, Docker 29.1.3/Rancher Desktop, 2 CPUs and 2 GiB container cap, network disabled, read-only source/binary mounts. This application's SQLite driver is pure Go. Baseline is exact base `54f12147a928e3e58b00bac3b120f634c1fed8db`. Candidate test binary SHA256: `2093df78e115634e9b54ff04884700ed2b4b599b5af57ff6c7435bc8236d79b1`; baseline binary SHA256: `36511dfdcca99ffb2f3e4e02aefb92f9903de47032dc1daef812f20644ce9812`. The partial `measured-source-sha256.txt` hashes eight Go product files and two host test files; it omits the JS sources, generated worker bundle, build-input manifest and MCP tools. Matching those omitted product hashes between `code-candidate-4.json` and the final manifest, together with matching measured Go hashes, supports an inference that measured product code matches the final candidate; it is not a complete source manifest recorded at measurement time. The final source commit is identified above.

| Saved docs / state | Total cgroup MiB | Parent PSS MiB | Children |
| --- | ---: | ---: | ---: |
| 1 / dormant | 60.02 | 35.98 | 0 |
| 10 / dormant | 59.56 | 28.20 | 0 |
| 100 / dormant | 84.28 | 28.89 | 0 |
| 100 / Service reopened, dormant | 96.07 | 40.53 | 0 |
| 100 / active 1 | 157.48 | 29.39 | 1 |
| 100 / active 5 | 425.77 | 30.31 | 5 |
| 100 / active 10 | 736.42 | 27.73 | 10 |

Baseline one document used 91.49 MiB cgroup with one child (49.19 MiB PSS); ten used 450.81 MiB with ten children (380.81 MiB combined child PSS). Ten dormant candidate documents used 59.56 MiB with no child: about 86.8% lower total in this synthetic comparison. Baseline 100 was not run because of resource capacity; no result is extrapolated.

Dormant 1/10/100 cgroup anonymous memory was 24.44/16.94/17.31 MiB; file memory was 31.57/34.96/53.52 MiB. Zero document processes is the achieved invariant; these sequential snapshots do not establish constant total host memory. The 100-document restore recreates Service in the same parent process and Manager, retaining parent heap/cache; it is not a fresh host-process measurement.

| Compilation cache (20 samples) | First page median / p95 / max ms | WS hello after page median / p95 / max ms |
| --- | --- | --- |
| Warm | 332.69 / 340.78 / 341.41 | 201.06 / 209.60 / 211.61 |
| Cold | 696.49 / 740.63 / 746.69 | 208.99 / 219.09 / 227.29 |

First-page latency includes actual docs child creation and complete page response, not browser rendering or editor assets. WS timing is the additional join after that page, not independently cold WS wake. Cold compilation removes only the test-owned cache after confirmed worker exit. Twenty synthetic samples are not an SLA or production forecast. Candidate includes a small attachment while baseline uses the existing text fixture. Logs: `linux-correctness.log`, `linux-baseline.log`, `linux-measurements.log`; raw sample arrays and memory decomposition: `measurement-summary.json`.

## Verification scope and remaining limits

The test source passed independent Claude review R3 after corrections; design R4, code R4 and cancellation hardening had already passed. Final-report review R3: APPROVE covers report accuracy for PR publication; it is not a CI, release or deployment sign-off. All ordinary new regressions run without custom build tags. Long real-clock cases and population/latency measurements remain opt-in.

Final corrected Linux execution uses the actual repository CI shard scripts serially in the same disposable Alpine container as the diagnostic run. The diagnostic environment log records Go 1.27.1 linux/arm64, CGO enabled with GCC 15.2.0 and Node 24.18.1 (CI uses Node 22). The execution setup used 2 CPUs / 4 GiB, read-only source/common Git/module-cache mounts and UID 501 for corrected tests, but container launch arguments and a separate corrected environment receipt were not retained; those setup details rely on the operator tool transcript, not the hashed artifacts. Linux evidence is Alpine/musl arm64 only; linux/amd64 and glibc were not tested. Frozen source/test input manifest: `corrected-source-final-inputs.json`, SHA256 `4025afbe5b8621d08757a08309dbc9669f9556a8f9c194a52b4a4568d3d1ab8f`.

| Corrected check | Result |
| --- | --- |
| Focused core/API/MCP/app, including precommit abort and permission case | PASS, 38s wall |
| Same focused cases with race detector | PASS, 58s wall |
| Runtime full shard | PASS, 83s wall |
| Managed runtime/stop/generation race | PASS, 41s wall |
| Docs soak / budget / full shards | PASS, 54 / 67 / 123s wall |
| Real host lifecycle, edits/quota, dirty drain, rollback crash race | PASS, 233s wall (230.779s test) |
| Real WebSocket beyond quota TTL | PASS, 129s wall (127.117s test); ping/pong keepalive and post-TTL edit |
| General package shard, nonroot | PASS, 205s wall |
| Full core/API race | PASS, 144s wall (126.015 / 11.238s package tests) |
| Final vet / docs-check | PASS, 5 / 1s wall |

The initial integrated run used the live worktree while test-only edits landed and had no frozen input manifest. It therefore contains mixed source versions. In particular, the general shard used the old app fixture and failed; the later app race pass likely compiled the corrected fixture. Its passing receipts must not be read as one pinned candidate run. The runtime soak and codec shards passed (108 / 34s wall); the coordinator tracked subsequent changes as test-only and outside their executed population, but the initial run was unfrozen, so this is a tracked-scope assessment rather than manifest proof; hosted CI will supply the pinned rerun. Main vet, docs-check, Python CI/image helpers, npm tests, tidy-diff and runtime FILES/WS race passed. Nested NetBird tidy-diff, vet, race, CGO-disabled trimpath release-style build and main-module dependency separation passed. Commands and exits are preserved in `run-integrated-linux.sh`, `integrated-linux-exits.tsv`, `run-corrected-linux.sh`, `corrected-linux-exits.tsv` and their per-check logs.

Initial diagnostic general/race runs failed because root bypassed permission bits in `TestSettingsSaveWithoutReadableFile`; the general shard also exposed an old app fixture lacking the trusted docs readiness protocol. The fixture was corrected in tests only, and permission-sensitive checks rerun as UID 501. Those failed receipts remain preserved and are not represented as an all-green run. The initial selected race passed app, MCP, store and portal; all these receipts share the run-wide mixed-source qualification, with app the known affected case. Integrated-only receipts (runtime soak/codec, npm/helpers/tidy, FILES/WS race, NetBird and separation) are relied upon only for unchanged executed test/product inputs, not as a pinned final-source run. Hosted CI will provide the pinned-head rerun. `corrected-source-before-runs.json` is a superseded preparation snapshot (23 paths); the coordinator confirms no tests ran against that snapshot and the runner was held until the R2 corrections and final manifest were ready. The R2/R3 review prompts’ “frozen run” wording was inaccurate for the initial live-worktree integrated run. Per the coordinator, test-source review R3 was supplied the same source/test bytes recorded in the final manifest; those bytes remained unchanged through the corrected run. The corrected runner used `corrected-source-final-inputs.json` (24 paths), and all 13 checks passed with its hashes reverified unchanged afterward.

Coverage includes exact precommit rollback preserving the old live version/hash/generation, committed readiness failure preserving content and eventual recovery, bounded lock/close/cancellation admission, single-start, all negative drain outcomes, clean-certificate sweep protection, preparation error classes and redaction, lazy maintenance/rename/delete, real fresh WS/API mutations and changed sleeping quota, actual lost-COMMIT crash recovery, and bounded 20-cycle PID/FD/goroutine/socket cleanup assertions (no final numeric counts retained). A real clean sleeper also deletes an orphan blob without waking; retention/unpin-triggered reclamation remains untested. The real pending-human-group test proves the person revision survives sleep; a background tick may import it before the final drain, so it does not prove that the drain itself performed that import.

Unperformed acceptance scenarios remain explicit: capacity-failed activation followed by restart/wake/rollback; startup with pending crash recovery and sweep; persistent import hash/blob-write failure after a real lazy restore; save during idle quiescence; stuck-child stop while other documents refresh in the same Tick; complete get/pin/quota/history/unchanged-save idle-clock census; retention/unpin physically freeing blobs; TECH validation 1's full provider-identity scenario (ordinary server on the same Manager is covered); long-run FD/goroutine/socket plateau beyond the bounded cycle test; continued real human-group recording after a `CommitPublished` failure; and a healthy sleeper preserving its changed-save label and message.

No production experiment, hosted CI, release/install or container-image smoke is claimed. First-edit latency, independently cold WS wake and browser editor-ready latency are unmeasured. Synthetic fake-provider workloads are not production forecasts.

Active worker memory is not an equivalent-workload comparison: candidate workers with open WS and loaded Yjs measured roughly 53–73 MiB PSS, while baseline eager workers without a connection measured 35.2–46.7 MiB for the ten-document run (up to 49.2 MiB including the one-document run). Active-10 total 736.42 MiB versus baseline eager-10 total 450.81 MiB establishes neither regression nor improvement for equivalent active workloads. The 86.8% reduction applies only to stored-and-idle documents. “Warm” means a new cold process with a warm compilation cache; there is no already-running-worker latency control. Approximately 200ms WS hello after a page is not attributed to wake cost.

The five documentation paths were outside the 24-path source/test freeze, so corrected-run docs-check inputs were not hash-pinned. Hosted CI on the published head will cover the committed documentation.
