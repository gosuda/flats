package sheet

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

const (
	groupIdle        = 30 * time.Second
	groupMaxAge      = 5 * time.Minute
	maintenanceTick  = 5 * time.Second
	maxActionOps     = 1000
	walTruncateBytes = 4 << 20
)

// groupCommitWindow is how long people's actions can share one transaction.
// It is a variable so that a test can hold a group open.
var groupCommitWindow = 10 * time.Millisecond

// work is one item of the writer queue: a person's action, or a function
// the writer runs by itself. Entry into the queue is the server's receive
// order (technical design, section 6).
type work struct {
	ctx    context.Context
	run    func() error
	action *actionWork
	done   chan error
	// soon marks work whose caller does not wait through a recalculation
	// that no edit started. It is refused as calculating instead.
	soon bool
	// state says who has the work, which is the queue, the writer that
	// took it, or the caller that withdrew it. Work is taken or withdrawn,
	// never both.
	state atomic.Int32
}

const (
	workQueued int32 = iota
	workTaken
	workWithdrawn
)

// take gives the work to the writer. It reports false when the caller has
// withdrawn it, and the writer then does nothing for it.
func (w *work) take() bool { return w.state.CompareAndSwap(workQueued, workTaken) }

// withdraw takes the work back from the queue. It reports false when the
// writer has taken it already, and the work then ends with an outcome.
func (w *work) withdraw() bool { return w.state.CompareAndSwap(workQueued, workWithdrawn) }

// actionWork is a person's action on its way through the writer.
type actionWork struct {
	s    *session
	n    uint64
	base uint64
	ops  []personOp
	hash []byte

	ack     json.RawMessage // the stored outcome
	prevMax uint64
	stored  bool      // this run stored the receipt, in memory too
	memory  bool      // the receipt is in memory only: the session has no row
	rowed   bool      // this run stored the row of its session
	x       *txn      // set when the action changed the sheet
	rec     *deltaRec // the delta of the action's own sequence
	pre     func()    // memory effects that wait for the commit:
	post    func()    // before and after the structural log is applied

	// kept is the receipt memory keeps for a session without a row, and
	// dropped what that let go, for forget.
	kept    *keptReceipt
	dropped []*keptReceipt

	calcTime time.Duration
	calcRan  bool
}

// alone reports whether the action has a transaction of its own. An undo
// or a redo reads its session's stacks, which every earlier action of the
// session changes when it commits.
func (a *actionWork) alone() bool {
	return len(a.ops) == 1 && (a.ops[0].Op == "undo" || a.ops[0].Op == "redo")
}

// closes reports whether the action is the last of its transaction. The
// structural log, the formulas the undo stacks hold and the creation marks
// follow a structural edit when it commits, and every later action is read
// against them (technical design, sections 8.4 and 14).
func (a *actionWork) closes() bool {
	return a.alone() || (a.x != nil && len(a.x.structs) > 0)
}

// execute queues work for the writer and waits for its outcome.
//
// The writer takes no work while the sheet loads and while it is in a
// recalculation that no edit started. Work that arrives meanwhile waits
// for the end of that pass before it enters the queue, so that callers who
// come and go for hours leave nothing in it. Work marked soon waits for
// agentPatience and is then refused as calculating.
//
// In the queue the wait ends with the caller's context, and for work
// marked soon when a pass has begun since. The work is withdrawn first,
// so the answer is definite. Withdrawn work never runs, and the caller
// gets the context's error or the refusal calculating. Work the writer
// has taken cannot be withdrawn. Its outcome can be durable, so the
// caller waits for it whatever became of its context. The work itself
// runs under that context and ends with it.
func (in *Instance) execute(ctx context.Context, w *work) error {
	var patience <-chan time.Time
	var timer *time.Timer
	if w.soon {
		timer = time.NewTimer(agentPatience)
		defer timer.Stop()
		patience = timer.C
	}
	for over := in.passOver(); over != nil; over = in.passOver() {
		in.probe("pass_wait")
		select {
		case <-over:
		case <-ctx.Done():
			return ctx.Err()
		case <-in.ctx.Done():
			return unavailable("stopped")
		case <-patience:
			return unavailable("calculating")
		}
	}
	if err := in.enqueue(ctx, w); err != nil {
		return err
	}
	for {
		select {
		case err := <-w.done:
			return err
		case <-ctx.Done():
			if w.withdraw() {
				return ctx.Err()
			}
			return <-w.done
		case <-patience:
			if in.calculating() && w.withdraw() {
				return unavailable("calculating")
			}
			timer.Reset(agentPatience)
		}
	}
}

// enqueue puts work into the writer's queue under the context it runs with.
func (in *Instance) enqueue(ctx context.Context, w *work) error {
	w.ctx, w.done = ctx, make(chan error, 1)
	in.admit.RLock()
	defer in.admit.RUnlock()
	if in.ctx.Err() != nil {
		return unavailable("stopped")
	}
	in.touch()
	// The instance does not stop while this holds the read side of admit:
	// a stop cancels its context under the write side.
	select {
	case in.queue <- w:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return unavailable("writer_backpressure")
	}
}

func (in *Instance) runWriter() {
	defer in.workers.Done()
	// The load publishes the workbook before it closes ready, also when a
	// stop comes after its last check point, so the writer looks at the
	// workbook only after ready. A stopped load gets there at its next
	// check point.
	<-in.ready
	refuse := func(reason string) {
		for {
			select {
			case w := <-in.queue:
				w.done <- unavailable(reason)
			default:
				return
			}
		}
	}
	if in.wb == nil {
		// The load failed or was cancelled: nothing can be served.
		for {
			select {
			case w := <-in.queue:
				w.done <- unavailable("load")
			case <-in.ctx.Done():
				refuse("stopped")
				return
			}
		}
	}
	tick := time.NewTicker(maintenanceTick)
	defer tick.Stop()
	var next *work
	for {
		if in.ctx.Err() != nil {
			if next != nil {
				next.done <- unavailable("stopped")
			}
			refuse("stopped")
			in.finishGroups()
			return
		}
		w := next
		next = nil
		if w == nil {
			select {
			case w = <-in.queue:
			case <-tick.C:
				in.maintain()
				continue
			case <-in.ctx.Done():
				continue
			}
		}
		if err := w.ctx.Err(); err != nil {
			w.done <- err
			continue
		}
		if !w.take() {
			continue
		}
		if w.action != nil {
			next = in.commitActions(w)
			continue
		}
		w.done <- w.run()
	}
}

