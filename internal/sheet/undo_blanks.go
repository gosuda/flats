package sheet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/sqliteuri"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

type cellID struct {
	tab TabID
	row RowID
	col ColID
}

// collectedBlank is a blank record the maintenance tick removed.
type collectedBlank struct {
	seq, textSeq uint64
	by           uint32
	stamps       *[propCount]uint64 // nil for a record without a style
}

// newest returns the newest stamp of the record's groups.
func (b *collectedBlank) newest() uint64 {
	at := b.seq
	if b.stamps != nil {
		for _, stamp := range b.stamps {
			at = max(at, stamp)
		}
	}
	return at
}

// blankLog is a workbook's hold on the collected blank records. store is
// nil until a tick collects a record of this workbook of which an undo
// entry holds a stamp.
type blankLog struct {
	store *blankStore
	// limit replaces maxBlankRecords when it is not zero, for tests.
	limit int
}

// blankLimit is the number of blank records without a format above which
// the tick collects the older half (technical design, section 7.5).
func (wb *Workbook) blankLimit() int {
	if wb.collected.limit != 0 {
		return wb.collected.limit
	}
	return maxBlankRecords
}

// blankStore keeps the collected blank records with their exact stamps
// (technical design, section 7.5). A collected record takes its stamps out
// of the sheet, and an undo guard needs them: without the stamp it cannot
// tell a cell that nobody touched since the session cleared it from one
// that another writer cleared later.
//
// A record is kept only for the undo entries that hold one of its stamps:
// the entry whose action wrote the stamp, and the entries that stand-in
// notes name for it (undo_notes.go). No other entry can read the record,
// so what writers without such an entry clear and collect costs no
// storage. The table has one row for each entry that holds a stamp of a
// record, under the entry's sequence. A row counts while its entry is on a
// stack, and the tick deletes the rows of the entries that have left, so
// the record stays for as long as one of its holders does. No entry comes
// to hold a stamp of a collected record later, because stamps and notes
// are made for groups that have a record. A cell that gets a record again
// takes the stamps of its row, and the row goes.
//
// The records live in a SQLite file in the instance's scratch directory,
// outside the sheet's quota and gone with the instance, like the undo
// stacks they serve. The file has a fixed largest size. A record that does
// not fit is not kept, and the entries that held its stamps skip the cell
// as lost (spec rule 23).
type blankStore struct {
	pool *undoScratch
	in   *Instance
	db   *sql.DB
	path string
	// owner is the workbook the rows belong to. A rollback installs
	// another one, whose cells the rows say nothing about.
	owner *Workbook
	rows  int64 // rows in the table
	// filter tells most cells without a row from the others, so that a
	// write into an empty cell does not read the file.
	filter blankFilter
	// maxBytes replaces blankStoreBytes when it is not zero, for tests.
	maxBytes int64
	// vfs names the SQLite VFS the file is opened with, for tests.
	vfs string
	// lost is set when the file is gone. What it knew cannot be asked any
	// more.
	lost bool
	// fails counts the errors of the file in a row.
	fails int
	// recheck says that rows, columns or tabs left the sheet, or the
	// entries that could bring them back, since the rows were last
	// compared with the sheet. cursor is where that comparison stands,
	// and again says that more left since it began, so that another one
	// follows it.
	recheck, again bool
	cursor         blankCursor
}

// blankCursor is the place of the pass that drops the rows of what is gone
// from the sheet for good: the tab, whether its columns are looked at, and
// the last id seen.
type blankCursor struct {
	started, open bool
	tab           int64
	cols          bool
	id            int64
}

const (
	// blankStoreBytes is the largest size of the file of collected blank
	// records. A row takes about 60 to 110 bytes with its indexes, the
	// more with format stamps, so the file holds about 300,000 to 560,000
	// rows.
	blankStoreBytes = 32 << 20
	blankPage       = 4096
	// blankChunk is how many records one transaction of the store writes,
	// and blankReserve the pages the tick leaves free below the largest
	// size. The file is measured after each record it stores, so they have
	// to hold the rows of one record: at most 20, a few pages.
	blankChunk   = 512
	blankReserve = 64
	// blankFails is how many errors in a row give the file up.
	blankFails = 8
	// blankRecheckWork is how many tabs, rows and columns one tick compares
	// with the sheet.
	blankRecheckWork = 4096
)

const blankSchema = `CREATE TABLE IF NOT EXISTS blanks(
	tab INTEGER NOT NULL, row INTEGER NOT NULL, col INTEGER NOT NULL, holder INTEGER NOT NULL,
	seq INTEGER NOT NULL, text_seq INTEGER NOT NULL, by INTEGER NOT NULL, stamps BLOB,
	PRIMARY KEY(tab, row, col, holder)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS blanks_holder ON blanks(holder);
CREATE INDEX IF NOT EXISTS blanks_col ON blanks(tab, col);`

