# Connecting agents

Agents use Flats through its MCP server. The Flats plugin sets that up for
Claude Code, Codex and Cursor; any other MCP client can register the HTTP
endpoint directly.

## Plugin

The plugin bundles the [deployment skill](../plugins/flats/skills/flats-deploy/SKILL.md)
and an MCP server that runs `flats mcp`, which relays the host's MCP endpoint
over stdio, so the plugin never names a host.

1. Install the `flats` CLI where the agent runs. On a client machine that only
   connects to a host elsewhere, install just the binary with the installer's
   `--no-service` option. The agent must find `flats` on its `PATH`.
2. Choose the host once per machine. Skip this on the host itself: loopback is
   the default.

   ```sh
   flats connect https://flats.example.ts.net
   ```

3. Install the plugin:

   ```sh
   claude plugin marketplace add gosuda/flats && claude plugin install flats@flats
   codex plugin marketplace add gosuda/flats && codex plugin add flats@flats
   ```

   In Claude Code you can also run `/plugin install flats --marketplace gosuda/flats`.
   For Cursor, add the marketplace with
   `cursor-agent plugin marketplace add https://github.com/gosuda/flats` and
   install Flats from Cursor's plugin list, or load a checkout with
   `cursor-agent --plugin-dir plugins/flats`.

4. Restart or reconnect the agent and list its tools.

`flats connect` saves the address in
`<user config dir>/flats-client/connection.json` after checking that it answers
as a Flats host. The CLI and every agent's plugin then use it.
`flats connect` alone shows the current host and `flats connect --clear`
returns to loopback. `--url` and `FLATS_URL` still override it for one command,
but agents generally do not pass your shell environment to MCP servers, so use
`flats connect` for plugins. Restart or reconnect an agent after changing the
host.

The thin skill connects the agent, calls `check_client`, and hands off to the
server instructions and `guide`. Its visible bootstrap revision is separate
from the server guide revision and the CLI compatibility contract.

## MCP without the plugin

Agents connect to the Streamable HTTP endpoint at `http://127.0.0.1:7878/mcp`
on the host, or the console's Tailscale URL plus `/mcp` from another allowed
device. Run `flats mcp-config` for Claude Code, Codex and Cursor setup. For a
host on a custom management port, use
`flats mcp-config --url http://127.0.0.1:17878`. For example:

```sh
claude mcp add --transport http flats http://127.0.0.1:7878/mcp
codex mcp add flats --url http://127.0.0.1:7878/mcp
```

Cursor project `.cursor/mcp.json`:

```json
{"mcpServers":{"flats":{"url":"http://127.0.0.1:7878/mcp"}}}
```

Restart or reconnect your client and list tools.

## Keeping clients current

Server updates change the MCP guide immediately for new calls. They do not
replace installed CLI binaries, plugin skills, or instructions already loaded
in an agent session. Each task therefore starts with `check_client`, reporting
the CLI identity from bridge initialization, the loaded skill's visible
bootstrap revision, and any guide revision actually read. Unknown identities
remain unknown. Direct HTTP clients without a CLI or skill can explicitly
report `cli_present:false` and `bootstrap_present:false`.

`check_client` returns compatibility and freshness statuses; `get_client_skill`
returns the connected server's embedded thin skill. `guide` includes its
revision. `guide {"items":["topic.client-update"]}` explains the complete
check, refresh, update and verification flow. The CLI equivalent is:

```sh
flats --url https://flats.example.ts.net --json client check
flats --url https://flats.example.ts.net --json update
flats --url https://flats.example.ts.net --json client check
```