// begin starts the clock of an action: the time limit of its operations
// and its recalculations together (spec rule 9). The budget looks at the
// clock each time it has counted 4,096 units of work, and txn.stop looks at
// it itself.
func (in *Instance) begin(x *txn) {
	began, limit := time.Now(), in.opts.RecalcLimit
	x.now, x.log = in.now(), &in.structs
	x.overdue = func() bool { return time.Since(began) > limit }
	if in.deadline != nil {
		x.overdue = in.deadline(x)
	}
	x.budget = formula.NewBudget(x.overdue)
	in.step("begin")
}

// commitActions applies people's actions in one transaction for at most the
// group commit window and returns the work that ended the group.
func (in *Instance) commitActions(first *work) *work {
	if in.answerCommitted(first) {
		return nil
	}
	started := time.Now()
	in.mu.Lock()
	tx, err := in.db.BeginTx(in.ctx, nil)
	if err != nil {
		in.mu.Unlock()
		first.done <- unavailable("storage")
		return nil
	}
	var batch []*work
	var errs []error
	var next *work
	for w := first; ; {
		batch = append(batch, w)
		errs = append(errs, in.runAction(in.ctx, tx, w.action))
		if w.action.closes() || time.Since(started) >= groupCommitWindow {
			break
		}
		for {
			select {
			case w = <-in.queue:
			default:
				w = nil
			}
			if w == nil || w.action == nil || !in.answerCommitted(w) {
				break
			}
		}
		if w == nil {
			break
		}
		if w.action == nil || w.ctx.Err() != nil || w.action.alone() || !w.take() {
			next = w
			break
		}
	}
	observe := func() {
		for _, w := range batch {
			if w.action.calcRan {
				in.observeCalc(true, w.action.calcTime)
			}
		}
	}
	if err := in.commit(tx); err != nil {
		for i := len(batch) - 1; i >= 0; i-- {
			if x := batch[i].action.x; x != nil {
				x.revert()
			}
			if errs[i] == nil {
				batch[i].action.s.undo.pushing, batch[i].action.s.undo.clearing = 0, false
				batch[i].action.forget()
			}
		}
		in.fitSheet()
		in.wb.settle()
		in.mu.Unlock()
		in.log.Warn("sheet commit", "slug", in.slug, "err", err)
		observe()
		for _, w := range batch {
			w.done <- unavailable("commit")
		}
		return next
	}
	for i, w := range batch {
		if errs[i] == nil {
			in.afterAction(w.action)
		}
		if a := w.action; a.stored && !a.memory {
			a.s.durable.Store(a.n)
		}
		w.action.committed()
	}
	in.fitSheet()
	in.wb.styles.sweep()
	in.tightenWire()
	in.wb.settle()
	in.mu.Unlock()
	in.hub.wake()
	observe()
	for i, w := range batch {
		w.done <- errs[i]
	}
	return next
}

// answerCommitted answers an action that was sent again after its receipt
// was committed, from that receipt, and reports whether it did. The action
// stores nothing, so it joins no commit: the failure of a transaction that
// other actions share cannot turn its known outcome into a refusal.
func (in *Instance) answerCommitted(w *work) bool {
	a := w.action
	if a.n > a.s.durable.Load() {
		return false
	}
	ack, err := in.committedAck(in.ctx, a.s, a.n, a.hash)
	a.ack = ack
	w.done <- err
	return true
}

// afterCommit tells the host about new outbox entries. The commit has
// bounded the log already (Instance.commit).
func (in *Instance) afterCommit(outbox bool) {
	if outbox {
		in.b.OutboxReady()
	}
}

type checkpoint struct {
	journal, steps, structs, over, fwrote, repairs, nwrote int
	held                                                   int64 // the journal's bytes
	count                                                  counters
	touches, pending, values                               int
	changed, tabList, order, dirtyAll, scan                bool
	wrote                                                  *Tab
	styleIDs                                               int // the length of the style table's log
}

// checkpoint marks the place an operation can be taken back to. The
// records the action wrote before it are journaled again when a later
// operation writes them (txn.epoch).
func (x *txn) checkpoint() checkpoint {
	x.epoch++
	return checkpoint{x.j.len(), len(x.steps), len(x.structs), len(x.over), len(x.fwrote), len(x.repairs), x.nwrote, x.j.bytes, x.wb.count,
		len(x.touches), len(x.pending), len(x.values), x.changed, x.tabList, x.order, x.dirtyAll, x.scan, x.wrote, x.wb.styles.mark()}
}

// revert takes the whole action back: the workbook, and the style states
// it interned, which no cell uses once the cells are back. A call that
// does not commit leaves no state behind.
func (x *txn) revert() {
	x.wb.revert(&x.j)
	x.wb.styles.forgetFrom(x.styleIDs)
}

