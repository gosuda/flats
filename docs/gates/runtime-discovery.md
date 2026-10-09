# Runtime discovery: unfamiliar-agent evidence

On 2026-10-03 an actual fresh Claude Code 2.1.288 client, model
`claude-opus-5-5`, authored a private notebook using the connected Flats MCP.
All three client phases exited 0, with empty stderr; independent operator HTTP
verification passed seed, persistence and failed-deploy retention. This is a
bounded source-free authoring result on the candidate below. It is separate
from Go contract tests and the prepared-HTML three-client static gate.

## Exact tested identity

| Item | Value |
|---|---|
| Host commit | `44cbc69130bed5860b94eea2c66cbc42d31438ba` |
| Host tree | `dd2904acf60b8f2afe6cdbc04074b5cde0cb8e7b` |
| Native binary SHA256 | `567506621e61fa81a2b9bccf846ae0f125f72593f6756ad2e0462137421ce977` |
| Host-reported version | `dev` |
| Reference | `flats://docs/runtime-api/v1`, documentation version `1` |
| Reference text SHA256 | `9bec2dc57e4b75382062a8203824b58d7364e240dde36a354c86d41d42b6451a` |
| MCP endpoint | `http://127.0.0.1:61663/mcp` |
| Returned private site URL | `http://notebook-gate.localhost:61664` |

The operator's initialization probe advertised the resource and fallback tool;
the client actually called `get_runtime_reference({})` before authoring. The
returned text matched the embedded reference hash. Resource reading itself was
covered by protocol tests, not exercised by this client.

## Inputs and enforced restrictions

The fresh working directory held only the ordinary request and MCP configuration,
with no prepared app or source checkout in the client directory. The system prompt was: “You are a coding
assistant helping build the requested app using the connected service.” The
initial request was:

> Build and deploy a small private notebook on the connected Flats host, slug notebook-gate. It should have a simple HTML home page and durable structured notes (SQLite) plus durable text attachments (file storage), so they survive a new deployment.
> For integration with my other tools provide these JSON HTTP endpoints: POST /api/notes accepts {"title":"...","body":"..."} and returns the created note {"id":...,"title":"...","body":"..."}; GET /api/notes returns an array of notes. PUT /api/attachments/&lt;key&gt; accepts a text body, GET the same path returns that text or 404 when absent, DELETE the same path removes it, and GET /api/attachments returns an array of keys. Use simple attachment names. Include a side-effect-free GET /healthz.
> Discover the host's supported APIs from the connected MCP server, author the application yourself, and deploy it. Keep it private. Report the returned site URL, live version, documentation version you used and any remaining uncertainty. Another operator will test your endpoints after you finish.

A sole Streamable HTTP Flats MCP was configured. Every phase used these options:

```sh
claude -p "$REQUEST" \
  --system-prompt 'You are a coding assistant helping build the requested app using the connected service.' \
  --tools '' --restricted --disable-slash-commands --setting-sources '' \
  --strict-mcp-config --mcp-config mcp.json \
  --disallowedTools mcp__flats__save_version_from_dir \
  --allowedTools 'mcp__flats__get_runtime_reference,mcp__flats__save_version,mcp__flats__deploy,mcp__flats__get_flat,mcp__flats__list_flats,mcp__flats__list_versions,mcp__flats__get_logs' \
  --permission-mode dontAsk --output-format stream-json --verbose
```

The latter two invocations resumed the newly created session with identical
restrictions. Initialization in all three phases exposed only these Flats MCP
tools (the permission allowlist above is not the complete exposed inventory):
`create_flat`, `delete_flat`, `deploy`, `get_approval`, `get_flat`, `get_logs`,
`get_runtime_reference`, `list_flats`, `list_secrets`, `list_versions`,
`open_preview`, `rollback`, `save_version`, `set_visibility`.
`save_version_from_dir` was absent. Native shell/file/browser/Agent tools,
skills and slash commands were absent. Although initialization listed agent
type metadata, result receipts reported zero spawned subagents in every phase.
Three builtin plugins were disclosed: `cc-plugin-agents-md`,
`cc-plugin-telemetry`, `cc-plugin-plugin-authoring`; no extra plugin was installed.

