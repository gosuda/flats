# Console home feedback

Status: implemented after independent review and a shared GO decision on
necessity and scope. See [verification](../review/console-home-polish/README.md).

## Problem and outcome

The home screen currently treats a failed pending-approval lookup as an empty
list. An operator cannot distinguish no requests from unavailable information.
New agent requests also remain unseen while the home screen stays open.
The empty screen describes only a website even though Flats supports documents,
HTML pages and design canvases. This change makes existing tasks discoverable
and the approval state trustworthy without changing the console's layout.

## Scope and necessity

Keep the existing search, list/grid switch, rows, pending banner and connection
link. Add no inbox, filters, sorting, approval summary, bulk actions or creation
wizard. All approval decisions remain on the existing approval page.

1. Fetch flats and pending approvals independently. An approval failure must
   never hide successfully loaded flats or be presented as zero requests.
2. Show a concise approval error with a keyboard-accessible Retry button.
   Do not expose raw server errors. If requests were already loaded, retain
   their links and explicitly label that list as last known, possibly outdated.
   Network failures and every non-2xx response show this error; the API has no
   session-expiry redirect or intentionally unsupported approval mode. Accept
   an `approvals` array or explicit `null` (the server encodes its empty nil
   slice as null); missing/other-shaped data is a failure. A successful retry clears the error; a confirmed empty response clears the
   old requests. Repeated failures do not stack banners.
3. Refresh pending approvals 15 seconds after the previous request settles while this page is visible,
   and immediately when returning to it. Keep the existing flats reload on
   return to visible. Periodic polls and Retry/Check again fetch approvals only. Initial load,
   action reloads and return-to-visible also reload flats. All approval
   fetches share one single-flight function. Retry and visibility join it;
   an action reload queues one coalesced trailing fetch if it is in flight.
   Abort approval requests after 15 seconds so a stalled request can recover. Reset the next poll
   after settlement, including when returning visible. An initial hidden
   mount still loads once, but does not schedule polls until visible. Use a single in-flight approval
   request: timer, Retry and visibility changes must not overlap it. Skip
   polling while hidden and stop timers/listeners on navigation. Responses
   arriving after navigation must not update the page. Periodic refresh
   changes only the approval area, preserving row actions, search and layout.
4. Do not replace an unchanged pending banner on every refresh: preserve
   keyboard focus on its links. Update existing time text in place. Key content
   on ids and rendered source fields, excluding relative-time formatting; changes may rebuild the list. Keep stale
   wording outside its links. A separate sr-only live region announces
   failure/recovery and changed ids/count only on transitions, not successful
   initial load or repeated failure; the first failure is announced.
5. Keep Retry focused through its busy state and completion. Once an error
   occurs, its status area and same button remain until page unmount. While
   busy use aria-disabled and ignore activation, never native disabled. On
   recovery by Retry or polling, show "Approval requests are up to date."
   and relabel the same button "Check again". This area is not a live region;
   the separate announcement handles transitions. No forced focus movement.
6. Retain the existing Connect an agent link. Add concise example requests for
   a website, a Markdown document, a single HTML page and a design canvas.
   Examples are wrapping plain text, not executable actions. Explain that an
   agent waits for approval to save or publish a website/page version, whereas
   documents/designs run when saved. Access is set separately through Share.
   Console Save/Publish confirmations also provide approval, as actions.js
   explicitly requests and decides the pending action.
7. Preserve the existing visual system and mobile wrapping; no new styles
   should be necessary. Keep generic errors and examples free of machine,
   provider, credential or personal data.

## Acceptance

Focused automated coverage checks independent loads, initial failure and retry,
last-known stale requests, recovery to empty, background arrival/removal,
unchanged-banner identity, no overlapping calls, hidden-page behavior and
cleanup/late responses, Retry/visibility joining an in-flight poll, explicit
null versus malformed payloads, post-action trailing refresh, request timeout,
search value/focus preservation and in-place time updates. It also checks all four onboarding examples and the
connection destination. Existing console tests remain passing.

A disposable localhost server with synthetic data verifies the actual browser
on desktop and mobile: populated and empty states, approval error/retry,
new requests without reloading (browser route interception induces these
synthetic states), keyboard focus through busy/recovery and overflow. Live
region assertions establish text transitions, not screen-reader testing. Screenshots show
only the app viewport and synthetic values and are retained outside the repository. No production host, real provider
or approve/deny decision is involved in this verification.

## Decision

Initial Claude review: GO on necessity/scope with four documentation blockers.
These are resolved above: stable Retry lifecycle; precise onboarding language;
verified HTTP/null response handling; and visibility/single-flight ownership.
Final Claude document review: GO with the Retry-scope and trailing-fetch
clarifications above. Codex agrees: this fixes misleading state and discovery
without adding management features. Onboarding claims were checked against
core.Publish, Deploy, SaveVersion and WriteDesign; console actions confirm and
decide their own approval. The existing sr-only utility remains sufficient.
