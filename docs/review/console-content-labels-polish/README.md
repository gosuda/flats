# Console content-label verification

The implementation follows [the approved spec](../../spec/console-content-labels-polish.md).
Independent Claude reviews of both the document and code returned GO; Codex
accepted the scope and verified the result. No required code correction came
out of the final review.

Validation on the change:

- `go test -p 1 ./internal/console -run TestConsole -count=1` passed.
- Focused tests cover flat/page/docs/design, unknown and missing types,
  zero/nonzero traffic, 7/30-day selection, quota labels, validation and save
  summaries. The existing console UI suite also runs unchanged.
- Disposable `consoledev` server with real core/API and synthetic fixtures,
  local network stand-ins only. Chrome at 1440 × 1000 and 390 × 844.
- Browser checks: document/design omit internal version counts; website
  keeps Versions; Page Views is the only headline metric; keyboard activation
  and pointer selection of both date ranges work; both quota values save and
  use the updated names in the success summary. No horizontal page overflow
  or JavaScript page errors during these paths.
- Screenshots show only relevant content regions. Site addresses, host
  configuration, keys and browser chrome are outside the captured areas.
  Settings values are synthetic and were raised during save verification.
  Some initial captures included a fixed header or toast; the final images
  were recaptured from stable document regions without modifying DOM/styles.

| Surface | Desktop | Mobile |
| --- | --- | --- |
| Document analytics | [Image](docs-analytics-desktop.png) | [Image](docs-analytics-mobile.png) |
| Design analytics | [Image](design-analytics-desktop.png) | [Image](design-analytics-mobile.png) |
| Website analytics | [Image](website-analytics-desktop.png) | [Image](website-analytics-mobile.png) |
| Settings limits | [Image](settings-limits-desktop.png) | [Image](settings-limits-mobile.png) |

This does not change retention, tracking, or network behavior. The existing
absence of a revision-limit decrease impact confirmation remains outside
scope. Pin limits do not remove existing pins if lowered below their count.
The Impeccable launcher was unavailable (permission denied); context and
craft-floor guidance were read directly, with manual browser verification.
