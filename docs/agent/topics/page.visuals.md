## Charts and diagrams in pages

How to make the visuals of a page (`topic.page`) carry information and
stay readable. The page contract in `topic.design` still applies. These are
defaults: depart from one when the content reads better without it.

### Pick the form before the colors

Decide what the reader must do with the numbers, then pick the form. Color
comes last.

| The reader should | Use |
|---|---|
| see one current value, maybe with its change | a stat tile (value, signed delta against a named period); not a one-bar chart |
| compare magnitudes | bars or columns, sorted when the order of the categories carries no meaning; a heatmap for a grid |
| follow a trend over time | a line; an area only for a single series |
| compare several series | grouped bars, several lines, or small multiples |
| see the parts of a meaningful whole | a stacked bar of additive parts; a pie or donut only for a few clearly different shares |
| see why one series matters | emphasis: that series in the accent color, the others quieter but still visible |
| see above or below a baseline or target | a diverging bar, or a line against the baseline |
| see a relationship between two measures | a scatterplot |
| see how values are distributed | a histogram or a box plot |
| look up many exact values | a table, possibly next to a smaller chart |

Never put two y-axes on one chart: the alignment of the two scales is
arbitrary and suggests a relation the data does not have. Use two charts, or
index both series to a common start (= 100).

Every chart states its source, units and period, and says what a gap or
missing value means.

### Scale and layout

* Map marks and ticks through the same scale. Bars and areas start at zero;
  a line chart may use a tighter range when its axis is clearly labeled. Use
  rounded domain bounds that contain all the data, and label units and any
  logarithmic scale.
* Leave room in the SVG `viewBox` for the outermost labels, give every shape
  an explicit fill or stroke, and give the chart's container room for its
  axis labels.
* At phone width, keep labels readable first: fewer ticks, shorter labels, a
  horizontal bar chart instead of columns, or small multiples. Shrinking an
  SVG also shrinks its text and hit areas, so when a chart cannot be
  simplified, give it a minimum readable width inside its own labeled
  `overflow-x: auto` box (the one exception to the phone-width rule in
  `topic.design` that charts share with tables, code and diagrams). The page
  body never scrolls sideways.

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
* Essential marks need at least 3:1 contrast against the background and
  against neighboring marks. Check that neighboring series stay
  distinguishable for color-blind readers (your browser's vision-deficiency
  emulation, or a palette checker); when two are close, add a second cue
  (direct labels, line style, markers).
* Every chart color is a theme token with a dark value chosen for the dark
  background, not an automatic inversion. SVG drawn in the page references
  the tokens through CSS (`fill: var(--series-1)`), so it follows the scheme
  by itself. A canvas or library chart copies colors when it draws: read the
  tokens with `getComputedStyle` and redraw when
  `matchMedia("(prefers-color-scheme: dark)")` changes, on `beforeprint` and
  again on `afterprint`.
* Text is never set in a series color: labels, values and legends use the
  page's text tokens, and a colored mark beside the text (a dot, a short
  line, a swatch) carries the identity.

### Marks and labels

* Keep marks light: lines about 2px, markers at least 8px across, bars that
  do not fill their whole slot, area fills as a faint wash. Gridlines and
  axes are thin, solid and quiet; keep dashed lines for thresholds and
  forecasts.
* Make identity readable without color matching: direct labels on the
  series (at the last point, for example) or, when they would collide, a
  legend. Label values sparingly (the last point, the extreme, the series the
  text is about) rather than every point, unless there are only a few. Round
  axis ticks to clean values with thousands separators.
* A label that does not fit inside its bar goes outside the bar end or into
  the tooltip; never clip it.

### Interaction

Interaction is worth adding when it reveals useful detail (exact values of a
dense chart) or supports exploration. A clearly labeled static chart with a
data table is enough for a simple figure.

* Lines and areas: a vertical guide that snaps to the nearest data position,
  with one tooltip listing every series there. Bars, dots and cells: a
  tooltip per mark, and the hovered mark visibly responds.
* The hit area is larger than the mark: a whole column per data position,
  or at least 24px around a point.
* Touch and keyboard get the same details. Open the tooltip on `click`
  (a tap; `pointerdown` also fires when a reader starts scrolling) and on
  keyboard focus. Keep it open while the pointer is over the trigger or the
  tooltip, close it with Escape, on blur and on a tap elsewhere.
* Keep keyboard stops bounded: one focusable chart (or one per series) that
  moves between data points with the arrow keys, rather than one Tab stop
  per datum. Announce the focused point with an `aria-label` on the focused
  element or a polite live region.
* In the tooltip the value is the prominent part and the series name is
  secondary. Keep it inside the chart's box: flip it below the point when
  there is no room above, and clamp it at the sides. When it adds a
  description to a focused element, give it `role="tooltip"` and point to it
  with `aria-describedby`.
* Tooltips add detail; they never hold the only copy of a value. Every
  value is also in direct labels or a data table.
* Insert category and series names with `textContent`, not `innerHTML`:
  they often come from a CSV header or a tool's output.
* Respect `prefers-reduced-motion` for tooltip and chart transitions.

### Text alternatives and data tables

* Wrap every chart in `<figure>` with a `<figcaption>` that states its
  point.
* A static SVG chart gets `role="img"` and `aria-labelledby` pointing to its
  `<title>` or the caption; a `<desc>` adds a longer description, not the
  name. An interactive chart keeps its focusable elements reachable: do not
  put `role="img"` on a container that holds them.
* Put the exact numbers in a table (it may sit in a `<details>` block).
  Numeric columns are right-aligned with `font-variant-numeric:
  tabular-nums`; large standalone figures (stat tiles) keep proportional
  digits.

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
  polygon, text about 11-13px at the drawn size, shapes aligned to a grid.
  Caption and label it like a static chart.
* Mermaid also works in a page when you bundle the library (or pin an exact
  CDN version) and render `<pre class="mermaid">` blocks. Its colors are
  rendering settings, not CSS: use `theme: "base"` with `themeVariables` set
  to the token colors converted to hex (`#rrggbb`), keep each diagram's
  source, and render again
  from that source when the color scheme changes.

### Check the chart

In the one preview check before publishing (`topic.preview-check`), spend the
look on the charts: labels that collide or clip, marks outside the axes, a
tooltip that leaves the chart, colors that vanish in dark mode, and the phone
width. Switch the color scheme without reloading once, to catch charts that
do not redraw.