// rollbackTo reverts what one operation did after the checkpoint: the
// workbook, and everything the action keeps for staging, for its delta and
// for its undo entry. Nothing of the operation reaches sheet.db.
func (x *txn) rollbackTo(m checkpoint) {
	for i := x.j.len() - 1; i >= m.journal; i-- {
		if e := x.j.at(i); e.fn == nil && e.fresh {
			delete(x.cells, e.ref)
		}
	}
	x.wb.revertTo(&x.j, m.journal, &m.count)
	x.wb.styles.forgetFrom(m.styleIDs)
	// What stays on the list of records to stage was journaled before the
	// checkpoint: the next write of it needs an entry again.
	x.j.bytes = m.held
	x.epoch++
	for i := len(x.touches) - 1; i >= m.touches; i-- {
		if s := &x.touches[i]; s.fresh {
			delete(x.tabs, s.tab)
		} else {
			*x.tabs[s.tab] = s.was
		}
	}
	x.touches = x.touches[:m.touches]
	// The undo entry of the action takes x.steps as it is: the steps of the
	// operation that is taken back must not stay behind its length.
	x.steps, x.structs = cut(x.steps, m.steps, len(x.steps)), cut(x.structs, m.structs, len(x.structs))
	x.over, x.fwrote, x.repairs = x.over[:m.over], x.fwrote[:m.fwrote], x.repairs[:m.repairs]
	// A recalculation inside the operation emptied pending; the revert made
	// the index stale, so the next recalculation covers every formula.
	x.pending, x.values = x.pending[:min(len(x.pending), m.pending)], x.values[:min(len(x.values), m.values)]
	x.nwrote, x.wrote, x.changed, x.tabList, x.order = m.nwrote, m.wrote, m.changed, m.tabList, m.order
	x.dirtyAll, x.scan = m.dirtyAll, m.scan
}

