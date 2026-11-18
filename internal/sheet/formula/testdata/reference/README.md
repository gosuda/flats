# Sheet function reference table

This directory is the contract of the sheet's formula engine (spec rule 12,
technical design section 9.6). It holds expected results, written before any
engine exists. The engine is written against these files. A disagreement
found later is settled here first, with a recorded reason, and never by
changing an expected value to match the engine.

## Files

| File | Rows | Holds |
|---|---|---|
| `math.jsonl` | 371 | the 21 math functions |
| `statistics.jsonl` | 212 | the 14 statistics functions |
| `conditional.jsonl` | 198 | the 8 conditional totals |
| `logic.jsonl` | 247 | the 18 logic functions |
| `text.jsonl` | 401 | the 19 text functions |
| `lookup.jsonl` | 241 | the 10 lookup functions |
| `date.jsonl` | 293 | the 14 date and time functions |
| `operators.jsonl` | 489 | grammar, comparisons, coercion, errors, references, number formats and shown text, CSV fields, the 32,767-character rule, other tabs |
| `typed.jsonl` | 69 | typed entry (rule 5) |
| `check_reference.py` | | the independent checker |

2,452 rows in the eight formula files, 2,521 with `typed.jsonl`:
1,031 normal, 297 boundary, 492 error and
701 coercion cases. Every function of rule 12 has at least 8
rows, among them a normal case, an error case and a call with a wrong
number of arguments.

Rows of `operators.jsonl` are grouped by id: operators and precedence
(001 to 059), comparisons (060 to 086), coercion (087 to 139), errors and
cycles (140 to 206), references (207 to 240), number formats and shown
text (241 to 348), CSV fields (349 to 414), the length limit on calculated
text (415 to 427), and from 428 on other tabs, formulas in other cells and
the grammar, error order, blank value, case folding and formula object
cases added later.

Each file is UTF-8 without a byte order mark, one JSON object per line,
lines end with a line feed. `typed.jsonl` has its own fields, see below. Ids are stable: new rows are added at the end
of a function's rows with the next number.

## Row format

```json
{"id":"sumif-004","fn":"SUMIF","class":"normal","date":"2026-11-13",
 "cells":{"A1":"apple","A2":"Apple pie","A3":7,"B1":10,"B2":20,"B3":30},
 "formula":"=SUMIF(A1:A3,\"a*\",B1:B3)","format":null,
 "value":30,"shown":"30","csv":"30",
 "note":"the example of section 9.6"}
```

Keys appear in this order. `tabs`, `at` and `differs` are optional.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The value of `fn` in lower case, a hyphen and three digits. Unique in the table. |
| `fn` | string | A function of rule 12, or `operators` in `operators.jsonl`. |
| `class` | `normal`, `boundary`, `error` or `coercion` | What the case shows. A case about how a value is converted is `coercion` also when its value is an error, and a case at an edge is `boundary` also when its value is an error. |
| `date` | `yyyy-mm-dd` | The calculation date of the state. `TODAY` gives this date. |
| `cells` | object | Address to value in the tab `Sheet1`, with the JSON typing of `write` (rule 43): number, boolean, string, `{"date":"2026-03-15"}`, `{"time":"14:30:00"}`, `{"datetime":"2026-03-15T14:30:00"}`. A string of `=` and more is a formula, and a string that starts with an apostrophe is the text after it. A formula object as a read returns it, `{"formula":"=B3/0","error":"#DIV/0!"}`, is a formula too: only `formula` counts, `value` and `error` are ignored. |
| `tabs` | object, optional | Tab name to a `cells` object, for cases that read another tab. |
| `formula` | string | The formula under test. |
| `at` | address, optional | Where the formula is written in `Sheet1`. The default is `Z1`. |
| `format` | object or `null` | The number format of the formula's cell, see below. `null` is a cell without a number format. |
| `value` | number, string, boolean, `null` or `{"error":"#N/A"}` | The calculated value as `get_sheet` returns it in `value` or `error`. `null` is a blank value. |
| `shown` | string | What the cell shows in a column wide enough. |
| `csv` | string | The exact CSV field, with its apostrophe and quotation marks (rule 60). |
| `note` | string | What the case establishes. |
| `differs` | `excel`, `sheets` or `both`, optional | The program whose result differs from the row. |

