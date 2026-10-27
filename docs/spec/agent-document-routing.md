# Agent document routing

Status: implemented

A user can connect an agent to Flats and still get documents somewhere else.
In Claude Code the connector `claude.ai Claude Docs` is available with no setup,
and the synced skill `anthropic-skills:docs` sends document requests to it. The
Flats skill is chosen only when the user names Flats. This spec adds
`flats agent-setup docs`, an opt-in command that makes Flats the document tool
of the agents the user selects, and that says what it cannot cover.

A line in `CLAUDE.md` is not enough. Claude Code treats `CLAUDE.md` as context,
not enforced configuration, and may follow either of two contradicting
instructions. For Claude Code the command therefore removes the competing route
first and adds the instruction as the tie-breaker. Codex and Cursor have no
known competing route, so for them it adds the instruction only.

The command keeps no state of its own. Everything it adds can be recognized in
the file it changed, so `--undo` works from the files alone.

## Requirements

1. **Command.** `flats agent-setup docs [--agent claude|codex|cursor|all]
   [--scope user|project] [--dry-run] [--yes] [--undo]`. `--scope` defaults to
   `user`. At user scope `--agent` defaults to every agent whose user directory
   exists: `$CLAUDE_CONFIG_DIR` or `~/.claude`, `$CODEX_HOME` or `~/.codex`, and
   `~/.cursor`. A relative value of either variable is taken from the current
   directory. At project scope `--agent` is required when setting up, so a
   repository gets files only for agents its owners name. With `--undo`,
   `--agent` defaults to `all` at either scope. An agent named explicitly is set
   up even if its directory is missing. If no agent is found, the command is a
   usage error. The project is the nearest directory, starting at the current
   one and going up, that holds a `.git` entry. If there is none, the project is
   the current directory and the plan says that no repository was found. The
   plan always names the project directory. A project that is the user's home
   directory is refused, with a message to run the command inside a repository
   or to use user scope. The command runs on macOS and Linux, like the rest of
   the CLI. Its output is text, and `--json` is a usage error. Ctrl-C at the
   question ends the command without writing.

2. **Targets.** Each target is one file or one printed text:

   | Agent | User scope | Project scope |
   | --- | --- | --- |
   | Claude Code | A block in `<claude dir>/CLAUDE.md` and three entries in `<claude dir>/settings.json` (requirement 5) | A block in `<project>/CLAUDE.md` |
   | Codex | A block in `<codex dir>/AGENTS.override.md` if that file exists, else in `<codex dir>/AGENTS.md` | A block in `<project>/AGENTS.md` |
   | Cursor | Prints the routing text for Cursor Settings, Rules, User Rules; writes nothing | The file `<project>/.cursor/rules/flats-documents.mdc` |

   Project scope never edits a settings file. Those files are checked in and
   shared, and the competing connector belongs to the person, not the
   repository. A project-scope Claude run adds the instruction only and says
   that Claude Docs stays available.

3. **Routing text.** One canonical text is rendered into every target. It is at
   most 800 bytes and names no host. It says which requests it covers (a
   document that people will read, share or edit afterwards), that the agent
   creates such a document with the Flats MCP tools (`create_flat` with type
   `docs`, then `save_document`), that it starts with `check_client` and the
   guide, and that it tells the user instead of using another document tool when
   Flats is not connected. The Flats MCP instructions and the deploy skill stay
   unchanged.

4. **Instruction blocks.** A block starts with the line
   `<!-- flats:documents:begin -->` and ends with `<!-- flats:documents:end -->`.
   A marker counts only when it is the whole line and lies outside a fenced code
   block (a fence of backticks or tildes), so a file that documents the markers
   is not mistaken for one that holds a block. A file without a block gets one
   appended: the command makes sure the text before it ends in a newline, adds a
   blank line, then the block. An empty file gets the block alone. A file with a
   block whose text differs gets that block replaced where it is. A file whose
   block is current is unchanged. A begin marker without an end marker, an end
   before a begin, or more than one block is refused. Bytes outside the markers
   never change, apart from the newline and blank line just described, and
   inserted lines use the file's line ending, CRLF if the file contains one. A
   missing file is created holding the block. The Cursor `.mdc` file is
   different: the command owns it, so a new file is a short front matter that
   sets `alwaysApply: true` followed by the block. Front matter is a first line
   `---` through the next line `---`, ignoring a carriage return at the end of
   either line; a first `---` with no later one is not front matter. An existing `.mdc` file without the markers
   that holds nothing but front matter and whitespace gets the block appended,
   which is the state an undo can leave behind. An existing `.mdc` file without
   the markers and with other content is refused instead of appended to, and the
   refusal tells the user to add the block by hand or move the file.

