package sheet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// stored is everything of a workbook that sheet.db keeps, as text: the
// counters, the tabs with their axes, widths and settings, and every cell
// record with its content, style and stamps. Calculated values are not
// stored and are left out.
func stored(wb *Workbook) string {
	var b strings.Builder
	fmt.Fprintf(&b, "seq %d reset %d change %d order %d next %d/%d/%d\n", wb.seq, wb.resetSeq, wb.tabChangeSeq, wb.tabOrderSeq, wb.nextTab, wb.nextRow, wb.nextCol)
	for _, t := range wb.tabs {
		fmt.Fprintf(&b, "tab %d %q born %d name %d settings %d struct %d frozen %d/%d rows %v cols %v\n", t.id, t.name, t.born, t.nameSeq, t.settingsSeq,
			t.structSeq, t.frozenRows, t.frozenCols, idRuns(t.rows.ids), idRuns(t.cols.ids))
		for _, id := range t.cols.ids {
			if w, ok := t.widths[ColID(id)]; ok {
				fmt.Fprintf(&b, " width %d = %+v\n", id, w)
			}
		}
		rows := make([]RowID, 0, len(t.data))
		for row := range t.data {
			rows = append(rows, row)
		}
		slices.Sort(rows)
		for _, row := range rows {
			for _, s := range t.data[row].cells {
				fmt.Fprintf(&b, " %d/%d %+v seq %d text %d style %+v\n", row, s.col, s.c.content, s.c.seq, s.c.textSeq, wb.styles.state(s.c.style))
			}
		}
	}
	// The revision files: the recorded list, and the dirty marks with the
	// reserve of each dirty row file.
	files := wb.files.sorted()
	fmt.Fprintf(&b, "files %v manifest %v\n", files, wb.files.manifest)
	axes := make([]TabID, 0, len(wb.files.axes))
	for id := range wb.files.axes {
		axes = append(axes, id)
	}
	slices.Sort(axes)
	chunks := make([]string, 0, len(wb.files.chunks))
	for key, reserve := range wb.files.chunks {
		chunks = append(chunks, fmt.Sprintf("%d/%d=%d", key.tab, key.first, reserve))
	}
	slices.Sort(chunks)
	fmt.Fprintf(&b, "dirty axes %v chunks %v\n", axes, chunks)
	return b.String()
}

// wantStored fails when the workbook in memory differs from what a reader
// of sheet.db loads.
func (e *env) wantStored(when string) {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	disk, err := loadWorkbook(context.Background(), e.in.rdb)
	if err != nil {
		e.t.Fatalf("%s: sheet.db does not load: %v", when, err)
	}
	if mem, db := stored(e.in.wb), stored(disk); mem != db {
		e.t.Fatalf("%s: memory and sheet.db differ:\n%s\n--- sheet.db ---\n%s", when, mem, db)
	}
}