Numbers compare equal when they agree after rounding to 15 significant
digits. Every other field compares exactly.

### Number format objects

| Number format | Object |
|---|---|
| General | `{"kind":"general"}` |
| Number | `{"kind":"number","decimals":2,"thousands":true}` |
| Percent | `{"kind":"percent","decimals":2}` |
| Currency | `{"kind":"currency","symbol":"$","decimals":2}` |
| Date | `{"kind":"date","pattern":"yyyy-mm-dd"}` |
| Time | `{"kind":"time","pattern":"hh:mm"}` |
| Date and time | `{"kind":"datetime","date":"yyyy-mm-dd","time":"hh:mm"}` |
| Text | `{"kind":"text"}` |

`thousands` is the thousands separator of Number, on or off. `pattern` is
one of the date forms `yyyy-mm-dd`, `m/d/yyyy`, `mmm d, yyyy` or one of the
time forms `hh:mm`, `hh:mm:ss`, `h:mm AM/PM`. A date and time format names
one of each in `date` and `time`. The technical design shows only the
currency object; the other field names are fixed here.

### Typed entry rows

A line of `typed.jsonl` has exactly these fields, in this order. It has no
`cells`, `formula`, `date`, `format` or `differs`: the entry goes into a
cell without a number format in an otherwise empty sheet.

```json
{"id":"typed-010","fn":"typed","class":"normal","typed":"50%",
 "sets":{"kind":"percent","decimals":0},
 "value":0.5,"shown":"50%","csv":"50%",
 "note":"a trailing % divides by 100 and sets the percent format"}
```

| Field | Type | Meaning |
|---|---|---|
| `id` | string | `typed`, a hyphen and three digits. |
| `fn` | string | Always `typed`. |
| `class` | `normal`, `boundary`, `error` or `coercion` | What the case shows. |
| `typed` | string | The text a person commits to the cell, read by rule 5. The same reading applies to one field of `import_csv`. |
| `sets` | object or `null` | The number format the entry sets on the cell, or `null` when it sets none. |
| `value` | as `get_sheet` returns the cell | A number, text, a boolean, a date or time object, or for a typed formula its calculated value or error. Text that starts with `=` or an apostrophe has one more apostrophe in front, as a read returns it. |
| `shown` | string | What the cell shows under the format in `sets`, or without a number format when `sets` is `null`. |
| `csv` | string | The exact CSV field. |
| `note` | string | What the case establishes. |

## Running the checker

```sh
python3 check_reference.py
```

It needs Python 3 and only the standard library, and no network. It can be
run from any directory. It exits 0 only when every row was recomputed and
agrees. It exits 1 on any mismatch, on any row it could not recompute, and
on any layout problem, and prints the rows per file, each row it could not
recompute with the reason, and each mismatch with the table's and its own
result.

For every row it recomputes `value`, `shown` and `csv` (and `sets` for
typed entry) with its own parser of the grammar of section 9.2 and its own
evaluator of the 104 functions. It compares as the contract does: numbers
after rounding to 15 significant digits, everything else exactly. It
checks the layout as well: key order, ids, classes, `differs` values, the
group of each function, at least 8 rows and a normal and an error case per
function, UTF-8, and line ends.

The checker is test tooling, not an engine. It is not used at run time and
the Go engine shares no code with it.

## How the expected values were established

1. Each row was written by hand by an author who does not write the Go
   engine: the formula, the cells and the expected result as a literal or a
   closed form. The source of each result is the documented behavior of the
   function in Excel and Google Sheets, then sections 9.2 to 9.5 of the
   technical design where they state the sheet's own behavior.
2. Where the two programs agree, the row follows them. Where they differ,
   the row follows section 9.4, and where section 9.4 is silent it follows
   Google Sheets or the choice listed under Chosen behaviors.
3. `check_reference.py` then recomputed every row with independent code:
   calendar arithmetic by a days-from-civil formula, `decimal` arithmetic
   for rounding to 15 significant digits and for number formats, and its
   own readers of typed entry, criteria, wildcards and CSV fields.
4. Each mismatch was settled by working out which side was wrong. The
   checker found four errors in the table, such as the 15th digit of the
   square root of 32/7 and a miscounted length after `TRIM`, and they were
   corrected in the table.