5. **Claude settings.** The command adds `"deniedMcpServers":
   [{"serverName": "claude.ai Claude Docs"}]` and, in `permissions.deny`, the
   entries `Skill(anthropic-skills:docs)` and `mcp__claude_ai_Claude_Docs__*`.
   The Claude Code permissions documentation says to write `mcp__<server>__*` to
   match every tool of an MCP server, that a glob in the tool-name position of a
   deny rule removes the matched tools from Claude's context, and that connector
   tools appear as `mcp__claude_ai_<server>__<tool>` with characters such as
   spaces and dots written as `_`. The command inserts text and leaves every
   other byte alone, apart from the empty gap described below: no reordering, no reformatting, the file's own indentation
   and line endings, and an inline array stays inline. A key that is absent goes
   after the last member of its object with that member's indentation. A closing
   brace or bracket is never moved: when the gap inside an empty pair holds a
   line break, the new text goes right after the opening one, and otherwise the
   gap is replaced. An object with no members becomes a multi-line object: the new members are indented one
   unit deeper than the line that holds its opening brace, and the closing brace
   is on its own line at that line's indentation. An array with no elements
   takes the new elements inline if its brackets are on one line, and otherwise
   one per line indented one unit deeper than the line that holds its opening
   bracket. The indentation unit is the file's own, and two spaces if the file
   has none. For a missing, empty or whitespace-only file it writes the entries
   at two-space indentation. The
   command parses the result, and unless it equals the original plus exactly
   these entries it refuses and writes nothing. It also refuses invalid JSON
   (comments included), a top-level value that is not an object, a wrong type
   for `deniedMcpServers`, `permissions` or `permissions.deny`, and a key it
   edits appearing twice. An entry is present when a `deniedMcpServers` element
   is an object whose only key is `serverName` with exactly that value, or when
   `permissions.deny` holds exactly that string. An element with other keys is
   a different rule and does not count. A present entry is not added again, and
   the plan says that it was already there and that `--undo` will remove it.

6. **Plan and confirmation.** The command first computes the plan for every
   target without writing. It prints each file's path, the action (create, add,
   replace, remove, delete or unchanged) and the exact lines added or removed,
   the backup directory, then the notes of requirement 9. Delete is the action
   for a whole file and is shown as such. If any target is refused
   it prints the reasons, writes nothing and exits 1, with or without
   `--dry-run`. If nothing would change it
   says so and exits 0. `--dry-run` stops after the plan, with exit 0, even with
   `--yes`. Otherwise a terminal gets the question `Apply these changes? [y/N]`,
   and any answer but `y` or `yes` leaves everything unchanged with exit 0.
   `--yes` skips the question on a terminal too. Without a terminal and without
   `--yes` the command writes nothing and exits 2, telling the user to rerun
   with `--yes` or `--dry-run`. A decline is the user's choice and exits 0; a
   refusal exits 1.

7. **Writing.** The command resolves symbolic links and edits the target, so a
   link kept by a dotfile manager stays a link. A dangling link or a target that
   is not a regular file is refused. Before it changes or deletes an existing
   file it copies the file into `agent-setup-backups/`, a directory next to
   `connection.json` in the `flats-client` directory of the user config
   directory. A backup is named `<UTC time as 20060102T150405Z>-<first 8 hex digits of the SHA-256 of
   the resolved path>-<file name>`, so files with the same name do not collide.
   The command writes a temporary file named `.<file name>.flats-<random>` in the
   target's directory and renames it over the target. It removes the temporary
   file if anything fails, and a killed process can leave one behind. An existing
   file keeps its mode. A new settings file is `0600` and a new instruction file
   `0644`. The command creates missing parent directories. Immediately before the
   rename it reads the file again, and if the content differs from the plan it
   changes nothing in that file, reports that the file changed while the command
   ran, and exits 1. It takes no lock across processes, so another program that
   writes the file at the same moment can still win. The check narrows that
   window and does not close it. The command writes files one by one. A failure
   leaves earlier files written, is reported per file, and exits 1. A rerun
   finishes the rest, because every step is idempotent.

