## Preview and verify

### Preview

`open_preview {slug, version: 0}` serves the current Private Draft; a positive
`version` previews that published version. Live is unchanged. Server and docs
flats get an isolated copy of the live DB/FILES: preview writes never reach
live. New previews capture current environment variables, secrets and network
grants. At most **5 open previews per flat**; a preview closes on the next
activation of that flat, or after the operator setting `preview_ttl_seconds`
(default **24 hours**) without visits. Previews are Private (loopback or the
existing tailnet ACL) and never use Public providers.

### Readiness

A URL can exist before it answers. `get_flat` reports `private_state` and each
preview's `state`: `ready`, or `starting` while a tailnet node joins or waits
for its HTTPS certificate (often 1-2 minutes for a new flat or preview). Check
`get_flat` again before fetching; a TLS error while `starting` is not a failed
deploy. Local mode serves plain HTTP at `<slug>.localhost:<local-port>`. To
verify a local-mode URL from another network namespace, connect to the host's
address and port with the returned URL's Host header; that is not public
exposure.

Give the user the URLs that `get_flat` and `open_preview` return; never build
one from a pattern. A Local link (`*.localhost`) opens on the machine where it
is clicked, so it reaches the flat only from the Flats host itself. On a host
whose private backend is Tailscale, new flats and Draft previews use the
tailnet address, which other allowed devices can open.

### Verify

Report a version as live only after `get_approval` says `approved`, `get_flat`
shows it as `live_version` with a ready endpoint, and you fetched the URL and
checked the behavior. A save result or a pending request never means live.
Report the verified version, address and observed health. Use
`get_logs {slug, kind?, after?, limit?}` for validation, health, runtime and
approval events (page with `next_after`). For Public flats, repeat the public
notice and recheck shared links over time.
