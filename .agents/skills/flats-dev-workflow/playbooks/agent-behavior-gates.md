# Playbook: testing an agent without source access

Use when a change claims that an agent can use Flats from its MCP tools and
guide alone. The recorded procedure is the
[runtime discovery gate](../../../../docs/gates/runtime-discovery.md).

## Set up

- [ ] A promise not to read source is not isolation. Remove the agent's native
  file, shell, browser and subagent tools, start it in an empty directory
  without a source checkout, and give it Flats as its only MCP server.
- [ ] Deny `save_draft_from_dir`: it uploads a host directory and can expose
  source indirectly.
- [ ] Disable user settings, extra skills and command shortcuts that could add
  tools or instructions.

## Verify

- [ ] Record the tool inventory the agent actually received at initialization.
  A permission allowlist is not the exposed inventory.
- [ ] Record every tool call and result, and check that no native or
  other-server tool was called.
- [ ] Verify the outcome independently over HTTP, not from the agent's report.
- [ ] Report the result as tool-surface isolation, not an operating-system
  sandbox. Keep raw transcripts outside the repository; publish only sanitized
  identities and observed outcomes.
- [ ] The agent under test never approves its own requests. Record any operator
  approval as an operator intervention.
