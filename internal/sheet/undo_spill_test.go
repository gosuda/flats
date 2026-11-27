package sheet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// scratch returns the scratch files of the instance's undo payloads and
// their bytes.
func (e *env) scratch() (files int, bytes int64) {
	e.t.Helper()
	e.in.mu.RLock()
	dir := e.in.scratch.dir
	e.in.mu.RUnlock()
	if dir == "" {
		return 0, 0
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, f := range list {
		if isBlankFile(f.Name()) {
			continue
		}
		info, err := f.Info()
		if err != nil {
			e.t.Fatal(err)
		}
		files++
		bytes += info.Size()
	}
	return files, bytes
}

// residentUndo returns the bytes the undo entries of the instance take in
// memory, their payloads and what the payloads in scratch files leave
// there: what the pool counts, and what the stacks hold, counted here.
func (e *env) residentUndo() (pool, stacks int64) {
	m := e.undoMemory()
	return m.pool, m.stacks
}

// spilled returns the scratch file of the i-th entry of a person's undo
// stack, or nil when the entry's payload is in memory.
func (p *person) spilled(i int) *undoFile {
	p.e.in.mu.RLock()
	defer p.e.in.mu.RUnlock()
	if i >= len(p.s.undo.undo) {
		return nil
	}
	return p.s.undo.undo[i].file
}

// editLog returns the segments of the scratch log of structural edits and
// the edits that wait in memory for the next segment.
func (e *env) editLog() (segments, open int) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return len(e.in.scratch.edits.segs), len(e.in.scratch.edits.open)
}

// tinySegments makes every structural action close a segment of the edit
// log.
func (e *env) tinySegments() {
	e.in.mu.Lock()
	e.in.undoPool().edits.segment = 1
	e.in.mu.Unlock()
}

