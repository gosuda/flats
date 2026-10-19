# Board performance harness

Measures a board against the performance rows of the board
[success criteria](../../docs/spec/board.md#success-criteria): pan and zoom
frame times, first interactive screen, remote display latency and
concurrent-editing consistency. The reference data comes from
[`internal/board/benchdata`](../../internal/board/benchdata).

The scripts need Node.js 22 or later (they use its global `WebSocket`) and
Google Chrome. They drive Chrome over the Chrome DevTools Protocol directly,
with no packages to install. Each run starts Chrome with its own temporary
profile and removes it afterwards.

## Prepare a host and the boards

Use a disposable development host, never a production one.

```sh
go build -o /tmp/board-perf/flats ./cmd/flats
go run ./internal/board/benchdata/cmd/benchdata -mix 5k  -o /tmp/board-perf/mixed-5k.zip
go run ./internal/board/benchdata/cmd/benchdata -mix 10k -o /tmp/board-perf/mixed-10k.zip

/tmp/board-perf/flats serve --data /tmp/board-perf/data --network local \
  --listen 127.0.0.1:17920 --local-addr 127.0.0.1:17921 &

export FLATS_URL=http://127.0.0.1:17920
/tmp/board-perf/flats board import bench5k  /tmp/board-perf/mixed-5k.zip  --replace
/tmp/board-perf/flats board import bench10k /tmp/board-perf/mixed-10k.zip --replace
/tmp/board-perf/flats board import lat5k    /tmp/board-perf/mixed-5k.zip  --replace
```

The import creates each board. Its Private local URL is
`http://<slug>.localhost:17921`. The latency run edits its board, so give it
a board of its own (`lat5k`) and keep `bench5k` unchanged for the other runs.
Import `lat5k` again before every latency run: the cross-bundle moves make
stickies drift, and the run stops when too few remain in the observer's view.

## Run

Each script prints a JSON summary; `--out file.json` also saves the raw
samples. `--headless` uses headless Chrome; without it a normal window opens.
`--width`/`--height` set the window size (default 1440×900), `--scale` forces a
device scale factor, `--chrome` names another Chrome executable.

```sh
cd scripts/board-perf
node pan-zoom.mjs       --url http://bench5k.localhost:17921  --expected 5000  --out pz5k.json
node pan-zoom.mjs       --url http://bench10k.localhost:17921 --expected 10000 --out pz10k.json
node first-screen.mjs   --url http://bench5k.localhost:17921  --expected 5000 --runs 5
node first-screen.mjs   --url http://bench5k.localhost:17921  --expected 5000 --runs 5 --rtt 50 --mbps 50
node remote-latency.mjs --url http://lat5k.localhost:17921    --expected 5000 --minutes 10
node remote-latency.mjs --url http://lat5k.localhost:17921    --expected 5000 --minutes 1 --inject-divergence
```

`pan-zoom.mjs` exits with status 1 when its content guard fails (see below);
a failed run is not a measurement. Frame times need a headed run: headless
Chrome paces its own frames (in smoke runs the intervals stayed pinned near
16.7 ms), so use `--headless` only to check that the script works.

| Script | Criterion | What it measures |
|---|---|---|
| `pan-zoom.mjs` | Pan and zoom | Waits until every element is loaded and drawn, then runs a fixed 60-second script of wheel pans and ctrl+wheel zooms in nine phases: slow pan at the board's "fit all" view, zoom to 100%, two pans at 100%, zoom to 50%, pan at 50%, zooming between 25% and 200%, zoom to 25%, fast pan at 25%. On the reference boards "fit all" is clamped to the 10% minimum zoom, so it shows the central part of the board (840–1,460 elements in view during smoke runs at the default window), not all of it. Reports animation-frame intervals (p50/p95/max), missed frames (intervals over 1.5 display periods), the renderer's script time per frame, long animation frames, page errors, and per phase the frame times, zoom range and elements in view. Each frame records the camera (x, y, zoom) it shows and, counted after the script, the elements in view. |
| `first-screen.mjs` | First screen | Cold: a new Chrome profile opens the board. Warm: the same browser opens it again. "Interactive" is the frame that shows a pan dispatched as soon as the snapshot is loaded, the first fit applied and the view fully drawn. `--rtt`/`--mbps` emulate the reference network in Chrome. |
| `remote-latency.mjs` | Remote display, concurrent editing | Five load bots (the board's own `state.js`/`net.js` client in Node) each send two edits per second for ten minutes to stickies in an observer browser's view, plus one cross-bundle round per second and presence traffic (below). Latency is the bot's send time to the start of the frame after the observer drew the change, for edits that set text. Afterwards it compares every element on every screen with a fresh server snapshot and checks that the last acknowledged write of each property stayed. `--inject-divergence` changes one element in one bot's copy, so the check must report it (a negative control). |

## Content guard of the pan and zoom script

A frame time over empty canvas measures nothing, so `pan-zoom.mjs` keeps the
view over content and fails the run when it cannot:

- Every phase starts at a known place: the first at "fit all", every later
  one centred on an anchor sticky (in the central half of the content, the
  sticky with the most elements around it at 100%), at the zoom reached so
  far. The jump is one frame and counts in that phase's frame times.
- Pans keep their speed but turn when the next step would take the view
  centre out of the content bounds, or when the elements ahead (estimated
  from a grid of element centres, constant time per frame) would fall under
  half of the phase start's estimate. `setup.panTurns` counts the turns.
- Zooms happen at the centre of the view, which is over content.
- After the script the page counts, for every recorded frame, the elements
  in view with the same spatial-index query the renderer uses to choose what
  to draw (frames are containers and are not counted). Counting afterwards
  keeps the measured frames free of the extra work. The viewer edits
  nothing; the run fails if the board's state changed while it ran.
- Each phase's reference is the count at its start position. Every frame of
  the phase must show at least `--floor` (default 0.2) of the reference,
  scaled by the view-area ratio `(z_ref / z)^2` when the frame is zoomed in
  further than the reference. A reference under `--min-ref` (default 5)
  elements fails too. The result's `guard` lists each phase's reference,
  minimum, median and maximum elements in view and the frames under the
  floor; `guard.pass` is false and the exit status 1 on any failure.

`--seconds` shortens the whole script proportionally (for a smoke run);
measurements use the default 60.

## What the concurrency check exercises

`remote-latency.mjs` writes these conflicts, all on stickies in the
observer's view:

- Owned stickies: five per bot, written only by that bot (text and a move
  in one batch).
- Same-bundle conflicts: one edit in three of every bot goes to a pool of
  four shared stickies, setting text and color in one batch, so different
  bots write the same two bundles of the same element.
- Cross-bundle conflicts (`--cross`, default 2 stickies; `--cross-rate`,
  default one round per second): in one round every bot, in the same
  event-loop turn, writes one bundle of the same sticky by its role (bot
  index mod 3): a move (`geom`), a recolor (`style.color`) or a text edit.
  With five bots one element receives two moves, two recolors and one text
  edit at once. Each bot writes from its own copy. `crossBundle` reports
  the rounds, the spread of send times per round and the rounds in which
  every batch was sent before any of them was acknowledged.
- Presence (`--pres-rate`, default 10 per second per bot; the limit is 20):
  every bot sends its cursor (circling the sticky it edited last), view
  rectangle and selection, alternately with an editing mark or a drag
  preview, so the observer draws remote cursors during the load. `presence`
  counts messages sent by the bots and received by the observer and bots.

The checks afterwards: every element of every screen equals the server
snapshot (content and full form), and for every element and bundle the
last write acknowledged `ok` is the stored value; `crossBundle.lost` gives
the result for the cross-bundle stickies. "Last" is the order of the
server's acknowledgement sequence numbers, so the check finds lost or
misapplied writes (including a write of one bundle overwriting another
bundle of the same element), not a wrong order chosen by the server. The
negative control `--inject-divergence` exercises the screen comparison
only; the last-write check has no negative control. Not exercised: deletes, frames, groups, guards, undo, rollback and offline
edits. Text latency is measured only for edits that set text; the moves and
recolors of the cross-bundle rounds count under `edits.withoutTextToken`.

## Notes on the definitions

- The server's receipt time of an edit is not visible outside the server.
  `remote-latency.mjs` therefore reports send-to-display, an upper bound of the
  criterion's receipt-to-display that also includes the client-to-server hop.
- Bot and page times are wall-clock milliseconds of the same machine
  (`performance.timeOrigin + performance.now()`), so the bots and the browser
  must run on one machine.
- On a 60 Hz display, frame intervals sit near 16.7 ms with about a
  millisecond of timestamp jitter. Read the p95 frame time together with the
  missed-frame count.
- Loopback hides network time. Use `--rtt`/`--mbps` for the first screen, and
  add the one-way network delay to loopback latency results.
- Record the machine, Chrome and OS versions (each result includes them) and
  the load average before and after each run (`uptimeBefore`/`uptimeAfter`).
  Other work on the machine changes the numbers.
