package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// recordSheet is a stopped sheet with a row in each table that holds text
// or a blob beside the content: a session with a receipt, an agent call
// stored under its op_id, an outbox entry with its snapshots and a group,
// and the token of the session and the call.
type recordSheet struct {
	e     *env
	token string
	sid   string
	call  string
}

func newRecordSheet(t *testing.T) *recordSheet {
	t.Helper()
	e := start(t)
	s, token, err := e.in.newSession(context.Background(), "editor:Ann", "Ann", false)
	if err != nil {
		t.Fatal(err)
	}
	ann := &person{e: e, s: s}
	ann.set("A1", "ann")
	call := `{"state":"` + e.state() + `","op_id":"call-1","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["=1/0"]]}]}`
	if _, err := e.agent(call); err != nil {
		t.Fatal(err)
	}
	e.in.Stop()
	return &recordSheet{e: e, token: token, sid: s.id, call: call}
}

// opened opens a copy of the sheet after the statements ran on it.
func (r *recordSheet) opened(t *testing.T, statements ...string) *env {
	t.Helper()
	e := copied(t, r.e, statements...)
	e.b, e.clock = r.e.b, r.e.clock
	e.open()
	t.Cleanup(func() { e.in.Stop() })
	return e
}

// readAfterLoad opens the sheet of e, waits for its load and then runs
// read, and returns the refusal of the open, the load's error and the
// read's, and the bytes all of it allocated. A refused open leaves nothing
// to read.
func readAfterLoad(t *testing.T, e *env, read func(*env) error) (openErr, loadErr, readErr error, allocs int64) {
	t.Helper()
	allocs = allocatedBy(func() {
		in, _, err := Open(e.dir, e.b, e.opts)
		if err != nil {
			openErr = err
			return
		}
		e.in, e.b.in = in, in
		loadErr = in.await(context.Background(), 0)
		readErr = read(e)
	})
	if e.in != nil {
		t.Cleanup(e.in.Stop)
	}
	return openErr, loadErr, readErr, allocs
}

