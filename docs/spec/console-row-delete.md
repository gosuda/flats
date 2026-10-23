# Delete from the flat list

Status: implemented.

## Summary

The console's flat list gets a Delete item in each flat's More actions menu
("…"). Choosing it does not open a dialog: the Delete item itself turns into
a slide control, in the same place and the same size, while the menu stays
open. The operator drags a handle along the track to its end, as in "slide
to unlock", and the flat is deleted only when the handle reaches the end.

Deletion itself does not change. The console already deletes a flat directly
(`DELETE /flats/{slug}`, see [surfaces](../surfaces.md)); this spec adds a
second place to start it and a different confirmation for that place.

## Before this change

A flat could be deleted only from its Settings page, Danger zone, with
"Delete flat…", whose dialog asks the operator to type the slug. That
deletion stays (rule 25).

## Behavior

### The menu item

1. Every flat in the list, of every content type, has a Delete item in its
   "…" menu, in both the list and grid views and at every screen width.
2. Delete is the last item, after Settings, set apart from the items above
   it by a divider. Its label is the word "Delete" in the console's danger
   color, in light and dark themes.
3. Delete is reachable like the other items: by pointer, by touch, and by
   the menu's keyboard navigation (arrow keys, Home, End, Enter).
4. Each time the menu opens, its last item is the plain Delete item. A slide
   control never carries over from an earlier opening, and its handle always
   starts at the start.

### Turning into the slide control

5. Choosing Delete keeps the menu open and replaces the Delete item with the
   slide control in exactly the Delete item's box: the same width, height
   and place. The menu keeps its size and position, and every other item
   and the divider keep theirs. Nothing is added above, below or beside the
   control. The plain Delete item itself has the size of every other item.
   At the console's default type size this box is 196 × 38.5 CSS pixels in
   a menu at its 210 px minimum width; it is wider when a longer item (a
   network address on narrow screens) widens the menu.
6. The control is a track with a square handle at its start. The track's
   corners have the menu's 12 px corner radius. The handle is square, inset
   4 px from the track's edge (a 3 px gap inside the 1 px border), with an
   8 px corner radius, so its curve follows the track's; at the default size
   it is 30.5 px square. The track shows one short state label at a time
   ("Slide to delete", and the labels of rules 15, 20 and 22) and no other
   text.
7. Focus moves to the handle, which is announced as a slider named "Slide
   to delete NAME" (NAME is the flat's name, or its slug when it has no
   name), with its position as a percentage.
8. Closing the menu without reaching the end leaves the flat untouched. The
   menu closes as it does today: Escape (focus returns to the "…" button),
   a press outside the menu, choosing "…" again, Tab out of the menu,
   opening another row's menu, leaving the list page, or the list being
   redrawn (for example when the window becomes visible again). Other items
   above the control keep working until the deletion starts.

### The slide control

9. Dragging the handle with a mouse, finger or pen moves it along the track
   and fills the track behind it in the danger color. The label fades as
   the handle moves over it. The handle never leaves the track.
10. Only the handle starts a drag. Pressing or tapping elsewhere on the
    track does not move the handle, so one click can never delete.
11. Releasing the handle before the end returns it to the start with a short
    animation. So does any end of the drag other than reaching the end: the
    pointer being cancelled (a lost touch), focus leaving the handle, or the
    window losing focus. Later movement from a cancelled drag does nothing.
12. The handle reaches the end when its edge touches the end of the track.
    At that moment the deletion starts; the operator does not need to
    release first.
13. Keyboard: each press of Right moves the handle forward by a tenth of the
    track and each press of Left back by a tenth; Home returns it to the
    start. Holding a key moves it one step only: repeated key events are
    ignored, so reaching the end takes ten separate presses. End and Page Up
    do not jump to the end. Right, Left, Home, End, Page Up and Page Down on
    the handle belong to the slider: they never move the menu's focus. Up
    and Down keep moving focus between the menu's items, as they do today,
    with the handle in the place of Delete, and moving focus away from the
    handle returns it to the start.
14. With reduced motion requested, the handle returns without animation,
    the fill follows without transitions and the label does not shimmer.

### While deleting

15. The track fills with the danger color, reads "Deleting…" and the handle
    shows progress; "Deleting NAME…" is announced. The other menu items are
    disabled, and Escape, a press outside, choosing "…" again, Tab and the
    arrow keys do not close the menu or move focus: focus stays on the
    handle. Another row's menu does not open while a deletion is pending.
16. From the start of a deletion until its menu closes, the list is not
    redrawn (for example when the window becomes visible again), so the
    control keeps its state: pending, "Failed. Slide to retry" (rule 20) or
    "Not confirmed" (rule 22), with its focus and enabled state. A redraw
    asked for meanwhile happens when that menu closes; when the flat is
    gone, the menu closes with the outcome and the list updates then.
    Leaving the list page does not cancel the request: its outcome still
    appears as a toast, and focus is not moved on a page that is gone.
17. The pending state has a bound. If the delete request has no answer
    after 30 seconds, or the list read of rule 20 has none after 10 seconds,
    the console stops waiting and shows the unknown result of rule 22. It
    does not claim the deletion failed or stopped. An answer that arrives
    later is ignored; the list refresh shows the actual state.

### Result