// group queues the actions behind a held writer, in order, and lets the
// writer go when all of them wait in its queue, so that it can take them
// into one transaction. It returns the number of transactions it used.
func (e *env) group(acts ...func()) int {
	e.t.Helper()
	in := e.in
	gate, held := make(chan struct{}), make(chan struct{})
	commits := 0
	var window time.Duration
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The window and the hook belong to the writer's goroutine.
		_ = in.execute(context.Background(), &work{run: func() error {
			window, groupCommitWindow = groupCommitWindow, time.Hour
			in.hook = func(step string) {
				if step == "commit" {
					commits++
				}
			}
			close(held)
			<-gate
			return nil
		}})
	}()
	<-held
	// Also when the test fails below, the writer goes on and the window is
	// what it was for the tests after this one.
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	e.t.Cleanup(func() {
		open()
		wg.Wait()
		_ = in.execute(context.Background(), &work{run: func() error {
			in.hook, groupCommitWindow = nil, window
			return nil
		}})
	})
	for i, act := range acts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			act()
		}()
		for deadline := time.Now().Add(10 * time.Second); len(in.queue) != i+1; {
			if time.Now().After(deadline) {
				e.t.Fatal("an action did not reach the writer's queue")
			}
			time.Sleep(time.Millisecond)
		}
	}
	open()
	wg.Wait()
	n := 0
	err := in.execute(context.Background(), &work{run: func() error {
		n, in.hook, groupCommitWindow = commits, nil, window
		return nil
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestRefusedActionLeavesNothing covers a limit that an operation meets
// after it already changed the sheet: the whole action is refused, and
// sheet.db keeps every cell, also after a restart.
func TestRefusedActionLeavesNothing(t *testing.T) {
	e := start(t)
	// B1 reads A2 and is 2 characters under the limit of a formula. Losing
	// row 2 turns A2 into #REF!, which is 3 characters longer.
	long := `=A2&"` + strings.Repeat("x", formula.MaxFormulaChars-2-6) + `"`
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,` + strconv.Quote(long) + `],[2],[3],["keep"]]}]}`)
	if n := len([]rune(e.text("B1"))); n != formula.MaxFormulaChars-2 {
		t.Fatalf("the fixture formula has %d characters", n)
	}
	alice := e.person("Alice")
	seq := e.seq()
	tab, row2, _ := e.ids("A2")
	_, row4, _ := e.ids("A4")
	// The delete takes row 4 first and then meets the limit at row 2.
	ack := alice.do(jsonf(`{"op":"delete_rows","tab":%d,"ids":[%d,%d]}`, tab, row2, row4))
	if status(ack) != "refused:formula_length" {
		t.Fatalf("an action over a limit answered %s", status(ack))
	}
	check := func(when string) {
		t.Helper()
		if e.seq() != seq || e.shown("A4") != "keep" || e.shown("C1") != "" || e.shown("A2") != "2" {
			t.Fatalf("%s: sequence %d, A2 %q, A4 %q, C1 %q", when, e.seq(), e.shown("A2"), e.shown("A4"), e.shown("C1"))
		}
		e.wantStored(when)
	}
	check("after the refusal")
	// The refusal is the stored outcome of the action.
	if again, err := alice.send(alice.n, seq, jsonf(`{"op":"delete_rows","tab":%d,"ids":[%d,%d]}`, tab, row2, row4)); err != nil || status(again) != status(ack) {
		t.Fatalf("the resent action answered %v %s", err, status(again))
	}
	// An edit after it stages only itself.
	if ack := alice.set("D1", "next"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.wantStored("after the next edit")
	e.restart()
	if e.shown("A4") != "keep" || e.shown("D1") != "next" || e.shown("C1") != "" {
		t.Fatalf("after the restart: A4 %q, D1 %q, C1 %q", e.shown("A4"), e.shown("D1"), e.shown("C1"))
	}
}

// TestRefusedOperationLeavesNoMark covers an operation that is refused on
// its own after it changed the sheet: the rest of the action applies, and
// neither sheet.db nor the delta carries anything of the refused one.
func TestRefusedOperationLeavesNoMark(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	s := serve(e)
	watcher := s.dial("private", `{"type":"hello","version":1,"name":"W","pending":0}`)
	watcher.next("welcome")
	watcher.watch(1, e.tabID("Sheet1"))
	alice := e.person("Alice")
	tab, row1, colD := e.ids("D1")
	_, _, colE := e.ids("E1")
	// The write adds a row and fills D1 before it finds that its second
	// line of values is too short.
	bad := jsonf(`{"op":"write","tab":%d,"rows":[%d,{"new":1}],"cols":[%d,%d],"values":[["a","b"],["c"]]}`, tab, row1, colD, colE)
	ack := alice.do(e.setOp("C1", "typed"), bad, jsonf(`{"op":"width","tab":%d,"cols":[[%d,1]],"width":90}`, tab, colE))
	if status(ack) != "applied,refused:invalid,applied" {
		t.Fatalf("the action answered %s", status(ack))
	}
	e.want("C1", "typed")
	e.want("D1", "")
	e.wantStored("after the action")
	d := watcher.next("delta")
	if d["struct"] != nil || d["tabs"] != nil {
		t.Fatalf("the delta carries structure of the refused operation: %v", d)
	}
	cells := d["cells"].([]any)
	if len(cells) != 1 || len(cells[0].([]any)[1].([]any)) != 1 || fmt.Sprint(d["widths"]) != fmt.Sprintf("[[%d 90]]", colE) {
		t.Fatalf("the delta carries more than the two applied operations: %v", d)
	}
	e.restart()
	e.want("C1", "typed")
	if e.in.wb.tabs[0].rows.len() != newTabRows {
		t.Fatalf("the tab has %d rows after the restart", e.in.wb.tabs[0].rows.len())
	}
}

// holdWriter stops the writer and fills its queue. Until the returned
// function is called nothing reaches the writer and no work is admitted.
func (e *env) holdWriter() (release func()) {
	e.t.Helper()
	in := e.in
	gate, held := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wait := func(first bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = in.execute(context.Background(), &work{run: func() error {
				if first {
					close(held)
				}
				<-gate
				return nil
			}})
		}()
	}
	wait(true)
	<-held
	for i := 0; i < cap(in.queue); i++ {
		wait(false)
	}
	for deadline := time.Now().Add(10 * time.Second); len(in.queue) != cap(in.queue); runtime.Gosched() {
		if time.Now().After(deadline) {
			e.t.Fatal("the writer's queue did not fill")
		}
	}
	var once sync.Once
	release = func() {
		once.Do(func() { close(gate) })
		wg.Wait()
	}
	e.t.Cleanup(release)
	return release
}

// failCommits makes every commit of sheet.db fail from the given one on,
// counted from now, until the returned function is called.
func (e *env) failCommits(from int) (restore func()) {
	e.t.Helper()
	set := func(fault func() error) {
		if err := e.in.execute(context.Background(), &work{run: func() error { e.in.fault = fault; return nil }}); err != nil {
			e.t.Fatal(err)
		}
	}
	commits := 0
	set(func() error {
		if commits++; commits >= from {
			return errors.New("injected commit failure")
		}
		return nil
	})
	return func() { set(nil) }
}

// TestReplayIsAnsweredFromItsReceipt sends an action again after it
// committed, as a client does that never got the ack. The answer is the
// stored ack every time, whatever happens to the work it arrives with: a
// commit that fails, or a writer whose queue is full. The action is not
// applied again, and actions that are new get the refusal.
func TestReplayIsAnsweredFromItsReceipt(t *testing.T) {
	setup := func(t *testing.T) (e *env, alice, bob *person, base uint64, ops []string, first ackBody) {
		e = start(t)
		alice, bob = e.person("Alice"), e.person("Bob")
		// Two operations: a row or column edit would be an action of its own.
		base, ops = e.seq(), []string{e.setOp("A1", "one"), e.setOp("A2", "first")}
		first = alice.act(base, ops...)
		if status(first) != "applied,applied" {
			t.Fatal(status(first))
		}
		return e, alice, bob, base, ops, first
	}
	replays := func(t *testing.T, e *env, alice *person, base uint64, ops []string, first ackBody) {
		t.Helper()
		rows := e.in.Stats().Applied
		for i := 0; i < 3; i++ {
			again, err := alice.send(alice.n, base, ops...)
			if err != nil || !reflect.DeepEqual(again, first) {
				t.Fatalf("replay %d answered %v %+v, want the stored ack %+v", i+1, err, again, first)
			}
		}
		if got := e.in.Stats().Applied; got != rows {
			t.Fatalf("the replays applied %d actions", got-rows)
		}
	}
	t.Run("commits fail", func(t *testing.T) {
		e, alice, bob, base, ops, first := setup(t)
		restore := e.failCommits(1)
		replays(t, e, alice, base, ops, first)
		if _, err := bob.try(e.seq(), e.setOp("B1", "new")); refusal(err).Reason != "commit" {
			t.Fatalf("a new action while commits fail: %v", err)
		}
		replays(t, e, alice, base, ops, first)
		restore()
		e.want("B1", "")
		e.want("A2", "first")
	})
	t.Run("the writer's queue is full", func(t *testing.T) {
		e, alice, bob, base, ops, first := setup(t)
		release := e.holdWriter()
		replays(t, e, alice, base, ops, first)
		if _, err := bob.try(base+1, e.setOp("B1", "new")); refusal(err).Reason != "writer_backpressure" {
			t.Fatalf("a new action at a full queue: %v", err)
		}
		release()
		e.want("A2", "first")
	})
	t.Run("queued behind its first send, in a commit that fails", func(t *testing.T) {
		e := start(t)
		alice, bob, carol, dave := e.person("Alice"), e.person("Bob"), e.person("Carol"), e.person("Dave")
		base, op := e.seq(), e.setOp("A1", "first")
		// The first commit is that of Alice's action, and every later one
		// fails: Bob's undo, which has a transaction of its own, and the
		// one that Carol's and Dave's actions share with the replay.
		restore := e.failCommits(2)
		var first, again ackBody
		var sent, resent, undone, third, fourth error
		e.group(
			func() { first, sent = alice.send(1, base, op) },
			func() { _, undone = bob.try(base, `{"op":"undo"}`) },
			func() { _, third = carol.try(base, e.setOp("C1", "carol")) },
			func() { again, resent = alice.send(1, base, op) },
			func() { _, fourth = dave.try(base, e.setOp("D1", "dave")) },
		)
		restore()
		if sent != nil || status(first) != "applied" {
			t.Fatalf("the first send: %v %s", sent, status(first))
		}
		if resent != nil || !reflect.DeepEqual(again, first) {
			t.Fatalf("the replay answered %v %+v, want the stored ack %+v", resent, again, first)
		}
		for name, err := range map[string]error{"the undo": undone, "Carol's action": third, "Dave's action": fourth} {
			if refusal(err).Reason != "commit" {
				t.Errorf("%s in a failed commit: %v", name, err)
			}
		}
		wantTexts(t, e, map[string]string{"A1": "first", "C1": "", "D1": ""})
		if e.seq() != base+1 {
			t.Fatalf("sequence %d, want %d", e.seq(), base+1)
		}
		e.wantStored("after the failed commits")
	})
	t.Run("an agent call", func(t *testing.T) {
		e := start(t)
		call := `{"state":"` + e.state() + `","op_id":"call-1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[7]]}]}`
		first := e.mustAgent(call)
		revs := e.b.importAll(t)
		replays := func(when string) {
			t.Helper()
			for i := 0; i < 3; i++ {
				again, err := e.agent(call)
				if err != nil || !again.Replay || again.State != first.State || again.CellsWritten != first.CellsWritten {
					t.Fatalf("%s: replay %d answered %v %+v", when, i+1, err, again)
				}
			}
		}
		restore := e.failCommits(1)
		replays("commits fail")
		if _, err := e.agent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1]]}]}`); err == nil {
			t.Fatal("a new call while commits fail was accepted")
		}
		restore()
		release := e.holdWriter()
		replays("the writer's queue is full")
		release()
		// The same op_id with another request is still refused.
		_, err := e.agent(`{"op_id":"call-1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[8]]}]}`)
		e.wantRefusal(err, "conflict", "op_id_reuse")
		if got := e.b.importAll(t); got != revs {
			t.Fatalf("the replays recorded %d revisions", got-revs)
		}
		e.want("A1", "7")
		e.want("B1", "")
	})
}