type undoAck struct {
	Applied int    `json:"applied"`
	Skipped int    `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
}

// addedIDs names the first row and the first column id an action added.
// The ids of one axis are given out in operation order and in a run, so
// the client finds the id of each {"new": k} without the action's delta.
type addedIDs struct {
	Rows uint64 `json:"rows,omitempty"`
	Cols uint64 `json:"cols,omitempty"`
}

// ackBody is the ack frame of Appendix A, and the stored outcome of an
// action.
type ackBody struct {
	Type    string     `json:"type"`
	N       string     `json:"n"`
	Seq     string     `json:"seq"`
	Results []opResult `json:"results"`
	Undo    *undoAck   `json:"undo,omitempty"`
	Added   *addedIDs  `json:"added,omitempty"`
}

func asWriteError(err error) *WriteError {
	var werr *WriteError
	if errors.As(err, &werr) {
		return werr
	}
	return &WriteError{Code: "unavailable", Reason: "storage", Retry: true, cause: err}
}

// runAction performs the steps of section 6 for one action inside the
// group's transaction. An error means that the action did not get a
// receipt; refusals of the sheet are part of the ack.
func (in *Instance) runAction(ctx context.Context, tx *sql.Tx, a *actionWork) error {
	wb, s := in.wb, a.s
	if a.n <= s.maxN {
		outcome, hash, err := in.receipt(ctx, tx, s, a.n)
		if err != nil {
			return err
		}
		if !bytes.Equal(hash, a.hash) {
			return errActionNumber()
		}
		a.ack = outcome
		return nil
	}
	if len(a.ops) == 0 || len(a.ops) > maxActionOps || a.base > wb.seq {
		return invalidf("action", "an action has 1 to %d operations and a base the server has sent", maxActionOps)
	}
	if err := actionShape(a.ops); err != nil {
		return err
	}
	if err := outboxAdmission(ctx, tx); err != nil {
		return err
	}
	var other int64
	if in.b.Limits().DiskQuota > 0 {
		var ok bool
		if other, ok = in.measure.get(); !ok {
			in.measure.refresh()
			return unavailable("measurement")
		}
	}
	// The receipt of a session with a row is stored in sheet.db, so room for
	// its refusal is reserved before anything applies (technical design,
	// section 6). Without it the action is refused and stored nowhere, and
	// its number stays free.
	if !s.rowless {
		if err := in.receiptRoom(ctx, tx, wb, s, other, len(a.ops)); err != nil {
			werr := asWriteError(err)
			if werr.Retry {
				return werr
			}
			return in.unstored(a, werr)
		}
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT act`); err != nil {
		return unavailable("storage")
	}
	x := newTxn(wb, wb.seq+1, a.base, s.ord)
	nextRow, nextCol := wb.nextRow, wb.nextCol
	var added *addedIDs
	in.begin(x)
	defer func() { a.calcTime, a.calcRan = x.calcTime, x.calcRan }()
	results := make([]opResult, len(a.ops))
	var undone *undoAck
	var popped *undoEntry
	redo := false
	// undo takes the action out of memory and out of the transaction. The
	// savepoint stays open for a refusal receipt.
	undo := func() error {
		x.revert()
		wb.settle()
		if a.rowed {
			s.rowless, a.rowed = true, false
		}
		_, err := tx.ExecContext(ctx, `ROLLBACK TO act`)
		return err
	}
	abandon := func(cause error) error {
		rollback := undo()
		_, release := tx.ExecContext(ctx, `RELEASE act`)
		return errors.Join(cause, rollback, release)
	}
	receipt := func(seq uint64) error {
		data, err := json.Marshal(ackBody{Type: "ack", N: decimal(a.n), Seq: decimal(seq), Results: results, Undo: undone, Added: added})
		if err != nil {
			return err
		}
		a.ack = data
		return in.storeReceipt(ctx, tx, a, seq)
	}
	// A limit, the quota and the recalculation time refuse the whole
	// action: nothing of it stays, in memory or in the transaction, and its
	// receipt says so for every operation.
	refuse := func(werr *WriteError) error {
		if err := undo(); err != nil {
			return abandon(unavailable("storage"))
		}
		for i := range results {
			results[i] = opResult{Status: "refused", Reason: werr.reason()}
		}
		undone, popped, added = nil, nil, nil
		if err := receipt(wb.seq); err != nil {
			a.forget()
			return abandon(asWriteError(err))
		}
		if !s.rowless {
			if err := in.receiptFits(ctx, tx, wb, other); err != nil {
				a.forget()
				if werr := asWriteError(err); werr.Retry {
					return abandon(werr)
				}
				// The refusal does not fit beside the content: it is
				// answered and stored nowhere, and its number stays free.
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO act`); err != nil {
					return abandon(unavailable("storage"))
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `RELEASE act`); err != nil {
			a.forget()
			return abandon(unavailable("storage"))
		}
		in.rejected.Add(1)
		return nil
	}
	for i := range a.ops {
		op := &a.ops[i]
		mark := x.checkpoint()
		var applied, ignored int
		var err error
		if op.Op == "undo" || op.Op == "redo" {
			if len(a.ops) != 1 {
				err = invalidf("undo", "undo and redo are actions of their own")
			} else {
				redo = op.Op == "redo"
				stack := s.undo.undo
				if redo {
					stack = s.undo.redo
				}
				if len(stack) == 0 {
					// Nothing applied and nothing skipped tells the client
					// that no earlier undo is available.
					undone = &undoAck{}
				} else {
					popped = stack[len(stack)-1]
					var werr *WriteError
					var reason string
					applied, ignored, reason, werr = x.applyUndo(&s.undo, popped)
					undone = &undoAck{Applied: applied, Skipped: ignored, Reason: reason}
					ignored = 0
					if werr != nil {
						err, popped, undone = werr, nil, nil
					}
				}
			}
		} else {
			applied, ignored, err = x.applyPerson(op)
		}
		switch {
		case err == nil:
			results[i] = opResult{Status: "applied", Applied: applied, Ignored: ignored}
		case errors.Is(err, errGone):
			x.rollbackTo(mark)
			results[i] = opResult{Status: "ignored", Reason: "gone"}
		default:
			werr := asWriteError(err)
			if werr.Retry {
				in.scratchRefused(werr)
				return abandon(werr)
			}
			if werr.Code == "sheet_capacity" {
				return refuse(werr)
			}
			x.rollbackTo(mark)
			results[i] = opResult{Status: "refused", Reason: werr.reason()}
		}
	}
	if x.changed {
		if s.rowless {
			if err := in.storeSession(ctx, tx, a); err != nil {
				return abandon(unavailable("storage"))
			}
		}
		if werr := in.complete(ctx, tx, x); werr != nil {
			if werr.Retry {
				return abandon(werr)
			}
			return refuse(werr)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups(editor,label,verified,started,last,dirty) VALUES(?,?,?,?,?,1)
			ON CONFLICT(editor) DO UPDATE SET label=excluded.label,last=excluded.last,dirty=1`, s.editor, s.author(), s.verified, x.now.Unix(), x.now.Unix()); err != nil {
			return abandon(unavailable("storage"))
		}
	}
	if wb.nextRow != nextRow || wb.nextCol != nextCol {
		added = &addedIDs{}
		if wb.nextRow != nextRow {
			added.Rows = nextRow
		}
		if wb.nextCol != nextCol {
			added.Cols = nextCol
		}
	}
	// The receipt is part of the action: if it cannot be stored, nothing of
	// the action stays.
	if err := receipt(wb.seq); err != nil {
		a.forget()
		return abandon(asWriteError(err))
	}
	// The quota check comes behind the receipt, the edit group and the row
	// of the session, so that it counts every byte the action stores. An
	// action that changed nothing stores only its receipt, which memory
	// keeps for a session without a row.
	if x.changed || !s.rowless {
		check := in.receiptFits
		if x.changed {
			check = in.checkQuota
		}
		if err := check(ctx, tx, wb, other); err != nil {
			a.forget()
			werr := asWriteError(err)
			if werr.Retry {
				return abandon(werr)
			}
			return refuse(werr)
		}
	}
	if x.changed {
		if werr := in.admitUndo(s, x.entry); werr != nil {
			in.scratchRefused(werr)
			a.forget()
			return abandon(werr)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE act`); err != nil {
		a.forget()
		return abandon(unavailable("storage"))
	}
	var entry *undoEntry
	if x.changed {
		// The delta is encoded now, from the state of this sequence. Later
		// actions of the transaction change the workbook before it commits.
		a.x, a.rec = x, buildDelta(x, nil)
		a.rec.from = `{"session_id":` + string(appendString(nil, s.id)) + `,"n":"` + decimal(a.n) + `"}`
		entry = x.entry
	}
	// What the action takes off the session's stacks when it commits no
	// longer holds a stamp for the actions behind it in the transaction.
	switch {
	case entry == nil:
	case popped != nil && !redo:
	case popped != nil:
		s.undo.pushing++
	default:
		s.undo.pushing++
		s.undo.clearing = true
	}
	// The entry an undo or a redo consumed leaves its stack before the
	// structural edits of this action are applied to the stacks: its saved
	// cells are live in the sheet again. The new entry joins after that,
	// because it was built on the state those edits left.
	a.pre = func() {
		if popped == nil {
			return
		}
		u := &s.undo
		if redo {
			u.redo = cut(u.redo, len(u.redo)-1, len(u.redo))
		} else {
			u.undo = cut(u.undo, len(u.undo)-1, len(u.undo))
		}
		popped.release()
	}
	a.post = func() {
		u := &s.undo
		switch {
		case entry == nil:
		case popped != nil && !redo:
			entry.markCreated()
			u.redo = append(u.redo, entry)
		case popped != nil:
			u.push(entry)
		default:
			u.push(entry)
			u.clearRedo()
		}
	}
	return nil
}

// scratchLogEvery is the least time between two log lines about the undo
// scratch storage of one sheet. A browser sends an action that was refused
// for it again, and every attempt would fail the same way.
const scratchLogEvery = 10 * time.Second

// scratchLog limits the log lines about undo scratch storage.
type scratchLog struct {
	last    time.Time
	skipped int
}

// scratchFailed logs an error of the undo scratch storage with the sheet it
// belongs to: the operator has to free or repair host storage, which the
// flat's quota does not show. op says what was read or written.
func (in *Instance) scratchFailed(op string, err error) {
	l := &in.scratch.said
	if now := time.Now(); now.Sub(l.last) >= scratchLogEvery {
		in.log.Warn("sheet undo scratch storage failed", "slug", in.slug, "op", op, "err", err, "suppressed", l.skipped)
		l.last, l.skipped = now, 0
		return
	}
	l.skipped++
}

// scratchRefused logs the storage error behind an action that is refused
// as unavailable for the undo scratch storage.
func (in *Instance) scratchRefused(werr *WriteError) {
	if werr != nil && werr.Reason == "undo_scratch" && werr.cause != nil {
		in.scratchFailed("undo payload", werr.cause)
	}
}

// finish completes an action or a call after its operations: it
// recalculates, checks the limits, takes the sequence, stages the journal
// and checks the quota (steps 3 to 5 of section 6).
func (in *Instance) finish(ctx context.Context, tx *sql.Tx, x *txn, other int64) *WriteError {
	if werr := in.complete(ctx, tx, x); werr != nil {
		return werr
	}
	if err := in.checkQuota(ctx, tx, x.wb, other); err != nil {
		return asWriteError(err)
	}
	return nil
}

// complete is finish without the quota check, for a person's action, whose
// receipt is staged before that check.
func (in *Instance) complete(ctx context.Context, tx *sql.Tx, x *txn) *WriteError {
	wb := x.wb
	if x.blankErr != nil {
		// A new record could not read the stamps its cell had (recall).
		return scratchRefusal(x.blankErr)
	}
	if wb.styles.full {
		// A style state found no free id (styleTable.take).
		return unavailable("ids_exhausted")
	}
	err := x.calc()
	if err == nil {
		var written []*fnode
		for _, ref := range x.fwrote {
			if c := ref.tab.cell(ref.row, ref.col); c != nil && c.node != nil {
				written = append(written, c.node)
			}
		}
		err = wb.checkLimits(written, x.scan, x.budget)
	}
	in.noteCalc(x.budget, x.passes)
	if err != nil {
		return err
	}
	// The work that the deadline is on ends here.
	in.step("calculated")
	wb.seq = x.seq
	x.j.push(func() { wb.seq = x.seq - 1 })
	if x.by != 0 {
		x.entry = x.undoEntry()
	}
	x.replaced = in.replacedNotes(x)
	x.markFiles()
	// The journal has the last entries it gets here. The bound is looked
	// at behind them.
	if err := x.fits(); err != nil {
		return err
	}
	if err := stage(ctx, tx, x); err != nil {
		return asWriteError(err)
	}
	return nil
}

// afterAction applies the memory effects of a committed action: the
// structural log, the undo stacks and the delta. The batch brings undo
// memory inside the sheet's bound after the last of its actions
// (fitSheet).
func (in *Instance) afterAction(a *actionWork) {
	if a.pre != nil {
		a.pre()
	}
	if a.x != nil {
		in.settleStamps(a.x)
		in.followStructure(a.x)
	}
	if a.post != nil {
		a.post()
	}
	if a.x == nil {
		return
	}
	in.applied.Add(1)
	in.send(a.x, a.rec)
}

// followStructure adds the structural edits of a committed action to the
// log and applies them to the formulas the undo stacks hold.
func (in *Instance) followStructure(x *txn) {
	logs := x.logEntries()
	if len(logs) == 0 {
		return
	}
	for _, e := range logs {
		in.structs.add(e)
	}
	in.structs.trim(x.now)
	in.smu.Lock()
	defer in.smu.Unlock()
	in.followUndo(x.wb, logs)
}

// noteTab returns the tab the note of an agent's call names: the one tab
// the call wrote cells in, or the one tab a call that wrote no cell
// changed. It is 0 when there are several, so that the notice names none.
func (x *txn) noteTab() TabID {
	if x.nwrote > 0 {
		if x.wrote == nil {
			return 0
		}
		return x.wrote.id
	}
	var one *Tab
	several := func(t *Tab) bool {
		if one == nil {
			one = t
		}
		return one != t
	}
	for ref := range x.cells {
		if several(ref.tab) {
			return 0
		}
	}
	for t := range x.tabs {
		if several(t) {
			return 0
		}
	}
	if one == nil {
		return 0
	}
	return one.id
}

// publish builds the delta of a committed change that had a transaction
// of its own and hands it to the connections. The caller holds the write
// lock and wakes the connections after it.
func (in *Instance) publish(x *txn, note *agentNote) {
	rec := buildDelta(x, note)
	x.wb.styles.sweep()
	in.send(x, rec)
	in.tightenWire()
}

// send hands the delta of a committed change, with its overwrite notices,
// to the connections.
func (in *Instance) send(x *txn, rec *deltaRec) {
	wb := x.wb
	in.hub.append(rec, in.now())
	for _, o := range x.over {
		if t := o.ref.tab; wb.byID[t.id] == t {
			in.hub.overwritten(o.session, t.id, o.ref.row, o.ref.col, o.replaced)
		}
	}
}

// closeGroups closes the edit groups that are idle for 30 seconds or open
// for 5 minutes, or all of them (spec rule 47). When a group that closes
// holds edits no revision has yet, one revision is recorded first. It
// credits every editor with such edits, because a revision holds the whole
// sheet. A group that is not due stays open and keeps its start. undo
// takes the memory part out again.
func (in *Instance) closeGroups(ctx context.Context, tx *sql.Tx, all bool) (recorded bool, undo func(), err error) {
	undo = func() {}
	now := in.now().Unix()
	// The order is that of the checked values, so the sort holds no value
	// longer than the writer stores; the load has checked the table too.
	rows, err := tx.QueryContext(ctx, `SELECT `+storedValues("groups", "", "editor", "label", "started", "last", "dirty")+`,`+
		storedFault("groups", "")+` FROM groups ORDER BY `+storedValues("groups", "", "started", "editor"))
	if err != nil {
		return false, undo, err
	}
	var authors []string
	due, dirtyDue := false, false
	for rows.Next() {
		var editor, label sql.NullString
		var started, last, fault int64
		var dirty bool
		if err := rows.Scan(&editor, &label, &started, &last, &dirty, &fault); err != nil {
			rows.Close()
			return false, undo, err
		}
		if err := faultError("groups", fault); err != nil {
			rows.Close()
			return false, undo, err
		}
		// A revision names the first maxRevisionAuthors editors, by when
		// their group started, so that its outbox row has a bound.
		if dirty && len(authors) < maxRevisionAuthors && !slices.Contains(authors, label.String) {
			authors = append(authors, label.String)
		}
		if all || now-last >= int64(groupIdle/time.Second) || now-started >= int64(groupMaxAge/time.Second) {
			due = true
			dirtyDue = dirtyDue || dirty
		}
	}
	if err := closeRows(rows); err != nil {
		return false, undo, err
	}
	if !due {
		return false, undo, nil
	}
	var seq int64
	if dirtyDue {
		if seq, undo, err = in.recordBoundary(ctx, tx, in.wb, boundary{kind: "person", authors: authors}); err != nil {
			return false, undo, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE groups SET dirty=0 WHERE dirty<>0`)
	}
	// The due groups go by the test that found them, so that no list of
	// them is held.
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM groups WHERE ?1 OR ?2-last>=?3 OR ?2-started>=?4`,
			all, now, int64(groupIdle/time.Second), int64(groupMaxAge/time.Second))
	}
	if err != nil {
		undo()
		return false, func() {}, err
	}
	return seq != 0, undo, nil
}

func (in *Instance) finishGroups() {
	ctx := context.Background()
	in.mu.Lock()
	defer in.mu.Unlock()
	var undo func()
	err := in.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		_, undo, err = in.closeGroups(ctx, tx, true)
		return err
	})
	if err != nil {
		if undo != nil {
			undo()
		}
		in.log.Warn("sheet final boundary", "err", err)
		return
	}
	in.b.OutboxReady()
}

// maintain is the maintenance tick: it closes due edit groups, expires
// sessions, stored results and blank records, trims the logs, and starts
// the change of the calculation date.
func (in *Instance) maintain() {
	ctx := in.ctx
	now := in.now()
	in.mu.Lock()
	wb := in.wb
	recorded := false
	undo := func() {}
	var blanks []cellRef
	var floor uint64
	err := in.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		if recorded, undo, err = in.closeGroups(ctx, tx, false); err != nil {
			return err
		}
		for _, query := range []string{
			`DELETE FROM receipts WHERE session IN (SELECT id FROM sessions WHERE last_at<=?1)`,
			`DELETE FROM sessions WHERE last_at<=?1`,
		} {
			if _, err := tx.ExecContext(ctx, query, now.Add(-sessionTTL).Unix()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE imported=1 AND expires<=?`, now.Unix()); err != nil {
			return err
		}
		if wb.count.blanks > wb.blankLimit() {
			blanks, floor = wb.oldBlanks()
			if blanks, err = in.keepBlanks(ctx, blanks); err != nil {
				return err
			}
			del, err := tx.PrepareContext(ctx, `DELETE FROM cells WHERE tab=? AND row=? AND col=?`)
			if err != nil {
				return err
			}
			defer del.Close()
			for _, ref := range blanks {
				if _, err := del.ExecContext(ctx, ref.tab.id, ref.row, ref.col); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `UPDATE meta SET guard_floor=? WHERE singleton=1`, max(floor, wb.guardFloor))
			return err
		}
		return nil
	})
	if err != nil {
		undo()
		// Sessions leave memory on time also when sheet.db cannot be
		// written now.
		in.dropIdleUndo(now)
		in.mu.Unlock()
		if ctx.Err() == nil {
			in.log.Warn("sheet maintenance", "err", err)
		}
		return
	}
	for _, ref := range blanks {
		if c := ref.tab.cell(ref.row, ref.col); c != nil {
			wb.weighCell(c, -1)
			ref.tab.drop(ref.row, ref.col)
		}
	}
	wb.guardFloor = max(wb.guardFloor, floor)
	in.structs.trim(now)
	in.dropIdleUndo(now)
	in.sweepUndo()
	in.sweepNotes()
	in.fitSheet()
	in.compactTabs(tabRoomWork)
	wb.settle()
	date := wb.date
	in.mu.Unlock()
	in.captures.expire(now)
	in.revs.expire(now)
	in.noteIdle(now)
	if recorded {
		in.afterCommit(true)
	}
	if in.hub.count() > 0 && in.today() != date {
		// A change that fails is tried again at the next tick; the open
		// screens keep the date they show.
		_ = in.changeDate()
	}
}

