## Sheets

A sheet (`type: "sheet"`) is a spreadsheet: tabs of cells that hold numbers,
text, dates and formulas. People edit it in the browser at the same time as
you, and every formula is recalculated with each edit. You read and write it
as structured data, by tab name and cell address.

Choose a sheet when the numbers must calculate: budgets, estimates, plans
with totals, and any table where people change inputs and read results.
Choose a notebook database (`topic.notebook`) for typed records that people
sort, filter and view as a board, such as a task tracker. Choose a table in
a document (`topic.docs`) for a fixed table inside prose. Choose a page
(`topic.page`) to present finished figures as charts.

A sheet runs at once and has no Draft, versions, previews, publish or
approval for its content. A new sheet is Private. Making it Public, Private
again, or deleting it needs approval (`topic.approvals`). A Public sheet is
read-only for its viewers and shows every change to everyone at once, so
tell the user before you write to one. Version tools refuse a sheet
(`refusal.invalid`); sheet tools refuse other types (`refusal.not_sheet`).

### The model

- A sheet holds 1 to 50 tabs in a fixed order. You name a tab by its name.
  A new sheet has one empty tab named `Sheet1`. A name has 1 to 31
  characters, is unique apart from capitals, and holds none of
  `[ ] : * ? / \` and no control character. It does not start or end with
  a space or an apostrophe. It is not `History`, in any capitals.
- A cell is named by its address: column letters `A` to `ZZ` and a row
  number from 1, as in `B7`. A tab has at most 100,000 rows and 702
  columns, and starts with 1,000 rows and 26 columns. A write past the last
  row or column grows the tab.
- A range is `B3:D9`, whole columns `B:D` or whole rows `2:5`.
- The JSON type of a value says what a cell holds, in reads and in writes:

  | JSON | Cell |
  |---|---|
  | number | a number |
  | `true`, `false` | a boolean |
  | `null` | a blank cell (a write clears the cell) |
  | `{"date": "2026-03-15"}` | a date |
  | `{"time": "14:30:00"}` | a time |
  | `{"datetime": "2026-03-15T14:30:00"}` | a date with time |
  | `{"formula": "=B2/$B$9", "value": 0.4}` | a formula with its value, or with `error` in place of `value` |
  | string | text, with two exceptions below |

- A string of `=` and at least one more character is a formula. A string
  that starts with an apostrophe is the text after the apostrophe. A read
  returns text that starts with `=` or with an apostrophe with one more
  apostrophe in front, as in `'=not a formula`.
- A string is never turned into a number or a date. Write `1200`, not
  `"1200"`, and a date as its object.
- Every value a read returns can be written back unchanged. Of a formula
  object only `formula` is written. `value` (a number, a string, a boolean
  or `null`) and `error` (a string) are accepted as a read returns them and
  ignored. Another type there, such as an object or a list, is `invalid`.

### Create and read

1. `create_flat {slug, name?, type: "sheet"}` creates a sheet with the tab
   `Sheet1`, or `update_sheet` on a new slug creates it and applies your
   operations. `create_flat` returns the new flat, and the creating
   `update_sheet` returns it under `flat`.
   Give the user `private_url`: it opens the sheet.
2. Read before you write. `get_sheet {slug}` returns the tab list: each
   tab's `id`, `name`, `rows`, `cols`, `used` (the smallest range that holds
   every filled cell) and frozen rows and columns. It also returns `state`
   and the calculation `date`.
3. `get_sheet {slug, tab, range?}` returns the cells of the range, row by
   row, in `rows`. Without `range` it reads the used range. Blank rows and
   columns at the end are left out, and `range` in the result names what was
   returned.
4. `include` adds parts: `"shown"` is the text each cell shows under its
   number format, `"formats"` the formats grouped into ranges, and
   `"widths"` the column widths.
