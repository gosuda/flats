## Flats guide index

Flats hosts websites, small server apps and collaborative Markdown documents
("flats") on the operator's own machine. Read only the topics your task needs,
several per call: `guide {"items":["topic.server","topic.server.db"]}`.

### Route by intent

| You want to | Read | Main tools |
|---|---|---|
| Publish a static site or single-page app build | `topic.static`, `topic.manifest`, `topic.design` | `save_draft`, `open_preview`, `publish` |
| Make a page look and read right (title, themes, phone width, assets) | `topic.design` | `save_draft` warnings, `open_preview` |
| Build a server app (HTTP API, SQLite, file storage) | `topic.server`, then `topic.server.db`, `topic.server.files`, `topic.server.fetch` as needed, and `topic.server.limits` | same as static |
| Type-check server code against the exact runtime API | `topic.server.types` | |
| Write or edit a Markdown document | `topic.docs` | `get_document`, `save_document`, `publish` |
| Give an app settings, credentials or API access | `topic.env-secrets`, `topic.server.fetch` | `list_env`, `set_env`, `list_secrets`, `get_network` |
| Make a flat public or private, or delete it | `topic.approvals` | `set_visibility`, `delete_flat` (ask the user first) |
| Roll back code or restore data | `topic.rollback-data` | `rollback` |
| Check a Draft or a published version | `topic.preview-verify` | `open_preview`, `get_flat`, `get_logs` |
| Understand a refused call | `refusal.<category>` named in the error | |

For an existing flat, call `get_flat` before changing anything.

### Workflow map

1. **Draft.** `save_draft` (or `save_version`) uploads the complete build;
   `save_document` saves one Markdown document. Saving creates a missing flat
   and only changes its Private Draft. Nothing is published.
2. **Review.** Read the save `warnings`, then `open_preview {slug, version:
   0}` serves the Draft privately; look once and fix (`topic.design`).
3. **Request.** `publish` (or `deploy: true` on a save) returns
   `pending_approval` with `approval_url`. Give that URL to the user exactly
   as returned.
4. **Wait.** Poll `get_approval {id}` until `approved`, `rejected` or `failed`.
5. **Verify.** `get_flat` shows the new `live_version` and a ready endpoint;
   fetch the URL and check the behavior before calling it live.

Later changes follow the same request-and-wait pattern: `deploy {version: N}`
reactivates a published version, and `rollback`, `set_visibility` and
`delete_flat` also wait for the operator (`topic.approvals`).
