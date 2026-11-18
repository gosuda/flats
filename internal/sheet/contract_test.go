package sheet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The contract cases of the technical design, section 22.2, that run
// through the Go API. Cases that need a connection are in live_test.go.

// TestCalculationDateAcrossRollback is C-02.
func TestCalculationDateAcrossRollback(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=TODAY()"]]}]}`)
	e.want("A1", "2026-11-13")
	rev := e.b.importAll(t)
	e.clock.Add(24 * time.Hour)
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
		t.Fatal(err)
	}
	rolled := e.b.importAll(t)
	check := func() {
		t.Helper()
		e.want("A1", "2026-11-14")
		for n, want := range map[int]string{rev: "2026-11-13", rolled: "2026-11-14"} {
			out := e.readRevision(n, ReadQuery{Tab: "Sheet1", Include: []string{"shown"}})
			shown, _ := json.Marshal(out["shown"])
			if string(shown) != `[["`+want+`"]]` || out["date"] != want {
				t.Errorf("revision %d shows %s on %v, want %s", n, shown, out["date"], want)
			}
		}
	}
	check()
	if !slices.Equal(e.b.entry(rev).Files, e.b.entry(rolled).Files) {
		t.Error("the rollback revision does not name the files of the revision it restored")
	}
	e.restart()
	check()
}

// TestCalculationDate covers the calculation date criterion of section
// 22.4 on the engine side.
func TestCalculationDate(t *testing.T) {
	e := start(t)
	e.clock.Add(14*time.Hour + 59*time.Minute + 30*time.Second) // 23:59:30
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=TODAY()","=A1+1",5],[1],[2],[3]]}]}`)
	rev := e.b.importAll(t)
	// A paged read begun before the change keeps its date on every page.
	first := e.read(ReadQuery{Tab: "Sheet1", Limit: 2, Include: []string{"shown"}})
	seq := e.seq()
	e.clock.Add(45 * time.Second)
	// With nobody connected the change happens before the next read.
	out := e.read(ReadQuery{})
	if out["date"] != "2026-11-14" || e.seq() != seq+1 {
		t.Fatalf("date %v at sequence %d, want the next day at %d", out["date"], e.seq(), seq+1)
	}
	e.want("A1", "2026-11-14")
	e.want("B1", "46341")
	next := first["next"].(map[string]any)
	page := e.read(ReadQuery{Cursor: next["cursor"].(string)})
	if first["date"] != "2026-11-13" || page["date"] != "2026-11-13" || page["state"] != first["state"] {
		t.Fatalf("pages of one read: %v then %v", first["date"], page["date"])
	}
	// It changes no content and creates no revision.
	if n := e.b.importAll(t); n != rev {
		t.Fatalf("the date change recorded revision %d", n)
	}
	old := e.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: "A1", Include: []string{"shown"}})
	if shown, _ := json.Marshal(old["shown"]); string(shown) != `[["2026-11-13"]]` {
		t.Fatalf("the revision closed before the change shows %s", shown)
	}
}

// TestAgentCallTabView is C-04.
func TestAgentCallTabView(t *testing.T) {
	call := func(state string) string {
		return `{"state":"` + state + `","ops":[{"op":"rename_tab","tab":"Sheet1","name":"Budget"},
			{"op":"write","tab":"Budget","at":"A1","rows":[[1]]},{"op":"add_tab","name":"Notes"},
			{"op":"write","tab":"Notes","at":"A1","rows":[["n"]]}]}`
	}
	t.Run("applied", func(t *testing.T) {
		e := start(t)
		if _, err := e.agent(call(e.state())); err != nil {
			t.Fatal(err)
		}
		e.want("Budget!A1", "1")
		e.want("Notes!A1", "n")
	})
	for name, change := range map[string]func(e *env, bob *person){
		"renamed": func(e *env, bob *person) { bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"X"}`, e.tabID("Sheet1"))) },
		"deleted": func(e *env, bob *person) {
			bob.do(`{"op":"add_tab","name":"Y"}`)
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1")))
		},
		"replaced": func(e *env, bob *person) {
			bob.do(`{"op":"add_tab","name":"Y"}`)
			bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1")))
			bob.do(`{"op":"add_tab","name":"Sheet1"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			state := e.state()
			change(e, e.person("Bob"))
			_, err := e.agent(call(state))
			e.wantRefusal(err, "conflict", "tab_changed")
		})
	}
	t.Run("structure", func(t *testing.T) {
		e := start(t)
		state := e.state()
		e.person("Bob").do(e.insertRowsOp("Sheet1", 1, 1))
		_, err := e.agent(call(state))
		e.wantRefusal(err, "conflict", "structure_changed")
		if got := e.tabNames(); !slices.Equal(got, []string{"Sheet1"}) {
			t.Fatalf("the refused call left tabs %v", got)
		}
	})
}