5. `format: "csv"` returns `csv`, the cell values as CSV text, in place of
   `rows`. A number has up to 15 significant digits whatever its number
   format. A date, a time or a date with time is in ISO 8601 form, such as
   `2026-03-15`, `14:30:00` or `2026-03-15 14:30:00`. A boolean is `TRUE`
   or `FALSE`, and an error is its text, such as `#N/A`. A text field whose
   first character, after any leading spaces, is `=`, `+`, `-`, `@`, a tab,
   a carriage return, a line feed, or the full-width `＝`, `＋`, `－` or `＠`
   is written with a leading apostrophe, so that a spreadsheet program does
   not read it as a formula.
6. One page holds at most `limit` cells (at most 10,000) and 4 MiB. When
   the result has `next`, call `get_sheet {slug, cursor: next.cursor}` with
   nothing else for the next page. `next.start` names the cell where it
   begins. When a single row is larger than a page, the page ends inside the
   row and the first row of the next page continues it. Joining the `csv`
   strings of all pages in order gives the whole text.
7. Every page of one read comes from the same state, even while people
   edit. A cursor lasts 60 seconds after its last page. After that it is
   refused with `conflict` (reason `read_expired`), and you read again from
   the start.

A read is refused as `unavailable` with `retry: true` and the reason
`calculating` while the sheet recalculates without an edit: right after a
host restart, and when the date has changed since the sheet was last
calculated. A large sheet can take minutes. The refusal comes within a
second, so repeat the read after a moment, at growing intervals. A cursor
of a read that began before stays valid for its 60 seconds. When the sheet
could not store a new date, a fresh read is refused with the reason
`commit` and the sheet keeps its previous date; repeat it, and tell the
user if it keeps failing.

### Write

`update_sheet {slug, state, ops, op_id?, message?, guard?}` applies every
operation in order, or none, and records one revision.

```json
{"slug": "offsite", "state": "s_k3v9.812", "op_id": "budget-1",
 "message": "Offsite budget",
 "ops": [
  {"op": "rename_tab", "tab": "Sheet1", "name": "Budget"},
  {"op": "write", "tab": "Budget", "at": "A1", "rows": [
    ["Item", "Amount", "Share"],
    ["Venue", 1200, "=B2/$B$9"],
    ["Food", 900, "=B3/$B$9"]]},
  {"op": "write", "tab": "Budget", "at": "A9", "rows": [["Total", "=SUM(B2:B8)"]]},
  {"op": "format", "tab": "Budget", "ranges": ["B2:B9"],
   "format": {"number": {"kind": "currency", "symbol": "$", "decimals": 2}}},
  {"op": "format", "tab": "Budget", "ranges": ["A1:C1"], "format": {"bold": true}}]}
```

| `op` | Fields |
|---|---|
| `write` | `tab`, `at` (top-left address), `rows` (rows of cell values) |
| `clear` | `tab`, `ranges`, optional `formats: true` to clear formats too |
| `format` | `tab`, `ranges`, `format` |
| `widths` | `tab`, `cols` (such as `"B:D"`), `width` (40 to 1,000 pixels) |
| `insert_rows`, `delete_rows` | `tab`, `at` (row number), `count` |
| `insert_cols`, `delete_cols` | `tab`, `at` (column letters), `count` |
| `sort` | `tab`, `rows` (such as `"2:200"`), `by` (column letters), optional `desc` |
| `add_tab` | `name`, optional `at` (position from 1) |
| `rename_tab` | `tab`, `name` |
| `move_tab` | `tab`, `to` (position from 1) |
| `delete_tab` | `tab` |
| `tab_settings` | `tab`, `frozen_rows` (0 to 20), `frozen_cols` (0 to 10) |
| `import_csv` | `tab`, `at`, `csv` |

- Operations apply in order, each on what the ones before it left. An
  address after an insert in the same call counts from that insert, and a
  tab renamed earlier in the call is named by its new name.
- `format` sets the properties it names and leaves the others: `bold`,
  `italic`, `underline`, `strikethrough`, `wrap`, `text_color` and `fill`
  (a palette color such as `"red-600"`), `align` (`"general"`, `"left"`,
  `"center"`, `"right"`) and `number`. `null` resets a property.
