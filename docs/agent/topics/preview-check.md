## Check a preview in your browser

Flats does not render pages. Save `warnings` read HTML tokens only; layout,
color, contrast and script errors show up only in a real browser. When you
have a browser tool (a browser pane, Playwright, Chrome DevTools), check the
Draft preview with it once before you publish.

### Before you start

* Open the URL that `open_preview {slug, version: 0}` returned, after
  `get_flat` reports that preview `ready` (`topic.preview-verify`). Never
  build the URL yourself.
* Your browser must reach it. A Local preview (`*.localhost`) opens only on
  the Flats host itself; a Tailscale preview opens on devices the tailnet ACL
  allows. If your browser cannot reach the preview, say so; do not report a
  visual check you did not make.

### The check: four views, one pass

Look at the page in each combination, reloading after you switch:

| Width | Color scheme |
|---|---|
| desktop (about 1280px) | light, then dark |
| phone (about 390px) | light, then dark |

Set the width with your tool's viewport or device emulation, and the scheme
with its `prefers-color-scheme` emulation (Playwright:
`page.setViewportSize` and `page.emulateMedia({colorScheme: "dark"})`;
DevTools: the device toolbar and Rendering > "Emulate CSS media feature
prefers-color-scheme").

In each view:

1. Look at it: a screenshot or the live pane. Read what is actually there,
   not what the code intended.
2. Run the script below in the page (your tool's JavaScript runner, or the
   DevTools console). It returns `{viewport, scheme, problems: [...]}`.
3. Read the browser's console log through your tool for script errors and
   failed requests. The script cannot see messages logged before it ran.

Then fix everything you found in one pass, save the Draft again and
`publish` (`topic.approvals`). Do not repeat the four views in a loop. Further
polish is for the user to ask for.

### What the script reports

| `check` | Meaning |
|---|---|
| `title`, `viewport` | No `<title>`, or no viewport meta with `width=device-width`, in the rendered page. Without it a phone lays the page out at a fixed width (about 980px), so overflow is not checked meaningfully. |
| `background` | Nothing paints `body`, so the viewer's default shows through and the page can be unreadable in one scheme. |
| `overflow` | The page scrolls sideways at this width. `elements` lists the outermost offenders outside any scrolling box. |
| `image`, `stylesheet`, `resource` | A broken image, an unloaded stylesheet, or a request that failed with HTTP 400 or above (`status`). |
| `lazy-image` | One entry for all `loading="lazy"` images the browser has not requested yet (`count`, first five `src`): they are unchecked, so scroll to them or fetch their URLs. |
| `hidden-text` | Text that is invisible at rest (`opacity: 0` on it or an ancestor, or `visibility: hidden`), usually content waiting for a scroll observer. Closed menus and tooltips are fine. |
| `contrast` | Text below 4.5:1 against its background in this scheme (3:1 for large text), counting translucent backgrounds and `opacity`. |

`problems` is empty when nothing was found. It lists at most 40 entries; `truncated: true` means more were found, so fix these and run it again.

The script is a fast heuristic, not proof that a page is fine. It skips
text over background images and gradients, reads at most 500 text nodes,
looks inside open shadow roots but not closed ones, ignores CSS transforms,
blend modes and filters, and does not see content that appears only after
interaction. Your own look at each view stays the
real check.

### Thumbnail

Take one more screenshot at desktop width in light mode, save it in the
bundle (PNG or WebP, well under 500 KiB) and name it as `screenshot` in
`flats.json` (`topic.manifest`). The operator's console shows it as the
flat's thumbnail, and the save warning about a missing screenshot goes away.

### Without a browser

Fetch the page and its assets, check their status codes, and fix the save
`warnings`. Tell the user that you did not look at the rendered page; never
describe how a page looks if you have not seen it.

### Script
