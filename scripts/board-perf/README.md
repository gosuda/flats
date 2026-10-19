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

## Run

Each script prints a JSON summary; `--out file.json` also saves the raw
samples. `--headless` uses headless Chrome; without it a normal window opens.
`--width`/`--height` set the window size (default 1440×900), `--scale` forces a
device scale factor, `--chrome` names another Chrome executable.

```sh
cd scripts/board-perf
node pan-zoom.mjs       --url http://bench5k.localhost:17921  --expected 5000
node pan-zoom.mjs       --url http://bench10k.localhost:17921 --expected 10000
node first-screen.mjs   --url http://bench5k.localhost:17921  --expected 5000 --runs 5
node first-screen.mjs   --url http://bench5k.localhost:17921  --expected 5000 --runs 5 --rtt 50 --mbps 50
node remote-latency.mjs --url http://lat5k.localhost:17921    --expected 5000 --minutes 10
```

| Script | Criterion | What it measures |
|---|---|---|
| `pan-zoom.mjs` | Pan and zoom | Waits until every element is loaded and drawn, then runs a fixed 60-second script of wheel pans and ctrl+wheel zooms (whole board in view, 100%, 50%, zooming between 25% and 200%, 25%). Reports animation-frame intervals (p50/p95/max), missed frames (intervals over 1.5 display periods), the renderer's script time per frame, long animation frames and page errors. |
| `first-screen.mjs` | First screen | Cold: a new Chrome profile opens the board. Warm: the same browser opens it again. "Interactive" is the frame that shows a pan dispatched as soon as the snapshot is loaded, the first fit applied and the view fully drawn. `--rtt`/`--mbps` emulate the reference network in Chrome. |
| `remote-latency.mjs` | Remote display, concurrent editing | Five load bots (the board's own `state.js`/`net.js` client in Node) each send two edits per second for ten minutes to stickies in an observer browser's view. Latency is the bot's send time to the start of the frame after the observer drew the change. Afterwards it compares every element on every screen with a fresh server snapshot and checks that the last acknowledged write of each property stayed. `--inject-divergence` changes one element in one bot's copy, so the check must report it (a negative control). |

Notes on the definitions:

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