5. Mutation tests on copies of the final table: 548 rows with a changed
   value, shown text or CSV field were reported exactly and gave exit 1; a
   number changed in its 10th significant digit (3 to 3.0000000003) and one
   changed in its 14th gave exit 1; a row with a cell object the checker
   cannot read was reported as not checked and gave exit 1; a row with a
   valid formula object added to its cells was checked and gave exit 0.
6. Rows added after the engine existed state results that this README and
   the technical design already fixed and that the engine did not give:
   `FLOOR` with a significance of 0 for the number 0 (`floor-026` to
   `floor-028`, `ceiling-026`, `ceiling-027`), the leftmost error among
   the arguments of the conditional totals, and their ranges outside the
   tab (the two rows on the conditional totals under Chosen behaviors).
   The checker had the engine's reading of the first two, returning 0 for
   `FLOOR(0,0)` and reading every range before any criteria, and was
   corrected with the rows. It agreed with the rows on ranges outside the
   tab without a change.
7. The rows on an error value where a range is read were added later
   still, with the decision they record. Until then such an error value
   was read as one cell that holds an error, in the engine and in the
   checker, so `COUNTIF(#REF!,1)` was 0. `sumifs-022` was added under that
   reading with the value 0 and now has `#N/A`. `log-024` and `log-025`
   fix an order of checks that the engine and the checker already had.
8. The limit of 255 characters on a pattern that holds a wildcard was
   added with its rows. It bounds what a match costs: before it, a pattern
   could be as long as a text. No earlier row had a longer pattern.
9. The rows on `AND`, `OR` and `XOR` over ranges of text and blank cells
   (`and-018` to `and-030`, `or-016` to `or-023`, `xor-015` to `xor-021`)
   were added after the engine existed. Excel documents `#VALUE!` for
   arguments without a logical value. The engine and the checker both gave
   the function's starting value there, `TRUE` for `AND` and `FALSE` for
   `OR` and `XOR`, and both were corrected with the rows.
10. The rows on an error cell before and after the cell that decides `AND`
   or `OR` in a range (`and-031` to `and-037`, `or-024` to `or-027`), with
   the `XOR` rows beside them (`xor-022` to `xor-025`), were added after the
   engine existed. The engine stopped at the deciding cell. The checker
   read every cell of a range before it looked for the deciding value, so
   an error cell after that value was its result, and it was corrected with
   the rows.

The table and the checker have the same author. The checker catches slips
of arithmetic, counting, rounding and formatting. It does not catch a
shared misreading of the spec, which is why the next section exists.

## Chosen behaviors

These are the cases where neither the spec nor the technical design states
the result, or where the row departs from Excel or Google Sheets. Each is
fixed by the rows named. This list is the place to review them. Changing
one means changing its rows and the checker together.