// TestFormulaInFlightNamesTab is C-06.
func TestFormulaInFlightNamesTab(t *testing.T) {
	setup := func(t *testing.T) (*env, *person, *person, uint64, string) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Old"},{"op":"write","tab":"Old","at":"A1","rows":[[4]]}]}`)
		return e, e.person("Alice"), e.person("Bob"), e.seq(), e.setOp("Sheet1!B2", "=Old!A1")
	}
	forget := func(e *env) {
		e.in.mu.Lock()
		e.in.structs.clear(e.in.wb.seq)
		e.in.mu.Unlock()
	}
	t.Run("renamed/trimmed", func(t *testing.T) {
		e, alice, bob, base, edit := setup(t)
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"New"}`, e.tabID("Old")))
		forget(e)
		if ack := alice.act(base, edit); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
	})
	t.Run("replaced/restart", func(t *testing.T) {
		e, _, bob, base, edit := setup(t)
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"New"}`, e.tabID("Old")))
		bob.do(`{"op":"add_tab","name":"Old"}`)
		e.restart()
		// The restart cleared the structural log; the durable stamps decide.
		alice := e.person("Alice again")
		if ack := alice.act(base, edit); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
		e.wantText("Sheet1!B2", "")
	})
	t.Run("deleted/trimmed", func(t *testing.T) {
		e, alice, bob, base, edit := setup(t)
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Old")))
		forget(e)
		if ack := alice.act(base, edit); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
	})
	t.Run("no tab name", func(t *testing.T) {
		e, alice, bob, base, _ := setup(t)
		edit := e.setOp("Sheet1!B2", "=A1+1")
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"New"}`, e.tabID("Old")))
		bob.set("Sheet1!A1", "2")
		forget(e)
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.want("Sheet1!B2", "3")
	})
}

// TestUndoRetention is C-07.
func TestUndoRetention(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	for i := 1; i <= 101; i++ {
		alice.set(fmt.Sprintf("A%d", i), fmt.Sprint(i))
	}
	for i := 101; i >= 2; i-- {
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo %d = %+v", 102-i, u)
		}
		e.want(fmt.Sprintf("A%d", i), "")
	}
	if u := undo(t, alice); u.Applied != 0 || u.Skipped != 0 {
		t.Fatalf("the 101st undo = %+v, want no earlier undo", u)
	}
	e.want("A1", "1")

	// A sort undo needs no structural log.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[[3],[1],[2]]}]}`)
	alice.do(e.sortOp("Sheet1", 1, 3, 3, false))
	e.in.mu.Lock()
	e.in.structs.clear(e.in.wb.seq)
	e.in.mu.Unlock()
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("sort undo = %+v", u)
	}
	if got := e.column("Sheet1", "C", 1, 3); !slices.Equal(got, []string{"3", "1", "2"}) {
		t.Fatalf("rows = %v", got)
	}

	// Many sessions with large deletions keep every entry while connected.
	people := make([]*person, 8)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Big"},{"op":"import_csv","tab":"Big","at":"A1","csv":"` + strings.Repeat(`x,y,z\n`, 400) + `"}]}`)
	for i := range people {
		people[i] = e.person(fmt.Sprint("P", i))
		people[i].do(e.deleteRowsOp("Big", 1, 40))
	}
	e.clock.Add(time.Hour)
	e.tick()
	for i := len(people) - 1; i >= 0; i-- {
		if u := undo(t, people[i]); u.Applied != 1 {
			t.Fatalf("undo of deletion %d = %+v", i, u)
		}
	}
	if got := e.column("Big", "A", 399, 400); !slices.Equal(got, []string{"x", "x"}) {
		t.Fatalf("restored rows = %v", got)
	}
}

