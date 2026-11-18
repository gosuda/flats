package sheet

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// wireIndex registers a format in the shared wire list under the journal,
// so that a reverted action leaves the list as every connection has it.
func (x *txn) wireIndex(f formula.Format) {
	w := &x.wb.wire
	if _, ok := w.idx[f]; ok {
		return
	}
	n := len(w.list)
	w.index(f)
	x.j.push(func() {
		for _, added := range w.list[n:] {
			delete(w.idx, added)
		}
		w.list = cut(w.list, n, len(w.list))
	})
}

// Rollback installs the exact content of a retained revision, whose files
// core has leased, as a new state (spec rule 50). It checks no limit and no
// quota, adds no blob and records a forced "rollback" outbox entry that
// names the revision's file list. label is the author, of which the entry
// keeps the first maxLabelChars code points, as a session keeps its label.
// A call with an opID the sheet has stored changes nothing.
//
// The outbox entry carries opID and leaves RollbackOf zero: core knows the
// revision it rolled back to from the opID it chose, which is at most as
// long as an agent call's.
func (in *Instance) Rollback(ctx context.Context, files []File, label, opID string) error {
	if len(opID) > maxOpIDBytes {
		return invalidf("op_id", "an op_id is at most 128 bytes")
	}
	label = strings.ToValidUTF8(label, "\uFFFD")
	n := 0
	for i := range label {
		if n == maxLabelChars {
			label = label[:i]
			break
		}
		n++
	}
	if err := in.await(ctx, agentPatience); err != nil {
		return err
	}
	if err := in.syncDate(ctx); err != nil {
		return err
	}
	return in.execute(ctx, &work{soon: true, run: func() error { return in.rollback(ctx, files, label, opID) }})
}