// TestQueuedActionsFollowStructure queues actions that depend on an
// earlier action of the same queue: each gets what it would get if the
// earlier one had committed alone before it arrived.
func TestQueuedActionsFollowStructure(t *testing.T) {
	t.Run("a formula typed before an insert", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[7]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		base := e.seq()
		insert, typed := e.insertRowsOp("Data", 1, 1), e.setOp("A1", "=Data!A1")
		var acks [2]ackBody
		commits := e.group(
			func() { acks[0] = bob.act(base, insert) },
			func() { acks[1] = alice.act(base, typed) },
		)
		if status(acks[0]) != "applied" || status(acks[1]) != "applied" {
			t.Fatalf("the actions answered %s and %s", status(acks[0]), status(acks[1]))
		}
		// The insert ends its transaction, so the formula is read against
		// a structural log that holds it.
		if commits != 2 {
			t.Fatalf("%d transactions, want the insert to end its own", commits)
		}
		e.wantText("A1", "=Data!A2")
		e.want("A1", "7")
	})
	t.Run("an undo after an edit of the same session", func(t *testing.T) {
		e := start(t)
		alice := e.person("Alice")
		alice.set("B1", "first")
		base := e.seq()
		second := e.setOp("C1", "second")
		var acks [2]ackBody
		alice.n += 2
		e.group(
			func() { acks[0], _ = alice.send(alice.n-1, base, second) },
			func() { acks[1], _ = alice.send(alice.n, base, `{"op":"undo"}`) },
		)
		if status(acks[0]) != "applied" || acks[1].Undo == nil || acks[1].Undo.Applied != 1 {
			t.Fatalf("the actions answered %s and %+v", status(acks[0]), acks[1])
		}
		e.want("B1", "first")
		e.want("C1", "")
	})
	t.Run("plain edits share a transaction", func(t *testing.T) {
		e := start(t)
		alice, bob := e.person("Alice"), e.person("Bob")
		base := e.seq()
		ops := [2]string{e.setOp("A1", "a"), e.setOp("B1", "b")}
		commits := e.group(
			func() { alice.act(base, ops[0]) },
			func() { bob.act(base, ops[1]) },
		)
		if commits != 1 {
			t.Fatalf("%d transactions for two queued edits", commits)
		}
		e.want("A1", "a")
		e.want("B1", "b")
		e.wantStored("after the group")
	})
}

