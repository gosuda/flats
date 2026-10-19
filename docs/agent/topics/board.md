## Boards

A board (`type: "board"`) is an infinite-canvas whiteboard of sticky notes,
shapes, connectors, text, pen strokes, images and frames. People edit it in
the browser at the same time as you, and see each other's cursors. You read
and write it as structured data.

Choose a board to spread out, group and connect ideas: brainstorms, sorted
feedback, flowcharts, retrospectives and plans that people will rearrange by
hand. Choose a design (`topic.canvas`) for pixel-exact screens, prototypes
and posters, a document (`topic.docs`) for prose, and a page (`topic.page`)
for one finished HTML document with charts.

A board runs at once and has no Draft, versions, previews, publish or
approval. A new board is Private. Making it Public, Private again, or
deleting it waits for the operator (`topic.approvals`). A Public board shows
every change to everyone at once, so tell the user before you write to one.
Version tools refuse a board (`refusal.invalid`); board tools refuse other
types (`refusal.not_board`).

### Create and read

1. `create_flat {slug, name?, type: "board"}` creates an empty board, or
   `update_board` on a new slug creates it and applies your operations. Give
   the user `private_url`: it opens the canvas.
2. Read before you change an existing board. People may have edited since you
   last looked. `get_board {slug, format?, frame?, rect?, ids?, revision?,
   limit?, cursor?}` returns `{seq, revision, pending, elements, next?}`.
   - `format: "outline"` lists element types per frame, short ids, text and
     connections ("A → B"). Use it to understand a board cheaply, and
     `"full"` (the default) for coordinates and styles before you move things.
   - `frame`, `rect` (an area `[x, y, w, h]`) or `ids` narrow the read.
   - A page holds up to `limit` elements (default 500, at most 2,000) or
     1 MiB. Pass `next` as `cursor` for the following page. Every page comes
     from the same state, even while people edit. A cursor lasts 60 seconds;
     an expired one is refused with `conflict`, so read again from the start.
   - `seq` identifies the current state. Each element carries `ver`, which
     grows with every change to it, including a rollback or replacement that
     only gives a pen other points or an image another picture. `pending`
     says people have edits that are not a revision yet.
   - `revision: N` reads an earlier revision (no cursor expiry).

### Write

`update_board {slug, op_id, ops, layout?, assets?, if_seq?, message?,
replace?}` applies all operations or none, and is one revision.

```json
{"op_id": "sort-onboarding-1", "if_seq": 233,
 "ops": [
  {"op": "add", "ref": "$f1", "el": {"type": "frame", "text": "Onboarding", "x": 0, "y": 0, "w": 900, "h": 600}},
  {"op": "add", "ref": "$s1", "el": {"type": "sticky", "text": "Fewer sign-up steps"}},
  {"op": "reparent", "ids": ["$s1", "e_7Hk2pQ9xLm"], "frame": "$f1"},
  {"op": "set", "id": "e_7Hk2pQ9xLm", "fields": {"style": {"color": "blue"}}, "if_ver": 57},
  {"op": "add", "el": {"type": "connector", "ends": [{"el": "$s1"}, {"el": "e_7Hk2pQ9xLm"}]}},
  {"op": "delete", "ids": ["e_old0000001"]}],
 "layout": [{"kind": "grid", "ids": ["$s1", "e_7Hk2pQ9xLm"], "in": "$f1", "columns": 4, "gap": 24}],
 "message": "Group the onboarding feedback"}
```

- `add` creates an element. A `ref` starting with `$` names it for later
  operations and layouts in the same call; the result's `ids` maps each ref
  to the new id. Without coordinates and without a layout, new elements go
  in a row to the right of the board. Default sizes: sticky 200×200, shape
  160×100, text fitted to its content.
- A frame you add without `z` goes behind every element, so the elements you
  move into it stay visible; several new frames in one call keep their
  order. Any other new element goes in front. A frame's fill hides what is
  behind it, so never give a frame a `z` above its members.
- `set {id, fields}` changes named fields. Elements and fields you do not
  name stay as they are, and `style` changes only the keys you give. A
  field cannot be `null`: it never clears text or unlocks. Change `frame`
  and `group` with `reparent`, `group` and `ungroup`.
- `reparent {ids, frame}` moves elements into a frame, or out with `null`.
  Grouped elements move together. Frames do not nest.
- `delete {ids}` deletes elements. Deleting a frame keeps its members;
  connectors attached to a deleted element keep a free end where they were
  attached, kept within the canvas range (±10,000,000).
