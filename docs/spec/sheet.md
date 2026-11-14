# Sheet content type

Status: planned. Nothing on this page is implemented yet; see
[Current state](#current-state). The technical design is in
[sheet-technical-design.md](sheet-technical-design.md).

This page sets what the first sheet release (S1) must do, why, and the rules
a sheet keeps. The technical design decides the storage format, the protocol
and where computation runs. The plan and its history are in
[issue 233](https://github.com/gosuda/flats/issues/233).

## Summary

A **sheet** (`type: "sheet"`) is a workbook of worksheets that people and
agents edit live in a cell grid. A cell holds a number, text, a boolean or a
formula. Formulas calculate with built-in functions that follow Excel. An
`.xlsx` or CSV file can come in and go out. Agents read and write cells
through MCP as structured data.

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
| **`sheet`** | **people and agents** | **every cell, format, row, column, worksheet and name** | **at once** |
| `page` | an agent | nothing | after an approved version and deployment |
| `flat` | an agent | nothing | after an approved version and deployment |

## Why

- **No type holds a free cell grid.** A notebook inline database is a typed
  table with targets of 5,000 rows and 32 properties. It has no formulas and
  cannot read or write a file. [notebook.md](notebook.md#not-in-n1) lists
  formulas and file export under "Not in N1".
- **People keep budgets, estimates and reports in xlsx files.** Flats cannot
  open one, and an agent cannot read or change one as structured data.
- **Large files are common.** A file of 100,000 to 1,000,000 rows must open,
  scroll and take an edit without a wait that grows with the row count.
- **Hosted spreadsheets need accounts and outside servers.** Flats already
  has a single binary, Private networking, the Revision model, live editing
  and MCP.

## Users and scenarios

| User | Scenario |
|---|---|
| An operator with a file | Imports a budget workbook, changes a few inputs, sees the totals change and exports the result. |
| A small team (tailnet members) | Edits one estimate at the same time, each person in a different worksheet or cell. |
| An agent | Reads a range as typed values, writes a block of rows with formulas in one call, and imports a file the user gave it. |
| A person with a large export | Opens a file of 1,000,000 rows, jumps to a row, finds a text and corrects cells. |
| An outside viewer | Reads, read-only and live, a sheet the operator approved as Public. |

A typical flow:

1. A person runs `flats sheet import budget.xlsx` and opens the Private URL.
   The import shows its progress, then the workbook and a report of the
   parts it dropped.
2. The person changes a rate in one cell. The cells that depend on it show
   new results. On a large worksheet the edit is confirmed at once and the
   results of rows off screen are calculated later.
3. The person asks an agent to "add the November rows from this CSV to the
   Costs worksheet". The agent reads the header row, then writes the rows and
   a total formula in one call. The open sheet shows them live, and that
   call becomes one revision.
4. A teammate inserts a column while the first person types. Both changes
   stay, and every formula still points at the cells it pointed at before.
5. Someone overwrites a block of cells by mistake. A person with write
   access opens History, opens a revision that still has the values, and
   rolls back.

## Phases

S1 is this page. Each later phase has its own spec status, pull request and
review cycle. S3 and S4 do not depend on each other and can swap.

| Phase | Scope |
|---|---|
| S0 (prototype, done) | A server flat that imported, showed and edited a large xlsx file inside the limits of the server flat runtime. Its three reports on issue 233 hold the numbers this design uses |
| **S1 (this page)** | The `sheet` type end to end: workbook model, built-in formulas, xlsx and CSV import and export, browser editor, live collaboration, revisions, agent tools, console and guide |
| S2 | Notebook links that point or copy: a link block, and copying a notebook database to a sheet and a sheet range to a notebook database |
| S3 | Live notebook links: a sheet range embedded in a notebook page, and sheet formulas that read notebook database rows |
| S4 | Custom functions: JavaScript functions that people and agents write in a sheet and call from cells |

## Principles

1. **Structure people can edit and agents can read.** Every cell, format,
   row, column, worksheet, merged range and name is structured data. People
   change all of it in the browser. Agents read and write the same data
   through MCP, never by scraping a rendered grid or editing a file.
2. **Concurrent editing is the default.** Several people and agents edit the
   same sheet at the same time. An edit that passes permission, validation,
   limit, disk quota and guard checks is never refused because someone else
   changed another value at the same moment. When two writers change the
   same value, the [concurrency rules](#concurrency-rules) decide; the
   server's order is final.
3. **Inputs are content, results are derived.** What a person or agent wrote
   into a cell is content, and history keeps it. A formula result is
   calculated from inputs and can always be calculated again. A revision
   holds inputs, not results, with the one exception of rule 22.
4. **Work is bounded.** No read and no edit starts work without a limit. An
   edit is confirmed in a time that fixed limits bound (a few slices and one
   stab). These limits do not grow with the number of formulas that read the
   edited cell. Work that is large by nature (an import, an export,
   a recalculation of a million formulas) runs in the background with
   progress, goes forward only, and can be stopped. The one exception is
   the apply of a large sort (rule 41), which pauses writes to the sheet for
   seconds.
5. **A cell runs no code.** A formula uses a fixed set of built-in
   functions. A sheet holds no macros and no script, and it never requests
   an outside resource. Custom functions (S4) will get their own rules.
6. **A file is a format, not the store.** Flats stores its own cell model.
   An xlsx or CSV file is read into that model and written from it. Import
   reports every part of a file that the model does not hold.
7. **Flats' existing rules apply unchanged.** A sheet does not redefine the
   Revision model, Private and Public access, the actions that need approval
   (visibility changes and deletion), the disk quota or fixed types.

## Content

1. **Explicit type.** A flat becomes a sheet only through
   `create_flat {type: "sheet"}`, a sheet write to a new slug, or a sheet
   import to a new slug. The type never changes, and saves of another type
   are refused.
2. **Workbook.** A sheet is one workbook: an ordered list of worksheets, a
   list of names (rule 10), a list of cell formats (rule 7) and two
   settings.
   - **Time zone.** `TODAY` and `NOW` use it (rule 23). A new workbook takes
     the time zone of the host. People and agents can change it.
   - **Date system.** 1900 by default. A workbook imported from a file that
     uses the 1904 date system keeps 1904. It cannot be changed.
   - A workbook always keeps at least one worksheet. Deleting the last one
     is refused.
3. **Worksheets.** A worksheet has an id unique in the workbook, a name, a
   position, a hidden flag, a number of frozen rows and of frozen columns,
   and a default row height and column width.
   - A name has 1 to 31 characters, none of `: \ / ? * [ ]`, and no
     apostrophe at its start or end. Names are unique in the workbook
     without regard to letter case.
   - People add, rename, reorder, duplicate, hide, show and delete
     worksheets. A duplicate gets new ids and the name "<name> (2)".
     Formulas inside the duplicate that read its own source worksheet read
     the duplicate. Duplicating a large worksheet takes time and shows
     progress.
   - A hidden worksheet is not in the tab row. Its cells stay content, and
     formulas read them.
4. **Rows and columns.** A worksheet has a number of rows and a number of
   columns. A new worksheet has 1,000 rows and 26 columns. An imported
   worksheet has the rows and columns that its file uses, and at least
   1,000 rows and 26 columns. The most are 1,048,576 rows and 16,384
   columns (A to XFD), as in Excel.
   - Each row and each column has an id that never changes and is never
     used again. Its position is separate from its id, so an insert moves
     later rows without changing what they are.
   - A row has a height and a column has a width, or the default of the
     worksheet. Each can be hidden. A hidden row or column is not drawn;
     its cells stay content, and formulas read them.
   - People add rows and columns at any place, and at the end.
5. **Cells.** A cell is the meeting of one row and one column. It holds one
   input, or none:

   | Input | Holds |
   |---|---|
   | Number | A finite IEEE 754 double. A date or a time is a number with a date or time format (rule 16) |
   | Text | Plain text of at most 32,767 characters |
   | Boolean | `TRUE` or `FALSE` |
   | Error value | One of the seven error values of Excel in rule 16, typed or imported as a literal. `#CIRC!` and `#LIMIT!` come only from calculation and cannot be typed |
   | Formula | Formula text of at most 8,192 characters (rule 14). The cell also has a result |

   A cell with no input and no format is not stored. Cell text is plain
   text: no rich text runs, no HTML.
6. **Typed input.** Text that a person types into a cell, an agent sends as
   typed input, or a CSV file holds, becomes an input by these rules, in
   this order. Flats has no locale setting, so the rules are the same for
   everyone.

   | Typed text | Becomes |
   |---|---|
   | Starts with `=` and has more characters | A formula. Text that does not parse is refused (rule 38) |
   | Starts with an apostrophe | Text without that apostrophe |
   | `TRUE` or `FALSE`, in any letter case | A boolean |
   | An error value, such as `#N/A` | That error value |
   | A number: digits with an optional sign, decimal point, grouping commas in groups of three, exponent, or parentheses for a negative amount. Examples: `1234.5`, `1,234.5`, `-3`, `1e3`, `(500)` | A number |
   | A number with a percent sign, such as `12%` | The number divided by 100, with a percent format |
   | A number with a currency sign in front (`₩`, `$`, `€`, `£`, `¥`), such as `₩1,000` | The number, with a currency format |
   | A date with the year first: `2026-11-13`, `2026/11/13`, `2026.11.13`, `2026년 11월 13일` | The date, with a date format |
   | A date with an English month name: `Nov 13, 2026`, `13 Nov 2026`, `November 13, 2026` | The date, with a date format |
   | A date in the order month/day/year, such as `11/13/2026` | The date, with a date format. A first number above 12, as in `13/11/2026`, stays text |
   | A time: `14:30`, `14:30:15`, `2:30 PM`, `오후 2:30` | The time as a fraction of a day, with a time format |
   | A date and a time with a space between them | The date and time, with a date and time format |
   | Anything else | Text |

   - A format comes with the value only when the cell has the General
     format. A cell that already has another format keeps it.
   - A whole number of more than 15 digits stays text, because a double
     would lose digits. Excel rounds such a number instead.
   - Leading and trailing spaces do not stop a number or date from being
     recognized. Text keeps its spaces.
   - Editing a text cell that these rules would read as something else
     shows an apostrophe in front, so saving it unchanged keeps it text.
7. **Formats.** A cell has a format. S1 holds these fields:

   | Field | Values |
   |---|---|
   | Number format | An Excel format code. S1 shows General, fixed decimals with or without grouping, percent, currency, scientific, text, and date and time codes, with up to four sections (positive; negative; zero; text). A code that S1 cannot show is kept and the value shows as General |
   | Bold, italic, strike | On or off, each |
   | Text color, fill color | An RGB color, or none |
   | Horizontal alignment | General, left, center or right. General puts text left, numbers right, and booleans and error values in the center |
   | Wrap | On or off |

   - A row and a column can have a format too. A cell without its own
     format shows the format of its row, or else of its column.
   - Formatting a whole row or column sets that format and also changes
     every cell in it that has its own format.
   - Colors keep enough contrast in the dark theme. The grid adjusts a
     text color that would be unreadable on the dark background, and the
     stored color does not change.
8. **Imported style table.** The style table of an imported xlsx file holds
   more than rule 7, for example borders, font family and size, and
   vertical alignment. S1 keeps that table and the theme of the file as
   assets of the workbook, and each imported format remembers its entry.
   - S1 does not show the fields it does not hold. Export writes them back,
     so another program shows the cell as before.
   - When a person changes a rule 7 field of such a cell, the cell keeps
     the other fields of its entry. Export writes a new entry that copies
     the old one and changes that field.
   - A sheet that never had a file, and a cell that got its format in
     Flats, export a style table that holds the rule 7 fields only.
   - The assets are content. A revision holds them, a rollback restores
     them, and a new import replaces them.
9. **Merged cells.** A merged range is a rectangle of at least two cells in
   one worksheet that shows as one cell. Merged ranges do not overlap.
   - Only the top-left cell holds an input and a format. A formula that
     reads another cell of the range reads an empty cell.
   - Merging a range in which other cells hold inputs deletes those
     inputs. The browser asks first and names how many. An agent must say
     so in the operation, or the call is refused.
   - Unmerging leaves the input in the top-left cell.
   - A merged range follows its rows and columns. Rows inserted inside it
     make it larger. Deleting some of its rows or columns makes it
     smaller, and a range of one cell stops being merged.
   - An input written to another cell of the range is refused, for a
     person and for an agent. The editor selects the whole range and
     writes to its top-left cell.
   - A sort (rule 12), and a move of rows or columns that would cut a
     merged range in two or change its rectangle, is refused.
10. **Names.** A name gives a cell, a range or a constant a word that
    formulas can use, as in `=SUM(Sales)`.
    - A name has a scope, which is the workbook or one worksheet, and is
      unique in its scope without regard to letter case. It has 1 to 255
      characters, starts with a letter, an underscore or a backslash,
      continues with letters, digits, underscores and periods, and is
      neither a cell reference nor `TRUE` or `FALSE`.
    - Its definition is one cell or one rectangular range of one
      worksheet, or one constant (a number, text or a boolean).
    - People add, rename, change and delete names in a Names dialog.
      Agents do it with operations.
    - A name that points at cells follows them as a formula reference does
      (rule 17). Renaming a name changes it in every formula that uses it.
    - A formula that uses a deleted name shows `#NAME?` and keeps the word.
      Defining that word again in the same scope makes the formula work
      again.
    - A name from a file whose definition is anything else (a formula, a
      relative reference, several areas, another workbook) is kept as
      text, shown in the dialog as not supported, and written back on
      export, except that a name whose text names a part that the import
      dropped (a table, a link to another workbook) is not written back,
      and the export report counts it. A formula that uses it follows rule
      22.
11. **Structure edits.** People and agents insert, delete, move, resize,
    hide and show rows and columns, and add, rename, reorder, duplicate,
    hide, show and delete worksheets.
    - Deleting a row or column deletes its cells. Undo and history bring
      them back with their ids.
    - A move changes positions only. Every cell reference, merged range
      and name still points at the same cells. A range follows its end
      rows and columns. Moving an end moves the end of the range with it
      (open question 12).
12. **Sort.** A sort orders the rows of one range by one or more of its
    columns, each ascending or descending, with an optional header row
    that does not move.
    - Sort order: numbers, then text without regard to letter case, then
      `FALSE`, then `TRUE`, then error values in their existing order.
      Empty cells come last in both directions. Ties keep their order.
    - A formula is sorted by its result as of the start of the sort. A
      formula that is pending then counts with its last result.
    - A sort moves inputs and formats between the rows of the range. Rows
      keep their ids, so a formula outside the range that reads a cell of
      the range still reads that position and now sees the new content. A
      formula inside the range moves with its row and its relative
      references move with it, as in a copy.
    - A sort is one action. A sort of at most the cell limit of one action
      (rule 13) applies at once. A sort of more cells is a large sort (rule
      41). A range that holds part of a merged range is refused.
13. **Limits.** Limits apply to the current workbook, and the host refuses
    an edit or import that exceeds one, whole, with the name of the limit.
    The grid size and the cell total are decided. The other numbers are S1
    targets, fixed by measurement before release
    ([Open questions](#open-questions)).

    | Limit | Target |
    |---|---|
    | Rows per worksheet | 1,048,576 |
    | Columns per worksheet | 16,384 |
    | Cells with an input or a format per workbook | 20,000,000 |
    | Rows of all worksheets of one workbook, empty rows included | 20,000,000 |
    | Worksheets per workbook | 200 |
    | Characters per text cell | 32,767 |
    | Characters per formula | 8,192 |
    | Nested function calls per formula | 64 |
    | Arguments per function call | 255 |
    | Distinct cell formats per workbook | 65,490 |
    | Merged ranges per worksheet | 100,000 |
    | Names per workbook | 10,000 |
    | Stored data per workbook: the text of each cell counted once for every cell that holds it, and 26 bytes for each cell, 36 for each row and 83 for each formula | 2 GiB |
    | Cells changed by one action or one agent call. Measurement may raise this number and may not lower it | 100,000 |
| Cells in one large sort, which runs as a job (rule 41). Measurement may raise this number and may not lower it | 2,000,000 |
    | Rows or columns created, deleted or moved by one action or one agent call, and blank rows that a write below the last row creates | 100,000 |
    | Requests of a Public viewer over the rates that the technical design fixes. A request over a rate is refused with a retry hint | The rates of the technical design |
    | Edits waiting for the host to finish marking their dependents, and the time that this work has been open without a break. A write over either is refused with a retry hint | 10,000 edits; 60 seconds |
    | Size of an imported file | `system.upload_max_bytes`, 20 MiB by default |

    Each limit is judged on its own. If a lowered limit leaves a workbook
    over it, the workbook and rollbacks to its revisions are not refused.
    Only edits that increase how far it is over the limit are refused.

## Formulas

14. **Syntax.** A formula is Excel formula text in the form an xlsx file
    stores, shown and typed in A1 style.
    - References: `A1`, absolute parts (`$A$1`, `A$1`, `$A1`), a range
      (`A1:B4`), a whole column or row (`A:A`, `1:3`), a cell or range of
      another worksheet (`Sheet2!A1:B4`, `'My sheet'!A1`), and a name.
    - Values: numbers, text in double quotes, `TRUE` and `FALSE`, and error
      values.
    - Operators: `+ - * / ^`, a leading `-` or `+`, a trailing `%`, `&`,
      and `= <> < > <= >=`. Operator order follows Excel: a leading minus
      binds tighter than `^`.
    - Function calls, with a comma between arguments and a period as the
      decimal point. Function names have no regard to letter case and are
      stored in capital letters in their English form.
    - Array formulas, results that fill several cells, references to other
      workbooks, table references such as `Table1[Amount]`, ranges over
      several worksheets and the space operator are not calculated (rule
      22).
    - A range where a single value is expected, as in `=A1:A10*2` in one
      cell, gives `#VALUE!`. Excel files of earlier versions that rely on
      implicit intersection get the same.
15. **Functions.** S1 has these 111 built-in functions. Each follows the
    documented behavior of Excel for the same arguments, with the
    differences listed below the table.

    | Group | Functions |
    |---|---|
    | Math | `SUM`, `PRODUCT`, `SUMPRODUCT`, `ABS`, `SIGN`, `INT`, `TRUNC`, `ROUND`, `ROUNDUP`, `ROUNDDOWN`, `MROUND`, `CEILING`, `FLOOR`, `MOD`, `POWER`, `SQRT`, `EXP`, `LN`, `LOG`, `LOG10`, `PI` |
    | Statistics | `AVERAGE`, `MIN`, `MAX`, `COUNT`, `COUNTA`, `COUNTBLANK`, `MEDIAN`, `LARGE`, `SMALL`, `RANK`, `RANK.EQ`, `STDEV`, `STDEV.S`, `STDEV.P`, `VAR`, `VAR.S`, `VAR.P` |
    | Conditional totals | `SUMIF`, `SUMIFS`, `COUNTIF`, `COUNTIFS`, `AVERAGEIF`, `AVERAGEIFS`, `MAXIFS`, `MINIFS` |
    | Logic | `IF`, `IFS`, `IFERROR`, `IFNA`, `AND`, `OR`, `NOT`, `XOR`, `SWITCH`, `TRUE`, `FALSE` |
    | Information | `ISBLANK`, `ISNUMBER`, `ISTEXT`, `ISERROR`, `ISNA`, `NA` |
    | Lookup | `VLOOKUP`, `HLOOKUP`, `XLOOKUP`, `INDEX`, `MATCH`, `CHOOSE`, `ROW`, `COLUMN`, `ROWS`, `COLUMNS` |
    | Text | `CONCAT`, `CONCATENATE`, `TEXTJOIN`, `LEFT`, `RIGHT`, `MID`, `LEN`, `TRIM`, `UPPER`, `LOWER`, `PROPER`, `SUBSTITUTE`, `REPLACE`, `FIND`, `SEARCH`, `REPT`, `EXACT`, `VALUE`, `TEXT` |
    | Date and time | `DATE`, `TIME`, `YEAR`, `MONTH`, `DAY`, `HOUR`, `MINUTE`, `SECOND`, `TODAY`, `NOW`, `EDATE`, `EOMONTH`, `WEEKDAY`, `WEEKNUM`, `DATEDIF`, `DAYS`, `NETWORKDAYS`, `WORKDAY`, `DATEVALUE` |

    - A formula gives one value. A formula whose result would be several
      values gives `#VALUE!`. Inside a formula, a function such as
      `SUMPRODUCT` can still work on whole ranges.
    - `XLOOKUP` returns one cell: its return range has one row or one
      column.
    - A lookup that asks for the nearest match (`VLOOKUP`, `HLOOKUP` and
      `MATCH` by default, `XLOOKUP` on request) expects keys sorted in
      ascending order, as Excel does. For keys that are not sorted, the
      result can differ from Excel.
    - Criteria of the conditional totals and `SEARCH`, and the exact-match
      lookups, take the wildcards `?`, `*` and `~`.
    - `TEXT` shows the format codes of rule 7 and gives `#VALUE!` for a
      code that S1 cannot show. `VALUE` and `DATEVALUE` read the forms of
      rule 6.
    - `RAND`, `RANDBETWEEN`, `OFFSET` and `INDIRECT` are not in S1.
16. **Compatibility rules.**
    - **Error values.** `#NULL!`, `#DIV/0!`, `#VALUE!`, `#REF!`, `#NAME?`,
      `#NUM!` and `#N/A`, as in Excel, and two that only Flats shows:
      `#CIRC!` for a circular reference (rule 20) and `#LIMIT!` for a
      formula over its work limit (rule 21). An error value in an operand
      or argument becomes the result, except in the functions that test
      for one. With several, the first in reading order wins. `ISERROR` gives
      `TRUE` for `#CIRC!` and `#LIMIT!`, `ISNA` and `IFNA` do not
      treat them as `#N/A`, and `IFERROR` does not catch them.
    - **Numbers.** IEEE 754 doubles. Two numbers are equal in a comparison
      when they agree to 15 significant digits. A result that is not
      finite is `#NUM!`.
    - **Dates.** A date is the number of days since 1899-12-31, so
      1900-01-01 is 1. Serial 60 is the day 1900-02-29, which never
      existed, as in Excel. A time is a fraction of a day. In a workbook
      with the 1904 date system, day 0 is 1904-01-01.
    - **Text.** Comparison has no regard to letter case. `EXACT` and `FIND`
      do.
    - **Empty cells.** An empty cell is 0 in arithmetic, empty text in text
      operations and `FALSE` in logic. Functions that count or average
      skip it.
    - **Conversion.** Text that rule 6 reads as a number, a date or a time
      is that number in arithmetic. Other text gives `#VALUE!`. `TRUE` is 1
      and `FALSE` is 0 in arithmetic.
    - **Order between kinds.** In a comparison, any number is less than any
      text, and any text is less than `FALSE`, which is less than `TRUE`.
17. **References follow cells.** A formula stores which rows and columns it
    reads, not their addresses, and shows A1 text made from their current
    positions.
    - Inserting, deleting or moving rows or columns never rewrites a
      formula. An insert and a cell edit at the same moment both apply.
    - A range grows when rows or columns are inserted between its ends,
      and shrinks when rows or columns inside it are deleted. A deleted
      end row or column leaves the range reaching to the nearest row or
      column inside it that remains. A range that loses all its rows or
      columns becomes `#REF!`. A moved end row or column moves the end of
      the range with it, and a range whose ends are in the wrong order is
      read with the ends swapped.
    - A reference to a deleted cell, row, column or worksheet shows and
      gives `#REF!`. If the deletion is undone, or the item comes back
      with its id through a rollback, the reference works again.
    - A whole column or row reference includes rows or columns added
      later. It covers the whole grid of the worksheet, as in Excel:
      `ROWS(A:A)` is 1,048,576, `COLUMNS(1:1)` is 16,384, `COUNTBLANK(A:A)`
      counts the blank cells of the whole column, and a row that does not
      exist yet reads as empty. A function that reads the reference directly,
      such as `SUM` or `COUNTIF`, reads only the rows that the worksheet has.
      An array expression over a whole column, such as `A:A=""` inside
      `SUMPRODUCT`, holds a value for each of the 1,048,576 positions and
      costs the time of them, as in Excel. `COUNTIF` and `COUNTIFS` with a
      criterion that matches blank cells count the blank positions of the
      grid, as `COUNTBLANK` does. With several criteria, the count is over
      the grid positions that match all of them, computed from the rows that
      exist. An array of more than 4,194,304 values gives `#LIMIT!`, which
      differs from Excel.
    - Copying or filling a formula moves its relative parts by the same
      distance and keeps its absolute parts. A relative part that would
      leave the worksheet becomes `#REF!`.
18. **Results.** A formula cell has a result that is either **current** or
    **pending**. A pending cell shows its last result, marked as not
    current, or nothing if it has none.
    - An edit makes pending every formula that reads the edited cell,
      directly or through other formulas. It changes no other result.
    - The host calculates a pending cell when a reader asks for it. The
      readers are the visible range of a Private editor, an agent read and
      an export. It also calculates pending cells in the background until
      none is left.
    - A reader never gets a result as current that an earlier confirmed
      edit made stale. While the host is still marking after an edit of a
      cell with many dependents, a window shows the results that the host
      can recalculate at once, and the other formulas as pending.
    - Results are not content (principle 3). After a rollback or an
      import, every formula is pending until it is calculated. A formula
      imported with a stored result shows that result as its last result
      meanwhile.
19. **Bounded reads.** One read calculates for a limited time and returns
    the cells it did not reach as pending. The server sends results as
    they become current, and the browser also asks again while pending
    cells are on screen. Each read and each background step keeps
    what it calculated, except a read that calculates a formula without a
    `dirty` row while the host's marking work is open. Repeated reads get
    further once that marking work is done.
20. **Circular references.** A formula that reads itself, directly or
    through other formulas, gives `#CIRC!`, and so does every formula of
    the cycle and every formula that reads one. A cycle never makes the
    sheet wait, and an edit that closes a cycle is not refused. When an
    edit opens the cycle again, the cells calculate normally.
21. **Work limit of one formula.** A formula may read a limited number of
    cells in one calculation: 50,000,000 as the S1 target. A range counts
    the cells that exist in it, not its positions, so a sum over a whole
    column of a large worksheet counts the cells that hold something. An array
    that an expression builds from a whole column counts a value for each
    position of the grid. A formula over
    it gives `#LIMIT!`. The limit counts cells, not time, so the same
    workbook gives the same result on every host. `IFERROR` does not
    catch `#LIMIT!` or `#CIRC!`.
22. **Formulas S1 cannot calculate.**
    - A formula that names a function outside rule 15 keeps its text,
      follows its cells (rule 17), and shows `#NAME?`.
    - A formula from a file that uses syntax outside rule 14 is kept as the
      text the file stores. It does not follow its cells, and a copy, fill
      or sort moves its stored result as a plain input, not the formula.
    - Either kind, when it comes from a file with a stored result, shows
      that result marked as **not recalculated**. This stored result is
      content: revisions, rollback and export keep it. Changing the input
      of the cell removes it. A formula that reads such a cell uses the
      stored result as if it were an input of that kind, so `=B1*100` over
      a cell `B1` with an unsupported function and the stored result 0.84
      gives 84. Without a stored result the cell gives `#NAME?`.
    - Export writes a formula of the first kind with its text, unless the
      text names a part that the import dropped (a table, a link to
      another workbook, or a name that rule 10 does not write back). It writes every
      other formula that S1 does not calculate, and every one that names a
      dropped part, as its stored result alone, so Excel never meets a
      reference that the file lacks. The import report and the export
      report count them.
23. **Time functions.** `TODAY` and `NOW` give the date and time of the
    calculation in the time zone of the workbook. They are the only functions
    whose result changes without an edit.
    - The host looks at the formulas that use them once a minute while a
      Private editor has the sheet open, and at most once an hour while only
      Public viewers have it open. A formula is pending only if its result
      changed at such a look. A look waits for the host's marking work. A formula that uses
      these functions can lag by the length of one pass of the step, which
      under a stream of edits can be many minutes.
    - An export, or an agent read that comes more than a minute after the
      last look, asks for a look first. The export waits for it, and the agent
      read waits for a limited time that the read states.

## Files

24. **xlsx import.** An import reads one `.xlsx` file into a workbook.
    - Into a new slug it creates the sheet. Into an existing sheet it
      replaces the whole workbook as one revision, so the content before
      it stays in history.
    - The same file always gives the same workbook and the same report.
    - An import that is refused, cancelled or interrupted changes nothing.
25. **What an import keeps.** Worksheets with their names, order and hidden
    flags; cell inputs and formulas; the stored results of formulas; the
    rule 7 formats and the style table (rule 8); row heights, column
    widths and hidden rows and columns; frozen rows and columns; merged
    ranges; and names.
    - Everything else is dropped and listed in the **import report** by
      kind and place (worksheet, and range where one exists): charts,
      images and drawings, pivot tables, macros, comments and notes,
      conditional formats, data validation, filters, tables, hyperlinks,
      links to other workbooks, embedded objects, protection, outline
      groups, print settings and rich text runs. Cells of a dropped table
      or pivot table stay as cells. Text with runs stays as plain text.
    - View state of the file, such as the selected cell, zoom and scroll
      position, is not imported and not reported.
    - The report has a second list of the formulas that follow rule 22,
      with their number. These are formulas kept, not parts dropped.
    - The report stays with the revision of the import, and people with
      write access open it from History.
26. **Import safety.** A file is a zip of XML from an untrusted source.
    - The host refuses a file larger than `system.upload_max_bytes` before
      any write, and the refusal names the setting.
    - While it reads, it enforces limits on the number of zip entries, the
      unpacked size of each entry and of all entries, the ratio of
      unpacked to packed size, the length of one XML tag and one text, the nesting depth of every
      part it parses, and the limits of rule 13. A file over a limit is refused with the name
      of the limit.
    - It refuses an encrypted or password-protected file, a legacy `.xls`
      file and a file that is not a zip, and says which.
    - It never requests a resource that a file names, and never runs
      anything a file holds.
    - Memory use of an import does not grow with the number of rows. An
      import through MCP also holds the base64 body of the call, at most
      2.4 times the file plus 2 MiB, counting the decoded file. The reader looks at the size and count
      of the zip directory before it builds the list of entries.
27. **xlsx export.** An export writes the current workbook as `.xlsx`:
    inputs, formulas, results, formats and the style table, sizes, hidden
    flags, frozen rows and columns, merged ranges and names.
    - Pending formulas are calculated before or during the export, so a
      program that does not recalculate still shows results. A result of
      `#CIRC!` or `#LIMIT!` is written as `#N/A`, and the export report
      counts those cells.
    - A text cell stays a text cell in the file, also when it begins with
      `=`, `+`, `-` or `@`, so an export cannot put a formula into another
      program.
    - The file comes from one state of the workbook, also when people edit
      during the export.
    - Memory use of an export does not grow with the number of rows.
28. **CSV.** CSV works for one worksheet at a time.
    - **Import** adds a new worksheet to a sheet, or creates a sheet with
      one worksheet. It reads UTF-8 with or without a byte order mark,
      fields in double quotes as in RFC 4180, and a comma between fields,
      or a tab for a `.tsv` file. Each field becomes an input by rule 6,
      except that a field is never a formula. A field that starts with `=`
      becomes text.
    - **Export** writes the results of one worksheet: numbers in full
      precision, dates and times in ISO 8601 form, booleans as `TRUE` and
      `FALSE`, and error values as their text. It writes UTF-8 with a byte
      order mark and CRLF line ends, so Excel reads Korean text. A text
      cell that begins with `=`, `+`, `-`, `@`, a tab or a carriage return
      gets an apostrophe in front.
29. **Round trip.** Importing an xlsx file, exporting the sheet and
    importing that export gives the same workbook: the same worksheets,
    cells, formats, style table entries, sizes, hidden flags, frozen rows
    and columns, merged ranges and names. Ids, and empty rows and columns
    after the last one in use, are not part of the comparison. A formula
    that the export wrote as its stored result alone (rule 22) compares as a
    plain value equal to that result. The second
    import report has no dropped part and lists the formulas of rule 22 that
    the export wrote as formulas. The first report names the formulas that
    became plain values, so nothing is lost without notice.
30. **Jobs.** An import and an export of a large workbook take longer than
    one request, so each runs as a job.
    - A job shows progress within 1 second of the host receiving the first
      bytes of the file, and until it ends. While the file is received,
      progress is the bytes received. A person or agent with write access can
      cancel it.
    - S1 shows no rows of an import before it is complete. People keep
      seeing and editing the earlier workbook until the import replaces
      it.
    - A sheet runs one import, rollback, large duplication or large sort
      at a time. A second one is refused while the first runs.
    - A host restart cancels a running job. The sheet is as it was before
      the job, and the job's record says that it was interrupted.

## Viewing

31. **Layout.** The screen has a row of worksheet tabs, a formula bar with
    the address or name of the selected cell and its input, and the grid
    with row numbers and column letters. Frozen rows and columns stay in
    place while the rest scrolls.
32. **Only what is visible.** The browser reads the visible range and a
    margin from the host, and never a whole worksheet. A jump to any row
    of a worksheet of 1,000,000 rows shows its cells without loading the
    rows before it. The scrollbar stands for row numbers, not pixels.
33. **Live.** An open sheet shows every change without a reload, whoever
    made it: cells in view, the worksheet list, sizes and names. Results
    that become current appear without a reload.
34. **Pending results.** A pending cell is drawn in a muted style that does
    not depend on color alone, and a screen reader says "calculating".
    While the workbook has pending cells, the screen says how many.
35. **Connection state.** The screen always shows whether the sheet is
    saving, saved or disconnected
    ([Disconnection and reconnection](#disconnection-and-reconnection)).
36. **Theme and phone width.** The sheet follows the light or dark theme.
    At about 400 px the tab row and the formula bar stay usable and only
    the grid scrolls.
37. **Accessibility.**
    - With the keyboard alone, people move between cells, select ranges,
      edit, use every menu and dialog, and switch worksheets.
    - A screen reader reads the grid as a grid: the address, the value
      and, for a formula cell, that it holds a formula.
    - Color is never the only way to convey meaning.

## Editing in the browser

Editing is available on Private routes only.

38. **Cells.** People type into a cell or into the formula bar. Enter or
    Tab stores the input, and Escape leaves the cell as it was.
    - The typed text shows at once. The host turns it into an input by
      rule 6 and answers with what the cell now shows.
    - A formula that does not parse is refused. The editor stays open with
      the text and says where the problem is.
    - While a formula is being typed, clicking a cell or dragging over a
      range puts its reference into the text. The editor suggests function
      names and shows the arguments of the current function.
    - While an input method (IME, for example for Korean) is composing,
      remote changes do not break the composition. They apply after the
      composition ends.
39. **Selection and keys.** Arrow keys, Tab, Enter, Home, End, Page Up and
    Page Down move as in a spreadsheet, and with Ctrl they jump to the
    edge of the data. Shift extends the selection. Clicking a row number
    or column letter selects the whole row or column.
40. **Copy, paste, fill and clear.**
    - Copying puts plain text with tabs and an HTML table on the
      clipboard, so other spreadsheet programs can paste it. Pasting from
      another program reads the same two forms: each value becomes an
      input by rule 6.
    - A paste inside the same sheet also carries formulas and formats.
      Relative references move as in rule 17. Paste values only carries
      results as plain inputs.
    - The fill handle copies the selected cells over the range it is
      dragged across. Two or more numbers or dates with a constant step
      continue that step.
    - A paste or fill that goes past the last row or column adds rows or
      columns, within the limits.
    - Delete clears the inputs of the selection and keeps formats.
41. **Large actions.** One action changes at most the cell limit of rule 13
    and creates, deletes or moves at most the row and column limit of rule
    13. It applies whole or not at all. The host checks the limits and the
    disk quota before it applies an action, and a refusal changes nothing.
    A paste or fill of 100,000 cells of ordinary size, with at most 80 bytes
    of text in each, is one action. A paste with more text is a series. Deleting a whole worksheet is one
    action, and a deletion of more rows than the limit is a series.
    - The browser sends a paste, fill, clear or format change of more
      cells as a series of actions, row range by row range. It shows
      progress and a way to stop. Stopping leaves the ranges already done. A part that the host refuses
      (a limit or the disk quota) stops the series in the same way, and the
      screen says which rows were done. One undo reverses what was done.
    - A sort cannot be split. A sort of more cells than the cell limit of
      one action, and at most the large-sort limit of rule 13, is a **large
      sort**. It runs as a job (rule 30) after the person confirms it, and
      the confirmation says that writes to the sheet pause. A sort of more
      cells than the large-sort limit is refused.
    - A large sort plans on one state of the worksheet while people keep
      editing, and then applies in one step. The apply pauses every write to
      the sheet, for about 5 seconds for each 1,000,000 cells, or about 20
      seconds for each 1,000,000 cells that hold formulas. These are
      estimates that the measurement of the technical design confirms.
    - If a key of the sort, or the structure of the worksheet, changed
      after the plan, or a merged range now holds part of the range, or the
      worksheet was deleted, the job ends as `conflict` and changes no
      content. A formula key that the host is still calculating can cause
      this, because its stored result changes. An edit of another column
      does not stop the sort, because the apply moves the current content of
      each row. The person starts the sort again.
    - A large sort closes the open edit group just before it applies, so
      the workbook before the sort is a revision (rule 55). The host can
      close the group once while it waits for the encoder, and a sort that
      does not apply leaves that revision in the history. A large sort has
      no undo entry. The way back is a rollback to the revision before the
      sort (rule 71).
    - An agent starts a large sort with the `sort` operation and the option
      `large`, as the only operation of its call (rule 63).
42. **Structure and formats.** Menus and keys insert, delete, move, resize,
    hide and show rows and columns; set the rule 7 formats; merge and
    unmerge; freeze rows and columns; sort a range; and manage worksheets
    and names.
43. **Find.** People search the open worksheet or the whole workbook for a
    text. Find looks in text inputs and in the text results of formulas,
    without regard to letter case. It does not look in numbers. It goes to the next or the previous match from the
    selected cell.
    - On a large workbook a find that must look at many cells shows that
      it is searching and how far it is, and can be stopped.
    - Replace is not in S1.
44. **Personal undo and redo.** As in a notebook: undo reverses only the
    person's own edits, never other people's or agents'.
    - The unit is one stored cell input, or one action such as a paste, a
      fill, a clear, a format change, a structure edit, a sort, a merge or
      a name change. A series of rule 41 is one unit.
    - If someone else later changed a cell the person had changed, undo
      leaves that cell and says "Changes other people made later were not
      undone". Undo of a sort is applied only if no cell of the sorted
      range changed after the sort, and otherwise it is refused whole with
      the same notice.
    - Undoing a person's own deletion restores the row, column or
      worksheet with its original id, so references to it work again.
    - Each tab has its own history, limited in entries and in bytes. An
      action too large to keep says that it cannot be undone before it
      applies. A reload, a rollback or an import clears the history.

## Collaboration

45. **Presence.** Private editors see who else has the sheet open, which
    worksheet each person is on and the cell or range each person has
    selected, marked with their name. This mark is not a lock.
46. **Identity.** On Tailscale routes the sheet shows the trusted Tailscale
    identity as verified. On Local and NetBird routes it uses the name
    entered on first opening and shows it as unverified. A value the
    client sends can never earn the verified mark.
47. **Agent activity.** An agent's write appears as a short notice, such as
    "The agent changed 240 cells in Costs", with a brief highlight of what
    changed. Presence is never exposed to agents.

## Concurrency rules

A **value group** is a set of values that only make sense together: they
change together and are overwritten together.

48. **Value groups.**

    | Group | Contains |
    |---|---|
    | Cell input | the input of one cell, and its stored result of rule 22 |
    | Cell format | the format of one cell |
    | Row place | the position of one row |
    | Row look | the height, hidden flag and format of one row |
    | Column place | the position of one column |
    | Column look | the width, hidden flag and format of one column |
    | Worksheet name | the name of one worksheet |
    | Worksheet place | the position of one worksheet |
    | Worksheet view | the hidden flag, frozen rows and columns, and default sizes of one worksheet |
    | Merged range | one merged range |
    | Name | the word, scope and definition of one name |
    | Workbook settings | the time zone |

49. **Different values all apply.** If one person types into a cell while
    another formats it, both stay. A write that names some fields of a
    group, such as the bold flag of a format, changes those fields of the
    current value, so two writers who change different fields of one
    format both stay. Two people editing different cells of
    one row both stay. An insert of rows and a cell edit at the same
    moment both apply, and the edit lands in the cell it was made in.
50. **In the same group, the last write stays.** When two writers change
    the same group, the value the server received later stays.
    - The person whose cell input was overwritten gets a notice with their
      text and a **Put my value back** action. The text can be put back
      only while that tab remembers it, and the host sends the notice only
      when the overwritten input was written less than five minutes before.
    - An overwritten value can be recovered from history only when a
      retained revision records it.
51. **Deletion wins.** An edit of a person to a deleted row, column,
    worksheet, merged range or name does not apply and is reported as
    ignored, and the text of an ignored cell input is offered back to the
    person. An agent call that targets a deleted item is refused whole
    (rule 56). Edits to other values in the same action still apply. An
    action that only makes sense whole, such as a paste, a fill or a sort,
    does not apply at all if any part of it is ignored or refused.
52. **Restored items.** A row, column or worksheet that comes back through
    undo or a rollback has its original id, and edits that arrive after
    that apply to it. An edit that was ignored while the item was deleted
    stays ignored. This differs from the notebook (rule 33), which ignores
    edits prepared before a restore. A sheet gives an item no incarnation,
    so it cannot tell such an edit apart.
53. **Structure stays valid.** The server checks every structure edit
    against the current workbook, not against what the writer saw. An edit
    that would pass a size limit, cut a merged range, overlap two merged
    ranges, repeat a worksheet name or a name, or delete the last
    worksheet is refused, and the writer's screen returns to the server's
    state.
54. **Formulas under concurrent edits.**
    - One person deletes a column that another person's formula reads.
      Both apply, and the formula shows `#REF!`.
    - A formula edit closes a cycle through a cell that someone else
      changed a moment before. Both apply, and the cells of the cycle show
      `#CIRC!`.
    - A long chain of formulas recalculates while other people edit. The
      cells of the chain show as pending until their results arrive, edits
      are confirmed meanwhile, and a new edit to an input of the chain
      makes its cells pending again.
55. **Whole-workbook replacement.** A rollback and an import into an
    existing sheet follow the rollback rule of the shared Revision model.
    Edits that arrive before the replacement are kept in the revision
    closed just before it. Edits that arrive after it apply on top of the
    new workbook by the rules on this page, except that edits prepared
    before it to items it removed are ignored (rule 51). Open tabs resync
    and everyone's personal undo history is cleared.
56. **Agent writes.**
    - One agent write call applies whole or not at all. If validation, a
      limit, the disk quota, a guard or a deleted target fails, nothing
      changes, as for a notebook (notebook rule 36).
    - An agent names cells by address. An address is a position, and a
      position is valid only for the rows and columns the agent read. A
      call that names cells by address therefore says which state of the
      worksheet structure it read, and the host refuses the whole call if
      rows or columns of that worksheet were inserted, deleted or moved
      since. The agent reads again and resends.
    - An agent can give a stronger guard: refuse if the workbook changed
      since the read, or refuse if cells of ranges that the call names
      changed since then. A
      guard checks the current state, including people's edits that are
      not yet part of a revision.
    - An agent's change that passes its guards is merged by the same group
      rules as a person's.
57. **Reported results.** For a person's action, the result tells apart
    what was applied, what was ignored (a deleted target) and what was
    refused (validation, structure, limit, quota). An agent's call either
    applies whole or is refused with its reason, and a deleted target is a
    reason.

## Disconnection and reconnection

S1 does not support offline editing, but people do not lose edits through
ordinary disconnections. These rules match the notebook's.

58. The sheet shows **Saved** only after the server confirms that it
    recorded the edit durably. Edits shown as saved stay even if the host
    restarts right after.
59. A person's own unconfirmed edits stay on screen and are sent again on
    reconnection. A resent edit never applies twice.
60. On reconnection the browser receives the current state of what it
    shows and merges its own unconfirmed edits by the
    [concurrency rules](#concurrency-rules). If the server can no longer
    accept them, editing stops and the sheet explains why; the person can
    copy their unconfirmed inputs out of the notice. Closing or reloading
    a tab with unconfirmed edits warns first.

## Agents

61. **Create.** `create_flat {type: "sheet"}` creates a Private sheet with
    one empty worksheet. `update_sheet` and `import_sheet` also create a
    sheet on a slug that does not exist yet.
62. **Read.** `get_sheet` reads the workbook as structured data.
    - Without a range, it returns the summary: worksheets with their ids,
      names, sizes and the range in use, the names, the settings, how many
      formulas are pending, and an identifier of the current state.
    - With a range, it returns the cells of that range: for each, the
      input as a typed value or formula text, the result of a formula with
      a pending mark where it is not current, and the text the cell shows.
      Formats are returned on request.
    - The host calculates pending cells of the range for a limited time
      before it answers. The answer says how many are still pending, so
      the agent can read again.
    - A range comes back in pages. Every page of one read comes from the
      same state, even when people edit meanwhile. The host keeps that
      state for a limited time; when it has expired, the read says so and
      the agent reads again.
    - With a text, it finds cells as rule 43 does and returns their
      addresses in pages.
63. **Write.** `update_sheet` sends a list of operations in one call, with
    the atomicity and guards of rule 56: set and clear cell inputs, set
    formats, fill a formula over a range, copy a range, insert, delete,
    move, resize, hide and show rows and columns, sort a range, merge and
    unmerge, manage worksheets and names, and change the settings.
    - A cell input is typed text (rule 6) or an explicit number, text,
      boolean, error value, date or formula, so an agent can store text
      that looks like a number.
    - An `op_id` makes a repeated call return its first result and apply
      nothing again.
    - Operations may refer to worksheets created earlier in the same call
      by a temporary name, and the result returns the new ids.
64. **Import and export.**
    - `import_sheet` takes an xlsx or CSV file in base64, of at most
      `system.upload_max_bytes`, as `import_board` takes an archive. It
      starts the job and returns its state, and `get_sheet` reads the
      progress and the report. A call that would replace the workbook of
      an existing sheet must say so, or it is refused.
    - `flats sheet import` and `flats sheet export` do the same from a
      file on disk, and the management API takes and returns the file
      body. No MCP tool returns a file. An agent reads ranges with
      `get_sheet`.
65. **Revisions.** Agents use the existing revision tools (`list_revisions`,
    `get_revision`, `rollback_revision`, `pin_revision`) without approval.
    `get_revision` reads the summary or a range of an earlier revision as
    `get_sheet` does.
66. **No versions.** Draft, version, preview, deployment and version
    rollback tools refuse a sheet and point to the sheet and revision
    tools. Version-model tools cannot write sheet content. A sheet tool on
    a flat of another type is refused as `not_sheet`, a version tool that
    meets a sheet as `invalid`, and a call over a limit or over the disk
    quota as `sheet_capacity`.
67. **Guidance.** The guide gets `topic.sheet` when the feature ships: when
    to choose a sheet over a notebook database, the cell and formula
    model, addresses and guards, pending results, and import and export.
    The guide index routes to it. Guide topics describe implemented
    behavior only.

## Revisions

A sheet follows the Revision model of [content-models.md](content-models.md)
unchanged.

68. **Revision unit.** One agent write call is one revision. A person's
    edits are grouped per person and close after 30 seconds idle or 5
    minutes from the group's start. One import is one revision. One
    rollback is one revision. Before an agent write, an import or a
    rollback applies, open groups close as one revision crediting everyone
    who edited.
69. **Whole workbook.** A revision holds the whole workbook: every
    worksheet, row, column, cell input, format, merged range and name, the
    style table assets and the settings. It holds no formula results,
    except the stored results of rule 22. Unchanged parts share storage,
    and closing a revision costs in proportion to what changed.
70. **Quotas and pins** follow the existing rules: defaults of 100 revisions
    and 20 pins, changeable per flat, 0 means unlimited.
71. **Rollback.** Rolling back to revision R creates a new revision equal to
    R's whole workbook. It is never refused for disk quota and may leave
    the sheet over its quota (rule 79). On a large workbook a rollback
    runs as a job with progress.
72. **History panel.** People with write access open History from the sheet
    to list revisions (kind, author, time, message and which worksheets
    changed), open an earlier revision read-only, read the import report
    of an import revision, pin and unpin, and roll back the whole
    workbook.
73. **Earlier revisions calculate on demand.** An earlier revision opens
    read-only with its inputs. While other earlier revisions are being opened
    and the host has no room for one more, the open is refused with a retry
    hint. The host calculates the results of the
    range someone looks at, with the same limits as a live read. `TODAY`
    and `NOW` give the present time there too. Opening a large earlier
    revision can take time and shows progress.

## Lifecycle and access

74. **Runs at once.** Creating or writing a sheet applies at once without
    approval. A new sheet is Private.
75. **Visibility and deletion need approval.** Making a sheet Public, making
    it Private again, and deleting it each need operator approval, as for
    every flat. A Public approval does not pin a revision, and the sheet
    as it is when the approval applies becomes Public.
76. **Write access.** Only Private routes (Local, Tailscale and NetBird) and
    MCP can write. The sheet trusts only the Flats core's route decision
    for the access level and treats a missing or unknown decision as
    Public. It accepts browser write requests only from the same origin.
77. **Public boundary.** A Public route receives only the current workbook:
    inputs, formulas, results, formats, merged ranges, sizes, frozen panes,
    names, the date system and the time zone, and the worksheet list without
    hidden worksheets.
    - Hidden worksheets, and the cells of hidden rows and columns, are not
      sent. Hiding is not access control: the formula or result of a
      visible cell can show a value that came from a hidden one, and a
      formula can name a hidden worksheet.
    - Viewers scroll, switch worksheets, select and copy cells, and find.
      None of this is saved.
    - There is no editing, undo, presence, History, import, export or
      import report. The revision list and earlier revisions are not
      reachable.
    - A Public read never starts a calculation. A viewer sees pending
      cells as pending until the background calculation, which only edits
      and the steps of rule 23 start, reaches them. While any
      record of the host's marking work is open, a Public viewer sees every
      formula as pending. A Public find
      looks only as far as a fixed limit allows.
    - When the sheet returns to Private, its route stops, or it is renamed
      or deleted, open Public connections close at once.
78. **No outside requests.** The sheet screen requests no outside resources
    and runs no content script or HTML. Cell text is drawn as text.
79. **Disk quota.** A sheet follows the existing
    [quota rules](content-models.md#quotas) unchanged. A person's edit is
    checked before it applies, including the space its revision will need,
    and refused if it does not fit; the optimistic display is undone.
    Agent writes get the same check. An import checks the space it needs
    before it starts and while it runs, counts the uploaded file, and
    leaves nothing when it is refused. While a sheet is over its quota,
    every edit is refused, a deletion included, until the quota is raised
    or space is freed.
80. **Console.** The console lists sheets with their own label and icon and
    links to the sheet. It shows no versions, previews, database or server
    settings for a sheet.
81. **No server runtime needed.** A sheet runs inside the host. It works on
    a host whose server runtime is disabled.

## Success criteria

These are the release criteria for S1. The technical design fixes the
reference data generators and the measurement scripts.

**Reference environment**

- Desktop: an Apple M2-class laptop, the latest stable Chrome and Safari, a
  1440×900 window. Phone: an iPhone of the last three generations, latest
  Safari.
- Network: tailnet RTT of 50 ms or less, bandwidth of 50 Mbps or more.

**Reference data**

- **Reference workbook.** 10 worksheets, 200,000 cells with an input, and
  40,000 formulas that include lookups and conditional totals.
- **Large workbook A.** An xlsx file of 20 MiB, the largest that the
  default limits accept, with text, numbers, dates and 5 percent formulas.
- **Large workbook B.** A worksheet of 1,000,000 rows and about 20 columns,
  imported with a raised `system.upload_max_bytes`. The S0 file of that shape
  had 18.4 million cells and 121 MiB as xlsx. B adds one column of 1,000,000
  distinct texts, one column of 1,000,000 formulas that read one shared cell
  and a cell of their own row, one column of 1,000,000 formulas that divide by
  the total of a whole column, and one column of 1,000,000 running balances
  that form a chain. The technical design measures its size.

| Criterion | Target |
|---|---|
| Open | The first worksheet of the reference workbook is interactive in 1.5 seconds with a warm cache and 3 seconds with a cold cache. |
| Edit to result | On the reference workbook, p95 of 200 ms or less from storing a cell input to its dependent results on the editor's own screen. |
| Scrolling | p95 frame time of 16.7 ms or less, on the reference workbook and on large workbook B. |
| Remote display | p95 of 200 ms or less from the server receiving an edit to it showing on another editor's screen, while 5 people each edit twice per second. The formulas on that screen that the edit made pending show as pending or with their new results in a p95 of 500 ms or less. |
| Concurrent editing | Under that load, every edit the concurrency rules say must apply stays, with the same result on every screen. |
| Formula conformance | For a fixed corpus of xlsx files that Excel calculated and saved, every formula that uses only rule 14 syntax and rule 15 functions, does not rely on implicit intersection, and does not give `#LIMIT!` from an array over whole columns (rule 21), gets the result the file stores: equal text, booleans and error values, and numbers equal to 12 significant digits. The technical design fixes the generator of the corpus. |
| Round trip | For a fixed corpus, rule 29 holds, and the first import report lists exactly the dropped parts and the formulas of rule 22. |
| Import safety | A hostile corpus (zip bombs, oversized entries and texts, deep XML, encrypted files, a legacy file) is refused inside a fixed memory bound and changes nothing. |
| Large import | Progress shows within 1 second of the host receiving the first bytes of the file. Large workbooks A and B import inside a fixed memory bound that does not depend on the row count. |
| Large open and scroll | On large workbook B, a jump to any row shows its cells in a p95 of 200 ms or less. |
| Large edit | On large workbook B, the host confirms an edit to the cell that 1,000,000 formulas read in a p95 of 200 ms or less. Formulas on screen that read it show new results in a p95 of 200 ms or less after that. Other people's edits are confirmed in a p95 of 200 ms or less while the host works on the rest. |
| Large recalculation | After that edit, with no other load, every one of the 1,000,000 results is current within 120 seconds. Each of those formulas reads the edited cell and one cell of its own row. |
| Long chain | On large workbook B, a column of 1,000,000 running balances forms a chain. After an edit of its first cell, every result is current within 600 seconds, and other people's edits are confirmed in a p95 of 200 ms or less meanwhile. |
| Large sort | On large workbook B, a sort of 2,000,000 cells ends as `done` within 180 seconds, and its apply pauses the writes of other people for at most 60 seconds. A large sort that is refused, cancelled or interrupted, or that ends as `conflict`, changes no content. The criteria Remote display and Large edit exclude the pause of a large sort. |
| Large export | Large workbooks A and B export inside the same memory bound. An export that starts while an edit is being marked waits for the marker and then runs. Importing each export again gives the same values in every cell, and Excel opens each file without a repair prompt and shows the same values for a fixed sample of ranges that the technical design names. |
| Find | On large workbook B, the next match of two fixed texts, one rare and one common, shows in a p95 of 1 second or less. |
| Reconnection | After disconnecting and reconnecting, each unconfirmed edit applies exactly once. |
| Recovery | Every worksheet and cell of a retained revision can be viewed and restored. Rollback succeeds with the disk quota full. |
| Refusal | On a Public route, writes, presence, history access, import and export are all refused, and a read starts no calculation. An edit over the disk quota is refused before other participants see it. |
| Dependencies | Runs from the single Flats binary with the server runtime disabled, with no outside service, account or separate server. |

## Not in S1

Charts, pivot tables, macros and VBA, conditional formatting, data
validation, cell comments, images and drawings, hyperlinks, rich text
inside a cell, borders and font choices as editable formats, text that
runs over into empty neighbor cells, saved filter
views and filters, tables with table references, array formulas and
results that fill several cells, `RAND`, `RANDBETWEEN`, `OFFSET` and
`INDIRECT`, links to other workbooks, encrypted or password-protected
files, the legacy `.xls` format, print layout and PDF, replace, find in numbers, a sort of
more cells than the large-sort limit, showing rows of an import before it is
complete, an upload in parts for a file over `system.upload_max_bytes`,
offline editing, anonymous guest editing, per-range permissions, notebook
links (S2, S3) and custom functions (S4).

Also outside S1, as for every type: accounts and per-person permissions,
model calls inside the sheet, plugins and arbitrary HTML, replication
across hosts, and Version-model features.

## Current state

Nothing of the `sheet` type is implemented. No type registration, tool,
route, CLI command, browser client or guide topic exists.

The S0 prototype was a server flat outside this repository. It is not the
product and none of its code is in the host. Its reports on
[issue 233](https://github.com/gosuda/flats/issues/233) and the probes of the
technical design's section 2 are the evidence for the decisions here. S0
stopped after its third step. Its fourth step, find and export, was not
built, and section 2 says which of its questions the design stage answered
instead.

## Open questions

The design made the choices of questions 3 to 17 inside what the plan left
to it. The requester decided them on 2026-11-14. Questions 3 to 13 and 15
to 17 stand as written, and questions 14 and 16 changed as written below.
The questions stay listed so that the reasons stay with the rules.
Questions 1 and 2 are still open.

1. **Limit numbers.** The limits of rule 13 that are not the grid size or
   the cell total, the work limit of rule 21 and the 120 seconds of the
   large recalculation criterion are planned values, fixed after
   measurement with the reference workbooks.
2. **Later phases.** Whether anonymous guest editing, as in
   [board-public-edit.md](board-public-edit.md), and an upload in parts
   for larger files become phases. Neither changes S1.
3. **Error values and exports.** `#CIRC!` and `#LIMIT!` exist beside the
   Excel error values (rule 16), and an export writes them as `#N/A`
   (rule 27).
4. **Numbers and formats.** A whole number of more than 15 digits stays text
   (rule 6). Rule 7 lists which formats the grid shows and which it only
   keeps.
5. **CSV.** The details of rule 28: a field is never a formula, output
   has a byte order mark and CRLF line ends, and a leading `=`, `+`,
   `-`, `@`, tab or carriage return gets an apostrophe.
6. **No rows before an import is complete.** People see the earlier
   workbook until the import replaces it (rule 30). Showing rows while it
   runs is not in S1.
7. **Find.** The find target of 1 second on large workbook B, checked for
   one rare and one common text, and no full-text index in S1
   (technical design section 20).
8. **Public viewers.** What a Public route receives and does not receive
   (rule 77), and the hourly step for time functions (rule 23).
9. **Agent addresses.** An agent write that names cells by address is
   refused when rows or columns of that worksheet were inserted, deleted or
   moved since its read, also far from the cells it names (rule 56).
10. **Overwritten notice.** The five-minute limit of rule 50, and that a
    restored item takes edits that arrive after the restore (rule 52).
11. **Counts.** 111 functions in rule 15, against "about 100" in the
    requester's decision. The plan named 28 functions, not the 31 that the
    question to the requester said. The per-action limit of 100,000 cells
    and the work limit of rule 21.
12. **Moving a range end.** A range follows its end rows (rule 11), and a
    row inserted where a dead end row of a range lies stays outside a range
    that ends there and inside one that starts there. The design has not
    checked in Excel that Excel does the same, and the conformance files of
    the requester can settle it.
13. **Names and volatile functions.** A formula keeps the name it was bound
    to, so adding a name of one worksheet with the word of a workbook name
    does not rebind the formulas that use the word, while Excel does after a
    round trip. Rule 10 calculates only names that
    point at one cell, one range or a constant, and keeps every other name
    as text. Rule 15 leaves out `RAND` and `RANDBETWEEN`, because their
    results change without an edit and without a seed.
14. **Long chain and large sort.** The 600 seconds of the long chain
    criterion. The requester asked on 2026-11-14 that a sort may pass the
    cell limit of one action. A sort of 100,001 to 2,000,000 cells is a large
    sort, a job whose apply pauses every write to the sheet for seconds
    (rule 41). A sort of more cells is refused, so a sort of large workbook
    B (about 20,000,000 cells) is still not possible. A larger limit needs an
    apply that does not pause writes, which S1 does not have.
15. **Pending while marking.** After an edit of a cell with many
    dependents, formulas that the host cannot recalculate at once show as
    pending, and Public viewers see every formula as pending, while the host
    marks. This lasts seconds for 1,000,000 dependents. Under a stream of
    such edits an episode of pending formulas lasts 60 seconds plus the
    marking that has started. The host then refuses edits of every editor
    until its marking work is done, and leaves a short gap in which
    exports and background calculation advance. The episodes recur while the
    stream lasts. A single edit whose marking takes longer than 60 seconds
    also causes the refusal. The alternative is to confirm the edit only
    after the marking, which fails the 200 ms criterion.
16. **Whole columns and find.** The requester asked on 2026-11-14 that a
    whole-column reference cover the whole grid, as in Excel. `ROWS(A:A)` is
    1,048,576, `COLUMNS(1:1)` is 16,384, and a row that does not exist yet
    reads as empty. A function that reads a whole column directly costs only
    the rows that exist. An array expression over a whole column, such as
    `SUMPRODUCT(--(A:A=""))`, holds all 1,048,576 positions and costs their
    time, as in Excel. A lazy array that skips the empty positions would
    remove that cost and is a new mechanism. Formulas that give `#LIMIT!`
    from such an array, or from an array of more than 4,194,304 values, are
    left out of the conformance criterion. Find does not look in numbers.
    Formulas that rely on implicit intersection give `#VALUE!`, and the
    conformance criterion leaves them out.
17. **Many ranges in one column.** An edit that leaves a slice with more
    than 2,000 ranges to read, counting ranges that do not overlap the edit,
    makes the host mark every range formula of the worksheet. An example is
    an edit in a column that has 1,000,000 cumulative sums. That takes about
    4 seconds for each 1,000,000 formulas, and repeated edits bring the
    refusal of question 15. A two-dimensional index removes the cost and is
    more work. The design leaves it out of S1 unless the requester wants it.