- `number` is one of the number format objects below. A format changes
  what a cell shows, never what it holds.
- `import_csv` is the one operation that reads text as typed: a field such
  as `1,200` or `2026-03-15` becomes a number or a date, as when a person
  types it. A field that is a whole number of more than 15 digits stays
  text, so that no digit is lost. A JSON number in `write` stays a number,
  whatever its digits.
- One call writes at most 50,000 cells and 8 MiB. The `slug` field of the
  tool does not count, so a call has the same bound through the API and
  the CLI. Cells the call does not name stay as they are.
- `flats sheet apply` sends the values of the file byte for byte, but the
  bound is that of the request it sends. The `op_id` it generates for a
  file without one and the values of `--state`, `--op-id` and `--message`
  count toward it, so it refuses a file of exactly 8 MiB without an
  `op_id`, which the API applies. Put the `op_id` in such a file.
- A sheet keeps at least one tab, and a tab at least one row and one
  column. Deleting the last one is `invalid`.
- Unknown operations and unknown fields are `invalid`, and the error names
  the operation or the field.
- Write every field name exactly as shown, once in its object: `Ops` is an
  unknown field, and a field that appears twice is `invalid`, in the call,
  an operation, a `format`, a number format and a cell object alike.
- `count`, `width`, `to`, `frozen_rows`, `frozen_cols`, `decimals` and a
  row number or a position in `at` are whole numbers written in digits:
  `2`, not `2.0`, `2e0` or `"2"`, and `0`, not `-0`. One has at most 18
  digits. A value in another form is `invalid`, also when its value is
  whole. `reason` names the field of the operation (`format` for
  `decimals`), and `detail` names the place and how to write the value,
  for example `count takes a whole number written in plain digits, such
  as 2, not 2.0 or 2e0`.
- A number in a cell of `rows` is any JSON number a double holds: `1.0`
  and `1e3` are numbers there, and `1e999` is `invalid`.
- Only a cell of `rows`, the `value` of a formula object and a property of
  a `format` may be `null`.
- The call is UTF-8. Bytes that are not UTF-8 are `invalid` with reason
  `json`, wherever they stand. A `\u` escape that names half of a
  surrogate pair is read as U+FFFD, the replacement character.

A number format is one of these objects. Each kind takes exactly the fields
shown for it. Leave none out and add no other.

| `number` | 1234.5 shows as |
|---|---|
| `{"kind": "general"}` | `1234.5` |
| `{"kind": "number", "decimals": 2, "thousands": true}` | `1,234.50` |
| `{"kind": "percent", "decimals": 2}` | `123450.00%` |
| `{"kind": "currency", "symbol": "$", "decimals": 2}` | `$1,234.50` |
| `{"kind": "date", "pattern": "yyyy-mm-dd"}` | `1903-05-18` |
| `{"kind": "time", "pattern": "hh:mm"}` | `12:00` |
| `{"kind": "datetime", "date": "yyyy-mm-dd", "time": "hh:mm"}` | `1903-05-18 12:00` |
| `{"kind": "text"}` | `1234.5` |

- `decimals` is a whole number from 0 to 10.
- `thousands` is `true` or `false` and says whether a number separates
  thousands with commas. A currency always does.
- `symbol` has 1 to 3 characters, each a letter or a currency sign, such as
  `"$"`, `"€"` or `"USD"`.
- The `pattern` of a date and the `date` of a datetime are `"yyyy-mm-dd"`,
  `"m/d/yyyy"` or `"mmm d, yyyy"`.
- The `pattern` of a time and the `time` of a datetime are `"hh:mm"`,
  `"hh:mm:ss"` or `"h:mm AM/PM"`.
- `general` is what a cell without a number format has. `text` shows a
  number as it was written, left-aligned.

The result:

```json
{"state": "s_k3v9.813", "revision": 14, "cells_written": 14,
 "formulas_written": 3, "formula_errors": 1,
 "errors": [{"at": "Budget!C3", "error": "#DIV/0!"}]}
```