func (e *env) fillOf(addr string) bool {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	c := t.cell(row, col)
	return c != nil && e.in.wb.styles.format(c.style).Fill != 0
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// wantHeap checks the growth of the heap over actions whose undo payloads
// hold the given bytes. The live sheet keeps one generation of the texts,
// and the instance keeps up to 16 MiB of recent deltas for connections that
// reconnect, whatever undo holds. Payloads kept in memory would show as
// growth near their size, so the check needs payloads far above that floor.
func wantHeap(t *testing.T, grown, payloads int64) {
	t.Helper()
	const floor = 64 << 20
	if payloads < 4*floor {
		return
	}
	if limit := floor + payloads/8; grown > limit {
		t.Fatalf("the heap grew by %d MiB over %d MiB of undo payloads; want under %d MiB", grown>>20, payloads>>20, limit>>20)
	}
}

// longText returns a text of n characters that no other call returns.
func longText(n, salt int) string {
	head := fmt.Sprintf("%08d|", salt)
	return head + strings.Repeat("x", n-len(head))
}

// writeTexts builds a write that replaces rows x cols cells from A1 with
// texts of n characters.
func (e *env) writeTexts(tab string, rows, cols, n, salt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[`, e.tabID(tab), e.rowsOf(tab, 1, rows), e.colsOf(tab, 1, cols))
	for r := 0; r < rows; r++ {
		if r > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('[')
		for c := 0; c < cols; c++ {
			if c > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "%q", longText(n, salt*1000+r*cols+c))
		}
		sb.WriteByte(']')
	}
	sb.WriteString("]}")
	return sb.String()
}

// The undo cases of the other files hold when every payload goes through a
// scratch file.
func TestUndoThroughScratchFiles(t *testing.T) {
	undoSpillAll = true
	defer func() { undoSpillAll = false }()
	for name, test := range map[string]func(*testing.T){
		"insert": TestStructureUndoInsert, "sort": TestStructureUndoSort, "delete": TestStructureUndoDelete,
		"recovery": TestStructureRecovery, "mark": TestUndoOtherWriterMark, "people": TestPeopleOperations,
		"formats": TestUndoDeleteKeepsFormats, "rename": TestUndoDeleteRepairsAfterRename, "runs": TestUndoDeleteSeparateRuns,
		"groups": TestUndoRepeatedTabGroups, "order": TestUndoTabOrderWithOtherWriters, "names": TestRestoredTabNameStaysValid,
		"sorts": TestUndoRepeatedSorts, "other sorts": TestUndoSortKeepsOtherWritersSort, "place": TestUndoDeletePlace,
		"later deletions": TestUndoDeleteKeepsLaterDeletedReferences,
	} {
		t.Run(name, test)
	}
	// The run above must have used the files.
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", "one")
	if files, _ := e.scratch(); files != 1 {
		t.Fatalf("%d scratch files after one action", files)
	}
}

// A hundred large replacements stay undoable while their payloads take
// almost no memory.
func TestUndoLargeReplacements(t *testing.T) {
	run := func(t *testing.T, actions, cells, chars int) {
		e := start(t)
		alice := e.person("Alice")
		alice.do(e.writeTexts("Sheet1", cells/10, 10, chars, 0))
		before := heapInUse()
		for i := 1; i <= actions; i++ {
			if ack := alice.do(e.writeTexts("Sheet1", cells/10, 10, chars, i)); status(ack) != "applied" {
				t.Fatalf("replacement %d: %s", i, status(ack))
			}
		}
		grown := int64(heapInUse()) - int64(before)
		replaced := int64(actions) * int64(cells) * int64(chars)
		files, bytes := e.scratch()
		pool, stacks := e.residentUndo()
		t.Logf("%d replacements of %d MiB each: heap grew by %d MiB, %d scratch files of %d MiB, %d KiB of payloads in memory",
			actions, int64(cells)*int64(chars)>>20, grown>>20, files, bytes>>20, stacks>>10)
		if pool != stacks || stacks > undoSessionBytes {
			t.Fatalf("the pool counts %d bytes in memory and the stacks hold %d, over the bound of %d", pool, stacks, undoSessionBytes)
		}
		if bytes < replaced {
			t.Fatalf("the scratch files hold %d bytes of %d replaced", bytes, replaced)
		}
		wantHeap(t, grown, replaced)
		for i := actions; i >= 1; i-- {
			if u := undo(t, alice); u.Applied != cells || u.Skipped != 0 {
				t.Fatalf("undo %d = %+v", i, u)
			}
			if got, want := e.shown("A1"), longText(chars, (i-1)*1000); got != want {
				t.Fatalf("after undo %d A1 starts %q, want %q", i, got[:16], want[:16])
			}
		}
		if got := e.shown("J" + fmt.Sprint(cells/10)); got != longText(chars, cells-1) {
			t.Fatalf("the last cell starts %q", got[:16])
		}
		// Every undone entry left its file; the redo entries have theirs.
		if files, _ := e.scratch(); files != actions {
			t.Fatalf("%d scratch files while %d redo entries are kept", files, actions)
		}
		alice.set("L1", "new edit")
		if files, _ := e.scratch(); files != 0 {
			t.Fatalf("%d scratch files after the redo stack was cleared", files)
		}
	}
	t.Run("100 replacements of 150 cells of 32,000 characters", func(t *testing.T) {
		full(t, "writes 480 MiB of undo payloads")
		run(t, 100, 150, 32000)
	})
	t.Run("12 replacements of 150 cells of 12,000 characters", func(t *testing.T) { run(t, 12, 150, 12000) })
}

// Deleted tabs full of text are held in scratch files, with their formats,
// formulas and marks, and come back whole.
func TestUndoLargeTabDeletions(t *testing.T) {
	run := func(t *testing.T, tabs, cells, chars int) {
		e := start(t)
		alice, bob := e.person("Alice"), e.person("Bob")
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5]]}]}`)
		before := heapInUse()
		for i := 1; i <= tabs; i++ {
			name := fmt.Sprint("Text_", i)
			bob.do(jsonf(`{"op":"add_tab","name":%q}`, name))
			bob.do(e.writeTexts(name, cells/10, 10, chars, i))
			bob.do(e.setOp(name+"!L1", "=LEN(A1)+Sheet1!A1"), e.formatOp(name+"!A1", `{"bold":true}`))
			bob.do(e.insertRowsOp(name, 2, 1))
			// Bob's own history would hold the texts too.
			bob.s.undo.drop()
			alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(name)))
		}
		grown := int64(heapInUse()) - int64(before)
		deleted := int64(tabs) * int64(cells) * int64(chars)
		files, bytes := e.scratch()
		_, stacks := e.residentUndo()
		t.Logf("%d deleted tabs of %d MiB each: heap grew by %d MiB, %d scratch files of %d MiB, %d KiB of payloads in memory",
			tabs, int64(cells)*int64(chars)>>20, grown>>20, files, bytes>>20, stacks>>10)
		if files != tabs || bytes < deleted || stacks > undoSessionBytes {
			t.Fatalf("%d scratch files of %d bytes for %d deleted bytes, %d bytes in memory", files, bytes, deleted, stacks)
		}
		wantHeap(t, grown, deleted)
		// A rename and structure in the tab the formulas read reach the
		// payloads in their files.
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Base"}]}`)
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Base","at":1,"count":2}]}`)
		for i := tabs; i >= 1; i-- {
			if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
				t.Fatalf("undo %d = %+v", i, u)
			}
			name := fmt.Sprint("Text_", i)
			if got := e.shown(name + "!A1"); got != longText(chars, i*1000) {
				t.Fatalf("%s!A1 starts %q", name, got[:16])
			}
			e.wantText(name+"!L1", "=LEN(A1)+Base!A3")
			e.want(name+"!L1", fmt.Sprint(chars+5))
			if got := e.formatOf(name, "A1"); got != `{"bold":true}` {
				t.Fatalf("%s!A1 has the format %s", name, got)
			}
			if files, _ := e.scratch(); files != i-1 {
				t.Fatalf("%d scratch files while %d deletions are kept", files, i-1)
			}
			if i > 1 {
				// The sheet's text limit has no room for every tab at once.
				e.mustAgent(jsonf(`{"ops":[{"op":"delete_tab","tab":%q}]}`, name))
			}
		}
		e.restart()
		e.want("Text_1!L1", fmt.Sprint(chars+5))
		if got := e.formatOf("Text_1", "A1"); got != `{"bold":true}` {
			t.Fatalf("Text_1!A1 has the format %s after a restart", got)
		}
	}
	t.Run("70 tabs of 4 MiB", func(t *testing.T) {
		full(t, "writes 290 MiB of undo payloads")
		run(t, 70, 140, 30000)
	})
	t.Run("4 tabs of 2 MiB", func(t *testing.T) { run(t, 4, 100, 20000) })
}