// oldBlanks returns the older half of the blank records without a format
// and the newest stamp among them (section 7.5).
func (wb *Workbook) oldBlanks() ([]cellRef, uint64) {
	type blank struct {
		ref   cellRef
		stamp uint64
	}
	var all []blank
	for _, t := range wb.tabs {
		for row, rd := range t.data {
			for _, s := range rd.cells {
				if !wb.counted(s.c) {
					all = append(all, blank{cellRef{t, row, s.col}, max(s.c.seq, s.c.textSeq)})
				}
			}
		}
	}
	slices.SortFunc(all, func(a, b blank) int {
		if a.stamp < b.stamp {
			return -1
		}
		if a.stamp > b.stamp {
			return 1
		}
		return 0
	})
	all = all[:len(all)/2]
	refs := make([]cellRef, len(all))
	var floor uint64
	for i, b := range all {
		refs[i], floor = b.ref, b.stamp
	}
	return refs, floor
}

// syncDate brings the calculation date up to the host's date before a
// request is answered (section 10.6). The change belongs to the writer,
// not to the request. The request waits for it for agentPatience and is
// then refused as calculating, and the change goes on for the next
// request. A change that ends without committing the new date leaves the
// sheet at the previous one, and the request is refused, so no answer
// stands for a date the sheet does not have.
func (in *Instance) syncDate(ctx context.Context) error {
	in.mu.RLock()
	stale := in.wb.date != in.today()
	in.mu.RUnlock()
	if !stale {
		return nil
	}
	run, err := in.requestDate()
	if err != nil {
		return err
	}
	timer := time.NewTimer(agentPatience)
	defer timer.Stop()
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	case <-in.ctx.Done():
		return unavailable("stopped")
	case <-timer.C:
		return unavailable("calculating")
	}
}