8. **Undo.** `--undo` plans the reverse with the same preview and confirmation.
   It looks at the same targets as a setup for the same scope and agents, so the
   user sets the same `CLAUDE_CONFIG_DIR` or `CODEX_HOME`. For Codex it looks at
   both `AGENTS.override.md` and `AGENTS.md`. It keeps no record of earlier runs.
   It removes the blocks and the settings entries that setup adds, and it cannot
   tell an entry setup added from an identical one the user had put there before.
   It removes both, and the plan lists each one before anything is removed. The
   issue asked that undo remove exactly what was added; the user accepted this
   relaxation in exchange for a command with no state to keep consistent. If undo
   finds nothing, it says so and exits 0.

   Removal is as careful as insertion. A block goes together with the blank line
   before it, and no other byte changes. A settings entry goes together with the
   separator that joins it to a neighbour: the comma and the whitespace up to
   the next element, or from the previous element to the end of the last one, in
   any layout, comma-first included. Every matching element goes if the entry
   appears more than once. An element that is alone in its array and has its
   line to itself goes with that line, which leaves the brackets as they were.
   The result must parse and equal the
   file's current content minus exactly those entries, or the file is refused.
   Undo never removes a key or an emptied array, and it never deletes
   `CLAUDE.md`, `AGENTS.md`, `AGENTS.override.md` or `settings.json`, even if
   nothing else is left in them. An empty override file silences `AGENTS.md` in
   Codex, and a tracked empty file belongs to its repository, so undo must not
   remove a file whose presence or emptiness decides what the agent reads, or a
   file the user or the repository owns. The one file undo deletes is the Cursor `.mdc`
   file, which only this command creates, so deleting it is intended even if it
   was committed. It goes when nothing is left but its front matter and
   whitespace, whatever that front matter says, and otherwise it stays with the
   other content. If the `.mdc` path is itself a symbolic link, undo removes the
   block from the target, keeps the link and the target, and says so. Undo does
   not remove the directory around the file. A file returns to its earlier bytes only if
   nothing else changed it since the setup and setup did not create it; if
   another program reformatted or edited it, undo removes the entries and keeps
   the other changes. A file that had no final newline keeps the one the setup
   added. A second undo changes nothing.

9. **Notes in the plan.** The plan always says that the agent needs Flats
   connected, with `flats mcp-config` named for that, and that until then it
   tells the user instead of using another tool. For Claude Code at user scope
   it says that the Claude desktop app does not read the connector entry and
   that Claude Docs is disconnected at claude.ai/customize/connectors if it is
   listed there. For Cursor at user scope it prints the text and the place to
   paste it. The plan of `--undo` carries none of these setup notes, including the
   project-scope note of requirement 2. It says
   instead that undo does not reconnect a connector the user disconnected at
   claude.ai and does not remove text the user pasted into Cursor's User Rules,
   for the agents and scopes where setup would have said so.

10. **Codex size.** Codex reads at most 32 KiB of `AGENTS.md` content by default.
    The command refuses a setup that adds or replaces a Codex block when the
    resulting file would be larger than 32768 bytes, and says why. A file that
    would stay unchanged, and an undo, are never refused for size.

11. **Installer and uninstall.** After a successful run, `install.sh` prints
    where to connect an agent (`flats mcp-config`) and that
    `flats agent-setup docs` is optional. It stays non-interactive and never
    edits an agent file. `flats uninstall` leaves agent files alone. Its text
    output adds one line after the existing message, in every outcome including
    "was not installed", that says `flats agent-setup docs --undo` reverts a
    setup made earlier. Its other output stays as it is, including the JSON of
    the macOS branch.