// Payloads under the size of a scratch file still leave memory once a
// session, or the sheet, holds more than its share.
func TestUndoMemoryBudgets(t *testing.T) {
	const actions, cells, chars = 40, 20, 12000 // 240 KiB a payload
	t.Run("one session", func(t *testing.T) {
		e := start(t)
		alice := e.person("Alice")
		for i := 0; i <= actions; i++ {
			alice.do(e.writeTexts("Sheet1", 2, 10, chars, i))
			if pool, stacks := e.residentUndo(); pool != stacks || stacks > undoSessionBytes {
				t.Fatalf("after %d actions the pool counts %d bytes and the stacks hold %d", i+1, pool, stacks)
			}
		}
		if files, _ := e.scratch(); files == 0 || files > actions {
			t.Fatalf("%d scratch files", files)
		}
		for i := actions; i >= 1; i-- {
			if u := undo(t, alice); u.Applied != cells {
				t.Fatalf("undo %d = %+v", i, u)
			}
		}
		if got := e.shown("A1"); got != longText(chars, 0) {
			t.Fatalf("A1 starts %q", got[:16])
		}
	})
	t.Run("all sessions of a sheet", func(t *testing.T) {
		e := start(t)
		e.in.mu.Lock()
		e.in.undoPool().instanceMax = 2 << 20
		e.in.mu.Unlock()
		people := make([]*person, 12)
		for i := range people {
			people[i] = e.person(fmt.Sprint("P", i))
		}
		for round := 0; round < 4; round++ {
			for i, p := range people {
				p.do(e.writeTexts("Sheet1", 2, 10, chars, round*100+i))
				if pool, stacks := e.residentUndo(); pool != stacks || stacks > 2<<20 {
					t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
				}
			}
		}
		// Each session undoes its own writes; the last writer's apply.
		last := people[len(people)-1]
		if u := undo(t, last); u.Applied != cells {
			t.Fatalf("undo = %+v", u)
		}
		if got := e.shown("A1"); got != longText(chars, (300+len(people)-2)*1000) {
			t.Fatalf("A1 starts %q", got[:16])
		}
	})
	// The notes an undo adds count in the sheet's memory beside the
	// payloads of every session. When the session that undoes has no
	// payload left in memory, the largest of any session goes to its file,
	// and nothing expires.
	t.Run("notes beside the payloads of another session", func(t *testing.T) {
		const sheetMax, rows = 560 << 10, 16000
		e := start(t)
		e.in.mu.Lock()
		e.in.undoPool().instanceMax = sheetMax
		e.in.mu.Unlock()
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, rows))
		alice, bob := e.person("Alice"), e.person("Bob")
		// Alice's second and third write each hold 240 KiB of text that
		// they replaced, in memory.
		for i := 0; i < 3; i++ {
			alice.do(e.writeTexts("Sheet1", 2, 10, chars, i))
		}
		if alice.spilled(1) != nil || alice.spilled(2) != nil {
			t.Fatal("Alice's payloads left memory while the sheet held less than its share")
		}
		// Bob formats every other row of one column twice and undoes the
		// second format: a note of 16 bytes for each of 8,000 cells.
		tab := e.tabID("Sheet1")
		e.in.mu.RLock()
		var runs []string
		for pos := 0; pos < rows; pos += 2 {
			runs = append(runs, fmt.Sprintf("[%d,1]", e.in.wb.tabs[0].rows.ids[pos]))
		}
		e.in.mu.RUnlock()
		op := func(set string) string {
			return jsonf(`{"op":"format","tab":%d,"rows":[%s],"cols":%s,"set":%s}`, tab, strings.Join(runs, ","), e.colsOf("Sheet1", 20, 1), set)
		}
		for _, set := range []string{`{"bold":true}`, `{"bold":null}`} {
			if ack := bob.do(op(set)); status(ack) != "applied" {
				t.Fatalf("format: %s", status(ack))
			}
		}
		wantUndo(t, "undo of the reset", undo(t, bob), undoAck{Applied: rows / 2})
		notes := bob.noteCounts()
		pool, stacks := e.residentUndo()
		if notes.session < rows/2*noteRunBytes || notes.sheet != notes.measured || pool != stacks {
			t.Fatalf("notes: %+v; the pool counts %d bytes and the stacks hold %d", notes, pool, stacks)
		}
		if pool+notes.sheet > sheetMax {
			t.Fatalf("the sheet holds %d bytes of payloads and %d of notes, its share is %d", pool, notes.sheet, sheetMax)
		}
		if alice.spilled(1) == nil && alice.spilled(2) == nil {
			t.Fatal("neither of Alice's payloads went to a scratch file")
		}
		if alice.lost(1) || alice.lost(2) || bob.lost(0) {
			t.Fatal("an entry expired where a payload could go to a scratch file")
		}
		for i := 2; i >= 1; i-- {
			if u := undo(t, alice); u.Applied != cells {
				t.Fatalf("Alice's undo %d = %+v", i, u)
			}
		}
		if got := e.shown("A1"); got != longText(chars, 0) {
			t.Fatalf("A1 starts %q", got[:16])
		}
		wantUndo(t, "Bob's undo of the format", undo(t, bob), undoAck{Applied: rows / 2})
	})
}