// TestGroupedDeltas takes three actions into one transaction and applies
// their deltas one by one: each is the state of its own sequence, and the
// screen ends as a fresh snapshot, formats included.
func TestGroupedDeltas(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1*2"]]}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	watchers := map[string]*client{"private": s.dial("private", `{"type":"hello","version":1,"name":"W","pending":0}`), "public": s.dial("public", helloV1)}
	screens := map[string]*screen{}
	for name, c := range watchers {
		c.next("welcome")
		screens[name] = newScreen(c.watch(1, tab))
	}
	alice, bob, carol := e.person("Alice"), e.person("Bob"), e.person("Carol")
	base := e.seq()
	_, row, colA := e.ids("A1")
	_, _, colC := e.ids("C1")
	format := func(col ColID, set string) string {
		return jsonf(`{"op":"format","tab":%d,"rows":[[%d,1]],"cols":[[%d,1]],"set":%s}`, tab, row, col, set)
	}
	ops := [3][]string{
		{e.setOp("A1", "10"), format(colA, `{"bold":true}`)},
		{e.setOp("A1", "20"), format(colC, `{"italic":true}`)},
		{e.setOp("C1", "=A1+B1"), format(colC, `{"underline":true}`)},
	}
	commits := e.group(
		func() { alice.act(base, ops[0]...) },
		func() { bob.act(base, ops[1]...) },
		func() { carol.act(base, ops[2]...) },
	)
	if commits != 1 || e.seq() != base+3 {
		t.Fatalf("%d transactions and sequence %d for three queued edits after %d", commits, e.seq(), base)
	}
	keyA, keyB, keyC := cellKey(e, "A1"), cellKey(e, "B1"), cellKey(e, "C1")
	// What each sequence holds: the value of A1, of B1 = A1*2 and of C1,
	// and the format lists the screen has by then.
	want := []struct{ a, b, c string }{
		{`n 10 {"bold":true}`, "nf 20", ""},
		{`n 20 {"bold":true}`, "nf 40", `blank {"italic":true}`},
		{`n 20 {"bold":true}`, "nf 40", `nf 60 {"italic":true,"underline":true}`},
	}
	for name, c := range watchers {
		sc := screens[name]
		for i, w := range want {
			d := c.next("delta")
			if d["seq"] != fmt.Sprint(base+uint64(i)+1) {
				t.Fatalf("%s screen: delta %v, want sequence %d", name, d["seq"], base+uint64(i)+1)
			}
			sc.apply(d)
			if got := [3]string{sc.brief(keyA), sc.brief(keyB), sc.brief(keyC)}; got != [3]string{w.a, w.b, w.c} {
				t.Fatalf("%s screen at sequence %v holds %q, want %q", name, d["seq"], got, w)
			}
		}
		fresh := s.dial("private", `{"type":"hello","version":1,"name":"F","pending":0}`)
		fresh.next("welcome")
		if diff := sc.diff(newScreen(fresh.watch(1, tab))); diff != "" {
			t.Errorf("%s screen: %s", name, diff)
		}
	}
}