The console reports what it knows about the flat afterwards, not what it
hoped. The host can answer with an error after the flat's record is already
gone (for example, when cleaning up its stored files fails). Without an
answer from the host, the deletion may still be running, so the result is
unknown. The track shows only the short state label; details, including the
host's message, appear in the console's toasts, so the menu keeps its size
in every state.

The console classifies the answer to the delete request by what it can
observe:

- confirmed: a 2xx answer whose JSON body is an object with `status` equal
  to `"done"` (the console's direct deletion), or to `"approved"` if the
  request went through an approval the console decided;
- not found: a 404 answer whose JSON body has a string `error` and
  `category` equal to `"not_found"`;
- host error: any other answer that is not 2xx and whose JSON body is an
  object with a string `error`;
- unknown: everything else, including no answer, a timeout, a body that is
  not JSON, a 2xx answer that is not a confirmed result, and an error
  without the host's error shape.

A proxy or other server in between that answers with exactly the host's
shapes cannot be told apart from the host; the console treats such an
answer as the host's.

18. Confirmed: the menu closes, a toast says "NAME was deleted.", and the
    flat disappears from the list. The list keeps its view, search text and
    scroll position.
19. Not found: the menu closes, a toast says "NAME was already deleted.",
    and the list refreshes.
20. Host error: the request has finished. The track stays in its
    "Deleting…" state while the console reads the flat list again:
    - If the flat is still listed, the handle returns to the start and
      keeps focus, the track reads "Failed. Slide to retry" in the danger
      color, the other items work again, and an error toast shows the
      host's message. The operator can slide again or close the menu.
    - If the flat is gone, the menu closes, the flat leaves the list, and an
      error toast says "NAME was deleted, but the host reported a problem:"
      followed by the host's message. The console does not offer a retry.
    - If the list cannot be read (any failure, or no answer within the
      bound of rule 17), the result is unknown as in rule 22.
21. The list read of rule 20 is readable only when it is a JSON object
    whose `flats` is `null` (an empty list) or an array in which every entry
    is an object with a non-empty string `slug`. Anything else, including an
    array with an entry that has no usable slug, counts as unreadable.
22. Unknown: the track reads "Not confirmed" and the control stays
    disabled, the other items work again, and an error toast says the
    console could not confirm whether NAME was deleted and that the
    deletion may still finish, with the error. The menu closes normally
    (Escape, a press outside, Tab); closing it refreshes the list, which
    then shows the flat as the host has it at that moment. The console does
    not read the list to decide this case, because a deletion still running
    can leave the flat listed for a while.
23. After the menu closes because the flat is gone (rules 18–20), focus
    moves to the "…" button of the row that took its place, or of the
    previous row when it was the last one, or to the page heading when the
    list is empty or no rows match the search. After the menu closes in
    rule 22, focus goes to the flat's "…" button if the refreshed list still
    has it, and otherwise follows this rule.
24. Deleting from the list uses the same request and the same direct
    console deletion as the Settings page. It does not create an approval
    request for the operator to decide separately.

### Unchanged

25. The Settings page's Danger zone keeps its "Delete flat…" button and its
    typed-slug confirmation.
26. Agents, the CLI and MCP are unchanged: their delete still waits for the
    operator's approval.

## Completion criteria

- Each rule above is covered by a console UI test or, for pointer, touch,
  layout and visual rules, by a check in a real browser against a
  disposable development host with synthetic flats.
- Browser checks cover:
  - the menu with Delete in list and grid views, desktop and narrow widths,
    light and dark themes;
  - the menu's box and every other item's box are identical before and
    after choosing Delete; at the default type size and minimum menu width
    the Delete item and the control are 196 × 38.5 px, the track radius is
    12 px and the handle is a 30.5 px square with an 8 px radius;
  - a mouse drag released early (returns) and to the end (deletes); a
    touch drag to the end; pressing the track beside the handle;
  - keyboard only: reaching Delete with the arrow keys and Enter, ten
    presses to delete, a held key moving one step, Home resetting and End
    doing nothing without moving the menu's focus, Up and Down still moving
    between items;
  - Escape, a press outside, Tab and opening another row's menu before the
    end (no deletion), and the same during deletion (the menu stays open,
    the other menu does not open, one DELETE request is sent);
  - focus after deleting the first, the last and the only flat, and the
    list keeping grid view, search and scroll position;
  - a flat deleted elsewhere while its control was shown.
- Console UI tests cover each classification and outcome with a stubbed
  API: confirmed; not found (404 with `not_found`) and a 404 without that
  category (host error); a host error with the flat still listed, gone, and
  with the list unreadable, malformed or not answering within its bound;
  and unknown answers (connection failure, a proxy error without the host's
  error body, a 2xx answer that is not a confirmed result, and no answer
  within 30 seconds, including an answer that arrives afterwards and is
  ignored). The unknown case shows the unknown result even when the flat
  is still listed.
- Console UI tests cover the pending lifecycle: one DELETE per completed
  slide; a redraw asked for while pending that is held back through a
  retry and through an unknown result (label, enabled state, menu and
  focus kept) and happens when the menu closes; and an outcome toast after
  the list page was left.
- Console UI tests cover unreadable list reads after a host error,
  including `{"flats":[{}]}` and `{"flats":[null]}`, which give the unknown
  result.
- The Settings page's deletion still requires the typed slug.

## Open questions

None.
