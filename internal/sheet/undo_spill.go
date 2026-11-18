package sheet

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// Undo history is kept inside fixed limits of memory, disk space and work
// (spec rule 23; technical design, section 14). The memory of the payloads
// is bounded: a payload above undoSpillBytes goes to a scratch file when
// its action commits or when following a structural edit makes it that
// large, and other payloads follow when a session or the sheet holds more
// than its share in memory.
const (
	undoSpillBytes    = 1 << 20
	undoSessionBytes  = 4 << 20
	undoInstanceBytes = 32 << 20
	// A spilled payload is not read for a structural edit. The edits it
	// depends on go to a log in the scratch directory, undoEditSegment at
	// a time, and the payload reads them when it is undone. A payload
	// waits for at most undoEditEntryBytes of edits and the log holds at
	// most undoEditLogBytes on disk. A payload that would pass the first
	// bound expires, and at the second the payload that waits for the most.
	undoEditSegment    = 1 << 20
	undoEditEntryBytes = 8 << 20
	undoEditLogBytes   = 64 << 20
	// The work of one undo to bring its payload up to the edits it waited
	// for, in the units of the calculation budget (undo_follow.go). The
	// entry of an undo that would pass it expires.
	undoFollowUnits = 2 << 20
)

// undoFile is the scratch file of one entry's payload. ids is the number
// of ids its runs stand for (spillEnc.wrote).
type undoFile struct {
	path       string
	bytes, ids int64
}

// undoScratch is the scratch storage of one instance's undo payloads and
// the count of the bytes that stay in memory. The writer owns it.
type undoScratch struct {
	root     string // where the directory is made; empty for the system's
	rootOf   func() (string, error)
	dir      string // made by makeDir before the first scratch file
	next     uint64
	resident int64
	log      *slog.Logger

	entryMax, sessionMax, instanceMax int64
	// followMax bounds the work of one catch-up (undoFollowUnits).
	followMax int64
	// plain is for tests: followRun.plain for every run.
	plain bool

	// marksAt is the number of write marks at which pruneMarks runs next,
	// and marksBelow the sequence below which it last dropped them.
	marksAt    int
	marksBelow uint64
	// markWork, when a test sets it, counts the marks and the history
	// entries that the fitting of the marks looks at (markFit).
	markWork func(n int)

	// admitted holds the entries of actions that have not committed yet.
	admitted []*undoEntry

	// edits is the log of structural edits for spilled payloads.
	edits editLog

	// fault lets tests fail a "write" or a "read" of a payload, and a
	// "blank-write" or a "blank-read" of the collected blank records.
	fault func(op string) error

	// blanks keeps the collected blank records (undo_blanks.go).
	blanks blankStore

	// notes counts the stand-in notes of every session (undo_notes.go).
	notes noteBudget
	// noteWork is the number of notes one tick looks through at most
	// (sweepNotes).
	noteWork int
	// said limits the log lines about storage errors (writer.go) and about
	// entries that expired.
	said scratchLog
}

var errSpillDamaged = errors.New("sheet: an undo scratch file is damaged")

// errSpillLate ends the read of a payload at the deadline of the undo's
// action. The entry and its file stay as they are.
var errSpillLate = errors.New("sheet: the deadline passed while an undo payload was read")

// spillPoll is how many bytes the check of a payload file reads between
// two looks at the deadline. Its decoding looks at the deadline once it
// has read spillPoll more bytes or spillPoll more things of a list since
// the last look, whichever comes first: a thing of a list can take a
// single byte, and a few bytes can stand for many ids.
const spillPoll = 4096

// spillReadWatch, when a test sets it, sees how far each read of a payload
// file got into the file before it ended, and the file's size.
var spillReadWatch func(at, size int64)

// The limits an entry can pass after its action committed. Its undo is
// then skipped with the reason lost (spec rule 23).
var (
	errUndoWork      = errors.New("sheet: the undo entry passed the work bound of following structural edits")
	errUndoEdits     = errors.New("sheet: the undo entry waits for more structural edits than one entry may")
	errUndoEditLog   = errors.New("sheet: the scratch log of structural edits is full")
	errUndoEditsGone = errors.New("sheet: structural edits the undo entry waited for could not be stored")
	errUndoNotes     = errors.New("sheet: the stand-in notes of undo passed the sheet's share of memory")
	errUndoMemory    = errors.New("sheet: what the undo payloads in scratch files leave in memory passed the sheet's share of memory")
	errUndoMarks     = errors.New("sheet: the write marks undo keeps passed the sheet's share of memory")
)

// undoSpillAll makes every instance opened while it is set spill every
// payload, so that tests can run ordinary undo cases through scratch files.
var undoSpillAll bool

func scratchRefusal(err error) *WriteError {
	return &WriteError{Code: "unavailable", Reason: "undo_scratch", Retry: true, cause: err}
}

func (in *Instance) undoPool() *undoScratch {
	p := &in.scratch
	if p.entryMax == 0 {
		p.root, p.rootOf, p.log = in.opts.ScratchDir, in.opts.ScratchRoot, in.log.With("slug", in.slug)
		p.entryMax, p.sessionMax, p.instanceMax = undoSpillBytes, undoSessionBytes, undoInstanceBytes
		p.followMax, p.noteWork = undoFollowUnits, noteSweepWork
		p.edits.segment, p.edits.entryMax, p.edits.max = undoEditSegment, undoEditEntryBytes, undoEditLogBytes
		if undoSpillAll {
			p.entryMax = -1
		}
	}
	return p
}

