# Sheet content type

Status: partly implemented. The sheet type runs on the host and agents can
use it, but no success criterion has been measured yet; see
[Current state](#current-state).

This page sets what the first sheet release (S1) must do, why, and the rules
a sheet keeps. The
[technical design](sheet-technical-design.md) decides the storage format, the
protocol and where formulas are calculated.

## Summary

A **sheet** (`type: "sheet"`) is a spreadsheet that people and agents edit
live. It holds one or more **tabs**. Each tab is a grid of cells in lettered
columns and numbered rows. A cell holds a number, text, a boolean, a date or
time, or a formula that calculates its value from other cells. People use a
sheet for budgets, trackers, comparisons and small calculations. Agents read
it through MCP as cells and write cells, formulas, formats, rows, columns and
tabs the same way people do.

A sheet follows the **Revision model** of
[content-models.md](content-models.md) unchanged. It has no Draft, versions,
previews or publish. Edits apply at once without approval, and history is
kept as revisions.

| Type | Written by | What people change in the browser | Changes reach the URL |
|---|---|---|---|
| `docs` | people and agents | Markdown text | at once |
| `design` | people and agents | artboard layout, text written in the source, tweaks and notes | at once |
| `board` | people and agents | every element of a whiteboard | at once |
| `notebook` | people and agents | every page, block, database, row and view | at once |
| **`sheet`** | **people and agents** | **every cell, format, row, column and tab** | **at once** |
| `page` | an agent | nothing | after an approved version and deployment |
| `flat` | an agent | nothing | after an approved version and deployment |

## Why

- **No type calculates.** A table in a `docs` flat is text: its total is
  typed, and it is wrong after the next edit. A notebook database holds typed
  records but has no formulas. A person who wants "sum this column" or "what
  if the price rises 10%" has to leave Flats.
- **Agents produce tables all the time.** A cost comparison, a test matrix or
  a data extract is today a Markdown table in a document or a one-off page.
  People cannot sort it, correct a number and see the totals follow, or add a
  column of their own.
- **People and agents should work on the same numbers.** A person fills in
  the inputs and an agent writes the formulas, or the other way round, on the
  same live grid and without one overwriting the other.
- **Hosted spreadsheets need accounts and outside servers.** Flats already
  has a single binary, Private networking, the Revision model and MCP.

## Users and scenarios

| User | Scenario |
|---|---|
| An operator | Keeps a household or project budget with totals per category and month. |
| A small team (tailnet members) | Fills in one tracker at the same time, each in their own rows, and sorts it by due date. |
| An agent | Writes a comparison table with formulas for totals and shares, then updates the inputs later without touching the columns people added. |
| An outside viewer | Reads, read-only and live, a sheet the operator approved as Public. |

A typical flow:

1. An agent creates a sheet with `create_flat {type: "sheet"}` and gives the
   user the address the call returns: its Private URL, or its Public URL on a
   host in public instance mode (rule 53).
2. The user asks for "a budget for the offsite with a total and a share per
   item". In one write the agent names the tab, writes the header row, the
   items, a `=SUM(...)` total and a share formula per row, and formats the
   amounts as currency. That write is one revision.
3. The user corrects two amounts in the browser. The total and the shares
   follow at once, on every open screen.
4. A teammate inserts a row for a new item while the user is typing in
   another row. Both edits stay, and the user's text lands in the cell they
   were editing.
5. Someone sorts the wrong rows. They undo it, or a person with write
   access opens History and rolls back.

## Principles

1. **One grid for people and agents.** Every cell, format, row, column and
   tab is structured data. People change all of it in the browser. Agents
   read and write the same data through MCP, never by scraping rendered HTML
   or editing a serialized file.
2. **One answer per cell.** A state of the sheet is its content together
   with its calculation date (rule 13). For one state a formula has one
   calculated value. A person in the browser, an agent through MCP, a Public
   viewer and a CSV export all get that value.
3. **Concurrent editing is the default.** Several people and agents edit the
   same sheet at the same time. An edit that passes permission, validation,
   limit, disk quota and guard checks is never refused because someone else
   changed another cell at the same moment. When two writers change the same
   value, the [concurrency rules](#concurrency-rules) decide; the server's
   order is final.
4. **An edit stays with its cell.** When rows or columns are inserted,
   deleted or sorted while someone edits a cell, the edit applies to the cell
   they were editing, wherever it now lies, and not to whatever cell took its
   address.
5. **Cell content is data.** A sheet never runs script or HTML from a cell.
   A formula calculates only from the sheet's own cells with the functions
   listed on this page. It reads no file and makes no request.
6. **Flats' existing rules apply unchanged.** A sheet does not redefine the
   Revision model, Private and Public access, the actions that need approval
   (visibility changes and deletion), the disk quota or fixed types.

## Content

1. **Explicit type.** A flat becomes a sheet only through
   `create_flat {type: "sheet"}` or a sheet write to a new slug. The type
   never changes, and saves of another type are refused.
2. **Tabs.** A sheet holds 1 to 50 tabs in a fixed order.
   - Each tab has an id unique within the sheet, a name, a position, a grid
     and its settings (rule 8).
   - A name has 1 to 100 characters, is unique in the sheet apart from
     capitals, and contains none of `[ ] : * ? / \` or a control character.
     It does not start or end with a space or an apostrophe.
   - A new sheet has one empty tab named `Sheet1`.
   - A sheet always keeps at least one tab. Deleting the last one is refused.
   - Deleting a tab deletes its cells. The browser asks for confirmation
     inline when the tab holds anything. A deleted tab is recovered through
     undo or from history ([Revisions](#revisions)).
3. **Grid.** A new tab has 1,000 rows and 26 columns.
   - Columns are named `A` to `Z`, then `AA` to `ZZ`: at most 702 columns.
     Rows are numbered from 1, at most 100,000.
   - A write past the last row or column grows the tab to hold it, up to
     those limits. People add rows and columns at the end from the grid.
   - A tab keeps at least one row and one column. A delete that would take
     every row or every column of a tab is refused.
   - Every row and every column has an identity of its own. An insert or a
     delete elsewhere leaves it unchanged, and a sort moves a row with its
     identity (rule 22). A cell is the meeting of one row and one column.
     Addresses such as `B7` name where a cell lies now.
4. **Cell content.** A cell is blank or holds exactly one of:

   | Content | Examples | Notes |
   |---|---|---|
   | Number | `1200`, `-0.5`, `1e6` | A double-precision number. |
   | Text | `Rent` | Up to 32,767 characters. Line breaks are kept. |
   | Boolean | `TRUE`, `FALSE` | |
   | Date, time, or date with time | `2026-03-15`, `14:30`, `2026-03-15 14:30` | Stored without a time zone. To a formula a date is a count of days, with 1900-01-01 as day 2 so that day numbers match other spreadsheets for every date from 1900-03-01, and a time is a fraction of a day. |
   | Formula | `=SUM(B2:B9)` | Up to 8,192 characters. Its value is calculated ([Formulas](#formulas)). |

5. **Typed entry.** What a person types or pastes is read once, when it is
   committed.
   - Text that starts with `=` and has more characters is a formula. A lone
     `=` is text.
   - `TRUE` and `FALSE`, capitals aside, are booleans.
   - A number is read with a period before decimals and optional commas
     between thousands: `1,200.50`. It may end in an exponent: `1.5E+20`. A trailing `%` divides by 100 and sets
     the percent format on a cell without a number format. A leading `$`,
     `€`, `£`, `¥` or `₩` sets the currency format with that symbol in the
     same way.
   - A date is read as `2026-03-15`, `3/15/2026` (month first) or
     `Mar 15, 2026`. A time is read as `14:30`, `14:30:05` or `2:30 PM`. A
     date and a time together are a date with time. A recognized date or
     time sets a matching format on a cell without a number format.
   - A leading apostrophe keeps the rest as text and is not shown: `'007`,
     `'=not a formula`.
   - Anything else is text.
   - S1 reads numbers and dates in this one way for every person. Reading
     them by locale is [not in S1](#not-in-s1).
6. **Cell format.** A cell, blank or not, can carry:
   - bold, italic, underline and strikethrough, each on or off;
   - a text color and a fill color, each either none or one of the palette
     colors: the hues gray, red, orange, yellow, green, teal, blue, purple
     and magenta in the shades 100, 200, 300, 400, 600 and 800. The browser
     draws each palette color for the light and the dark theme, so the same
     cell reads in both;
   - horizontal alignment: general (text left, numbers and dates right,
     booleans and errors centered), left, center or right;
   - wrap, on or off. Wrapped text breaks at the column's width and the row
     grows to show it;
   - a number format, one of the following.

     | Number format | Settings | 1234.5 shows as |
     |---|---|---|
     | General | none | `1234.5` |
     | Number | decimals 0 to 10, thousands separator on or off | `1,234.50` |
     | Percent | decimals 0 to 10 | `123450.00%` |
     | Currency | symbol of 1 to 3 characters, each a letter or a currency sign, decimals 0 to 10 | `$1,234.50` |
     | Date | `yyyy-mm-dd`, `m/d/yyyy` or `mmm d, yyyy` | `1903-05-18` |
     | Time | `hh:mm`, `hh:mm:ss` or `h:mm AM/PM` | `12:00` |
     | Date and time | a date form and a time form | `1903-05-18 12:00` |
     | Text | none | `1234.5`, left-aligned |

   - General shows up to 15 significant digits and switches to scientific
     notation for numbers that would need more than 15 digits before the
     decimal point or more than 9 zeros after it.
   - A format changes what a cell shows, never what it holds. A number
     format on text has no effect. A number that does not fit its column
     shows as `####`, never as a shorter number.
7. **Column width.** Each column has a width of 40 to 1,000 pixels, 104 by
   default. Row height follows the content: one line, or more when a cell of
   the row wraps or holds line breaks.
8. **Tab settings.** Each tab has a count of frozen rows (0 to 20) and of
   frozen columns (0 to 10). Frozen rows and columns stay in view while the
   rest scrolls, for everyone.
9. **Limits.** The host enforces these limits on every write, by a person or
   an agent.

   | Limit | Value |
   |---|---|
   | Tabs in a sheet | 50 |
   | Rows in a tab | 100,000 |
   | Columns in a tab | 702 |
   | Cells that hold content or a format, in a sheet | 500,000 |
   | Formulas in a sheet | 100,000 |
   | Characters in a text cell | 32,767 |
   | Characters in a formula | 8,192 |
   | Nesting of brackets and functions in a formula | 64 levels |
   | Cells one formula reads, ranges included | 1,000,000 |
   | Cells all formulas of a sheet read, ranges included | 20,000,000 |
   | Characters of typed text in all cells of a sheet | 50,000,000 |
   | Characters of text that all formulas of a sheet calculate, in one state | 50,000,000 |
   | Characters of one text value inside a calculation | 32,767 |
   | Bytes of one agent write call | 8 MiB |
   | Bytes of one agent read or write result | 4 MiB |

   - A write that would exceed a limit is refused with the limit's name, and
     nothing of that write applies. This holds for a formula that is too
     long or nested too deep, which is refused and never stored as an error,
     and for an insert of rows that would take a whole-column reference
     over a read limit.
   - **Calculated text.** No text value inside a calculation, intermediate
     or final, is longer than 32,767 characters: a function or operator
     that would build a longer one stops and gives `#VALUE!`. Ranges are
     read in place and never copied whole. One formula therefore needs
     memory bounded by its nesting limit and this length.
   - The limit on the text that all formulas calculate is a property of a
     state, not of an edit, because a date change or a rollback can pass it
     without any write. When the text results of a state would together
     pass it, the state is calculated in limited mode instead: a formula
     whose text result would be longer than 255 characters has `#LIMIT!` as
     its value, and a formula that reads such a cell reads that error, by
     the usual rules for errors. With at most 100,000 formulas the results
     of limited mode fit. Whether a state is in limited mode, and every
     value in it, follows from the state alone, so every reader sees the
     same. No edit is refused for this limit, and the next state whose
     ordinary results fit shows every result again.
   - Every other limit in the table is checked on what is stored, so every
     stored state, a revision included, meets it. The two read limits bound
     the work of any recalculation, and the two rules above bound its
     memory, so that a small edit cannot start an unbounded one.
   - The host also has a time limit for one edit: for the work of the edit
     itself, such as a sort, a fill or an insert of rows, and for the
     recalculation it starts. If they do not finish in time, the edit is
     refused like any other refused edit, and the sheet stays as it was.
     The same holds for an edit that changes more at once than the host
     can take back if it has to refuse it; the technical design gives that
     bound.
   - A recalculation that no edit starts has no such edit to refuse: after
     a host restart, a rollback, a change of the calculation date, or for
     viewing a revision. The host runs it to its end, which the limits
     above bound, without delaying other flats. Until it ends, readers of
     that sheet or revision get no partly calculated values: the browser
     shows that the sheet is calculating, and an agent read is refused as
     temporarily unavailable and can be retried. A change of the
     calculation date takes effect when its recalculation has ended; until
     then the sheet stays whole in its previous state.
   - A CSV download is produced row by row and is bounded by the limits of
     the sheet. Results of agent calls are bounded by the result limit
     (rules 42 and 43).
   - A character is one Unicode code point, in every limit and in the text
     functions.
   - The numbers are planned values ([Open question 1](#open-questions)).

## Formulas

10. **Syntax.** A formula starts with `=` and holds:
    - numbers, `"text"` with `""` for a quotation mark inside it, `TRUE` and
      `FALSE`;
    - references: a cell `B3`, a range `B3:D9`, whole columns `B:D`, whole
      rows `2:5`, and each of these in another tab as `Costs!B3` or
      `'Q1 costs'!B3:D9`. A `$` before a column or a row, as in `$B$3`,
      keeps that part fixed when the formula is copied, filled or moved by a
      sort;
    - the operators `+ - * / ^ &`, the comparisons `= <> < <= > >=`, a
      trailing `%`, a leading `-` or `+`, and brackets;
    - calls of the functions in rule 12, with capitals in a name ignored.
    Names for ranges and arrays in braces are [not in S1](#not-in-s1).
11. **Values and errors.** A formula's value is one number, text, boolean or
    error, or blank when the formula returns a blank cell it read, as `=A1`
    does for a blank A1; such a cell shows nothing. A date function gives a number, shown as a date when the cell has
    a date format; a formula cell that has no number format and whose
    outermost function is one of `DATE`, `TODAY`, `EDATE` or `EOMONTH` shows
    as a date.

    | Error | Means |
    |---|---|
    | `#ERROR!` | The formula cannot be read: a bracket is missing, or an operator has nothing to work on. |
    | `#NAME?` | It calls a function that does not exist. |
    | `#REF!` | It reads a cell, row, column or tab that was deleted, or one outside the grid limits. |
    | `#VALUE!` | A value has the wrong kind: text where a number is needed, or a range where one value is needed. |
    | `#DIV/0!` | It divides by zero. |
    | `#NUM!` | A number is out of range for the function, or the result is not a finite number. |
    | `#N/A` | A lookup found nothing, or `NA()` was called. |
    | `#CYCLE!` | The formula reads its own cell, directly or through other formulas. Every cell on the cycle shows it. |
    | `#LIMIT!` | The formulas of the sheet calculate more text than a state may hold, and this formula's text result is longer than limited mode allows (rule 9). |

    - A formula that reads a cell holding an error has that error as its
      value, unless a function such as `IFERROR` handles it.
    - A formula with an error is still stored as typed, so correcting what
      it reads corrects its value.
    - Text is converted to a number, and a number to text, where an operator
      needs it: `="3"+1` is 4 and `=1&"a"` is `1a`. Text that is no number
      gives `#VALUE!`. A blank cell counts as 0 in arithmetic and as empty
      text in `&`. Comparisons of text ignore capitals.
    - A reference to a range where one value is needed is `#VALUE!`. There
      are no array results in S1: a formula never fills other cells.
12. **Functions.** S1 has exactly these functions.

    | Group | Functions |
    |---|---|
    | Math | `ABS`, `CEILING`, `EXP`, `FLOOR`, `INT`, `LN`, `LOG`, `LOG10`, `MOD`, `PI`, `POWER`, `PRODUCT`, `QUOTIENT`, `ROUND`, `ROUNDDOWN`, `ROUNDUP`, `SIGN`, `SQRT`, `SUM`, `SUMPRODUCT`, `TRUNC` |
    | Statistics | `AVERAGE`, `COUNT`, `COUNTA`, `COUNTBLANK`, `LARGE`, `MAX`, `MEDIAN`, `MIN`, `RANK`, `SMALL`, `STDEV`, `STDEVP`, `VAR`, `VARP` |
    | Conditional totals | `AVERAGEIF`, `AVERAGEIFS`, `COUNTIF`, `COUNTIFS`, `MAXIFS`, `MINIFS`, `SUMIF`, `SUMIFS` |
    | Logic | `AND`, `FALSE`, `IF`, `IFERROR`, `IFNA`, `IFS`, `ISBLANK`, `ISERROR`, `ISLOGICAL`, `ISNA`, `ISNUMBER`, `ISTEXT`, `NA`, `NOT`, `OR`, `SWITCH`, `TRUE`, `XOR` |
    | Text | `CONCAT`, `CONCATENATE`, `EXACT`, `FIND`, `LEFT`, `LEN`, `LOWER`, `MID`, `PROPER`, `REPLACE`, `REPT`, `RIGHT`, `SEARCH`, `SUBSTITUTE`, `TEXT`, `TEXTJOIN`, `TRIM`, `UPPER`, `VALUE` |
    | Lookup | `CHOOSE`, `COLUMN`, `COLUMNS`, `HLOOKUP`, `INDEX`, `MATCH`, `ROW`, `ROWS`, `VLOOKUP`, `XLOOKUP` |
    | Date and time | `DATE`, `DATEDIF`, `DAY`, `DAYS`, `EDATE`, `EOMONTH`, `HOUR`, `MINUTE`, `MONTH`, `SECOND`, `TIME`, `TODAY`, `WEEKDAY`, `YEAR` |

    - Each function takes the arguments and gives the results that the same
      name has in the common spreadsheet programs, for the cases S1
      supports. The technical design fixes a reference table of inputs and
      expected results for every function; that table is the contract.
    - Functions that total or count a range pass over text and blank cells
      as those programs do, and criteria such as `">100"`, `"<>done"` and
      `"a*"` are read as they read them.
    - `SUMPRODUCT` multiplies ranges of the same size cell by cell. No other
      operator or function works through a range cell by cell.
    - `XLOOKUP` supports exact match and its if-not-found argument.
      `VLOOKUP`, `HLOOKUP` and `MATCH` support exact match and the sorted
      approximate match.
    - `TEXT` takes the patterns that the number formats of rule 6 can
      express.
    - `TODAY` gives the calculation date of the state it is calculated in
      (rule 13).
    - A function outside this list is `#NAME?`. Random numbers, the current
      time and every function that returns a range are
      [not in S1](#not-in-s1).
13. **Recalculation.** After any edit, every formula whose value can change
    is recalculated before the edit is reported as saved, and each reader
    gets the new values together with the edit. No reader sees a state in
    which an edit has applied and a formula that reads it still shows the
    old value.
    - **Calculation date.** The live sheet has a calculation date: the date
      of the host's clock in the host's time zone. When that date changes,
      the sheet moves to a new state with the new date, and formulas that
      read `TODAY` are recalculated as after an edit. While anyone has the
      sheet open this starts within a minute and takes effect when the
      recalculation has ended (rule 9); otherwise it happens before the next
      read or edit is answered. It changes no content and creates no
      revision.
    - A read in several pages (rule 42) stays in the state it started in,
      with that state's date, even past midnight.
    - A revision records the calculation date it was closed with. Viewing
      it, or reading it through `get_revision`, calculates with that date,
      so a retained revision always shows the same values. A rollback
      restores content and calculates it with the live date.
14. **References follow structure.**
    - Inserting rows or columns moves the cells after them, and every
      formula that names a moved cell names its new address. A range grows
      when rows or columns are inserted between its first and its last, and
      not when they are inserted before its first or after its last.
    - Deleting rows or columns shrinks a range that loses part of its cells.
      A reference to a deleted cell, or a range that loses all of its cells,
      becomes `#REF!` inside the formula, as in `=#REF!*2`.
    - A formula names another tab by its name. Renaming a tab renames it in
      every formula that reads it. A formula that names a tab that does not
      exist keeps its text and shows `#REF!`; it reads the tab again once a
      tab has that name, whether through undo, tab recovery or a new tab of
      that name.
    - Undoing a deletion of rows or columns also gives each formula that
      the deletion changed the text it had before the deletion. A formula is
      left as it is if someone has edited it since, or if rows or columns of
      its own tab or of a tab it read were inserted, deleted or sorted after
      the deletion; rows and columns added at the end do not count. No
      other operation repairs a `#REF!` inside a formula.
    - A rollback restores every formula exactly as revision R has it
      (rule 50).
    - Whole-column and whole-row references cover rows and columns added
      later.
    - **A formula in flight.** While a person types a formula, structural
      changes that arrive adjust the references already typed, in the
      editor, by the rules above. A formula a person commits is read
      against the grid their screen showed. If rows or columns were
      inserted, deleted or sorted, or a tab was renamed, before the server
      receives it, the server adjusts its references, `$` references and
      references to other tabs included, as if the formula had been in its
      cell since that screen state. If the server no longer keeps the
      changes made since then, or the sheet was rolled back since then, it
      refuses the edit and the text is offered back (rule 24).
    - A rollback is not a change the editor can adjust to. When a rollback
      reaches a browser in which a person is typing in a cell, a formula or
      not, the editor closes without committing, nothing is written, and
      the typed text is offered back in a notice (rule 24). The person
      types it again against the restored grid.
15. **Copying formulas.** When a formula is copied by paste, by fill or by a
    sort moving its row, each reference without `$` shifts by the distance
    the formula moved. A shifted reference that would leave the grid limits
    becomes `#REF!`. Cutting and pasting moves a formula without shifting
    what it reads: pasted into another tab, a reference written without a
    tab name gains the name of the tab the formula was cut from. Formulas
    elsewhere that read the cut cells keep their addresses and read whatever
    those addresses hold after the paste.

## Viewing

16. **Grid.** The browser shows one tab at a time: column letters across the
    top, row numbers down the left, gridlines, and each cell with its shown
    value and format. Text longer than its cell runs on over blank cells to
    its right unless the cell wraps; numbers, dates and booleans never run
    on. Only the cells in view are drawn, so a tab at its limits scrolls as
    a small one does.
17. **Tab bar.** A bar under the grid lists the tabs in order. Selecting one
    shows it. Which tab a person has open, where they have scrolled and what
    they have selected is theirs alone and is not saved.
18. **Formula bar.** A bar above the grid shows the address of the active
    cell and what it holds: the formula text for a formula, and otherwise
    the content as it would be typed.
19. **Status.** Selecting two or more cells that hold numbers shows their
    sum, average and count. The sheet shows whether every edit is saved
    (rule 38).
20. **Themes and sizes.** The sheet follows the light or dark theme of the
    system. On a phone-width screen a person scrolls, selects a cell, reads
    the formula bar, edits one cell at a time and switches tabs; the
    remaining editing of rule 21 may need a wider screen.
20a. **Find.** Ctrl or Cmd+F opens a find field for the open tab. It matches
    text in what cells show, capitals ignored, counts the matches, and moves
    the selection to the next or previous one. It works the same on a Public
    route. Replacing is [not in S1](#not-in-s1).
20b. **Screen readers.** The grid is exposed as a grid, although only the
    cells in view are drawn.
    - It states the tab's row and column counts, and each cell its row
      number and column letter.
    - Moving to a cell reads its address and what it shows, says when it
      holds a formula or an error, and says when it is part of the
      selection. The formula bar is a labeled field that reads the content
      of the active cell.
    - The tab bar is a list of tabs with the open one marked. Menus and
      dialogs are named, take focus when they open and give it back when
      they close.
    - Tab moves between cells inside the grid, as in other spreadsheet
      programs. F6 and Shift+F6 move between the top bar, the toolbar, the
      formula bar, the grid and the tab bar, and Escape returns to the
      grid. The grid's description names these keys.
    - Save status, notices of refused, ignored and overwritten edits, agent
      activity and the find count are announced without moving focus.

## Editing in the browser

21. **Editing.** A person with write access can do all of the following
    without leaving the grid.

    | Action | How |
    |---|---|
    | Select | Click a cell; drag or Shift-click for a range; click a column letter or row number for whole columns or rows; the corner selects the tab. Arrow keys, Home, End, Page Up and Page Down move, with Shift to extend and with Ctrl or Cmd to jump to the edge of the data. Tab moves one cell right and Shift+Tab one cell left. |
    | Type into a cell | Typing replaces the content; Enter or F2 or a double click edits it in place. While editing, Enter commits and moves down, Tab commits and moves right, each with Shift for the opposite direction, and Escape cancels. Alt+Enter or Option+Enter adds a line break. |
    | Enter a formula | After `=`, clicking a cell or dragging a range inserts its reference at the caret. Typing a function name offers the matching functions with their arguments. |
    | Clear | Delete or Backspace clears the content of the selection and keeps its formats. |
    | Copy, cut, paste | To and from the system clipboard as tab-separated text, so a range moves to and from other spreadsheet programs with its shown values. A paste inside the same sheet also carries formulas (rule 15) and formats. Paste values only is a separate command. |
    | Fill | Dragging the handle at the selection's corner, or Ctrl+D and Ctrl+R, copies the first cells over the rest, shifting formulas by rule 15. |
    | Format | A toolbar sets the formats of rule 6 on the selection, with Ctrl or Cmd+B, I and U for the marks. |
    | Resize a column | Drag the edge of its letter; a double click fits it to its content. |
    | Insert and delete rows and columns | From the menu of a row number or column letter: insert above, below, left or right, as many as are selected, and delete the selected ones. |
    | Sort | Sort the selected rows, or all rows under the frozen rows, by one column, ascending or descending (rule 22). |
    | Freeze | Freeze rows down to, or columns up to, the selection, or unfreeze. |
    | Tabs | Add, rename, reorder by dragging, and delete with the confirmation of rule 2. |
    | Export | Download the open tab as a CSV file of its shown values. |

    - Composition input (IME) works in a cell and in the formula bar: text
      under composition is never committed or cancelled by the keys that
      belong to the composition.
    - Every action has a keyboard path, and focus returns to the grid after
      a menu or a dialog closes.
22. **Sort.** A sort reorders whole rows of the tab: the rows of the
    selection, or every row under the frozen rows when one cell is selected.
    Each sorted row moves with its identity, so every cell of the row, in
    every column, moves with it. Sorting only some columns of a row is
    [not in S1](#not-in-s1).
    - Order ascending: numbers and dates from the smallest, then text from
      A with capitals ignored, then `FALSE`, then `TRUE`, then errors.
      Descending reverses that. Blank cells come last either way. A formula
      counts as its value. Rows that tie keep their order.
    - Formulas inside the moved rows shift by rule 15. Formulas in other
      rows keep their addresses and read whatever the sort put there.
    - Frozen rows never move.
23. **Undo and redo.** Each person can undo and redo their own last 100
    actions of rule 21, in order, with Ctrl or Cmd+Z and Shift+Ctrl or
    Cmd+Z, for as long as the tab stays open.
    - An undo changes only what that action changed and applies as a new
      edit by the [concurrency rules](#concurrency-rules). Where someone
      else has changed the same value since, the undo leaves their value and
      says so.
    - Undoing a deletion of rows, columns or a tab brings them back with
      their cells and identities, where they were among the rows, columns
      or tabs that remain. A tab whose name was taken meanwhile comes back
      as "<name> (restored)". Formulas are repaired as rule 14 says.
    - An undo never deletes another writer's work. Undoing an insertion of
      rows or columns, the creation of a tab or a tab recovery removes them
      only if no other writer has since changed content or a format in
      them. Otherwise the undo is skipped with a notice and they stay.
    - Undoing a sort puts the rows it moved back in their earlier order
      among themselves. Rows inserted since stay where they are and rows
      deleted since stay deleted. If another writer has sorted any of those
      rows since, the undo is skipped with a notice.
    - A rollback of the sheet clears everyone's undo history, and so does a
      restart of the host. A browser tab that was disconnected for more than
      10 minutes may also find its undo history cleared when it reconnects,
      and so may one that was disconnected while many other tabs opened the
      sheet.
    - The host keeps undo history within fixed limits of memory, disk space
      and work, which the technical design gives. An action that no longer
      fits them can no longer be undone or redone. It is skipped with a
      notice that says so, nothing in the sheet changes, and the person's
      other actions stay available. Where the host could keep only part of
      what an action needs, its undo skips that part with the same notice
      and applies the rest. Some of these limits are shared by everyone
      who edits the sheet, so heavy undo history of others can make a
      person's largest or oldest actions leave their undo history earlier.
24. **Refusals.** When the host refuses an edit, the grid returns to the
    host's state, a notice says why, and typed text is offered back so it
    can be copied.

## Collaboration

25. **Presence.** Private editors see who else has the sheet open, which tab
    each person is on, and each person's active cell or selected range,
    outlined in their color with their name. A cell someone else is editing
    is marked. The mark is not a lock.
26. **Identity.** On Tailscale routes the sheet shows the trusted Tailscale
    identity as verified. On Local and NetBird routes it uses the name
    entered on first opening and shows it as unverified. A value the client
    sends can never earn the verified mark.
27. **Agent activity.** An agent's write appears as a short notice, such as
    "The agent wrote 42 cells in Budget", with a brief highlight of the
    cells that changed. Presence is never exposed to agents.

## Concurrency rules

A **value group** is a set of values that only make sense together. They
change together and are overwritten together.

28. **Value groups.**

    | Group | Contains |
    |---|---|
    | Cell content | what one cell holds: its value or its formula |
    | Cell format property | one of a cell's format properties of rule 6: each mark, the text color, the fill color, the alignment, wrap, the number format with its settings |
    | Column width | one column's width |
    | Row order | the order of the rows of one tab |
    | Column order | the order of the columns of one tab |
    | Tab name | one tab's name |
    | Tab settings | one tab's frozen rows and columns |
    | Tab order | the order of the tabs |

29. **Different values all apply.** Two people typing in different cells
    both keep their text. One person making a cell bold while another
    colors it, or changes its content, all stay.
30. **In the same group, the last write stays.** When two writers change the
    same group, the value the server received later stays. Text inside one
    cell is not merged character by character.
    - The person whose content was overwritten gets a notice with their
      text, which they can copy while that tab remembers it.
    - An overwritten value can be recovered from history only when a
      retained revision records it.
31. **Edits follow identities.** A person's edit names its cell by the row's
    and the column's identity, not by an address. If rows or columns were
    inserted, deleted or sorted before the server received it, it applies to
    that same cell at its new address (principle 4). A sort moves whole
    rows, so every cell of a row has one destination. A formula in such an
    edit is adjusted by rule 14.
32. **Deletion wins.** An edit to a cell of a deleted row, column or tab
    does not apply and is reported as ignored, with the text offered back.
    A paste or a fill that covers both deleted and remaining cells applies
    to the remaining ones. A cut cell whose destination was deleted stays
    where it was, and the same cut writes nothing over it, so a cut cell
    whose destination is such a cell does not move either.
33. **Order of structure edits.** Inserts, deletes and sorts apply in the
    order the server receives them. An insert names the row or column it is
    beside by identity; if that one was deleted meanwhile, the insert goes
    where it was. A person's sort names its rows by identity: it reorders
    those of them that still exist, in the places they occupy when the
    server receives it, and rows inserted among them meanwhile stay where
    they are. Two sorts that share rows leave those rows in the order of
    the later one.
34. **Restored rows, columns and tabs.** When these reappear through undo,
    tab recovery or a rollback, edits prepared before that do not apply to
    them.
35. **Whole-sheet rollback** follows the rollback rule of the shared
    Revision model. Edits that arrive after the rollback apply on top of the
    restored sheet by the rules on this page, except that edits prepared
    before the rollback to rows, columns and tabs it removed or brought back
    are ignored. An edit to a cell whose row and column both exist before
    and after the rollback applies to that cell by identity, wherever the
    rollback put it, except that a formula committed before the rollback is
    refused (rule 14). Open browser tabs resync, text being typed in a cell
    when the rollback arrives is not committed and is offered back
    (rule 14), and everyone's undo history is cleared.
36. **Agent writes.**
    - One agent write call applies whole or not at all. If validation, a
      limit, the disk quota or a guard fails, nothing changes.
    - Agents name tabs by name and cells by address. Every write call to an
      existing sheet names the state the agent read (rule 43). The host
      refuses the whole call, and the agent reads again and resends, if
      since that state a tab the call names was renamed, deleted or replaced
      by another tab of the same name, or if the call names an address in a
      tab whose rows or columns were inserted, deleted or sorted since that
      state, or if the sheet was rolled back since that state, which can
      move rows and columns that exist before and after it. Rows and columns
      added at the end of a tab do not count, because no address that
      existed changes its meaning. A name therefore always means the tab the
      agent read, and an address the cell it read. This covers calls that only rename, move or
      delete a tab or change its settings. A call that only adds tabs, or
      that creates the sheet, needs no state.
    - Changes other writers made since that state to cell content, formats,
      widths and settings do not refuse the call. They are merged by the
      same group rules as a person's edits.
    - An agent can give a guard: refuse if the sheet is no longer in the
      state it read, or refuse if the content of any cell of named ranges
      changed since then. The first guard fails after any change of state, a
      change of the calculation date included. The second compares what the
      cells hold, not what their formulas calculate. A guard checks the
      current state, including people's edits that are not yet part of a
      revision.
37. **Reported results.** For a person's action, the result tells apart what
    was applied, what was ignored (a deleted target) and what was refused
    (validation, limit, quota). An agent's call either applies whole or is
    refused with its reason.

## Disconnection and reconnection

S1 does not support offline editing, but people do not lose edits through
ordinary disconnections. These rules match the board's and the notebook's.

38. The sheet shows **Saved** only after the server confirms that it
    recorded the edit durably. Edits shown as saved stay even if the host
    restarts right after.
39. A person's own unconfirmed edits stay on screen and are sent again on
    reconnection. A resent edit never applies twice.
40. On reconnection the browser receives other people's changes made in the
    meantime and merges its own unconfirmed edits by the
    [concurrency rules](#concurrency-rules). If the server can no longer
    accept them, editing stops and the sheet explains why; the person can
    copy their unconfirmed text out of the notice. Closing or reloading a
    browser tab with unconfirmed edits warns first.

## Agents

41. **Create.** `create_flat {type: "sheet"}` creates a sheet with one empty
    tab. `update_sheet` also creates a sheet on a slug that does not exist
    yet.
42. **Read.** `get_sheet` reads the sheet as structured data.
    - Without a tab, it returns the tabs in order (id, name, row and column
      counts, the smallest range that holds every filled cell, frozen rows
      and columns) and an identifier of the current state.
    - With a tab and a range, it returns the cells of that range, row by
      row, with blank rows and columns at the end left out. A cell reads as
      its value: a number, text, a boolean, or an object for a date, a time
      or a date with time. Text that starts with `=` or an apostrophe reads
      with a leading apostrophe. A formula reads as an object of its text
      and its calculated value or error. Every value a read returns can be
      written back unchanged (rule 43). Asked for, the read also returns what each
      cell shows under its number format, and the formats and column widths
      of the range, grouped into ranges that share a format.
    - `format: "csv"` returns the range as CSV text of the shown values.
    - One read returns at most 10,000 cells and 4 MiB; a larger range is
      read in pages, and a page that would pass the byte limit ends early
      and says where the next one starts. Every page of one read comes from the same state, even when
      people edit meanwhile. The host keeps that state for a limited time;
      when it has expired, the read says so and the agent reads again.
43. **Write.** `update_sheet` sends a list of operations in one call, with
    the atomicity, the state and the guards of rule 36. The operations:

    | Operation | Does |
    |---|---|
    | write cells | Writes a rectangle of content from a top-left address. |
    | clear cells | Clears the content of ranges, and their formats when asked. |
    | format cells | Sets format properties of rule 6 on ranges; properties it does not name stay. |
    | set column widths | Sets the width of columns. |
    | insert or delete rows or columns | By address and count. |
    | sort rows | Sorts a run of rows by one column (rule 22). |
    | add, rename, move or delete a tab | A tab is named by its name. |
    | set tab settings | Frozen rows and columns. |
    | import csv | Writes CSV text from a top-left address, each field read by rule 5. |

    - Operations apply in order, each on what the ones before it left, so
      an address after an insert in the same call counts from that insert.
    - In `write cells`, the JSON type decides what a cell holds: a number is
      a number, a boolean a boolean, `null` clears the cell, an object names
      a date, a time or a date with time, or is a formula object as a read
      returns it, of which only the formula text is written, and a string is
      text, except that
      a string of `=` and at least one more character is a formula and a
      string starting with an apostrophe is the text after it. A string is never turned into a
      number or a date. `import csv` is the operation that reads text as
      typed.
    - One call writes at most 50,000 cells and 8 MiB. Cells the call does
      not name stay as they are.
    - The result returns the identifier of the new state, how many cells
      and formulas the call wrote, how many of those formulas have an error
      as their value, and the address and error of the first 100 of them, so
      the agent sees a wrong formula without another read. It returns no
      other values.
44. **Revisions.** Agents use the existing revision tools (`list_revisions`,
    `get_revision`, `rollback_revision`, `pin_revision`) without approval.
    `get_revision` reads an earlier revision as `get_sheet` does.
45. **No versions.** Draft, version, preview, deployment and version
    rollback tools refuse a sheet and point to the sheet and revision tools.
    Version-model tools cannot write sheet content.
46. **Guidance.** The guide gets `topic.sheet` when the feature ships: when
    to choose a sheet over a notebook database or a table in a document,
    the cell and address model, the formula syntax and function list, the
    state and guard rules, and how to build a table (headers first, formulas
    for anything derived, formats last). The guide index routes to it.

## Revisions

A sheet follows the Revision model of [content-models.md](content-models.md)
unchanged.

47. **Revision unit.** One agent write call is one revision. A person's edits
    are grouped per person and close after 30 seconds idle or 5 minutes from
    the group's start. One rollback is one revision. Before an agent write or
    a rollback applies, open groups close as one revision crediting everyone
    who edited.
48. **Whole sheet.** A revision holds the whole sheet: every tab, cell,
    format, width and setting. Calculated values are not part of a revision;
    they follow from its content. Unchanged parts share storage.
49. **Quotas and pins** follow the existing rules: defaults of 100 revisions
    and 20 pins, changeable per flat, 0 means unlimited.
50. **Rollback.** Rolling back to revision R creates a new revision equal to
    R's whole sheet: every tab, cell, formula text, format, width and
    setting exactly as R has them, whatever was edited since. It adds only
    metadata, is never refused for disk quota, and may leave the sheet over
    its quota (rule 57).
51. **History panel.** People with write access open History from the sheet
    to list revisions (kind, author, time, message and which tabs changed),
    open an earlier revision read-only with its calculated values, pin and
    unpin, and roll back the whole sheet.
52. **Tab recovery.** With an earlier revision R open, a person picks a tab
    and restores it. Unlike a rollback, recovery adds one tab to the current
    sheet and leaves the rest as it is.
    - The tab comes back at the end of the tab bar with R's rows, columns,
      cells, formats, widths and settings. It keeps its name if no tab has
      that name now. Otherwise it is named "<name> (restored)", numbered if
      that is taken too. A tab that still exists is recovered the same way,
      as a copy.
    - Formulas in the recovered tab keep the text they have in R, with one
      exception: when the tab comes back under another name, a reference in
      its own formulas that names the tab by its name in R is rewritten to
      the new name, as a rename does (rule 14). A reference to the tab's own
      cells, written with or without the tab's name, therefore reads the
      recovered tab, and stays with it through later renames.
    - A reference to another tab reads the tab of that name as it is now,
      at the same address, and shows `#REF!` if there is none. Recovery does
      not adjust these references for rows or columns inserted, deleted or
      sorted in other tabs since R.
    - No formula text in another tab changes. A formula elsewhere that
      names the recovered tab's name reads it again (rule 14) at the
      address it names, whether or not R holds anything there. A formula in
      which a deleted row or column had already become `#REF!` stays so.
    - Recovery is an ordinary edit: it is part of the person's edit group,
      it can be undone (rule 23), and it changes no content of other tabs.

## Lifecycle and access

53. **Runs at once.** Creating or writing a sheet applies at once without
    approval. A new sheet is Private. While
    [public instance mode](public-instance.md) is on, a sheet is created as
    that page sets for every type, through `create_flat` and through a first
    `update_sheet` alike: Public on the host's default public network,
    without a visibility approval, with its Public address returned and no
    private link handed out or served. If that creation cannot be made
    Public, it fails as that page says and no Private sheet is left behind
    in its place.
54. **Visibility and deletion need approval.** Making a sheet Public, making
    it Private again, and deleting it each need operator approval, as for
    every flat. A Public approval does not pin a revision, and the sheet as
    it is when the approval applies becomes Public.
55. **Write access.** Only Private routes (Local, Tailscale and NetBird) and
    MCP can write. The sheet trusts only the Flats core's route decision for
    the access level and treats a missing or unknown decision as Public. It
    accepts browser write requests only from the same origin.
56. **Public boundary.** A Public route receives only the current sheet
    content: tabs, cells with their formulas and calculated values, formats,
    widths and settings.
    - Viewers scroll, switch tabs, select and copy cells and download a tab
      as CSV. None of this is saved.
    - There is no editing, undo, presence or History. The revision list and
      earlier revisions are not reachable.
    - When the sheet returns to Private, its route stops, or it is renamed
      or deleted, open Public connections close at once.
57. **Disk quota.** A sheet follows the existing
    [quota rules](content-models.md#quotas) unchanged. A person's edit is
    checked before it applies, including the space its revision will need,
    and refused if it does not fit; the optimistic display is undone. Agent
    writes get the same check. While a sheet is over its quota, every edit
    is refused, a deletion included, until the quota is raised or space is
    freed.
58. **Console.** The console lists sheets with their own label and icon and
    links to the sheet. It shows no versions, previews, database or server
    settings for a sheet.
59. **No outside requests.** The sheet screen requests no outside resources
    and runs no content script or HTML. Cell text that looks like a web
    address is shown as text; S1 has no links in cells.
60. **Exports are plain data.** A CSV export, in the browser and through
    `get_sheet`, writes one field per cell: what the cell shows.
    - Fields follow RFC 4180: a field that holds a comma, a quotation mark
      or a line break is quoted, and a quotation mark inside it is doubled.
      Lines end with a carriage return and a line feed. The export covers
      the tab's rows and columns up to the last filled cell. The text is
      UTF-8. A file, downloaded in the browser or written by the CLI,
      starts with a byte order mark, so that a program opening it directly
      reads it as UTF-8; `get_sheet` returns the text without the mark.
    - Fields are separated by commas. A field that holds a comma, a
      semicolon, a tab, a quotation mark or a line break is quoted.
    - The apostrophe check is made on the field as it will be written,
      whatever the cell holds. A field whose first character, after any
      leading spaces, is `=`, `+`, `@`, a tab, a carriage return or a line
      feed is written with a leading apostrophe.
    - A field that starts with `-` is written with a leading apostrophe
      unless the whole field reads as a number by rule 5: a minus sign, then
      at most one of the currency signs `$`, `€`, `£`, `¥` and `₩`, then
      digits with optional commas and decimals, then an optional exponent
      or an optional `%`. Only the space character counts as a leading
      space. A
      negative amount under a currency symbol made of letters, such as
      `-CHF5`, therefore gets the apostrophe and arrives as text.
    - Supported ways of opening the file are those that read it as
      comma-delimited UTF-8 and leave text as text: LibreOffice Calc and
      Apple Numbers opening the file, import into Google Sheets, and
      Microsoft Excel opening the file on a system whose list separator is
      a comma, or importing it with the comma delimiter chosen. In each of
      them every field of an export shows as text or as a number, date or
      boolean, and none is calculated. The apostrophe may show as part of
      the text.
    - Outside that, Flats promises nothing: a program set to another
      delimiter, such as Excel on a system whose list separator is a
      semicolon, splits fields at that character, and an import option that
      turns text into formulas calculates it.

## Success criteria

These are the release criteria for S1. The technical design fixes the
reference data generator and the measurement scripts.

**Reference environment**

- Desktop: an Apple M2-class laptop, the latest stable Chrome and Safari, a
  1440×900 window. Phone: an iPhone of the last three generations, latest
  Safari.
- Network: tailnet RTT of 50 ms or less, bandwidth of 50 Mbps or more.
- Data "reference sheet": 10 tabs. The large tab has 20,001 rows by 20
  columns: a frozen header row and 20,000 filled rows, in which 2 columns
  are formulas that read their own row (40,000 formulas). A second tab holds
  20 totals, each a `SUM` of one whole column of the large tab, with their
  expected values known from the generator.

| Criterion | Target |
|---|---|
| Open | Warm cache: the reference sheet's large tab is interactive in 1.5 seconds or less; cold cache: 3 seconds or less. |
| Scrolling | On the large tab, scrolling keeps p95 frame time of 16.7 ms or less. |
| Typing | p95 of 16.7 ms or less from a key press to the character on screen in the cell editor. |
| Recalculation | Changing one input cell of the large tab updates its row's formulas on the editor's own screen, and the column total on another screen showing the totals tab, in p95 of 150 ms or less after Enter. Both screens show the edit and the values that follow from it together, never one without the other. |
| Structure | Inserting a row at the top of the large tab completes on the editor's screen in 1 second or less. |
| Remote display | p95 of 200 ms or less from the server receiving an edit to it showing on another editor's screen, while 5 people each edit a cell twice per second. |
| Concurrent editing | Under that load, with one of them inserting a row every 5 seconds and another sorting the rows every 10 seconds, every edit the concurrency rules say must apply stays in the cell it was made in, with the same result on every screen. |
| Structure cases | A fixed list of cases, set by the technical design, each gives the result this page states: a formula committed while a row is inserted above what it reads (rule 14); references after insert, delete and sort, with `$` and other tabs; undo of an insert after someone else typed in the new row, and undo of a sort after someone else sorted (rule 23); a stale agent call that names a renamed, deleted or replaced tab (rule 36); tab recovery in each case of rule 52, with a formula that names its own tab recovered under another name; a rollback after formulas were edited (rule 50); a stale agent write across a rollback that only reorders rows (rule 36); a formula committed just before such a rollback, and a formula still being typed when it arrives (rules 14 and 35). |
| Formulas | Every row of the function reference table (rule 12) gives its expected result, in the browser, through `get_sheet` and in a CSV export. |
| Calculation date | With the host's date moved forward: on a small sheet (the offsite budget of the typical flow with one `TODAY` formula) an open screen shows the new value within a minute; on the reference sheet the recalculation starts within a minute and the new values appear together when it has ended; a read begun before the change returns the old date on every page; and a revision closed before it still shows the old value. |
| Limits | Each limit of rule 9 has its stated outcome. A write over a stored limit (tabs, rows, columns, filled cells, formulas, characters of a cell or a formula, nesting, cells read by one formula and by all formulas, typed text, bytes of a call) is refused whole with the limit's name. A text value inside a calculation that would pass 32,767 characters gives `#VALUE!` and the write is accepted. A state whose formulas calculate more text than it may hold is accepted and calculated in limited mode, with `#LIMIT!` exactly where rule 9 puts it, the same on every screen and through `get_sheet`; this is checked for a write that causes it and for a sheet that passes the limit only from a new calculation date on, and every result shows again after an edit brings the total back under the limit. While the host refuses a write, runs the longest recalculation the limits allow, with or without an edit starting it, or produces the largest CSV download, it answers other flats' requests within 1 second, and no reader of the sheet gets partly calculated values. |
| Durability | An edit shown as Saved is present, with the values that follow from it, after the host process is killed and restarted. |
| Export | A fixed list of cells, set by the technical design, covers every case of rule 60: each dangerous first character with and without leading spaces, text typed and text calculated by a formula, negative numbers in every number format, with a currency sign and with a currency symbol made of letters, text that holds a semicolon followed by `=`, quotes, commas and line breaks. It exports to the exact expected CSV bytes, and that file, opened in each supported way of rule 60, shows no calculated field. |
| Accessibility | With VoiceOver on Safari and on Chrome, a person moves through the large tab, hears each cell's address and content, edits a formula, switches tabs and hears a refused edit and the save status (rule 20b). |
| Reconnection | After disconnecting and reconnecting, each unconfirmed edit applies exactly once. |
| Recovery | Every tab of a retained revision can be viewed and restored. Rollback succeeds with the disk quota full. |
| Refusal | On a Public route, writes, presence and history access are all refused. An edit over the disk quota is refused before other participants see it. |
| Agent | From `topic.sheet` and the tools alone, an agent builds the offsite budget of the typical flow, then changes two inputs after a person inserted a row and added a column: the first write after the insert is refused by rule 36, the resent write applies, and the person's row and column are unchanged. |
| Dependencies | Runs from the single Flats binary, with no outside service, account or separate server. |

## Not in S1

Charts, conditional formats, dropdowns and checkboxes in cells, merged
cells, borders, cell comments and notes, links and images in cells, rich
text inside one cell, named ranges, arrays in braces, formulas whose result
fills several cells (such as `FILTER`, `SORT` and `UNIQUE`), random numbers
and the current time, pivot tables, filters and filter views, replacing
found text, sorting only some columns of a row, sorting by more than one
column, moving rows or columns by dragging, hiding and grouping rows or
columns, protected ranges, number and date entry by locale, a time zone per
sheet, custom number format patterns, import and export of `.xlsx` files,
printing, anonymous guest editing, offline editing, and character-level
merging of text inside one cell.

A sheet leaves S1 only as CSV, one tab at a time, so formulas, formats and
the other tabs do not travel with the file. Workbook files (`.xlsx`) are the
first candidate for the release after S1
([Open question 2](#open-questions)).

Also outside S1, as for every type: accounts and per-person permissions,
model calls inside the sheet, plugins and arbitrary HTML, replication across
hosts, and Version-model features.

## Current state

The `sheet` type, the server that calculates and stores it, the tools
`get_sheet` and `update_sheet`, the management routes, the `flats sheet` CLI
commands, revisions with rollback, the read-only Public route, the console
entries and `topic.sheet` are implemented but not released. Host tests cover
creation, writes, reads in pages and as CSV, revisions and rollback, import
after a crash, refusals between types, public instance mode and the unloading
of idle sheets. The
[technical design](sheet-technical-design.md#appendix-b-implementation-notes)
lists where the implementation differs from its text.

Not done yet:

- No success criterion is measured against the reference sheet on a host
  through the browser, MCP and CSV.
- The measurement scripts and manual protocols that section 22.4 of the
  technical design names (`scripts/sheet-perf/*`, `scripts/sheet-e2e/*`,
  `scripts/sheet-agent-eval`) are not written, and no command loads the
  reference sheet into a host. Until they exist, Open, Scrolling, Typing,
  Remote display, Accessibility and Agent cannot be measured, and
  Recalculation, Structure, Concurrent editing, Formulas, Limits, Export and
  Reconnection have only their engine half.
- Safari, a phone, touch and VoiceOver are unchecked, and nothing has run
  over a real tailnet.
- Exported CSV has not been opened in the spreadsheet programs the success
  criteria name.
- The lifecycle authority census (`scripts/lifecycle-gate.sh`) has sheet
  cases, and they have never run. The census cannot run at present, because
  the script does not build against its pinned historical source, on `main`
  as well.
- The limits of [Open question 1](#open-questions) are the planned values.

## Open questions

1. **Limit numbers.** The limits in rule 9 and the read and write sizes in
   rules 42 and 43 are planned values, fixed after measurement with the
   reference sheet.
2. **Workbook files.** Whether importing and exporting `.xlsx` files belongs
   in the release after S1 or later, and which parts of a workbook an import
   keeps.
