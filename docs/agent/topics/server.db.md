## JavaScript env.DB

Per-flat local SQLite. `env.DB.query(sql, ...params)` and
`env.DB.exec(sql, ...params)` are **synchronous** and throw on SQL, parameter,
storage or runtime-limit errors. SQL is string-coerced. Pass positional `?`
parameters as separate arguments or one array, e.g. `query("SELECT ? AS x", [7])`.
Do not interpolate untrusted input. No prepared-statement object or ORM API.

`query` returns an array of objects keyed by column names; no rows returns
`[]`. Alias columns uniquely. SQL NULL becomes null; numbers become JS numbers
(large integers can lose precision), text becomes strings, BLOB results become
text through JSON/UTF-8 rather than typed arrays. Use SQL/application encoding
for binary. Text in a column declared DATE, DATETIME or TIMESTAMP that parses
as a time comes back reformatted as RFC 3339 (`2024-01-02 03:04:05` becomes
`2024-01-02T03:04:05Z`); declare the column TEXT to read stored text
unchanged. `exec` returns `{changes, last_insert_id}` numbers from SQLite
RowsAffected/LastInsertId (not meaningful new insert IDs for every statement).

Parameters: null, string, number; booleans become 1/0; objects/arrays become
JSON text (a single array argument is treated as the parameter list, so nest
an array to store one). The JS-to-host JSON transport converts undefined array
entries to null and rejects BigInt/cyclic objects. The Go host then reserializes
compound values with `encoding/json`: object keys are sorted and `<`, `>` and
`&` are escaped as `\u003c`, `\u003e` and `\u0026` (U+2028/U+2029 are also
escaped). Stored text need not equal the original `JSON.stringify` text;
parse it as JSON instead of relying on textual identity. Parameters are not BLOBs.
`CREATE TABLE IF NOT EXISTS` is useful for idempotent setup.

One SQLite connection per JS VM keeps concurrent request transactions apart.
Use `exec("BEGIN")`, parameterized statements, `exec("COMMIT")`, and rollback
on errors within one handler invocation. Any transaction left open at the end
of a handler/callback is rolled back; never span requests with a transaction.
WAL, foreign keys and 5-second busy timeout are enabled. Query result cap is
**16 MiB** while accumulating serialized rows (the closing bracket is added
afterward; add LIMIT); SQL value/row length cap **32 MiB**;
SQLite allocation ceiling **256 MiB**. Runtime deadline also bounds queries.
ATTACH and VACUUM INTO and directory-changing pragmas are blocked. There is
no hard DB disk-size quota: DB growth is reported against host disk usage;
operator disk quota can reject uploads, but does not hard-cap runtime DB growth.
DB and FILES do not participate in a shared transaction.
