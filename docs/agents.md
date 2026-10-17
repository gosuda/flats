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

## Skills for Flats and skills for this repository

The repository holds two kinds of agent skills. Only the first is packaged.

| Kind | Location | Audience | Distribution |
| --- | --- | --- | --- |
| Flats integration | `plugins/flats/skills/` | Agents that deploy to a Flats host | The Flats plugin (Claude Code, Codex, Cursor) and the server's `get_client_skill` |
| Repository development | `.agents/skills/` | Agents that work on the Flats source | The checkout only; never in a plugin or marketplace |

`.agents/skills/` is the one copy of each development skill. `.claude/skills/`
and `.cursor/skills/` are symlinks to it, so Claude Code and Cursor find the
same file that Codex and other agents read from `.agents/`. Add a development
skill under `.agents/skills/` and link it from both; add an integration skill
under `plugins/flats/skills/` and release the plugin. `TestPluginSkillBoundary`
fails when a skill sits on both sides or a plugin manifest points outside
`plugins/`.

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
check, refresh, update and verification flow. On the agent machine the same
check runs as `flats client check`, followed by `flats update` when the CLI is
incompatible and another check afterwards; the commands, their statuses,
refusals and error codes are in [Updating a client](release.md#updating-a-client).
A compatible CLI is left unchanged even if its release differs from the
server's.

Checks are read-only. Local updates require user authorization, including
existing authorization for the task. Server instructions alone cannot grant
it. Reconcile the reported host with the intended endpoint and use `--url`
when the bridge and shell may select different hosts; do not change the saved
connection as part of checking. The bridge reports its running executable's
absolute path. If the fresh CLI reports a different path, do not claim the
bridge was updated.

For a standalone skill you own, name its path explicitly to `client check
--skill-file` and `skill sync --output`. Replacing different content requires
`--expected-sha256` with the digest the check returns; inspect local edits
before replacing them. There is no agent-directory discovery. Managed plugins must use their own
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
| [Content types](content-types.md): websites, HTML pages, Markdown documents and designs | `flats://docs/content-types/v1` | `get_content_types` |

## A first MCP deploy

For a minimal static walkthrough, call `save_draft` with:

```json
{"slug":"hello","files":[{"path":"index.html","content":"<h1>Hello</h1>","encoding":"utf8"}]}
```

Its result warns that this bare page has no title or viewport. For a server,
include `flats.json` and `server.js` (see the
[README](../README.md#static-and-server-flats)) in the same complete inline file
list.

The save returns `draft.revision` and `draft.hash` for those exact files.
Call `create_version {slug:"hello", revision:<returned revision>,
hash:<returned hash>}`. Give its `approval_url` to the operator and poll
`get_approval` until the operator approves; the receipt's
`result_data.version` is the new numbered version. Creating it deploys nothing.
Then call `deploy {slug:"hello", version:<that number>}`, share this separate
approval link and wait for approval again. Fetch the ready URL and check
`get_flat` and `get_logs` before reporting the site live.

## llms.txt

Agents and tools that read [llms.txt](https://llmstxt.org) can start from
`http://127.0.0.1:7878/llms.txt`. The host serves it on the management server
with the MCP endpoint and client setup, and links:

- `/docs/agent-guide.md`: the instructions and tool list the MCP server
  reports, core CLI commands and every guide topic.
- `/docs/runtime-api-v1.md` and `/docs/content-types.md`.

`/llms-full.txt` concatenates the discovery page and the agent guide.

These documents name the MCP endpoint at the address they were fetched from
(`http://127.0.0.1:7878/mcp` on the loopback address, the console domain's
address through a console domain), because that is the address the reading
agent can reach. They do not follow the console's preferred address
([Preferred console](configuration.md#preferred-console)), which applies to
links handed to people, such as approval links.