This establishes client tool-surface isolation, not an OS filesystem sandbox
or a plugin-free client. Provenance records the supplied prompt/configuration
and effective inventories; it does not establish visibility into every internal
provider instruction. No native or other-server tool call appears in the
transcripts. Inline payloads exactly matched the saved version files, and
review confirmed synchronous DB query/exec and FILES put/get/list/delete use.
The failing candidate performs no storage writes at module load or on health.

## Actions and independently observed results

| Phase (UTC) | Actual client actions | Operator result |
|---|---|---|
| Author, 12:03:10–12:03:50 | Reference read; inline `save_version(deploy:true)`; `get_flat`; `get_logs` | v1 private/ready; health 200; seed PASS |
| Redeploy, 12:04:19–12:04:47 | Complete inline `save_version(deploy:true)`; `get_flat` | v2 private/ready; health 200; persistence PASS |
| Failure, 12:05:07–12:05:37 | Inline `save_version(deploy:false)`; `deploy(version:3)`; `get_flat`; `list_versions`; `get_logs` | MCP health error 503; v3 saved but not live; v2 retained; failed PASS |

The verifier obtained the site from the host's returned private URL each time;
it did not assume a site URL. On v1 it checked home and `/healthz` HTTP 200,
created a random structured note via POST (2xx), and retrieved the exact object
via GET (200). It PUT a random Unicode text attachment (2xx), GET checked exact
text (200), PUT a second attachment, listed both keys (200), DELETE removed the
second (2xx), GET confirmed 404, and listing kept only the surviving key.
The receipts retain PASS outcomes and assertions, not a full per-request status
trace; exact 201/204 write statuses come from the authored code rather than a
recorded HTTP trace.

After v2, the same URL returned HTTP 200 with “Notebook revision two”, the
original note object, and identical Unicode attachment text. After rejected v3,
the same URL still served v2, the homepage was byte-for-byte unchanged, and the
note and attachment remained intact. Version records preserved v3; the health
log recorded `GET /healthz -> 503` for it, and no v3 live-deploy event appeared.
This side-effect-free failure does not claim that arbitrary failed candidates
cannot mutate live DB/FILES storage.

Authored payload SHA256 (UTF-8 bytes; manifest identical in all three versions):

| File | v1 | v2 | rejected v3 |
|---|---|---|---|
| `flats.json` | `ca2344c7307f04f58dca51c031a82ca5d1a1c592aeef4abc5315513dd51376d2` | same | same |
| `server.js` | `23b43a8e2e32499a5416749b9f578333c96f43c12f505615824957ad7ebb3fe9` | `94588ee4ebf848af5eec3e853472931e86a25fc7a6bff46c60f5004c1af76b21` | `e39ea1c98ce537d0ba8ae7352d425c257829ec261ebdb064e4c72b450a2ae18c` |

Manual interventions were host/configuration preparation, the initial ordinary
request, two ordinary continuation requests, and independent HTTP verification
after each phase. No operator app edit, repair prompt, retry or endpoint hint was
needed. The continuation requests were:

> Update notebook-gate's home page to say "Notebook revision two" and redeploy the complete app as a new version. Preserve existing notes and attachments, keep the same endpoints, and keep it private. Report the returned URL and live version.

> Demonstrate that a rejected release leaves notebook-gate serving its working version: save a complete candidate whose health endpoint deliberately returns HTTP 503, attempt to deploy it, and report the health failure and which version remains live. Keep its startup and health path free of storage mutations, and keep it private.

## Reproduction and scope

Build the identified commit with `CGO_ENABLED=0 go build -o flats ./cmd/flats`;
record binary hash because toolchain/build differences can change it. Start a
host with fresh disposable data (`GATE_DATA` names the new directory):

```sh
./flats serve --data "$GATE_DATA" --listen 127.0.0.1:61663 \
  --network local --local-addr 127.0.0.1:61664 --portal=false
```

In a fresh source-free client directory, use this `mcp.json`:

```json
{"mcpServers":{"flats":{"type":"http","url":"http://127.0.0.1:61663/mcp"}}}
```

