# Console content labels

Status: approved for implementation after independent Claude and Codex GO.

## Problem and outcome

Settings calls the shared revision and pin limits “Design” limits although
both documents and designs use them. Analytics displays an unsupported unique
visitor metric and labels internal document/design bundle counts “Versions”. Operators
should see terminology that matches the data the server actually returns.

This is a narrow refinement of the existing console. Preserve its layout,
colors, controls, date ranges, loading behavior and network behavior.

## Required behavior

- Settings labels the shared controls “Revisions to keep” and “Pinned
  revisions allowed”. Help explicitly says each document or design uses these limits
  and can override them. Preserve the existing unpinned/pinned distinction,
  immediate pruning on lowering the revision limit, and zero semantics.
- The Versions-to-keep help identifies websites and pages as its scope.
- Analytics shows Page Views as the supported traffic metric. Remove the
  “Unique Visitors —” metric; keep the existing explanatory note that unique
  visitors are not tracked. Do not add tracking.
- For websites (`flat`) and pages (`page`), retain the Versions count from
  `flat.versions` and the existing disk description.
- For documents (`docs`) and designs (`design`), show only the formatted byte value for disk usage,
  with no parenthetical. Omit the Versions row. Do not rename
  `flat.versions` to Revisions: FlatView has no revision count and these values
  are not interchangeable. No additional API requests or new metrics.
- Unknown or missing type keeps the existing Version-model presentation for
  compatibility.

## Evidence and scope

`internal/core/service.go` exposes `versions` and `disk_bytes` in FlatView;
`disk_bytes` is the size of the flat directory, not a categorized breakdown.
`internal/core/design.go` applies quotas to both revision-model types, and
`docs/spec/content-models.md` documents the implemented shared revision store.
Only settings and analytics product modules change. Separate focused test and
review evidence files may be added. Do not change shared CSS, DOM helpers,
Share, list views, polling, loading transitions, retention or API semantics.

## Review decisions

Independent Claude review and Codex agree this correction is needed. Host
revision-limit changes call `retainAllRevisions(ctx, true)`, including docs.
The existing `isLive` helper classifies documents/designs. Existing tests and
docs contain no assertions/references needing old quota label updates.
A revision-limit decrease currently has no retention-impact confirmation;
that pre-existing behavior is outside this copy-only change. Quota help does
not claim a specific place to override limits.

## Validation

Render all four content types, missing type, and zero/nonzero traffic. Assert
supported metrics, correct presence/absence of history counts, and unchanged
7/30-day range requests. Verify both quota controls, help, and ordinary save
flow with synthetic settings. Run `go test -p 1 ./internal/console -run TestConsole -count=1`.
Also test an unknown type, quota validation labels and saved summaries. After
independent code review, inspect actual browser rendering and interactions at
desktop and mobile sizes on a disposable local development server. Public
screenshots contain only synthetic names/data and no local host details,
private addresses, credentials or internal metadata. Record review outcomes
and any limitations separately; no production/provider activation.