| Behavior | Differs from | Rows |
|---|---|---|
| 0 to the power 0 is 1. | Excel (`#NUM!`) | `power-006`, `operators-113` |
| `CHOOSE` with an index outside its list, also 0 from a blank cell, is `#NUM!`. | Excel (`#VALUE!`) | `choose-007`, `choose-008`, `choose-015` |
| `INDEX` with a row or column past the range, or negative, is `#NUM!`. A row or column of 0 is `#VALUE!` (section 9.4). | Excel (`#REF!`, `#VALUE!`) | `index-006`, `index-007`, `index-010`, `index-008`, `index-009` |
| `INDEX` with one index takes it as the row on a range of one column and as the column on a range of one row. On a rectangle it is `#VALUE!`. One argument alone is `#ERROR!`. | both on the rectangle, Sheets on one argument | `index-003`, `index-027` to `index-031` |
| A call with a number of arguments the function does not take is `#ERROR!` for the whole formula. It is decided before evaluation, so `IFERROR` does not replace it. A condition without a value in `IFS`, and a range without criteria in `COUNTIFS` or `SUMIFS`, count as a wrong number. | Sheets (`#N/A`); Excel refuses the entry | the 113 rows whose note names a number of arguments the function does not take, at least one per function, and `operators-195`, `operators-194` |
| `CEILING` and `FLOOR` with a negative number and a negative significance follow the common programs: `CEILING(-2.5,-2)` is -4 and `FLOOR(-2.5,-2)` is -2. | neither | `ceiling-008`, `floor-008` |
| `CEILING` with a significance of 0 is 0. `FLOOR` with a significance of 0 is `#DIV/0!`. Both hold for every number, 0 included. A blank cell as significance counts as 0. | Sheets on `CEILING`, not confirmed | `ceiling-023` to `ceiling-027`, `floor-023` to `floor-028` |
| A formula whose result is a blank cell has a blank value (`null`, shown as nothing), also when the cell comes back through `IF`, `INDEX`, `VLOOKUP`, `HLOOKUP`, `XLOOKUP`, `CHOOSE`, `IFERROR` or a reference to another tab. | Excel (0) | `iferror-013`, `index-015`, `vlookup-020`, `xlookup-020`, `hlookup-018`, `choose-013`, `operators-129`, `operators-130`, `operators-237`, `operators-441` |
| A cell that holds a formula with a blank value counts as 0 and as empty text for a formula that reads it. `COUNTBLANK` counts it, since it counts cells whose value is blank or empty text, filled or not. `COUNTA` counts it too, and it is not blank for `ISBLANK`. | Excel on the first row | `operators-470` to `operators-473`, `isblank-009`, `counta-013`, `countblank-011`, `countblank-012` |
| A reference to a tab that does not exist is the error value `#REF!`, which `IFERROR` and `ISERROR` handle. | neither | `iferror-007`, `operators-234`, `operators-235` |
| `#CYCLE!` follows static dependencies: a whole row, a whole column or a range that holds the formula, an argument that is not taken, and a reference whose value is not read (`ROW(Z1)` in Z1) are all cycles. A cell on a cycle shows `#CYCLE!` even inside `IFERROR`. A cell off the cycle that reads a cycle cell gets an ordinary error value that `ISERROR` sees. | both on references that are not read | `operators-196` to `operators-205`, `operators-447`, `operators-453`, `operators-454`, `operators-468`, `operators-469`, `operators-484` |
| `AND` and `OR` stop at the first value that decides the result, so a later error is not evaluated (section 9.3). This holds inside a range too: a range is read row by row, left to right, and the first cell that decides ends the reading, so an error cell after it is not read, in that range or in a later argument. An error cell read before the deciding cell is the result, and of several the first one read. `XOR` reads every cell, so the first error cell of its arguments is its result. | both | `and-011`, `or-010`, `and-031` to `and-037`, `or-024` to `or-027`, `xor-022` to `xor-025` |
| `AND`, `OR` and `XOR` are `#VALUE!` when no argument holds a logical value: ranges and referenced cells of text and blank cells alone. A number or boolean in a range is a logical value, and so is any other argument that reads as a boolean. An error in an argument that is read comes first. | Sheets not confirmed | `and-018` to `and-030`, `or-016` to `or-023`, `xor-015` to `xor-021` |
| With errors on both sides of an operator, or in several arguments, the leftmost wins. The left operand is evaluated and converted before the right one. | not confirmed | `operators-465` to `operators-467`, `operators-163` to `operators-165` |
| Text in a position that takes a boolean is `#VALUE!` unless it reads `TRUE` or `FALSE`, capitals aside. Numeric text there is `#VALUE!`. In a range such text is passed over. | Sheets not confirmed | `if-018` to `if-020`, `ifs-014`, `ifs-015`, `not-010` to `not-012`, `and-015` to `and-017`, `or-014`, `or-015`, `xor-013`, `xor-014`, `vlookup-031` to `vlookup-033`, `hlookup-027` to `hlookup-029`, `textjoin-023` to `textjoin-025`, `if-008`, `and-009`, `not-007`, `or-008`, `xor-008`, `ifs-009` |
| Empty text is no number: `=""+1` and `VALUE("")` are `#VALUE!`. `VALUE` of a boolean is `#VALUE!`. | Sheets (1 and 0) | `operators-104`, `value-020`, `value-024` |
| `TEXT` returns text that is no number unchanged, reads numeric text and a boolean (as 1 or 0) as numbers, and coerces a pattern that is no text to text. The pattern is checked first, and a pattern outside rule 6 is `#VALUE!` for any value. | Excel on a boolean value and on an empty pattern | `text-035` to `text-042` |
| `TEXT` with a date or time pattern on a negative number gives the General text, as the number format does. | Excel (`#VALUE!`) | `text-043`, `text-044` |
| `TEXT` accepts a currency prefix before `0` forms and before `#,##0` forms. | neither in effect | `text-045`, `text-046`, `text-009`, `text-012` |
| Invalid numeric arguments of text functions (a negative count, a start under 1 or past the end, a place number under 1) are `#VALUE!`, as Excel documents. | Sheets may give `#NUM!`, not confirmed | `find-007`, `find-008`, `find-024`, `left-005`, `mid-006`, `mid-007`, `mid-020`, `replace-007`, `replace-008`, `replace-022`, `rept-005`, `right-005`, `search-009`, `search-010`, `search-024`, `substitute-008`, `substitute-026` |
| `FIND("","")` is 1. | neither | `find-015` |
| `SUMPRODUCT` treats a direct value as a range of one cell, where text and booleans count as 0. | not confirmed | `sumproduct-013` to `sumproduct-018` |
| A direct value where a function reads a range is a range of one cell. `ROWS` and `COLUMNS` of a value are 1. `ROW` and `COLUMN` need a reference and give `#VALUE!` for a value. | Excel refuses a value in the conditional totals, `COUNTBLANK` and `RANK` | `countblank-009`, `countblank-010`, `countif-034`, `countifs-015`, `sumif-018`, `sumifs-016`, `averageif-014`, `averageifs-013`, `maxifs-015`, `minifs-015`, `rank-024`, `large-019`, `small-018`, `index-032`, `match-041`, `vlookup-035`, `hlookup-031`, `xlookup-023`, `rows-010`, `columns-010`, `row-013`, `column-013` |
| The conditional totals read their arguments in the order they are written, and the leftmost one that is an error is the result: a range on a tab that does not exist, an error value where a range is read (next row), or criteria that are an error. Ranges of different sizes are `#VALUE!` only when no argument is an error. An error in a cell of a range is no such error: the cell matches no criteria, and as a number cell it is the result only when its criteria match. | not confirmed | `countifs-021` to `countifs-023`, `countifs-025`, `countifs-026`, `sumif-023`, `sumif-024`, `sumifs-019`, `sumifs-020`, `averageif-017`, `averageifs-015`, `maxifs-017`, `minifs-017`, `sumif-010`, `sumif-016` |
| An error value that stands where a function reads a range is the result, like a range on a tab that does not exist, and not a cell that matches nothing or counts for nothing: an error written in the formula, such as the `#REF!` that a deleted reference leaves, or any other direct value that is an error. This holds for the criteria ranges and the value range of the conditional totals, also when no cell matches, for the range of `MATCH`, `VLOOKUP`, `HLOOKUP`, `XLOOKUP` (both ranges), `INDEX`, `COUNTBLANK`, `RANK`, `LARGE`, `SMALL` and `SUMPRODUCT`, and for `ROWS`, `COLUMNS`, `ROW` and `COLUMN`. | Excel and Sheets not confirmed on a direct value that is no `#REF!` | `countif-037`, `countif-038`, `countifs-024`, `sumif-025`, `sumif-026`, `sumifs-021` to `sumifs-024`, `averageif-018`, `averageif-019`, `averageifs-016`, `averageifs-017`, `maxifs-018`, `maxifs-019`, `minifs-018`, `minifs-019`, `match-043`, `vlookup-036`, `hlookup-032`, `xlookup-024`, `xlookup-025`, `index-033`, `countblank-013`, `rows-011`, `columns-011`, `row-014`, `column-014` |
| For the conditional totals a cell under the rows or beside the columns of the tab is a blank cell like any other: it matches criteria that a blank cell matches, whatever the order of the pairs and also next to direct values. | both (their grids differ, and Excel refuses a direct value) | `countif-036`, `countifs-016` to `countifs-020`, `sumif-020` to `sumif-022`, `sumifs-017`, `sumifs-018`, `averageif-016`, `averageifs-014`, `maxifs-016`, `minifs-016` |
| Criteria taken from a blank cell match blank cells, empty text and the number 0. | Excel (the number 0 only) | `countif-033`, `countifs-014`, `sumif-017`, `sumifs-015`, `averageif-013`, `averageifs-012`, `maxifs-014`, `minifs-014` |
| A lookup key taken from a blank cell matches the number 0 and empty text, counts as 0 in an approximate match, and never matches a blank cell. Blank cells in a lookup range match no key. A boolean key matches only a boolean. | Excel on empty text | `match-026` to `match-029`, `vlookup-034`, `hlookup-030`, `xlookup-021`, `xlookup-022`, `match-030`, `match-021` |
| `SWITCH` compares as the `=` operator does: capitals are ignored, the text 1 is not the number 1, and a blank cell matches the cases 0 and empty text. | not confirmed | `switch-005`, `switch-009`, `switch-010`, `switch-013` |
| `SUMIF` and `AVERAGEIF` with a sum range of another size than the criteria range are `#VALUE!`, like `SUMIFS`. Only matching cells are read, so an error in a cell that does not match is passed over. | both on the size | `sumif-019`, `averageif-015`, `sumif-016` |
| A date after a comparison sign in criteria is read as a date. | neither | `countif-027` |
| `COUNT` passes over direct text that is no number and errors in a range without returning an error. | neither | `count-005`, `count-014`, `count-006` |
| `MATCH` type -1 works on descending data and gives the smallest value that is not under the key. Any type above 0 is type 1, any type under 0 is type -1. A range that is neither one row nor one column is `#N/A`. | neither | `match-031` to `match-040` |
| `XLOOKUP` has no wildcards. | neither | `xlookup-015` |
| `LOG` with base 1 is `#DIV/0!`. | Sheets not confirmed | `log-011`, `log-023` |
| `LOG` checks the number before the base: a number that is 0 or negative is `#NUM!` also with base 1. | not confirmed | `log-024`, `log-025` |
| Coercion by argument position follows one rule for every function: numeric text is its number, text that is no number is `#VALUE!`, `TRUE` is 1, a blank cell is 0, and where text is taken a number is its General text, a boolean its name and a blank cell empty text. Under the sheet's calendar this makes `DAY(TRUE)` 31 and `YEAR` of a blank cell 1899. | Excel on some date rows (`DAY(TRUE)` is 1 there) | the 273 rows whose note has the form "... as the first (second, third, fourth) argument ...", for example `mod-017`, `left-016`, `day-015`, `date-033` |
| Of those, the rows with `TRUE` or a blank cell as a date. | Excel | `date-032`, `date-033`, `date-036`, `date-037`, `date-040`, `date-041`, `datedif-028`, `datedif-029`, `datedif-032`, `datedif-033`, `datedif-035`, `datedif-036`, `day-015`, `day-016`, `month-014`, `month-015`, `year-015`, `year-016`, `days-014`, `days-015`, `days-018`, `days-019`, `edate-017`, `edate-018`, `edate-021`, `edate-022`, `eomonth-016`, `eomonth-017`, `eomonth-020`, `eomonth-021`, `weekday-021`, `weekday-022`, `weekday-025`, `weekday-026` |
| `DATE` truncates fractions. `TIME` adds hours, minutes and seconds before the range check, so negative minutes count back. | Sheets on `TIME`, not confirmed | `date-024`, `time-008` |
| `DATEDIF` with the unit `MD` counts from the start day in the month before the end. | neither on this case | `datedif-011` |
| Whole rows and columns cover the rows and columns the tab has: 1,000 and 26 in a new tab, more after a write past the edge. | both (their grids differ) | `rows-006`, `rows-009`, `columns-006`, `columns-009`, `operators-221` |
| A range written from its last cell to its first covers the same cells. A tab name in a formula ignores capitals. A row past 100,000 or a column past ZZ is `#REF!`. | neither | `operators-213`, `operators-228`, `operators-440`, `operators-236`, `operators-464` |
| Formulas may hold spaces between their parts, names and addresses in small letters, numbers with an exponent and numbers that start with a period. A bare name is `#NAME?`. | neither | `operators-045`, `operators-046`, `operators-457` to `operators-463` |
| A pattern that holds a wildcard, which is a `*` or a `?` without a `~` before it, has at most 255 characters: a longer one is `#VALUE!`. This holds for the text of criteria after the comparison sign, where the sign is `=`, `<>` or none, for the key of `MATCH` with type 0 and of `VLOOKUP` and `HLOOKUP` with an exact match, and for the find text of `SEARCH`. Text without a wildcard has no such limit, nor has the text after `<`, `<=`, `>` or `>=` in criteria, which is compared and not matched, nor the key of `XLOOKUP`, which has no wildcards. In the conditional totals a pattern that is too long is an error of its argument, in the order of the arguments. | Excel limits criteria to 255 characters with or without wildcards; Sheets not confirmed | `countif-039` to `countif-045`, `sumif-027`, `sumif-028`, `countifs-027`, `match-044` to `match-046`, `vlookup-037`, `vlookup-038`, `hlookup-033`, `xlookup-026`, `search-026` to `search-029` |
| Text is compared, and criteria, lookup keys and `SEARCH` are matched, after Unicode simple case folding (CaseFolding statuses C and S), one code point at a time: the two sigmas are equal, the long s equals s, the micro sign equals the Greek mu, the sharp s equals the capital sharp s and does not equal ss, and capital I with dot above does not equal i. See Unicode versions below. | not confirmed | `operators-474` to `operators-481`, `match-042`, `countif-035`, `search-025` |
| `LOWER`, `UPPER` and `PROPER` use the Unicode simple case mapping, one code point to one code point, so the length never changes: `UPPER` leaves the sharp s, `LOWER` gives the plain sigma at a word end and `i` for capital I with dot above. `PROPER` starts a word after any character that is no letter (`2nd` becomes `2Nd`) and gives its first letter the simple upper case, not the title case. | both on `UPPER` of the sharp s | `lower-013` to `lower-016`, `upper-013` to `upper-016`, `proper-013`, `proper-014`, `upper-004`, `lower-004`, `proper-004` |
| General at the edge: a number is rounded to 15 significant digits first and then tested for 16 digits. Scientific text has a sign, at least two exponent digits and at most 6 significant digits. | neither in effect | `operators-249`, `operators-246`, `operators-260`, `operators-253` |
| `hh:mm` cuts off the seconds: 23:59:59 shows `23:59`. A date result under the Text format shows its day number. | not confirmed | `operators-329`, `operators-348` |
| The `value` of a formula whose text starts with `=` or an apostrophe is written without the extra apostrophe a read adds to plain text cells. | does not apply | `operators-349`, `operators-392`, `operators-393`, `operators-400`, `operators-405` |
| Typed entry: a percent sets 0 decimals when none are typed and 2 otherwise. | neither | `typed-010` to `typed-013` |
| Typed entry: a currency sign sets 2 decimals. | Excel (0 for a whole amount) | `typed-014` to `typed-021` |
| Typed entry: commas set no format, `1e6` is a number, and `007` is the number 7. | both set a Number format for commas | `typed-005`, `typed-003`, `typed-009`, `value-005` |