`errors` lists the first 100 formulas of this call whose value is an error.
The result returns no other values, so read the cells when you need them. A
call that created the sheet adds `created: true` and `flat`, the new flat
as `get_flat` returns it. `revision` is `null` with
`revision_pending: true` when the write is applied but its revision is not
recorded yet. Do not repeat the write then.

Give `op_id` (1 to 128 characters of letters, digits and `. _ : -`) so that
a resend is safe. An empty `op_id` is the same as none. When a call times out, send the same call again with the
same `op_id`. The sheet then returns the stored result with `replay: true`
and applies nothing. The same `op_id` with another request is `conflict`
(reason `op_id_reuse`). Two calls are one request when they ask the same of
the sheet: the order of fields, white space, a formula as a string or as its
object, and an optional field written at the value it has when it is left
out, such as `"desc": false`, do not make another request. Neither does
the interface. A call sent again through the management API or
`flats sheet apply` gets the stored result too.

### State and guards

Every read returns `state`. Every write to an existing sheet names the
`state` it is based on, and the result returns the new one. Only two calls
need no `state`: the call that creates the sheet, and a call that only adds
tabs. An empty `state` is the same as none.

The host refuses the whole call with `conflict` when your addresses or tab
names may no longer mean what you read:

| Reason | What happened since your state |
|---|---|
| `state_required` | The call named no `state`. |
| `state_unknown`, `state_too_old` | The state is not one of this sheet, or the host no longer keeps the changes since it. |
| `tab_changed` | A tab the call names was renamed, deleted or replaced, or the sheet has no tab of that name. |
| `structure_changed` | Rows or columns were inserted, deleted or sorted in a tab where the call names an address. |
| `rolled_back` | The sheet was rolled back. |
| `guard` | A guard of the call failed. |

After any of these, read the sheet again and build the call against what
you read. Other people's changes to cell content, formats, widths and
settings do not refuse your call. They are merged cell by cell, and your
write wins on the cells it names.

Add a guard when your write depends on values you read:

- `"guard": {"unchanged": true}` refuses the call if the sheet left the
  state at all.
- `"guard": {"ranges": ["Budget!B2:B8"]}` refuses it if the content of a
  cell of those ranges changed. A guard range names its tab. It compares
  what the cells hold, not what their formulas calculate.

### Formulas

A formula starts with `=` and holds numbers, `"text"`, `TRUE` and `FALSE`,
references, the operators `+ - * / ^ &`, the comparisons
`= <> < <= > >=`, a trailing `%` and brackets.

- References are a cell `B3`, a range `B3:D9`, whole columns `B:D` and
  whole rows `2:5`. Another tab is `Costs!B3`, or `'Q1 costs'!B3:D9` when
  the name has spaces or other signs.
- `$` keeps a column or a row fixed when a formula is copied, filled or
  moved by a sort: `$B$9`.
- Inserting and deleting rows and columns rewrites the references of every
  formula, and renaming a tab renames it in every formula.

The functions, by group. Any other name gives `#NAME?`.