// Each field beside the content is read only when it is no longer than
// the writer stores it: one of 8 MiB in an outbox row, in the file list,
// in a snapshot, in a stored call, a receipt, a session or a group is
// refused when the sheet opens or by its load, which checks every table,
// and again where a read outside the writer reads it, which allocates far
// less than the field. The undamaged rows read.
func TestOversizedRecordFieldsAreNotRead(t *testing.T) {
	const n = 8 << 20
	r := newRecordSheet(t)
	ctx := context.Background()
	outbox := func(e *env) error { _, err := e.in.Outbox(ctx, 0, 0); return err }
	refs := func(e *env) error { _, err := e.in.References(ctx); return err }
	snapshot := func(e *env) error {
		var hash string
		if err := e.in.rdb.QueryRow(`SELECT hash FROM snapshots WHERE length(hash)=64 LIMIT 1`).Scan(&hash); err != nil {
			hash = contentHash(nil)
		}
		_, err := e.in.SnapshotData(ctx, hash)
		return err
	}
	replay := func(e *env) error { _, err := e.agent(r.call); return err }
	receipt := func(e *env) error {
		_, err := e.in.committedAck(ctx, &session{id: r.sid}, 1, nil)
		return err
	}
	resume := func(e *env) error { _, err := e.in.resumeSession(ctx, r.token); return err }
	// The tick closes groups that are an hour old. The damaged group
	// staying is the refusal; a tick that closed it read it, and a count
	// that cannot be read fails the test.
	groups := func(e *env) error {
		count := func() int {
			var n int
			if err := e.in.rdb.QueryRow(`SELECT count(*) FROM groups`).Scan(&n); err != nil {
				t.Fatalf("the groups cannot be counted: %v", err)
			}
			return n
		}
		if before := count(); before != 1 {
			t.Fatalf("the sheet holds %d groups", before)
		}
		e.clock.Add(time.Hour)
		err := e.in.execute(ctx, &work{run: func() error {
			e.in.maintain()
			return nil
		}})
		if count() != 1 {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("the tick left the group")
		}
		return err
	}
	group := `INSERT INTO groups(editor,label,verified,started,last,dirty) VALUES(%s,%s,0,0,0,1)`
	cases := []struct {
		name, statement string
		read            func(*env) error
	}{
		{"an outbox kind", `UPDATE outbox SET kind=CAST(zeroblob(%d) AS TEXT)`, outbox},
		{"outbox authors", `UPDATE outbox SET authors=CAST(zeroblob(%d) AS TEXT)`, outbox},
		{"an outbox message", `UPDATE outbox SET message=CAST(zeroblob(%d) AS TEXT)`, outbox},
		{"an outbox op_id", `UPDATE outbox SET op_id=CAST(zeroblob(%d) AS TEXT)`, outbox},
		{"an outbox date", `UPDATE outbox SET calc_date=CAST(zeroblob(%d) AS TEXT)`, outbox},
		{"outbox tabs", `UPDATE outbox SET tabs=CAST(zeroblob(%d) AS TEXT)`, outbox},
		{"an outbox file list", `UPDATE outbox SET files=zeroblob(%d)`, outbox},
		{"the file list of meta", `UPDATE meta SET files=zeroblob(%d)`, refs},
		{"the file list of an outbox row", `UPDATE outbox SET files=zeroblob(%d)`, refs},
		{"a snapshot", `UPDATE snapshots SET data=zeroblob(%d)`, snapshot},
		{"a snapshot's hash", `UPDATE snapshots SET hash=CAST(zeroblob(%d) AS TEXT) WHERE rowid=(SELECT min(rowid) FROM snapshots)`, snapshot},
		{"a stored call's outcome", `UPDATE commands SET outcome=zeroblob(%d) WHERE op_id='call-1'`, replay},
		{"a stored call's hash", `UPDATE commands SET hash=CAST(zeroblob(%d) AS TEXT) WHERE op_id='call-1'`, replay},
		{"a receipt's outcome", `UPDATE receipts SET outcome=zeroblob(%d)`, receipt},
		{"a receipt's hash", `UPDATE receipts SET hash=zeroblob(%d)`, receipt},
		{"a session's editor", `UPDATE sessions SET editor=CAST(zeroblob(%d) AS TEXT)`, resume},
		{"a session's label", `UPDATE sessions SET label=CAST(zeroblob(%d) AS TEXT)`, resume},
		{"a group's editor", fmt.Sprintf(group, "CAST(zeroblob(%d) AS TEXT)", "'Ann'"), groups},
		{"a group's label", fmt.Sprintf(group, "'editor:Ann'", "CAST(zeroblob(%d) AS TEXT)"), groups},
	}
	_, _, _, baseline := readAfterLoad(t, copied(t, r.e), func(*env) error { return nil })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := copied(t, r.e, fmt.Sprintf(c.statement, n))
			e.b, e.clock = r.e.b, r.e.clock
			openErr, loadErr, readErr, got := readAfterLoad(t, e, c.read)
			if openErr != nil {
				t.Logf("the open refused the sheet after %d KiB: %v", got>>10, openErr)
			} else {
				if loadErr == nil {
					t.Error("the sheet loaded")
				}
				if readErr == nil {
					t.Error("the field was read")
				}
				t.Logf("refused after %d KiB, %d KiB for an undamaged sheet: %v; %v", got>>10, baseline>>10, loadErr, readErr)
			}
			if measures && got > baseline+n/16 {
				t.Errorf("the refused read allocated %d KiB for a field of %d KiB, %d KiB without it", got>>10, n>>10, baseline>>10)
			}
		})
	}
	t.Run("undamaged", func(t *testing.T) {
		e := r.opened(t)
		for name, read := range map[string]func(*env) error{"outbox": outbox, "references": refs, "snapshot": snapshot,
			"replay": replay, "session": resume} {
			if err := read(e); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := readReceipt(ctx, e.in.rdb, &session{id: r.sid}, 1); err != nil {
			t.Errorf("receipt: %v", err)
		}
		// A group the writer stores closes with the tick, which the groups
		// read reports as the group read.
		if err := groups(r.opened(t, fmt.Sprintf(group, "'editor:Bob'", "'Bob'"))); err != nil {
			t.Errorf("an undamaged group: %v", err)
		}
	})
}