// makeDir makes the scratch directory when the instance has none, or has
// one that was removed. The host is asked for the place every time
// (Options.ScratchRoot): a directory that could not be made at one attempt
// can be made at the next, after the host's storage was repaired.
func (p *undoScratch) makeDir() error {
	if p.dir != "" {
		if _, err := os.Stat(p.dir); !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		p.dir = ""
	}
	root := p.root
	if p.rootOf != nil {
		var err error
		if root, err = p.rootOf(); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp(root, "flats-sheet-undo-")
	if err != nil {
		return err
	}
	p.dir = dir
	return nil
}

// close removes the scratch directory with every file in it.
func (p *undoScratch) close() {
	p.blanks.close()
	if p.dir == "" {
		return
	}
	if err := os.RemoveAll(p.dir); err != nil && p.log != nil {
		p.log.Warn("sheet undo scratch", "err", err)
	}
	p.dir = ""
}

// admitUndo takes the undo entry of an action that is about to commit. It
// spills what the bounds ask for: the new payload, older ones of the same
// session, and for the sheet's bound those of other sessions when this one
// has none left in memory. A scratch file that cannot be written refuses
// the action as unavailable, before anything of it is durable. Where
// nothing is left to spill, entries expire (squeeze). They stay expired
// when the action fails to commit after that, so the bounds hold at every
// moment, whatever becomes of the action.
func (in *Instance) admitUndo(s *session, e *undoEntry) *WriteError {
	if s == nil || e == nil {
		return nil
	}
	if !e.canon {
		e.canonical(&s.undo, false)
	}
	gone := s.undo.leaving(e)
	e.pushes, e.takes = nil, nil
	p := in.undoPool()
	// An action that was refused or whose commit failed gave its sequence
	// back. Its entry never reached a stack, and what it left here goes.
	waiting := p.admitted[:0]
	for _, earlier := range p.admitted {
		switch {
		case earlier.placed:
		case earlier.seq >= e.seq:
			p.forget(earlier)
		default:
			waiting = append(waiting, earlier)
		}
	}
	clear(p.admitted[len(waiting):])
	p.admitted = append(waiting, e)
	e.depend()
	e.pool, e.owner, e.size = p, &s.undo, e.measure()
	p.resident += e.size
	e.counted = true
	refuse := func(err error) *WriteError {
		p.forget(e)
		return scratchRefusal(err)
	}
	if e.size > p.entryMax {
		if err := p.spill(e); err != nil {
			return refuse(err)
		}
	}
	// The entries of the session's earlier actions in this transaction
	// join its stacks with this one.
	var joining []*undoEntry
	for _, a := range p.admitted {
		if a.owner == &s.undo {
			joining = append(joining, a)
		}
	}
	// What the action takes off the stacks when it commits counts as gone
	// here, with the notes that name it, so that nothing gives way in its
	// place.
	var free int64
	if len(gone) > 0 {
		u := &s.undo
		for _, g := range gone {
			if !g.leaving {
				g.leaving = true
				free += g.inMemory()
			}
		}
		for _, sets := range u.notes {
			for _, set := range sets {
				if named := u.entry(set.canon, set.redo); named != nil && named.leaving {
					free += set.bytes
				}
			}
		}
		defer func() {
			for _, g := range gone {
				g.leaving = false
			}
		}()
	}
	if err := p.squeeze(&s.undo, joining, free, p.spill); err != nil {
		return refuse(err)
	}
	// The sheet's bound counts the write marks, those of this action
	// among them. What the action needs to go to a scratch file goes now,
	// while a file that cannot be written can still refuse it; after the
	// commit, such a payload could only expire.
	if p.resident+p.notes.bytes+in.wb.markBytes()-free > p.instanceMax {
		in.smu.Lock()
		err := in.squeezeSheet(&s.undo, joining, free, p.spill)
		in.smu.Unlock()
		if err != nil {
			return refuse(err)
		}
	}
	return nil
}

// leaving returns the entries that leave the stacks when the action of the
// new entry e commits. An undo or a redo takes one entry back, and its
// notes can push others out of the limits (install). Any other action
// clears the redo stack. An entry that joins the undo stack pushes the
// oldest out where the stack then holds more than it keeps. The entries
// that the earlier actions of the transaction take off the stacks are
// among them (pushing, clearing).
func (u *undoState) leaving(e *undoEntry) []*undoEntry {
	var out []*undoEntry
	for _, ex := range e.pushes {
		out = append(out, ex.e)
	}
	pushing, clearing := u.pushing, u.clearing
	switch {
	case e.takes == nil:
		pushing, clearing = pushing+1, true
	case e.redoes:
		pushing++
		out = append(out, e.takes)
	default:
		out = append(out, e.takes)
	}
	if clearing {
		out = append(out, u.redo...)
	}
	if over := len(u.undo) + pushing - maxUndoEntries; over > 0 {
		out = append(out, u.undo[:min(over, len(u.undo))]...)
	}
	return out
}

// squeeze brings what a session holds in memory back inside its bound.
// Its payloads go to scratch files through spill, largest first. When none
// is left in memory, what the spilled payloads leave there is all that can
// still give way: the entry that leaves the most expires, then the next
// (spec rule 23). The session filled its own budget, so that is not
// logged (expire). joining holds the entries that are about to join the
// stacks, and free is what the entries marked as leaving take, which do
// not count and are not chosen. An error of spill ends the call.
func (p *undoScratch) squeeze(u *undoState, joining []*undoEntry, free int64, spill func(*undoEntry) error) error {
	held := func() int64 {
		n := u.resident() - free
		for _, e := range joining {
			n += e.inMemory()
		}
		return n
	}
	for held() > p.sessionMax {
		// An unfinished sweep of notes gives its room back first.
		if p.notes.yield(u) {
			continue
		}
		if victim := u.largest(joining...); victim != nil {
			if err := spill(victim); err != nil {
				return err
			}
			continue
		}
		victim := u.heaviest(joining)
		if victim == nil {
			break
		}
		victim.expire(nil)
	}
	return nil
}

// squeezeSheet brings what all sessions hold in memory back inside the
// sheet's bound, with the write marks, in the order of squeeze: the marks
// no entry can ask about go, payloads go to scratch files, and when none
// is left in memory the entries whose spilled payloads leave the most
// there expire. An entry that expires can be the oldest that asks about
// marks, and the marks only it could ask about go before the next step.
// What can still be over the bound after that is the marks, which the
// caller sees to (fitSheet).
//
// first is the session whose action commits, and joining its entries that
// are about to join its stacks. They give way before those of the other
// sessions. first is nil after a structural edit, which can have changed
// what any session holds, and for fitSheet. free is as for squeeze. The
// caller holds the sessions' lock.
func (in *Instance) squeezeSheet(first *undoState, joining []*undoEntry, free int64, spill func(*undoEntry) error) error {
	f := &markFit{in: in}
	defer f.finish()
	return in.squeezeSheetWith(f, first, joining, free, spill)
}

// squeezeSheetWith is squeezeSheet within the fit f of the marks, which
// fitSheet goes on with.
func (in *Instance) squeezeSheetWith(f *markFit, first *undoState, joining []*undoEntry, free int64, spill func(*undoEntry) error) error {
	p := &in.scratch
	over := func() bool { return p.resident+p.notes.bytes+f.bytes()-free > p.instanceMax }
	if !over() {
		return nil
	}
	f.begin()
	reconsider := f.follow
	// The entries that can go to a file, and then those whose files can
	// expire, are listed once, of the first session apart, largest first:
	// what goes changes no other entry's size.
	var spillable, heavy victims
	for over() {
		// An unfinished sweep of notes gives its room back first.
		if p.notes.yield(nil) {
			continue
		}
		if victim := spillable.next(in, first, joining, func(e *undoEntry) bool { return e.file == nil }); victim != nil {
			// spillOrExpire can make the victim expire.
			if err := spill(victim); err != nil {
				return err
			}
			reconsider()
			continue
		}
		if p.resident+p.notes.bytes-free <= p.instanceMax {
			break
		}
		victim := heavy.next(in, first, joining, func(e *undoEntry) bool { return e.file != nil })
		if victim == nil {
			break
		}
		victim.expire(errUndoMemory)
		reconsider()
	}
	return nil
}

// victims lists the entries squeezeSheet can choose by one kind, largest
// first: those of the first session, with the entries about to join its
// stacks, and then those of any session. While an action is admitted, a
// session's entries are those on its stacks and those the earlier actions
// of the transaction admitted for it, which commit with the action. After
// a commit, admitted entries can belong to an action that was refused
// later; they are not chosen. Of entries that take the same, the oldest on
// the undo stack comes first, and one that is about to join last, as
// undoState.most chooses them.
type victims struct {
	first, others []*undoEntry
	listed        bool
}

// next returns the largest entry of the kind that can still be chosen.
// The lists are made at the first call; an entry that was chosen, or can
// no longer be, leaves them when it comes first.
func (v *victims) next(in *Instance, first *undoState, joining []*undoEntry, kind func(e *undoEntry) bool) *undoEntry {
	can := func(e *undoEntry) bool { return !e.lost && !e.leaving && e.size > 0 && kind(e) }
	p := &in.scratch
	if !v.listed {
		v.listed = true
		list := func(out []*undoEntry, u *undoState, joining []*undoEntry) []*undoEntry {
			p.count(len(u.undo) + len(u.redo) + len(joining))
			for _, part := range [3][]*undoEntry{u.undo, u.redo, joining} {
				for _, e := range part {
					if e != nil && can(e) {
						out = append(out, e)
					}
				}
			}
			return out
		}
		if first != nil {
			v.first = list(nil, first, joining)
		}
		var admitted map[*undoState][]*undoEntry
		if first != nil {
			p.count(len(p.admitted))
			for _, e := range p.admitted {
				if !e.placed {
					if admitted == nil {
						admitted = map[*undoState][]*undoEntry{}
					}
					admitted[e.owner] = append(admitted[e.owner], e)
				}
			}
		}
		for _, s := range in.sessions {
			v.others = list(v.others, &s.undo, admitted[&s.undo])
		}
		for _, l := range [2][]*undoEntry{v.first, v.others} {
			slices.SortStableFunc(l, func(a, b *undoEntry) int { return cmp.Compare(b.size, a.size) })
		}
	}
	for _, l := range [2]*[]*undoEntry{&v.first, &v.others} {
		for len(*l) > 0 {
			if e := (*l)[0]; can(e) {
				return e
			}
			p.count(1)
			*l = (*l)[1:]
		}
	}
	return nil
}

// inMemory returns the bytes an entry takes in memory: its payload, what
// its payload in a scratch file leaves there (left), or the record of an
// entry that expired.
func (e *undoEntry) inMemory() int64 { return e.size }

// entryBytes is the record of an entry. It stays on its stack until the
// entry leaves, also after its payload expired.
var entryBytes = formula.HeapBytes(unsafe.Sizeof(undoEntry{}), true)

// keepsBytes is the list of what an entry can bring back, which stays in
// memory beside a payload in a scratch file.
func (e *undoEntry) keepsBytes() int64 {
	return numsBytes(cap(e.keeps), unsafe.Sizeof(keptIDs{}))
}

// left returns what a payload in a scratch file leaves in memory: the
// entry's record, what it can bring back and the record and path of its
// file, and for a payload that follows structure what the entry depends on
// now, what the payload in the file depends on, and where the edits it
// waits for begin.
func (e *undoEntry) left() int64 {
	n := entryBytes + e.keepsBytes()
	if f := e.file; f != nil {
		n += formula.HeapBytes(unsafe.Sizeof(undoFile{}), true) + formula.TextBytes(len(f.path))
	}
	if w := e.wait; w != nil {
		n += formula.HeapBytes(unsafe.Sizeof(undoWait{}), true) + e.deps.bytes() + w.base.bytes() + mapBytes(w.needs)
	}
	return n
}

// resident returns the bytes the entries on the stacks and the session's
// stand-in notes take in memory.
func (u *undoState) resident() int64 {
	n := u.noteBytes
	for _, stack := range [2][]*undoEntry{u.undo, u.redo} {
		for _, e := range stack {
			n += e.inMemory()
		}
	}
	return n
}

// largest returns the entry with the largest payload in memory, among the
// stacks and the entries that are about to join them.
func (u *undoState) largest(joining ...*undoEntry) *undoEntry {
	return u.most(joining, func(e *undoEntry) bool { return e.file == nil })
}

// heaviest returns the entry whose payload in a scratch file leaves the
// most in memory, among the stacks and the entries that are about to join
// them.
func (u *undoState) heaviest(joining []*undoEntry) *undoEntry {
	return u.most(joining, func(e *undoEntry) bool { return e.file != nil })
}

func (u *undoState) most(joining []*undoEntry, counts func(e *undoEntry) bool) *undoEntry {
	var found *undoEntry
	consider := func(e *undoEntry) {
		if e != nil && !e.lost && !e.leaving && e.size > 0 && counts(e) && (found == nil || e.size > found.size) {
			found = e
		}
	}
	// Of entries that take the same, the oldest on the undo stack comes
	// first, and one that is about to join last.
	for _, stack := range [2][]*undoEntry{u.undo, u.redo} {
		for _, e := range stack {
			consider(e)
		}
	}
	for _, e := range joining {
		consider(e)
	}
	return found
}

// unplaced returns the entries admitted for u that have not reached its
// stacks. While an action is admitted, they belong to the earlier actions
// of its transaction (admitUndo forgets the others first).
func (p *undoScratch) unplaced(u *undoState) []*undoEntry {
	var out []*undoEntry
	for _, e := range p.admitted {
		if e.owner == u && !e.placed {
			out = append(out, e)
		}
	}
	return out
}

// forgetUnplaced forgets the entries that were admitted and reached no
// stack. The writer calls it between two changes, where such an entry
// belongs to an action that did not commit.
func (p *undoScratch) forgetUnplaced() {
	for _, e := range p.admitted {
		if !e.placed {
			p.forget(e)
		}
	}
	p.admitted = nil
}

// sweepUndo runs with the maintenance tick. It drops the write marks no
// undo entry can ask about (pruneMarks), counts the resident bytes again
// and removes the scratch files no entry on a stack owns, such as those of
// actions whose commit failed. A rollback lets what it clears go itself
// (clearUndo). The tick brings the sheet inside its bound after the notes
// are swept (fitSheet).
func (in *Instance) sweepUndo() {
	in.pruneMarks()
	in.pruneBlanks()
	p := &in.scratch
	if p.entryMax == 0 {
		return
	}
	p.forgetUnplaced()
	owned := map[string]bool{}
	var resident int64
	in.smu.Lock()
	for _, s := range in.sessions {
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for _, e := range stack {
				if e.file != nil {
					owned[e.file.path] = true
				}
				resident += e.inMemory()
			}
		}
	}
	// The edits no payload waits for any more leave the log.
	p.trimEdits(in.waiting())
	in.smu.Unlock()
	p.resident = resident
	for _, seg := range p.edits.segs {
		owned[seg.path] = true
	}
	if p.dir == "" {
		return
	}
	files, err := os.ReadDir(p.dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if path := filepath.Join(p.dir, f.Name()); !owned[path] && !isBlankFile(f.Name()) {
			os.Remove(path)
		}
	}
}

// sharedFormulaBytes is what the workbook's table of shared formulas
// attaches to each formula it shares (intern): a weak handle and the
// cleanup that drops the formula's entry, which live as long as the
// formula.
const sharedFormulaBytes = 192

// formulaBytes returns what a parsed formula keeps in memory for a holder
// that may be the last one to let go of it: its size, tree included
// (formula.Size), and what the table of shared formulas attaches to it.
func formulaBytes(f *formula.Formula) int64 { return f.Size() + sharedFormulaBytes }

// What the allocator takes for a saved record, and for a row of them
// without its records.
var (
	cellBytes    = formula.HeapBytes(unsafe.Sizeof(Cell{}), true)
	rowDataBytes = formula.HeapBytes(unsafe.Sizeof(rowData{}), true)
)

// sliceBytes returns what the allocator takes for the backing array of a
// slice of n places of size bytes each, with pointers.
func sliceBytes(n int, size uintptr) int64 { return formula.HeapBytes(uintptr(n)*size, true) }

// numsBytes is sliceBytes for places of size bytes without pointers.
func numsBytes(n int, size uintptr) int64 { return formula.HeapBytes(uintptr(n)*size, false) }

// mapBytes returns what the allocator takes, at most, for a map that was
// made for its entries or grew to them without deleting any: its header,
// its tables and the groups of slots in them. A table keeps an eighth of
// its slots empty, and somewhat more than half of them just after it
// doubled; with the rounding of large groups to whole pages, three slots
// and their control bytes for each entry cover it. The smallest map has
// one group of eight slots. TestMapBytesCoverTheHeap holds the bound
// against the heap. A map that held more entries keeps their room, so a
// saved tab's maps are made anew before it is saved (txn.compactSaved).
func mapBytes[K comparable, V any](m map[K]V) int64 {
	if m == nil {
		return 0
	}
	return mapRoom[K, V](len(m))
}

// mapRoom is mapBytes for a map that held at most n entries.
func mapRoom[K comparable, V any](n int) int64 {
	var slot struct {
		k K
		v V
	}
	return 512 + 3*int64(max(n, 8))*int64(unsafe.Sizeof(slot)+1)
}

// axisBytes returns what an axis keeps: its ids at the capacity of their
// array, their positions and their born runs.
func axisBytes(a *axis) int64 {
	return numsBytes(cap(a.ids), 8) + mapBytes(a.pos) + numsBytes(cap(a.born), unsafe.Sizeof(stampRun{}))
}

