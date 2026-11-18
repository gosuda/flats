# Sheet export fixture

One tab and the exact bytes of its CSV export (spec rule 60, technical
design section 22.2, cases E-01 to E-86).

| File | Holds |
|---|---|
| `cells.json` | The tab. `rows` are the rows of a `write` operation from `A1`, with the JSON typing of rule 43. `formats` are number formats by cell, as a read returns them. `widths` are column widths. `date` is the calculation date. |
| `expected.csv` | The export of the used range, `A1` to the last filled row and column: UTF-8, fields separated by commas, each row ended by CR LF. It holds the bytes after the byte order mark of E-86. |

Column A names the case of each row. Odd cases of E-01 to E-58 are content
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
