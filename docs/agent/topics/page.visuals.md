## Charts and diagrams in pages

How to make the visuals of a page (`topic.page`) carry information and
stay readable. The page contract in `topic.design` still applies.

### Pick the form before the colors

Decide what the reader must do with the numbers, then pick the form. Color
comes last.

| The reader should | Use |
|---|---|
| see one current value, maybe with its change | a stat tile (value, signed delta against a named period); not a one-bar chart |
| compare a few headline numbers | a row of stat tiles, only when those numbers are the point of the page |
| compare magnitudes | bars or columns; a heatmap for a grid |
| follow a trend over time | a line; an area only for a single series |
| tell a few series apart | grouped or stacked bars, or several lines |
| see why one series matters | emphasis: that series in the accent color, the rest in a muted grey |
| see above or below a baseline or target | a diverging bar, or a line against the baseline |
| see parts of a whole | a stacked bar; a pie or donut only for at most a few clearly different shares |
| read more than about seven categories | a table, or a table next to a smaller chart |

Never put two y-axes on one chart: the alignment of the two scales is
arbitrary and suggests a relation the data does not have. Use two charts, or
index both series to a common start (= 100).

### Color by its job

* **Identity** (which series): a fixed order of distinct hues, assigned to
  each series once. A series keeps its color when filters hide others. Past
  six to eight series, fold the rest into "Other" or split into small
  multiples instead of inventing more hues.
* **Magnitude**: one hue from light to dark. No rainbow scales.
* **Polarity** (above/below): two hues that read as opposites (blue and
  orange, not blue and teal) with a neutral grey midpoint.
* **Status** (good, warning, critical): reserved for status, always with a
  text label or icon, never reused as a series color.
* Every chart color is a theme token with a dark value chosen for the dark
  background, not an automatic inversion. Check that neighboring series stay
  distinguishable for color-blind readers (a simulator in your browser's
  developer tools, or a palette checker); when two are close, add a second
  cue (direct labels, line style, markers).
* Text is never set in a series color: labels, values and legends use the
  page's text tokens, and a colored mark beside the text (a dot, a short
  line, a swatch) carries the identity.

### Marks, labels and scale

* Draw every mark, tick and label with one scale per axis, and only label
  values the chart actually reaches. Leave room in the SVG `viewBox` for the
  outermost labels, and give every shape an explicit fill or stroke.
* Keep marks light: lines about 2px, markers at least 8px across, bars that
  do not fill their whole slot, area fills as a faint wash. Gridlines and
  axes are thin, solid and quiet; dashed lines suggest a threshold or a
  forecast, so keep dashes for those.
* Two or more series get a legend. Direct labels supplement it, sparingly:
  the last point, the extreme, the series the text is about. Never a number
  on every point. Round axis ticks to clean values with thousands separators.
* A label that does not fit inside its bar goes outside the bar end or into
  the tooltip; never clip it.
* Give the chart's container room for its axis labels, so the chart never
  gets its own scrollbar.

### Interaction: hover, tap and keyboard

A chart in a page is interactive by default; a bare stat tile is the
exception.

* Lines and areas: a vertical guide that snaps to the nearest data position,
  with one tooltip listing every series at that position. Bars, dots and
  cells: a tooltip per mark, and the hovered mark visibly responds.
* The hit area is larger than the mark: a whole column per data position,
  or at least 24px around a point. Nobody should have to land on a 2px line.
* Hover is not available on touch screens or to keyboard users: show the
  same tooltip on tap (`pointerdown`) and on keyboard focus (focusable hit
  areas with `tabindex="0"` and an `aria-label` that states the values).
* In the tooltip the value is the prominent part and the series name is
  secondary. Keep it inside the chart's box: flip it below the point when
  there is no room above, and clamp it at the sides.
* Tooltips add detail; they never hold the only copy of a value. Every
  value is also in direct labels or a data table.
* Insert category and series names with `textContent`, not `innerHTML`:
  they often come from a CSV header or a tool's output.
* Respect `prefers-reduced-motion` for tooltip and chart transitions.

### Data tables and text alternatives

Give every chart a caption that states its point (`<figure>` with
`<figcaption>`), `role="img"` with an `aria-label` or `<desc>` on the SVG,
and the exact numbers in a table (it may sit in a `<details>` block). Long
tables scroll in their own `overflow-x: auto` box; numeric columns are
right-aligned with `font-variant-numeric: tabular-nums`. Large standalone
figures (stat tiles) keep proportional digits.

### Diagrams

Draw a diagram when it shows a mechanism a reader would otherwise assemble
from prose: where data flows, which parts talk, what state something moves
through, what differs between two options. If a sentence says it faster,
write the sentence.

* Draw the mechanism, not its name: the path through the parts and the
  boundary being crossed. For a comparison, draw both options and the edge
  that differs between them.
* Label arrows with what happens on them (`writes`, `polls every 30s`); use
  a legend only when an encoding repeats.
* Hand-written inline SVG is enough: `viewBox` sized to the content and
  scaled with CSS (`max-width: 100%; height: auto`), strokes and text in
  `currentColor` so both schemes work, arrowheads as `<marker>` or a small
  polygon, text about 11-13px, shapes aligned to a grid.
* One figure, one claim: wrap it in `<figure>` with a `<figcaption>` that
  states it, and give the SVG `role="img"` and an `aria-label` with the same
  claim.
* Mermaid also works in a page when you bundle the library (or pin an exact
  CDN version) and render `<pre class="mermaid">` blocks; give it theme
  variables for both schemes.

### Check the chart

In the one preview check before publishing (`topic.preview-check`), spend the
look on the charts: labels that collide or clip, marks outside the axes, a
tooltip that leaves the chart, colors that vanish in dark mode, and the phone
width.
