package sheet

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// undoMemory is what the undo memory of a sheet reads: the bytes the pool
// counts for the payloads and for what spilled payloads leave in memory,
// the same counted here from the entries on the stacks, the most a session
// holds with its notes, and the most tabs one spilled payload notes the
// first edit of.
type undoMemory struct {
	pool, stacks, session int64
	tabs, waiting, lost   int
}

func (e *env) undoMemory() undoMemory {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	e.in.smu.Lock()
	defer e.in.smu.Unlock()
	m := undoMemory{pool: e.in.scratch.resident}
	for _, s := range e.in.sessions {
		m.session = max(m.session, s.undo.resident())
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for _, entry := range stack {
				switch {
				case entry.lost:
					m.lost++
					m.stacks += entryBytes
				case entry.file == nil:
					m.stacks += entry.measure()
				default:
					m.stacks += entry.left()
					if w := entry.wait; w != nil {
						m.waiting++
						m.tabs = max(m.tabs, len(w.needs))
					}
				}
			}
		}
	}
	return m
}

// A spilled payload leaves a little in memory: what it depends on, and
// where the edits it waits for begin. That counts in the undo memory of
// its session and of the sheet, and it does not grow with the tabs that
// come and go while the payload waits.
func TestUndoWaitingStaysInBudget(t *testing.T) {
	// The formula of each entry names more tabs than an entry keeps the
	// names of, so the entry depends on every tab, and two tabs that are
	// there.
	text := longRefs(depsKept+2, func(i int) string { return fmt.Sprintf("None%d!A1", i) }) + "+Data!A5+Other!A5"
	// setup lets every person replace count formulas, one in each action,
	// with every payload in a scratch file.
	setup := func(t *testing.T, people, count int) (*env, []*person) {
		e := start(t)
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.mu.Unlock()
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"}]}`)
		row := make([]string, people)
		for i := range row {
			row[i] = fmt.Sprintf("%q", text)
		}
		rows := make([]string, count)
		for i := range rows {
			rows[i] = "[" + strings.Join(row, ",") + "]"
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
		ps := make([]*person, people)
		for i := range ps {
			ps[i] = e.person(fmt.Sprint("P", i))
			for r := 1; r <= count; r++ {
				ps[i].set(fmt.Sprintf("Sheet1!%c%d", 'A'+i, r), "x")
			}
		}
		return e, ps
	}
	t.Run("tabs that come and go", func(t *testing.T) {
		slow(t, "adds and deletes thousands of tabs")
		// Four sessions with 100 entries each, and 8,000 tabs at full
		// size: a note for each tab in each entry would be 3.2 million of
		// them. By default 2,000 tabs come and go, and 300 under the race
		// detector, where the heap is not measured.
		people, count, cycles := 4, maxUndoEntries, raced(sized(8000, 2000), 300)
		e, ps := setup(t, people, count)
		// Carol's payload lies in a tab of its own, which the others do
		// not depend on.
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Third"},{"op":"write","tab":"Third","at":"A1","rows":[[3,"=A1*2"],[1],[2]]}]}`)
		bob, carol := e.person("Bob"), e.person("Carol")
		carol.do(e.clearOp("Third", 1, 1, 2, 1))
		// An edit of Data before the others: the payloads note where their
		// edits of Data begin.
		bob.do(e.insertRowsOp("Data", 1, 1))
		start := e.undoMemory()
		if start.waiting != people*count+1 || start.pool != start.stacks || start.pool == 0 {
			t.Fatalf("before the tabs: %+v, want %d payloads that wait, and their bytes counted", start, people*count+1)
		}
		// An agent adds a tab, inserts a row above its first and deletes
		// it, 40 times in each call, so that the calls leave few revisions
		// for the host to import.
		const perCall = 40
		call := `{"ops":[` + strings.TrimSuffix(strings.Repeat(`{"op":"add_tab","name":"Tmp"},{"op":"insert_rows","tab":"Tmp","at":1,"count":1},{"op":"delete_tab","tab":"Tmp"},`, perCall), ",") + `]}`
		before := int64(heapInUse())
		for i := 0; i < cycles; i += perCall {
			if i/perCall%(maxOutboxPending/2) == maxOutboxPending/2-1 {
				e.b.importAll(t)
			}
			e.mustAgent(call)
		}
		grown := int64(heapInUse()) - before
		m := e.undoMemory()
		t.Logf("%d payloads waited through %d tabs: the pool counts %d bytes, was %d; a payload notes %d tabs at most; the heap grew by %d KiB", m.waiting, cycles, m.pool, start.pool, m.tabs, grown>>10)
		if m.lost != 0 || m.waiting != people*count+1 {
			t.Fatalf("%d payloads wait and %d expired, want all %d kept", m.waiting, m.lost, people*count+1)
		}
		if m.pool != m.stacks || m.session > undoSessionBytes || m.pool > undoInstanceBytes {
			t.Fatalf("the pool counts %d bytes and the stacks hold %d; a session holds up to %d", m.pool, m.stacks, m.session)
		}
		if m.tabs > needsKept {
			t.Fatalf("a payload notes the first edit of %d tabs, want %d at most", m.tabs, needsKept)
		}
		// The heap holds the edits the payloads wait for, once for all
		// payloads, and what the sheet keeps of the calls themselves:
		// their deltas for connections that come back, and their place in
		// the structural log. That was about 560 bytes for each tab when
		// this was written. A note for each tab in each entry would be 48
		// bytes 400 times.
		if limit := int64(cycles)*1024 + 2<<20; measures && grown > limit {
			t.Fatalf("the heap grew by %d KiB over %d tabs, want under %d KiB", grown>>10, cycles, limit>>10)
		}
		// The payloads still follow what they waited for: the edits of the
		// tabs they noted, and those of every other tab since. A sort of
		// Third is logged for Carol's payload alone. The others read it
		// with the rest and pass over it.
		bob.do(e.insertRowsOp("Data", 1, 1))
		bob.do(e.insertRowsOp("Other", 3, 2))
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Info"}`, e.tabID("Data")))
		if ack := bob.do(e.sortOp("Third", 1, 3, 1, false)); status(ack) != "applied" {
			t.Fatalf("sort: %s", status(ack))
		}
		want := strings.Replace(text, "Data!A5+Other!A5", "Info!A7+Other!A7", 1)
		for i, p := range ps {
			wantUndo(t, "undo of the newest edit", undo(t, p), undoAck{Applied: 1})
			e.wantText(fmt.Sprintf("Sheet1!%c%d", 'A'+i, count), want)
		}
		wantUndo(t, "undo of the clear in the sorted tab", undo(t, carol), undoAck{Applied: 1})
		e.wantText("Third!B3", "=A3*2")
		if after := e.undoMemory(); after.pool != after.stacks || after.lost != 0 {
			t.Fatalf("after the undos: %+v", after)
		}
	})
	// What a payload depends on stays in memory when the payload is in a
	// scratch file. Formulas that name tabs by very long names make that
	// large. Each action below replaces 16 formulas that name 32 tabs of
	// 1,900 letters between them, each formula a sum of 2,000 terms
	// besides: its payload passes 1 MiB and goes to a file, and leaves
	// about 125 KiB of names.
	const letters, formulas, terms = 1900, 16, 2000
	nameRow := func(entry int) []string {
		row := make([]string, formulas)
		for i := range row {
			name := func(k int) string { return fmt.Sprintf("N%sE%02dF%02dK%d", strings.Repeat("X", letters), entry, i, k) }
			row[i] = fmt.Sprintf("%q", "="+strings.Repeat("0+", terms)+name(0)+"!A1+"+name(1)+"!A1")
		}
		return row
	}
	// longNames writes the formulas of an entry into a row of their own
	// and returns the operation that clears them.
	longNames := func(e *env, tab string, col, entry int) string {
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"A%d","rows":[[%s]]}]}`, tab, entry+1, strings.Join(nameRow(entry), ",")))
		return e.clearOp(tab, entry+1, 1, col, formulas)
	}
	// perEntry is what one such payload leaves, as left counts it: the
	// entry's record and the record and path of its file, and twice what
	// it depends on, the tab it lies in and the names, each of the letters
	// and nine more characters. A path takes at most 128 bytes here.
	deps := formula.HeapBytes(unsafe.Sizeof(undoDeps{}), true) + mapRoom[TabID, struct{}](1) +
		mapRoom[string, struct{}](2*formulas) + 2*formulas*formula.TextBytes(letters+9)
	perEntry := entryBytes + formula.HeapBytes(unsafe.Sizeof(undoFile{}), true) + formula.TextBytes(128) +
		formula.HeapBytes(unsafe.Sizeof(undoWait{}), true) + 2*deps
	// fitting is how many of entries such payloads fit a session's memory
	// beside the records of the entries that expired.
	fitting := func(sessionMax int64, entries int) int {
		k := entries
		for k > 0 && int64(k)*perEntry+int64(entries-k)*entryBytes > sessionMax {
			k--
		}
		return k
	}
	// filled lets one session make more such payloads than its memory
	// holds, and checks the session's count after every action.
	filled := func(t *testing.T) (e *env, alice *person, sink *logSink, entries int, sessionMax int64) {
		slow(t, "writes formulas with very long tab names")
		sink = &logSink{}
		e = start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
		alice = e.person("Alice")
		entries = raced(64, 20)
		e.in.mu.Lock()
		p := e.in.undoPool()
		if raceEnabled {
			p.sessionMax = 8 * perEntry
		}
		sessionMax = p.sessionMax
		e.in.mu.Unlock()
		if fit := fitting(sessionMax, entries); fit < 4 || fit >= entries-4 {
			t.Fatalf("%d of the %d payloads fit a session's memory: the case does not reach the bound", fit, entries)
		}
		for i := 0; i < entries; i++ {
			if ack := alice.do(longNames(e, "Sheet1", 1, i)); status(ack) != "applied" {
				t.Fatalf("clear %d: %s", i, status(ack))
			}
			if alice.spilled(i) == nil {
				t.Fatalf("the payload of clear %d stays in memory", i)
			}
			if m := e.undoMemory(); m.session > sessionMax || m.pool != m.stacks {
				t.Fatalf("after %d actions the session holds %d bytes, its bound is %d; the pool counts %d and the stacks hold %d", i+1, m.session, sessionMax, m.pool, m.stacks)
			}
		}
		return e, alice, sink, entries, sessionMax
	}
	t.Run("names that fill a session's memory", func(t *testing.T) {
		e, alice, sink, entries, sessionMax := filled(t)
		m := e.undoMemory()
		fit := fitting(sessionMax, entries)
		t.Logf("%d payloads in scratch files: %d wait and %d expired; the session holds %d bytes", entries, m.waiting, m.lost, m.session)
		if m.waiting != fit || m.lost != entries-fit {
			t.Fatalf("%d payloads wait and %d expired, want the %d that fit kept", m.waiting, m.lost, fit)
		}
		// The oldest expired, as the 101st action pushes the first out,
		// and nothing tells the operator.
		for i := 0; i < entries; i++ {
			if got, want := alice.lost(i), i < m.lost; got != want {
				t.Fatalf("entry %d expired: %v, want %v: the oldest go first", i, got, want)
			}
		}
		if lines := sink.lines("sheet undo payload lost"); len(lines) != 0 {
			t.Fatalf("a session that filled its own memory was logged: %q", lines)
		}
		wantUndo(t, "undo of the newest clear", undo(t, alice), undoAck{Applied: formulas})
		if got := e.text(fmt.Sprintf("Sheet1!A%d", entries)); !strings.HasSuffix(got, fmt.Sprintf("E%02dF00K1!A1", entries-1)) {
			t.Fatalf("the newest formula came back as ...%s", got[max(0, len(got)-24):])
		}
		for i := 1; i < m.waiting; i++ {
			wantUndo(t, "undo of a clear that was kept", undo(t, alice), undoAck{Applied: formulas})
		}
		wantUndo(t, "undo of a clear that expired", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
		if after := e.undoMemory(); after.pool != after.stacks {
			t.Fatalf("after the undos the pool counts %d bytes and the stacks hold %d", after.pool, after.stacks)
		}
	})
	// What the history of the session holds is what the heap gives back
	// when the history goes. The count takes the names twice, for the
	// entry and for its file, and the two share their letters, so the heap
	// holds about half of what is counted.
	t.Run("names that fill a session's memory, on the heap", func(t *testing.T) {
		if !measures {
			t.Skip("the race detector's heap says nothing about the undo history")
		}
		e, alice, _, entries, sessionMax := filled(t)
		before := settledHeap(e)
		e.in.mu.Lock()
		alice.s.undo.drop()
		e.in.mu.Unlock()
		held := before - settledHeap(e)
		t.Logf("the undo history of %d actions held %d KiB of the heap; the session may hold %d KiB", entries, held>>10, sessionMax>>10)
		if held > sessionMax {
			t.Fatalf("the undo history held %d KiB of the heap, a session may hold %d KiB", held>>10, sessionMax>>10)
		}
		if held < sessionMax/8 {
			t.Fatalf("the undo history held %d KiB of the heap: the case measures nothing", held>>10)
		}
	})
	// The sessions of a sheet share its memory. Where what their payloads
	// leave passes it, the session whose action commits gives up its own
	// oldest entry, and the others keep theirs.
	t.Run("names that fill the sheet's memory", func(t *testing.T) {
		slow(t, "writes formulas with very long tab names")
		sink := &logSink{}
		e := start(t, func(o *Options) {
			o.Slug = "ledger"
			o.Log = slog.New(slog.NewTextHandler(sink, nil))
		})
		// Each session keeps four payloads, and the sheet has room for
		// those of two sessions and one more.
		const each = 4
		e.in.mu.Lock()
		p := e.in.undoPool()
		p.sessionMax, p.instanceMax = each*perEntry+perEntry/2, 2*each*perEntry+perEntry+perEntry/2
		sheetMax := p.instanceMax
		e.in.mu.Unlock()
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"add_tab","name":"Three"}]}`)
		people := []*person{e.person("Alice"), e.person("Bob"), e.person("Carol")}
		for i, tab := range []string{"Sheet1", "Two", "Three"} {
			for k := 0; k < each; k++ {
				if ack := people[i].do(longNames(e, tab, 1, k)); status(ack) != "applied" {
					t.Fatalf("clear %d of session %d: %s", k, i, status(ack))
				}
				if m := e.undoMemory(); m.pool > sheetMax || m.pool != m.stacks {
					t.Fatalf("after clear %d of session %d the pool counts %d bytes and the stacks hold %d; the sheet's bound is %d", k, i, m.pool, m.stacks, sheetMax)
				}
			}
		}
		alice, bob, carol := people[0], people[1], people[2]
		for i := 0; i < each; i++ {
			if alice.lost(i) || bob.lost(i) {
				t.Fatalf("entry %d of a session that committed inside the sheet's memory expired: Alice %v, Bob %v", i, alice.lost(i), bob.lost(i))
			}
			// Carol's first payload fits beside theirs, and each later one
			// pushes the one before it out.
			if got, want := carol.lost(i), i < each-1; got != want {
				t.Fatalf("Carol's entry %d expired: %v, want %v", i, got, want)
			}
		}
		lines := sink.lines("sheet undo payload lost")
		if len(lines) != 1 || !strings.Contains(lines[0], "leave in memory") || !strings.Contains(lines[0], "slug=ledger") || !strings.Contains(lines[0], fmt.Sprintf("suppressed=%d", 0)) {
			t.Fatalf("the log lines about the expiry are %q, want one that names the sheet and the cause", lines)
		}
		wantUndo(t, "Carol's undo of her newest clear", undo(t, carol), undoAck{Applied: formulas})
		wantUndo(t, "Carol's undo of a clear that expired", undo(t, carol), undoAck{Skipped: 1, Reason: "lost"})
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: formulas})
	})
	// The entries an action takes off the stacks when it commits make room
	// for its own: the redo stack that a new action clears counts as gone
	// when the payload of that action is admitted, and nothing expires in
	// its place.
	t.Run("the redo stack makes room for a new action", func(t *testing.T) {
		slow(t, "writes formulas with very long tab names")
		const half = 8
		e := start(t)
		e.in.mu.Lock()
		p := e.in.undoPool()
		p.sessionMax = 2*half*perEntry + perEntry/2
		e.in.mu.Unlock()
		alice := e.person("Alice")
		// Eight clears of formulas stay on the undo stack. Eight writes of
		// formulas into empty rows are undone: the entries of those undos,
		// on the redo stack, hold the formulas.
		for i := 0; i < half; i++ {
			if ack := alice.do(longNames(e, "Sheet1", 1, i)); status(ack) != "applied" {
				t.Fatalf("clear %d: %s", i, status(ack))
			}
		}
		tab := e.tabID("Sheet1")
		for i := half; i < 2*half; i++ {
			op := jsonf(`{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[[%s]]}`, tab, e.rowsOf("Sheet1", i+1, 1), e.colsOf("Sheet1", 1, formulas), strings.Join(nameRow(i), ","))
			if ack := alice.do(op); status(ack) != "applied" {
				t.Fatalf("write %d: %s", i, status(ack))
			}
		}
		for i := half; i < 2*half; i++ {
			wantUndo(t, "undo of a write", undo(t, alice), undoAck{Applied: formulas})
		}
		m := e.undoMemory()
		if m.lost != 0 || m.waiting != 2*half || m.session > 2*half*perEntry {
			t.Fatalf("before the new action: %+v, want %d payloads that wait on the two stacks", m, 2*half)
		}
		// The new clear leaves as much as each of them, more than the
		// session has room for while the redo stack counts.
		if ack := alice.do(longNames(e, "Sheet1", 1, 2*half)); status(ack) != "applied" {
			t.Fatalf("the new clear: %s", status(ack))
		}
		after := e.undoMemory()
		if after.lost != 0 || after.waiting != half+1 || after.pool != after.stacks {
			t.Fatalf("after the new action: %+v, want the %d payloads of the undo stack kept", after, half+1)
		}
		for i := 0; i <= half; i++ {
			wantUndo(t, "undo of a clear", undo(t, alice), undoAck{Applied: formulas})
		}
	})
	// A rename gives the payloads that name the tab a longer name to depend
	// on. What they leave in memory is counted again when the rename
	// commits, and where it then passes a limit, the oldest of the entries
	// that leave the most expires: without a line in the log at the limit
	// of its own session, and with one at the limit all sessions share.
	for _, shared := range []bool{false, true} {
		name := "a rename that fills a session's memory"
		if shared {
			name = "a rename that fills the sheet's memory"
		}
		t.Run(name, func(t *testing.T) {
			const count = 8
			sink := &logSink{}
			e := start(t, func(o *Options) {
				o.Slug = "ledger"
				o.Log = slog.New(slog.NewTextHandler(sink, nil))
			})
			e.in.mu.Lock()
			p := e.in.undoPool()
			p.entryMax = -1
			e.in.mu.Unlock()
			rows := make([]string, count)
			for i := range rows {
				rows[i] = `["=Data!A5+Other!A5"]`
			}
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"},
				{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			for r := 1; r <= count; r++ {
				alice.set(fmt.Sprintf("Sheet1!A%d", r), "x")
			}
			bob.do(e.insertRowsOp("Data", 1, 1))
			before := e.undoMemory()
			if before.waiting != count || before.lost != 0 || before.pool != before.stacks {
				t.Fatalf("before the rename: %+v, want %d payloads that wait", before, count)
			}
			// The new name takes more of the heap in what the entry depends
			// on now: the name of the most bytes, of a letter of four, takes
			// 112 bytes more. What its file depends on stays as it was
			// written. The limit leaves room for the payloads as they are,
			// for the rename's own entry, which is admitted before the
			// payloads are counted again, and for all but the last of the
			// longer names.
			long := strings.Repeat("\U00020000", maxTabName)
			if formula.TabNameNeedsQuotes(long) {
				t.Fatal("the long name needs apostrophes in a formula")
			}
			grows := formula.TextBytes(len(long)) - formula.TextBytes(len("Data"))
			e.in.mu.Lock()
			if shared {
				p.instanceMax = before.pool + count*grows - grows/2
			} else {
				p.sessionMax = before.session + count*grows - grows/2
			}
			e.in.mu.Unlock()
			if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, e.tabID("Data"), long)); status(ack) != "applied" {
				t.Fatalf("rename: %s", status(ack))
			}
			after := e.undoMemory()
			if after.lost != 1 || after.waiting != count-1 || after.pool != after.stacks || after.pool > before.pool+count*grows-grows/2 {
				t.Fatalf("after the rename: %+v, want one entry expired and the rest inside the limit; before it %+v", after, before)
			}
			if !alice.lost(0) {
				t.Fatal("the oldest entry did not expire")
			}
			lines := sink.lines("sheet undo payload lost")
			switch {
			case !shared && len(lines) != 0:
				t.Fatalf("a session that filled its own memory was logged: %q", lines)
			case shared && (len(lines) != 1 || !strings.Contains(lines[0], "leave in memory") || !strings.Contains(lines[0], "slug=ledger")):
				t.Fatalf("the log lines about the expiry are %q, want one that names the sheet and the cause", lines)
			}
			wantUndo(t, "undo of the newest edit", undo(t, alice), undoAck{Applied: 1})
			e.wantText(fmt.Sprintf("Sheet1!A%d", count), "="+long+"!A6+Other!A5")
			for i := 2; i < count; i++ {
				wantUndo(t, "undo of an edit that was kept", undo(t, alice), undoAck{Applied: 1})
			}
			wantUndo(t, "undo of the edit that expired", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
		})
	}
}