// Scratch files go when their entries leave the stacks, when the session
// ends, on a rollback and when the instance stops.
func TestUndoScratchCleanup(t *testing.T) {
	setup := func(t *testing.T) (*env, *person) {
		e := start(t)
		alice := e.person("Alice")
		// The first write replaces blank cells and has a small payload.
		for i := 0; i <= 3; i++ {
			alice.do(e.writeTexts("Sheet1", 15, 10, 12000, i))
		}
		if files, _ := e.scratch(); files != 3 {
			t.Fatalf("%d scratch files after three large actions", files)
		}
		return e, alice
	}
	t.Run("entries pushed off the stack", func(t *testing.T) {
		e, alice := setup(t)
		for i := 0; i < maxUndoEntries; i++ {
			alice.set("N1", fmt.Sprint(i))
		}
		if files, _ := e.scratch(); files != 0 {
			t.Fatalf("%d scratch files after the entries left the stack", files)
		}
		if pool, stacks := e.residentUndo(); pool != stacks {
			t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
		}
	})
	t.Run("an undone entry and a cleared redo stack", func(t *testing.T) {
		e, alice := setup(t)
		undo(t, alice)
		// The consumed entry's file is gone and the redo entry has one.
		if files, _ := e.scratch(); files != 3 {
			t.Fatalf("%d scratch files after one undo", files)
		}
		alice.set("N1", "new")
		if files, _ := e.scratch(); files != 2 {
			t.Fatalf("%d scratch files after the redo stack was cleared", files)
		}
	})
	t.Run("a session that stays away", func(t *testing.T) {
		e, alice := setup(t)
		e.in.smu.Lock()
		alice.s.left = e.clock.Now()
		e.in.smu.Unlock()
		e.clock.Add(undoDisconnected + time.Minute)
		e.tick()
		if files, _ := e.scratch(); files != 0 {
			t.Fatalf("%d scratch files after the session's history was dropped", files)
		}
		if pool, _ := e.residentUndo(); pool != 0 {
			t.Fatalf("the pool still counts %d bytes", pool)
		}
	})
	t.Run("a rollback", func(t *testing.T) {
		e, alice := setup(t)
		alice.set("N1", "small")
		if _, err := e.b.Rollback(context.Background(), e.b.importAll(t), "Alice"); err != nil {
			t.Fatal(err)
		}
		// The rollback lets the files and the bytes go itself, and a tick
		// after it finds nothing left.
		for _, when := range []string{"right after a rollback", "after the tick that follows"} {
			if files, _ := e.scratch(); files != 0 {
				t.Fatalf("%d scratch files %s", files, when)
			}
			if pool, stacks := e.residentUndo(); pool != 0 || stacks != 0 {
				t.Fatalf("%s the pool counts %d bytes and the stacks hold %d", when, pool, stacks)
			}
			e.tick()
		}
	})
	t.Run("the instance stops", func(t *testing.T) {
		e, _ := setup(t)
		dir := e.in.scratch.dir
		e.in.Stop()
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the scratch directory is still there: %v", err)
		}
	})
	t.Run("the instance stops when it is idle", func(t *testing.T) {
		e, _ := setup(t)
		dir := e.in.scratch.dir
		// The edit group closes and the host takes its revision; nothing
		// uses the instance then.
		closeRevision(e)
		if !e.in.StopIfIdle(0) {
			t.Fatal("the idle instance did not stop")
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the scratch directory is still there: %v", err)
		}
	})
	t.Run("a failed commit", func(t *testing.T) {
		e, alice := setup(t)
		e.in.fault = func() error { return errors.New("the disk is gone") }
		if _, err := alice.try(e.seq(), e.writeTexts("Sheet1", 15, 10, 12000, 9)); err == nil {
			t.Fatal("the action committed")
		}
		e.in.fault = nil
		// The writer forgets what the failed action admitted as soon as its
		// commit fails.
		if files, _ := e.scratch(); files != 3 {
			t.Fatalf("%d scratch files right after a failed commit, want none of the failed action", files)
		}
		if pool, stacks := e.residentUndo(); pool != stacks {
			t.Fatalf("after the failed commit the pool counts %d bytes and the stacks hold %d", pool, stacks)
		}
		e.person("Bob").set("N1", "next")
		if files, _ := e.scratch(); files != 3 {
			t.Fatalf("%d scratch files after the action that followed a failed commit", files)
		}
		if pool, stacks := e.residentUndo(); pool != stacks {
			t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
		}
		e.in.fault = func() error { return errors.New("the disk is gone") }
		if _, err := alice.try(e.seq(), e.writeTexts("Sheet1", 15, 10, 12000, 9)); err == nil {
			t.Fatal("the action committed")
		}
		e.in.fault = nil
		e.tick()
		if files, _ := e.scratch(); files != 3 {
			t.Fatalf("%d scratch files after a failed commit and a tick", files)
		}
		if pool, stacks := e.residentUndo(); pool != stacks {
			t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
		}
		// The failed action left nothing: the same edit applies afterwards.
		if ack := alice.do(e.writeTexts("Sheet1", 15, 10, 12000, 9)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		for range 2 {
			if u := undo(t, alice); u.Applied != 150 {
				t.Fatalf("undo = %+v", u)
			}
		}
	})
	t.Run("the host's directory", func(t *testing.T) {
		dir := t.TempDir()
		e := start(t, func(o *Options) { o.ScratchDir = dir })
		alice := e.person("Alice")
		alice.do(e.writeTexts("Sheet1", 15, 10, 12000, 1))
		alice.do(e.writeTexts("Sheet1", 15, 10, 12000, 2))
		if got := filepath.Dir(e.in.scratch.dir); got != dir {
			t.Fatalf("the scratch files are under %s", got)
		}
		e.in.Stop()
		if list, _ := os.ReadDir(dir); len(list) != 0 {
			t.Fatalf("%d entries left in the host's scratch directory", len(list))
		}
	})
}