func (in *Instance) rollback(ctx context.Context, files []File, label, opID string) error {
	if opID != "" {
		var n int
		if err := in.db.QueryRowContext(ctx, `SELECT count(*) FROM commands WHERE op_id=?`, opID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	// A full outbox refuses the rollback before its target is built, and
	// again in the transaction that would append to it.
	if err := outboxAdmission(ctx, in.db); err != nil {
		return err
	}
	defer in.beginPass(0)()
	// The request and a stop of the instance end the rollback, in line at
	// the gate and at its next check point, before anything of it is
	// stored.
	pass, done := in.passContext(ctx)
	defer done()
	stopped := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return unavailable("stopped")
	}
	// Only the writer changes the live workbook, and the rollback runs on
	// the writer. So the pass reads it without the lock, and readers keep
	// it until the swap.
	live := in.wb
	date, seq := live.date, live.seq+1
	// The target workbook is built, calculated and made the next state of
	// the live one beside it. The build takes turns at the gate with the
	// passes of other sheets from its first file on, as the build of a
	// revision view does, and holds no lock, because the target is its own.
	turn, ok := calcSlots.enter(in, pass)
	defer turn.leave()
	if !ok {
		return stopped()
	}
	target, err := loadRevision(files, in.revisionBlobs(pass, turn), turn.point)
	if pass.Err() != nil {
		return stopped()
	}
	if err != nil {
		return &WriteError{Code: "unavailable", Reason: "revision", Retry: true, cause: err}
	}
	target.date = date
	budget := turn.budget()
	var out calcOut
	cerr := target.buildDeps(budget)
	if cerr == nil {
		out, cerr = turn.recalc(target, calcReq{all: true, budget: budget})
	}
	adopted := false
	if cerr == nil {
		target.apply(out, nil, 0)
		adopted = target.adoptFrom(live, seq, func() bool { return turn.point("adopt") })
		if adopted {
			// The rows are put in position order before the swap, as a load
			// does, so that readers do not wait for it.
			target.settle()
		}
	}
	turn.leave()
	in.recalcs.Add(1)
	in.observeCalc(false, out.took)
	// A stop or the end of the request seen after the calculation, at the
	// gate or after it, ends the rollback before anything of it is stored.
	// pass.Err reads the stop of the instance itself (passContext), so a
	// stop is seen here also while its forward to pass has not run.
	if pass.Err() != nil {
		return stopped()
	}
	if cerr != nil {
		// A retained revision met the read limits when it was recorded.
		return &WriteError{Code: "unavailable", Reason: "revision", cause: cerr}
	}
	if !adopted {
		return stopped()
	}

	in.mu.Lock()
	if pass.Err() != nil {
		in.mu.Unlock()
		return stopped()
	}
	undo := func() {}
	// The transaction runs under the pass, so that a stop or the end of
	// the request before its commit stores nothing.
	err = in.transaction(pass, func(tx *sql.Tx) error {
		// A rollback appends to the outbox as an agent call does, so it
		// waits as one does for the host to import it.
		if err := outboxAdmission(pass, tx); err != nil {
			return err
		}
		// Open groups close as one revision before the rollback applies.
		var err error
		if _, undo, err = in.closeGroups(pass, tx, true); err != nil {
			return err
		}
		if err := writeAll(pass, tx, target); err != nil {
			return err
		}
		authors := []string{}
		if label != "" {
			authors = append(authors, label)
		}
		outbox, err := in.insertOutbox(pass, tx, target, boundary{kind: "rollback", opID: opID, authors: authors}, nil, nil)
		if err != nil {
			return err
		}
		if opID != "" {
			if _, err = tx.ExecContext(pass, `INSERT INTO commands(op_id,hash,outcome,seq,outbox_seq,imported,expires) VALUES(?,'','{}',?,?,0,0)`, opID, seq, outbox); err != nil {
				return err
			}
		}
		// A stop or the end of the request until here leaves nothing
		// stored.
		return pass.Err()
	})
	if err != nil {
		undo()
		in.mu.Unlock()
		if pass.Err() != nil {
			return stopped()
		}
		return asWriteError(err)
	}
	in.wb = target
	in.applied.Add(1)
	in.structs.clear(seq)
	in.clearUndo()
	in.hub.append(&deltaRec{seq: seq, reset: true, size: 64}, in.now())
	in.mu.Unlock()
	in.hub.wake()
	in.afterCommit(true)
	return nil
}

// adoptFrom makes a workbook built from revision files the next state of
// the live sheet (section 16.3): ids that are live keep their born stamp,
// every other stamp becomes the rollback's sequence, and the id counters
// stay as they are. It runs at the gate before the swap and changes only
// wb. more is its check point, every loadCheckCells styles, ids, widths or
// cells, and it reports false when more ended it, with wb left to drop.
func (wb *Workbook) adoptFrom(live *Workbook, seq uint64, more func() bool) bool {
	pace := loadPace{pause: more}
	step := func(units int) bool { return pace.step(units) }
	wb.store = live.store
	wb.seq, wb.resetSeq, wb.tabChangeSeq, wb.tabOrderSeq = seq, seq, seq, seq
	wb.guardFloor = live.guardFloor
	wb.nextTab, wb.nextRow, wb.nextCol = max(wb.nextTab, live.nextTab), max(wb.nextRow, live.nextRow), max(wb.nextCol, live.nextCol)
	// Every format gets one state whose property stamps are the rollback's.
	styles := newStyleTable()
	remap := map[uint32]uint32{}
	for id, e := range wb.styles.byID {
		if !pace.step(1) {
			return false
		}
		state := styleState{f: e.f}
		for prop := range state.stamps {
			state.stamps[prop] = seq
		}
		remap[id] = styles.intern(state)
	}
	wb.styles = styles
	for _, t := range wb.tabs {
		was := live.byID[t.id]
		t.born = seq
		if was != nil {
			t.born = was.born
		}
		t.nameSeq, t.settingsSeq, t.structSeq = seq, seq, seq
		stamp := func(a *axis, old *axis) bool {
			sorted, ok := sortedIDs(a.ids, step)
			if !ok {
				return false
			}
			a.born, ok = bornAfter(sorted, old, seq, &pace)
			return ok
		}
		var oldRows, oldCols *axis
		if was != nil {
			oldRows, oldCols = &was.rows, &was.cols
		}
		if !stamp(&t.rows, oldRows) || !stamp(&t.cols, oldCols) {
			return false
		}
		for col, w := range t.widths {
			if !pace.step(1) {
				return false
			}
			t.widths[col] = width{w.px, seq}
		}
		for _, rd := range t.data {
			for _, s := range rd.cells {
				if !pace.step(1) {
					return false
				}
				s.c.seq, s.c.textSeq, s.c.by = seq, seq, 0
				if s.c.style != 0 {
					s.c.style = remap[s.c.style]
					wb.styles.ref(s.c.style, 1)
				}
			}
		}
	}
	return true
}

// recoverTab adds one tab of a retained revision to the live sheet, as an
// edit of the person's edit group with an undo entry (spec rule 52). s is
// nil when the request names no session; the edit is then credited to
// label and has no undo entry.
func (in *Instance) recoverTab(ctx context.Context, s *session, label string, files []File, tab TabID) (TabID, error) {
	if err := in.await(ctx, agentPatience); err != nil {
		return 0, err
	}
	if err := in.syncDate(ctx); err != nil {
		return 0, err
	}
	source, err := in.buildSource(ctx, files)
	if err != nil {
		return 0, err
	}
	src := source.byID[tab]
	if src == nil {
		return 0, &WriteError{Code: "not_found", Reason: "tab"}
	}
	var id TabID
	err = in.execute(ctx, &work{soon: true, run: func() error {
		var other int64
		if in.b.Limits().DiskQuota > 0 {
			var ok bool
			if other, ok = in.measure.get(); !ok {
				in.measure.refresh()
				return unavailable("measurement")
			}
		}
		in.mu.Lock()
		wb := in.wb
		var by uint32
		editor := "browser"
		if s != nil {
			by, editor, label = s.ord, s.editor, s.author()
		}
		x := newTxn(wb, wb.seq+1, wb.seq, by)
		in.begin(x)
		err := in.transaction(ctx, func(tx *sql.Tx) error {
			if err := outboxAdmission(ctx, tx); err != nil {
				return err
			}
			t, werr := x.recover(source, src)
			if werr != nil {
				return werr
			}
			id = t.id
			if werr := in.finish(ctx, tx, x, other); werr != nil {
				return werr
			}
			if werr := in.admitUndo(s, x.entry); werr != nil {
				in.scratchRefused(werr)
				return werr
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO groups(editor,label,verified,started,last,dirty) VALUES(?,?,?,?,?,1)
				ON CONFLICT(editor) DO UPDATE SET label=excluded.label,last=excluded.last,dirty=1`, editor, label, s != nil && s.verified, x.now.Unix(), x.now.Unix())
			return err
		})
		if err != nil {
			x.revert()
			// The entry admitted for the recovery reached no stack. It and
			// its scratch file go now, and the sheet is brought inside its
			// bound, as after any change that does not commit.
			in.fitSheet()
			wb.settle()
			in.mu.Unlock()
			if x.calcRan {
				in.observeCalc(true, x.calcTime)
			}
			var werr *WriteError
			if errors.As(err, &werr) {
				return werr
			}
			return unavailable("storage")
		}
		in.settleStamps(x)
		in.followStructure(x)
		if s != nil && x.entry != nil {
			s.undo.push(x.entry)
			s.undo.clearRedo()
		}
		in.fitSheet()
		in.applied.Add(1)
		in.publish(x, nil)
		wb.settle()
		in.mu.Unlock()
		in.hub.wake()
		if x.calcRan {
			in.observeCalc(true, x.calcTime)
		}
		return nil
	}})
	return id, err
}

// revisionBlobs returns the reader of revision files of a build at the
// gate: it looks at the gate before each file, and its readers end with
// the pass.
func (in *Instance) revisionBlobs(pass context.Context, turn *calcTurn) func(hash string) (io.ReadCloser, error) {
	return func(hash string) (io.ReadCloser, error) {
		if !turn.check() {
			return nil, pass.Err()
		}
		return in.b.OpenBlob(pass, hash)
	}
}

// buildSource builds the revision workbook a tab is recovered from. The
// build takes turns at the gate with the passes of other sheets, as the
// build of a revision view does, and ends with the request and with the
// instance. It leaves the gate before the recovery waits for the writer.
func (in *Instance) buildSource(ctx context.Context, files []File) (*Workbook, error) {
	pass, done := in.passContext(ctx)
	defer done()
	stopped := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return unavailable("stopped")
	}
	turn, ok := calcSlots.enter(in, pass)
	defer turn.leave()
	if !ok {
		return nil, stopped()
	}
	source, err := loadRevision(files, in.revisionBlobs(pass, turn), turn.point)
	if pass.Err() != nil {
		return nil, stopped()
	}
	if err != nil {
		return nil, &WriteError{Code: "unavailable", Reason: "revision", Retry: true, cause: err}
	}
	return source, nil
}

// recover builds the recovered tab from a tab of a revision workbook and
// places it at the end. A tab whose id is not live keeps its id and its
// row and column ids; a tab that still exists comes back as a copy with
// new ids. Formulas keep their stored form, except that a tab that comes
// back under another name has the references to its own name rewritten.
func (x *txn) recover(source *Workbook, src *Tab) (*Tab, *WriteError) {
	wb := x.wb
	if len(wb.tabs) >= maxTabs {
		return nil, capacity("tabs", maxTabs, int64(len(wb.tabs)+1))
	}
	t := &Tab{id: src.id, name: src.name, born: x.seq, nameSeq: x.seq, settingsSeq: x.seq, structSeq: x.seq,
		frozenRows: src.frozenRows, frozenCols: src.frozenCols, widths: map[ColID]width{}, data: map[RowID]*rowData{}, chunks: []RowID{0},
		recovered: x.seq, mark: writeMark{by: x.by, last: x.seq}}
	rowID := func(id uint64) uint64 { return id }
	colID := rowID
	if wb.byID[src.id] == nil {
		if err := x.keepsIDs(src); err != nil {
			return nil, err
		}
	}
	rows, cols := slices.Clone(src.rows.ids), slices.Clone(src.cols.ids)
	if wb.byID[src.id] != nil {
		next := [3]uint64{wb.nextTab, wb.nextRow, wb.nextCol}
		if wb.nextTab+1 >= maxID || wb.nextRow+uint64(len(rows)) >= maxID || wb.nextCol+uint64(len(cols)) >= maxID {
			return nil, unavailable("ids_exhausted")
		}
		x.j.push(func() { wb.nextTab, wb.nextRow, wb.nextCol = next[0], next[1], next[2] })
		t.id = TabID(wb.nextTab)
		wb.nextTab++
		rowBase, colBase := wb.nextRow, wb.nextCol
		wb.nextRow += uint64(len(rows))
		wb.nextCol += uint64(len(cols))
		rowID = func(id uint64) uint64 { return rowBase + uint64(src.rows.pos[id]) }
		colID = func(id uint64) uint64 { return colBase + uint64(src.cols.pos[id]) }
		for i := range rows {
			rows[i] = rowBase + uint64(i)
		}
		for i := range cols {
			cols[i] = colBase + uint64(i)
		}
	}
	fill := func(a *axis, ids []uint64) {
		a.ids, a.pos = ids, make(map[uint64]int32, len(ids))
		a.reindex(0)
		sorted := slices.Clone(ids)
		slices.Sort(sorted)
		forRuns(sorted, func(first, count uint64) { a.born.set(first, count, x.seq) })
	}
	fill(&t.rows, rows)
	fill(&t.cols, cols)
	for col, w := range src.widths {
		t.widths[ColID(colID(uint64(col)))] = width{w.px, x.seq}
	}
	styles := map[uint32]uint32{}
	for row, rd := range src.data {
		// The copy of a row counts as its count into a placed tab does
		// (placeTab), and the copy ends once the deadline has passed.
		x.spend(rowSteps(rd))
		if err := x.paced(); err != nil {
			return nil, err
		}
		for _, s := range rd.cells {
			c := &Cell{content: s.c.content, seq: x.seq, by: x.by}
			if s.c.f != nil {
				c.f = wb.intern(s.c.f)
				c.content = formulaContent(c.f)
			} else {
				c.val = c.content.Value()
			}
			if s.c.style != 0 {
				id, ok := styles[s.c.style]
				if !ok {
					state := styleState{f: source.styles.format(s.c.style)}
					for prop := range state.stamps {
						state.stamps[prop] = x.seq
					}
					x.wireIndex(state.f)
					id = wb.styles.intern(state)
					styles[s.c.style] = id
				}
				c.style = id
			}
			t.put(RowID(rowID(uint64(row))), ColID(colID(uint64(s.col))), c)
		}
	}
	if name := wb.restoredName(t.name); name != t.name {
		if err := x.renameOwn(t, name); err != nil {
			return nil, err
		}
	}
	order := wb.tabOrderSeq
	x.placeTab(t, len(wb.tabs))
	x.steps = append(x.steps, undoStep{kind: stepTabAdded, tab: t.id, prev: order})
	return t, nil
}

// keepsIDs checks that a tab of a revision whose id is not live can come
// back with its own ids: each lies below its counter, so that the counter
// does not give it out again and a load takes it, and no row or column id
// is live in another tab. Every id of a revision of the store is so
// (section 4.1); a damaged revision is refused before anything changes.
// Each id looked at is a step of the action's budget.
func (x *txn) keepsIDs(src *Tab) *WriteError {
	wb := x.wb
	damaged := &WriteError{Code: "unavailable", Reason: "revision"}
	tk := ticker{more: x.pace}
	below := func(ids []uint64, next uint64) bool {
		for _, id := range ids {
			if id >= next || !tk.tick() {
				return false
			}
		}
		return true
	}
	if uint64(src.id) >= wb.nextTab || !below(src.rows.ids, wb.nextRow) || !below(src.cols.ids, wb.nextCol) {
		return x.lateOr(damaged)
	}
	// Each live axis is compared from its shorter side, so the look goes
	// through at most the live ids of the sheet.
	shares := func(a, b *axis) bool {
		if len(a.ids) > len(b.ids) {
			a, b = b, a
		}
		for _, id := range a.ids {
			if _, live := b.pos[id]; live || !tk.tick() {
				return true
			}
		}
		return false
	}
	for _, t := range wb.tabs {
		if shares(&t.rows, &src.rows) || shares(&t.cols, &src.cols) {
			return x.lateOr(damaged)
		}
	}
	return nil
}

// lateOr returns the refusal of an action whose deadline ended a loop, or
// err when the loop ended by itself.
func (x *txn) lateOr(err *WriteError) *WriteError {
	if x.late() {
		return calcTimeout()
	}
	return err
}