// TestStructuralOrderInOneCall renames a tab and inserts a row in one
// agent call. A formula typed before the call, and a formula an undo entry
// holds, follow both edits in the order the call made them.
func TestStructuralOrderInOneCall(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[7]]}]}`)
	alice := e.person("Alice")
	alice.set("B1", "=Data!A1")
	alice.set("B1", "plain")
	base := e.seq()
	typed := e.setOp("A1", "=Data!A1")
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Data","name":"Info"},{"op":"insert_rows","tab":"Info","at":1,"count":1}]}`)
	if ack := alice.act(base, typed); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.wantText("A1", "=Info!A2")
	e.want("A1", "7")
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo = %+v", u)
	}
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo = %+v", u)
	}
	e.wantText("B1", "=Info!A2")
	e.want("B1", "7")
	// The other order: the row first, then the name.
	base = e.seq()
	typed = e.setOp("C1", "=Info!A2")
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Info","at":1,"count":1},{"op":"rename_tab","tab":"Info","name":"Facts"}]}`)
	if ack := alice.act(base, typed); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.wantText("C1", "=Facts!A3")
}

func manifestTabs(t *testing.T, e *env, revision int) []string {
	t.Helper()
	files, _, release, err := e.b.RevisionFiles(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wb, err := loadRevision(files, func(hash string) (io.ReadCloser, error) { return e.b.OpenBlob(context.Background(), hash) }, nil)
	if err != nil {
		t.Fatalf("revision %d: %v", revision, err)
	}
	names := make([]string, len(wb.tabs))
	for i, tab := range wb.tabs {
		names[i] = tab.name
	}
	return names
}

// TestTabMoveInRevisions moves tabs and nothing else: the revisions of a
// person's group and of an agent call hold the order, and a rollback to
// them brings it back.
func TestTabMoveInRevisions(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"B"},{"op":"add_tab","name":"C"}]}`)
	start := e.b.importAll(t)
	if got := fmt.Sprint(manifestTabs(t, e, start)); got != "[Sheet1 B C]" {
		t.Fatalf("revision %d holds %s", start, got)
	}
	alice := e.person("Alice")
	if ack := alice.do(jsonf(`{"op":"move_tab","tab":%d,"before":%d}`, e.tabID("C"), e.tabID("Sheet1"))); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.wantStored("after the move")
	e.clock.Add(groupIdle + time.Second)
	e.tick()
	moved := e.b.importAll(t)
	if moved != start+1 {
		t.Fatalf("a move alone recorded %d revisions", moved-start)
	}
	if got := fmt.Sprint(manifestTabs(t, e, moved)); got != "[C Sheet1 B]" {
		t.Fatalf("the revision of the move holds %s", got)
	}
	// An agent call that only moves a tab.
	e.mustAgent(`{"ops":[{"op":"move_tab","tab":"B","to":1}]}`)
	agent := e.b.importAll(t)
	if got := fmt.Sprint(manifestTabs(t, e, agent)); agent != moved+1 || got != "[B C Sheet1]" {
		t.Fatalf("revision %d of the agent's move holds %s", agent, got)
	}
	// A person's move before an agent call is in the group's revision, and
	// the call's revision keeps it.
	alice.do(jsonf(`{"op":"move_tab","tab":%d}`, e.tabID("B")))
	e.mustAgent(`{"ops":[{"op":"write","tab":"C","at":"A1","rows":[[1]]}]}`)
	last := e.b.importAll(t)
	if a, b := fmt.Sprint(manifestTabs(t, e, last-1)), fmt.Sprint(manifestTabs(t, e, last)); last != agent+2 || a != "[C Sheet1 B]" || b != a {
		t.Fatalf("revisions %d and %d hold %s and %s", last-1, last, a, b)
	}
	for _, c := range []struct {
		revision int
		want     string
	}{{agent, "[B C Sheet1]"}, {start, "[Sheet1 B C]"}, {moved, "[C Sheet1 B]"}} {
		if _, err := e.b.Rollback(context.Background(), c.revision, "Alice"); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(e.tabNames()); got != c.want {
			t.Fatalf("after the rollback to revision %d the tabs are %s, want %s", c.revision, got, c.want)
		}
		e.wantStored("after the rollback")
	}
	e.restart()
	if got := fmt.Sprint(e.tabNames()); got != "[C Sheet1 B]" {
		t.Fatalf("after the restart the tabs are %s", got)
	}
}

// TestStructureOfEveryTab checks what a connection learns of structural
// edits: one list with every edit of the sequence, on the tab it watches
// and on every other tab, in the order they were made, each once and with
// its tab, and nothing of the cells of a tab it does not watch. A Public
// connection gets the same list.
func TestStructureOfEveryTab(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[3,"x"],[1,"y"],[2,"z"]]}]}`)
	s := serve(e)
	sheet1, data := e.tabID("Sheet1"), e.tabID("Data")
	bob := s.dial("private", `{"type":"hello","version":1,"name":"Bob","pending":0}`)
	bob.next("welcome")
	bob.watch(1, sheet1)
	viewer := s.dial("public", helloV1)
	viewer.next("welcome")
	viewer.watch(1, sheet1)
	alice := e.person("Alice")
	base := e.seq()
	ids := func(tab string, rows bool, from, count int) string {
		var runs any
		if err := json.Unmarshal([]byte(e.axisIDs(tab, rows, from, count)), &runs); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(runs)
	}
	// Both connections get the delta; it is checked on each.
	expect := func(what string, other bool, want ...string) map[string]any {
		t.Helper()
		var private map[string]any
		for _, c := range []*client{bob, viewer} {
			d := c.next("delta")
			list, _ := d["struct"].([]any)
			got := make([]string, len(list))
			for i, entry := range list {
				got[i] = fmt.Sprint(entry)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%s: struct is\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if other && (d["cells"] != nil || d["widths"] != nil || fmt.Sprint(d["touched"]) != fmt.Sprintf("[%d]", data)) {
				t.Fatalf("%s: a connection that watches another tab got %v", what, d)
			}
			if c == viewer && (d["from"] != nil || d["note"] != nil) {
				t.Fatalf("%s: the Public connection got %v", what, d)
			}
			if c == bob {
				private = d
			}
		}
		return private
	}
	edit := func(tab TabID, op string, more ...string) string {
		fields := append([]string{"op:" + op, fmt.Sprintf("tab:%d", tab)}, more...)
		slices.Sort(fields)
		return "map[" + strings.Join(fields, " ") + "]"
	}
	_, above, _ := e.ids("Data!A1")
	alice.do(e.insertRowsOp("Data", 2, 2))
	expect("an insert", true, edit(data, "insert_rows", "ids:"+ids("Data", true, 2, 2), fmt.Sprintf("after:%d", above)))
	// Bob's editor held =Data!B2+1 from before the insert. The exchange of
	// section 8.4 moves it with the row it read.
	bob.send(jsonf(`{"type":"rebase","id":1,"tab":%d,"row":1,"col":1,"text":"=Data!B2+1","at":[0,0],"base":"%d","through":"%d"}`, sheet1, base, base+1))
	if r := bob.next("rebased"); r["text"] != "=Data!B4+1" {
		t.Fatalf("rebase over an insert in another tab = %v", r)
	}
	rows, cols := ids("Data", true, 2, 2), ids("Data", false, 3, 1)
	alice.do(e.deleteRowsOp("Data", 2, 2))
	expect("a delete of rows", true, edit(data, "delete_rows", "ids:"+rows))
	alice.do(e.deleteColsOp("Data", 3, 1))
	expect("a delete of columns", true, edit(data, "delete_cols", "ids:"+cols))
	alice.do(e.sortOp("Data", 1, 3, 1, false))
	e.in.mu.RLock()
	sorted := fmt.Sprint(e.in.wb.byID[data].rows.ids[:3])
	e.in.mu.RUnlock()
	expect("a sort", true, edit(data, "sort", "positions:[[0 3]]", "ids:"+sorted))
	// Rows added at the end are an insert after the last row.
	e.in.mu.RLock()
	last := e.in.wb.byID[data].rows.ids[e.in.wb.byID[data].rows.len()-1]
	e.in.mu.RUnlock()
	alice.do(jsonf(`{"op":"append_rows","tab":%d,"count":3}`, data))
	e.in.mu.RLock()
	length := e.in.wb.byID[data].rows.len()
	e.in.mu.RUnlock()
	expect("an append", true, edit(data, "insert_rows", "ids:"+ids("Data", true, length-2, 3), fmt.Sprintf("after:%d", last)))
	// Tab edits and row edits of one call keep their order, each once.
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Data","name":"Info"},{"op":"insert_rows","tab":"Info","at":1,"count":1},
		{"op":"add_tab","name":"New"},{"op":"move_tab","tab":"New","to":1},{"op":"delete_tab","tab":"New"}]}`)
	next := data + 1
	d := expect("an agent call", true, edit(data, "rename_tab", "name:Info"), edit(data, "insert_rows", "ids:"+ids("Info", true, 1, 1), "after:<nil>"),
		edit(next, "add_tab"), edit(next, "move_tab"), edit(next, "delete_tab"))
	if d["tabs"] == nil || d["note"] == nil {
		t.Fatalf("the agent call's delta lacks the tab list or the note: %v", d)
	}
	// An edit of the watched tab is in the same list, once, with its
	// cells beside it.
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":1},{"op":"write","tab":"Sheet1","at":"A1","rows":[["top"]]}]}`)
	d = expect("an insert in the watched tab", false, edit(sheet1, "insert_rows", "ids:"+ids("Sheet1", true, 1, 1), "after:<nil>"))
	if d["cells"] == nil || d["touched"] != nil {
		t.Fatalf("an edit of the watched tab = %v", d)
	}
	// A plain edit carries no structure.
	alice.set("Sheet1!A1", "plain")
	if d := bob.next("delta"); d["struct"] != nil {
		t.Fatalf("a plain edit carries structure: %v", d)
	}
}