12. **Console.** Settings, Agents shows one more block with the command
    `flats agent-setup docs` and a copy button, in a group of its own. The text
    says that the command runs on the machine where the agent runs and that it
    shows a preview before it writes anything. The block adds no tab and no block
    to the existing client tabs. The issue proposed copy-only cards with the same
    snippets; the user chose the command alone, because the console cannot write
    the agent's files and a pasted fragment would skip the preview, the
    validation and the undo.

13. **Help and documentation.** `flats help agent-setup` describes the command.
    [Connecting agents](../agents.md), [Surfaces](../surfaces.md) and the
    [installer section](../release.md) document it.

## Completion criteria

Each requirement is checked by a test that runs the real code against
temporary directories, except where noted.

- Command and targets (1, 2, 10): defaults and detection through
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME`, the Codex override file, the project
  root search from a subdirectory, outside a repository and at the home
  directory, project scope with
  and without `--undo`, a Codex file one byte under and over the limit, and an
  over-limit file that stays unchanged or shrinks.
- Routing text (3): a test checks the size, that no URL or host is present, and
  that the named tools exist in the MCP tool registry.
- Blocks (4): insert, replace, current, each refused shape, markers inside
  fenced code blocks, CRLF files, and empty and whitespace-only files.
- Settings (5): a table of real-world shapes (tabs, two and four spaces, CRLF,
  inline and multi-line arrays, empty inline and multi-line arrays, `{}` and a
  multi-line empty object at the top level and as `permissions`, absent and
  existing `permissions`, existing `deniedMcpServers`, entries already present)
  where the result differs from the input only by inserted bytes; each refusal;
  and a check by hand against a large real `settings.json`, whose result the
  final report gives, because such a file cannot be committed.
- Flow (6, 7): `--dry-run`, a refused target with `--dry-run`, no terminal, decline, `--yes` with and without a
  terminal, symbolic links, modes, backups and their names, a leftover temporary
  file after a failure, a file that changes between plan and write, and a write
  failure in the middle.
- Undo (8): setup then undo returns an existing instruction file to its
  earlier bytes except the documented final newline, and a settings file to its
  earlier entries with none of the three left; empty arrays that setup created
  or that were there stay, so a settings file that setup created is left
  holding an empty `deniedMcpServers` array and an empty `permissions.deny`
  array; a settings entry that was there before setup is
  listed in the setup plan and removed by undo; an element with other keys than
  `serverName` is kept; duplicates are all removed; undo after a setup that
  failed halfway; undo after another program reformatted the file; an empty
  override file and an empty tracked project `CLAUDE.md` are never deleted;
  an instruction file created by setup is left empty, not deleted; the Cursor
  file is deleted when only its front matter, or nothing, is left; kept when it
  holds other content, after which a new setup is refused with the stated
  message; kept when its path is a symbolic link; a front-matter-only `.mdc`
  file accepts a new setup; undo when the override file appeared after setup;
  undo twice; setup, undo and setup again on a settings file that held none of
  the entries ends with each of the three present once and parses to the same
  value as after the first setup.
- Plan notes (9): the setup output of each agent and scope names the notes, and
  the `--undo` output names the undo notes and none of the setup ones.
- Installer and uninstall (11): `shellcheck -s sh install.sh`, the installer
  smoke script, and a CLI test of the uninstall line.
- Console (12): the console UI test.
- Documentation (13): `make docs-verify`.

The effect on the agents themselves is not covered by these tests. For Claude
Code, connector denial was checked in a real VS Code session on 2026-10-27: the
connector's tools disappeared when the setting was saved. The glob rule follows
the documentation quoted in requirement 5, and the effect of the skill rule on
the skill listing was not observed. The Codex and Cursor instruction files are
not verified with the real agents. The final report says so.

## Non-goals

Silent edits. Managed or organization settings. The claude.ai web and desktop
chat connectors. Agents other than the three, such as Gemini CLI and Copilot,
which were not checked. Registering the Flats MCP server with an agent, which
`flats mcp-config` and the plugin do. Checking that the host answers. A record
of what earlier runs changed, which is why undo cannot tell added entries from
identical ones already there. Console blocks that show the routing text itself.
Windows, where the CLI is not built.

## Open questions

- Whether Codex reads `AGENTS.override.md` first in its user directory, as its
  documentation describes. No Codex is installed where this was designed.
