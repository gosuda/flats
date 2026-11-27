# Sheet export fixture

One tab and the exact bytes of its CSV export (spec rule 60, technical
design section 22.2, cases E-01 to E-132; E-86, the byte order mark, is a property of the
file and has no cell).

| File | Holds |
|---|---|
| `cells.json` | The tab. `rows` are the rows of a `write` operation from `A1`, with the JSON typing of rule 43. `formats` are number formats by cell, as a read returns them. `widths` are column widths. `date` is the calculation date. |
| `expected.csv` | The export of the used range, `A1` to the last filled row and column: UTF-8, fields separated by commas, each row ended by CR LF. It holds the bytes after the byte order mark of E-86. |

Column A names the case of each row. E-87 to E-110 are the full-width signs
`＝`, `＋`, `－` and `＠` with 0, 1 and 3 leading spaces, typed (odd) and calculated (even)
like E-01 to E-42. E-111 to E-120 are dates and times under their formats, and
E-121 to E-132 the numbers and the dates that rule 60 writes in full, a number
to 15 significant digits: E-129 (`=LOG10(1000)`) and E-132 (`=-0`) are new, and
E-130 and E-131 are the dates and times of the former E-129 and E-130. E-126 holds
two cells, a percent in B and a currency amount in C, for the one case of
technical design section 22.2. Odd cases of E-01 to E-58 are content
as typed and even ones the same value as a formula calculates it. The row
between E-83 and E-84 is blank. The two rows named `errors` show the
errors a formula can give that E-78 does not hold. E-85 is the number in
column D, which is 40 pixels wide: the grid shows `####` there and the
export shows the full text.

The expected bytes were not written by the Go engine. They were computed
with `check_reference.py`, the independent checker of the reference table
in `../reference`: its reader of formulas, its number formats and its CSV
field rule. `TestExportFixture` in the formula package checks the field of
every cell against them. The engine's `TestExportBytes`, the browser
download, the CLI and `get_sheet` with `format: "csv"` must give the same
bytes.