// measure estimates the bytes the payload takes in memory. Saved records
// and the lists that hold them count at what the allocator takes for them,
// the lists at their capacity: a deleted row keeps the list it had in the
// sheet.
func (e *undoEntry) measure() int64 {
	n := entryBytes + e.keepsBytes() + sliceBytes(cap(e.cells), unsafe.Sizeof(cellRestore{})) + mapBytes(e.styles)
	// What following adds: the deletions the entry keeps apart, with the
	// ids they took, and the tabs it depends on.
	n += e.gapBytes() + e.deps.bytes()
	// A parsed formula is held by pointer and can be shared by many cells.
	// Each is counted once, at what its parsed form takes.
	var counted map[*formula.Formula]struct{}
	parsed := func(f *formula.Formula) {
		if f == nil {
			return
		}
		if _, ok := counted[f]; ok {
			return
		}
		if counted == nil {
			counted = map[*formula.Formula]struct{}{}
		}
		counted[f] = struct{}{}
		n += formulaBytes(f)
	}
	// A currency symbol is a text of its own, shared by the copies of the
	// format that holds it. It counts for each record that keeps it,
	// unless the record before it keeps the same one.
	var symbol *byte
	format := func(f *formula.Format) {
		if sym := f.Number.Symbol; sym != "" && unsafe.StringData(sym) != symbol {
			symbol = unsafe.StringData(sym)
			n += formula.TextBytes(len(sym))
		}
	}
	for i := range e.cells {
		r := &e.cells[i]
		if r.f != nil {
			parsed(r.f)
		} else {
			n += formula.TextBytes(len(r.before.Text))
		}
		format(&r.format)
	}
	for _, state := range e.styles {
		format(&state.f)
	}
	cell := func(c *Cell) {
		if c == nil {
			return
		}
		n += cellBytes
		if c.f != nil {
			parsed(c.f)
			n += formula.TextBytes(len(c.val.Text()))
		} else {
			n += formula.TextBytes(len(c.content.Text))
		}
	}
	row := func(rd *rowData) {
		n += rowDataBytes + sliceBytes(cap(rd.cells), unsafe.Sizeof(cellSlot{}))
		for _, slot := range rd.cells {
			cell(slot.c)
		}
	}
	n += sliceBytes(cap(e.steps), unsafe.Sizeof(undoStep{}))
	for i := range e.steps {
		s := &e.steps[i]
		// The lists of a step count at the capacity of their arrays: a
		// list that grew keeps the room it grew by.
		n += numsBytes(cap(s.ids), 8) + numsBytes(cap(s.stamps), 8) + numsBytes(cap(s.order), 8) + numsBytes(cap(s.rowAt), 4)
		n += sliceBytes(cap(s.colAt), unsafe.Sizeof([]int32(nil)))
		for _, at := range s.colAt {
			n += numsBytes(cap(at), 4)
		}
		n += numsBytes(cap(s.widths), unsafe.Sizeof(savedWidth{})) + formula.TextBytes(len(s.name))
		n += numsBytes(cap(s.sorted), unsafe.Sizeof(savedSort{}))
		n += sliceBytes(cap(s.repairs), unsafe.Sizeof(repair{}))
		for k := range s.repairs {
			n += numsBytes(cap(s.repairs[k].tabs), 8)
			parsed(s.repairs[k].before)
		}
		n += sliceBytes(cap(s.saved), unsafe.Sizeof(savedRow{}))
		for _, saved := range s.saved {
			row(saved.rd)
		}
		n += sliceBytes(cap(s.cells), unsafe.Sizeof(savedCell{}))
		for _, saved := range s.cells {
			cell(saved.c)
		}
		if t := s.savedTab; t != nil {
			// The tab, its axes and its maps, which were made for what it
			// holds when it was saved (txn.compactSaved).
			n += formula.HeapBytes(unsafe.Sizeof(Tab{}), true) + formula.TextBytes(len(t.name))
			n += axisBytes(&t.rows) + axisBytes(&t.cols) + numsBytes(cap(t.chunks), 8)
			n += mapBytes(t.widths) + mapBytes(t.lastSort) + mapBytes(t.marks) + mapBytes(t.colMarks) + mapBytes(t.data)
			for _, rd := range t.data {
				row(rd)
			}
		}
	}
	return n
}

// spill moves the payload of an entry into a scratch file.
func (p *undoScratch) spill(e *undoEntry) error {
	file, err := p.write(e)
	if err != nil {
		return err
	}
	if e.deps != nil {
		// The payload in the file depends on what the entry depends on now.
		e.wait = &undoWait{base: e.deps.clone()}
	}
	e.file, e.cells, e.steps, e.styles, e.gaps = file, nil, nil, nil, nil
	p.weigh(e)
	return nil
}

// weigh counts again what a payload in a scratch file leaves in memory,
// and reports whether that is more than before.
func (p *undoScratch) weigh(e *undoEntry) (grew bool) {
	size := e.left()
	grew = size > e.size
	if e.counted {
		p.resident += size - e.size
	}
	e.size = size
	return grew
}

// resize counts a payload in memory again after following changed it.
func (p *undoScratch) resize(e *undoEntry) {
	e.depend()
	size := e.measure()
	if e.counted {
		p.resident += size - e.size
	}
	e.size = size
}

// fit brings the payloads of one session back inside the bounds after
// following made some of them larger, as admitUndo does for a new one.
func (p *undoScratch) fit(u *undoState) {
	for _, stack := range [2][]*undoEntry{u.undo, u.redo} {
		for _, e := range stack {
			switch {
			case e.file != nil || e.lost:
			case e.gapBytes() > p.edits.entryMax:
				// The deletions an entry keeps apart are edits it waits
				// for, in memory or in the log.
				e.expire(errUndoEdits)
			case e.size > p.entryMax:
				p.spillOrExpire(e)
			}
		}
	}
	p.squeeze(u, nil, 0, p.orExpire)
}

// orExpire is spillOrExpire for squeeze.
func (p *undoScratch) orExpire(e *undoEntry) error {
	p.spillOrExpire(e)
	return nil
}

// spillOrExpire moves a payload out of memory. The action that made it
// too large has committed and cannot be refused, so a payload that cannot
// be written expires.
func (p *undoScratch) spillOrExpire(e *undoEntry) {
	if err := p.spill(e); err != nil {
		e.expire(err)
	}
}

// expire gives up an entry that passed a limit of undo history, or whose
// payload cannot be kept (spec rule 23). Every such entry comes through
// here, and gives up at once all it holds: its payload in memory and its
// file, what it depends on and the edits it waits for, the rows, columns
// and tabs it could bring back, and the stand-in notes that name it, with
// their bytes in the budgets of the session and of the sheet. The entry
// stays on its stack, and its undo is skipped with the reason lost.
//
// The stacks, the notes and the pool belong to the writer, which calls
// this with the workbook locked. The edit log drops the edits that only
// this entry waited for when it is trimmed next (logEdits, sweepUndo):
// trimming reads the payloads of every session, under the sessions' lock.
//
// why is the cause for the host's log. It is nil for a session that
// filled its own budget: its oldest entries leave as they do at
// maxUndoEntries, and that is ordinary use. Every other cause is a
// storage error, or a bound that the sessions of a sheet share or that
// other people's edits fill, and nothing but the log tells the operator.
func (e *undoEntry) expire(why error) {
	if e.lost {
		return
	}
	if why != nil && e.pool != nil {
		e.pool.logLost(why)
	}
	e.release()
	e.cells, e.steps, e.styles, e.gaps, e.keeps = nil, nil, nil, nil, nil
	e.deps, e.wait, e.lost = nil, nil, true
	// The record stays on its stack until the entry leaves it, and counts
	// until then (forget).
	e.size = entryBytes
	if e.pool != nil {
		e.pool.resident += e.size
		e.counted = true
	}
	if e.owner != nil {
		e.owner.prune()
	}
}

// logLost logs that an entry expired. The line counts in the limit of the
// lines about scratch storage errors (scratchFailed in writer.go): one in
// scratchLogEvery for a sheet, with the count of those between.
func (p *undoScratch) logLost(why error) {
	if p.log == nil {
		return
	}
	l := &p.said
	if now := time.Now(); now.Sub(l.last) >= scratchLogEvery {
		p.log.Warn("sheet undo payload lost", "err", why, "suppressed", l.skipped)
		l.last, l.skipped = now, 0
		return
	}
	l.skipped++
}

// forget drops what the pool holds for an entry that is gone.
func (p *undoScratch) forget(e *undoEntry) {
	if e.counted {
		p.resident -= e.size
		e.counted = false
	}
	if e.file != nil {
		os.Remove(e.file.path)
		e.file = nil
	}
}

// load reads a spilled payload and brings it up to the present structure:
// it follows the edits of the scratch log that it waited for, through the
// same function that an entry in memory follows them with, and applies
// the deletions it still keeps apart. That work is metered: it may take
// the pool's followMax units, and budget, the budget of the undo's action,
// ends it at the action's deadline. load returns nil for a payload that is
// lost, also when the work passes its bound, and a refusal when a file
// cannot be read now. Reading the file looks at the deadline as it goes,
// and one that passes it is refused as calculating. The entry and its
// file stay as they are otherwise.
func (e *undoEntry) load(wb *Workbook, budget *formula.Budget) (*undoEntry, *WriteError) {
	if e.lost {
		return nil, nil
	}
	p := e.pool
	var late func() bool
	if budget != nil {
		late = func() bool { return !budget.Check() }
	}
	loaded, err := p.read(wb, e, late)
	if errors.Is(err, errSpillLate) {
		return nil, calcTimeout()
	}
	if w := e.wait; err == nil && w != nil {
		loaded.deps = w.base.clone()
		c := &followRun{e: loaded, wb: wb, plain: p.plain, metered: true, budget: budget, left: p.followMax}
		reads := 0
		err = p.edits.replay(p, w, c, func(edits []logEntry) {
			reads += len(edits)
			loaded.follow(c, edits)
		})
		switch {
		case err != nil:
		case c.stop:
			err = errUndoWork
		case reads != w.reads:
			err = fmt.Errorf("%w: the edit log holds %d of the %d edits the payload reads", errSpillDamaged, reads, w.reads)
		default:
			if loaded.settleIn(c); c.stop {
				err = errUndoWork
			} else if c.changed && !loaded.shareSaved(wb, budget) {
				return nil, calcTimeout()
			}
		}
		if followWatch != nil {
			followWatch(c)
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, errSpillDamaged), errors.Is(err, errUndoWork):
		e.expire(err)
		return nil, nil
	case err != nil:
		return nil, scratchRefusal(err)
	}
	return loaded, nil
}

// shareSaved shares the formulas of the saved cells with the live sheet,
// which is where an undo puts them. Before each spillPoll cells it reads
// the clock and counts them on budget, a sixteenth of a unit each: the
// units alone would reach the budget's own look only every 65,536 cells.
// shareSaved reports false, and leaves the rest, once the deadline has
// passed.
func (e *undoEntry) shareSaved(wb *Workbook, budget *formula.Budget) bool {
	n := 0
	cell := func(c *Cell) bool {
		if n%spillPoll == 0 {
			if !budget.Check() {
				return false
			}
			budget.Add(spillPoll / 16)
		}
		n++
		if c != nil && c.f != nil {
			c.f = wb.intern(c.f)
			c.content = formulaContent(c.f)
		}
		return true
	}
	for i := range e.steps {
		s := &e.steps[i]
		for _, saved := range s.saved {
			for _, slot := range saved.rd.cells {
				if !cell(slot.c) {
					return false
				}
			}
		}
		for _, saved := range s.cells {
			if !cell(saved.c) {
				return false
			}
		}
		if s.savedTab != nil {
			for _, rd := range s.savedTab.data {
				for _, slot := range rd.cells {
					if !cell(slot.c) {
						return false
					}
				}
			}
		}
	}
	return true
}

// undoWait is what a spilled payload that follows structure keeps in
// memory: what the payload in the file depends on, and the edits of the
// scratch log it waits for. It counts in the undo memory of the session
// and of the sheet (left), and its size has a bound: base holds at most
// depsKept tabs and names, and needs at most needsKept tabs.
type undoWait struct {
	base *undoDeps
	// needs holds, for each tab with an edit the payload waits for, the
	// ordinal of the first of them. It notes at most needsKept tabs, in
	// the order their edits come.
	needs map[TabID]uint64
	// rest is set when the payload waits for an edit of a tab that needs
	// has no room for: the ordinal of that edit. From it on the payload
	// reads the edits of every tab that needs does not hold, as an entry
	// that names more tabs than it keeps depends on every tab. Tabs that
	// come and go therefore add nothing here.
	rest  uint64
	bytes int64 // what the edits it waits for take in the log
	// reads counts the edits of the log that replay hands the payload:
	// those it waits for, and those logged for other payloads in the tabs
	// it reads. A replay that hands it another number has lost edits.
	reads int
}

