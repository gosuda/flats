package sheet

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// tabTexts writes count distinct texts of size characters into one column
// of a tab, from at down, in calls of 100 cells.
func tabTexts(e *env, tab string, col string, from, count, size int) {
	e.t.Helper()
	for first := 0; first < count; first += 100 {
		var b strings.Builder
		fmt.Fprintf(&b, `{"ops":[{"op":"write","tab":%q,"at":"%s%d","rows":[`, tab, col, from+first)
		for i := first; i < min(first+100, count); i++ {
			if i > first {
				b.WriteByte(',')
			}
			seed := fmt.Sprintf("%s-%d ", tab, i)
			b.WriteString(`["` + strings.Repeat(seed, size/len(seed)+1)[:size] + `"]`)
		}
		b.WriteString(`]}]}`)
		e.mustAgent(b.String())
	}
}

// TestUndoKeepsNoDeletedTab has Alice delete the one formula cell of a
// tab, by its row or by its column, while 1,000 texts of 32,767 characters
// stay in the tab's other cells. An agent then deletes the tab. Alice's
// entry is small and stays in memory, and what it keeps of the heap is
// what it counts: the cell it saved, not the tab that cell lay in.
func TestUndoKeepsNoDeletedTab(t *testing.T) {
	slow(t, "writes 31 MiB of text")
	for _, rows := range []bool{true, false} {
		t.Run(map[bool]string{true: "row", false: "column"}[rows], func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"T","at":"A1","rows":[["=1"]]}]}`)
			del := e.deleteColsOp("T", 1, 1)
			if rows {
				tabTexts(e, "T", "A", 2, 1000, 32767)
				del = e.deleteRowsOp("T", 1, 1)
			} else {
				tabTexts(e, "T", "B", 1, 1000, 32767)
			}
			alice := e.person("Alice")
			if ack := alice.do(del); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"T"}]}`)
			if alice.spilled(0) != nil {
				t.Fatal("the entry of one deleted cell went to a scratch file")
			}
			// What the actions, the revisions and the maintenance hold for a
			// while goes first.
			e.b.importAll(t)
			e.b.forgetRevisions()
			e.tick()
			sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
			_, stacks := e.residentUndo()
			held := sheetHeap()
			dropUndo(e, alice)
			e.tick()
			kept := held - sheetHeap()
			t.Logf("undo counts %d KiB in memory and kept %d KiB of the heap", stacks>>10, kept>>10)
			if measures && kept > stacks+1<<20 {
				t.Errorf("undo kept %d KiB of the heap and counts %d KiB", kept>>10, stacks>>10)
			}
		})
	}
}