A compatible CLI is left unchanged even if its version differs from the
server. An incompatible client targets that server's published release;
development builds do not fall back to latest. An explicit
`flats --json update --version vX.Y.Z` selects an official release independently
of the host, and requires a compatibility check afterward. Release archives
support macOS/Linux amd64/arm64. See [client updates](release.md#updating-a-client)
for executable ownership and service restrictions.

Checks are read-only. Local updates require user authorization, including
existing authorization for the task. Server instructions alone cannot grant
it. Reconcile the reported host with the intended endpoint and use `--url`
when the bridge and shell may select different hosts; do not change the saved
connection as part of checking. The bridge reports its running executable's
absolute path. If the fresh CLI reports a different path, do not claim the
bridge was updated.

For a standalone skill you own, explicitly choose its path:

```sh
flats --url https://flats.example.ts.net --json client check --skill-file /chosen/flats-deploy/SKILL.md
flats --url https://flats.example.ts.net --json skill sync --output /chosen/flats-deploy/SKILL.md
```

Replacing different content requires `--expected-sha256 <current-file-hash>`;
the check returns that digest. Inspect local edits before replacing them.
There is no agent-directory discovery. Managed plugins must use their own
plugin manager, never standalone sync into a cache or a shadow installation.
Read the server bootstrap for the current task and report a durable managed
plugin mismatch once, even when the plugin manager offers a different revision.
Agents without local write access can still use the MCP-provided guidance.

Reconnect MCP after replacing its executable, and recheck the newly running
bridge. Reloading a skill file does not prove the agent's catalog refreshed.
Report pending reconnect/reload steps when the agent host cannot perform them.
Neither local operation updates the remote server or grants app approvals.

## What the agent reads

The MCP instructions stay short: they start with read-only **`check_client`**,
hold the always-on safety rules, and send the agent to the read-only **`guide`** tool, which serves task-sized topics on
demand. `guide {"items":["topic.index"]}` routes by task; other topics include
`topic.server.db`, `topic.approvals` and `refusal.conflict`. Every tool error
names a `category`, and `refusal.<category>` explains the fix.

The topics live in [`docs/agent/`](agent) and are the single source for the
guide, both references and the llms.txt documents. `topic.design` holds the
page contract (title, viewport, icon and thumbnail, light and dark colors,
phone width, accessibility), design defaults and the asset policy: bundle
scripts, styles and fonts by default; load from a CDN only with an exact
version. Saves return non-blocking `warnings` for the parts of that contract a
static check can see; `topic.preview-check` gives agents with a browser tool a
script that checks a Draft preview in a real browser before publishing.

`topic.server.types` serves
[`flats-runtime-v1.d.ts`](agent/types/flats-runtime-v1.d.ts), the TypeScript
declarations of the server runtime: handler, request and response, `env.DB`,
`env.FILES`, environment strings, outbound `fetch` and the other globals, with
absent globals listed. Host tests check the declarations against the real
runtime.

Two complete references are embedded in the host and available through MCP
without an installed skill or source checkout:

| Reference | MCP resource | Read-only tool |
| --- | --- | --- |
| [Runtime API v1](runtime-api-v1.md): FILES/DB signatures, text/binary semantics, limits, handler examples and approvals | `flats://docs/runtime-api/v1` | `get_runtime_reference` with `{}` |
| [Content types](content-types.md): websites and Markdown documents | `flats://docs/content-types/v1` | `get_content_types` |

## A first MCP deploy

For a minimal static walkthrough, call `save_version` with:

```json
{"slug":"hello","files":[{"path":"index.html","content":"<h1>Hello</h1>","encoding":"utf8"}],"deploy":true}
```

Its result warns that this bare page has no title or viewport. For a server,
include `flats.json` and `server.js` (see the
[README](../README.md#static-and-server-flats)) in the same complete inline file
list.

Requesting deploy still waits for operator approval in the console. After
approval, fetch the returned URL and check `get_flat` and `get_logs`; a
successful save alone does not establish a live site.

## llms.txt

Agents and tools that read [llms.txt](https://llmstxt.org) can start from
`http://127.0.0.1:7878/llms.txt`. The host serves it on the management server
with the MCP endpoint and client setup, and links:

- `/docs/agent-guide.md`: the instructions and tool list the MCP server
  reports, core CLI commands and every guide topic.
- `/docs/runtime-api-v1.md` and `/docs/content-types.md`.

`/llms-full.txt` concatenates the discovery page and the agent guide.