// TestUndoOtherWriterMark is C-08.
func TestUndoOtherWriterMark(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	alice, bob := e.person("Alice"), e.person("Bob")
	alice.do(e.insertRowsOp("Sheet1", 2, 1))
	bob.set("C2", "bob")
	alice.set("C2", "alice")
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo of the overwrite = %+v", u)
	}
	e.want("C2", "bob")
	if u := undo(t, alice); u.Applied != 0 || u.Reason != "written" {
		t.Fatalf("undo of the insert = %+v", u)
	}
	e.want("C2", "bob")
	e.want("A3", "2")
}

// TestCSVRowOverPage is C-10.
func TestCSVRowOverPage(t *testing.T) {
	heavy(t, "writes 4 MiB of text")
	e := start(t)
	// 129 cells of 32,767 characters in one row, in three calls under the
	// byte bound of a call.
	text := strings.Repeat("a", 32767)
	for start := 0; start < 129; start += 43 {
		cells := make([]string, 43)
		for i := range cells {
			cells[i] = `"` + text + `"`
		}
		at := columnLetters(start) + "1"
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + at + `","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
	}
	first := e.read(ReadQuery{Tab: "Sheet1", Format: "csv"})
	next, ok := first["next"].(map[string]any)
	if !ok {
		t.Fatal("the row was read in one page")
	}
	second := e.read(ReadQuery{Cursor: next["cursor"].(string)})
	if _, more := second["next"]; more {
		t.Fatal("the row needs more than two pages")
	}
	want := strings.Repeat(text+",", 128) + text + "\r\n"
	if got := first["csv"].(string) + second["csv"].(string); got != want {
		t.Fatalf("joined pages have %d bytes, want %d", len(got), len(want))
	}
	if !strings.HasSuffix(first["csv"].(string), ",") {
		t.Fatal("the first page does not end after a comma")
	}
	again := e.read(ReadQuery{Cursor: next["cursor"].(string)})
	if again["csv"] != second["csv"] || again["start"] != second["start"] {
		t.Fatal("repeating a cursor gave another page")
	}
}

func columnLetters(col int) string {
	out := ""
	for n := col + 1; n > 0; n = (n - 1) / 26 {
		out = string(rune('A'+(n-1)%26)) + out
	}
	return out
}

// TestAgentRevisions is C-12.
func TestAgentRevisions(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"x"]]}]}`)
	n := e.b.importAll(t)
	call := `{"state":"` + e.state() + `","op_id":"same-1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"x"]]}]}`
	same := e.mustAgent(call)
	if got := e.b.importAll(t); got != n+1 {
		t.Fatalf("a call that writes equal values recorded %d revisions", got-n)
	}
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":1},{"op":"delete_rows","tab":"Sheet1","at":1,"count":1}]}`)
	if got := e.b.importAll(t); got != n+2 {
		t.Fatalf("a call whose operations cancel recorded %d revisions", got-n-1)
	}
	seq := e.seq()
	replay := e.mustAgent(call)
	if !replay.Replay || replay.State != same.State || e.seq() != seq {
		t.Fatalf("replay = %+v", replay)
	}
	if got := e.b.importAll(t); got != n+2 {
		t.Fatal("a replayed call recorded a revision")
	}
	_, err := e.agent(`{"op_id":"same-1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2]]}]}`)
	e.wantRefusal(err, "conflict", "op_id_reuse")
	// The replay protection survives a restart.
	e.restart()
	replay = e.mustAgent(call)
	if !replay.Replay {
		t.Fatal("the stored result was lost in the restart")
	}
}

