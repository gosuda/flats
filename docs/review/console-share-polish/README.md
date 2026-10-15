# Share dialog verification

Screenshots are retained outside the repository as local review evidence. They show synthetic Project guide data and example.com URLs. They are cropped to the dialog; no browser chrome, real network domains, personal data or server identifiers are included.

Desktop (1280 × 900) and mobile (390 × 844) checks covered:

- Entry through the console Share menu and refresh against the development API.
- NetBird-specific access wording and public-link preparation.
- Preparing → ready updates, Copy/Visit availability and clipboard contents.
- Refresh failure preserving the last observed link state.
- Keyboard focus on Refresh and Visit, Escape dismissal and horizontal overflow.

Provider states in screenshots are synthetic. No external network provider was activated. Independent Claude review accepted the final implementation; the focused UI regression suite passed. Screenshot variants are `private`, `preparing`, and `refresh-error` at both viewport sizes.