// dateRun is a change of the date that requests wait for. err is set
// before done is closed: nil when the sheet is at the date the change
// read, else the refusal for those who wait.
type dateRun struct {
	done chan struct{}
	err  error
}

// requestDate queues a change of the date, unless one is queued already,
// and returns it.
func (in *Instance) requestDate() (*dateRun, error) {
	in.dateMu.Lock()
	defer in.dateMu.Unlock()
	if in.dateRun != nil {
		return in.dateRun, nil
	}
	run := &dateRun{done: make(chan struct{})}
	err := in.enqueue(in.ctx, &work{run: func() error {
		run.err = in.changeDate()
		in.dateMu.Lock()
		in.dateRun = nil
		in.dateMu.Unlock()
		close(run.done)
		return nil
	}})
	if err != nil {
		return nil, err
	}
	in.dateRun = run
	return run, nil
}

// changeDate moves the sheet to the host's date. The formulas that read
// TODAY, and their dependents, are evaluated into the overlay while
// readers keep the previous state, whole, until the swap. The change takes
// the next sequence, changes no content and creates no revision (spec rule
// 13).
//
// The pass runs on the writer's goroutine and holds no lock. Nothing else
// changes what it reads: only the writer changes cells, the index and the
// structure, and the writer is here. So the pass can wait in line at the
// gate, for as long as the passes of other sheets take, and can run for
// minutes, without holding the read lock: a goroutine that asks for the
// write lock in that time (the rebuild of the format list for a snapshot)
// gets it, and no reader of this sheet waits behind it. A stop of the
// instance ends the pass at its next check point; the sheet then keeps its
// previous state, date included.
//
// The writer takes no other work while the pass runs. What waits for it
// is told so (beginPass), and the browsers are told once the pass has
// lasted for agentPatience. The date change of a sheet whose formulas do
// not read the date ends long before, and tells nobody.
//
// It returns nil when the sheet is at the date it read, and else the
// refusal of the requests that wait for it: the sheet then keeps its
// previous state.
func (in *Instance) changeDate() error {
	date := in.today()
	in.mu.RLock()
	wb := in.wb
	stale := wb.date != date
	in.mu.RUnlock()
	if !stale {
		return nil
	}
	defer in.beginPass(agentPatience)()
	turn, ok := calcSlots.enter(in, in.ctx)
	defer turn.leave()
	if !ok {
		return unavailable("stopped")
	}
	budget := turn.budget()
	out, cerr := turn.recalc(wb, calcReq{today: true, date: &date, budget: budget})
	in.noteCalc(budget, out.passes)
	in.observeCalc(false, out.took)
	if cerr != nil {
		// Without a finished recalculation the sheet stays whole in its
		// previous state, date included (spec rule 9).
		if in.ctx.Err() != nil {
			return unavailable("stopped")
		}
		in.log.Warn("sheet calculation date", "err", cerr)
		return unavailable("calculating")
	}
	in.mu.Lock()
	var j journal
	old := wb.date
	x := newTxn(wb, wb.seq+1, wb.seq, 0)
	x.values = wb.apply(out, &j, x.seq)
	wb.seq, wb.date = x.seq, date
	err := in.transaction(in.ctx, func(tx *sql.Tx) error { return stageMeta(in.ctx, tx, wb) })
	if err != nil {
		wb.revert(&j)
		wb.seq, wb.date = x.seq-1, old
		wb.deps.stale = false
		in.mu.Unlock()
		if in.ctx.Err() != nil {
			return unavailable("stopped")
		}
		in.log.Warn("sheet calculation date", "err", err)
		return unavailable("commit")
	}
	rec := buildDelta(x, nil)
	rec.date = date.String()
	in.hub.append(rec, in.now())
	in.mu.Unlock()
	in.hub.wake()
	return nil
}

