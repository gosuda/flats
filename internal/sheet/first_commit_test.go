package sheet

import (
	"database/sql"
	"fmt"
	"testing"
)

// A load that passes over blank records leaves their rows and styles in
// sheet.db for the first commit to delete, and can give the id of such a
// style out again. Here the stored style 1 is of no format, its id comes
// back first once a style at the last id leaves few, and the first commit
// gives it to a blank cell it makes bold. The cleanup runs before the
// commit stages anything, so it neither takes that cell for a record the
// load passed over nor deletes it: the format is there after a restart. A
// first commit that fails leaves the cleanup to the next, which applies
// the same and keeps the format as well.
func TestFirstCommitKeepsAFormatUnderAReusedStyleID(t *testing.T) {
	slow(t, "loads 100,000 blank records")
	limit := maxBlankRecords
	n := limit + 104
	e, base := blankSheet(t, n, 0, 0)
	var stale [2][propCount]uint64
	stale[0][propBold], stale[1][propItalic] = 1, 1
	execDB(t, e, func(db *sql.DB) error {
		for i, id := range []uint64{1, maxStyleID} {
			if _, err := db.Exec(`INSERT INTO styles(id,format,stamps,refs) VALUES(?,'{}',?,1)`, id, encodeStamps9(stale[i])); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE cells SET style=? WHERE tab=1 AND seq=?`, id, base+uint64(i)+1); err != nil {
				return err
			}
		}
		return nil
	})
	e.open()
	e.in.mu.RLock()
	floor, held := e.in.wb.guardFloor, e.in.wb.styles.byID[1]
	e.in.mu.RUnlock()
	if floor < base+2 || held != nil {
		t.Fatalf("the load kept the floor %d below the records of the stale styles, or holds style 1: %v", floor, held)
	}
	bold := `{"ops":[{"op":"format","tab":"Sheet1","ranges":["A4000"],"format":{"bold":true}}]}`
	styleOf := func() uint32 {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		t, row, col := e.at("A4000")
		if c := t.cell(row, col); c != nil {
			return c.style
		}
		return 0
	}
	in := e.in
	setFault := func(fault func() error) {
		in.mu.Lock()
		defer in.mu.Unlock()
		in.fault = fault
	}
	t.Cleanup(func() { setFault(nil) })
	before := e.dump()
	setFault(func() error { return fmt.Errorf("injected commit failure") })
	if _, err := e.agent(bold); err == nil {
		t.Fatal("the first commit was acknowledged over a failed commit")
	}
	setFault(nil)
	if after := e.dump(); after != before {
		t.Fatalf("the failed first commit changed the sheet:\n%s", dumpDiff(before, after))
	}
	e.mustAgent(bold)
	if id := styleOf(); id != 1 {
		t.Fatalf("the bold cell has style %d, not the reused id 1", id)
	}
	var rows int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM cells WHERE tab=1 AND style=1`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("sheet.db holds %d cells of style 1: %v", rows, err)
	}
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently after the first commit:\n%s", dumpDiff(dump, after))
	}
	id := styleOf()
	e.in.mu.RLock()
	f := e.in.wb.styles.format(id)
	e.in.mu.RUnlock()
	if !f.Bold {
		t.Fatal("the bold format of A4000 is gone after a restart")
	}
}