// TestGuards is C-13.
func TestGuards(t *testing.T) {
	setup := func(t *testing.T) (*env, string, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Summary"},{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1],[2]]},{"op":"write","tab":"Other","at":"A1","rows":[[2],[1]]},
			{"op":"write","tab":"Summary","at":"A1","rows":[["=Data!A1"]]}]}`)
		return e, e.state(), e.person("Bob")
	}
	guarded := func(state, rng string) string {
		return `{"state":"` + state + `","guard":{"ranges":["` + rng + `"]},"ops":[{"op":"write","tab":"Summary","at":"B1","rows":[["w"]]}]}`
	}
	t.Run("unchanged", func(t *testing.T) {
		e, state, _ := setup(t)
		e.clock.Add(24 * time.Hour)
		_, err := calculated(func() (WriteResult, error) {
			return e.agent(`{"state":"` + state + `","guard":{"unchanged":true},"ops":[{"op":"write","tab":"Summary","at":"B1","rows":[["w"]]}]}`)
		})
		e.wantRefusal(err, "conflict", "guard")
	})
	t.Run("a", func(t *testing.T) {
		e, state, _ := setup(t)
		e.mustAgent(`{"ops":[{"op":"format","tab":"Summary","ranges":["A1"],"format":{"bold":true}}]}`)
		if _, err := e.agent(guarded(state, "Summary!A1")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("b", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.set("Data!A1", "9")
		e.want("Summary!A1", "9")
		if _, err := e.agent(guarded(state, "Summary!A1")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("c", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.set("Summary!A1", "=Data!A2")
		_, err := e.agent(guarded(state, "Summary!A1"))
		e.wantRefusal(err, "conflict", "guard")
		if refusal(err).Name != "Summary!A1" {
			t.Fatalf("the guard names %q", refusal(err).Name)
		}
	})
	for _, restart := range []bool{false, true} {
		suffix := ""
		if restart {
			suffix = "/f"
		}
		t.Run("d"+suffix, func(t *testing.T) {
			e, state, bob := setup(t)
			bob.do(e.insertRowsOp("Data", 1, 1))
			e.wantText("Summary!A1", "=Data!A2")
			if restart {
				e.restart()
			}
			_, err := e.agent(guarded(state, "Summary!A1"))
			e.wantRefusal(err, "conflict", "guard")
		})
		t.Run("e"+suffix, func(t *testing.T) {
			e, state, bob := setup(t)
			bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Info"}`, e.tabID("Data")))
			if restart {
				e.restart()
			}
			_, err := e.agent(guarded(state, "Summary!A1"))
			e.wantRefusal(err, "conflict", "guard")
		})
	}
	t.Run("g", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.do(e.insertRowsOp("Summary", 1, 1))
		_, err := e.agent(guarded(state, "Summary!A1"))
		e.wantRefusal(err, "conflict", "structure_changed")
	})
	t.Run("h", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.do(e.sortOp("Other", 1, 2, 1, false))
		_, err := e.agent(guarded(state, "Other!A1"))
		e.wantRefusal(err, "conflict", "structure_changed")
	})
}