// blankFile is the name of the store's file in the scratch directory. The
// sweep of orphaned scratch files leaves it and its companions alone.
const blankFile = "blanks.db"

func isBlankFile(name string) bool { return strings.HasPrefix(name, blankFile) }

// blankFilter is a Bloom filter over the cells that have a row. It never
// says no for a cell that has one.
type blankFilter struct {
	bits []uint64
	adds int64
}

func (f *blankFilter) probes(id cellID) (h1, h2 uint64) {
	h := uint64(id.tab)*0x9e3779b97f4a7c15 ^ uint64(id.row)*0xbf58476d1ce4e5b9 ^ uint64(id.col)*0x94d049bb133111eb
	h ^= h >> 31
	h *= 0xd6e8feb86659fd93
	h ^= h >> 32
	return h, h>>17 | 1
}

func (f *blankFilter) add(id cellID) {
	h1, h2 := f.probes(id)
	n := uint64(len(f.bits)) * 64
	for i := uint64(0); i < 4; i++ {
		bit := (h1 + i*h2) % n
		f.bits[bit/64] |= 1 << (bit % 64)
	}
	f.adds++
}

func (f *blankFilter) may(id cellID) bool {
	if len(f.bits) == 0 {
		return false
	}
	h1, h2 := f.probes(id)
	n := uint64(len(f.bits)) * 64
	for i := uint64(0); i < 4; i++ {
		if bit := (h1 + i*h2) % n; f.bits[bit/64]&(1<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}

func (b *blankStore) limit() int64 {
	if b.maxBytes != 0 {
		return b.maxBytes
	}
	return blankStoreBytes
}

func (b *blankStore) open() error {
	if b.db != nil {
		return nil
	}
	p := b.pool
	if err := p.makeDir(); err != nil {
		return err
	}
	path := filepath.Join(p.dir, blankFile)
	// The rows start empty: a file left from before, or one that could
	// not be removed when the store was given up, holds rows nobody
	// counts.
	if err := removeBlankFile(path); err != nil {
		return err
	}
	// The file is scratch: it need not survive a crash, and one writer
	// uses it. It gives the pages of deleted rows back at every commit,
	// and SQLite refuses to grow it past its largest size. The rollback
	// journal is a file beside it, so a large transaction takes no memory.
	dsn := sqliteuri.File(path) + "?_pragma=auto_vacuum(FULL)&_pragma=journal_mode(TRUNCATE)&_pragma=synchronous(OFF)" +
		"&_pragma=max_page_count(" + strconv.FormatInt(b.limit()/blankPage, 10) + ")"
	if b.vfs != "" {
		dsn += "&vfs=" + b.vfs
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(blankSchema); err != nil {
		db.Close()
		removeBlankFile(path)
		return err
	}
	b.db, b.path, b.lost, b.rows, b.fails = db, path, false, 0, 0
	// 16 bits for each row the file can hold.
	b.filter = blankFilter{bits: make([]uint64, max(64, b.limit()/64*16/64))}
	return nil
}

func (b *blankStore) close() {
	if b.db != nil {
		b.db.Close()
		b.db = nil
	}
	b.rows, b.owner, b.filter = 0, nil, blankFilter{}
}

// removeBlankFile removes the store's file and its journal, if they are
// there.
func removeBlankFile(path string) error {
	for _, suffix := range []string{"", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// noRoom reports whether an error of the store means that there is no
// room: in the file, at its largest size, or on the scratch file system,
// for the file or for its rollback journal. SQLite reports each of them
// as SQLITE_FULL.
func noRoom(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_FULL {
		return true
	}
	return errors.Is(err, syscall.ENOSPC)
}

// failed looks at an error of the store. A file that is gone is lost for
// good, and so is one that fails blankFails times in a row; any other
// error may pass. No room is no error of the file: SQLite takes back the
// statement or the transaction that found none, so the rows stored before
// it stay, and it neither counts toward blankFails nor gives the file up.
func (b *blankStore) failed(err error) error {
	if !noRoom(err) {
		b.fails++
	}
	_, serr := os.Stat(b.path)
	if gone := errors.Is(serr, fs.ErrNotExist); gone || b.fails >= blankFails {
		b.in.scratchFailed("blank records lost", err)
		b.db.Close()
		if !gone {
			removeBlankFile(b.path)
		}
		b.db, b.lost, b.rows, b.filter = nil, true, 0, blankFilter{}
		// Every entry may have had a stamp in the file.
		for _, e := range b.in.holders().entries {
			e.blanksLost = true
		}
		return err
	}
	b.in.scratchFailed("blank records", err)
	return err
}

func (b *blankStore) fault(op string) error {
	if b.pool.fault != nil {
		return b.pool.fault(op)
	}
	return nil
}

// bytes returns the size of the file.
func (b *blankStore) bytes() (int64, error) {
	var pages int64
	err := b.db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	return pages * blankPage, err
}

func encodeBlankStamps(stamps *[propCount]uint64) []byte {
	if stamps == nil {
		return nil
	}
	buf := make([]byte, 0, propCount*binary.MaxVarintLen64)
	for _, stamp := range stamps {
		buf = binary.AppendUvarint(buf, stamp)
	}
	return buf
}

func decodeBlankStamps(buf []byte) (*[propCount]uint64, error) {
	if buf == nil {
		return nil, nil
	}
	var stamps [propCount]uint64
	for i := range stamps {
		v, n := binary.Uvarint(buf)
		if n <= 0 {
			return nil, errSpillDamaged
		}
		stamps[i], buf = v, buf[n:]
	}
	return &stamps, nil
}

// keptIDs names rows or columns of a tab, or a whole tab, that an undo
// entry holds and can bring back: the ids from lo to hi. It is a bound,
// not a list, so an entry takes a few of them whatever it deleted.
type keptIDs struct {
	tab         TabID
	rows, whole bool
	lo, hi      uint64
}

// keptBy returns what the steps of an entry can bring back.
func keptBy(steps []undoStep) []keptIDs {
	var out []keptIDs
	for i := range steps {
		s := &steps[i]
		switch s.kind {
		case stepTabDeleted:
			out = append(out, keptIDs{tab: s.savedTab.id, whole: true})
		case stepDeleted:
			if len(s.ids) == 0 {
				continue
			}
			k := slices.IndexFunc(out, func(k keptIDs) bool { return k.tab == s.tab && k.rows == s.rows && !k.whole })
			if k < 0 {
				out, k = append(out, keptIDs{tab: s.tab, rows: s.rows, lo: s.ids[0], hi: s.ids[0]}), len(out)
			}
			out[k].lo, out[k].hi = min(out[k].lo, slices.Min(s.ids)), max(out[k].hi, slices.Max(s.ids))
		}
	}
	return out
}

// stampHolders is what the undo stacks of every session say about stamps
// at one moment: the entries that can still be undone, by sequence, the
// sessions with stand-in notes, and what the entries can bring back.
type stampHolders struct {
	entries map[uint64]*undoEntry
	noted   []*undoState
	keeps   []keptIDs
}

func (in *Instance) holders() *stampHolders {
	h := &stampHolders{entries: map[uint64]*undoEntry{}}
	in.smu.Lock()
	defer in.smu.Unlock()
	for _, s := range in.sessions {
		// What an action that waits for its commit takes off the stacks is
		// gone already for the actions behind it in the transaction.
		undo, redo := s.undo.undo, s.undo.redo
		undo = undo[min(len(undo), max(0, len(undo)+s.undo.pushing-maxUndoEntries)):]
		if s.undo.clearing {
			redo = nil
		}
		for _, stack := range [2][]*undoEntry{undo, redo} {
			for _, e := range stack {
				if !e.lost {
					h.entries[e.seq] = e
					h.keeps = append(h.keeps, e.keeps...)
				}
			}
		}
		if len(s.undo.notes) > 0 {
			h.noted = append(h.noted, &s.undo)
		}
	}
	return h
}

// holders calls fn for every entry that holds stamp of group g: the entry
// whose action wrote it, and the entries it stands in for. A stamp that an
// undo or a redo wrote can have both, one on each stack, and either can
// leave its stack while the other stays.
func (h *stampHolders) holders(g groupKey, stamp uint64, fn func(e *undoEntry)) {
	if stamp == 0 {
		return
	}
	if e := h.entries[stamp]; e != nil {
		fn(e)
	}
	for _, u := range h.noted {
		for _, s := range u.notes[stamp] {
			if !s.has(g) {
				continue
			}
			if e := h.entries[s.canon]; e != nil {
				fn(e)
			}
		}
	}
}

// each calls fn for every entry that holds a stamp of a record. An entry
// that holds several of them comes once for each.
func (h *stampHolders) each(id cellID, rec *collectedBlank, fn func(e *undoEntry)) {
	if len(h.entries) == 0 {
		return
	}
	g := groupKey{id.tab, id.row, id.col, groupContent}
	h.holders(g, rec.seq, fn)
	for prop := 0; prop < propCount && rec.stamps != nil; prop++ {
		g.prop = int8(prop)
		h.holders(g, rec.stamps[prop], fn)
	}
}

// brings reports whether an entry can bring back the row or column id of a
// tab, or the tab itself when id is 0.
func (h *stampHolders) brings(tab TabID, rows bool, id uint64) bool {
	for _, k := range h.keeps {
		if k.tab == tab && (k.whole || id != 0 && k.rows == rows && k.lo <= id && id <= k.hi) {
			return true
		}
	}
	return false
}

// keptBlank is a record the tick collects and the entries that hold one of
// its stamps, by sequence. The record has one row in the store for each.
type keptBlank struct {
	id      cellID
	rec     collectedBlank
	holders []uint64
}

// keepBlanks stores the blank records the tick is about to collect, before
// it removes them from the sheet: those of which an undo entry holds a
// stamp. It returns the records the tick collects. An error leaves every
// record in the sheet for a later tick: the tick never drops a stamp an
// undo may ask for while the file can take it. A record there is no room
// for is collected without its rows, and the entries that held it skip
// its cell as lost; that is no error. A record whose cell may still have
// rows from an earlier collection is collected only after they are
// deleted: when there is no room for even that, it stays in the sheet for
// a later tick, since those rows would otherwise be read for the cell.
func (in *Instance) keepBlanks(ctx context.Context, refs []cellRef) ([]cellRef, error) {
	wb := in.wb
	b := &in.undoPool().blanks
	b.pool, b.in = &in.scratch, in
	h := in.holders()
	if b.owner != nil && b.owner != wb {
		b.clear()
	}
	if len(h.entries) == 0 && b.rows == 0 {
		return refs, nil
	}
	var rows []keptBlank
	for _, ref := range refs {
		c := ref.tab.cell(ref.row, ref.col)
		if c == nil {
			continue
		}
		r := keptBlank{id: cellID{ref.tab.id, ref.row, ref.col}, rec: collectedBlank{seq: c.seq, textSeq: c.textSeq, by: c.by}}
		if c.style != 0 {
			stamps := wb.styles.state(c.style).stamps
			r.rec.stamps = &stamps
		}
		h.each(r.id, &r.rec, func(e *undoEntry) {
			if !slices.Contains(r.holders, e.seq) {
				r.holders = append(r.holders, e.seq)
			}
		})
		// A row the cell has from an earlier collection is out of date
		// either way.
		if len(r.holders) > 0 || b.filter.may(r.id) {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return refs, nil
	}
	if err := b.fault("blank-write"); err != nil {
		in.scratchFailed("blank records", err)
		return nil, err
	}
	if err := b.open(); err != nil {
		in.scratchFailed("blank records", err)
		if !noRoom(err) {
			return nil, err
		}
		// No file can be made: no record fits, and no cell has rows.
		for i := range rows {
			h.each(rows[i].id, &rows[i].rec, func(e *undoEntry) { e.blanksLost = true })
		}
		return refs, nil
	}
	full, tried := false, false
	var stuck map[cellID]bool
	// room measures the file and reports whether it can take more rows.
	room := func() (bool, error) {
		size, err := b.bytes()
		if err != nil {
			return false, b.failed(err)
		}
		return size+blankReserve*blankPage <= b.limit(), nil
	}
	for from := 0; from < len(rows); {
		chunk := rows[from:min(from+blankChunk, len(rows))]
		if !full {
			ok, err := room()
			if err != nil {
				return nil, err
			}
			if !ok && !tried {
				// Make room first: drop what no entry can read any more.
				// A prune that finds no room either leaves the rows for a
				// later tick.
				tried = true
				if err := b.prune(wb, h); err != nil && !noRoom(err) {
					return nil, err
				}
				if ok, err = room(); err != nil {
					return nil, err
				}
			}
			full = !ok
		}
		kept, filled, blocked, err := b.store(ctx, chunk, full)
		if err != nil {
			return nil, b.failed(err)
		}
		if !blocked {
			b.fails = 0
		}
		for i := range chunk[:kept] {
			if len(chunk[i].holders) > 0 {
				b.filter.add(chunk[i].id)
			}
		}
		if filled && kept < len(chunk) && !tried {
			tried = true
			if err := b.prune(wb, h); err != nil && !noRoom(err) {
				return nil, err
			}
			from += kept
			continue
		}
		if full = full || filled; full {
			// There is no more room. The entries that held the records it
			// did not take skip their cells as lost, except where the
			// record stays in the sheet.
			for i := kept; i < len(chunk); i++ {
				if blocked && b.filter.may(chunk[i].id) {
					if stuck == nil {
						stuck = map[cellID]bool{}
					}
					stuck[chunk[i].id] = true
					continue
				}
				h.each(chunk[i].id, &chunk[i].rec, func(e *undoEntry) { e.blanksLost = true })
			}
		}
		from += len(chunk)
	}
	b.owner = wb
	wb.collected.store = b
	if stuck != nil {
		refs = slices.DeleteFunc(refs, func(ref cellRef) bool { return stuck[cellID{ref.tab.id, ref.row, ref.col}] })
	}
	return refs, nil
}

// store writes a chunk of records in one transaction. It deletes the rows
// their cells have from an earlier collection, and adds a row for each
// holder of each record, in order, while the file has room. With full set
// it adds none. It returns how many records of the chunk are stored, and
// whether the file filled before the last of them.
//
// It measures the file after every record, so the reserve has to cover
// the rows of one record only, at most two holders for each of its
// groups. SQLite refuses to grow the file past its largest size, and
// reports a full disk the same way, as SQLITE_FULL, for the file and for
// its rollback journal alike. It then rolls the transaction back. The
// records it had written before the one it refused fit, and store writes
// them again without the others. When there is no room even to delete
// the earlier rows, which takes room in the journal, it stores nothing
// and reports blocked: the cells that may have such rows cannot be
// collected yet.
func (b *blankStore) store(ctx context.Context, chunk []keptBlank, full bool) (kept int, filled, blocked bool, err error) {
	keep := len(chunk)
	if full {
		keep = 0
	}
	cut := false
	for {
		kept, filled, done, err := b.write(ctx, chunk, keep)
		if err == nil {
			return kept, filled || cut, false, nil
		}
		if !noRoom(err) {
			return 0, false, false, err
		}
		if keep == 0 {
			b.in.scratchFailed("blank records", err)
			return 0, true, true, nil
		}
		keep, cut = min(done, keep-1), true
	}
}

// write is one attempt of store, with rows for the first keep records at
// most. done is how many records it had written when an error came.
func (b *blankStore) write(ctx context.Context, chunk []keptBlank, keep int) (kept int, filled bool, done int, err error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, 0, err
	}
	kept = keep
	err = func() error {
		del, err := tx.PrepareContext(ctx, `DELETE FROM blanks WHERE tab=? AND row=? AND col=?`)
		if err != nil {
			return err
		}
		defer del.Close()
		put, err := tx.PrepareContext(ctx, `INSERT INTO blanks(tab,row,col,holder,seq,text_seq,by,stamps) VALUES(?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer put.Close()
		count, err := tx.PrepareContext(ctx, `PRAGMA page_count`)
		if err != nil {
			return err
		}
		defer count.Close()
		var n int64
		for i := range chunk {
			id, rec := chunk[i].id, &chunk[i].rec
			if b.filter.may(id) {
				res, err := del.ExecContext(ctx, int64(id.tab), int64(id.row), int64(id.col))
				if err != nil {
					return err
				}
				gone, _ := res.RowsAffected()
				n -= gone
			}
			if i >= kept {
				continue
			}
			for _, holder := range chunk[i].holders {
				if _, err := put.ExecContext(ctx, int64(id.tab), int64(id.row), int64(id.col), int64(holder),
					int64(rec.seq), int64(rec.textSeq), int64(rec.by), encodeBlankStamps(rec.stamps)); err != nil {
					return err
				}
				n++
			}
			done = i + 1
			if len(chunk[i].holders) == 0 || done == kept {
				continue
			}
			var pages int64
			if err := count.QueryRowContext(ctx).Scan(&pages); err != nil {
				return err
			}
			if pages*blankPage+blankReserve*blankPage > b.limit() {
				kept, filled = done, true
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		b.rows += n
		return nil
	}()
	if err != nil {
		tx.Rollback()
		return 0, false, done, err
	}
	return kept, filled, done, nil
}

// clear empties the store by removing its file, which needs no room in a
// journal, and the next collection makes a new one.
// clear drops the rows and the workbook they belong to, whether or not
// the file is open. A file given up after a failure is closed and removed
// already, and owner still names its workbook until clear.
func (b *blankStore) clear() {
	b.close()
	if b.path != "" {
		removeBlankFile(b.path)
	}
	b.lost = false
	b.recheck, b.again, b.cursor, b.fails = false, false, blankCursor{}, 0
}

// pruneBlanks runs with the maintenance tick. It deletes the rows no undo
// entry can read any more. An error, no room included, leaves them for a
// later tick.
func (in *Instance) pruneBlanks() {
	b := &in.scratch.blanks
	if b.owner != nil && b.owner != in.wb {
		b.clear()
		return
	}
	if b.db == nil {
		return
	}
	b.prune(in.wb, in.holders())
}

// prune deletes the rows whose stamp no entry holds any more, and those of
// rows, columns and tabs that are gone from the sheet and that no entry
// can bring back. Of the latter it looks at blankRecheckWork at most, so
// that a tick holds the workbook lock for a bounded time, whether it
// prunes for itself or to make room for a collection.
func (b *blankStore) prune(wb *Workbook, h *stampHolders) error {
	if b.rows == 0 {
		return nil
	}
	exec := func(query string, args ...any) error {
		res, err := b.db.Exec(query, args...)
		if err != nil {
			return b.failed(err)
		}
		n, _ := res.RowsAffected()
		b.rows -= n
		return nil
	}
	next := func(query string, args ...any) (int64, bool, error) {
		var v sql.NullInt64
		if err := b.db.QueryRow(query, args...).Scan(&v); err != nil {
			return 0, false, b.failed(err)
		}
		return v.Int64, v.Valid, nil
	}
	// The rows of the entries that left their stacks.
	for stamp := int64(-1); ; {
		v, ok, err := next(`SELECT min(holder) FROM blanks WHERE holder>?`, stamp)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if stamp = v; h.entries[uint64(stamp)] == nil {
			if err := exec(`DELETE FROM blanks WHERE holder=?`, stamp); err != nil {
				return err
			}
		}
	}
	c := &b.cursor
	for n := 0; b.recheck && n < blankRecheckWork; n++ {
		if !c.open {
			if !c.started {
				c.started, b.again = true, false
			}
			tab, ok, err := next(`SELECT min(tab) FROM blanks WHERE tab>?`, c.tab)
			if err != nil {
				return err
			}
			if !ok {
				// The pass is through. What left the sheet meanwhile starts
				// another one.
				*c, b.recheck = blankCursor{}, b.again
				break
			}
			if wb.byID[TabID(tab)] == nil {
				// A tab that is not in the sheet returns whole or not at all.
				if !h.brings(TabID(tab), false, 0) {
					if err := exec(`DELETE FROM blanks WHERE tab=?`, tab); err != nil {
						return err
					}
				}
				// The cursor moves past what is deleted, so that a
				// deletion that failed comes again.
				c.tab = tab
				continue
			}
			c.tab, c.cols, c.id, c.open = tab, false, -1, true
			continue
		}
		t := wb.byID[TabID(c.tab)]
		if t == nil {
			// The tab left the sheet since the last tick: look at it again.
			c.tab, c.open = c.tab-1, false
			continue
		}
		query, del, a := `SELECT min(row) FROM blanks WHERE tab=? AND row>?`, `DELETE FROM blanks WHERE tab=? AND row=?`, &t.rows
		if c.cols {
			query, del, a = `SELECT min(col) FROM blanks WHERE tab=? AND col>?`, `DELETE FROM blanks WHERE tab=? AND col=?`, &t.cols
		}
		id, ok, err := next(query, c.tab, c.id)
		if err != nil {
			return err
		}
		switch {
		case !ok && !c.cols:
			c.cols, c.id = true, -1
		case !ok:
			c.open = false
		default:
			if _, live := a.pos[uint64(id)]; !live && !h.brings(t.id, !c.cols, uint64(id)) {
				if err := exec(del, c.tab, id); err != nil {
					return err
				}
			}
			c.id = id
		}
	}
	b.fails = 0
	if b.rows <= 0 {
		b.rows = 0
		clear(b.filter.bits)
		b.filter.adds = 0
	} else if b.filter.adds > 2*b.rows+4096 {
		return b.refilter()
	}
	return nil
}

// refilter builds the filter again from the rows, after many of the cells
// it was built from have left.
func (b *blankStore) refilter() error {
	rows, err := b.db.Query(`SELECT DISTINCT tab,row,col FROM blanks`)
	if err != nil {
		return b.failed(err)
	}
	defer rows.Close()
	clear(b.filter.bits)
	b.filter.adds = 0
	for rows.Next() {
		var tab, row, col int64
		if err := rows.Scan(&tab, &row, &col); err != nil {
			return b.failed(err)
		}
		b.filter.add(cellID{TabID(tab), RowID(row), ColID(col)})
	}
	if err := rows.Err(); err != nil {
		// A filter with too few cells would hide rows: let every cell ask.
		for i := range b.filter.bits {
			b.filter.bits[i] = ^uint64(0)
		}
		return b.failed(err)
	}
	return nil
}

// gone notes that rows, columns or tabs left the sheet, or an entry that
// could bring some back.
func (b *blankStore) gone() {
	b.recheck, b.again = true, true
}

// blankBatch is how many cells one statement of the store names. Three
// values a cell stay well inside SQLite's limit of 32,766 bound values.
const blankBatch = 1024

func compareCells(a, b cellID) int {
	if a.tab != b.tab {
		return cmp.Compare(a.tab, b.tab)
	}
	if a.row != b.row {
		return cmp.Compare(a.row, b.row)
	}
	return cmp.Compare(a.col, b.col)
}

// sortedCells returns the cells in key order, each once. Cells in key
// order read and write the same pages of the store one after another.
func sortedCells(ids []cellID) []cellID {
	ids = slices.Clone(ids)
	slices.SortFunc(ids, compareCells)
	return slices.Compact(ids)
}

// cellList returns the list of the cells for a statement, and appends
// their values to args.
func cellList(ids []cellID, args []any) (string, []any) {
	for _, id := range ids {
		args = append(args, int64(id.tab), int64(id.row), int64(id.col))
	}
	return "(VALUES " + strings.TrimSuffix(strings.Repeat("(?,?,?),", len(ids)), ",") + ")", args
}

// blankScanRows is how many rows a read of the store scans for each cell
// it asks for, beside blankScanExtra, before it looks the cells up one by
// one. Rows of other cells can lie between them.
const (
	blankScanRows  = 4
	blankScanExtra = 64
)

// read returns the records of the given cells whose row belongs to an
// entry that is still on a stack. Of the rows of a cell it takes the one of
// the first such entry.
//
// It reads blankBatch cells at a time, in key order. A batch first scans
// the rows from its first cell to its last, which reads the leaves of the
// store in order and costs far less than looking each cell up. The scan
// stops after blankScanRows rows a cell; when the rows of other cells
// fill that, the cells it did not reach are looked up one by one. A batch
// therefore reads at most that many rows, whatever the store holds
// between its cells.
func (b *blankStore) read(ids []cellID, h *stampHolders) (map[cellID]*collectedBlank, error) {
	if err := b.fault("blank-read"); err != nil {
		return nil, b.failed(err)
	}
	tx, err := b.db.Begin()
	if err != nil {
		return nil, b.failed(err)
	}
	defer tx.Rollback()
	var want []cellID
	for _, id := range ids {
		if b.filter.may(id) {
			want = append(want, id)
		}
	}
	want = sortedCells(want)
	recs := make(map[cellID]*collectedBlank, len(want))
	holders := make(map[cellID]int64, len(want))
	// take reads the rows of a query into recs, those of the cells keep
	// accepts, and returns the last cell read and how many rows it read.
	take := func(rows *sql.Rows, err error, keep func(cellID) bool) (last cellID, n int, _ error) {
		if err != nil {
			return last, 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var tab, row, col, holder, seq, textSeq, by int64
			var stamps []byte
			if err := rows.Scan(&tab, &row, &col, &holder, &seq, &textSeq, &by, &stamps); err != nil {
				return last, n, err
			}
			n++
			id := cellID{TabID(tab), RowID(row), ColID(col)}
			last = id
			if !keep(id) || h.entries[uint64(holder)] == nil {
				continue
			}
			if first, ok := holders[id]; ok && first < holder {
				continue
			}
			rec := &collectedBlank{seq: uint64(seq), textSeq: uint64(textSeq), by: uint32(by)}
			if rec.stamps, err = decodeBlankStamps(stamps); err != nil {
				return last, n, err
			}
			recs[id], holders[id] = rec, holder
		}
		return last, n, rows.Err()
	}
	const columns = `SELECT tab,row,col,holder,seq,text_seq,by,stamps FROM blanks WHERE `
	args := make([]any, 0, 3*min(len(want), blankBatch))
	for len(want) > 0 {
		batch := want[:min(len(want), blankBatch)]
		want = want[len(batch):]
		lo, hi := batch[0], batch[len(batch)-1]
		limit := blankScanRows*len(batch) + blankScanExtra
		rows, err := tx.Query(columns+`(tab,row,col)>=(?,?,?) AND (tab,row,col)<=(?,?,?) ORDER BY tab,row,col,holder LIMIT ?`,
			int64(lo.tab), int64(lo.row), int64(lo.col), int64(hi.tab), int64(hi.row), int64(hi.col), limit)
		last, n, err := take(rows, err, func(id cellID) bool {
			_, found := slices.BinarySearchFunc(batch, id, compareCells)
			return found
		})
		if err != nil {
			return nil, b.failed(err)
		}
		if n < limit {
			continue
		}
		// The scan stopped inside the batch, maybe among the rows of its
		// last cell. From that cell on, the cells are looked up.
		at, _ := slices.BinarySearchFunc(batch, last, compareCells)
		rest := batch[at:]
		for _, id := range rest {
			delete(recs, id)
			delete(holders, id)
		}
		list, args := cellList(rest, args[:0])
		rows, err = tx.Query(columns+`(tab,row,col) IN `+list, args...)
		if _, _, err := take(rows, err, func(cellID) bool { return true }); err != nil {
			return nil, b.failed(err)
		}
	}
	b.fails = 0
	return recs, nil
}

// consume deletes the rows of cells that have a record in the sheet again:
// the record carries the stamps now. Rows it cannot delete, for want of
// room or after another error, stay harmless: the store is read only for
// cells without a record, and a collection of the cell deletes them before
// it stores the cell again.
func (b *blankStore) consume(wb *Workbook, ids []cellID) {
	if b.db == nil || b.rows == 0 {
		return
	}
	var back []cellID
	for _, id := range ids {
		if t := wb.byID[id.tab]; t != nil && t.cell(id.row, id.col) != nil {
			back = append(back, id)
		}
	}
	tx, err := b.db.Begin()
	if err != nil {
		b.failed(err)
		return
	}
	var n int64
	back = sortedCells(back)
	args := make([]any, 0, 3*min(len(back), blankBatch))
	for len(back) > 0 {
		batch := back[:min(len(back), blankBatch)]
		back = back[len(batch):]
		var list string
		list, args = cellList(batch, args[:0])
		res, err := tx.Exec(`DELETE FROM blanks WHERE (tab,row,col) IN `+list, args...)
		if err != nil {
			tx.Rollback()
			b.failed(err)
			return
		}
		gone, _ := res.RowsAffected()
		n += gone
	}
	if err := tx.Commit(); err != nil {
		b.failed(err)
		return
	}
	b.rows -= n
	b.fails = 0
}

// blankFetch is how many cells one read of the store covers.
const blankFetch = 4096

// fetchBlanks reads the collected blank records of cells that have no
// record into x.recalls, which then has an entry for each of the cells: the
// record, or nil. A record counts while an entry that held a stamp of it
// is on a stack, and not in a tab that a recovery replaced since. lost
// reports that the store is gone: nothing is known about the cells.
func (x *txn) fetchBlanks(ids []cellID) (lost bool, err error) {
	wb := x.wb
	x.recalls = make(map[cellID]*collectedBlank, len(ids))
	for _, id := range ids {
		x.recalls[id] = nil
	}
	b := wb.collected.store
	if b == nil || b.owner != wb {
		return false, nil
	}
	if b.lost {
		return true, nil
	}
	if b.rows == 0 || len(ids) == 0 {
		return false, nil
	}
	if x.held == nil {
		x.held = b.in.holders()
	}
	recs, err := b.read(ids, x.held)
	if err != nil {
		if b.lost {
			return true, nil
		}
		return false, err
	}
	for id, rec := range recs {
		if t := wb.byID[id.tab]; t == nil || rec.newest() >= t.recovered {
			x.recalls[id] = rec
		}
	}
	return false, nil
}

// collectedFor reads the collected records of those cells of a stretch of
// an entry that have no record in the sheet.
func (x *txn) collectedFor(rs []cellRestore) (lost bool, werr *WriteError) {
	wb := x.wb
	if b := wb.collected.store; b == nil || b.owner != wb || (b.rows == 0 && !b.lost) {
		// The store holds no record of this workbook: no cell is looked up.
		x.recalls = nil
		return false, nil
	}
	var ids []cellID
	for i := range rs {
		r := &rs[i]
		if t := wb.byID[r.tab]; t != nil && t.cell(r.row, r.col) == nil {
			ids = append(ids, cellID{r.tab, r.row, r.col})
		}
	}
	lost, err := x.fetchBlanks(ids)
	if err != nil {
		return false, scratchRefusal(err)
	}
	return lost, nil
}

// recall gives a record that an action creates the stamps of the blank
// record the tick collected from its cell: the action writes some groups
// of the cell, and the others are as the collected record left them, so
// that an undo entry that holds one of them still finds it. The journal
// entry of the new record takes the stamps as those before the action. An
// error of the store refuses the action when it finishes.
func (x *txn) recall(t *Tab, row RowID, col ColID, c *Cell) {
	wb := x.wb
	b := wb.collected.store
	if b == nil || b.owner != wb || (b.rows == 0 && !b.lost) {
		return
	}
	id := cellID{t.id, row, col}
	rec, asked := x.recalls[id]
	if !asked {
		if b.lost || !b.filter.may(id) {
			return
		}
		if _, err := x.fetchBlanks([]cellID{id}); err != nil {
			x.blankErr = err
			return
		}
		rec = x.recalls[id]
	}
	if rec == nil {
		return
	}
	c.seq, c.textSeq, c.by = rec.seq, rec.textSeq, rec.by
	if rec.stamps != nil {
		c.style = wb.styles.intern(styleState{stamps: *rec.stamps})
	}
	x.recalled = append(x.recalled, id)
}

// settleStamps applies what a committed action means for the stamps undo
// keeps outside the sheet: the notes of groups it wrote are gone, the
// entries its notes pushed out expire, and the collected records of cells
// that have a record again leave the store.
func (in *Instance) settleStamps(x *txn) {
	for s, n := range x.replaced {
		if w := in.scratch.notes.sweep; w != nil && w.set == s {
			w.gone += n.swept
		}
		if s.gone += n.set; s.gone >= s.n {
			s.owner.removeNotes(s)
		}
	}
	for _, ex := range x.expired {
		ex.e.expire(ex.why)
	}
	b := x.wb.collected.store
	if b == nil || b.owner != x.wb {
		return
	}
	if len(x.recalled) > 0 {
		b.consume(x.wb, x.recalled)
	}
	for i := range x.structs {
		if k := x.structs[i].kind; k == editDelete || k == editTabDelete {
			b.gone()
			break
		}
	}
}

// authorOf is who a restored value is credited to: a blank that an undo
// brings back stays the clear of the writer who made it.
func authorOf(r *cellRestore, by uint32) uint32 {
	if r.before.Kind == formula.ContentBlank && r.f == nil {
		return r.beforeBy
	}
	return by
}