| Group | Functions |
|---|---|
| Math | `ABS`, `CEILING`, `EXP`, `FLOOR`, `INT`, `LN`, `LOG`, `LOG10`, `MOD`, `PI`, `POWER`, `PRODUCT`, `QUOTIENT`, `ROUND`, `ROUNDDOWN`, `ROUNDUP`, `SIGN`, `SQRT`, `SUM`, `SUMPRODUCT`, `TRUNC` |
| Statistics | `AVERAGE`, `COUNT`, `COUNTA`, `COUNTBLANK`, `LARGE`, `MAX`, `MEDIAN`, `MIN`, `RANK`, `SMALL`, `STDEV`, `STDEVP`, `VAR`, `VARP` |
| Conditional totals | `AVERAGEIF`, `AVERAGEIFS`, `COUNTIF`, `COUNTIFS`, `MAXIFS`, `MINIFS`, `SUMIF`, `SUMIFS` |
| Logic | `AND`, `FALSE`, `IF`, `IFERROR`, `IFNA`, `IFS`, `ISBLANK`, `ISERROR`, `ISLOGICAL`, `ISNA`, `ISNUMBER`, `ISTEXT`, `NA`, `NOT`, `OR`, `SWITCH`, `TRUE`, `XOR` |
| Text | `CONCAT`, `CONCATENATE`, `EXACT`, `FIND`, `LEFT`, `LEN`, `LOWER`, `MID`, `PROPER`, `REPLACE`, `REPT`, `RIGHT`, `SEARCH`, `SUBSTITUTE`, `TEXT`, `TEXTJOIN`, `TRIM`, `UPPER`, `VALUE` |
| Lookup | `CHOOSE`, `COLUMN`, `COLUMNS`, `HLOOKUP`, `INDEX`, `MATCH`, `ROW`, `ROWS`, `VLOOKUP`, `XLOOKUP` |
| Date and time | `DATE`, `DATEDIF`, `DAY`, `DAYS`, `EDATE`, `EOMONTH`, `HOUR`, `MINUTE`, `MONTH`, `SECOND`, `TIME`, `TODAY`, `WEEKDAY`, `YEAR` |

A formula's value is one number, text, boolean or error:

| Error | Means |
|---|---|
| `#ERROR!` | The formula cannot be read: a bracket is missing, or an operator has nothing to work on. |
| `#NAME?` | It calls a function that does not exist. |
| `#REF!` | It reads a cell, row, column or tab that was deleted or that lies outside the grid. |
| `#VALUE!` | A value has the wrong kind, such as text where a number is needed. Also: `FIND` or `SEARCH` found nothing, a formula built a text longer than 32,767 characters, or a wildcard pattern is too long (see Wildcards below). |
| `#DIV/0!` | It divides by zero. |
| `#NUM!` | A number is out of range for the function. |
| `#N/A` | A lookup found nothing. |
| `#CYCLE!` | The formula reads its own cell, directly or through other formulas. |
| `#LIMIT!` | The sheet's formulas calculate more text than a sheet may hold. |

A sheet has no array formulas. A formula never fills other cells, and a
range where one value is needed is `#VALUE!`. A sheet has no named ranges,
no random numbers and no current time. `TODAY` gives the sheet's calculation
date, which is the `date` of the read.

Wildcards: in a pattern `*` stands for any run of characters, `?` for one
character, and `~*`, `~?` and `~~` for that character itself. Capitals do
not count.

- A pattern is read in the criteria of every conditional total, when the
  criteria start with `=`, `<>` or no sign; in the key of `MATCH` with type
  0 and of `VLOOKUP` and `HLOOKUP` with exact match, as in
  `=VLOOKUP("a*",A:B,2,FALSE)`; and in the text `SEARCH` looks for.
- A pattern with a `*` or a `?` that no `~` precedes has at most 255
  characters, counted as written (a `~` counts) without the sign of
  criteria. A longer one is `#VALUE!`, whatever it is matched against.
  Shorten it, or write `~` before each `*` and `?` that stands for itself.
- The limit does not apply to text without such a wildcard, to criteria
  after `<`, `<=`, `>` or `>=`, or to the key of `XLOOKUP` and of a sorted
  match, which read no wildcards.

A write over a limit is refused as `sheet_capacity` and names the `limit`:

| `limit` | Bound |
|---|---|
| `tabs` | 50 tabs in a sheet |
| `rows`, `columns` | 100,000 rows and 702 columns in a tab |
| `cells` | 500,000 cells with content or a format in a sheet |
| `formulas` | 100,000 formulas in a sheet |
| `text_length` | 32,767 characters in a text cell |
| `formula_length`, `formula_depth` | 8,192 characters in a formula, and 64 levels of brackets and functions |
| `formula_reads` | 1,000,000 cells that one formula reads, ranges included |
| `sheet_reads` | 20,000,000 cells that all formulas of a sheet read |
| `sheet_text` | 50,000,000 characters of typed text in a sheet |
| `call_cells`, `call_bytes` | 50,000 cells and 8 MiB in one `update_sheet` call |
| `recalculation_time` | 10 seconds for the guards and the operations of a call and the recalculations they start |
| `action_changes` | 288,000,000 bytes of what one call replaces and the host keeps until the call ends, to take it back: the cells it changes as they were before the call, values its own recalculations replaced, rows and tabs it moved. Only a call that changes the same large sheet many times over meets it. Split the call |