// TestQuotaReserve is C-03 and L-16: with a quota that leaves room for
// little more than one revision of a chunk, actions that add 30 KiB cells
// to that chunk are refused from the point where the cumulative reserve
// passes the quota, and closing the group then never passes it.
func TestQuotaReserve(t *testing.T) {
	e := start(t)
	alice, bob := e.person("Alice"), e.person("Bob")
	alice.set("A1", "seed")
	usage := func() int64 {
		var total int64
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		err := e.in.transaction(context.Background(), func(tx *sql.Tx) error {
			var err error
			total, _, err = e.in.quotaUsage(context.Background(), tx, e.in.wb, 0, 0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return total
	}
	quota := usage() + 400<<10
	e.b.setQuota(quota)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("q", 30<<10)
	applied, seq := 0, e.seq()
	var refused ackBody
	for i := 2; i <= 21; i++ {
		ack := alice.set(fmt.Sprintf("A%d", i), text)
		if status(ack) != "applied" {
			refused = ack
			break
		}
		applied++
		seq = e.seq()
	}
	if status(refused) != "refused:disk_quota" || applied == 0 || applied >= 20 {
		t.Fatalf("%d actions applied, then %q", applied, status(refused))
	}
	// The refusal reached nobody: the sequence did not move, and the
	// refused cell is blank.
	if e.seq() != seq || e.shown(fmt.Sprintf("A%d", applied+2)) != "" {
		t.Fatal("the refused edit changed the sheet")
	}
	// While over the reserve every edit is refused, a deletion included.
	if ack := bob.do(e.deleteRowsOp("Sheet1", 500, 1)); status(ack) != "refused:disk_quota" {
		t.Fatalf("delete at the quota: %s", status(ack))
	}
	// Closing the group records the revision within the quota.
	e.clock.Add(time.Minute)
	e.tick()
	onDisk := func() int64 {
		var total int64
		for _, name := range []string{dbName, dbName + "-wal"} {
			if info, err := os.Stat(filepath.Join(e.dir, name)); err == nil {
				total += info.Size()
			}
		}
		e.b.mu.Lock()
		defer e.b.mu.Unlock()
		for _, data := range e.b.blobs {
			total += int64(len(data))
		}
		return total
	}
	if got := onDisk(); got > quota {
		t.Fatalf("%d bytes on disk pass the quota %d after the group closed", got, quota)
	}
	if n := e.b.importAll(t); e.b.entry(n).Kind != "person" {
		t.Fatalf("the group did not close as a revision: %+v", e.b.entry(n).Kind)
	}
	if got := onDisk(); got > quota {
		t.Fatalf("%d bytes on disk pass the quota %d after the import", got, quota)
	}
	// A rollback is never refused for the quota; TestRollbackAtFullQuota
	// covers it in full.
	e.b.setQuota(1)
	if _, err := e.b.Rollback(context.Background(), 1, "Alice"); err != nil {
		t.Fatalf("rollback at a full quota: %v", err)
	}
	e.want("A1", "")
	if ack := alice.set("A1", "x"); status(ack) != "refused:disk_quota" {
		t.Fatalf("an edit over the quota: %s", status(ack))
	}
}

// TestRollbackAtFullQuota rolls back a sheet whose flat is at its disk
// quota (spec rule 50). An edit is refused there and the rollback is not.
// It restores the revision whole, its revision names the files of the one
// it restored and adds no blob, and the sheet stays over its quota, where
// every edit is still refused.
func TestRollbackAtFullQuota(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	alice.do(e.setOp("A1", "first"), e.setOp("B1", "=LEN(A1)"))
	rev := closeRevision(e)
	alice.do(e.setOp("A1", "second"), e.setOp("C1", strings.Repeat("q", 30<<10)))
	alice.do(e.insertRowsOp("Sheet1", 1, 1))
	closeRevision(e)
	blobs := func() int {
		e.b.mu.Lock()
		defer e.b.mu.Unlock()
		return len(e.b.blobs)
	}
	stored := blobs()
	e.b.setQuota(1)
	if err := e.in.Remeasure(context.Background()); err != nil {
		t.Fatal(err)
	}
	seq := e.seq()
	if ack := alice.set("D1", "x"); status(ack) != "refused:disk_quota" || e.seq() != seq {
		t.Fatalf("an edit at a full quota: %s", status(ack))
	}
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
		t.Fatalf("rollback at a full quota: %v", err)
	}
	wantTexts(t, e, map[string]string{"A1": "first", "B1": "=LEN(A1)", "C1": "", "A2": ""})
	e.want("B1", "5")
	n := e.b.importAll(t)
	if entry := e.b.entry(n); entry.Kind != "rollback" || !slices.Equal(entry.Files, e.b.entry(rev).Files) {
		t.Fatalf("revision %d is %s and does not share the files of revision %d", n, entry.Kind, rev)
	}
	if got := blobs(); got != stored {
		t.Fatalf("the rollback added %d blobs", got-stored)
	}
	if ack := alice.set("D1", "x"); status(ack) != "refused:disk_quota" {
		t.Fatalf("an edit after the rollback, over the quota: %s", status(ack))
	}
	e.restart()
	e.wantText("A1", "first")
	e.want("B1", "5")
}

// TestIdleSince covers what core needs to stop an unused instance: the
// instance reports since when it has no connection, no open edit group, an
// empty outbox and no live capture.
func TestIdleSince(t *testing.T) {
	e := start(t)
	e.tick()
	if !e.in.Stats().IdleSince.IsZero() {
		t.Fatal("an instance with an unimported revision reports idle")
	}
	e.b.importAll(t)
	e.tick()
	since := e.in.Stats().IdleSince
	if since.IsZero() || !e.in.Stats().Loaded {
		t.Fatalf("stats = %+v", e.in.Stats())
	}
	e.clock.Add(time.Minute)
	e.tick()
	if got := e.in.Stats().IdleSince; !got.Equal(since) {
		t.Fatalf("idle since %v, then %v", since, got)
	}
	// An open edit group, and later its unimported revision, end the idle time.
	e.person("Alice").set("A1", "1")
	e.tick()
	if !e.in.Stats().IdleSince.IsZero() {
		t.Fatal("an instance with an open edit group reports idle")
	}
	e.clock.Add(time.Minute)
	e.tick()
	if e.b.importAll(t) != 2 {
		t.Fatal("the idle group did not close as a revision")
	}
	e.tick()
	if e.in.Stats().IdleSince.IsZero() {
		t.Fatal("the instance is idle again after the import")
	}
}
