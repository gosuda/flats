# Board agent evaluation

These scripts run the board spec's agent success criterion
([board.md](../../docs/spec/board.md#success-criteria), TECH §14 "Agents"):
a real MCP client sorts 30 stickies, and the result is judged automatically.

| Script | Does |
|---|---|
| `seed.mjs` | Creates a board with 30 stickies from three topics (10 each), scattered and overlapping, with colors that carry no meaning, five smaller stickies and two at 10 px, plus 7 connectors between specific stickies (three labeled, two across topics, one with explicit attachment points). Writes `baseline.json`: the seeded elements, each sticky's topic and each connector's intended ends. |
| `judge.mjs` | Reads the board through the HTTP API and checks it against the baseline. Exit 0 when every check passes, 1 when one fails, 2 on errors. `--json` writes the full result. |
| `task.md` | The task given to the agent (replace `SLUG` with the board's slug). |
| `selftest.mjs` | Checks the judge: the unsorted seed fails, a scripted correct sort passes, and single violations fail exactly their check. |
| `measure.mjs` | Font sizes as the client draws them (used by the judge). |
| `crossings.mjs` | For information only: which elements each connector line crosses, and lines that run through one of their own end elements. Never fails. |

They need Node 22 or later and a disposable Flats host. Never point them at a
host with real boards: they create boards and write to them.

```sh
go build -o /tmp/flats ./cmd/flats
/tmp/flats serve --data /tmp/bt-agent --network local --listen 127.0.0.1:17940 --local-addr 127.0.0.1:17941 --portal=false &
node scripts/board-agent-eval/selftest.mjs --url http://127.0.0.1:17940 --dir /tmp/bt-selftest
node scripts/board-agent-eval/seed.mjs --url http://127.0.0.1:17940 --slug feedback-1 --out /tmp/bt-run-1/baseline.json
```

Then give an MCP client `task.md` (with `SLUG` replaced) and connect it only to
the host's MCP server (`http://127.0.0.1:17940/mcp`), with no other tools, so
it cannot read the repository or the board's files. When it is done, judge:

```sh
node scripts/board-agent-eval/judge.mjs --url http://127.0.0.1:17940 --slug feedback-1 --baseline /tmp/bt-run-1/baseline.json --json /tmp/bt-run-1/judge.json
```

## Checks

The first five are the spec's criterion; `sorted` is the task itself.

- **kept**: every seeded element exists with the same id, type and text
  (sticky text and connector labels). Added elements are listed, not refused.
- **visible**: no element is hidden under a frame. The client draws in
  `(z, id)` order and a frame's fill (anything but `none`) is opaque, so an
  element or connector line below a frame in that order and inside its box
  cannot be seen there. An element that exists in the data but cannot be seen
  is not kept for the people looking at the board.
- **overlap**: no two elements other than frames and connectors overlap,
  using each element's box (`x`, `y`, `w`, `h`; the bounding box when
  rotated). Two boxes overlap when they share more than 0.5 px in both
  directions; touching edges do not count. Frames may contain elements, so
  instead: frames do not overlap each other; an element over a frame is one
  of its members and lies wholly inside it; and no element covers a frame's
  name, drawn 8 px above the frame at 13 px. Connector lines and labels are
  not boxes and are not checked.
- **text**: every text is drawn at 12 px or more at 100% zoom and is not cut
  off. The judge runs the client's own `fitText` from
  `internal/board/web/render.js` with the client's box rules: a sticky
  starts at 16 px with `fontSize: "auto"` and shrinks to fit (down to 10 px),
  inside its box less 12 px padding and a 12 px foot (26 px when it shows a
  tailnet author's name); a fixed size does not shrink and can be cut off. A
  shape shrinks from its `fontSize`; text and connector labels keep theirs.
  Frame names are always 13 px. By default the code runs in headless Chrome
  for real font metrics (`--measure chrome`, or `CHROME=/path`); `--measure
  approx` estimates glyph widths in Node and is only a fallback.
- **connectors**: each seeded connector still exists, and both ends are
  attached (`el`) to the intended elements. Reversed ends are accepted with a
  note. Connectors the agent added are listed as notes.
- **sorted**: each topic's stickies share one frame, that frame has a name,
  and no two topics share a frame. Frame names are reported, not judged.

## Limits

- Font metrics are those of the machine running the judge (system fonts):
  another OS draws other widths, so a sticky near a wrap boundary can shrink
  differently there.
- Overlap uses boxes. An ellipse or diamond's empty corners count, and a
  connector crossing a sticky does not.
- An agent run is not deterministic; run it several times.
