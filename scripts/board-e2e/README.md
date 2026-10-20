# Board browser end-to-end checks

`run.mjs` verifies the board canvas app in headless Chrome over the Chrome
DevTools Protocol, as the "Browser automation" row of the
[board validation plan](../../docs/spec/board-TECH.md#14-validation-plan)
asks. It needs Node 22 or later (global `WebSocket`, `zlib.crc32`), Google
Chrome and the `flats` CLI. It has no npm dependencies.

The script works against a running Flats host. It creates boards with the
CLI, opens them in fresh browser contexts with a temporary Chrome profile,
and edits them with real mouse, keyboard and IME input events
(`Input.dispatchMouseEvent`, `Input.dispatchKeyEvent`,
`Input.imeSetComposition`). It judges the results from the page state, from
the server through `flats board get`, and from downloaded files. All data is
synthetic.

## Run it

Use a disposable development host, never production: the script creates
boards and, with `--public adapter`, enables a provider and approves
visibility changes in the host's console.

```sh
go build -o /tmp/board-e2e/flats ./cmd/flats
/tmp/board-e2e/flats serve --data /tmp/board-e2e/data --network local \
  --listen 127.0.0.1:17930 --local-addr 127.0.0.1:17931 &
node scripts/board-e2e/run.mjs --console http://127.0.0.1:17930 \
  --flats /tmp/board-e2e/flats --out /tmp/board-e2e/out
```

The script prints one line per check and a check list, writes
`results.json`, screenshots and the exported PNG and data file to `--out`,
and exits 1 when any check fails.

Chrome runs in a process group of its own with a new temporary profile.
When the run ends, the script ends that group and any process of the Chrome
executable (on macOS, its application bundle) whose `--user-data-dir` is
exactly that profile, removes the profile, and
records a final `harness` check that no browser process is left. When the
process list cannot be read, that check fails instead of passing.

`node --test scripts/board-e2e/lib/lib.test.mjs` tests the helpers that need
no browser or host: which processes belong to the run's browser, and PNG
decoding with every filter type.

| Option | Meaning |
|---|---|
| `--console URL` | Management address of the host (default `http://127.0.0.1:7878`) |
| `--flats PATH` | `flats` CLI used to create, read and import boards (it gets `--url`) |
| `--chrome PATH` | Chrome executable (default: the macOS application) |
| `--out DIR` | Results, screenshots and downloads (default: a new temporary directory) |
| `--only a,b` / `--skip a,b` | Run or skip sections: `tools select keys text collab a11y find share png data history converge public` (`setup` always runs) |
| `--public adapter` | Also run the Public boundary checks (see below) |
| `--headful` | Show the browser window |

## Public boundary checks

A Public route needs a public provider, and every real provider (Portal,
Funnel, zrok, Cloudflare, NetBird Expose) reaches the internet. The
`public` section therefore runs only against the loopback lifecycle test
host, whose production provider manager uses a loopback Portal double:

```sh
go build -o /tmp/board-e2e/flats-adapter ./internal/lifecyclecheck/adapter
/tmp/board-e2e/flats-adapter serve --data /tmp/board-e2e/public-data \
  --listen 127.0.0.1:17932 --local-addr 127.0.0.1:17933 &
node scripts/board-e2e/run.mjs --console http://127.0.0.1:17932 \
  --flats /tmp/board-e2e/flats-adapter --only public --public adapter
```

The section enables the double for the host (`/__gate/host-permission`),
allows Portal for the test board on the console's flat settings page, and
approves the Public and Private requests on the console's approval pages.
These are operator actions done by the test in the browser; `results.json`
lists each one under `interventions`. Use it only on a disposable host.

## What it covers and what it does not

Covered: every tool, selection, move, resize, rotate, group, stacking order,
delete, tool and editing shortcuts, undo and redo, copy and paste, IME
composition during remote changes, concurrent edits from two browser
contexts, the accessible element list, find, Send to agent, PNG export,
data file export and import, History (list, view, pin, element recovery,
rollback), theme, and the Public boundary.

Each check in the `tools`, `select`, `keys`, `text`, `collab`, `a11y` and
`history` sections ends with a server cross-check: once the open editors
have no unsent edits, every element they show as saved must equal what
`flats board get` returns (ids, types, geometry, stacking keys, frames,
groups, locks, text and styles).

The `png` section also exports a small board with known content
(`lib/pixels.mjs`) at 1x and 2x in the light theme and checks pixels
against an expectation computed from the board, the 32px margin, the
frame's name band and the theme palette: the image size, solid colors at
each element and in the frame, sticky widths and left edges, and the frame
name's position above the frame. A separate check expects the frame name to
double in height in a 2x export, like the rest of the image.

Limits:

- Copy and paste use synthetic `ClipboardEvent`s with a script-made
  `DataTransfer`, so the system clipboard is never touched. The browser's
  own Cmd/Ctrl+C and Cmd/Ctrl+V handling is not exercised.
- Keyboard shortcuts are sent with Ctrl, which the app treats like Cmd.
- IME input uses `Input.imeSetComposition`; a real Korean IME, Safari, iPad
  touch and pen, and VoiceOver remain manual checks (board-TECH.md section
  14, "Real devices").
- Checks use the app's state (`window.board`) to find on-screen targets
  and to wait for results. The server cross-check confirms the saved
  elements, not presence, selection, the camera or other screen-only state.
  The `find`, `share`, `png`, `data` and `converge` sections have no
  cross-check after each check; `data` and `converge` compare with the CLI
  themselves.
- The pixel check covers one small board in the light theme: stickies, a
  rectangle and a frame. Text inside elements, connectors, pens, images,
  rotation and the dark theme are not checked pixel by pixel.
- Disk quota, limits and performance are not covered here.
