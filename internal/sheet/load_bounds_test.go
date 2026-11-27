package sheet

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/sqliteuri"
)

// execDB runs statements on the sheet.db of a stopped instance, each with
// its arguments, however many rows each changes.
func execDB(t *testing.T, e *env, run func(db *sql.DB) error) {
	t.Helper()
	db, _, _, err := openDB(filepath.Join(e.dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	err = run(db)
	if cerr := db.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
}

// readOnly opens the sheet.db of a stopped instance for reading, as the
// instance's reader does.
func readOnly(t *testing.T, e *env) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(e.dir, dbName))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// allocatedBy returns the bytes fn allocates.
func allocatedBy(fn func()) int64 {
	runtime.GC()
	before := allocated()
	fn()
	return allocated() - before
}

// A content kind is checked as sheet.db stores it. Narrowed to the byte of
// a ContentKind first, 257 reads as a number and 263 as a formula, and a
// negative kind wraps the same way. The kinds the writer stores load.
func TestStoredKindIsCheckedBeforeNarrowing(t *testing.T) {
	for name, set := range map[string]string{
		"257, a number modulo 256":   "kind=257,num=1,str=NULL",
		"263, a formula modulo 256":  "kind=263,num=NULL,str='=1'",
		"-255, a number modulo 256":  "kind=-255,num=1,str=NULL",
		"-249, a formula modulo 256": "kind=-249,num=NULL,str='=1'",
		"-1":                         "kind=-1,num=NULL,str=NULL",
	} {
		t.Run(name, func(t *testing.T) {
			e := storedAxisSheet(t, `UPDATE cells SET `+set+` WHERE tab=1 AND row=1 AND col=1`)
			wantLoadRefused(t, e, name)
		})
	}
	for _, c := range []struct{ set, shown string }{
		{"kind=0,num=NULL,str=NULL", ""},
		{"kind=1,num=5,str=NULL", "5"},
		{"kind=2,num=NULL,str='five'", "five"},
		{"kind=3,num=1,str=NULL", "TRUE"},
		{"kind=4,num=46096,str=NULL", "2026-03-15"},
		{"kind=5,num=0.5,str=NULL", "12:00:00"},
		{"kind=6,num=46096.5,str=NULL", "2026-03-15 12:00:00"},
		{"kind=7,num=NULL,str='=2+3'", "5"},
		{"kind=8,num=NULL,str='=1+@'", "#ERROR!"},
	} {
		t.Run(c.set, func(t *testing.T) {
			e := storedAxisSheet(t, `UPDATE cells SET `+c.set+` WHERE tab=1 AND row=1 AND col=1`)
			e.open()
			e.want("A1", c.shown)
		})
	}
}

// A date, a time and a date with time in sheet.db are numbers that a row
// file holds as the same date or time. Day 0 would be written to a
// revision as 1899-12-30, which no revision reads, and a time of 2 days as
// midnight, read back as 0. The load refuses them. The numbers the writer
// gives, at the ends of their ranges, load and come back the same from a
// revision and after a restart.
func TestStoredDatesKeepTheirMeaning(t *testing.T) {
	for name, n := range map[string]struct {
		kind formula.ContentKind
		num  float64
	}{
		"day 0":                              {formula.ContentDate, 0},
		"day 1":                              {formula.ContentDate, 1},
		"the day after 9999-12-31":           {formula.ContentDate, lastDay + 1},
		"a negative day":                     {formula.ContentDate, -3},
		"half a day":                         {formula.ContentDate, 45000.5},
		"a time of a whole day":              {formula.ContentTime, 1},
		"a time of two days":                 {formula.ContentTime, 2},
		"a negative time":                    {formula.ContentTime, -0.25},
		"half a second":                      {formula.ContentTime, 0.5 / 86400},
		"a date with time on day 1":          {formula.ContentDateTime, 1.5},
		"a date with time after 9999-12-31":  {formula.ContentDateTime, lastDay + 1.25},
		"a date with time and half a second": {formula.ContentDateTime, 45000 + 0.5/86400},
	} {
		t.Run(name, func(t *testing.T) {
			e := storedAxisSheet(t)
			execDB(t, e, func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE cells SET kind=?,num=?,str=NULL WHERE tab=1 AND row=1 AND col=1`, int(n.kind), n.num)
				return err
			})
			wantLoadRefused(t, e, name)
		})
	}

	e := storedAxisSheet(t)
	legit := []struct {
		kind formula.ContentKind
		num  float64
	}{
		{formula.ContentDate, firstDay},
		{formula.ContentDate, lastDay},
		{formula.ContentTime, 0},
		{formula.ContentTime, 86399.0 / 86400},
		{formula.ContentDateTime, firstDay},
		{formula.ContentDateTime, lastDay + 86399.0/86400},
		{formula.ContentDateTime, float64(formula.DateOf(2026, 3, 15)) + float64(14*3600+30*60+5)/86400},
	}
	execDB(t, e, func(db *sql.DB) error {
		for i, c := range legit {
			if _, err := db.Exec(`INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(1,2,?,?,?,NULL,0,1,0)`, i+1, int(c.kind), c.num); err != nil {
				return err
			}
		}
		return nil
	})
	e.open()
	q := ReadQuery{Tab: "Sheet1", Range: "A2:G2", Include: []string{"shown"}}
	strip := func(out map[string]any) string {
		delete(out, "state")
		delete(out, "revision")
		return fmt.Sprint(out)
	}
	live := strip(e.read(q))
	for i, want := range []string{"1900-01-01", "9999-12-31", "00:00:00", "23:59:59", "1900-01-01T00:00:00", "9999-12-31T23:59:59", "2026-03-15T14:30:05"} {
		if !strings.Contains(live, want) {
			t.Errorf("cell %d of row 2 does not read as %s: %s", i+1, want, live)
		}
	}
	// A write in the same chunk makes its row file part of the revision.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"H2","rows":[[1]]}]}`)
	n := closeRevision(e)
	if got := strip(e.readRevision(n, q)); got != live {
		t.Errorf("the revision reads\n%s\nthe sheet\n%s", got, live)
	}
	e.restart()
	if got := strip(e.read(q)); got != live {
		t.Errorf("after a restart the sheet reads\n%s\nbefore\n%s", got, live)
	}
}

// The tab order bounds the tabs of a load before their axes are built: a
// sheet.db whose order names one tab and that holds 60 more of 20,000 rows
// each is refused having built at most the tabs of the order, which the
// bytes it allocates show beside those of one axis. A sheet without a tab,
// an order of more tabs than a sheet may hold, one that names a tab twice
// and one spelled otherwise than the writer spells it are refused. A sheet
// of 50 tabs loads.
func TestStoredTabOrderBoundsTheTabs(t *testing.T) {
	const extra, rows = 60, 20_000
	ids := make([]uint64, rows)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	axisIDs, axisBorn := encodeIDs(ids), encodeStamps(runStamps{{1, rows, 1}})
	one := allocatedBy(func() {
		if _, err := loadAxis(axisIDs, axisBorn, formula.MaxRows, rows+1, &loadPace{}); err != nil {
			t.Fatal(err)
		}
	})
	e := storedAxisSheet(t)
	execDB(t, e, func(db *sql.DB) error {
		if _, err := db.Exec(`UPDATE meta SET next_tab=1000,next_row=max(next_row,?)`, rows+1); err != nil {
			return err
		}
		for i := range extra {
			if _, err := db.Exec(`INSERT INTO tabs(id,name,name_key,born,name_seq,settings_seq,struct_seq,frozen_rows,frozen_cols,
				row_ids,row_born,col_ids,col_born,chunks) SELECT ?,?,?,born,name_seq,settings_seq,struct_seq,0,0,?,?,col_ids,col_born,chunks FROM tabs WHERE id=1`,
				100+i, fmt.Sprintf("Extra%d", i), fmt.Sprintf("extra%d", i), axisIDs, axisBorn); err != nil {
				return err
			}
		}
		return nil
	})
	db := readOnly(t, e)
	var err error
	got := allocatedBy(func() { _, err = loadWorkbook(context.Background(), db) })
	if err == nil {
		t.Fatal("a sheet.db of tabs out of its order was loaded")
	}
	t.Logf("the refused load allocated %d KiB, one axis of %d rows %d KiB", got>>10, rows, one>>10)
	if measures && got > 4*one {
		t.Errorf("the refused load allocated %d KiB, %d axes of %d rows, before it refused the tabs out of the order",
			got>>10, got/one, rows)
	}
	wantLoadRefused(t, e, "tabs out of the order")

	for name, damage := range map[string][]string{
		"no tab":            {`DELETE FROM cells`, `DELETE FROM cols`, `DELETE FROM tabs`, `UPDATE meta SET tab_order='[]'`},
		"an order of 51":    {`UPDATE meta SET tab_order='[` + strings.TrimSuffix(strings.Repeat("1,", 51), ",") + `]'`},
		"a tab twice":       {`UPDATE meta SET tab_order='[1,1]'`},
		"a space":           {`UPDATE meta SET tab_order=replace(tab_order,',',', ')`},
		"a leading zero":    {`UPDATE meta SET tab_order=replace(tab_order,'[1','[01')`},
		"a long tab order":  {`UPDATE meta SET tab_order='[1' || replace(hex(zeroblob(5000)),'0',' ') || ',2]'`},
		"an order of three": {`UPDATE meta SET tab_order=replace(tab_order,']',',3]')`},
	} {
		t.Run(name, func(t *testing.T) {
			e := storedAxisSheet(t)
			execDB(t, e, func(db *sql.DB) error {
				for _, d := range damage {
					if _, err := db.Exec(d); err != nil {
						return err
					}
				}
				return nil
			})
			wantLoadRefused(t, e, name)
		})
	}

	t.Run("50 tabs", func(t *testing.T) {
		e := start(t)
		ops := make([]string, maxTabs-1)
		for i := range ops {
			ops[i] = fmt.Sprintf(`{"op":"add_tab","name":"T%d"}`, i)
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		dump := e.dump()
		e.restart()
		if after := e.dump(); after != dump {
			t.Fatalf("the sheet of 50 tabs opens differently:\n%s", dumpDiff(dump, after))
		}
	})
}

// blankSheet returns a stopped sheet whose sheet.db holds n blank records
// in Sheet1, 26 to a row from A1, as clears leave them before a tick
// collects them. The stamp of record i is base+(i*7919)%n, so newer and
// older ones come in no order. The oldest styled of them have a style of
// their own with stamps and no format. A1, the oldest, has a bold style
// and so holds a format, and unused more style rows that no cell uses.
func blankSheet(t *testing.T, n, styled, unused int) (e *env, base uint64) {
	t.Helper()
	e = start(t, unhurried)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":%d}]}`, rowsFor(n)))
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		if err := db.QueryRow(`SELECT seq+1 FROM meta`).Scan(&base); err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE meta SET seq=seq+?`, n+10); err != nil {
			return err
		}
		if _, err := tx.Exec(`WITH RECURSIVE i(n) AS (SELECT 0 UNION ALL SELECT n+1 FROM i WHERE n+1<?)
			INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) SELECT 1,1+n/26,1+n%26,0,NULL,NULL,0,?+(n*7919)%?,0 FROM i`,
			n, base, n); err != nil {
			return err
		}
		for i := range styled {
			var stamps [propCount]uint64
			stamps[propBold] = base + uint64(i)
			if _, err := tx.Exec(`INSERT INTO styles(id,format,stamps,refs) VALUES(?,'{}',?,1)`, 1000+i, encodeStamps9(stamps)); err != nil {
				return err
			}
		}
		if styled > 0 {
			if _, err := tx.Exec(`UPDATE cells SET style=1000+(seq-?) WHERE tab=1 AND seq<?`, base, base+uint64(styled)); err != nil {
				return err
			}
		}
		for i := range unused {
			var stamps [propCount]uint64
			stamps[propItalic] = base + uint64(i)
			if _, err := tx.Exec(`INSERT INTO styles(id,format,stamps,refs) VALUES(?,'{"italic":true}',?,1)`, 100000+i, encodeStamps9(stamps)); err != nil {
				return err
			}
		}
		var bold [propCount]uint64
		bold[propBold] = base + uint64(n) + 1
		if _, err := tx.Exec(`INSERT INTO styles(id,format,stamps,refs) VALUES(900000,'{"bold":true}',?,1)`, encodeStamps9(bold)); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE cells SET style=900000 WHERE tab=1 AND row=1 AND col=1`); err != nil {
			return err
		}
		return tx.Commit()
	})
	return e, base
}

// heldBlanks walks the tabs of the sheet for its blank records and returns
// how many there are and the oldest stamp among them.
func heldBlanks(wb *Workbook) (n int, oldest uint64) {
	oldest = ^uint64(0)
	for _, t := range wb.tabs {
		for _, rd := range t.data {
			for _, s := range rd.cells {
				if s.c.blank() && wb.styles.format(s.c.style) == (formula.Format{}) {
					n++
					oldest = min(oldest, max(s.c.seq, s.c.textSeq))
				}
			}
		}
	}
	return n, oldest
}

// A sheet.db can hold more blank records than the tick keeps: a burst of
// clears adds them between two ticks. A load of six times as many as the
// tick keeps holds the newest it keeps, and the heap it holds is that of a
// load of as many. The guard floor rises to the newest record left out, so
// an older range guard is refused as state_too_old. The rows left out, and
// the style rows that only they or nothing use, stay in sheet.db until the
// next commit deletes them; the sheet opens the same before and after it,
// and reads of its blank cells answer.
func TestLoadKeepsTheNewestBlankRecords(t *testing.T) {
	slow(t, "loads 600,000 blank records")
	limit := maxBlankRecords
	n := 6 * limit
	e, base := blankSheet(t, n, 1000, 500)
	var held int64
	if measures {
		before := quietHeap(t, 1<<20)
		e.open()
		held = quietHeap(t, 1<<20) - before
	} else {
		e.open()
	}
	// A1 holds a format, and the n-1 blank records have the stamps after
	// its own.
	floor := base + uint64(n-1-limit)
	check := func(when string) {
		t.Helper()
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		wb := e.in.wb
		blanks, oldest := heldBlanks(wb)
		if blanks != limit || oldest != floor+1 || wb.guardFloor != floor {
			t.Fatalf("%s: the sheet holds %d blank records from stamp %d with the floor %d, want %d from %d with %d",
				when, blanks, oldest, wb.guardFloor, limit, floor+1, floor)
		}
		if len(wb.styles.byID) != 1 || len(wb.wire.list) != 2 {
			t.Fatalf("%s: the table holds %d styles and the wire list %d formats", when, len(wb.styles.byID), len(wb.wire.list))
		}
	}
	check("after the load")
	// The rows the load left out are still stored.
	rows := func() (blanks, styles int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM cells WHERE kind=0 AND style<>900000`).Scan(&blanks); err != nil {
			t.Fatal(err)
		}
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM styles`).Scan(&styles); err != nil {
			t.Fatal(err)
		}
		return blanks, styles
	}
	if blanks, styles := rows(); blanks != n-1 || styles != 1+1000+500 {
		t.Fatalf("after the load sheet.db holds %d blank records and %d styles", blanks, styles)
	}
	if measures {
		f, _ := blankSheet(t, limit, 0, 0)
		before := quietHeap(t, 1<<20)
		f.open()
		kept := quietHeap(t, 1<<20) - before
		t.Logf("a load of %d blank records holds %d KiB, one of %d holds %d KiB", n, held>>10, limit, kept>>10)
		if held > kept+kept/4+4<<20 {
			t.Errorf("a load of %d blank records holds %d KiB, one of the %d it keeps %d KiB", n, held>>10, limit, kept>>10)
		}
	}
	out := e.read(ReadQuery{Tab: "Sheet1", Range: "B1:Z23077"})
	if rows, ok := out["rows"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("the read of the blank records: %v", out)
	}
	e.want("Sheet1!Z23077", "")

	token := func(seq uint64) string {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return stateToken(e.in.wb.store, seq)
	}
	_, err := e.agent(fmt.Sprintf(`{"state":%q,"guard":{"ranges":["Sheet1!A23090:B23095"]},"ops":[{"op":"write","tab":"Sheet1","at":"A23100","rows":[[1]]}]}`, token(floor-1)))
	e.wantRefusal(err, "conflict", "state_too_old")

	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("before a commit the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
	check("after a restart")
	e.mustAgent(fmt.Sprintf(`{"state":%q,"guard":{"ranges":["Sheet1!A23090:B23095"]},"ops":[{"op":"write","tab":"Sheet1","at":"A23100","rows":[[1]]}]}`, token(floor)))
	if blanks, styles := rows(); blanks != limit || styles != 1 {
		t.Fatalf("after the commit sheet.db holds %d blank records and %d styles, want %d and 1", blanks, styles, limit)
	}
	dump = e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("after the commit the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
}

// A style row that no cell uses is no part of the sheet. The writer leaves
// such rows when the tick collects the only blank records of a style and
// no commit follows. A load does not hold them, whether one or many, and
// the next commit deletes them. A cell's style is still checked: one with
// a format in another spelling than the writer's, which reads as the same
// format, or with bytes after its stamps is refused.
func TestLoadHoldsOnlyTheStylesCellsUse(t *testing.T) {
	e, _ := blankSheet(t, 26, 0, 5000)
	e.open()
	styles := func() (held, stored int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM styles`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		return len(e.in.wb.styles.byID), stored
	}
	if held, stored := styles(); held != 1 || stored != 5001 {
		t.Fatalf("after the load the table holds %d styles and sheet.db %d", held, stored)
	}
	e.want("Sheet1!A1", "")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1]]}]}`)
	if held, stored := styles(); held != 1 || stored != 1 {
		t.Fatalf("after a commit the table holds %d styles and sheet.db %d", held, stored)
	}
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently:\n%s", dumpDiff(dump, after))
	}

	for name, set := range map[string]string{
		"a format with a space":      `format='{"bold": true}'`,
		"a format of a false":        `format='{"bold":true,"italic":false}'`,
		"no format in another spell": `format='{"bold":false}'`,
		"bytes after the stamps":     `stamps=stamps||x'00'`,
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := blankSheet(t, 26, 0, 0)
			execDB(t, e, func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE styles SET ` + set + ` WHERE id=900000`)
				return err
			})
			wantLoadRefused(t, e, name)
		})
	}
}

// A row file holds the cells with content or a format, and each format its
// cells use once. An entry of neither, which no tick of a revision would
// collect, a format of no property, a format listed twice or one no cell
// uses, a row without cells and a null value, which reads as no key or as
// the number 0, are refused by a view and a rollback, and the sheet stays
// as it was. A file of a chunk's rows of empty entries is refused as one. A
// cell of a format and no content loads.
func TestRevisionHoldsNoBlankRecords(t *testing.T) {
	f := newLiveFixture(t)
	e := f.e
	for name, file := range map[string]string{
		"an empty entry":          rowFile(`{}`),
		"6,656 empty entries":     cellsFile(repeated(`{}`, 26*chunkRows)),
		"a format of no property": `{"rows":[{"cells":{"1":{"s":0}},"id":1}],"styles":[{}]}`,
		"a format twice":          `{"rows":[{"cells":{"1":{"s":0},"2":{"s":1}},"id":1}],"styles":[{"bold":true},{"bold":true}]}`,
		"an unused format":        `{"rows":[{"cells":{"1":{"s":0},"2":{"v":1}},"id":1}],"styles":[{"bold":true},{"italic":true}]}`,
		"a row without cells":     `{"rows":[{"cells":{"1":{"v":1}},"id":1},{"cells":{},"id":2}],"styles":[]}`,
		"a null value":            rowFile(`{"v":null}`),
		"a null date and a value": rowFile(`{"d":null,"v":1}`),
		"a null style":            rowFile(`{"s":null,"v":1}`),
	} {
		f.wantRefused(name, f.revision(1000, file))
	}
	formatted := f.revision(1000, `{"rows":[{"cells":{"1":{"s":0}},"id":1}],"styles":[{"bold":true}]}`)
	if err := f.view(formatted); err != nil {
		t.Fatalf("the view of a cell with a format: %v", err)
	}
	if err := e.in.Rollback(context.Background(), formatted, "Alice", "format"); err != nil {
		t.Fatalf("the rollback to a cell with a format: %v", err)
	}
	e.restart()
	e.in.mu.RLock()
	formats := slices.Collect(maps.Values(e.in.wb.styles.byID))
	e.in.mu.RUnlock()
	if len(formats) != 1 || !formats[0].f.Bold {
		t.Fatalf("after the rollback the table holds %v", formats)
	}
	e.want("A1", "")
}
