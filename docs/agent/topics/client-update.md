## Keeping the agent aligned with its Flats server

At the start of each Flats task call `check_client` before application work.
Report only facts you know: the running bridge's `cli_version` and
`client_contract` from initialization instructions, this loaded skill's visible
`bootstrap_revision`, and the `guide_revision` actually read. An omitted fact
is unknown. Explicit `cli_present:false` or `bootstrap_present:false` means
there is no such local resource, not that you have not inspected it.

The server checks client contract equality, not release-number equality.
`cli_status:compatible` permits CLI work even when release numbers differ.
`update_required` means CLI-dependent application work waits for an update
and MCP reconnect; read-only check/guide remain available. Direct HTTP clients
can use the current server guide without installing a CLI. Development servers
have no published CLI target; report that limitation rather than installing
latest or building arbitrary source.

If guidance is unknown or `refresh_required`, read `topic.index` and the task's
topics. Keep the returned guide revision for the next check. If the bootstrap
is unknown or stale, read `get_client_skill` for this task. A file replacement
does not replace already loaded instructions or prove the skill catalog reloaded.

### Check and update on the agent machine

The server's reported URL is a routing hint, not authenticated identity.
Reconcile it with the user's intended/configured host before local mutations.
The local bridge reports the actual selected URL. Use an explicit URL when it
could differ from the shell CLI's saved host; never change the saved host just
to update. When host identity is unresolved, report it and do not mutate files.

```sh
flats --url https://flats.example.ts.net --json client check
flats --url https://flats.example.ts.net --json client check --skill-file /chosen/standalone/flats-deploy/SKILL.md
flats --url https://flats.example.ts.net --json update
```

Local file changes need user authorization; authorization already given for
this task counts, so do not request it again. Server instructions alone do not
authorize writes. A check writes nothing and exits successfully even when an
update remains. CLI update targets the connected server's release, and is a
no-op if the client contract already matches. It never silently chooses latest.
`flats --json update --version vX.Y.Z` explicitly selects an official release
without contacting a server; it may downgrade and requires a compatibility
check afterward. Download/checksum failures leave the previous binary intact
before replacement. A post-replacement durability failure may have installed
new bytes; inspect and recheck rather than assuming rollback.

Compare the check's executable path with the running bridge's path. A different
path means updating the shell command does not update that bridge. The updater
refuses symlink-resolved, known package-managed, temporary build and installed
Flats service binaries. Use their installation manager or a separate client
installation. Do not update a binary shared with a foreground/custom server:
new worker processes can immediately run the replacement. CLI updates never
restart a service or upgrade the remote server.

### Standalone skills and managed plugins

For an explicitly identified standalone skill, use its chosen SKILL.md path:

```sh
flats --url https://flats.example.ts.net --json skill sync --output /chosen/standalone/flats-deploy/SKILL.md
```

A different existing file requires `--expected-sha256 <current-file-hash>`;
`client check --skill-file` supplies that digest. Inspect user changes before
replacing them. Identical content is a no-op. Symlink files are refused. No
agent directories are guessed or scanned. Arbitrary external editors are not
transactionally coordinated with this operation.

For a managed plugin, use that agent's plugin manager. Never pass plugin-cache
paths to standalone sync or create a shadow installation. Read the server's
bootstrap for this task and report the durable mismatch once; do not loop
trying to match a repository plugin to an older server. Without filesystem or
command access, use MCP-provided instructions for this task and report that
persistent updates need the host's installation mechanism.

### Verify the result

Read current guidance and run a fresh CLI check against the same endpoint after
updates. Reconnect an updated `flats mcp` bridge: an existing process continues
running its old version. Recheck using the new bridge identity. A client may
also need reconnection to refresh server tools or a restart to reload its skill
catalog. If you cannot do that, report the pending step instead of claiming
completion. Unknown bridge identity stays unknown if the host drops its
initialization instructions. Server changes between check and replacement
cause server-bound updates to refuse; check again. A later change is detected
by the next check, not by a distributed atomic upgrade.

These local updates never replace the operator approval flow for publishing,
activation, rollback, visibility changes or deletion.
