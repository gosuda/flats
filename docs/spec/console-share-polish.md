# Share dialog polish

Status: implemented and independently reviewed; desktop and mobile verification completed.

## Outcome

A person opening Share can understand who can use the selected link and check whether that link is ready without closing the dialog. Preserve the existing dialog layout, visibility approval flow, and provider permissions.

## Current state

Share describes all private access as Tailscale access, including Local and NetBird routes. Missing addresses always say a public link is being prepared, even for an unpublished or private flat. The dialog asks people to reopen it to check progress. Copy and Visit can be offered for an address whose endpoint is still starting or failed.

## Requirements

- Describe private access using the selected current endpoint: Local opens only on the Flats host, Tailscale follows Tailscale access rules, and NetBird follows NetBird access policies. Unknown routes receive neutral private-network wording, never an inferred Tailscale claim. Public access retains its existing disclosure.
- Prefer a ready permitted current route for the chosen visibility. Exclude draft, foreign-host and stopped routes. Explicit endpoint readiness takes precedence over a URL merely being present. Preserve compatibility with legacy URL-only responses when no matching endpoint is reported.
- Distinguish unpublished, missing, preparing, failed and ready states. Do not claim preparation without evidence of an active starting/connecting route. A deployed but non-running flat does not offer an active link.
- Offer Refresh in the dialog. Refresh is read-only, fetches the current flat, reports errors inline, and preserves the last observed state while clearly marking it stale. During refresh, duplicate requests and visibility changes are disabled. A completed request must not update a closed dialog or discard keyboard focus. Unchanged Visit links keep their element and focus; when a focused Visit link changes, focus follows the replacement link or moves to Refresh if the link disappears.
- Refresh success updates access, notice, address and action availability without closing the dialog. Copy/Visit require a live, usable address; preserve the existing server-only Copy restriction.
- Keep refresh manual: there is no background polling or provider retry/configuration mutation. The user can refresh pending, failed or ready states.

## Validation

Exercise Local, Tailscale, NetBird, unknown and public routes; ready, starting, failed, unpublished, missing and stopped states; draft and other-host exclusions; refresh success/failure and closure during a request. Verify current visibility changes still require the existing approval flow. Inspect desktop and mobile against a disposable development server with synthetic data. Public screenshots must contain no personal data, internal server details or tailnet domains.

## Open questions

None. Provider activation and repairing provider connection failures are outside this polish scope.

## Review decision

Claude and Codex agree to proceed. The observed false Tailscale claims, misleading missing-link state and unusable actions justify this bounded change. The independent review's corrections are accepted:

- Ready flags include key-expiring endpoints; starting means preparing; error, needs-login and unavailable are unavailable; unknown states never imply preparation.
- Selection is deterministic: ready, starting, unavailable, then private provider preference (Tailscale, NetBird, Local). On a remote console, prefer another candidate over a host-only route. Preserve the same audience/host/permission boundary as existing link controls.
- Explicitly non-running documents/designs say not running, never instruct a version publication. Website/page flats without a live version say not published. Legacy responses lacking running remain compatible; legacy state fields still constrain actions, and URL-only links do not claim readiness.
- Replace the intro, access explanation and private notice together. Keep the shared console helpers unchanged; this dialog needs readiness classification that existing open-link controls do not provide.
- Refresh and visibility mutation exclude each other. Persistent controls preserve focus, closed-dialog responses are discarded, and a successful changed observation refreshes the parent on close.

A reported current route that is stopped or unpermitted must not be revived by a legacy URL fallback. Unrelated providers, draft routes and another host's routes do not suppress the server's selected legacy URL. A ready route without an address must not outrank a ready route with an address. Refresh remains keyboard-focusable while marked unavailable with `aria-disabled`; its single-flight guard prevents another request. A failed refresh preserves the last observed state text as well as its address.
