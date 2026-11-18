package sheet

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// groupRows returns the open edit groups of the sheet as stored.
func (e *env) groupRows() string {
	e.t.Helper()
	rows, err := e.in.rdb.Query(`SELECT editor,label,verified,started,last,dirty FROM groups ORDER BY editor`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var editor, label string
		var verified bool
		var started, last, dirty int64
		if err := rows.Scan(&editor, &label, &verified, &started, &last, &dirty); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %v %d %d %d", editor, label, verified, started, last, dirty))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// counters returns the id counters of the live sheet.
func (e *env) counters() [3]uint64 {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return [3]uint64{e.in.wb.nextTab, e.in.wb.nextRow, e.in.wb.nextCol}
}

// recoveryFixture is a sheet whose revision n holds Sheet1 alone, and
// whose tab gone was added and deleted after it, so that the ids of gone
// lie below the counters next and are live nowhere.
type recoveryFixture struct {
	e     *env
	alice *person
	files []File
	gone  TabID
	next  [3]uint64
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", "before")
	n := closeRevision(e)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Gone"}]}`)
	gone := e.tabID("Gone")
	rows, cols := e.rowsOf("Gone", 1, 1), e.colsOf("Gone", 1, 1)
	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Gone"}]}`)
	alice.set("A2", "live")
	files, _, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	f := &recoveryFixture{e: e, alice: alice, files: files, gone: gone, next: e.counters()}
	if rows != "[[1001,1]]" || cols != "[[27,1]]" || f.next != [3]uint64{3, 2001, 53} || gone != 2 {
		t.Fatalf("the deleted tab %d had rows %s and columns %s; the counters are %v", gone, rows, cols, f.next)
	}
	return f
}

// revision returns the files of revision n with a second tab, Archive, of
// the given id, whose one row and one column have the given ids and whose
// one cell holds "archived".
func (f *recoveryFixture) revision(id, row, col uint64) []File {
	manifest := fmt.Sprintf(`{"format":1,"tabs":[{"frozen_cols":0,"frozen_rows":0,"id":1,"name":"Sheet1"},`+
		`{"frozen_cols":0,"frozen_rows":0,"id":%d,"name":"Archive"}]}`, id)
	files := f.e.withFile(f.files, manifestPath, manifest)
	files = f.e.addFile(files, axesPath(TabID(id)), fmt.Sprintf(`{"cols":[[%d,1]],"rows":[[%d,1]],"widths":{}}`, col, row))
	return f.e.addFile(files, chunkPrefix(TabID(id), 0)+"0.json",
		fmt.Sprintf(`{"rows":[{"cells":{"%d":{"v":"archived"}},"id":%d}],"styles":[]}`, col, row))
}

// A tab of a revision whose id is not live comes back with its own ids
// only when each lies below its counter and no row or column id is live in
// another tab: the counters would give out the others again, and a load of
// sheet.db refuses an id at or past its counter. A revision that holds
// such a tab is refused, for the tab id, the row ids and the column ids
// each on its own, and the content, the counters, the undo, the edit
// groups and the outbox stay as they were; the sheet opens again as it
// was. Ids just below the counters come back as they are, and the sheet
// opens again with them and with its counters as they were.
func TestRecoveryKeepsOnlyIDsBelowTheCounters(t *testing.T) {
	ctx := context.Background()
	// The tab, row and column ids of the deleted tab are 2, 1001 and 27,
	// and the counters 3, 2001 and 53.
	for _, c := range []struct {
		name          string
		tab, row, col uint64
	}{
		{"a tab id at its counter", 3, 1001, 27},
		{"a tab id past its counter", 8, 1001, 27},
		{"a row id at its counter", 2, 2001, 27},
		{"a column id at its counter", 2, 1001, 53},
		{"a row id live in another tab", 2, 1, 27},
		{"a column id live in another tab", 2, 1001, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			e := f.e
			seq, before, groups, outbox := e.seq(), e.dump(), e.groupRows(), e.outboxAfter(0)
			depth, lost := f.alice.undoDepth()
			_, err := e.in.recoverTab(ctx, f.alice.s, "", f.revision(c.tab, c.row, c.col), TabID(c.tab))
			if refusal(err).Reason != "revision" {
				t.Errorf("the recovery answered %v, want a refusal of the revision", err)
			}
			if got, after := e.seq(), e.dump(); got != seq || after != before {
				t.Fatalf("the sheet went from %d to %d:\n%s", seq, got, dumpDiff(before, after))
			}
			if got := e.groupRows(); got != groups {
				t.Fatalf("the edit groups are\n%s\nwere\n%s", got, groups)
			}
			if got := e.outboxAfter(0); !slices.Equal(got, outbox) {
				t.Fatalf("the outbox holds %v, held %v", got, outbox)
			}
			if d, l := f.alice.undoDepth(); d != depth || l != lost {
				t.Fatalf("Alice's undo holds %d entries, %d lost; it held %d, %d lost", d, l, depth, lost)
			}
			e.restart()
			if after := e.dump(); after != before {
				t.Fatalf("the sheet opens differently after the refusal:\n%s", dumpDiff(before, after))
			}
		})
	}
	t.Run("ids below the counters", func(t *testing.T) {
		f := newRecoveryFixture(t)
		e := f.e
		got, err := e.in.recoverTab(ctx, f.alice.s, "", f.revision(2, 2000, 52), 2)
		if err != nil || got != 2 {
			t.Fatalf("the recovery answered %d, %v", got, err)
		}
		e.restart()
		if c := e.counters(); c != f.next {
			t.Fatalf("the counters are %v after the recovery, were %v", c, f.next)
		}
		if r, c := e.rowsOf("Archive", 1, 1), e.colsOf("Archive", 1, 1); r != "[[2000,1]]" || c != "[[52,1]]" {
			t.Fatalf("the recovered tab has rows %s and columns %s", r, c)
		}
		e.want("Archive!A1", "archived")
	})
}

// A tab deleted after a revision comes back from it under its own id and
// with its own row and column ids, and a tab that is still live comes back
// as a copy whose ids the counters give out. The sheet opens again with
// both.
func TestRecoveryOfADeletedTabAndOfACopy(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"write","tab":"Two","at":"A1","rows":[["two"]]}]}`)
	two := e.tabID("Two")
	rows, cols := e.rowsOf("Two", 1, 1000), e.colsOf("Two", 1, 26)
	n := closeRevision(e)
	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Two"}]}`)
	next := e.counters()
	if got, err := alice.recover(n, two); err != nil || got != two {
		t.Fatalf("the recovery of the deleted tab answered %d, %v", got, err)
	}
	copied, err := alice.recover(n, 1)
	if err != nil || uint64(copied) != next[0] {
		t.Fatalf("the recovery of the live tab answered %d, %v; the tab counter was %d", copied, err, next[0])
	}
	e.restart()
	if r, c := e.rowsOf("Two", 1, 1000), e.colsOf("Two", 1, 26); r != rows || c != cols {
		t.Fatalf("the deleted tab came back with rows %s and columns %s, had %s and %s", r, c, rows, cols)
	}
	copyRows := fmt.Sprintf("[[%d,1000]]", next[1])
	if r := e.rowsOf("Sheet1 (restored)", 1, 1000); r != copyRows {
		t.Fatalf("the copy has rows %s, want %s", r, copyRows)
	}
	if c := e.counters(); c != [3]uint64{next[0] + 1, next[1] + 1000, next[2] + 26} {
		t.Fatalf("the counters are %v after the copy, were %v", c, next)
	}
	e.want("Two!A1", "two")
}