Use the exact ordinary
requests and restrictions above, retain each initialization inventory and result,
and independently run the HTTP assertion sequence above between phases. Record
returned URLs, live versions, reference/payload hashes, health logs, all exits
and interventions. Shut down only the owned host and preserve evidence.

The test establishes this model/client's authoring and runtime storage usability
on the identified candidate, with operator HTTP verification. It does not test
browser interactions, actual tailnet access, public exposure, service recovery,
other clients or OS isolation. Accepted-main integration, final frozen-binary
revalidation of the existing generated app, and independent review must be
recorded separately before final acceptance; this historical run cannot certify
a later binary merely because its reference text remains identical.


## Integrated frozen-binary revalidation

On 2026-10-03, after integration onto the explicitly authorized merged main,
the operator restarted the existing test host with the frozen integrated binary
against the same retained data. This checks the historical generated app on the
final binary; it is separate from the unfamiliar-agent authoring run above.

| Item | Value |
|---|---|
| Authorized merged-main base | `313ef2557d28350cb91a0daff719aca5bf6813c9` |
| Base tree | `c7ff38c3cfa5fe5a2c4c683707368adae66e3f1c` |
| Binary source commit | `e3ab93f0c6df28fbb9e36915c7f32fc73343009f` |
| Binary source tree | `df49ff3fda26e16fbefbd9333546b28af7a61399` |
| Native binary SHA256 | `1aa3100e773f698974efaee3dd0ae81f0ae8a2d3191ba8af7c829b9a2fea5163` |
| Served reference text SHA256 | `9bec2dc57e4b75382062a8203824b58d7364e240dde36a354c86d41d42b6451a` |
| Documentation version | `1` |

The two issue-9 commits rebased without conflicts from their historical base;
range comparison showed unchanged patches. The independent base fixes and
console files were retained, and runtime API v1 bytes did not change. Full Go
tests and vet, core/API/MCP/runtime/portal races, focused reference/limits/FILES
concurrency/WASI checks, static/API/CLI/app/console tests, module tidy with zero
module diff, module verification, native and CGO-free Linux amd64/arm64 builds,
and the real-binary server gate all passed. The server gate used separate data
and disposable ports. Build metadata recorded the binary source commit above,
`vcs.modified=false`, Go 1.27.1 and `CGO_ENABLED=0` on darwin/arm64.

The operator verified the old test process identity, stopped only that process,
confirmed exit and released ports, and launched the exact hashed binary on the
same data and ports. The separate restart and HTTP verification receipts record:

- The existing private notebook restored live v2, with saved rejected v3 retained.
- The same returned site URL served HTTP 200, the unchanged revision-two homepage,
  the original SQLite note and the exact Unicode FILES attachment.
- Persistence and failed-version retention checks both passed after restart.
- MCP returned documentation version 1 and the exact reference hash above.

The retained v3 health-503 log belongs to the historical failed deployment.
This restart check did not perform another failed deployment or fresh-agent run;
it does not extend the historical side-effect-free failure result to arbitrary
candidate storage mutations. Receipt copies were kept separately from the
historical observations. The documentation append itself follows the binary
source commit above and does not identify a newly built binary.

The coordinator reported main CI run `37122887675` passed on the exact authorized
base. That merged tree also contains an unrelated three-file console change;
independent review of that base delta and final issue-9 source acceptance were
still pending when this revalidation was recorded. These results complete local
integration and final-binary restart validation, with final acceptance remaining
subject to those independent reviews.

## Corrected-reference authoring and final-contract revalidation

The reference corrections describe existing behavior: host filename limits and
I/O errors, filesystem-dependent case/Unicode identity, compound DB JSON encoding,
list cutoff order, preview settings, WASI secrets and absent platform globals.
The historical identities and outcomes above remain unchanged.

A second fresh Claude Code 2.1.288 client (`claude-opus-5-5`) authored
`notebook-final` using the same ordinary notebook request (with the new slug)
and MCP-only restrictions above. The host already held the original test app;
there was no prepared app or source checkout in the new client directory.
The client read documentation version 1 through `get_runtime_reference`.