### Build a table

1. Write the header row first, then the data rows, in one `write` from the
   top-left cell.
2. Write a formula for everything that is derived: totals, shares,
   differences. Do not calculate a value yourself and write the number,
   because it will not follow when people change the inputs.
3. Use `$` for the cells every row reads, such as a total: `=B2/$B$9`.
4. Apply formats last: number formats on the value columns, bold on the
   header, `tab_settings` with `frozen_rows: 1` for a long table.
5. Check `formula_errors` in the result and correct each formula that
   `errors` lists.

### History

Every accepted `update_sheet` call is one revision, and people's edits are
grouped into revisions as they work.

- `list_revisions {slug}` lists them with `tabs`, the tabs each changed.
- `get_revision {slug, revision, tab, range?}` reads a revision as
  `get_sheet` reads the live sheet, with the same `format`, `include`,
  `limit` and `cursor`, and returns the result under `sheet`.
  `get_revision {slug, revision, format: "json"}` returns its tab list. The
  values are calculated with the date the revision closed with, so a
  revision always shows the same values. A cursor of a revision read needs
  `revision` again.
- `rollback_revision` restores the whole sheet to a revision as a new
  revision, and `pin_revision` keeps one from being deleted. Neither needs
  approval.
- To bring back one deleted tab rather than the whole sheet, tell the user
  to open History in the sheet, open a revision that still has the tab, and
  restore it.

### Refusals

A refused call changed nothing. Its error has `category`, `reason` and
`retry`, and where they apply `limit`, `max`, `value`, `name` and `detail`.

- `invalid`: the call is malformed (`refusal.invalid`). `reason` names the
  field or the part of the call, and `detail` says what is wrong, with the
  operation counted from 1 when one is at fault. Correct the call. Sent
  again unchanged, it is refused again.
- `not_sheet`: the flat is another type (`refusal.not_sheet`).
- `not_found`: the flat or the revision does not exist, or, with reason
  `tab`, a read names a tab the sheet does not have. `get_sheet {slug}`
  lists the tabs. A write that names such a tab is `conflict` with reason
  `tab_changed`.
- `sheet_capacity`: a limit of the sheet (`refusal.sheet_capacity`). The
  error names the `limit`, its `max` and the `value` the write would reach.
  Write less or split the call. With reason `disk_quota` the write would
  put the flat over its disk quota, or the flat is over it already and
  every write is refused, a deletion included. Tell the user.
- `conflict`: read again and resend (`refusal.conflict`). The reasons are
  those in State and guards, `read_expired` for a cursor and `op_id_reuse`.
- `unavailable` (`refusal.unavailable`): with `retry: true`, wait a moment
  and send the same call again with the same `op_id`. The sheet is
  calculating (`calculating`: after a host restart, a change of the date
  or a rollback, for minutes when the sheet is large; the call was not
  applied), busy with other writes or large reads
  (`writer_backpressure`, `outbox_backpressure`, `capture_budget`),
  measuring its storage (`measurement`), or it is being unloaded and the
  host loads it again for the next call (`stopped`). With `undo_scratch`
  the host could not read its scratch storage for undo during a write: send
  the call again, and if it fails more than a few times tell the user that
  the disk of the host's data directory is full or failing. With
  `ids_exhausted` the sheet ran out of ids. Ids for formats come back at
  the sheet's next maintenance, a few seconds later: send the call again
  after that. When the call keeps failing, or the reason is `load` (the
  stored sheet could not be read or calculated), `storage`, `commit`,
  `revision` or `sheet_unavailable` (the sheet is not running or the host
  could not open it; the error text says why), the sheet or its storage
  has a fault. Tell the user and read `get_logs`.
