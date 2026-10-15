# Console home polish verification

Verified on 2026-10-16 against the implementation in this change.

## Review and tests

- The overview was written before implementation. Independent Claude review
  and Codex assessment agreed GO after clarifying Retry focus, empty responses,
  onboarding language and post-action refresh behavior.
- Independent Claude code review returned GO. The correction pass also returned
  GO with no high or medium findings.
- `go test ./internal/console -run 'TestConsole(HomeFeedback|UI)$' -count=1` passed.
- `go test ./internal/console -count=1` passed, including the new dedicated
  fake-DOM/fake-clock suite and existing console UI checks.
- `git diff --check` passed.

The focused suite covers independent loading, aborted stalled requests,
malformed versus valid empty responses, last-known requests during failures,
single-flight Retry/visibility calls, post-save trailing refresh, unchanged
link identity, time text, announcements, search preservation, hidden-page
behavior and unmount cleanup. It checks all four onboarding examples and the
connection destination.

## Browser verification

Used the repository's disposable `consoledev` test server and headless Chrome
with desktop (1280 × 1000) and mobile (390 × 844) viewports. Its seeded data is
synthetic. The populated screen used its real core/API. Browser route
interception supplied approval outages, delayed responses, a new pending
request and an empty collection; these are UI fixtures, not proof of server
failure injection or live provider behavior. No approval decision was made.

Verified actual Retry keyboard activation, single-flight behavior while its
response waited, and focus remaining on the same button through busy/recovery.
A new synthetic approval appeared after the real 15-second poll without
reloading; search text and focus stayed intact. Both viewports had no horizontal
overflow or page errors. The Connect an agent link navigated to Settings.

Desktop and mobile screenshots were inspected for layout and privacy. They
contain only synthetic app content, without browser chrome, operator identifiers,
server addresses, provider domains, logs or credentials. The six captures are
retained outside the repository; this PR does not contain or embed image files.
No native GitHub attachment upload was available in the connected tools.

Captured states: populated desktop, approval lookup failure, recovery and a new
approval, approval on mobile, empty desktop, and empty mobile. The desktop empty
screen was recaptured in a fresh browser context after a sticky-header screenshot
artifact following viewport resizing; the DOM had one header and no product
change was needed.

## Limits

Live-region text and focus were checked, but a screen reader was not run.
Replacing a pending request with another at the same count can leave identical
announcement text; the visible list still updates. The inherited brief empty
state before the first flats response remains outside this change. The host's
network providers and production state were not exercised.
