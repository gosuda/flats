package sheet

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/sqliteuri"
)

// fieldSheet returns a stopped sheet with a field of each kind sheet.db
// stores: two tabs, a bold number in A1, a text in B1, a formula in C1,
// and dirty marks of its revision files.
func fieldSheet(t *testing.T) *env {
	t.Helper()
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Empty"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"text","=A1+1"]]},
		{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}}]}`)
	e.in.Stop()
	return e
}

// copied returns an env of a copy of the sheet.db of a stopped sheet, in a
// directory of its own, after the statements ran on it, each changing a
// row at least.
func copied(t *testing.T, from *env, statements ...string) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), b: newBackend(), clock: newClock(), opts: from.opts}
	copyFile(t, filepath.Join(from.dir, dbName), filepath.Join(e.dir, dbName))
	execDB(t, e, func(db *sql.DB) error {
		for _, s := range statements {
			res, err := db.Exec(s)
			if err != nil {
				return err
			}
			if rows, _ := res.RowsAffected(); rows == 0 {
				return fmt.Errorf("%s changed no row", s)
			}
		}
		return nil
	})
	return e
}

// loadAllocs loads the sheet.db of a stopped sheet as the instance does and
// returns the error and the bytes the load allocated.
func loadAllocs(t *testing.T, e *env) (error, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(e.dir, dbName))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	got := allocatedBy(func() { _, err = loadWorkbook(context.Background(), db) })
	return err, got
}

// oversizedFields set each field the load reads to the bytes of their
// argument, written as a zero blob, as text where the column holds text.
var oversizedFields = map[string]string{
	"the store id":         `UPDATE meta SET store=CAST(zeroblob(%d) AS TEXT)`,
	"the calculation date": `UPDATE meta SET calc_date=CAST(zeroblob(%d) AS TEXT)`,
	"the tab order":        `UPDATE meta SET tab_order=CAST(zeroblob(%d) AS TEXT)`,
	"a tab name":           `UPDATE tabs SET name=CAST(zeroblob(%d) AS TEXT) WHERE id=2`,
	"the row ids":          `UPDATE tabs SET row_ids=zeroblob(%d) WHERE id=1`,
	"the row stamps":       `UPDATE tabs SET row_born=zeroblob(%d) WHERE id=1`,
	"the column ids":       `UPDATE tabs SET col_ids=zeroblob(%d) WHERE id=2`,
	"the column stamps":    `UPDATE tabs SET col_born=zeroblob(%d) WHERE id=2`,
	"the chunk list":       `UPDATE tabs SET chunks=zeroblob(%d) WHERE id=2`,
	"a format":             `UPDATE styles SET format=CAST(zeroblob(%d) AS TEXT)`,
	"a format's stamps":    `UPDATE styles SET stamps=zeroblob(%d)`,
	"a text":               `UPDATE cells SET str=CAST(zeroblob(%d) AS TEXT) WHERE kind=2`,
	"a formula":            `UPDATE cells SET str=CAST(zeroblob(%d) AS TEXT) WHERE kind=7`,
	"a number's text":      `UPDATE cells SET str=CAST(zeroblob(%d) AS TEXT) WHERE kind=1`,
	"a file path":          `UPDATE files SET path=CAST(zeroblob(%d) AS TEXT) WHERE rowid=(SELECT min(rowid) FROM files)`,
	"a file hash":          `UPDATE files SET hash=CAST(zeroblob(%d) AS TEXT) WHERE rowid=(SELECT min(rowid) FROM files)`,
}

// Each field of sheet.db is read only when it is no longer than the writer
// stores it. A field of 8 MiB in any of them, in a sheet.db that is valid
// otherwise, is refused, and the load allocates far less than the field,
// which never reaches memory. The undamaged sheet loads.
func TestOversizedStoredFieldsAreNotRead(t *testing.T) {
	const n = 8 << 20
	base := fieldSheet(t)
	if err, _ := loadAllocs(t, base); err != nil {
		t.Fatalf("the undamaged sheet: %v", err)
	}
	for name, statement := range oversizedFields {
		t.Run(name, func(t *testing.T) {
			e := copied(t, base, fmt.Sprintf(statement, n))
			err, got := loadAllocs(t, e)
			if err == nil {
				t.Fatal("the sheet.db was loaded")
			}
			t.Logf("refused after %d KiB: %v", got>>10, err)
			if measures && got > n/16 {
				t.Errorf("the refused load allocated %d KiB for a field of %d KiB", got>>10, n>>10)
			}
			wantLoadRefused(t, e, name)
		})
	}
}

// longestFormats are formats with every property set, to the longest
// value of each property, and each number format of the longest spelling
// of its kind.
func longestFormats() []formula.Format {
	var out []formula.Format
	for _, nf := range []formula.NumberFormat{
		{Kind: formula.NumberCurrency, Decimals: 10, Symbol: "\U0001E800\U0001E800\U0001E800"},
		{Kind: formula.NumberFixed, Decimals: 10, Thousands: true},
		{Kind: formula.NumberDateTime, Date: formula.DateMonth, Time: formula.TimeAMPM},
	} {
		for c := formula.Color(1); c <= formula.PaletteColors; c++ {
			out = append(out, formula.Format{Bold: true, Italic: true, Underline: true, Strikethrough: true,
				TextColor: c, Fill: c, Align: formula.AlignCenter, Wrap: true, Number: nf})
		}
	}
	return out
}

// The bounds of the fields hold what the writer stores at its largest:
// axes of as many runs as ids, each of ids and stamps near their limits, a
// chunk list of the most chunks, a format with every property, nine
// stamps of the largest number, a tab name, a text and an unreadable
// formula of four-byte code points, the longest stored formula, and the
// longest revision file path.
func TestStoredFieldBoundsHoldTheWritersLargest(t *testing.T) {
	spread := func(n int) []uint64 {
		ids := make([]uint64, n)
		for i := range ids {
			ids[i] = maxID - 1 - 2*uint64(n-i)
		}
		return ids
	}
	stamps := func(ids []uint64) runStamps {
		r := make(runStamps, len(ids))
		for i, id := range ids {
			r[i] = stampRun{id, 1, math.MaxUint64}
		}
		return r
	}
	rows, cols := spread(formula.MaxRows), spread(formula.MaxCols)
	chunks := make([]RowID, maxTabChunks)
	for i := range chunks {
		chunks[i] = RowID(maxID - 1 - uint64(maxTabChunks-i))
	}
	format := 0
	for _, f := range longestFormats() {
		spelled, err := appendFormat(nil, f)
		if err != nil {
			t.Fatal(err)
		}
		format = max(format, len(spelled))
	}
	var high [propCount]uint64
	for i := range high {
		high[i] = math.MaxUint64
	}
	wide := strings.Repeat("\U0010FFFF", formula.MaxTextLen)
	for _, c := range []struct {
		name      string
		size, max int
	}{
		{"row ids", len(encodeIDs(rows)), maxIDRunBytes(formula.MaxRows)},
		{"row stamps", len(encodeStamps(stamps(rows))), maxBornBytes(formula.MaxRows)},
		{"column ids", len(encodeIDs(cols)), maxIDRunBytes(formula.MaxCols)},
		{"column stamps", len(encodeStamps(stamps(cols))), maxBornBytes(formula.MaxCols)},
		{"chunk list", len(encodeRowIDs(chunks)), maxChunkBytes},
		{"format", format, maxFormatBytes},
		{"format stamps", len(encodeStamps9(high)), maxStampsBytes},
		{"tab name", len(strings.Repeat("\U0010FFFF", maxTabName)), maxNameBytes},
		{"text", len(wide), maxCellTextBytes(int64(formula.ContentText))},
		{"unreadable formula", len("=" + wide[:4*(formula.MaxFormulaChars-1)]), maxCellTextBytes(int64(formula.ContentUnreadable))},
		{"stored formula", formula.MaxStoredBytes, maxCellTextBytes(int64(formula.ContentFormula))},
		{"file path", len(chunkPrefix(maxID-1, maxID-1) + strconv.Itoa(math.MaxInt) + ".json"), maxPathBytes},
		{"file hash", len(contentHash(nil)), maxHashBytes},
		{"store id", len(strings.Repeat("ff", 16)), maxStoreBytes},
		{"calculation date", len(formula.Date(formula.DateOf(9999, 12, 31)).String()), maxDateBytes},
	} {
		if c.size > c.max {
			t.Errorf("the writer's largest %s takes %d bytes, past its bound of %d", c.name, c.size, c.max)
		}
	}
}

// boundarySheet returns a stopped sheet at the largest state sheet.db
// holds that the bounds have to let through: a tab name of four-byte code
// points, a text of the longest, five tabs of the most chunks a tab can
// have, together within what the cells of a sheet allow, each chunk marked
// as edits since the last boundary mark it, and in the first tab 100,000
// rows in runs of one id each.
func boundarySheet(t *testing.T, mods ...func(*Options)) *env {
	t.Helper()
	e := start(t, append(mods, unhurried)...)
	name := strings.Repeat("\U0001F600", maxTabName)
	text := strings.Repeat("\U0001F600", formula.MaxTextLen)
	e.mustAgent(jsonf(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":%q},{"op":"add_tab","name":"B"},{"op":"add_tab","name":"C"},
		{"op":"add_tab","name":"D"},{"op":"add_tab","name":"E"},{"op":"write","tab":%q,"at":"A1","rows":[[%q]]}]}`, name, name, text))
	e.in.Stop()
	ids := make([]uint64, formula.MaxRows)
	born := make(runStamps, formula.MaxRows)
	for i := range ids {
		ids[i] = uint64(2*i + 1)
		born[i] = stampRun{ids[i], 1, 1}
	}
	chunks := make([]RowID, maxTabChunks)
	for i := range chunks {
		chunks[i] = RowID(1000 * i)
	}
	if 5*maxTabChunks > maxSheetChunks {
		t.Fatalf("five tabs of %d chunks are past the bound of %d", maxTabChunks, maxSheetChunks)
	}
	execDB(t, e, func(db *sql.DB) error {
		if _, err := db.Exec(`UPDATE meta SET next_row=max(next_row,?)`, 2*formula.MaxRows+1); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE tabs SET row_ids=?,row_born=? WHERE id=1`, encodeIDs(ids), encodeStamps(born)); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE tabs SET chunks=?`, encodeRowIDs(chunks)); err != nil {
			return err
		}
		// The chunks hold no rows, as after a boundary of full tabs their
		// rows were cleared: every chunk is marked, as the edits marked it.
		for tab := TabID(1); tab <= 5; tab++ {
			for _, first := range chunks {
				if _, err := db.Exec(`INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES(?,'',0,1,0)
					ON CONFLICT(path) DO UPDATE SET dirty_seq=1`, chunkPrefix(tab, first)+"0.json"); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return e
}

// A sheet.db at the bounds loads, opens the same again, and its backup,
// put in place as a restore does, opens the same. One chunk more in a tab,
// or a sixth tab of the most chunks, which the cells of a sheet cannot
// fill, is refused, and so is a chunk list of 1,250,000 ascending ids,
// with far less allocated than the list. The backup of such a sheet.db is
// refused as well.
func TestStoredBoundariesLoad(t *testing.T) {
	slow(t, "loads tabs of 100,000 rows in runs of one")
	base := boundarySheet(t)
	e := copied(t, base)
	e.open()
	dump := e.dump()
	e.in.mu.RLock()
	chunks := len(e.in.wb.tabs[1].chunks)
	e.in.mu.RUnlock()
	if chunks != maxTabChunks {
		t.Fatalf("the tab holds %d chunks", chunks)
	}
	e.want("A1", strings.Repeat("\U0001F600", formula.MaxTextLen))
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet at the bounds opens differently:\n%s", dumpDiff(dump, after))
	}
	e.in.Stop()
	back := &env{t: t, dir: restored(t, e.dir), b: e.b, clock: e.clock, opts: e.opts}
	back.open()
	if after := back.dump(); after != dump {
		t.Fatalf("the restored backup opens differently:\n%s", dumpDiff(dump, after))
	}
	back.in.Stop()

	one := make([]RowID, maxTabChunks+1)
	for i := range one {
		one[i] = RowID(i)
	}
	sixth := `INSERT INTO tabs(id,name,name_key,born,name_seq,settings_seq,struct_seq,frozen_rows,frozen_cols,
		row_ids,row_born,col_ids,col_born,chunks) SELECT 99,'F','f',born,name_seq,settings_seq,struct_seq,0,0,row_ids,row_born,col_ids,col_born,chunks
		FROM tabs WHERE id=2`
	order := `UPDATE meta SET tab_order=replace(tab_order,']',',99]'),next_tab=100`
	var many []byte
	for i := range 1_250_000 {
		many = binary.AppendUvarint(many, uint64(i))
	}
	for name, damaged := range map[string]*env{
		"a chunk more":          copied(t, base, fmt.Sprintf(`UPDATE tabs SET chunks=x'%x' WHERE id=2`, encodeRowIDs(one))),
		"a sixth tab of chunks": copied(t, base, sixth, order),
		"1,250,000 chunks":      copied(t, base, fmt.Sprintf(`UPDATE tabs SET chunks=x'%x' WHERE id=2`, many)),
	} {
		t.Run(name, func(t *testing.T) {
			err, got := loadAllocs(t, damaged)
			if err == nil {
				t.Fatal("the sheet.db was loaded")
			}
			ok, _ := loadAllocs(t, base)
			if ok != nil {
				t.Fatal(ok)
			}
			_, whole := loadAllocs(t, base)
			t.Logf("refused after %d KiB, the sheet at the bounds loads in %d KiB: %v", got>>10, whole>>10, err)
			if measures && got > whole+int64(len(many))/16 {
				t.Errorf("the refused load allocated %d KiB, one of the sheet at the bounds %d KiB", got>>10, whole>>10)
			}
			wantLoadRefused(t, damaged, name)
			wantLoadRefused(t, &env{t: t, dir: restored(t, damaged.dir), b: newBackend(), clock: newClock(), opts: damaged.opts}, name+" restored")
		})
	}
}

// A load of a sheet.db with a field past its bound takes turns at the gate
// as any load does until it reaches the field: with every slot taken by
// such loads held while they build an axis of 100,000 runs, a small sheet
// loads twice, and a stop ends them there. Opened again, each is refused
// at the field. It runs with a gate of one slot and with several.
func TestLoadsOfOversizedFieldsTakeTurns(t *testing.T) {
	slow(t, "loads tabs of 100,000 rows in runs of one while others wait")
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			long := make([]*env, slots)
			for i := range long {
				long[i] = boundarySheet(t, func(o *Options) { o.Slug = heavySlug })
				execDB(t, long[i], func(db *sql.DB) error {
					_, err := db.Exec(fmt.Sprintf(`UPDATE tabs SET row_born=zeroblob(%d) WHERE id=2`, 8<<20))
					return err
				})
			}
			w := watchLoads(t)
			w.holdAt("axes")
			for _, e := range long {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				e.in, e.b.in = in, in
			}
			handOverAndStop(t, w, slots, "axes", []string{"widths", "styles", "cells", "formats", "refill", "files"}, long)
			for _, e := range long {
				wantLoadRefused(t, e, "a row stamps field of 8 MiB")
			}
			gateSettles(t, "after the loads", slots, 0)
		})
	}
}