// Update applies one agent write call: whole or not at all (spec rule 36).
// other is a fresh measurement of the rest of the flat's directory, taken
// under the flat lock.
func (in *Instance) Update(ctx context.Context, u Update, other int64) (WriteResult, error) {
	if u.hash == "" || other < 0 {
		return WriteResult{}, invalidf("update", "an update comes from DecodeUpdate, with a measurement")
	}
	// A call that was stored under its op_id is answered from sheet.db
	// alone, whatever the sheet is doing: its outcome is known. The stored
	// calls are checked first, since the load may not have yet.
	if u.OpID != "" {
		if err := in.checkTables(ctx, "commands"); err != nil {
			in.rejected.Add(1)
			if ctx.Err() != nil {
				return WriteResult{}, ctx.Err()
			}
			if problem := (*WriteError)(nil); errors.As(err, &problem) {
				return WriteResult{}, problem
			}
			return WriteResult{}, unavailable("storage")
		}
	}
	if res, found, err := storedCall(ctx, in.rdb, &u); found {
		if err != nil {
			in.rejected.Add(1)
		}
		return res, err
	}
	if err := in.await(ctx, agentPatience); err != nil {
		return WriteResult{}, err
	}
	if err := in.syncDate(ctx); err != nil {
		return WriteResult{}, err
	}
	var res WriteResult
	err := in.execute(ctx, &work{soon: true, run: func() error {
		var err error
		res, err = in.update(ctx, &u, other)
		return err
	}})
	if err != nil {
		in.rejected.Add(1)
		return WriteResult{}, err
	}
	return res, nil
}