| Item | Corrected-reference authoring run |
|---|---|
| Binary source commit | `a0dde30239b755e56a15c4ba80ed7fad1df465a3` |
| Binary source tree | `c853d01bb0b300bbe431275d93cb863dbe80f763` |
| Native binary SHA256 | `e6612222e5e8a5e20a56e235d4acfa7db45f0be23e065174929b84253bfca40b` |
| Read reference SHA256 | `86196d7454698ca554083d6330a24b42c254fbeb1961b8d906fc8ec06ca2e960` |
| Returned private URL | `http://notebook-final.localhost:61664` |

This was **not a first-pass success**. The authored v1 attachment PUT wrote data
then returned HTTP 500 because the app used unsupported `TextEncoder`. An
ordinary v2 redeploy ran prematurely before successful seed verification; that
operator sequencing error and its transcript were retained separately. The
operator then supplied only the observed HTTP failure and asked the client to
investigate host diagnostics/public documentation and repair its own app.
No API hints, replacement code or operator app edits were supplied. The client
read logs and deployed its repair as v3; independent seed verification passed.

An ordinary request for the distinct heading “Notebook revision final” produced
v4; independent persistence verification passed. A subsequent complete v5
candidate deliberately returned health HTTP 503 without startup/health storage
mutations. Deployment was rejected, v5 remained saved, and the same URL retained
v4, its byte-identical homepage, exact SQLite note and Unicode FILES attachment.
Independent failed-deploy retention verification passed. All five CLI phases
(author, premature redeploy, repair, final redeploy, failure) exited 0. Native
and source tools, skills and slash commands were absent; the three builtin
plugins were disclosed, with zero spawned subagents. These are tool-surface
restrictions, not OS isolation. The repair feedback is an explicit intervention.

The operator subsequently restarted the same retained data with the final
contract binary, separately from that fresh-client run:

| Item | Final contract binary |
|---|---|
| Authorized base | `313ef2557d28350cb91a0daff719aca5bf6813c9` |
| Binary source commit | `f57c70f7362d39b195b7db14b58b4f55b781a8ff` |
| Binary source tree | `b4540d1c7994151ab9f1fc43d85c1c133827c32d` |
| Native binary SHA256 | `d90c6d74fd5e4f4354133b16098954e3f3bb2796399370dd2e9e3d895397d74c` |
| Served reference SHA256 | `ed795e74b20061a0bb73127ed54e1c76ee1d0d8f0765a945096824693718b78d` |
| Documentation version | `1` |

Build metadata records the source commit above, `vcs.modified=false`, Go 1.27.1,
`CGO_ENABLED=0`, darwin/arm64. Full tests, vet, core/API/app/MCP/runtime/store/
expose/docs races, static/API/CLI/console checks, module tidy with no diff, module
verification, native and Linux amd64/arm64 cross-builds, and the real-binary
server gate passed on that source. Section-scoped documentation checks and
executable filename, case/normalization, JSON, list and absent-global tests
passed; deliberate documentation mutations were rejected by the checks.

After verifying and stopping only the prior owned test process, the operator
confirmed exit and restarted the exact hashed binary on the same data/ports.
MCP text matched the final reference hash. Independent persistence and retained
failure checks passed for both apps: original `notebook-gate` live v2/saved v3
at `http://notebook-gate.localhost:61664`, and new `notebook-final` live v4/rejected
v5 at its URL above. Both kept their exact pages, SQLite notes and Unicode files.
Those restart checks performed no new failed deployment or fresh authoring on
the final reference. The new authoring run remains bound to its earlier hash.

The coordinator classifies unchanged static product plus executed static/API/MCP
tests and historical three-client evidence as continued static gates; no new
three-client prepared-static script run is claimed. This evidence-only appendix
follows the final binary source and does not identify a rebuilt binary. Exact
final-head independent review remains required before acceptance.

## Guide-route authoring run (PR #35)

On 2026-10-09 (UTC) a fresh restricted client authored, published, changed
and deliberately broke a private notebook using only the MCP `guide` tool and
the action tools. No skill was installed, and the client never called
`get_runtime_reference`. Every step of the reproduction procedure in
[gates.md](../gates.md) passed.

