package sheet

import (
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

const (
	structLogEntries = 256
	structLogBytes   = 4 << 20
	structLogAge     = time.Hour
)

// logEntry is one structural edit in the structural log: a transform of
// formula references, or a sort with the rows it moved.
type logEntry struct {
	seq   uint64
	at    time.Time
	tab   TabID
	tr    formula.Transform // Kind is zero for a sort
	rows  bool
	gone  []uint64           // ids an edit deleted, from position tr.At on
	came  []uint64           // ids an insert brought, from position tr.At on
	moved map[RowID][2]int32 // sort: position before and after, per moved row
	// above and below are the ids that lay around the deleted ones, 0 at
	// an end of the tab.
	above, below uint64
}

func (e *logEntry) size() int { return 112 + 8*(len(e.gone)+len(e.came)) + 24*len(e.moved) }

// structLog keeps the structural edits of the recent past, in memory only
// (technical design, section 8.4). floor is the sequence up to which
// entries are missing: the log reaches back to a base at or above it.
type structLog struct {
	entries []logEntry
	floor   uint64
	bytes   int
}

func (l *structLog) add(e logEntry) {
	l.entries = append(l.entries, e)
	l.bytes += e.size()
}

func (l *structLog) trim(now time.Time) {
	drop := 0
	for drop < len(l.entries) && (len(l.entries)-drop > structLogEntries || l.bytes > structLogBytes ||
		now.Sub(l.entries[drop].at) > structLogAge) {
		l.bytes -= l.entries[drop].size()
		l.floor = l.entries[drop].seq
		drop++
	}
	// Entries of one action share a sequence and leave together.
	for drop < len(l.entries) && l.entries[drop].seq == l.floor {
		l.bytes -= l.entries[drop].size()
		drop++
	}
	l.entries = cut(l.entries, 0, drop)
}

func (l *structLog) clear(seq uint64) {
	l.entries, l.bytes, l.floor = nil, 0, seq
}

// after returns the entries with a sequence above base.
func (l *structLog) after(base uint64) []logEntry {
	i := len(l.entries)
	for i > 0 && l.entries[i-1].seq > base {
		i--
	}
	return l.entries[i:]
}

// logEntries turns the structural edits of an action into log entries, in
// the order the action made them: a rename between two inserts stays
// between them.
func (x *txn) logEntries() []logEntry {
	var out []logEntry
	for _, e := range x.structs {
		if e.appended || (!e.axisEdit() && e.kind != editRename) {
			continue
		}
		entry := logEntry{seq: x.seq, at: x.now, tab: e.tab.id, tr: e.tr, rows: e.rows}
		switch e.kind {
		case editInsert:
			entry.came = e.ids
		case editDelete:
			entry.gone, entry.above, entry.below = e.ids, e.above, e.below
		case editSort:
			entry.moved = make(map[RowID][2]int32)
			before := make(map[uint64]int32, len(e.before))
			for i, id := range e.before {
				before[id] = e.positions[i]
			}
			for i, id := range e.ids {
				if before[id] != e.positions[i] {
					entry.moved[RowID(id)] = [2]int32{before[id], e.positions[i]}
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

// screenPos returns where the cell that lies at cur now lay on the screen
// of the action's base: its position before the structural edits the log
// holds since then. When the log does not reach back it returns cur, and
// adjust decides by the durable stamps whether the screen can have moved.
func (x *txn) screenPos(t *Tab, row RowID, cur formula.Pos) formula.Pos {
	if x.base < x.log.floor {
		return cur
	}
	entries := x.log.after(x.base)
	pos := cur
	for i := len(entries) - 1; i >= 0; i-- {
		e := &entries[i]
		if e.tab != t.id {
			continue
		}
		at, n := e.tr.At, e.tr.Count
		switch e.tr.Kind {
		case formula.InsertRows:
			if pos.Row >= at+n {
				pos.Row -= n
			}
		case formula.DeleteRows:
			if pos.Row >= at {
				pos.Row += n
			}
		case formula.InsertCols:
			if pos.Col >= at+n {
				pos.Col -= n
			}
		case formula.DeleteCols:
			if pos.Col >= at {
				pos.Col += n
			}
		case 0:
			if mv, ok := e.moved[row]; ok {
				pos.Row = mv[0]
			}
		}
	}
	return pos
}

func staleFormula() *WriteError { return &WriteError{Code: "conflict", Reason: "stale_formula"} }

// adjust reads formula text a person typed against the screen of base and
// brings it to the sequence through, as if the formula had been in its cell
// since that screen (spec rule 14). at is the address the screen showed.
// through is the current sequence for a commit.
func (x *txn) adjust(t *Tab, row RowID, text string, at formula.Pos, through uint64) (*formula.Formula, formula.Pos, *WriteError) {
	wb := x.wb
	if x.base < wb.resetSeq {
		return nil, at, staleFormula()
	}
	if !at.InGrid() {
		return nil, at, invalidf("at", "the address is outside the grid")
	}
	if x.base < x.log.floor {
		f, lerr := x.parse(text, at)
		if lerr != nil {
			return nil, at, lerr
		}
		// The log no longer reaches back. Only the durable stamps can rule
		// a change out (section 8.4).
		stale := t.structSeq > x.base || t.nameSeq > x.base
		f.Refs(at, func(r formula.Ref) {
			if r.Tab == "" {
				return
			}
			if rt := wb.tab(r.Tab); rt != nil && (rt.structSeq > x.base || rt.nameSeq > x.base) {
				stale = true
			}
		})
		if stale || (f.NamesTab() && wb.tabChangeSeq > x.base) {
			return nil, at, staleFormula()
		}
		return f, at, nil
	}
	entries := x.log.after(x.base)
	for len(entries) > 0 && entries[len(entries)-1].seq > through {
		entries = entries[:len(entries)-1]
	}
	// The name the formula's tab had on the screen of base.
	name := t.name
	for i := len(x.log.entries) - 1; i >= 0; i-- {
		if e := x.log.entries[i]; e.seq > x.base && e.tr.Kind == formula.RenameTab && e.tab == t.id {
			name = e.tr.Tab
		}
	}
	f, lerr := x.parse(text, at)
	if lerr != nil {
		return nil, at, lerr
	}
	pos := at
	for i := range entries {
		e := &entries[i]
		// Each edit the formula is brought through is counted as a
		// structural edit counts a formula of the sheet (txn.rewrote).
		if err := x.stop(); err != nil {
			return nil, at, err
		}
		was := f
		switch {
		case e.tr.Kind == formula.RenameTab:
			f, _, _ = f.Apply(name, pos, e.tr)
			if e.tab == t.id {
				name = e.tr.Name
			}
		case e.tr.Kind != 0:
			if _, alive := e.tr.Map(name, pos); !alive {
				return nil, at, staleFormula()
			}
			f, pos, _ = f.Apply(name, pos, e.tr)
		case e.tab == t.id:
			if mv, ok := e.moved[row]; ok {
				if mv[0] != pos.Row {
					return nil, at, staleFormula()
				}
				to := formula.Pos{Row: mv[1], Col: pos.Col}
				f, pos = f.Moved(pos, to), to
			}
		}
		x.rewrote(was, f)
	}
	if n := f.TextLen(pos); n > formula.MaxFormulaChars {
		return nil, at, capacity("formula_length", formula.MaxFormulaChars, int64(n))
	}
	return f, pos, nil
}

func (x *txn) parse(text string, at formula.Pos) (*formula.Formula, *WriteError) {
	f, err := formula.Parse(text, at, formula.Limits{})
	if err != nil {
		if lim, ok := err.(*formula.LimitError); ok {
			return nil, limitError(lim)
		}
		return nil, invalidf("formula", "%v", err)
	}
	return f, nil
}

// placeBeside finds where a row or column that is gone was, for an insert
// beside it (spec rule 33). ok is false when the log does not tell.
func (l *structLog) placeBeside(t *Tab, rows bool, id uint64, base uint64) (int, bool) {
	if base < l.floor {
		return 0, false
	}
	entries := l.after(base)
	for i := range entries {
		e := &entries[i]
		if e.tab != t.id || e.rows != rows || e.gone == nil {
			continue
		}
		for _, gone := range e.gone {
			if gone != id {
				continue
			}
			pos := int(e.tr.At)
			for _, later := range entries[i+1:] {
				if later.tab != t.id || later.rows != rows {
					continue
				}
				at, n := int(later.tr.At), int(later.tr.Count)
				switch later.tr.Kind {
				case formula.InsertRows, formula.InsertCols:
					if at <= pos {
						pos += n
					}
				case formula.DeleteRows, formula.DeleteCols:
					if at+n <= pos {
						pos -= n
					} else if at < pos {
						pos = at
					}
				}
			}
			return pos, true
		}
	}
	return 0, false
}