// await notes the edits of a committed action that a spilled payload
// depends on. It does not touch the payload. first is the ordinal of the
// first edit, and keep gets true for each edit the payload waits for. all
// makes it wait for every edit (followRun.plain).
func (e *undoEntry) await(edits []logEntry, first uint64, keep []bool, all bool) {
	w := e.wait
	for i := range edits {
		ed := &edits[i]
		if !e.deps.take(ed) && !all {
			continue
		}
		if _, ok := w.needs[ed.tab]; !ok && w.rest == 0 {
			if len(w.needs) < needsKept {
				if w.needs == nil {
					w.needs = map[TabID]uint64{}
				}
				w.needs[ed.tab] = first + uint64(i)
			} else {
				w.rest = first + uint64(i)
			}
		}
		w.bytes += editBytes(ed)
		keep[i] = true
	}
}

// needsKept is the number of tabs of which a spilled payload notes where
// the edits it waits for begin. It is more than the tabs a sheet has at
// one time, so a payload reads the rest only after tabs came and went.
const needsKept = 64

// editBytes is what an edit counts for in the scratch log.
func editBytes(ed *logEntry) int64 { return int64(ed.size() + len(ed.tr.Tab) + len(ed.tr.Name)) }

// editLog is the scratch log of the structural edits that spilled payloads
// wait for. An action adds its own edits and nothing else: no payload is
// read or written for it. An edit reaches the log only when some spilled
// payload depends on it, and leaves when none waits for it any more.
//
// The newest edits are in memory, in commit order. A flush writes them to
// one file for each tab they changed, so that the edits of a tab leave as
// soon as no payload waits for that tab, whatever was logged beside them.
type editLog struct {
	segs []editSegment // on disk: by flush, oldest first, and then by tab
	// open holds the newest edits, which no segment has yet.
	open      []loggedEdit
	openBytes int64
	bytes     int64  // of the segments, each counted in whole blocks
	ord       uint64 // the ordinal of the last structural edit of the sheet
	flushes   uint64
	next      uint64 // names the files
	// segment is the size of open that starts a flush, entryMax the edits
	// one payload may wait for and max the bytes on disk.
	segment, entryMax, max int64
}

type loggedEdit struct {
	logEntry
	ord uint64
}

type editSegment struct {
	path        string
	tab         TabID
	flush       uint64
	first, last uint64 // ordinals
	bytes       int64
	ids         int64 // as for undoFile
}

// editBlock is the least a file takes on disk.
const editBlock = 4096

// followUndo applies the structural edits of a committed action to the
// undo stacks. A payload in memory follows those it depends on, and is
// counted again when they changed it; for a spilled payload they go to the
// scratch log, and what the payload leaves in memory is counted again.
// Sessions that hold more afterwards, and then the sheet, are brought
// back inside their bounds. The caller holds the sessions' lock.
func (in *Instance) followUndo(wb *Workbook, edits []logEntry) {
	p := in.undoPool()
	l := &p.edits
	first := l.ord + 1
	l.ord += uint64(len(edits))
	var keep []bool
	await := func(e *undoEntry) {
		if keep == nil {
			keep = make([]bool, len(edits))
		}
		e.await(edits, first, keep, p.plain)
	}
	runs := map[*logEntry][][2]uint64{}
	grown := false
	// The payloads in memory follow the edits inside the work the action
	// has for all of them, which is what one catch-up may take.
	spare := p.followMax
	for _, s := range in.sessions {
		changed := false
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for _, e := range stack {
				switch {
				case e.deps == nil || e.lost:
				case e.file != nil:
					await(e)
					// A rename can give the entry a longer name to depend
					// on, and an edit a tab to note.
					changed = p.weigh(e) || changed
				case e.size+e.deps.apart > p.entryMax && e.deps.parts(edits):
					// Such an edit can leave every cell with a formula of
					// its own, and the payload over its bound before it is
					// counted again. It follows from a file instead.
					if p.spillOrExpire(e); e.file != nil {
						await(e)
						p.weigh(e)
					}
					changed = true
				default:
					c := &followRun{e: e, wb: wb, plain: p.plain, intern: true, runs: runs, spare: &spare}
					taken := e.follow(c, edits)
					if c.changed {
						p.resize(e)
						changed = true
					}
					if taken < len(edits) {
						// The work is used up. The payload follows the rest
						// of the edits from a file, at its undo. What it
						// leaves in memory is counted again after it took
						// those edits, which can add tabs it waits for and
						// names it depends on, and the session is brought
						// back inside its bound: also when an earlier
						// payload used up the work and this one followed
						// none of the edits.
						if p.spillOrExpire(e); e.file != nil {
							if keep == nil {
								keep = make([]bool, len(edits))
							}
							e.await(edits[taken:], first+uint64(taken), keep[taken:], p.plain)
							p.weigh(e)
						}
						changed = true
					}
				}
			}
		}
		if changed {
			p.fit(&s.undo)
			grown = true
		}
	}
	if grown {
		in.squeezeSheet(nil, nil, 0, p.orExpire)
	}
	if keep != nil {
		in.logEdits(edits, first, keep)
	}
}