This host has operator approvals, so publication waits for the operator. The
tested client never approved anything. The root operator approved each request
in the disposable host's console, under the user's standing authorization for
development hosts. Each approval is listed below as an operator intervention.

### Identity

| Item | Value |
|---|---|
| Host commit | `956cedff17bee47930e0bb45f541cce867e5fdcb` (PR #35 head) |
| Host tree | `4a617c8418ef3e903c1a8fe7624927abdafa8534` |
| `docs/agent` tree | `2ebb03ade2f0b2953360c93b41f377982fa0a450` |
| Native binary SHA256 | `288934474fa2d076966a7596d32147a3288325f24183e20fbb47781e09f120ff` (Go 1.27.1, `CGO_ENABLED=0`, darwin/arm64) |
| Host-reported version | `956cedf` |
| MCP instructions | 1,932 bytes, SHA256 `0661f60e63e67fb84f5bcadf01711a601d68a989a46b886bdb1d10884e35ff14` |
| Reference | `flats://docs/runtime-api/v1`, documentation version `1`, SHA256 `10a0521e9814c66768039c51c7a4ca6f059d03467ae79ac3b60efa50b9e48ddf` (served text equals the repository file) |
| Client | Claude Code 2.1.296, model `claude-opus-5-5`, one session resumed across five invocations |
| Host | Fresh synthetic data directory, local network only, Portal off, runtime on, loopback management/site ports 17991/17992 |

### Inputs and restrictions

The client directory held only the MCP configuration and the request texts.
Every invocation used the options of the earlier run:

* `--tools ''`, `--restricted`, `--disable-slash-commands`, `--setting-sources ''`;
* `--strict-mcp-config` with only the Flats Streamable HTTP MCP;
* `--disallowedTools mcp__flats__save_version_from_dir`, `--allowedTools mcp__flats`;
* `--permission-mode dontAsk`.

The system prompt was also unchanged. Initialization reported:

* 25 Flats tools including `guide`, and no `save_version_from_dir`;
* no native tools, skills or slash commands;
* the builtin plugins `cc-plugin-agents-md`, `cc-plugin-telemetry` and `cc-plugin-plugin-authoring`.

No subagent was spawned. All five invocations exited 0 with empty stderr.

The first request was the original notebook request, with slug
`notebook-gate`. Its last sentence asks the client to say what to approve and
stop. Later requests:

1. "I approved it. Confirm the live version and report the site URL I should test."
2. Add `GET /api/stats`, keep all endpoints and data, deploy.
3. Deploy a deliberately unhealthy candidate whose `/healthz` returns 503, with
   side-effect-free startup and health.
4. "I approved it. Report what the host did."

No API hints, code or repairs were supplied.

| Request text | SHA256 |
|---|---|
| Initial | `dcfdd6c72e10143f88c83e8d0069bfd7fb3a7ec3742ad669b5efb468ae8a86be` |
| Change | `fefe6b2f091bc50cf7a3af69b93083319e43b973ed5173941386cac392fdd39f` |
| Unhealthy | `4845a2592e14e324e54db24f168f63332f9ff83fc7065ed8834507263408bf1e` |

### Client actions

| Phase | Tool calls |
|---|---|
| Author | `guide` (index, server, server.files, server.db, approvals, preview-verify, manifest); `get_flat` (`not_found` with `See guide refusal.not_found.`); `guide` (server.limits, rollback-data); `save_version` (`expected_revision` 0); `open_preview` 0; `publish`; `get_logs`; `get_approval`. It stopped with the approval link. |
| Confirm | `get_approval`, `get_flat`, `get_logs`; reported the returned URL `http://notebook-gate.localhost:17992` |
| Change | `get_draft`, `save_version` (`expected_revision` 1), `open_preview` 0, `publish`, `get_logs`; stopped with the approval link |
| Unhealthy | `get_approval`, `get_flat`, `save_version` (`expected_revision` 2), `publish`; stopped with the approval link |
| Report | `get_approval`, `get_flat`, `list_versions`, `get_logs` |

Across all phases the client called only `guide`, `get_flat`, `get_draft`,
`save_version`, `open_preview`, `publish`, `get_approval`, `get_logs` and
`list_versions`.

| Authored file | Bytes | SHA256 |
|---|---|---|
| `flats.json` (all three revisions) | 116 | `2f38838c6a3e6b0d8ab1da13325464463ceae7fb41bc6ede3b39e5c79dc076be` |
| `server.js` revision 1 (v1) | 6,188 | `119eaf41536fd0c4681b3a97384363eff7b21bccd51f3b00c3f8a6c0e186f70a` |
| `server.js` revision 2 (v2) | 6,812 | `c7bb92ec3d911d13e9c926b8e48f5d99b51cc518438699c9bcb1dc83561226fc` |
| `server.js` revision 3 (unhealthy) | 7,023 | `7ffaa21bf0c21c4f25226fd3b04e300ec20ef665c137079581ee8e0587b380a3` |

The revision 1 handler calls `env.FILES.get`, `put`, `delete` and `list`, and
`env.DB.query` and `exec`.

### Operator interventions

| UTC | Approval | Action | Result |
|---|---|---|---|
| 22:28:25 | `apr-bwcbm9ep9lvz` | publish Draft revision 1 | `approved`: v1 live; `data_impact` `runtime_start`, `health_data` `isolated_copy` |
| 22:32:12 | `apr-jgl818dlydml` | publish Draft revision 2 | `approved`: v2 live |
| 22:35:16 | `apr-bgfxxu7muixb` | publish unhealthy Draft revision 3 | `failed`: `failure_code` `health_check_failed`, `data_impact` `none`, `live_data` `untouched` |

There were no other operator actions, repair prompts or retries.

### Independent HTTP observations

All requests went to the returned URL through the loopback site port, with
`Host: notebook-gate.localhost:17992`.

1. **v1.**
   * `/healthz` returned 200; `/` returned 200 HTML.
   * Notes: POST returned 201 with the Unicode note; GET listed it.
   * Attachments: two PUTs returned 201, GET returned the text, the list showed both keys, DELETE returned 200, and a later GET returned 404 with the list shrinking to one key.
2. **Seed.** A random-suffix note and a FILES attachment were added through the API.
3. **v2.**
   * `/api/stats` returned `{"notes":2,"attachments":2}`.
   * Both notes, the seeded attachment's exact text and the original attachment were still there.
4. **Unhealthy candidate.** After the failed approval:
   * the host still served v2: `/healthz` returned 200 and `/api/stats` was unchanged;
   * notes and the seeded attachment were intact;
   * `live_version` was 2 with two published versions;
   * the unhealthy code remained the Current Draft (revision 3, hash `3234529f…`);
   * the log recorded `GET /healthz -> 503` for the candidate.

   The client's MCP report matched this.

### Transcripts

The raw transcripts stay in private local storage, not in this repository.

| Phase | SHA256 |
|---|---|
| Author | `fdbb77abc85cc381217fc30b4bd274e1ff7ef34608fcef95b4607f3a89c6e1cd` |
| Confirm | `31abde0f8fd52e1db6ed9c80e7d9d9305a338be5ee79fb5807445a65643957b5` |
| Change | `b2fc89e08c86406d3b0e30caa3a690ae580c7d72f42867549d9c754cfee3dad5` |
| Unhealthy | `86f7e719e73aa6de3093603d9fe84cb15ee954419bdca5e712756d71959c9530` |
| Report | `185a59fe12a394a787125d4e8cce3adab7a3ff3023e3608177dbb3f5c9a3f22f` |

The operator stopped only the owned test host process. Its listeners were gone
afterwards.

### Observations outside this PR

* Every worker start logs `flats: ignoring the saved connection: $HOME is not defined` at error level. This is the inherited worker warning from #30, and the client reported it as unexplained noise.
* A failed Draft publication is reported as "deploy of version 0". The client correctly read 0 as the Draft.
* The pre-deploy snapshot event names only the `.sqlite` file, although current snapshots also hold FILES in a sidecar. The client flagged that it could not confirm FILES were captured.