### Unicode versions

The contract for text comparison is Unicode simple case folding, statuses
C and S of CaseFolding.txt, as `fold_table.go` of the formula package
holds it. That file is generated from `unicode.SimpleFold` of the Go
toolchain (`go test -run TestFoldTable -update`), and `TestFoldTable`
fails when the toolchain gives another fold for any code point, so the
folding is that of the toolchain's Unicode version. `LOWER`, `UPPER` and
`PROPER` use the simple case mapping of the toolchain (`unicode.ToLower`,
`unicode.ToUpper`).

The table holds only cases on which the checker and the Go toolchain
agree:

| Checked with | Unicode version |
|---|---|
| `check_reference.py` under Python 3.10.13 | 13.0.0 |
| Go 1.27.1, `unicode.Version` | 17.0.0 |

The checker prints the Unicode version of the Python it runs with. It
derives the folding from Python's own data: a code point whose full fold
is one code point folds to it, and code points that share a longer full
fold are equal to each other. That derivation also equates U+0390 with
U+1FD3, U+03B0 with U+1FE3 and U+FB05 with U+FB06, which Unicode gave
status S in version 15.1. Go 1.27.1 puts each of these pairs in one
`unicode.SimpleFold` orbit, so the rows `operators-487` to `operators-489`
fix them. Letters added after Unicode 13.0.0 have no row, because the
checker's Python does not know them; rows for them can be added when the
checker runs on a newer Python.

Not in the table: `#LIMIT!`, which needs 50,000,000 characters of
calculated text, the limits on formula length and nesting, which refuse a
write and give no value, and `####`, which the browser draws.
