# Content types

Contract version: `1`. MCP resource: `flats://docs/content-types/v1`.
Read-only fallback tool: `get_content_types` (no arguments); the document part
is also `guide {"items":["topic.docs"]}`, and the page part
`guide {"items":["topic.page"]}`.

A flat's **type** says what an agent authored; the **kind** says how Flats runs
it. Agents upload content of a type; a type adapter in the host turns that
content into something Flats can serve. Websites and pages share the
lifecycle of Drafts, versions, approvals and previews; documents, designs and
boards run at once and keep revisions (below). Visibility and exposure work the same for every type.

| Type | Agent authors | Host runs it as | Live data |
|---|---|---|---|
| `flat` (default) | a website: static files, or a server module (`kind` `static`/`server`) | static file server or the server-flat runtime | server flats: `env.DB` / `env.FILES` |
| `docs` | Markdown documents (plus images/other assets) | the built-in collaborative editor app on the server-flat runtime (kind `server`) | the live collaborative document state |
| `page` | one static HTML document with charts or visualizations, plus its assets | static file server (kind `static`) | none |
| `design` | a canvas of HTML artboards (`canvas.json`, `.html` files and assets) | the built-in design canvas inside the host | the current canvas files, which people edit live |
| `board` | whiteboard elements (stickies, shapes, connectors, text, pen strokes, images and frames) | the built-in board canvas inside the host | the live board state |

A flat keeps its type: a save of another type is refused. Slides and other
types are candidates for later adapters; they are not implemented. Websites are described in `flats://docs/runtime-api/v1`.

The built-in editor uses host helpers (`runtimeGeneration`,
`ws.setSendLimits`, `__flats_docsCodec`) that are not part of runtime API v1
and are not supported for agent-authored server flats.
