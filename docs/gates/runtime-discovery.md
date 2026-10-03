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
