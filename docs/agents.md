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
   the default. This chooses which host the CLI and the plugin talk to. It does
   not register an agent.

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

## Use Flats for documents

Connecting an agent does not make Flats the place where it creates documents.
In Claude Code the connector `claude.ai Claude Docs` is available with no setup,
and the synced skill `anthropic-skills:docs` sends document requests to it. The
Flats skill is chosen only when the user names Flats. A line in `CLAUDE.md` does
not settle this alone. Claude Code reads `CLAUDE.md` as context, not as enforced
configuration, so it may follow either of two instructions that disagree.

`flats agent-setup docs` is an opt-in command that makes Flats the document tool
of the agents you select. For Claude Code it removes the competing route first
and then adds an instruction that breaks the tie. Codex and Cursor have no known
competing route, so for them it adds the instruction only. Run it on the machine
where the agent runs.

```sh
flats agent-setup docs [--agent claude|codex|cursor|all] [--scope user|project] [--dry-run] [--yes] [--undo]
```

| Flag | Effect |
| --- | --- |
| `--agent claude\|codex\|cursor\|all` | The agents to set up. At user scope the default is every agent whose user directory exists. At project scope the flag is required when setting up, so a repository gets files only for the agents its owners name. With `--undo` the default is `all`. An agent you name is set up even if its directory is missing. |
| `--scope user\|project` | `user` (the default) targets the agent's own configuration directory. `project` targets files in the current repository. |
| `--dry-run` | Prints the plan and stops, even with `--yes`. |
| `--yes` | Applies the plan without asking. |
| `--undo` | Reverses an earlier setup. |

The project is the nearest directory, starting at the current one and going up,
that holds a `.git` entry. If there is none, the project is the current
directory and the plan says that it found no repository. The command refuses a
project that is your home directory. Run it inside a repository, or use user
scope.

| Agent | User scope | Project scope |
| --- | --- | --- |
| Claude Code | A block in `<claude dir>/CLAUDE.md` and three entries in `<claude dir>/settings.json` | A block in `<project>/CLAUDE.md` |
| Codex | A block in `<codex dir>/AGENTS.override.md` if that file exists, else in `<codex dir>/AGENTS.md` | A block in `<project>/AGENTS.md` |
| Cursor | Prints the routing text for Cursor Settings, Rules, User Rules and writes nothing | The file `<project>/.cursor/rules/flats-documents.mdc` |

`<claude dir>` is `$CLAUDE_CONFIG_DIR` or `~/.claude`, `<codex dir>` is
`$CODEX_HOME` or `~/.codex`, and Cursor's user directory is `~/.cursor`.

Every target gets the same routing text, at most 800 bytes and with no host name.
The text covers a document people will read, share or edit afterwards. It tells
the agent to create such a document with the Flats MCP tools (`create_flat` with
type `docs`, then `save_document`) after `check_client` and the guide, and to
tell you instead of using another document tool when Flats is not connected. A
block starts with the line `<!-- flats:documents:begin -->` and ends with
`<!-- flats:documents:end -->`. The command appends a block to a file that has
none and replaces it in place when its text differs. Bytes outside the markers
do not change, except the newline and blank line that separate an appended block
from the text before it. The Cursor rule file is different. The command owns it,
so a new file holds a short front matter that sets `alwaysApply: true`, then the
block. Codex reads at most 32 KiB of `AGENTS.md` by default, so the command
refuses to add or replace a Codex block when the resulting file would be larger
than 32768 bytes.

For Claude Code at user scope the command also adds three entries to
`settings.json`: `{"serverName": "claude.ai Claude Docs"}` in `deniedMcpServers`,
and `Skill(anthropic-skills:docs)` and `mcp__claude_ai_Claude_Docs__*` in
`permissions.deny`. It inserts text and leaves every other byte alone, so the
file keeps its order, indentation and line endings. It refuses to write unless
the result is the original plus exactly these entries. It also refuses invalid
JSON (comments included), a wrong type for a key it edits, and a key that
appears twice. The command skips an entry that is already there, and the plan
says that it was already there and that `--undo` will remove it. Project scope never edits a settings file,
because those files are checked in and shared, and the competing connector
belongs to the person, not the repository. A project-scope Claude run adds the
instruction only and says that Claude Docs stays available.

The command computes the plan for every target before it writes anything. The
plan lists each file's path, the action (create, add, replace, remove, delete or
unchanged), the exact lines added or removed, the backup directory and notes. If
any target is refused, the command prints the reasons, writes nothing and exits
1, with or without `--dry-run`. If nothing would change, it says so and exits 0.
`--dry-run` stops after the plan with exit 0. Otherwise a terminal gets the
question `Apply these changes? [y/N]`, and any answer but `y` or `yes` leaves
everything unchanged with exit 0. `--yes` skips the question. Without a terminal
and without `--yes` the command writes nothing, exits 2 and tells you to rerun
with `--yes` or `--dry-run`.

