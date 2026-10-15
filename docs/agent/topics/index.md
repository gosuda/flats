## Flats guide index

Flats hosts websites, small server apps, HTML pages and collaborative Markdown
documents ("flats") on the operator's own machine. Read only the topics your task needs,
several per call: `guide {"items":["topic.server","topic.server.db"]}`.

### Route by intent

| You want to | Read | Main tools |
|---|---|---|
| Check client compatibility or refresh CLI, skill and guidance | `topic.client-update` | `check_client`, `get_client_skill` |
| Publish a static site or single-page app build | `topic.static`, `topic.manifest`, `topic.design` | `save_draft`, `open_preview`, `publish`, `deploy` |
| Make a page look and read right (title, themes, phone width, assets) | `topic.design` | `save_draft` warnings, `open_preview` |
| Build a server app (HTTP API, SQLite, file storage) | `topic.server`, then `topic.server.db`, `topic.server.files`, `topic.server.fetch` as needed, and `topic.server.limits` | same as static |
| Type-check server code against the exact runtime API | `topic.server.types` | |
| Write a document with charts, graphs or visualizations as one HTML page (Draft, then approved publish) | `topic.page`, `topic.page.visuals`, `topic.design` | `create_flat {type: "page"}`, `save_page`, `open_preview`, `publish`, `deploy` |
| Draw a chart, tooltip or diagram that reads correctly (in a page or any website) | `topic.page.visuals` | |
| Design screens, a prototype, posters or social images on a canvas people edit with you (runs Private at once; no publish or approval) | `topic.canvas`, `topic.design` | `create_flat {type: "design"}`, `save_design`, `get_design`, `list_revisions` |
| Write a Markdown document (runs Private at once; no publish or approval) | `topic.docs` | `create_flat {type: "docs"}`, `save_document` |
| Fix or add to a Markdown document (no approval; Public is visible at once) | `topic.docs` | `get_document`, `update_document` |
| See or undo earlier changes of a document or design | `topic.docs`, `topic.canvas` | `list_revisions`, `get_revision`, `rollback_revision` |
| Give an app settings, credentials or API access | `topic.env-secrets`, `topic.server.fetch` | `list_env`, `set_env`, `list_secrets`, `get_network` |
| Make a flat public or private, or delete it | `topic.approvals` | `set_visibility`, `delete_flat` (ask the user first) |
| Roll back code or restore data | `topic.rollback-data` | `rollback` |
| Check a Draft or a published version | `topic.preview-verify` | `open_preview`, `get_flat`, `get_logs` |
| Look at a rendered preview in your browser (overflow, contrast, failed loads, thumbnail) | `topic.preview-check` | your browser tool, `open_preview` |
| Understand a refused call | `refusal.<category>` named in the error | |

For an existing flat, call `get_flat` before changing anything.

### Workflow map

Websites and pages follow these steps. Documents and designs do not: they have no Draft,
versions, previews or publish, and every save runs at once (`topic.docs`, `topic.canvas`).

1. **Draft.** `save_draft` (or `save_version`) uploads the complete build;
   saving creates a missing flat and only changes its Private Draft. Nothing
   is published.
2. **Review.** Read the save `warnings`, then `open_preview {slug, version:
   0}` serves the Draft privately; look once in your browser and fix
   (`topic.design`, `topic.preview-check`).
3. **Save version.** `publish` (or `deploy: true` on a save) returns
   `pending_approval` with `approval_url` to save the Draft as a numbered
   version (`v1`, `v2`, ...) without deploying it. Give that URL to the user
   exactly as returned, then poll `get_approval {id}` until `approved`
   (`result_data.version` is the saved version number).
4. **Deploy.** Call `deploy {slug, version: N}` with that saved version number,
   share its `approval_url` with the user, and poll `get_approval {id}` until
   `approved`, `rejected` or `failed`.
5. **Verify.** `get_flat` shows the new `live_version` and a ready endpoint;
   fetch the URL and check the behavior before calling it live.

Later changes follow the same request-and-wait pattern: `deploy {version: N}`
activates or redeploys a saved version, and `rollback`, `set_visibility` and
`delete_flat` also wait for the operator (`topic.approvals`).
