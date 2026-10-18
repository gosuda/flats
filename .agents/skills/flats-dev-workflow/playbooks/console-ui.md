# Playbook: console, docs editor and design UI

Covers `internal/console/static/`, the docs editor in `internal/apps/docs/_web/`
and the design app in `internal/design/web/`.

## Build

- [ ] Ask for confirmation in the page: the console's `confirmDialog`
  (`assets/js/ui.js`), the design app's `confirmDialog`, or an inline banner as
  in the docs History panel. Do not use native `confirm()`: some embedded and
  automated browsers never show it.
- [ ] Byte-size settings compare stored byte counts, not formatted text, and
  show values exactly (`byteValue` and `toBase` in `assets/js/settings.js`).
  Do not reintroduce rounded display or text comparison.
- [ ] An empty Go slice can serialize as `null`. Treat `null` as an empty list
  (`r.approvals || []`), and keep that separate from a missing or malformed
  response.
- [ ] Extend the existing UI tests (`internal/console/console_ui_test.go`,
  `internal/console/testdata/`) and reuse their cleanup before adding new
  shims.

## Verify

- [ ] Check every changed screen in a real browser against a disposable
  development host with synthetic data. Unit tests alone do not show that a
  change reached the screen.
- [ ] Console API calls need same-origin browser headers. Perform approvals and
  settings changes in the browser; do not forge the headers with a script.
- [ ] Design artboards run at an opaque origin
  ([Designs](../../../../docs/design.md#designs)). Some embedded browsers block
  such frames or their requests; if artboards do not render, check in a
  standard or headless browser before calling it a product bug.
- [ ] Long element screenshots can composite a fixed header or toast over the
  content. Capture from a fresh page at a stable scroll position and inspect
  every image.

## Screenshots in a PR

- [ ] Use synthetic data only: no real hostnames, tailnet names, personal data
  or server internals.
- [ ] Never commit screenshots to the repository. Attach them to the PR with
  the hosting service's native attachments, and keep the originals outside the
  worktree.