// TestEditGroupsClosePerPerson follows two people whose edit groups become
// due at different times (spec rule 47). A group that is due closes alone,
// as one revision that credits everyone whose edits no revision holds yet.
// The other person's group keeps its start, so its five minutes are not
// counted anew. An agent call closes every open group.
func TestEditGroupsClosePerPerson(t *testing.T) {
	e := start(t)
	alice, bob := e.person("Alice"), e.person("Bob")
	open := func() (n int) {
		t.Helper()
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM groups`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// revision passes the time, runs the tick and returns the authors of
	// the revision it recorded, or nil when it recorded none.
	revisions := e.b.importAll(t)
	revision := func(pass time.Duration) []string {
		t.Helper()
		e.clock.Add(pass)
		e.tick()
		n := e.b.importAll(t)
		if n == revisions {
			return nil
		}
		if n != revisions+1 {
			t.Fatalf("the tick recorded %d revisions", n-revisions)
		}
		revisions = n
		return e.b.entry(n).Authors
	}
	alice.set("A1", "1")
	if got := revision(20 * time.Second); got != nil {
		t.Fatalf("a revision by %v before any group was due", got)
	}
	bob.set("B1", "1")
	// Alice has been idle for 31 seconds and Bob for 11.
	if got := revision(11 * time.Second); !slices.Equal(got, []string{"Alice", "Bob"}) || open() != 1 {
		t.Fatalf("Alice's group closed as a revision by %v and left %d groups open", got, open())
	}
	// Bob goes on every 20 seconds. His group began 20 seconds after the
	// start, and it closes five minutes after that, whatever Alice did.
	for at := 31 * time.Second; at+20*time.Second < 20*time.Second+groupMaxAge; at += 20 * time.Second {
		bob.set("B1", fmt.Sprint(at))
		if got := revision(20 * time.Second); got != nil {
			t.Fatalf("a revision by %v after %v, before Bob's group was due", got, at+20*time.Second)
		}
	}
	if got := revision(20 * time.Second); !slices.Equal(got, []string{"Bob"}) || open() != 0 {
		t.Fatalf("five minutes after Bob's group began: a revision by %v, %d groups open", got, open())
	}
	// A group whose edits a revision already holds closes without one, also
	// while another person's group is open and not due.
	alice.set("A2", "2")
	bob.set("B2", "2")
	e.clock.Add(25 * time.Second)
	alice.set("A3", "3")
	if got := revision(6 * time.Second); !slices.Equal(got, []string{"Alice", "Bob"}) || open() != 1 {
		t.Fatalf("Bob's group closed as a revision by %v and left %d groups open", got, open())
	}
	bob.set("B3", "3")
	if got := revision(25 * time.Second); got != nil || open() != 1 {
		t.Fatalf("Alice's group, whose edits are recorded, closed as a revision by %v and left %d groups open", got, open())
	}
	// An agent call closes Bob's open group first, as its own revision.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[[1]]}]}`)
	n := e.b.importAll(t)
	person, agent := e.b.entry(n-1), e.b.entry(n)
	if n != revisions+2 || person.Kind != "person" || !slices.Equal(person.Authors, []string{"Bob"}) || agent.Kind != "agent" || open() != 0 {
		t.Fatalf("the agent call: %d revisions, %s by %v then %s, %d groups open", n-revisions, person.Kind, person.Authors, agent.Kind, open())
	}
}

