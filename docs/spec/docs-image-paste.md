# Pasting images into documents

Status: implemented.

## Summary

A person editing a document on a Private route can paste an image from the
clipboard. The image becomes a file of the document's bundle and a Markdown
image appears at the caret. Everything that already handles a document's assets
keeps working: serving, History, rollback, Public reading and the agent tools.

## Decisions

The owner chose to store pasted images in the bundle (2026-11-06), rather than
in a separate image store as boards do. That choice brings three costs, which
stay until a later change removes them:

- All of a flat's files together stay within `upload_max_bytes` (20 MiB by
  default), because this is the existing limit on a bundle.
- Each added image is a changed save, which replaces the running worker. It
  takes under a second on an idle host, and every open editor and reader of the
  flat reconnects.
- Each added image is a revision of the flat, and counts toward the revision
  quota like any other.

## Behavior

### What counts as pasting an image

1. A paste in the editor adds images when the clipboard holds at least one file
   whose type starts with `image/`, and no text. Other files are ignored.
2. Text wins over an image: when the clipboard also holds non-blank plain text,
   the paste inserts the text as today. Spreadsheets and word processors put a
   rendered picture on the clipboard next to the text a person meant to copy.
3. There is one exception to rule 2: when the plain text is exactly the name of
   the single image file, as when an image file is copied in a file manager,
   the image wins.
4. Images inside copied HTML are not fetched or extracted. The editor loads
   nothing from another site (see [docs-public-edit.md](docs-public-edit.md)
   rule 21a).

### Who can do it

5. Only members on a Private route can add images. The host refuses every other
   request whatever the page sends: a Public route, including anyone-can-edit
   guests, gets 404, as the revision routes do.
6. A guest who pastes an image sees a notice saying that guests cannot add
   images, and the text does not change.
7. A read-only editor ignores the paste, as it ignores a text paste. View mode
   and an open revision have no editor to paste into.

### The text

8. While an image is on its way, the status line says "Adding image…" in place
   of the usual sync state, and the person can keep typing. The text does not
   change, and no placeholder is written into the document.
9. When the host has accepted the image, the page inserts `![](<reference>)`.
   - The image goes where the caret or selection was when the paste happened,
     following concurrent edits by anyone.
   - A selection that is unchanged is replaced, as an ordinary paste replaces
     it. If its text changed in the meantime, the image goes right after it and
     the selection's text stays.
   - The insert is one local edit, apart from typing around it: Undo removes
     the image alone, and it syncs like typing.
10. The caret moves after the image only if it is still at the place the image
    goes; text typed at the caret moves that place along. If the person put the
    caret somewhere else, it stays there.
11. If the image would be alone on its line, a line break follows it, so that
    the caret is on the next line and the picture shows at once. Otherwise it is
    inserted inline and shows when the caret leaves the line, as all Markdown in
    the editor does.
12. The alt text is empty; a person can type one into the Markdown.
13. Several images in one paste, and pastes made while another is in progress,
    are added one at a time, in order, and inserted in that order.
14. Text typed or synced before, during and after an image is added is kept,
    including while the page reconnects.

### Failure

15. A refused or failed image leaves the text unchanged, including any
    selection. A notice above the document gives the reason in plain words and
    can be dismissed. Editing continues.
16. If the page has stopped syncing by the time the host accepts the image, the
    reference is not inserted and the notice says so. The file stays in the
    bundle.
17. Leaving the page during an add ends the page's part: no reference is
    inserted, and images still waiting are not sent. The host may still finish
    adding the file already on its way.

### The file

18. The image is stored at `images/<name>.<ext>` in the same directory as the
    document, and the reference is the relative path `images/<name>.<ext>`.
    `<name>` is the first 16 hexadecimal digits of the SHA-256 of the bytes.
    `<ext>` is `png`, `jpg`, `gif` or `webp`, from the format the host detected,
    not from what the browser claimed.
19. The bytes are stored unchanged: no resizing, recompression or metadata
    removal. A person pasting a photo should expect that its embedded metadata
    is served with it.
20. Pasting bytes that the flat already holds at that path adds nothing: no
    save, no revision, no reconnect. The page inserts the reference as usual.
21. If a file with different bytes already holds that path, or a file holds a
    path that `images/` needs as a directory, the host refuses and changes
    nothing.
22. Removing the reference from the text leaves the file in the bundle, and on a
    Public document it stays reachable at its address. Only a whole-bundle save
    that omits it, or a rollback to a revision without it, removes it. An agent
    replacing the whole bundle must include these files or they are deleted, as
    with any asset ([content-models.md](content-models.md), rule 8).

### What the host accepts

23. The formats and pixel limits are those of board images
    ([board.md](board.md), rule 69): PNG, JPEG, GIF and still WebP, at most
    16,384 pixels per side and 24 megapixels, and an image that decodes. SVG and
    every other format are refused, although an SVG that a bundle already holds
    is still served.
24. One image is at most `upload_max_bytes`.
25. The bundle after the addition is at most `upload_max_bytes`, and the flat
    stays within its disk quota. These refusals name the limit and that the
    operator can raise it.
26. Every image is served by the existing asset rules: a fixed MIME type, no
    sniffing, and the asset Content Security Policy.
27. The write protection of the revision routes applies: `POST` only, the
    page's own request header, and a same-origin request. A request without them
    is refused and changes nothing.
28. The add is a save of the bundle: other files and the live text of every
    document are kept as with `save_document`
    ([content-models.md](content-models.md), rule 8). It runs at once, without
    approval. On a Public document the file is public as soon as it is added.
    A save that would fail its health check or start refuses the image and
    leaves the running document as it was. A failure after the new version has
    taken over does not refuse it: the image is added and served, and a later
    request retries preparing the document, as with any save.
29. The revision is labeled with the person's name, as a revision rollback is,
    and its message names the file. People see it in History like other file
    changes.

## Not in scope

- Dragging images into the editor, or a file picker.
- Other file types, or uploads from guests.
- Removing files that no text references any more.
- Reducing the reconnect or lifting the bundle limit. Either belongs to a
  separate decision about storage.

## Completion criteria

| Rules | Evidence |
| --- | --- |
| 1–4 | Unit tests of the clipboard classification, with the cases named in the rules |
| 5, 27 | Route tests: Public route, wrong method, missing header and cross-site requests change nothing |
| 6, 7, 15, 16 | Browser checks of the guest notice and a refused image; unit tests where the logic is pure |
| 8–14 | A real browser pasting real clipboard events on a development host: position, selection, caret, order, Undo, and a second editor typing during the add |
| 18–22 | Core tests: path beside nested documents, detected extension, unchanged bytes, repeated paste adds no version or revision, conflicts refused |
| 23–26 | Core tests of each refusal; route test of the served headers |
| 28, 29 | Core tests: sibling files and live edits kept, revision label and message; a failed health check keeps the running document |