Before it changes or deletes an existing file, the command copies the file into
`agent-setup-backups/`, next to `connection.json` in the `flats-client`
directory of the user config directory. It follows symbolic links and edits the
target, so a link kept by a dotfile manager stays a link. It writes the files one
by one. If a write fails, the earlier files stay written, the command reports
the failure per file and exits 1, and a rerun finishes the rest.

`--undo` shows the same preview and asks the same question. It looks at the same
targets as a setup for the same scope and agents, so set the same
`CLAUDE_CONFIG_DIR` or `CODEX_HOME`. For Codex it looks at both
`AGENTS.override.md` and `AGENTS.md`. The command keeps no record of earlier
runs. Undo therefore cannot tell a settings entry that setup added from an
identical one that was there before, and it removes both. The plan lists each
entry before anything is removed. Undo never deletes `CLAUDE.md`, `AGENTS.md`,
`AGENTS.override.md` or `settings.json`, even when nothing else is left in them.
An empty `AGENTS.override.md` silences `AGENTS.md` in Codex, and a tracked file
belongs to its repository. The one file undo deletes is the Cursor rule file,
once only its front matter is left. A second undo changes nothing.

The command cannot reach every route. The Claude desktop app does not read the
connector entry, so disconnect Claude Docs at claude.ai/customize/connectors if
it is listed there. Undo does not reconnect a connector you disconnected. The
command does not edit Cursor User Rules. At user scope it prints the text and
the place to paste it, and undo does not remove text you pasted.

The agent also needs Flats connected, which this command does not do. Run
`flats mcp-config` for the setup commands (see
[MCP without the plugin](#mcp-without-the-plugin)), or install the
[plugin](#plugin). Until Flats is connected, the agent tells you instead of
using another tool.

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

Repository changes follow the [development protocol](development-protocol.md).
Use [flats-dev-workflow](../.agents/skills/flats-dev-workflow/SKILL.md) for change
requests, or, when explicitly limited to one stage, invoke [design](../.agents/skills/flats-design/SKILL.md),
[implementation](../.agents/skills/flats-implement/SKILL.md) or
[testing](../.agents/skills/flats-test/SKILL.md) independently. Each stage includes
review by a different model and correction; the shared protocol defines evidence
and completion. [Repository instructions](../AGENTS.md) provide the default route
for agents without automatic skill discovery.

The workflow selects [playbooks](../.agents/skills/flats-dev-workflow/SKILL.md#playbooks)
by change type, such as flaky tests, interface renames or console UI; they add
steps and known pitfalls inside the stages. Two cleanup skills support the
stages: [flats-deslop](../.agents/skills/flats-deslop/SKILL.md) removes
generated-code slop from a branch diff before review, and
[flats-unslop](../.agents/skills/flats-unslop/SKILL.md) removes generated-writing
patterns from docs, specs and PR text. Both are adapted from MIT-licensed
upstream skills; each names its source and carries the license.

The [Flats issue triage skill](../.agents/skills/flats-issue-triage/SKILL.md)
classifies repository issues with a small workflow-oriented label set, checks
relevant specs and code, and proposes evidence-backed duplicate candidates. Its `triaged` label marks
completed initial classification, not human approval or a fix.
It can be used by an authorized issue worker; adding the skill does not
register a webhook or start automatic execution.

## MCP without the plugin

Agents connect to the Streamable HTTP endpoint at `http://127.0.0.1:7878/mcp`
on the host, or the console's Tailscale URL plus `/mcp` from another allowed
device. Run `flats mcp-config` for Claude Code, Codex and Cursor setup, including
the plugin install commands. For a host on a custom management port, use
`flats mcp-config --url http://127.0.0.1:17878`. For example:

```sh
claude mcp add --transport http flats http://127.0.0.1:7878/mcp
codex mcp add flats --url http://127.0.0.1:7878/mcp
```

Cursor project `.cursor/mcp.json`:

```json
{"mcpServers":{"flats":{"url":"http://127.0.0.1:7878/mcp"}}}
```

Restart or reconnect your client and list tools. The agent's first call is
`check_client`. In a terminal, `flats status` checks that the CLI reaches the
host.

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
`topic.board` covers boards: when to choose one, the element model, the
`update_board` operations, guards and layout help, and readability.

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
| [Content types](content-types.md): websites, HTML pages, Markdown documents, designs and boards | `flats://docs/content-types/v1` | `get_content_types` |

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

Behind a reverse proxy, the operator can declare
[`console.external_origins`](configuration.md#external-origins-behind-a-reverse-proxy).
`/llms.txt`, `/llms-full.txt` and `/docs/agent-guide.md` then name the matching
declared origin, including its HTTPS scheme when the upstream uses HTTP.
Forwarded headers cannot override it. Status and approval links also follow
the declared origin; requests on loopback keep the existing preferred-console
and fallback behavior. This declaration does not authenticate agents or
operators; the proxy or private network must protect access.

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