// An action whose payload cannot be written is refused whole and can be
// sent again. A payload that cannot be read back is skipped with its own
// reason, and the entries under it stay reachable.
func TestUndoScratchFailures(t *testing.T) {
	large := func(e *env, salt int) string { return e.writeTexts("Sheet1", 15, 10, 12000, salt) }
	// The sheet starts with texts another person wrote, so that every large
	// action of Alice replaces 1.8 MiB.
	setup := func(t *testing.T) (*env, *person) {
		e := start(t)
		bob := e.person("Bob")
		bob.do(large(e, 0))
		bob.s.undo.drop()
		alice := e.person("Alice")
		alice.set("N9", "first")
		return e, alice
	}
	first := longText(12000, 0)
	t.Run("the scratch file cannot be written", func(t *testing.T) {
		e, alice := setup(t)
		failing := true
		e.in.mu.Lock()
		e.in.undoPool().fault = func(op string) error {
			if failing && op == "write" {
				return errors.New("no space left on device")
			}
			return nil
		}
		e.in.mu.Unlock()
		seq := e.seq()
		_, err := alice.try(seq, large(e, 1))
		werr := refusal(err)
		if err == nil || werr.Code != "unavailable" || werr.Reason != "undo_scratch" || !werr.Retry {
			t.Fatalf("the action = %v, want an unavailable refusal that can be retried", err)
		}
		if e.seq() != seq || e.shown("A1") != first {
			t.Fatalf("a refused action changed the sheet: sequence %d, A1 %q", e.seq(), e.shown("A1")[:16])
		}
		if files, _ := e.scratch(); files != 0 {
			t.Fatalf("%d scratch files after a refused action", files)
		}
		// An action with a small payload needs no file and applies meanwhile.
		e.person("Carol").set("N1", "small")
		// The same action, sent again once the storage is back, applies.
		failing = false
		if ack, err := alice.send(alice.n, seq, large(e, 1)); err != nil || status(ack) != "applied" {
			t.Fatalf("the action sent again = %v %s", err, status(ack))
		}
		if files, _ := e.scratch(); files != 1 {
			t.Fatalf("%d scratch files after the action applied", files)
		}
		e.restart()
		if got := e.shown("A1"); got != longText(12000, 1000) {
			t.Fatalf("A1 starts %q after a restart", got[:16])
		}
	})
	t.Run("the scratch file is missing at the undo", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(large(e, 1))
		alice.do(large(e, 2))
		lost := alice.spilled(2)
		if lost == nil {
			t.Fatal("the second replacement was not spilled")
		}
		if err := os.Remove(lost.path); err != nil {
			t.Fatal(err)
		}
		seq := e.seq()
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || u.Reason != "lost" {
			t.Fatalf("undo of the lost payload = %+v", u)
		}
		if e.seq() != seq || e.shown("A1") != longText(12000, 2000) {
			t.Fatalf("a skipped undo changed the sheet")
		}
		// The entry is gone from the stack, and the one under it applies
		// only where its stamp still stands: the lost action's cells stay.
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 150 || u.Reason != "changed" {
			t.Fatalf("undo under the lost payload = %+v", u)
		}
		if u := undo(t, alice); u.Applied != 1 || e.shown("N9") != "" {
			t.Fatalf("undo of the first edit = %+v", u)
		}
		if u := alice.do(`{"op":"undo"}`).Undo; u == nil || u.Applied != 0 || u.Skipped != 0 {
			t.Fatalf("undo on an empty stack = %+v", u)
		}
	})
	t.Run("an entry under a missing one", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(large(e, 1))
		tab, row, col := e.ids("N20")
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"CZ200","rows":[[1]]}]}`)
		alice.do(jsonf(`{"op":"format","tab":%d,"rows":[[%d,100]],"cols":[[%d,60]],"set":{"fill":"red-600"}}`, tab, row, col))
		alice.set("M1", "later")
		lost := alice.spilled(2)
		if lost == nil {
			t.Fatal("the format of 6,000 cells was not spilled")
		}
		if err := os.Remove(lost.path); err != nil {
			t.Fatal(err)
		}
		for i, want := range []undoAck{{Applied: 1}, {Skipped: 1, Reason: "lost"}, {Applied: 150}, {Applied: 1}} {
			if u := undo(t, alice); *u != want {
				t.Fatalf("undo %d = %+v, want %+v", i+1, *u, want)
			}
		}
		if e.shown("A1") != first || e.shown("M1") != "" || e.shown("N9") != "" {
			t.Fatalf("A1 starts %q, M1 shows %q, N9 shows %q", e.shown("A1")[:16], e.shown("M1"), e.shown("N9"))
		}
		// The format whose payload was lost stays.
		if !e.fillOf("N20") {
			t.Fatal("N20 lost its fill")
		}
	})
	t.Run("the scratch file is damaged", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(large(e, 1))
		file := alice.spilled(1)
		if file == nil {
			t.Fatal("the replacement was not spilled")
		}
		path := file.path
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)/2] ^= 0x40
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || u.Reason != "lost" {
			t.Fatalf("undo of the damaged payload = %+v", u)
		}
		if files, _ := e.scratch(); files != 0 {
			t.Fatalf("%d scratch files after the payload was given up", files)
		}
		if got := e.shown("A1"); got != longText(12000, 1000) {
			t.Fatalf("A1 starts %q", got[:16])
		}
	})
	t.Run("the scratch file cannot be read now", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(large(e, 1))
		failing := true
		e.in.mu.Lock()
		e.in.undoPool().fault = func(op string) error {
			if failing && op == "read" {
				return errors.New("input/output error")
			}
			return nil
		}
		e.in.mu.Unlock()
		seq := e.seq()
		_, err := alice.try(seq, `{"op":"undo"}`)
		if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "undo_scratch" || !werr.Retry {
			t.Fatalf("the undo = %v, want an unavailable refusal that can be retried", err)
		}
		failing = false
		if u := undo(t, alice); u.Applied != 150 || e.shown("A1") != first {
			t.Fatalf("the undo sent again = %+v, A1 %q", u, e.shown("A1")[:16])
		}
	})
}

// A deletion whose payload lies in a scratch file follows the structure
// that changes while it waits, through many edits.
func TestUndoScratchFollowsStructure(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Summary"},
		{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3,"=A1+A5+Summary!A1"],[4],[5]]},
		{"op":"write","tab":"Summary","at":"A1","rows":[[100,"=Data!A3+Data!A4"]]}]}`)
	alice, bob := e.person("Alice"), e.person("Bob")
	alice.do(e.setOp("Data!D3", longText(12000, 1)))
	e.in.mu.Lock()
	e.in.undoPool().entryMax = 1 << 10
	e.in.mu.Unlock()
	alice.do(e.deleteRowsOp("Data", 3, 1))
	first := alice.spilled(1)
	if first == nil {
		t.Fatal("the deletion was not spilled")
	}
	// Bob renames both tabs and inserts 80 rows above the deleted one and 80
	// in the other tab, one action each. None of them reads or writes the
	// payload: the edits go to the log, a segment for each action here.
	e.tinySegments()
	touched := 0
	e.in.mu.Lock()
	e.in.undoPool().fault = func(op string) error {
		if op == "read" || op == "write" {
			touched++
		}
		return nil
	}
	e.in.mu.Unlock()
	bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Info"}`, e.tabID("Data")))
	bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Totals"}`, e.tabID("Summary")))
	for i := 0; i < 80; i++ {
		bob.do(e.insertRowsOp("Info", 1, 1))
		bob.do(e.insertRowsOp("Totals", 1, 1))
	}
	if second := alice.spilled(1); second == nil || second.path != first.path || touched != 0 {
		t.Fatalf("structural edits touched the payload %d times; its file is %v, was %v", touched, second, first)
	}
	if segments, open := e.editLog(); segments != 162 || open != 0 {
		t.Fatalf("the edit log has %d segments and %d open edits, want 162 and 0", segments, open)
	}
	if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
		t.Fatalf("undo = %+v", u)
	}
	// With the entry gone no payload follows edits, and the log goes.
	e.tick()
	if segments, open := e.editLog(); segments != 0 || open != 0 {
		t.Fatalf("the edit log keeps %d segments and %d open edits for no payload", segments, open)
	}
	if files, _ := e.scratch(); files != 0 {
		t.Fatalf("%d scratch files with no spilled entry", files)
	}
	e.want("Info!A83", "3")
	e.wantText("Info!B83", "=A81+A85+Totals!A81")
	e.want("Info!B83", "106")
	if got := e.shown("Info!D83"); got != longText(12000, 1) {
		t.Fatalf("Info!D83 starts %q", got[:16])
	}
	// The formula in the other tab was changed by the deletion. The rows
	// inserted since stop its repair, and the insert of the row moves it.
	e.wantText("Totals!B81", "=#REF!+Info!A84")
	if got := e.column("Info", "A", 81, 85); !slices.Equal(got, []string{"1", "2", "3", "4", "5"}) {
		t.Fatalf("rows = %v", got)
	}
}

