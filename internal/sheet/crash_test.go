package sheet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// crashCopy copies sheet.db and its write-ahead log as a killed process
// would leave them, and opens the copy as a restarted host does.
func crashCopy(t *testing.T, e *env) *env {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{dbName, dbName + "-wal"} {
		src, err := os.Open(filepath.Join(e.dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			t.Fatal(err)
		}
		src.Close()
		dst.Close()
	}
	copied := &env{t: t, dir: dir, b: e.b.clone(), clock: e.clock, opts: e.opts}
	return copied
}

func (b *fakeBackend) clone() *fakeBackend {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := newBackend()
	for hash, data := range b.blobs {
		c.blobs[hash] = data
	}
	c.revs, c.cursor, c.quota, c.other = append(c.revs, b.revs...), b.cursor, b.quota, b.other
	return c
}

// TestCrashAtWritePathSteps kills the host before and after the sheet.db
// commit of a person's action and compares what a restart finds (section
// 22.3, the Durability criterion). TestCrashAroundAgentCall and
// TestCrashAroundGroupBoundary do the same for an agent call and for the
// boundary of an edit group.
func TestCrashAtWritePathSteps(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1*10"]]}]}`)
	alice := e.person("Alice")
	alice.set("A1", "2")
	var mu sync.Mutex
	copies := map[string]*env{}
	e.in.hook = func(step string) {
		mu.Lock()
		defer mu.Unlock()
		if _, taken := copies[step]; !taken {
			copies[step] = crashCopy(t, e)
		}
	}
	base := e.seq()
	edit := e.setOp("A1", "5")
	alice.act(base, edit)
	e.in.hook = nil
	e.want("B1", "50")

	before, after := copies["commit"], copies["committed"]
	// Killed before the commit: the edit is not there, and neither is its
	// receipt, so the resent action applies once.
	before.open()
	defer before.in.Stop()
	before.want("A1", "2")
	before.want("B1", "20")
	if before.seq() != base {
		t.Fatalf("sequence %d after a kill before the commit, want %d", before.seq(), base)
	}
	// Killed right after the commit: the edit is present with the values
	// that follow from it, its receipt answers a resend, and the undo
	// history is empty.
	after.open()
	defer after.in.Stop()
	after.want("A1", "5")
	after.want("B1", "50")
	if after.seq() != base+1 {
		t.Fatalf("sequence %d after a kill behind the commit, want %d", after.seq(), base+1)
	}
	for name, c := range map[string]*env{"before": before, "after": after} {
		s, err := c.in.resumeSession(context.Background(), "no such token")
		if err == nil || s != nil {
			t.Fatalf("%s: an unknown token resumed a session", name)
		}
		// The session comes back from sheet.db by its token hash.
		resumed := &session{hash: alice.s.hash, id: alice.s.id}
		var maxN uint64
		if err := c.in.rdb.QueryRow(`SELECT max_n,editor,label FROM sessions WHERE id=?`, alice.s.id).Scan(&maxN, &resumed.editor, &resumed.label); err != nil {
			t.Fatalf("%s: session: %v", name, err)
		}
		resumed.maxN = maxN
		p := &person{e: c, s: c.adopt(resumed), n: alice.n}
		ack, err := p.send(alice.n, base, edit)
		if err != nil || status(ack) != "applied" || ack.Seq != fmt.Sprint(base+1) {
			t.Fatalf("%s: resent action: %v %+v", name, err, ack)
		}
		if c.seq() != base+1 || c.shown("B1") != "50" {
			t.Fatalf("%s: the resent action left sequence %d and B1 %q", name, c.seq(), c.shown("B1"))
		}
		// The action that was applied before the kill is not on the undo
		// stack of the restarted host; the one applied after it is.
		wantUndo := 0
		if name == "before" {
			wantUndo = 1
		}
		if u := p.do(`{"op":"undo"}`).Undo; u == nil || u.Applied != wantUndo || u.Skipped != 0 {
			t.Fatalf("%s: undo history after a restart: %+v", name, u)
		}
	}
}

// TestCrashAroundAgentCall kills the host around the commit of an agent
// call: its revision is recorded once or not at all, and a call resent
// with its op_id never applies twice.
func TestCrashAroundAgentCall(t *testing.T) {
	e := start(t)
	e.b.importAll(t)
	var mu sync.Mutex
	copies := map[string]*env{}
	e.in.hook = func(step string) {
		mu.Lock()
		defer mu.Unlock()
		if _, taken := copies[step]; !taken {
			copies[step] = crashCopy(t, e)
		}
	}
	call := `{"state":"` + e.state() + `","op_id":"call-1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[7,"=A1+1"]]}]}`
	first := e.mustAgent(call)
	e.in.hook = nil
	for name, want := range map[string]bool{"commit": false, "committed": true} {
		c := copies[name]
		c.open()
		defer c.in.Stop()
		if got := c.shown("B1") == "8"; got != want {
			t.Fatalf("%s: the call is present = %v", name, got)
		}
		revs := c.b.importAll(t)
		res, err := c.agent(call)
		if err != nil || res.State != first.State || res.Replay != want {
			t.Fatalf("%s: resent call: %v %+v", name, err, res)
		}
		c.want("B1", "8")
		// One revision for the call, however often it was sent.
		if got := c.b.importAll(t); got != 2 || (want && got != revs) {
			t.Fatalf("%s: %d revisions, %d before the resend", name, got, revs)
		}
	}
}

// TestCrashAroundGroupBoundary kills the host around the commit that closes
// a person's edit group. The group's revision is recorded once: by that
// commit, or by the first tick of the restarted host.
func TestCrashAroundGroupBoundary(t *testing.T) {
	e := start(t)
	revs := e.b.importAll(t)
	e.person("Alice").set("A1", "1")
	e.clock.Add(groupIdle + time.Second)
	var mu sync.Mutex
	copies := map[string]*env{}
	e.in.hook = func(step string) {
		mu.Lock()
		defer mu.Unlock()
		if _, taken := copies[step]; !taken {
			copies[step] = crashCopy(t, e)
		}
	}
	e.tick()
	e.in.hook = nil
	if got := e.b.importAll(t); got != revs+1 {
		t.Fatalf("the tick recorded %d revisions", got-revs)
	}
	for name, recorded := range map[string]bool{"commit": false, "committed": true} {
		c := copies[name]
		if c == nil {
			t.Fatalf("the tick did not reach the step %q", name)
		}
		c.open()
		defer c.in.Stop()
		c.want("A1", "1")
		want := revs
		if recorded {
			want++
		}
		if got := c.b.importAll(t); got != want {
			t.Fatalf("%s: %d revisions after the restart, want %d", name, got, want)
		}
		// The group that was open at the kill is due at once, and later
		// ticks find nothing more to record.
		for i := 0; i < 2; i++ {
			c.tick()
			if got := c.b.importAll(t); got != revs+1 {
				t.Fatalf("%s: %d revisions after tick %d, want %d", name, got, i+1, revs+1)
			}
			c.clock.Add(time.Minute)
		}
		if entry := c.b.entry(revs + 1); entry.Kind != "person" || !slices.Equal(entry.Authors, []string{"Alice"}) {
			t.Fatalf("%s: the revision is %s by %v", name, entry.Kind, entry.Authors)
		}
	}
}

// TestFailedCommit injects a failed commit: memory is reverted and matches
// the database, and nothing reaches a connection (section 22.3).
func TestFailedCommit(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1*10"],[2],[3]]}]}`)
	alice := e.person("Alice")
	alice.set("C1", "kept")
	s := serve(e)
	watcher := s.dial("private", `{"type":"hello","version":1,"name":"W","pending":0}`)
	watcher.next("welcome")
	watcher.watch(1, e.tabID("Sheet1"))
	seq := e.seq()
	fail := errors.New("injected commit failure")
	e.in.fault = func() error { return fail }
	if _, err := alice.try(seq, e.setOp("A1", "9")); refusal(err).Reason != "commit" {
		t.Fatalf("a failed commit answered %v", err)
	}
	if _, err := alice.try(seq, e.insertRowsOp("Sheet1", 1, 2)); refusal(err).Reason != "commit" {
		t.Fatalf("a failed commit answered %v", err)
	}
	if _, err := alice.try(seq, `{"op":"undo"}`); refusal(err).Reason != "commit" {
		t.Fatalf("a failed commit answered %v", err)
	}
	_, err := e.agent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A","desc":true},{"op":"add_tab","name":"X"}]}`)
	if err == nil {
		t.Fatal("the agent call was acknowledged over a failed commit")
	}
	e.in.fault = nil
	check := func(when string) {
		t.Helper()
		if e.seq() != seq || e.shown("A1") != "1" || e.shown("B1") != "10" || e.shown("C1") != "kept" || len(e.tabNames()) != 1 {
			t.Fatalf("%s: sequence %d, A1 %q, B1 %q, tabs %v", when, e.seq(), e.shown("A1"), e.shown("B1"), e.tabNames())
		}
	}
	check("after the failed commits")
	// The action numbers were not consumed, the undo history is intact,
	// and the next edit applies on the unchanged state.
	alice.n -= 3
	if ack := alice.set("A1", "4"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	if d := watcher.next("delta"); d["seq"] != fmt.Sprint(seq+1) {
		t.Fatalf("the first delta after the failures has sequence %v, want %d", d["seq"], seq+1)
	}
	e.want("B1", "40")
	undo(t, alice)
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo of the edit before the failures = %+v", u)
	}
	e.want("C1", "")
	e.restart()
	e.want("A1", "1")
	e.want("B1", "10")
}

// TestRestartKeepsEverything compares the workbook before and after a
// restart: content, formats, widths, settings, stamps and counters, and
// what a read returns.
func TestRestartKeepsEverything(t *testing.T) {
	e := start(t)
	e.mustAgent(offsite)
	alice := e.person("Alice")
	alice.do(e.insertRowsOp("Budget", 2, 3))
	alice.do(e.insertColsOp("Budget", 2, 1))
	alice.do(e.sortOp("Budget", 5, 2, 1, false))
	alice.set("Budget!A3", "'=text")
	alice.set("Budget!E1", "=SUM(")
	alice.set("Budget!F1", "3/15/2026")
	alice.set("Budget!G1", "12.5%")
	alice.do(jsonf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":1}`, e.tabID("Budget")),
		jsonf(`{"op":"width","tab":%d,"cols":%s,"width":300}`, e.tabID("Budget"), e.colsOf("Budget", 1, 2)))
	alice.do(`{"op":"add_tab","name":"Notes"}`)
	alice.do(e.deleteRowsOp("Budget", 2, 1))
	dump := func() string {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		wb := e.in.wb
		var b strings.Builder
		fmt.Fprintf(&b, "seq %d reset %d change %d order %d next %d/%d/%d count %+v\n", wb.seq, wb.resetSeq, wb.tabChangeSeq, wb.tabOrderSeq, wb.nextTab, wb.nextRow, wb.nextCol, wb.count)
		for _, t := range wb.tabs {
			fmt.Fprintf(&b, "tab %d %q born %d name %d settings %d struct %d frozen %d/%d rows %v cols %v born %v %v\n", t.id, t.name, t.born, t.nameSeq, t.settingsSeq,
				t.structSeq, t.frozenRows, t.frozenCols, idRuns(t.rows.ids), idRuns(t.cols.ids), t.rows.born, t.cols.born)
			for _, id := range t.cols.ids {
				if w, ok := t.widths[ColID(id)]; ok {
					fmt.Fprintf(&b, " width %d = %+v\n", id, w)
				}
			}
			for pos, rd := range t.rowsByPos() {
				if rd == nil {
					continue
				}
				for _, s := range rd.cells {
					state := wb.styles.state(s.c.style)
					fmt.Fprintf(&b, " %d/%d %+v val %+v seq %d text %d style %+v\n", pos, t.cols.pos[uint64(s.col)], s.c.content, s.c.val, s.c.seq, s.c.textSeq, state)
				}
			}
		}
		return b.String()
	}
	// What a reader gets goes through the capture, not through the
	// workbook's own tables, so it is compared as well.
	view := func() string { return e.liveView("Budget", "A1:H12") }
	before, seen := dump(), view()
	if !strings.Contains(seen, `"$1,200.00"`) || !strings.Contains(seen, `"bold":true`) || !strings.Contains(seen, `"width":300`) {
		t.Fatalf("the sheet reads without its formats and widths before the restart:\n%s", seen)
	}
	e.restart()
	if after := dump(); after != before {
		t.Fatalf("the workbook changed in the restart:\n%s\n--- after ---\n%s", before, after)
	}
	if after := view(); after != seen {
		t.Fatalf("what a reader gets changed in the restart:\n%s\n--- after ---\n%s", seen, after)
	}
	e.wantText("Budget!A2", "'=text")
	e.want("Budget!E1", "#ERROR!")
	e.want("Budget!F1", "3/15/2026")
	e.want("Budget!G1", "12.50%")
}

// TestImportCursorAndMissingBlob covers the remaining faults of section
// 22.3 on the engine side.
func TestImportCursorAndMissingBlob(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	rev := e.b.importAll(t)
	// An acknowledgement that is repeated after a restart changes nothing.
	if err := e.in.Acknowledge(context.Background(), e.b.cursor, 0); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if entries, err := e.in.Outbox(context.Background(), e.b.cursor, 0); err != nil || len(entries) != 0 {
		t.Fatalf("outbox after the import: %v %v", entries, err)
	}
	refs, err := e.in.References(context.Background())
	if err != nil || len(refs) != len(e.b.entry(rev).Files) {
		t.Fatalf("references = %v, %v", refs, err)
	}
	// A missing blob refuses the rollback and leaves the sheet as it was.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[99]]}]}`)
	e.b.mu.Lock()
	saved := e.b.blobs
	e.b.blobs = map[string][]byte{}
	e.b.mu.Unlock()
	seq := e.seq()
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); refusal(err).Code != "unavailable" {
		t.Fatalf("rollback without its blobs: %v", err)
	}
	if e.seq() != seq || e.shown("A1") != "99" {
		t.Fatal("the failed rollback changed the sheet")
	}
	e.b.mu.Lock()
	e.b.blobs = saved
	e.b.mu.Unlock()
	// A sheet.db behind the import cursor of flats.db is refused.
	e.in.Stop()
	opts := e.opts
	opts.ImportStore, opts.ImportSeq = e.in.StoreID(), e.b.cursor+5
	if _, _, err := Open(e.dir, e.b, opts); err == nil || !strings.Contains(err.Error(), "behind flats.db") {
		t.Fatalf("a sheet.db behind the import cursor opened: %v", err)
	}
	e.open()
	// Stop closes the database.
	e.in.Stop()
	if _, err := e.in.db.Exec(`SELECT 1`); err == nil {
		t.Fatal("the database is still open after Stop")
	}
	// A newer schema fails without a write: sheet.db holds the same bytes
	// after the refused open, and no frame waits in its write-ahead log.
	// SQLite makes the log's index files for a reader, too.
	e.open()
	if _, err := e.in.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	e.in.Stop()
	stored := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(e.dir, dbName))
		if err != nil {
			t.Fatal(err)
		}
		return contentHash(data)
	}
	before := stored()
	if _, _, err := Open(e.dir, e.b, e.opts); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer schema opened: %v", err)
	}
	if after := stored(); after != before {
		t.Fatalf("the refused open changed sheet.db: %s, was %s", after, before)
	}
	if info, err := os.Stat(filepath.Join(e.dir, dbName+"-wal")); err == nil && info.Size() != 0 {
		t.Fatalf("the refused open left %d bytes in the write-ahead log", info.Size())
	}
}