// The look of a recovery through the ids of a tab that comes back with
// them, and the copy of its cells, also of a tab that is still live and
// comes back as a copy, count on the action's budget and end at the first
// look that finds the deadline passed: the recovery is refused with
// recalculation_time and counts little after that look, and the sheet
// stays as it was. Without a deadline it applies.
func TestRecoveryEndsAtTheDeadline(t *testing.T) {
	const n = 20000
	for _, deleted := range []bool{true, false} {
		name := "a live tab"
		if deleted {
			name = "a deleted tab"
		}
		t.Run(name, func(t *testing.T) {
			e := start(t)
			alice := e.person("Alice")
			e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"Big"},{"op":"insert_rows","tab":"Big","at":1,"count":%d}]}`, n-newTabRows))
			rows := make([]string, n)
			for i := range rows {
				rows[i] = fmt.Sprintf("[%d]", i)
			}
			e.mustAgent(`{"ops":[{"op":"write","tab":"Big","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
			big := e.tabID("Big")
			rev := closeRevision(e)
			name := "'Big (restored)'"
			if deleted {
				e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Big"}]}`)
				name = "Big"
			}
			seq, before := e.seq(), e.dump()
			deadline := e.workDeadline()
			_, w := deadline.run(1, func() string {
				_, err := alice.recover(rev, big)
				if werr := refusal(err); werr == nil || werr.Limit != "recalculation_time" {
					t.Errorf("the recovery answered %v, want a refusal for its deadline", err)
				}
				return ""
			})
			if w.found < 0 || w.used-w.found > budgetCheck {
				t.Errorf("the recovery found its deadline passed at %d units and counted on to %d", w.found, w.used)
			}
			if got, after := e.seq(), e.dump(); got != seq || after != before {
				t.Fatalf("the refused recovery changed the sheet from %d to %d:\n%s", seq, got, dumpDiff(before, after))
			}
			if _, err := alice.recover(rev, big); err != nil {
				t.Fatalf("the recovery without a deadline answered %v", err)
			}
			e.want(name+"!A20000", "19999")
		})
	}
}
