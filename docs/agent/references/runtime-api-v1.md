# Flats runtime API reference v1

Contract version: `1`. MCP resource: `flats://docs/runtime-api/v1`.
Read-only fallback tool: `get_runtime_reference` (no arguments).
This document ships inside the host binary; no checkout, source access, plugin,
or installed skill is needed. The tool returns `documentation_version`,
`uri`, `markdown`, `host_version` and the current `upload_limit_bytes`.
The version identifies the documented contract, separately from the host build.
Changes to this contract require a new reference version; corrections that do
not change behavior may revise v1. Record the document hash with gate evidence.

The sections below are also served one at a time by the read-only MCP tool
`guide` (for example `guide {"items":["topic.server","topic.server.db"]}`);
`topic.index` routes by task.

Non-website content types are described in `flats://docs/content-types/v1`.
Built-in app host internals (`runtimeGeneration`, `ws.setSendLimits` and
`__flats_docsCodec`) are explicitly outside runtime API v1.

## Connect from an MCP client

Connect using Streamable HTTP to the operator's console URL plus `/mcp`
(loopback default `http://127.0.0.1:7878/mcp`). Discover tools, then read this
resource with `resources/read`, call `get_runtime_reference` with `{}`, or call
`guide`. The management endpoint is privileged; use it only on a trusted
host/tailnet. See the README for installation and host/client setup.