- `group`, `ungroup`, `zorder` and `delete_frame_contents` group elements,
  change stacking order and delete a frame with its members. `group` makes
  a new group id unless you pass one; group ids are never reused, so an id
  the board used before is refused.
- Moving a frame by `x` or `y` alone moves its members (`carry: "members"`
  is the default). A `set` that also changes `w` or `h` leaves members in
  place unless you pass `carry: "members"`. `carry: false` never moves them,
  and `carry: "group"` moves the element's group with it.
- A locked element refuses the call unless an earlier operation in the same
  call sets `locked: false`. Unlock only what the user asked you to change.
- `layout` places elements in input order: `grid` (with `columns` and
  `gap`), `row` or `column`. With `in`, placement starts 32 px inside that
  frame, and the frame grows to fit. The same input gives the same
  coordinates; the result's `layout` returns them as `{id: [x, y]}`. Layout
  runs after the operations, so it sees their result.
- Connector ends keep the side they were attached to when their elements
  move. When you move or lay out elements that connectors join, set those
  connectors' ends again without `at` in the same call, for example
  `{"op": "set", "id": "e_conn000001", "fields": {"ends": [{"el": "e_7Hk2pQ9xLm"}, {"el": "$s1"}]}}`.
  The host then picks the facing sides from the final places, after
  `layout`. Otherwise a line can leave a note on its far side and run
  through its text.
- `assets` adds images for image elements in the same call:
  `[{"ref": "$logo", "content": "<base64>"}]`. An image element gives the
  ref as its `asset`, for example
  `{"op": "add", "el": {"type": "image", "asset": "$logo", "alt": "Logo"}}`;
  the host replaces it with the image's hash and fills `natural` (its pixel
  size) when you leave it out. The result's `assets` maps each ref to its
  hash, which later calls can use as `asset`. PNG, JPEG, GIF and still WebP
  only, each at most the upload limit; no SVG. One call carries images of
  about one upload limit in total, since they travel as base64. Add more over
  several calls (name earlier images by their hash), or put them in a board
  data file and import it with the CLI or HTTP API. A call or import whose
  images do not fit the disk quota is refused before any image is stored.
- `replace: true` replaces the whole board with the call's `add` operations.
  `layout` can place those new elements. Use it only when the user asked for
  a fresh board.
- On a new slug the board exists only if the call applies: a refused call
  leaves no flat behind.
- `op_id` (8 to 120 characters) is required. Resending the same request with
  the same `op_id` within 24 hours returns the first result and applies
  nothing again, so retry with it after a timeout. The same `op_id` with a
  different request is refused with `conflict`.

The result is `{revision, seq, ids, layout, results, assets?}`. `revision` is
null, with a `warning`, while the call's revision is still being recorded;
the call itself applied, so do not resend it with a new `op_id`.

### Import and shared selections

- `import_board {slug, op_id, archive, mode}` takes a Flats board data file
  (`.zip` as base64, at most the host upload limit). `mode: "add"` copies it
  into the board with new ids, keeps connections inside the file and places
  it to the right. `mode: "replace"` replaces the whole board. The CLI
  (`flats board import`) accepts files up to four times that limit; pass
  `--op-id` so that resending the same file after a timeout imports it once.
- People can select elements and press **Send to agent**. The screen then
  says "Tell the agent `#7`". `get_board_share {slug, n: 7}` returns that
  selection; without `n` it lists recent ones. Elements deleted since are
  marked `missing`. A share lasts 24 hours or until the board restarts. You
  never see cursors or live selections.

### Elements

