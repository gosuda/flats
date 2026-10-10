---
name: flats-deploy
description: Deploy, publish, preview, share or roll back a website, small server app (JavaScript with SQLite and file storage) or Markdown document on the user's own Flats host, then verify it. Use when the user mentions Flats or a flat, or asks to host something on their own machine or tailnet with Flats. Do not use for cloud hosting providers (Vercel, Netlify, Cloudflare), for running a local dev server, for Portal tunnels outside Flats, or to change who can see a flat or delete one without the user asking.
---

# Flats

The Flats MCP server carries the instructions and the on-demand guide. This
skill only gets you connected.

## When the Flats tools are available

Follow the Flats MCP server instructions. Call the read-only `guide` tool with
`{"items":["topic.index"]}` first; it routes the task to short topics
(static sites, server apps, documents, approvals, rollback and data, preview
and verification). Every publish, activation, rollback, visibility change and
deletion waits for the operator: give the user the returned `approval_url`
exactly as returned and poll `get_approval`. Never approve your own request.

## When the Flats tools are missing or point at the wrong host

The plugin's MCP server runs `flats mcp`, which relays the host chosen on this
machine: `--url`, else `FLATS_URL`, else the address saved by
`flats connect`, else loopback `http://127.0.0.1:7878`. MCP servers usually do
not receive the shell environment, so plugins rely on `flats connect`.

1. Run `flats connect` to see the current host. If `flats` is not on `PATH`,
   ask the user to install the Flats CLI.
2. If the host is missing or not the one the user means, ask the user for
   their Flats console address and run `flats connect <url>`. Never guess a
   host or switch hosts on your own.
3. Ask the user to restart or reconnect the agent so it picks up the host.

Without MCP, `flats help` lists CLI commands, and the host serves the same
guidance at `<console>/llms.txt`.