const agentPatience = 250 * time.Millisecond

type commandOutcome struct {
	Result WriteResult `json:"result"`
	Outbox int64       `json:"outbox"`
}

// maxCommandOutcomeBytes holds the stored outcome of a call: a result of
// the most formula errors it lists (txn.result), each at a cell of a tab
// whose quoted name takes six bytes for each of its bytes where JSON
// escapes them, and room for the other fields.
const maxCommandOutcomeBytes = 1<<10 + maxResultErrors*(len(`{"at":"!$ZZ$100000:$ZZ$100000","error":"#VALUE!"},`)+6*(2*4*maxTabName+2))

// errStoredCall ends the transaction of a call that turned out to be
// stored already: it has nothing to commit.
var errStoredCall = errors.New("sheet: the call is stored")

// storedCall looks for the stored outcome of a call by its op_id. found is
// false when there is none, or when it cannot be read now. A stored call
// with another request is a conflict.
func storedCall(ctx context.Context, q querier, u *Update) (res WriteResult, found bool, err error) {
	if u.OpID == "" {
		return res, false, nil
	}
	var hash sql.NullString
	var outcome []byte
	var hashLen, outcomeLen sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT `+boundedField("hash", maxHashBytes)+`,`+boundedField("outcome", maxCommandOutcomeBytes)+
		` FROM commands WHERE op_id=?`, u.OpID).Scan(&hash, &hashLen, &outcome, &outcomeLen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return res, false, err
	}
	if errors.Join(fieldFits("command hash", hashLen, maxHashBytes), fieldFits("command outcome", outcomeLen, maxCommandOutcomeBytes)) != nil {
		return res, true, unavailable("storage")
	}
	if hash.String != u.hash {
		return res, true, conflict("op_id_reuse", "")
	}
	var stored commandOutcome
	if err := json.Unmarshal(outcome, &stored); err != nil {
		return res, true, unavailable("storage")
	}
	res = stored.Result
	res.Replay, res.OutboxSeq = true, stored.Outbox
	return res, true, nil
}

func (in *Instance) update(ctx context.Context, u *Update, other int64) (WriteResult, error) {
	in.mu.Lock()
	wb := in.wb
	var res WriteResult
	var x *txn
	undo := func() {}
	revert := func() {
		if x != nil {
			x.revert()
		}
		undo()
		wb.settle()
	}
	replay := false
	err := in.transaction(ctx, func(tx *sql.Tx) error {
		// The call may have committed after Update looked for it.
		stored, found, err := storedCall(ctx, tx, u)
		switch {
		case found && err == nil:
			res, replay = stored, true
			return errStoredCall
		case err != nil:
			return err
		}
		if err := outboxAdmission(ctx, tx); err != nil {
			return err
		}
		// Open groups close as one revision before the call applies.
		if _, undo, err = in.closeGroups(ctx, tx, true); err != nil {
			return err
		}
		x = newTxn(wb, wb.seq+1, wb.seq, 0)
		in.begin(x)
		if u.Create && u.State == "" {
			u.State = stateToken(wb.store, wb.seq)
		}
		if werr := x.applyAgent(u); werr != nil {
			return werr
		}
		if werr := in.finish(ctx, tx, x, other); werr != nil {
			return werr
		}
		seq, undoFiles, err := in.recordBoundary(ctx, tx, wb, boundary{kind: "agent", message: u.Message, opID: u.OpID, force: true})
		if err != nil {
			return err
		}
		x.j.push(undoFiles)
		// The boundary added the last entry of the call's journal.
		if werr := x.fits(); werr != nil {
			return werr
		}
		res = x.result()
		res.OutboxSeq = seq
		if u.OpID == "" {
			return nil
		}
		outcome, err := json.Marshal(commandOutcome{Result: res, Outbox: seq})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO commands(op_id,hash,outcome,seq,outbox_seq,imported,expires) VALUES(?,?,?,?,?,0,0)`,
			u.OpID, u.hash, outcome, x.seq, seq); err != nil {
			return err
		}
		// The stored outcome counts against the quota like the call.
		return in.checkQuota(ctx, tx, wb, other)
	})
	if replay && errors.Is(err, errStoredCall) {
		err = nil
	}
	if err != nil || replay {
		if err != nil {
			revert()
			in.fitSheet()
		}
		in.mu.Unlock()
		if x != nil && x.calcRan {
			in.observeCalc(true, x.calcTime)
		}
		if err != nil {
			return WriteResult{}, asWriteError(err)
		}
		return res, nil
	}
	in.settleStamps(x)
	in.followStructure(x)
	in.fitSheet()
	in.applied.Add(1)
	in.publish(x, &agentNote{tab: x.noteTab(), cells: x.nwrote})
	wb.settle()
	in.mu.Unlock()
	in.hub.wake()
	if x.calcRan {
		in.observeCalc(true, x.calcTime)
	}
	in.afterCommit(true)
	return res, nil
}

// noteIdle records since when the instance is idle: no connection, no open
// edit group, an empty outbox and no live capture. Core reads it from Stats
// to stop an instance that nobody uses, which bounds host memory to the
// sheets in use (section 3.2).
func (in *Instance) noteIdle(now time.Time) {
	var busy int
	err := in.rdb.QueryRowContext(in.ctx, unsettledQuery).Scan(&busy)
	in.captures.mu.Lock()
	captures := len(in.captures.list)
	in.captures.mu.Unlock()
	in.idleMu.Lock()
	defer in.idleMu.Unlock()
	if err != nil || busy > 0 || captures > 0 || in.hub.count() > 0 {
		in.idleSince = time.Time{}
	} else if in.idleSince.IsZero() {
		in.idleSince = now
	}
}