Every element has `id`, `type`, `x`, `y`, `w`, `h` and `rot` (CSS px at
100% zoom; the host computes a connector's box from its ends), `z`, `frame`,
`group`, `locked`, and its `style` keys. A missing style key takes its
default; an unknown key is refused.

| Type | Fields | `style` keys |
|---|---|---|
| `sticky` | `text` | `color`: `yellow`, `orange`, `pink`, `blue`, `green`, `gray`; `fontSize`: `"auto"` (16, shrinking to fit) or 10 to 72 |
| `text` | `text` | `color` (ink); `fontSize` 12 to 144 (20); `align`: `left`, `center`, `right` |
| `shape` | `text` | `shape`: `rect`, `round`, `ellipse`, `diamond`; `fill`: `none`, `white` or a sticky color; `stroke` (ink); `strokeWidth`: 1, 2, 4; `dash`: `solid`, `dashed`, `dotted`; `fontSize` 10 to 72 (14) |
| `connector` | `ends`, `text` (label) | `stroke`, `strokeWidth`, `dash`; `route`: `straight`, `elbow`; `head0`, `head1`: `none` or `arrow`; `fontSize` 10 to 32 |
| `pen` | `pts` | `stroke` (ink or `highlight`); `width`: `pen`, `marker` |
| `image` | `asset`, `alt`, `natural` | none |
| `frame` | `text` (name) | `fill`: `white`, `none`, `gray` |

Ink colors are `ink`, `gray`, `red`, `orange`, `green`, `blue` and `purple`.
Colors are names; the canvas picks values that read in light and dark
themes. A connector end is `{"el": id, "at": [ax, ay]}` (attached, `at` from
0 to 1 inside the target's box) or `{"pt": [x, y]}` (free); `at` and `pt`
have exactly two numbers. Without `at`, the
host attaches the end to the middle of the target's side that faces the other
end, after `layout` has placed the elements, so the line starts on the
outline; an attached other end without `at` counts at its target's center.
The host stores that side as the end's `at`, so it stays when the elements
move later; attach the end again without `at` to recompute it (Write).
A frame is drawn behind its members only if its `z` is lower: the canvas
draws elements in order of `z` compared as plain strings (ties by `id`),
later ones on top.
Text is plain, at most 4,000 characters (200 for frame names and connector
labels, 1,000 for an image's `alt`).

Limits per board: 10,000 elements, 512 images (24 megapixels each), 2,000
points per pen stroke, 2,000 operations and 1,000 layout targets per call.
An edit that would exceed the per-flat disk quota is refused with
`board_capacity`.

### People editing at the same time

- People's edits merge by property: position and size, each connector end,
  text, and each style key change on their own. Your change to one property
  does not undo another person's change to a different one; on the same
  property the later write wins.
- If an element you target was deleted, the whole call is refused with
  `conflict`. Read again and rebuild the call.
- `if_seq` refuses the call if anything on the board changed since your read.
  `if_ver` on an operation refuses the whole call if that element changed:
  a version for one element or `{id: ver}`. `delete_frame_contents` takes
  the frame's version, and `ungroup` takes `{id: ver}` of its members. Both
  guards include people's edits that are not yet a revision. Use `if_ver`
  on elements you rewrite from what you read, such as text you edit.
- After a guard refusal, read again, apply your change to what people wrote
  and send it with a new `op_id`. Never resend without the guard, or use
  `replace`, to overwrite them.

### History

- One `update_board` or `import_board` call is one revision. A person's edits
  are grouped per person (closed after 30 s idle or 5 min), and open groups
  close into a revision before your write.
- Use the shared revision tools, without approval: `list_revisions`,
  `get_revision` (the revision's file list; read its elements with
  `get_board {slug, revision}`), `rollback_revision` (a new revision equal to
  that one, applied to open canvases at once) and `pin_revision`. When
  `rollback_revision` answers with a `warning` and no revision, the restore
  applied but its revision is not recorded yet: do not call it again.
- Old unpinned revisions are deleted beyond the host's quota (default 100,
  20 pins). People see the same history in the canvas's History panel.

### Verify

1. Read `results` and `layout`, then `get_board {slug, format: "outline"}`:
   every element is there, in the right frame, and every connector joins the
   intended elements.
2. The outline does not show stacking. For each frame you created or
   filled, read `get_board {slug, frame: <id>}` (the full format lists the
   frame and its members with their `z`) and check that every member's `z`
   is greater than the frame's. If not, send the frame back with
   `{"op": "zorder", "ids": [<frame id>], "to": "back"}`.
3. Give the user `private_url`. If you have a browser tool, open the canvas
   and look at the changed area.

### Readability

- No overlaps: use `layout`, or leave at least 24 px between elements.
- Group related elements in frames with short, clear names.
- Use color for meaning, for example one color per topic or status, and say
  what each color means in a text element. Also state that meaning in frame
  names or text, so the board reads without color.
- Keep text readable at 100% zoom: at least 12 px, and short stickies.
  Sticky `fontSize: "auto"` starts at 16 px and shrinks to fit, down to
  10 px, inside the sticky minus 12 px on each side (more at the bottom
  when people show author names). A line takes 1.38 × the font size. A
  default 200×200 sticky holds 7 lines at 16 px; a small one, such as
  120×90, drops below 12 px with a short sentence. Keep stickies with text
  at the default size or larger, or set `fontSize` to 12 or more and make
  the sticky big enough: a fixed size never shrinks, and text that does not
  fit is cut off with "…". Shape text also shrinks from its `fontSize` down
  to 10 px, so size shapes for their text. Never set a `fontSize` below 12.
- Attach both ends of a connector to elements; point arrows in the reading
  direction, left to right or top to bottom.