// TestActionOutcomes goes through the ways a person's action ends (the
// table at submit): what the sender is told, and what the same action sent
// again is told, while the situation lasts or after it. An answer other
// than an ack leaves the action unapplied and its number free, so sending
// it again then applies it once.
func TestActionOutcomes(t *testing.T) {
	answer := func(ack ackBody, err error) string {
		if err != nil {
			werr := refusal(err)
			return werr.Code + " " + werr.Reason
		}
		return status(ack)
	}
	type situation struct {
		first string        // the answer to the action
		again func() string // sends it again while the situation lasts
		end   func()        // ends the situation
	}
	cases := []struct {
		name string
		// The answer to the action, to the same action sent again, and
		// the text of A1 in the end.
		answer, again, a1 string
		run               func(t *testing.T, e *env, alice *person, base uint64, op string) situation
	}{
		{"applied", "applied", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			return situation{first: answer(alice.send(1, base, op))}
		}},
		{"refused by a limit, with its receipt", "refused:text_length", "refused:text_length", "", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			long := e.setOp("A1", strings.Repeat("x", formula.MaxTextLen+1))
			return situation{first: answer(alice.send(1, base, long)), again: func() string { return answer(alice.send(1, base, long)) }}
		}},
		{"sent again with other content", "applied", "invalid action_number", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			return situation{first: answer(alice.send(1, base, op)), again: func() string { return answer(alice.send(1, base, e.setOp("A1", "other"))) }}
		}},
		{"sent again after its receipt was dropped", "applied", "conflict receipt_expired", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			first := answer(alice.send(1, base, op))
			if err := e.in.execute(context.Background(), &work{run: func() error {
				_, err := e.in.db.Exec(`DELETE FROM receipts WHERE session=? AND n=1`, alice.s.id)
				return err
			}}); err != nil {
				t.Fatal(err)
			}
			return situation{first: first}
		}},
		{"without operations", "invalid action", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			return situation{first: answer(alice.send(1, base))}
		}},
		{"with a base the server has not sent", "invalid action", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			return situation{first: answer(alice.send(1, base+5, op))}
		}},
		{"the writer's queue is full", "unavailable writer_backpressure", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			release := e.holdWriter()
			return situation{first: answer(alice.send(1, base, op)), end: release}
		}},
		{"the outbox is full", "unavailable outbox_backpressure", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			for i := 0; i < maxOutboxPending; i++ {
				e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z9","rows":[[1]]}]}`)
			}
			return situation{first: answer(alice.send(1, base, op)), end: func() { e.b.importAll(t) }}
		}},
		{"no measurement of the directory", "unavailable measurement", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			e.b.setQuota(1 << 40)
			e.b.setOther(-1)
			if err := e.in.Remeasure(context.Background()); err == nil {
				t.Fatal("a measurement of a negative usage succeeded")
			}
			return situation{first: answer(alice.send(1, base, op)), end: func() {
				e.b.setOther(0)
				if err := e.in.Remeasure(context.Background()); err != nil {
					t.Fatal(err)
				}
			}}
		}},
		{"its commit failed", "unavailable commit", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			restore := e.failCommits(1)
			return situation{first: answer(alice.send(1, base, op)), end: restore}
		}},
		{"sent twice into one commit", "applied", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			var first, second string
			if commits := e.group(func() { first = answer(alice.send(1, base, op)) }, func() { second = answer(alice.send(1, base, op)) }); commits != 1 || second != first {
				t.Fatalf("%d commits answered %q and %q", commits, first, second)
			}
			return situation{first: first}
		}},
		{"sent twice into one commit that fails", "unavailable commit", "applied", "typed", func(t *testing.T, e *env, alice *person, base uint64, op string) situation {
			restore := e.failCommits(1)
			var first, second string
			e.group(func() { first = answer(alice.send(1, base, op)) }, func() { second = answer(alice.send(1, base, op)) })
			if second != first {
				t.Fatalf("the two sends answered %q and %q", first, second)
			}
			return situation{first: first, end: restore}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.b.importAll(t)
			alice := e.person("Alice")
			base, op := e.seq(), e.setOp("A1", "typed")
			s := c.run(t, e, alice, base, op)
			if s.first != c.answer {
				t.Fatalf("the action was answered %q, want %q", s.first, c.answer)
			}
			if applied := e.text("A1") != ""; applied != (c.answer == "applied") {
				t.Fatalf("after the answer %q A1 holds %q", s.first, e.text("A1"))
			}
			if s.end != nil {
				s.end()
			}
			if s.again == nil {
				s.again = func() string { return answer(alice.send(1, base, op)) }
			}
			for i := 0; i < 2; i++ {
				if got := s.again(); got != c.again {
					t.Fatalf("sent again it was answered %q, want %q", got, c.again)
				}
			}
			if got := e.text("A1"); got != c.a1 {
				t.Fatalf("A1 holds %q in the end, want %q", got, c.a1)
			}
			e.wantStored("in the end")
		})
	}
	t.Run("the instance stopped", func(t *testing.T) {
		e := start(t)
		alice := e.person("Alice")
		base, op := e.seq(), e.setOp("A1", "typed")
		e.in.Stop()
		if got := answer(alice.send(1, base, op)); got != "unavailable stopped" {
			t.Fatalf("the action was answered %q", got)
		}
		e.open()
		e.wantText("A1", "")
	})
}