// TestUndoScratchRefusesWhatItCannotWrite hands the scratch storage an
// entry with a format that has no written form. No cell can hold one, as
// every entry of a format into the sheet checks it. The write fails and
// leaves no file; it does not store another format in its place.
func TestUndoScratchRefusesWhatItCannotWrite(t *testing.T) {
	e := start(t)
	pool := e.in.undoPool()
	pool.root = t.TempDir()
	bad := formula.Format{Bold: true, Number: formula.NumberFormat{Kind: formula.NumberCurrency, Symbol: "1"}}
	if _, err := appendFormat(nil, bad); err == nil {
		t.Fatal("the format has a written form")
	}
	file, err := pool.write(&undoEntry{styles: map[uint32]styleState{1: {f: bad}}})
	if err == nil || file != nil {
		t.Fatalf("the write = %v, %v", file, err)
	}
	if files, _ := e.scratch(); files != 0 {
		t.Fatalf("%d scratch files after the refused write", files)
	}
	// What a person types sets only formats that have one.
	alice := e.person("Alice")
	for i, text := range []string{"$1,234.50", "12%", "2026-03-15", "14:30", "3/15/2026 2:30 PM", "1,000", "€5", "1e3"} {
		alice.set(fmt.Sprintf("A%d", i+1), text)
	}
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	if len(e.in.wb.styles.byID) < 4 {
		t.Fatalf("the typed entries set %d formats", len(e.in.wb.styles.byID))
	}
	for id, entry := range e.in.wb.styles.byID {
		if _, err := appendFormat(nil, entry.f); err != nil {
			t.Errorf("style %d cannot be written: %v", id, err)
		}
	}
}