// waiting returns the spilled payloads that follow structure. The caller
// holds the sessions' lock.
func (in *Instance) waiting() []*undoEntry {
	var out []*undoEntry
	for _, s := range in.sessions {
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for _, e := range stack {
				if e.wait != nil && e.file != nil && !e.lost {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// logEdits adds the edits of one committed action that some spilled
// payload waits for to the scratch log, and keeps the log inside its
// bounds: a payload that waits for more than one payload may expires, and
// while the log holds more on disk than it may, the payload that waits for
// the most.
func (in *Instance) logEdits(edits []logEntry, first uint64, keep []bool) {
	p := &in.scratch
	l := &p.edits
	waiting := in.waiting()
	for i := range edits {
		if !keep[i] {
			continue
		}
		ord := first + uint64(i)
		l.open = append(l.open, loggedEdit{edits[i], ord})
		l.openBytes += editBytes(&edits[i])
		for _, e := range waiting {
			if e.wait.wants(edits[i].tab, ord) {
				e.wait.reads++
			}
		}
	}
	expired := false
	expire := func(e *undoEntry, why error) {
		e.expire(why)
		expired = true
	}
	// trim lets the edits go that only expired payloads waited for, and
	// those of payloads that have left their stacks since the last tick.
	trim := func() {
		if expired {
			p.trimEdits(waiting)
			expired = false
		}
	}
	for _, e := range waiting {
		if e.wait.bytes > l.entryMax {
			expire(e, errUndoEdits)
		}
	}
	trim()
	if l.openBytes >= l.segment {
		if err := p.flushEdits(); err != nil {
			in.scratchFailed("edit log", err)
			// The edits wait in memory for the next try, up to eight
			// segments. Beyond that the payloads that wait for them expire.
			if l.openBytes > 8*l.segment {
				for _, e := range waiting {
					if !e.lost && slices.ContainsFunc(l.open, func(ed loggedEdit) bool { return e.wait.wants(ed.tab, ed.ord) }) {
						expire(e, fmt.Errorf("%w: %v", errUndoEditsGone, err))
					}
				}
			}
		}
	}
	expired = expired || l.bytes > l.max
	for {
		trim()
		if l.bytes <= l.max {
			return
		}
		var victim *undoEntry
		for _, e := range waiting {
			if !e.lost && (victim == nil || e.wait.bytes > victim.wait.bytes) {
				victim = e
			}
		}
		if victim == nil {
			return
		}
		expire(victim, errUndoEditLog)
	}
}

// wants reports whether the payload waits for the edit of a tab with the
// given ordinal.
func (w *undoWait) wants(tab TabID, ord uint64) bool {
	if from, ok := w.needs[tab]; ok {
		return ord >= from
	}
	return w.rest != 0 && ord >= w.rest
}

// trimEdits drops the edits that none of the given payloads waits for.
func (p *undoScratch) trimEdits(waiting []*undoEntry) {
	l := &p.edits
	// For each tab, the first ordinal some payload waits for, and the
	// first from which some payload reads every tab it does not note. A
	// payload notes its tabs before it reads the rest, so what it notes
	// begins before its rest does.
	from := map[TabID]uint64{}
	var rest uint64
	for _, e := range waiting {
		if e.lost || e.wait == nil {
			continue
		}
		for tab, ord := range e.wait.needs {
			if have, ok := from[tab]; !ok || ord < have {
				from[tab] = ord
			}
		}
		if r := e.wait.rest; r != 0 && (rest == 0 || r < rest) {
			rest = r
		}
	}
	wanted := func(tab TabID, last uint64) bool {
		if ord, ok := from[tab]; ok && last >= ord {
			return true
		}
		return rest != 0 && last >= rest
	}
	segs := l.segs[:0]
	for _, seg := range l.segs {
		if wanted(seg.tab, seg.last) {
			segs = append(segs, seg)
			continue
		}
		os.Remove(seg.path)
		l.bytes -= seg.bytes
	}
	clear(l.segs[len(segs):])
	l.segs = segs
	open := l.open[:0]
	for _, ed := range l.open {
		if wanted(ed.tab, ed.ord) {
			open = append(open, ed)
			continue
		}
		l.openBytes -= editBytes(&ed.logEntry)
	}
	clear(l.open[len(open):])
	if l.open = open; len(open) == 0 {
		l.open = nil
	}
}

const editMagic = "flats sheet undo edits 2\n"

// flushEdits writes the open edits as segments, one for each tab. Either
// every segment is written or none: the segments of a flush hold the edits
// between the flush before and the next, which is the order replay reads
// them in.
func (p *undoScratch) flushEdits() error {
	l := &p.edits
	if len(l.open) == 0 {
		return nil
	}
	if p.fault != nil {
		if err := p.fault("edits-write"); err != nil {
			return err
		}
	}
	if err := p.makeDir(); err != nil {
		return err
	}
	var tabs []TabID
	byTab := map[TabID][]int{}
	for i := range l.open {
		tab := l.open[i].tab
		if _, ok := byTab[tab]; !ok {
			tabs = append(tabs, tab)
		}
		byTab[tab] = append(byTab[tab], i)
	}
	segs := make([]editSegment, 0, len(tabs))
	var size int64
	for _, tab := range tabs {
		list := byTab[tab]
		var buf bytes.Buffer
		w := bufio.NewWriter(&buf)
		enc := spillEnc{w: w}
		w.WriteString(editMagic)
		enc.uv(uint64(len(list)))
		for _, i := range list {
			enc.uv(l.open[i].ord)
			enc.edit(&l.open[i].logEntry)
		}
		err := w.Flush()
		var path string
		if err == nil {
			var tail [4]byte
			binary.LittleEndian.PutUint32(tail[:], crc32.ChecksumIEEE(buf.Bytes()))
			buf.Write(tail[:])
			l.next++
			path = filepath.Join(p.dir, strconv.FormatUint(l.next, 10)+".edits")
			err = os.WriteFile(path, buf.Bytes(), 0o600)
		}
		if err != nil {
			os.Remove(path)
			for _, seg := range segs {
				os.Remove(seg.path)
			}
			if errors.Is(err, fs.ErrNotExist) {
				// The directory is gone, and the segments with it.
				p.dir = ""
			}
			return err
		}
		blocks := (int64(buf.Len()) + editBlock - 1) / editBlock * editBlock
		segs = append(segs, editSegment{path, tab, l.flushes + 1, l.open[list[0]].ord, l.open[list[len(list)-1]].ord, blocks, enc.wrote})
		size += blocks
	}
	l.flushes++
	l.segs = append(l.segs, segs...)
	l.bytes += size
	clear(l.open)
	l.open, l.openBytes = nil, 0
	return nil
}

// replay hands the edits a payload waits for to apply, in commit order: a
// flush at a time, and then those in memory. It reads only the segments of
// tabs the payload waits for. Reading counts as work of the run c, and
// replay stops when the run does.
func (l *editLog) replay(p *undoScratch, w *undoWait, c *followRun, apply func([]logEntry)) error {
	if w.reads == 0 {
		return nil
	}
	var batch []loggedEdit
	hand := func(files int) {
		if len(batch) == 0 {
			return
		}
		if files > 1 {
			slices.SortFunc(batch, func(a, b loggedEdit) int { return cmp.Compare(a.ord, b.ord) })
		}
		edits := make([]logEntry, len(batch))
		for i := range batch {
			edits[i] = batch[i].logEntry
		}
		apply(edits)
	}
	for i := 0; i < len(l.segs) && !c.stop; {
		flush, files := l.segs[i].flush, 0
		batch = batch[:0]
		for ; i < len(l.segs) && l.segs[i].flush == flush; i++ {
			seg := &l.segs[i]
			if !w.wants(seg.tab, seg.last) {
				continue
			}
			// The file counts before it is read, so a run that has no work
			// left reads no other one.
			if !c.charge(seg.bytes / 16) {
				return nil
			}
			edits, err := p.readEdits(seg)
			if err != nil {
				return err
			}
			for _, ed := range edits {
				if w.wants(ed.tab, ed.ord) {
					batch = append(batch, ed)
				}
			}
			files++
		}
		hand(files)
	}
	if c.stop {
		return nil
	}
	batch = batch[:0]
	if !c.charge(int64(len(l.open)) / 16) {
		return nil
	}
	for _, ed := range l.open {
		if w.wants(ed.tab, ed.ord) {
			batch = append(batch, ed)
		}
	}
	hand(1)
	return nil
}

func (p *undoScratch) readEdits(seg *editSegment) ([]loggedEdit, error) {
	if p.fault != nil {
		if err := p.fault("edits-read"); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(seg.path)
	if err != nil {
		return nil, err
	}
	body := len(data) - 4
	if body < len(editMagic) || string(data[:len(editMagic)]) != editMagic ||
		binary.LittleEndian.Uint32(data[body:]) != crc32.ChecksumIEEE(data[:body]) {
		return nil, errSpillDamaged
	}
	dec := newSpillDec(bytes.NewReader(data[len(editMagic):body]), int64(body-len(editMagic)), seg.ids, nil)
	n := dec.count(spillEditBytes, 0)
	edits := make([]loggedEdit, 0, n)
	for i := 0; i < n && dec.err == nil; i++ {
		ord := dec.uv()
		edits = append(edits, loggedEdit{dec.edit(), ord})
	}
	if dec.err != nil {
		return nil, fmt.Errorf("%w: %v", errSpillDamaged, dec.err)
	}
	return edits, nil
}

func (s *spillEnc) edit(ed *logEntry) {
	s.uv(ed.seq)
	s.uv(uint64(ed.tab))
	s.uv(uint64(ed.tr.Kind))
	s.str(ed.tr.Tab)
	s.str(ed.tr.Name)
	s.iv(int64(ed.tr.At))
	s.iv(int64(ed.tr.Count))
	s.flag(ed.rows)
	s.ids(ed.gone)
	s.ids(ed.came)
	s.uv(ed.above)
	s.uv(ed.below)
	s.uv(uint64(len(ed.moved)))
	for row, mv := range ed.moved {
		s.uv(uint64(row))
		s.iv(int64(mv[0]))
		s.iv(int64(mv[1]))
	}
}

func (d *spillDec) edit() logEntry {
	ed := logEntry{seq: d.uv(), tab: TabID(d.uv())}
	ed.tr = formula.Transform{Kind: formula.TransformKind(d.uv()), Tab: d.name(), Name: d.name(), At: d.i32(), Count: d.i32()}
	ed.rows = d.flag()
	ed.gone, ed.came = d.ids(), d.ids()
	ed.above, ed.below = d.uv(), d.uv()
	if n := d.count(3, formula.MaxRows); n > 0 || ed.tr.Kind == 0 {
		ed.moved = make(map[RowID][2]int32, n)
		for i := 0; i < n && d.err == nil; i++ {
			d.poll()
			ed.moved[RowID(d.uv())] = [2]int32{d.i32(), d.i32()}
		}
	}
	d.checkEdit(&ed)
	return ed
}

// checkEdit checks the fields of a logged edit against each other: an
// insert brought, and a deletion took, as many ids as its transform
// counts from its place on, each once, on the axis of its kind, and a sort
// moved rows between places a tab has. A catch-up moves formulas by the
// transform and the places, and returns rows by the ids, so they must
// say the same.
func (d *spillDec) checkEdit(ed *logEntry) {
	if d.err != nil {
		return
	}
	tr := ed.tr
	if tr.Kind != 0 && len(ed.moved) > 0 {
		d.fail(fmt.Errorf("an edit of kind %d that moved rows", tr.Kind))
		return
	}
	var ids []uint64
	switch tr.Kind {
	case 0, formula.RenameTab:
		// A sort moves rows between places of its tab.
		for _, mv := range ed.moved {
			if d.poll(); d.err == nil && (mv[0] < 0 || mv[1] < 0 || mv[0] >= formula.MaxRows || mv[1] >= formula.MaxRows) {
				d.fail(fmt.Errorf("a sort that moved a row from %d to %d", mv[0], mv[1]))
			}
		}
		if len(ed.gone) > 0 || len(ed.came) > 0 {
			d.fail(fmt.Errorf("an edit of kind %d with ids", tr.Kind))
		}
		return
	case formula.InsertRows, formula.InsertCols:
		ids = ed.came
		if len(ed.gone) > 0 {
			d.fail(fmt.Errorf("an insert that took ids"))
			return
		}
	case formula.DeleteRows, formula.DeleteCols:
		ids = ed.gone
		if len(ed.came) > 0 {
			d.fail(fmt.Errorf("a deletion that brought ids"))
			return
		}
	default:
		d.fail(fmt.Errorf("an edit of kind %d", tr.Kind))
		return
	}
	rows := tr.Kind == formula.InsertRows || tr.Kind == formula.DeleteRows
	if tr.At < 0 || tr.Count <= 0 || int(tr.Count) != len(ids) || ed.rows != rows {
		d.fail(fmt.Errorf("an edit of kind %d at %d of %d with %d ids", tr.Kind, tr.At, tr.Count, len(ids)))
		return
	}
	d.idSet(ids, "an edit")
}

const spillMagic = "flats sheet undo 2\n"

// The least that some of the things a scratch file counts take in it, in
// bytes: each of their values is a byte or more. spillDec.count holds a
// count against them. A cell of an entry is three ids, a flag, its
// content, its place and three more values; a step is its 26 values with
// every list empty; and an edit of the log its ordinal and 13 values.
const (
	spillCellBytes = 10
	spillStepBytes = 26
	spillEditBytes = 14
)

func (p *undoScratch) write(e *undoEntry) (*undoFile, error) {
	if p.fault != nil {
		if err := p.fault("write"); err != nil {
			return nil, err
		}
	}
	var f *os.File
	var path string
	for attempt := 0; ; attempt++ {
		if err := p.makeDir(); err != nil {
			return nil, err
		}
		p.next++
		path = filepath.Join(p.dir, strconv.FormatUint(p.next, 10)+".undo")
		var err error
		if f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			break
		}
		if attempt > 0 || !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// The directory is gone. Its files are lost; a new one serves the
		// payloads from here on.
		p.dir = ""
	}
	sum := crc32.NewIEEE()
	w := bufio.NewWriterSize(io.MultiWriter(f, sum), 64<<10)
	enc := spillEnc{w: w, formulas: map[*formula.Formula]uint64{}}
	enc.entry(e)
	err := w.Flush()
	if err == nil {
		err = enc.err
	}
	if err == nil {
		var tail [4]byte
		binary.LittleEndian.PutUint32(tail[:], sum.Sum32())
		_, err = f.Write(tail[:])
	}
	var size int64
	if info, serr := f.Stat(); serr == nil {
		size = info.Size()
	} else if err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return &undoFile{path, size, enc.wrote}, nil
}

// read decodes the payload of a spilled entry into a new entry. The file
// is checked whole before any of it is used. late, when it is not nil,
// reports that the deadline passed; it is asked every spillPoll bytes of
// the check and things of the decoding, and read then returns
// errSpillLate.
func (p *undoScratch) read(wb *Workbook, e *undoEntry, late func() bool) (*undoEntry, error) {
	if p.fault != nil {
		if err := p.fault("read"); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(e.file.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if spillReadWatch != nil {
		defer func() {
			at, _ := f.Seek(0, io.SeekCurrent)
			spillReadWatch(at, info.Size())
		}()
	}
	body := info.Size() - 4
	if body < int64(len(spillMagic)) {
		return nil, errSpillDamaged
	}
	// The check reads the file through one buffer: a copy of each piece
	// would allocate as many bytes as the file has.
	sum, buf := crc32.NewIEEE(), make([]byte, spillPoll)
	for left := body; left > 0; left -= spillPoll {
		if late != nil && late() {
			return nil, errSpillLate
		}
		piece := buf[:min(left, spillPoll)]
		if _, err := io.ReadFull(f, piece); err != nil {
			return nil, err
		}
		sum.Write(piece)
	}
	var tail [4]byte
	if _, err := io.ReadFull(f, tail[:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(tail[:]) != sum.Sum32() {
		return nil, errSpillDamaged
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	dec := newSpillDec(f, body, e.file.ids, wb)
	dec.late = late
	loaded := dec.entry()
	if errors.Is(dec.err, errSpillLate) {
		return nil, errSpillLate
	}
	if dec.err != nil {
		return nil, fmt.Errorf("%w: %v", errSpillDamaged, dec.err)
	}
	loaded.seq = e.seq
	return loaded, nil
}

// copyPayload returns a copy of an entry's payload that shares nothing a
// follow changes with the entry. It takes it through the form a scratch
// file has, in memory. Its decoding asks late, when it is not nil, as the
// decoding of a scratch file does, and returns errSpillLate when the
// deadline passed.
func (e *undoEntry) copyPayload(wb *Workbook, late func() bool) (*undoEntry, error) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	enc := spillEnc{w: w, formulas: map[*formula.Formula]uint64{}}
	enc.entry(e)
	if err := errors.Join(w.Flush(), enc.err); err != nil {
		return nil, err
	}
	dec := newSpillDec(&buf, int64(buf.Len()), enc.wrote, wb)
	dec.late = late
	copied := dec.entry()
	if dec.err != nil {
		return nil, dec.err
	}
	copied.seq = e.seq
	return copied, nil
}

// spillEnc writes a payload. A formula is written once, as its stored
// text, and named by its number after that.
type spillEnc struct {
	w        *bufio.Writer
	tmp      [binary.MaxVarintLen64]byte
	formulas map[*formula.Formula]uint64
	formats  map[formula.Format]uint64
	err      error // the first value that could not be encoded
	// wrote counts the ids that the runs written stand for. The decoder
	// gives back no more than that.
	wrote int64
}

func (s *spillEnc) uv(v uint64) { s.w.Write(s.tmp[:binary.PutUvarint(s.tmp[:], v)]) }
func (s *spillEnc) iv(v int64)  { s.w.Write(s.tmp[:binary.PutVarint(s.tmp[:], v)]) }

func (s *spillEnc) flag(v bool) {
	if v {
		s.w.WriteByte(1)
	} else {
		s.w.WriteByte(0)
	}
}

func (s *spillEnc) str(v string) {
	s.uv(uint64(len(v)))
	s.w.WriteString(v)
}

func (s *spillEnc) ids(ids []uint64) {
	s.wrote += int64(len(ids))
	s.uv(uint64(len(ids)))
	forRuns(ids, func(first, count uint64) {
		s.uv(first)
		s.uv(count)
	})
}

func (s *spillEnc) formula(f *formula.Formula) {
	if f == nil {
		s.uv(0)
		return
	}
	if n, ok := s.formulas[f]; ok {
		s.uv(n + 2)
		return
	}
	s.formulas[f] = uint64(len(s.formulas))
	s.uv(1)
	s.flag(f.Unreadable())
	s.str(f.Stored())
}

// content writes what a cell holds. Formula content is its formula.
func (s *spillEnc) content(c formula.Content, f *formula.Formula) {
	s.formula(f)
	if f != nil {
		return
	}
	s.uv(uint64(c.Kind))
	binary.LittleEndian.PutUint64(s.tmp[:8], math.Float64bits(c.Num))
	s.w.Write(s.tmp[:8])
	s.str(c.Text)
}

func (s *spillEnc) cell(c *Cell) {
	s.content(c.content, c.f)
	s.uv(uint64(c.style))
	s.uv(uint64(c.by))
	s.uv(c.seq)
	s.uv(c.textSeq)
}

// format writes a format the first time it comes, and from then on its
// number. The cells of a formatted range hold a few formats between them.
func (s *spillEnc) format(f formula.Format) {
	if n, ok := s.formats[f]; ok {
		s.uv(n + 1)
		return
	}
	if s.formats == nil {
		s.formats = map[formula.Format]uint64{}
	}
	s.formats[f] = uint64(len(s.formats))
	s.uv(0)
	data, err := appendFormat(nil, f)
	if err != nil && s.err == nil {
		// A file that held another format in its place would restore that
		// one. The write fails and its action is refused.
		s.err = err
	}
	s.str(string(data))
}

func (s *spillEnc) stamps(v *[propCount]uint64) {
	for _, stamp := range v {
		s.uv(stamp)
	}
}

func (s *spillEnc) entry(e *undoEntry) {
	s.w.WriteString(spillMagic)
	s.uv(uint64(len(e.styles)))
	for id, state := range e.styles {
		s.uv(uint64(id))
		s.format(state.f)
		s.stamps(&state.stamps)
	}
	s.uv(uint64(len(e.cells)))
	for i := range e.cells {
		r := &e.cells[i]
		s.uv(uint64(r.tab))
		s.uv(uint64(r.row))
		s.uv(uint64(r.col))
		s.flag(r.content)
		s.content(r.before, r.f)
		s.iv(int64(r.pos.Row))
		s.iv(int64(r.pos.Col))
		s.uv(r.beforeSeq)
		s.uv(uint64(r.beforeBy))
		s.uv(uint64(r.props))
		if r.props != 0 {
			s.format(r.format)
			s.stamps(&r.beforeStamps)
		}
	}
	s.uv(uint64(len(e.steps)))
	for i := range e.steps {
		s.step(&e.steps[i])
	}
	s.uv(uint64(len(e.gaps)))
	for i := range e.gaps {
		g := &e.gaps[i]
		s.uv(uint64(g.tab))
		s.flag(g.rows)
		s.str(g.name)
		s.iv(int64(g.at))
		s.iv(int64(g.n))
		s.uv(g.above)
		s.uv(g.below)
		s.flag(g.aboveGone)
		s.uv(uint64(len(g.runs)))
		for _, run := range g.runs {
			s.uv(run[0])
			s.uv(run[1])
		}
	}
}

func (s *spillEnc) step(st *undoStep) {
	s.uv(uint64(st.kind))
	s.uv(uint64(st.tab))
	s.flag(st.rows)
	s.ids(st.ids)
	s.uv(st.above)
	s.uv(st.below)
	s.flag(st.aboveGone)
	s.iv(int64(st.at))
	s.uv(uint64(len(st.saved)))
	for k, saved := range st.saved {
		s.uv(uint64(saved.id))
		s.iv(int64(saved.pos))
		s.row(saved.rd)
		placed := st.colAt != nil && st.colAt[k] != nil
		s.flag(placed)
		if placed {
			for _, at := range st.colAt[k] {
				s.iv(int64(at))
			}
		}
	}
	s.uv(uint64(len(st.cells)))
	s.flag(st.rowAt != nil)
	for k, saved := range st.cells {
		s.uv(uint64(saved.row))
		s.uv(uint64(saved.col))
		s.flag(saved.c != nil)
		if saved.c != nil {
			s.cell(saved.c)
		}
		if st.rowAt != nil {
			s.iv(int64(st.rowAt[k]))
		}
	}
	s.uv(uint64(len(st.widths)))
	for _, w := range st.widths {
		s.uv(uint64(w.col))
		s.iv(int64(w.w.px))
		s.uv(w.w.seq)
	}
	s.uv(uint64(len(st.repairs)))
	for i := range st.repairs {
		r := &st.repairs[i]
		s.uv(uint64(r.tab))
		s.uv(uint64(r.row))
		s.uv(uint64(r.col))
		s.formula(r.before)
		s.uv(uint64(len(r.tabs)))
		for _, id := range r.tabs {
			s.uv(uint64(id))
		}
	}
	s.uv(uint64(st.col))
	s.iv(int64(st.width.px))
	s.uv(st.width.seq)
	s.flag(st.had)
	s.iv(int64(st.frozen[0]))
	s.iv(int64(st.frozen[1]))
	s.str(st.name)
	s.uv(uint64(len(st.order)))
	for _, id := range st.order {
		s.uv(uint64(id))
	}
	s.uv(st.seq)
	s.uv(st.prev)
	s.uv(uint64(len(st.stamps)))
	for _, stamp := range st.stamps {
		s.uv(stamp)
	}
	s.uv(uint64(len(st.sorted)))
	for _, saved := range st.sorted {
		s.uv(uint64(saved.row))
		s.uv(saved.stamp.seq)
		s.uv(uint64(saved.stamp.by))
	}
	s.flag(st.savedTab != nil)
	if st.savedTab != nil {
		s.tab(st.savedTab)
	}
}

func (s *spillEnc) mark(m writeMark) {
	s.uv(uint64(m.by))
	s.uv(m.last)
	s.uv(m.other)
}

func (s *spillEnc) marks(n int, each func(yield func(uint64, writeMark))) {
	s.uv(uint64(n))
	each(func(id uint64, m writeMark) {
		s.uv(id)
		s.mark(m)
	})
}

func (s *spillEnc) row(rd *rowData) {
	s.uv(uint64(len(rd.cells)))
	for _, slot := range rd.cells {
		s.uv(uint64(slot.col))
		s.cell(slot.c)
	}
}

func (s *spillEnc) axis(a *axis) {
	s.ids(a.ids)
	s.uv(uint64(len(a.born)))
	for _, run := range a.born {
		s.uv(run.first)
		s.uv(run.count)
		s.uv(run.seq)
	}
}

func (s *spillEnc) tab(t *Tab) {
	s.uv(uint64(t.id))
	s.str(t.name)
	s.uv(t.born)
	s.uv(t.nameSeq)
	s.uv(t.settingsSeq)
	s.uv(t.structSeq)
	s.axis(&t.rows)
	s.axis(&t.cols)
	s.uv(uint64(len(t.widths)))
	for col, w := range t.widths {
		s.uv(uint64(col))
		s.iv(int64(w.px))
		s.uv(w.seq)
	}
	s.iv(int64(t.frozenRows))
	s.iv(int64(t.frozenCols))
	s.uv(uint64(len(t.chunks)))
	for _, first := range t.chunks {
		s.uv(uint64(first))
	}
	s.uv(uint64(len(t.lastSort)))
	for row, stamp := range t.lastSort {
		s.uv(uint64(row))
		s.uv(stamp.seq)
		s.uv(uint64(stamp.by))
	}
	s.uv(uint64(len(t.data)))
	for row, rd := range t.data {
		s.uv(uint64(row))
		s.row(rd)
	}
	s.uv(t.recovered)
	s.mark(t.mark)
	for _, marks := range [2]map[uint64]writeMark{t.marks, t.colMarks} {
		s.marks(len(marks), func(yield func(uint64, writeMark)) {
			for id, m := range marks {
				yield(id, m)
			}
		})
	}
}

// spillDec reads what spillEnc wrote. The first failure stays in err and
// every later read returns zero values.
//
// The bytes of a file are checked whole before it is decoded, but a file
// can be intact and still say what cannot be: a count of things that its
// bytes do not hold. Nothing is allocated for a count before it is held
// against the bytes that are left and against what a sheet can have
// (count, ids). Runs of ids, which take a few bytes for any number of
// ids, are held against the number of ids that was written. The memory a
// damaged file costs is therefore in proportion to its size, and to the
// ids the payload held, at most.
type spillDec struct {
	r *bufio.Reader
	// src is what r reads from. It counts down the bytes of the payload
	// that r has not taken yet.
	src *io.LimitedReader
	// idsLeft counts down the ids the runs may still stand for.
	idsLeft  int64
	wb       *Workbook
	err      error
	formulas []*formula.Formula
	formats  []formula.Format
	// styles are the style states of the entry, read before its cells:
	// each style a cell names is one of them.
	styles map[uint32]styleState
	tmp    [8]byte
	// late, when it is not nil, reports that the deadline passed. polled
	// counts the things read since it was last asked, and asked is what
	// left returned then.
	late   func() bool
	polled int
	asked  int64
}

// newSpillDec returns a decoder for the size bytes that r holds next,
// whose runs were written for the given number of ids.
func newSpillDec(r io.Reader, size, ids int64, wb *Workbook) *spillDec {
	src := &io.LimitedReader{R: r, N: size}
	return &spillDec{r: bufio.NewReaderSize(src, int(min(size, 64<<10))), src: src, idsLeft: ids, wb: wb, asked: size}
}

// left returns the bytes of the payload that are not read yet.
func (d *spillDec) left() int64 { return d.src.N + int64(d.r.Buffered()) }

func (d *spillDec) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

// poll counts one thing about to be read, and asks late once spillPoll
// things or bytes were read since it was last asked. Every loop of the
// decoder calls it for each thing, and ends once the decoder fails.
func (d *spillDec) poll() {
	if d.late == nil || d.err != nil {
		return
	}
	if d.polled++; d.polled < spillPoll && d.asked-d.left() < spillPoll {
		return
	}
	d.polled, d.asked = 0, d.left()
	if d.late() {
		d.fail(errSpillLate)
	}
}

func (d *spillDec) uv() uint64 {
	if d.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(d.r)
	if err != nil {
		d.fail(err)
	}
	return v
}

func (d *spillDec) iv() int64 {
	if d.err != nil {
		return 0
	}
	v, err := binary.ReadVarint(d.r)
	if err != nil {
		d.fail(err)
	}
	return v
}

// u32 reads a value that the encoder wrote from 32 bits. A larger one is
// damage, which a cast would turn into another value.
func (d *spillDec) u32() uint32 {
	v := d.uv()
	if v > math.MaxUint32 {
		d.fail(fmt.Errorf("a value of %d where 32 bits were written", v))
		return 0
	}
	return uint32(v)
}

// i32 is u32 for a signed value.
func (d *spillDec) i32() int32 {
	v := d.iv()
	if v < math.MinInt32 || v > math.MaxInt32 {
		d.fail(fmt.Errorf("a value of %d where 32 bits were written", v))
		return 0
	}
	return int32(v)
}

// count reads the number of things that follow. Each of them takes at
// least unit bytes of the payload, and a sheet has at most limit of them
// when limit is not 0. A number that the bytes left do not hold, or one
// past the limit, is damage, and the caller makes room for nothing.
func (d *spillDec) count(unit int64, limit int) int {
	n := d.uv()
	if d.err != nil {
		return 0
	}
	if left := d.left(); n > uint64(left/unit) || (limit > 0 && n > uint64(limit)) {
		d.fail(fmt.Errorf("a count of %d with %d bytes left", n, left))
		return 0
	}
	return int(n)
}

func (d *spillDec) flag() bool {
	if d.err != nil {
		return false
	}
	b, err := d.r.ReadByte()
	if err != nil {
		d.fail(err)
	}
	return b != 0
}

// str reads a text of at most limit bytes, or of any length the payload
// has bytes for when limit is 0.
func (d *spillDec) str(limit int) string {
	n := d.count(1, limit)
	if d.err != nil || n == 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		d.fail(err)
		return ""
	}
	return unsafe.String(unsafe.SliceData(buf), n)
}

// name reads the name of a tab: at most maxTabName code points of up to
// four bytes.
func (d *spillDec) name() string { return d.str(4 * maxTabName) }

// ids reads ids of one axis of a tab, which are written as runs. A run of
// two values stands for any number of ids, so the bytes that are left say
// nothing about their number. An axis has at most formula.MaxRows, and
// the payload as many as were written.
func (d *spillDec) ids() []uint64 {
	n := d.uv()
	if d.err != nil || n == 0 {
		return nil
	}
	if n > formula.MaxRows {
		d.fail(fmt.Errorf("a list of %d ids", n))
		return nil
	}
	var ids []uint64
	for uint64(len(ids)) < n && d.err == nil {
		first, count := d.uv(), d.uv()
		if d.err != nil {
			break
		}
		if count == 0 || count > n-uint64(len(ids)) {
			d.fail(fmt.Errorf("an id run of %d", count))
			break
		}
		if count > uint64(d.idsLeft) {
			d.fail(fmt.Errorf("more ids than were written: a run of %d with %d left", count, d.idsLeft))
			break
		}
		d.idsLeft -= int64(count)
		// Room is made for a run once the run is read.
		ids = slices.Grow(ids, int(count))
		for i := uint64(0); i < count && d.err == nil; i++ {
			d.poll()
			ids = append(ids, first+i)
		}
	}
	return ids
}

func (d *spillDec) formula() *formula.Formula {
	switch n := d.uv(); {
	case d.err != nil || n == 0:
		return nil
	case n > 1:
		if n-2 >= uint64(len(d.formulas)) {
			d.fail(fmt.Errorf("formula %d is not defined", n-2))
			return nil
		}
		return d.formulas[n-2]
	}
	unreadable, text := d.flag(), d.str(0)
	if d.err != nil {
		return nil
	}
	var f *formula.Formula
	var err error
	if unreadable {
		f, err = spilledUnreadable(text)
	} else {
		f, err = formula.ParseRetained(text)
	}
	if err != nil {
		d.fail(err)
		return nil
	}
	f = d.wb.intern(f)
	d.formulas = append(d.formulas, f)
	return f
}

// spilledUnreadable reads the typed text of an unreadable formula of a
// payload, as the loaders read one from storage. Text the lexer can split
// is a readable formula, whose references would be read from another cell
// than its own. Text the lexer cannot split has no references: its text is
// the same at every cell, which the payload's formulas share, so Parse
// holds it to the limits of its text at any cell.
func spilledUnreadable(typed string) (*formula.Formula, error) {
	if len(typed) < 2 || typed[0] != '=' {
		return nil, fmt.Errorf("an unreadable formula without its \"=\"")
	}
	f, err := formula.Parse(typed, formula.Pos{}, formula.Limits{})
	if err == nil && !f.Unreadable() {
		err = fmt.Errorf("an unreadable formula that can be read")
	}
	return f, err
}

func (d *spillDec) content() (formula.Content, *formula.Formula) {
	if f := d.formula(); f != nil {
		return formulaContent(f), f
	}
	kind := d.uv()
	c := formula.Content{Kind: formula.ContentKind(kind)}
	d.tmp = [8]byte{}
	if d.err == nil {
		if _, err := io.ReadFull(d.r, d.tmp[:]); err != nil {
			d.fail(err)
		}
	}
	c.Num = math.Float64frombits(binary.LittleEndian.Uint64(d.tmp[:]))
	c.Text = d.str(0)
	if d.err == nil {
		if kind >= uint64(formula.ContentFormula) {
			d.fail(fmt.Errorf("content of kind %d", kind))
		} else if err := savedContent(c); err != nil {
			d.fail(err)
		}
	}
	return c, nil
}

// savedContent checks content without a formula as storedCell checks it
// in sheet.db: text only in a text, within the text limit, and in a
// number, a date, a time or a boolean a number that sheet.db and a row
// file hold as that same value (durableNumber). An undo puts the content
// back into the sheet, whose next load would refuse anything else.
func savedContent(c formula.Content) error {
	switch c.Kind {
	case formula.ContentBlank:
		if c.Num != 0 || c.Text != "" {
			return fmt.Errorf("a blank with content")
		}
	case formula.ContentText:
		if c.Num != 0 {
			return fmt.Errorf("a text with the number %v", c.Num)
		}
		return storedText(c.Text)
	default:
		n := c.Num
		if c.Text != "" || math.IsNaN(n) || math.IsInf(n, 0) || (c.Kind == formula.ContentBool && n != 0 && n != 1) || !durableNumber(c.Kind, n) {
			return fmt.Errorf("content of kind %d with the number %v", c.Kind, n)
		}
	}
	return nil
}

func (d *spillDec) cell() *Cell {
	c := &Cell{}
	c.content, c.f = d.content()
	if c.f == nil {
		c.val = c.content.Value()
	}
	c.style, c.by, c.seq, c.textSeq = d.u32(), d.u32(), d.uv(), d.uv()
	if _, ok := d.styles[c.style]; c.style != 0 && !ok && d.err == nil {
		d.fail(fmt.Errorf("a saved cell names style %d, which the payload does not hold", c.style))
	}
	return c
}

func (d *spillDec) format() formula.Format {
	if n := d.uv(); n > 0 {
		if n > uint64(len(d.formats)) {
			d.fail(fmt.Errorf("format %d is not defined", n-1))
			return formula.Format{}
		}
		return d.formats[n-1]
	}
	text := d.str(0)
	if d.err != nil {
		return formula.Format{}
	}
	patch, err := decodeFormatPatch([]byte(text), true)
	if err != nil {
		d.fail(err)
	}
	d.formats = append(d.formats, patch.f)
	return patch.f
}

func (d *spillDec) stamps() (v [propCount]uint64) {
	for i := range v {
		v[i] = d.uv()
	}
	return v
}

func (d *spillDec) entry() *undoEntry {
	e := &undoEntry{}
	magic := make([]byte, len(spillMagic))
	if _, err := io.ReadFull(d.r, magic); err != nil || string(magic) != spillMagic {
		d.fail(fmt.Errorf("not an undo payload"))
		return e
	}
	// A style is its id, its format and nine stamps. An id comes once, or
	// the cells that name it would get one of two states.
	if n := d.count(11, 0); n > 0 {
		e.styles = make(map[uint32]styleState, n)
		for i := 0; i < n && d.err == nil; i++ {
			d.poll()
			id := d.u32()
			if id == 0 && d.err == nil {
				d.fail(fmt.Errorf("a payload holds a state of style 0"))
			}
			putOnce(d, e.styles, id, styleState{f: d.format(), stamps: d.stamps()})
		}
	}
	d.styles = e.styles
	// An entry holds a cell once, and an action changes no more cells than
	// its journal has entries.
	if n := d.count(spillCellBytes, journalMax/journalEntryBytes); n > 0 {
		e.cells = make([]cellRestore, 0, n)
		seen := make(map[cellID]struct{}, n)
		for i := 0; i < n && d.err == nil; i++ {
			d.poll()
			r := cellRestore{tab: TabID(d.uv()), row: RowID(d.uv()), col: ColID(d.uv()), content: d.flag()}
			r.before, r.f = d.content()
			r.pos = formula.Pos{Row: d.i32(), Col: d.i32()}
			r.beforeSeq, r.beforeBy = d.uv(), d.u32()
			props := d.uv()
			r.props = uint16(props)
			if props >= 1<<propCount {
				d.fail(fmt.Errorf("a cell with the properties %#x", props))
			}
			if r.props != 0 {
				r.format, r.beforeStamps = d.format(), d.stamps()
			}
			putOnce(d, seen, cellID{r.tab, r.row, r.col}, struct{}{})
			e.cells = append(e.cells, r)
		}
	}
	if n := d.count(spillStepBytes, 0); n > 0 {
		e.steps = make([]undoStep, 0, n)
		for i := 0; i < n && d.err == nil; i++ {
			d.poll()
			e.steps = append(e.steps, d.step())
		}
	}
	// A deletion kept apart is nine values, and a run of its ids two. Its
	// runs are the ids it deleted, as many as it says, and its tab had a
	// name.
	for i, n := 0, d.count(9, gapsKept); i < n && d.err == nil; i++ {
		d.poll()
		g := undoGap{tab: TabID(d.uv()), rows: d.flag(), name: d.name(), at: d.i32(), n: d.i32(), above: d.uv(), below: d.uv(), aboveGone: d.flag()}
		runs := d.count(2, formula.MaxRows)
		g.runs = make([][2]uint64, 0, runs)
		var ids uint64
		for k := 0; k < runs && d.err == nil; k++ {
			d.poll()
			run := [2]uint64{d.uv(), d.uv()}
			if run[0] == 0 || run[0] >= maxID || run[1] == 0 || run[1] > formula.MaxRows || run[1] > maxID-run[0] {
				d.fail(fmt.Errorf("a deletion kept apart with the run %v", run))
			}
			ids += run[1]
			g.runs = append(g.runs, run)
		}
		if d.err == nil && (g.at < 0 || g.n <= 0 || ids != uint64(g.n) || !validTabName(g.name)) {
			d.fail(fmt.Errorf("a deletion kept apart at %d of %d ids, with %d in its runs", g.at, g.n, ids))
		}
		e.gaps = append(e.gaps, g)
	}
	return e
}

func (d *spillDec) step() undoStep {
	st := undoStep{kind: int(d.uv()), tab: TabID(d.uv()), rows: d.flag(), ids: d.ids()}
	st.above, st.below, st.aboveGone, st.at = d.uv(), d.uv(), d.flag(), int(d.iv())
	// The lists of a step are of one axis of one tab, of its formulas or of
	// the tabs of the sheet. A saved row is its id, its place, the number
	// of its cells and a flag.
	for i, n := 0, d.count(4, formula.MaxRows); i < n && d.err == nil; i++ {
		d.poll()
		st.saved = append(st.saved, savedRow{id: RowID(d.uv()), pos: d.i32(), rd: d.row()})
		if !d.flag() {
			if st.colAt != nil {
				st.colAt = append(st.colAt, nil)
			}
			continue
		}
		if st.colAt == nil {
			st.colAt = make([][]int32, i)
		}
		at := make([]int32, len(st.saved[i].rd.cells))
		for c := 0; c < len(at) && d.err == nil; c++ {
			d.poll()
			at[c] = d.i32()
		}
		st.colAt = append(st.colAt, at)
	}
	n, placed := d.count(3, 0), d.flag()
	for i := 0; i < n && d.err == nil; i++ {
		d.poll()
		saved := savedCell{row: RowID(d.uv()), col: ColID(d.uv())}
		if d.flag() {
			saved.c = d.cell()
		}
		st.cells = append(st.cells, saved)
		if placed {
			st.rowAt = append(st.rowAt, d.i32())
		}
	}
	for i, n := 0, d.count(3, formula.MaxCols); i < n && d.err == nil; i++ {
		d.poll()
		st.widths = append(st.widths, savedWidth{ColID(d.uv()), width{int(d.iv()), d.uv()}})
	}
	for i, n := 0, d.count(5, maxFormulas); i < n && d.err == nil; i++ {
		d.poll()
		r := repair{tab: TabID(d.uv()), row: RowID(d.uv()), col: ColID(d.uv()), before: d.formula()}
		for k, tabs := 0, d.count(1, maxTabs); k < tabs && d.err == nil; k++ {
			d.poll()
			r.tabs = append(r.tabs, TabID(d.uv()))
		}
		if r.before == nil {
			d.fail(fmt.Errorf("a repair without its formula"))
		}
		st.repairs = append(st.repairs, r)
	}
	st.col, st.width = ColID(d.uv()), width{int(d.iv()), d.uv()}
	st.had = d.flag()
	st.frozen = [2]int{int(d.iv()), int(d.iv())}
	st.name = d.name()
	for i, n := 0, d.count(1, maxTabs); i < n && d.err == nil; i++ {
		d.poll()
		st.order = append(st.order, TabID(d.uv()))
	}
	st.seq, st.prev = d.uv(), d.uv()
	for i, n := 0, d.count(1, formula.MaxRows); i < n && d.err == nil; i++ {
		d.poll()
		st.stamps = append(st.stamps, d.uv())
	}
	for i, n := 0, d.count(3, formula.MaxRows); i < n && d.err == nil; i++ {
		d.poll()
		st.sorted = append(st.sorted, savedSort{RowID(d.uv()), sortStamp{d.uv(), d.u32()}})
	}
	if d.flag() {
		st.savedTab = d.tab()
	}
	d.checkStep(&st)
	return st
}

// The decoder checks the fields of a step and of a saved tab against each
// other once they are read: an intact file can hold counts that fit and
// still say what no action leaves, and an undo that consumed such a step
// would index past its lists or put a row or an id where it does not
// belong. A step that fails a check makes its entry lost (technical
// design, Appendix B).

// idSet returns ids as a set. It fails the decoding when one of them is
// not an id or is there twice.
func (d *spillDec) idSet(ids []uint64, what string) map[uint64]struct{} {
	set := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if d.poll(); d.err != nil {
			return nil
		}
		if _, twice := set[id]; twice || id == 0 || id >= maxID {
			d.fail(fmt.Errorf("%s holds id %d twice or past the ids", what, id))
			return nil
		}
		set[id] = struct{}{}
	}
	return set
}

// checkRow checks that the cells of a row are in column order, each column
// once, and in cols when it is not nil.
func (d *spillDec) checkRow(rd *rowData, cols map[uint64]struct{}) {
	for i, slot := range rd.cells {
		if d.poll(); d.err != nil {
			return
		}
		_, live := cols[uint64(slot.col)]
		if (i > 0 && rd.cells[i-1].col >= slot.col) || (cols != nil && !live) {
			d.fail(fmt.Errorf("a saved row holds column %d out of order or outside its tab", slot.col))
			return
		}
	}
}

func (d *spillDec) checkStep(st *undoStep) {
	if d.err != nil {
		return
	}
	switch {
	case st.kind < stepInserted || st.kind > stepTabDeleted:
		d.fail(fmt.Errorf("a step of kind %d", st.kind))
	case (st.kind == stepTabDeleted) != (st.savedTab != nil) || (st.savedTab != nil && st.savedTab.id != st.tab):
		d.fail(fmt.Errorf("a step of kind %d of tab %d with a saved tab that does not belong to it", st.kind, st.tab))
	case st.at < 0 || st.frozen[0] < 0 || st.frozen[1] < 0 || st.frozen[0] > maxFrozenRows || st.frozen[1] > maxFrozenCols:
		d.fail(fmt.Errorf("a step at %d with frozen %v", st.at, st.frozen))
	case st.kind == stepSorted && len(st.stamps) != len(st.ids):
		d.fail(fmt.Errorf("a sort of %d rows with %d stamps", len(st.ids), len(st.stamps)))
	case (st.kind == stepInserted || st.kind == stepDeleted || st.kind == stepSorted) && len(st.ids) == 0:
		// An insert, a deletion and a sort name the rows or columns they
		// changed, at least one. The undo of a deletion puts its ids back
		// and starts from the first.
		d.fail(fmt.Errorf("a step of kind %d of no ids", st.kind))
	case st.kind == stepTabName && !validTabName(st.name):
		d.fail(fmt.Errorf("a rename back to a name no tab can have"))
	case st.kind == stepWidth && st.had && !validWidth(st.width.px):
		d.fail(fmt.Errorf("a width of %d", st.width.px))
	}
	ids := d.idSet(st.ids, "a step")
	tabs := make([]uint64, len(st.order))
	for i, id := range st.order {
		tabs[i] = uint64(id)
	}
	d.idSet(tabs, "a tab order")
	// A step saves a cell, a width, the last sort of a row and the formula
	// of a cell once. Otherwise the undo would put two records back in one
	// place and keep the last.
	cells := make(map[[2]uint64]struct{}, len(st.cells))
	for _, saved := range st.cells {
		d.poll()
		putOnce(d, cells, [2]uint64{uint64(saved.row), uint64(saved.col)}, struct{}{})
	}
	widths := make(map[ColID]struct{}, len(st.widths))
	for _, w := range st.widths {
		d.poll()
		putOnce(d, widths, w.col, struct{}{})
	}
	sorted := make(map[RowID]struct{}, len(st.sorted))
	for _, saved := range st.sorted {
		d.poll()
		putOnce(d, sorted, saved.row, struct{}{})
	}
	repaired := make(map[cellID]struct{}, len(st.repairs))
	for _, r := range st.repairs {
		d.poll()
		putOnce(d, repaired, cellID{r.tab, r.row, r.col}, struct{}{})
	}
	if d.err != nil || st.kind != stepDeleted {
		return
	}
	// What a deletion saved lies in what it deleted: rows in a deletion
	// of rows, cells, columns and widths in one of columns.
	in := func(id uint64, rows bool) bool {
		_, ok := ids[id]
		return ok && rows == st.rows
	}
	seen := make(map[RowID]struct{}, len(st.saved))
	for k, saved := range st.saved {
		if d.poll(); d.err != nil {
			return
		}
		// A saved row lies at its place among the deleted ones, which is
		// where following puts its formulas, and the undo puts it back by
		// its id: the id at that place is its own.
		if _, twice := seen[saved.id]; twice || !in(uint64(saved.id), true) || saved.pos < 0 || int(saved.pos) >= len(st.ids) ||
			st.ids[saved.pos] != uint64(saved.id) {
			d.fail(fmt.Errorf("a deletion saved row %d twice, at %d, or that it did not delete there", saved.id, saved.pos))
			return
		}
		seen[saved.id] = struct{}{}
		d.checkRow(saved.rd, nil)
		if st.colAt != nil {
			d.checkPlaces(st.colAt[k])
		}
	}
	for _, saved := range st.cells {
		if d.poll(); d.err == nil && !in(uint64(saved.col), false) {
			d.fail(fmt.Errorf("a deletion saved a cell of column %d that it did not delete", saved.col))
		}
	}
	d.checkPlaces(st.rowAt)
	for _, w := range st.widths {
		if d.poll(); d.err == nil && (!in(uint64(w.col), false) || !validWidth(w.w.px)) {
			d.fail(fmt.Errorf("a deletion saved a width of %d of column %d that it did not delete", w.w.px, w.col))
		}
	}
	for _, saved := range st.sorted {
		if d.poll(); d.err == nil && !in(uint64(saved.row), true) {
			d.fail(fmt.Errorf("a deletion saved the last sort of row %d that it did not delete", saved.row))
		}
	}
}

// checkPlaces checks the places on the other axis that following keeps
// for saved formulas: a position, or gonePos for a formula whose cell a
// later deletion took.
func (d *spillDec) checkPlaces(at []int32) {
	for _, p := range at {
		if d.poll(); d.err == nil && p < gonePos {
			d.fail(fmt.Errorf("a saved formula at %d", p))
		}
	}
}

// validWidth reports whether a column can have the width (spec rule 7).
func validWidth(px int) bool { return px >= minWidth && px <= maxWidth }

// checkAxis checks that an axis of a saved tab holds ids, each once, and
// that its born runs are in order, apart, and stamp exactly its ids.
func (d *spillDec) checkAxis(a *axis) map[uint64]struct{} {
	ids := d.idSet(a.ids, "a saved axis")
	if d.err == nil && len(a.ids) == 0 {
		d.fail(fmt.Errorf("a saved axis without ids"))
	}
	var stamped uint64
	for i, run := range a.born {
		if d.poll(); d.err != nil {
			return nil
		}
		if run.count == 0 || run.seq == 0 || run.first == 0 || run.first >= maxID || run.count > maxID-run.first ||
			(i > 0 && a.born[i-1].first+a.born[i-1].count > run.first) {
			d.fail(fmt.Errorf("a saved axis with a born run %+v out of order", run))
			return nil
		}
		stamped += run.count
	}
	if d.err == nil && stamped != uint64(len(a.ids)) {
		d.fail(fmt.Errorf("a saved axis of %d ids with %d born stamps", len(a.ids), stamped))
	}
	for _, id := range a.ids {
		if d.poll(); d.err == nil && a.born.get(id) == 0 {
			d.fail(fmt.Errorf("a saved axis holds id %d without a born stamp", id))
		}
	}
	return ids
}

func (d *spillDec) checkTab(t *Tab) {
	if d.err != nil {
		return
	}
	rows, cols := d.checkAxis(&t.rows), d.checkAxis(&t.cols)
	switch {
	case d.err != nil:
	case t.frozenRows < 0 || t.frozenCols < 0 || t.frozenRows > maxFrozenRows || t.frozenCols > maxFrozenCols:
		d.fail(fmt.Errorf("a saved tab with frozen %d and %d", t.frozenRows, t.frozenCols))
	case !validTabName(t.name):
		d.fail(fmt.Errorf("a saved tab with a name no tab can have"))
	case len(t.chunks) == 0 || t.chunks[0] != 0:
		// The files of a tab's rows start with one at row 0 and hold every
		// row from there (sheet.db, rebalance).
		d.fail(fmt.Errorf("a saved tab whose row files do not start at its first row"))
	}
	for i := 1; i < len(t.chunks) && d.err == nil; i++ {
		if d.poll(); t.chunks[i] <= t.chunks[i-1] {
			d.fail(fmt.Errorf("a saved tab whose row files are out of order"))
		}
	}
	for row, rd := range t.data {
		if d.err != nil {
			return
		}
		if _, live := rows[uint64(row)]; !live {
			d.fail(fmt.Errorf("a saved tab holds the cells of row %d outside it", row))
			return
		}
		d.checkRow(rd, cols)
	}
	for col, w := range t.widths {
		if d.poll(); d.err == nil {
			if _, live := cols[uint64(col)]; !live || !validWidth(w.px) {
				d.fail(fmt.Errorf("a saved tab holds a width of %d of column %d outside it", w.px, col))
			}
		}
	}
}

// putOnce puts a decoded entry into a map. A key that comes twice fails
// the decoding: a saved tab's map was made for the count the file gives
// and is counted at the entries it holds, and a list of saved records
// holds each of them once.
func putOnce[K comparable, V any](d *spillDec, m map[K]V, k K, v V) {
	if d.err != nil {
		return
	}
	if _, twice := m[k]; twice {
		d.fail(fmt.Errorf("a payload holds %v twice", k))
		return
	}
	m[k] = v
}

func (d *spillDec) mark() writeMark {
	return writeMark{by: d.u32(), last: d.uv(), other: d.uv()}
}

// row reads the cells of a row: at most one for each column a tab can
// have, each its column and a cell of five values or more.
func (d *spillDec) row() *rowData {
	n := d.count(6, formula.MaxCols)
	rd := &rowData{cells: make([]cellSlot, 0, n)}
	for i := 0; i < n && d.err == nil; i++ {
		d.poll()
		slot := cellSlot{col: ColID(d.uv()), c: d.cell()}
		if slot.c.f != nil {
			rd.nf++
		}
		rd.cells = append(rd.cells, slot)
	}
	return rd
}

func (d *spillDec) axis() axis {
	a := axis{ids: d.ids()}
	a.pos = make(map[uint64]int32, len(a.ids))
	a.reindex(0)
	for i, n := 0, d.count(3, formula.MaxRows); i < n && d.err == nil; i++ {
		d.poll()
		a.born = append(a.born, stampRun{d.uv(), d.uv(), d.uv()})
	}
	return a
}

func (d *spillDec) tab() *Tab {
	t := &Tab{id: TabID(d.uv()), name: d.name(), born: d.uv(), nameSeq: d.uv(), settingsSeq: d.uv(), structSeq: d.uv(),
		rows: d.axis(), cols: d.axis(), widths: map[ColID]width{}, data: map[RowID]*rowData{}}
	// What a tab keeps for its rows and columns is at most one for each
	// of them.
	for i, n := 0, d.count(3, formula.MaxCols); i < n && d.err == nil; i++ {
		d.poll()
		putOnce(d, t.widths, ColID(d.uv()), width{int(d.iv()), d.uv()})
	}
	t.frozenRows, t.frozenCols = int(d.iv()), int(d.iv())
	for i, n := 0, d.count(1, maxTabChunks); i < n && d.err == nil; i++ {
		d.poll()
		t.chunks = append(t.chunks, RowID(d.uv()))
	}
	if n := d.count(3, 0); n > 0 {
		t.lastSort = make(map[RowID]sortStamp, n)
		for i := 0; i < n && d.err == nil; i++ {
			d.poll()
			putOnce(d, t.lastSort, RowID(d.uv()), sortStamp{d.uv(), d.u32()})
		}
	}
	for i, n := 0, d.count(2, formula.MaxRows); i < n && d.err == nil; i++ {
		d.poll()
		putOnce(d, t.data, RowID(d.uv()), d.row())
	}
	t.recovered = d.uv()
	t.mark = d.mark()
	for _, marks := range [2]*map[uint64]writeMark{&t.marks, &t.colMarks} {
		if n := d.count(4, 0); n > 0 {
			*marks = make(map[uint64]writeMark, n)
			for i := 0; i < n && d.err == nil; i++ {
				d.poll()
				putOnce(d, *marks, d.uv(), d.mark())
			}
		}
	}
	d.checkTab(t)
	return t
}