// TestUndoCountsTheRecordsItKeeps has Alice delete 100,000 formula cells
// and 200,000 numbers: a tab of them, all but one of its rows, or all but
// one of its columns. The entry stays in memory, and what it keeps of the
// heap is what it counts: the records as the allocator holds them and
// their formulas, not the nodes the dependency index had for them.
func TestUndoCountsTheRecordsItKeeps(t *testing.T) {
	slow(t, "writes 300,000 cells")
	formulas, numbers := raced(1000, 100), raced(2000, 200)
	for _, c := range []struct {
		name string
		op   func(e *env) string
	}{
		{"tab", func(e *env) string { return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")) }},
		{"rows", func(e *env) string { return e.deleteRowsOp("T", 1, formulas+numbers-1) }},
		{"columns", func(e *env) string { return e.deleteColsOp("T", 1, 99) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.in.mu.Lock()
			p := e.in.undoPool()
			p.entryMax, p.sessionMax, p.instanceMax = 1<<40, 1<<40, 1<<40
			e.in.mu.Unlock()
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"}]}`)
			write := func(first, count int, cell string) {
				row := "[" + strings.TrimSuffix(strings.Repeat(cell+",", 100), ",") + "]"
				for at := first; at < first+count; at += 100 {
					e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"T","at":"A%d","rows":[%s]}]}`, at, strings.TrimSuffix(strings.Repeat(row+",", 100), ",")))
				}
			}
			write(1, formulas, `"=ROW()"`)
			write(1+formulas, numbers, `7`)
			alice := e.person("Alice")
			if ack := alice.do(c.op(e)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			if alice.spilled(0) != nil {
				t.Fatal("the entry went to a scratch file")
			}
			e.b.importAll(t)
			e.b.forgetRevisions()
			e.tick()
			sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
			_, stacks := e.residentUndo()
			held := sheetHeap()
			dropUndo(e, alice)
			e.tick()
			kept := held - sheetHeap()
			t.Logf("undo counts %d KiB in memory and kept %d KiB of the heap", stacks>>10, kept>>10)
			if measures && kept > stacks+1<<20 {
				t.Errorf("undo kept %d KiB of the heap and counts %d KiB", kept>>10, stacks>>10)
			}
		})
	}
}

// TestRefusedDeletionKeepsItsFormulasLive has Alice delete a row, a column
// and a tab of formulas while the undo scratch storage fails. Each entry is
// too large for memory, so the deletion is refused when its payload cannot
// be written: after its undo entry was made, which cut the saved records
// loose from the dependency index. The records come back with the refusal:
// a deleted row or tab gives back the records it saved, still loose, and a
// deleted column the copies its journal kept. They are in the index again
// after the next recalculation, and their formulas, and the formulas that
// read them, follow later edits.
func TestRefusedDeletionKeepsItsFormulasLive(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1+1","=SUM(B1:B2)"],["=B1*2",3,"=T!A1"]]},{"op":"write","tab":"T","at":"A1","rows":[["=Sheet1!A1*10",1]]}]}`)
	alice := e.person("Alice")
	writes := 0
	e.in.mu.Lock()
	p := e.in.undoPool()
	p.entryMax = 1
	p.fault = func(op string) error {
		if op == "write" {
			writes++
			return errors.New("no space left on device")
		}
		return nil
	}
	e.in.mu.Unlock()
	// loose reports whether the formula record at an address has no node
	// in the dependency index.
	loose := func(addr string) bool {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		tab, row, col := e.at(addr)
		c := tab.cell(row, col)
		return c != nil && c.f != nil && c.node == nil
	}
	for i, c := range []struct {
		op    string
		saved string
		same  bool // the refusal gives back the saved records themselves
	}{
		{e.deleteRowsOp("Sheet1", 1, 1), "Sheet1!B1", true},
		{e.deleteColsOp("Sheet1", 2, 1), "Sheet1!B1", false},
		{jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")), "T!A1", true},
	} {
		if loose(c.saved) {
			t.Fatalf("%s has no node before %s", c.saved, c.op)
		}
		seq, before, wrote := e.seq(), e.dump(), writes
		_, err := alice.try(seq, c.op)
		if werr := refusal(err); err == nil || werr.Reason != "undo_scratch" || e.seq() != seq {
			t.Fatalf("%s without scratch storage: %v", c.op, err)
		}
		// The payload was written, so the entry had been made.
		if writes == wrote {
			t.Fatalf("the refused %s wrote no undo payload", c.op)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused %s changed the sheet: %s", c.op, dumpDiff(before, after))
		}
		if c.same && !loose(c.saved) {
			t.Fatalf("the refused %s gave back %s with its node", c.op, c.saved)
		}
		// An agent's action has no undo entry, and its recalculation
		// builds the index again with the records that came back.
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"E9","rows":[[%d]]}]}`, i))
		if loose(c.saved) {
			t.Fatalf("%s has no node after the index was built again", c.saved)
		}
	}
	e.in.mu.Lock()
	p.fault = nil
	e.in.mu.Unlock()
	if ack := alice.set("A1", "5"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	for addr, shown := range map[string]string{"B1": "6", "A2": "12", "C1": "9", "C2": "50", "T!A1": "50"} {
		e.want(addr, shown)
	}
}