// TestUndoScratchErrorWhileFollowing fails the scratch storage while a
// spilled deletion has to follow structural edits. An error that passes
// keeps the payload: the edits wait, and the undo gives the row back at the
// right place once the storage answers again. A file that is gone or
// damaged gives the payload up, and so do edits the payload waits for that
// cannot be kept: because the storage stays away, or because they pass the
// bound of the edit log.
func TestUndoScratchErrorWhileFollowing(t *testing.T) {
	setup := func(t *testing.T) (e *env, alice, bob *person) {
		e = start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2],[3,"=A1+A5"],[4],[5]]}]}`)
		alice, bob = e.person("Alice"), e.person("Bob")
		alice.do(e.setOp("D3", longText(12000, 1)))
		e.in.mu.Lock()
		e.in.undoPool().entryMax = 1 << 10
		e.in.mu.Unlock()
		alice.do(e.deleteRowsOp("Sheet1", 3, 1))
		if alice.spilled(1) == nil {
			t.Fatal("the deletion was not spilled")
		}
		return e, alice, bob
	}
	fail := func(e *env, op string, err error) (stop func()) {
		failing := true
		e.in.mu.Lock()
		e.in.undoPool().fault = func(got string) error {
			if failing && got == op {
				return err
			}
			return nil
		}
		e.in.mu.Unlock()
		return func() {
			e.in.mu.Lock()
			failing = false
			e.in.mu.Unlock()
		}
	}
	inserts := func(e *env, bob *person, n int) {
		for i := 0; i < n; i++ {
			bob.do(e.insertRowsOp("Sheet1", 1, 1))
		}
	}
	const edits = 84
	wantRestored := func(t *testing.T, e *env, alice *person) {
		t.Helper()
		if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
			t.Fatalf("undo = %+v", u)
		}
		row := edits + 3
		e.wantText(fmt.Sprintf("B%d", row), fmt.Sprintf("=A%d+A%d", row-2, row+2))
		if got := e.shown(fmt.Sprintf("D%d", row)); got != longText(12000, 1) {
			t.Fatalf("the restored text starts %q", got[:16])
		}
	}
	wantRefused := func(t *testing.T, e *env, alice *person) {
		t.Helper()
		seq := e.seq()
		_, err := alice.try(seq, `{"op":"undo"}`)
		if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "undo_scratch" || !werr.Retry {
			t.Fatalf("undo = %v, want an unavailable refusal that can be retried", err)
		}
		if e.seq() != seq {
			t.Fatal("a refused undo changed the sheet")
		}
	}
	// The undo reads the payload and the segments of the edit log. While
	// one of them cannot be read it is refused, and it applies afterwards.
	for _, op := range []string{"read", "edits-read"} {
		t.Run("a passing "+op+" error", func(t *testing.T) {
			e, alice, bob := setup(t)
			e.tinySegments()
			inserts(e, bob, edits)
			stop := fail(e, op, errors.New("input/output error"))
			wantRefused(t, e, alice)
			stop()
			wantRestored(t, e, alice)
		})
	}
	// Edits that cannot be written wait in memory and reach a segment once
	// the storage answers again.
	t.Run("a passing edits-write error", func(t *testing.T) {
		e, alice, bob := setup(t)
		e.in.mu.Lock()
		e.in.undoPool().edits.segment = 4 << 10
		e.in.mu.Unlock()
		stop := fail(e, "edits-write", errors.New("no space left on device"))
		inserts(e, bob, edits-1)
		if segments, open := e.editLog(); segments != 0 || open != edits-1 {
			t.Fatalf("while the storage fails the log has %d segments and %d open edits", segments, open)
		}
		stop()
		inserts(e, bob, 1)
		if segments, open := e.editLog(); segments != 1 || open != 0 {
			t.Fatalf("after the storage is back the log has %d segments and %d open edits", segments, open)
		}
		wantRestored(t, e, alice)
	})
	wantLost := func(t *testing.T, e *env, alice *person) {
		t.Helper()
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || u.Reason != "lost" {
			t.Fatalf("undo = %+v", u)
		}
		e.tick()
		if files, _ := e.scratch(); files != 0 {
			t.Fatalf("%d scratch files after the payload was given up", files)
		}
	}
	t.Run("an edits-write error that lasts", func(t *testing.T) {
		e, alice, bob := setup(t)
		e.tinySegments()
		fail(e, "edits-write", errors.New("no space left on device"))
		inserts(e, bob, edits)
		if _, open := e.editLog(); open != 0 {
			t.Fatalf("%d edits wait in memory for a storage that does not answer", open)
		}
		wantLost(t, e, alice)
	})
	// The bound of the edit log counts what payloads wait for. Edits of a
	// tab the payload does not depend on never reach the log, however many
	// they are, and the undo applies.
	t.Run("edits of another tab, more than the edit log holds", func(t *testing.T) {
		e, alice, bob := setup(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"}]}`)
		e.tinySegments()
		e.in.mu.Lock()
		e.in.undoPool().edits.max = 1 << 10
		e.in.mu.Unlock()
		for i := 0; i < edits; i++ {
			bob.do(e.insertRowsOp("Other", 1, 1))
		}
		if segments, open := e.editLog(); segments != 0 || open != 0 {
			t.Fatalf("the edit log holds %d segments and %d open edits of a tab no payload depends on", segments, open)
		}
		if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
			t.Fatalf("undo = %+v", u)
		}
		e.wantText("B3", "=A1+A5")
		if got := e.shown("D3"); got != longText(12000, 1) {
			t.Fatalf("the restored text starts %q", got[:16])
		}
	})
	// Edits of the payload's own tab are what it waits for. When they pass
	// the bound, the payload expires (spec rule 23) and the log lets its
	// edits go at once.
	t.Run("edits of its own tab, more than the edit log holds", func(t *testing.T) {
		e, alice, bob := setup(t)
		e.tinySegments()
		e.in.mu.Lock()
		e.in.undoPool().edits.max = 16 << 10
		e.in.mu.Unlock()
		inserts(e, bob, 4)
		if segments, _ := e.editLog(); segments != 4 {
			t.Fatalf("the edit log holds %d segments after 4 edits inside its bound", segments)
		}
		inserts(e, bob, edits-4)
		if segments, open := e.editLog(); segments != 0 || open != 0 {
			t.Fatalf("the edit log keeps %d segments and %d open edits past its bound of 16 KiB", segments, open)
		}
		wantLost(t, e, alice)
	})
	t.Run("the payload is gone", func(t *testing.T) {
		e, alice, bob := setup(t)
		if err := os.Remove(alice.spilled(1).path); err != nil {
			t.Fatal(err)
		}
		inserts(e, bob, edits)
		wantLost(t, e, alice)
	})
	for _, damage := range []string{"gone", "damaged"} {
		t.Run("a segment of the edit log is "+damage, func(t *testing.T) {
			e, alice, bob := setup(t)
			e.tinySegments()
			inserts(e, bob, edits)
			e.in.mu.RLock()
			path := e.in.scratch.edits.segs[edits/2].path
			e.in.mu.RUnlock()
			var err error
			if damage == "gone" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte(editMagic+"\x05"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantLost(t, e, alice)
		})
	}
}

// A deleted tab whose payload lies in a scratch file keeps the marks of the
// rows created in it, and the last sorts of its rows.
func TestUndoScratchKeepsMarks(t *testing.T) {
	setup := func(t *testing.T) (*env, *person, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[3],[1],[2]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.sortOp("Data", 1, 3, 1, false)) // 1 2 3
		alice.do(e.insertRowsOp("Data", 2, 1))     // 1 _ 2 3
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.mu.Unlock()
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
		if alice.spilled(2) == nil {
			t.Fatal("the deletion was not spilled")
		}
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo of the deletion = %+v", u)
		}
		return e, alice, bob
	}
	t.Run("the person's own rows and sort", func(t *testing.T) {
		e, alice, _ := setup(t)
		for i, want := range [][]string{{"1", "2", "3"}, {"3", "1", "2"}} {
			if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
				t.Fatalf("undo %d = %+v", i+1, u)
			}
			if got := e.column("Data", "A", 1, 3); !slices.Equal(got, want) {
				t.Fatalf("rows after undo %d = %v, want %v", i+1, got, want)
			}
		}
	})
	t.Run("another writer's cell in the created row", func(t *testing.T) {
		e, alice, bob := setup(t)
		bob.set("Data!B2", "bob")
		if u := undo(t, alice); u.Applied != 0 || u.Reason != "written" {
			t.Fatalf("undo of the insert = %+v", u)
		}
		e.want("Data!B2", "bob")
	})
}