// The bounds of those fields hold the largest the writer stores: authors
// of the most editors a revision names with labels JSON writes at six
// bytes a byte, a message of 1,000 four-byte code points, the tabs and new
// files of an entry, a file list of the most files of a revision, a stored
// call of the most errors at a tab of the longest escaped name, a receipt
// outcome at its limit and the largest revision file.
func TestRecordFieldBoundsHoldTheWritersLargest(t *testing.T) {
	label := strings.Repeat("<", maxLabelChars)
	authors := make([]string, maxRevisionAuthors)
	for i := range authors {
		authors[i] = label
	}
	authorsJSON, _ := json.Marshal(authors)
	tabs := make([]uint64, 2*maxTabs)
	for i := range tabs {
		tabs[i] = maxID - 1 - uint64(i)
	}
	hashes := make([]string, maxRevisionFiles)
	files := make([]File, maxRevisionFiles)
	for i := range hashes {
		hashes[i] = contentHash([]byte{byte(i), byte(i >> 8)})
		files[i] = File{Path: chunkPrefix(maxID-1, maxID-1) + "9223372036854775807.json", Hash: hashes[i], Size: int64(maxPartBytes)}
	}
	tabsJSON, _ := json.Marshal(outboxTabs{Tabs: tabs, New: hashes})
	filesJSON, _ := json.Marshal(files)
	name := strings.Repeat("'", maxTabName/2) + strings.Repeat("<", maxTabName/2)
	res := WriteResult{State: stateToken(strings.Repeat("f", 32), math.MaxUint64), CellsWritten: math.MaxInt64,
		FormulasWritten: math.MaxInt64, FormulaErrors: math.MaxInt64, Created: true, Replay: true}
	for range maxResultErrors {
		res.Errors = append(res.Errors, FormulaError{At: formula.Range{Tab: name, First: formula.Pos{Row: formula.MaxRows - 1, Col: formula.MaxCols - 1}, Last: formula.Pos{Row: formula.MaxRows - 1, Col: formula.MaxCols - 1}}.String(), Error: formula.ErrValue.String()})
	}
	outcome, _ := json.Marshal(commandOutcome{Result: res, Outbox: math.MaxInt64})
	for _, c := range []struct {
		name      string
		size, max int
	}{
		{"authors", len(authorsJSON), maxAuthorsBytes},
		{"message", len(strings.Repeat("\U0010FFFF", 1000)), maxMessageBytes},
		{"kind", len("rollback"), maxKindBytes},
		{"tabs", len(tabsJSON), maxOutboxTabsBytes},
		{"file list", len(filesJSON), maxFileListBytes},
		{"stored call", len(outcome), maxCommandOutcomeBytes},
		{"label", len(strings.Repeat("\U0010FFFF", maxLabelChars)), maxLabelBytes},
	} {
		if c.size > c.max {
			t.Errorf("the writer's largest %s takes %d bytes, past its bound of %d", c.name, c.size, c.max)
		}
	}
}

// A revision names the first editors of its groups, at most
// maxRevisionAuthors of them. A rollback keeps the first maxLabelChars
// code points of its label and refuses an op_id longer than a call's.
// The outbox rows the writer stores at those limits read back.
func TestRecordsAtTheirLimitsReadBack(t *testing.T) {
	slow(t, "edits as 300 people")
	e := start(t)
	for i := range maxRevisionAuthors + 44 {
		p := e.person(fmt.Sprintf("p%03d", i))
		p.set(fmt.Sprintf("A%d", i+1), "x")
		e.in.release(p.s)
	}
	rev := closeRevision(e)
	if got := e.b.entry(rev).Authors; len(got) != maxRevisionAuthors || got[0] != "p000" {
		t.Fatalf("the revision names %d editors, first %.20q", len(got), got[0])
	}
	files, _, release, err := e.b.RevisionFiles(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := e.in.Rollback(context.Background(), files, "Ann", strings.Repeat("x", maxOpIDBytes+1)); refusal(err).Code != "invalid" {
		t.Fatalf("a rollback with an op_id of 129 bytes answered %v", err)
	}
	long := strings.Repeat("\U0001F600", maxLabelChars+100)
	if err := e.in.Rollback(context.Background(), files, long, "sheet-rollback:1:x"); err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("\U0010FFFF", 1000)
	quoted, _ := json.Marshal(message)
	e.mustAgent(jsonf(`{"message":%s,"op_id":"long-1","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["=1/0"]]}]}`, quoted))
	entries, err := e.in.Outbox(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries, func(o OutboxEntry) bool { return o.Kind == "rollback" })
	if i < 0 {
		t.Fatal("the outbox holds no rollback entry")
	}
	if got := entries[i].Authors; len(got) != 1 || got[0] != long[:4*maxLabelChars] {
		t.Fatalf("the rollback entry names %d authors, the first of %d bytes", len(got), len(got[0]))
	}
	if last := entries[len(entries)-1]; last.Message != message || last.OpID != "long-1" {
		t.Fatalf("the agent entry holds %.20q and %q", last.Message, last.OpID)
	}
}
