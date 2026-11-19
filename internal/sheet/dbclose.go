package sheet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// closeDB closes the reading connections rdb, then the writing connection
// db, of sheet.db at path; either can be nil. When the last connection to
// a database in WAL mode closes, SQLite checkpoints the whole log into the
// database first, work in proportion to the log, which the writer bounds
// only by the disk quota (openLog). The readers close while the writer
// still holds its shared lock of the file, so none of them is the last
// connection, and the writer closes without that checkpoint when the log
// holds more than walTruncateBytes, the size the writer truncates a log
// above after its commits, and when there is no log. The close then costs
// no work in proportion to a longer log; the log stays, and the next open
// recovers every commit from it at the gate. A shorter log is
// checkpointed, as the writer does after a commit, and the file is left
// without one.
func closeDB(path string, db, rdb *sql.DB) error {
	info, err := os.Stat(path + "-wal")
	return closeConns(db, rdb, err != nil || info.Size() > walTruncateBytes)
}

// closeConns closes rdb, then db, either of which can be nil. With keepLog
// it first turns off the checkpoint on close of db, so that sheet.db and its
// -wal file keep their bytes whatever the length of the log: closeDB asks
// for that for a long log, and the close of a load that refused the sheet
// for any log (refuseLoad). When keepLogOnClose fails, both still close and
// the error is returned; SQLite may then checkpoint the log, which changes
// the bytes of sheet.db and not its rows.
func closeConns(db, rdb *sql.DB, keepLog bool) error {
	var err error
	if rdb != nil {
		err = rdb.Close()
	}
	if db == nil {
		return err
	}
	if keepLog {
		conn, cerr := db.Conn(context.Background())
		if cerr == nil {
			cerr = conn.Raw(func(dc any) error { return keepLogOnClose(dc) })
			cerr = errors.Join(cerr, conn.Close())
		}
		err = errors.Join(err, cerr)
	}
	return errors.Join(err, db.Close())
}

// keepLogOnClose turns off the checkpoint SQLite runs when conn, a
// connection of the modernc driver, is the last to close: the option
// SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE of sqlite3_db_config, which the driver
// does not offer. It calls it with the driver's own handle of the
// connection, and fails when the driver no longer has the fields that hold
// it.
func keepLogOnClose(conn any) error {
	if err := keepLogFault.Load(); err != nil {
		return *err
	}
	v := reflect.ValueOf(conn)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("sheet.db connection %T has no SQLite handle", conn)
	}
	handle, state := v.Elem().FieldByName("db"), v.Elem().FieldByName("tls")
	if !handle.IsValid() || handle.Kind() != reflect.Uintptr || handle.Uint() == 0 ||
		!state.IsValid() || state.Type() != reflect.TypeFor[*libc.TLS]() || state.IsNil() {
		return fmt.Errorf("sheet.db connection %T has no SQLite handle", conn)
	}
	tls := (*libc.TLS)(state.UnsafePointer())
	// The option takes an int, 1 to keep the log, and a pointer for the
	// value it sets, which is not wanted here.
	args := tls.Alloc(16)
	defer tls.Free(16)
	if rc := sqlite3.Xsqlite3_db_config(tls, uintptr(handle.Uint()), sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE,
		libc.VaList(args, int32(1), uintptr(0))); rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sheet.db checkpoint on close: SQLite result %d", rc)
	}
	return nil
}

// keepLogFault, when a test sets it, makes keepLogOnClose fail with its
// error, as it fails when the driver no longer has the fields that hold the
// handle of a connection. It is nil outside tests.
var keepLogFault atomic.Pointer[error]