// The formulas an entry holds count with what their parsed form takes, so
// entries full of long formulas go to scratch files like entries full of
// text: as values before an edit, in deleted rows, and as the earlier form
// of formulas a deletion changed.
func TestUndoFormulaPayloadsLeaveMemory(t *testing.T) {
	slow(t, "parses hundreds of formulas of 7,600 characters")
	// 8 rounds of 100 formulas at full size, 3 rounds of 30 by default:
	// 30 such formulas count as more than 5 MiB, so each entry still has
	// to leave memory, and three entries that stayed would pass the bound.
	// Under the race detector the entries still have to leave memory and
	// come back for the undo; only the heap is not measured.
	rounds, per := sized(8, 3), sized(100, 30)
	// A formula of 950 references, about 7,600 characters, that no other
	// call returns.
	long := func(salt int) string {
		var sb strings.Builder
		sb.WriteString("=")
		for i := 0; i < 950; i++ {
			fmt.Fprintf(&sb, "$Z$%d+", 902+(salt+i)%7)
		}
		fmt.Fprintf(&sb, "Z%d", 950+salt%40)
		return sb.String()
	}
	fill := func(e *env, round int) {
		rows := make([]string, per)
		for i := range rows {
			rows[i] = fmt.Sprintf("[%q]", long(round*100+i))
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
	}
	settled := func(e *env) int64 {
		// The table of shared formulas keeps those of earlier states until
		// it is rebuilt. That is not undo's memory.
		e.in.mu.Lock()
		e.in.wb.pruneFormulas()
		e.in.mu.Unlock()
		return int64(heapInUse())
	}
	for name, c := range map[string]struct {
		refill bool
		act    func(e *env) string
	}{
		"values before a clear":  {act: func(e *env) string { return e.clearOp("Sheet1", 1, per, 1, 1) }},
		"deleted rows":           {refill: true, act: func(e *env) string { return e.deleteRowsOp("Sheet1", 1, per) }},
		"formulas a row changed": {act: func(e *env) string { return e.deleteRowsOp("Sheet1", 901, 1) }},
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[[1]]}]}`)
			people := []*person{e.person("Alice"), e.person("Bob")}
			before := settled(e)
			for round := 1; round <= rounds; round++ {
				if c.refill && round > 1 {
					e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, per))
				}
				fill(e, round)
				p := people[round%2]
				if ack := p.do(c.act(e)); status(ack) != "applied" {
					t.Fatalf("round %d: %s", round, status(ack))
				}
				if p.spilled((round-1)/2) == nil {
					t.Errorf("round %d: an entry that holds %d long formulas stays in memory", round, per)
				}
			}
			if u := undo(t, people[rounds%2]); u.Applied == 0 || u.Skipped != 0 {
				t.Fatalf("undo of the last round = %+v", u)
			}
			// With the tab empty again, what the heap holds over the start
			// is what the entries keep.
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, per))
			grown := settled(e) - before
			pool, stacks := e.residentUndo()
			files, bytes := e.scratch()
			t.Logf("%d entries of %d long formulas: heap grew by %d KiB, %d scratch files of %d KiB, %d bytes of payloads in memory", rounds, per, grown>>10, files, bytes>>10, stacks)
			if pool != stacks || stacks > undoSessionBytes {
				t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
			}
			if measures && grown > 6<<20 {
				t.Fatalf("the heap grew by %d KiB over %d entries of %d long formulas, with %d bytes of payloads counted in memory", grown>>10, rounds, per, stacks)
			}
		})
	}
}

// A structural edit costs the spilled payloads nothing when it commits,
// however many they are and however large the edit: it is one more entry
// of the edit log, and each payload follows it at its own undo.
func TestUndoScratchEditCostsNoPayloadWork(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":39000}]}`)
	const entries = 20
	formulas := make([]string, entries)
	for i := range formulas {
		formulas[i] = `["=A39990+A50"]`
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[` + strings.Join(formulas, ",") + `]}]}`)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	alice, bob := e.person("Alice"), e.person("Bob")
	for i := 1; i <= entries; i++ {
		alice.do(e.setOp(fmt.Sprintf("B%d", i), "text"))
		if alice.spilled(i-1) == nil {
			t.Fatalf("entry %d was not spilled", i)
		}
	}
	// The deletion writes one payload, its own.
	touched := map[string]int{}
	e.in.mu.Lock()
	e.in.undoPool().fault = func(op string) error {
		touched[op]++
		return nil
	}
	e.in.mu.Unlock()
	if ack := bob.do(e.deleteRowsOp("Sheet1", 100, 35000)); status(ack) != "applied" {
		t.Fatalf("delete: %s", status(ack))
	}
	if segments, open := e.editLog(); len(touched) != 1 || touched["write"] != 1 || segments != 0 || open != 1 {
		t.Fatalf("the deletion touched scratch files: %v; the log has %d segments and %d open edits", touched, segments, open)
	}
	e.in.mu.Lock()
	e.in.undoPool().fault = nil
	e.in.mu.Unlock()
	for i := entries; i >= 1; i-- {
		wantUndo(t, fmt.Sprintf("undo %d", i), undo(t, alice), undoAck{Applied: 1})
		e.wantText(fmt.Sprintf("B%d", i), "=A4990+A50")
	}
}
