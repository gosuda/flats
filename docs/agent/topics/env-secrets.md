## Environment variables and secrets

Ordinary app environment variables and operator-managed secrets are strings
injected as `env.NAME` when the worker starts. `env` is frozen; `env.DB` and
`env.FILES` are reserved host bindings. New variable and secret writes require names matching
`[A-Z_][A-Z0-9_]*`, at most 64 characters; `DB`, `FILES`, `__PROTO__`, `PROTOTYPE` and `CONSTRUCTOR` are rejected for new writes.
New values are at most 64 KiB and must be valid UTF-8 without NUL; empty strings are supported.
A name cannot exist in both namespaces. Missing properties are undefined.
Historical secrets remain stored and keep their runtime behavior: JavaScript
`DB`/`FILES` host bindings take precedence over historical secrets with those
names, while WASI receives their stored strings. Other historical names and
values are preserved, including WASI rejecting NUL values as before. Replacing
a historical secret must pass the current write validation.

Manage ordinary values with MCP `list_env {slug}`, `set_env {slug, name, value}`
and `delete_env {slug, name}`, the CLI `flats env set <slug> <name> <value>`,
`flats env ls <slug>` and `flats env rm <slug> <name>`, or the console Settings
page. `list_env` returns ordinary names, values and update times. Ordinary
values are readable by management clients and stored without secret
encryption: use secrets for credentials. An ordinary variable cannot share a
secret's name; remove the old kind before changing kinds.

Changes leave running handlers and previews on their startup snapshot. Approved
deployment, redeployment, rollback or standalone data snapshot restoration captures current variables and secrets
when activation begins; the health check and live worker share that snapshot.
Settings are not pinned to the approval request or code version. Writes after
capture apply at the next activation. New previews and a Flats host restart load
current settings; automatic worker restarts reuse the captured snapshot. Redeploy
through the existing approval flow to apply changes. Neither
ordinary variables nor secrets enter frontend bundles, static files or build
substitution. There is no inherited host environment.

`list_secrets {slug}` shows names/update times only. Agent MCP cannot set/read
secret values; operators use the console or `flats secret set`. Secrets are
encrypted on disk with local secret.key, but anyone with data-dir access can
recover them; trust your handler, which can itself return secrets. Never log
secret values or put credentials in ordinary variables.

To apply changed settings deliberately, redeploy the live version with
`deploy {slug, version: <live>}` and wait for approval (`topic.approvals`), or
ask the user to use **Redeploy (apply environment)** in the console.
