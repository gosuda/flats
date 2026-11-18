package sheet

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// maxOutboxPending is the number of unimported outbox entries from which
// new work is refused with a retryable error (section 6).
const maxOutboxPending = 256

// The most rows the outbox and the snapshots hold at a commit. Work that
// appends to the outbox is admitted while fewer than maxOutboxPending
// entries wait (outboxAdmission): an agent call or a rollback appends two
// at most, the revision of the groups it closes and its own, and an action
// or a recovery none, as it only marks its group dirty. Only the closing
// of groups appends without admission, by a tick or when the instance
// stops, and only for a dirty group, which an admitted edit marked; the
// revision it records takes every mark away. So after the last admitted
// work, of at most maxOutboxPending-1 entries before it, the outbox holds
// at most two more. The first revision of a new sheet is the only entry
// then. A snapshot is a new file of a pending entry, which names at most
// the files of a revision.
const (
	maxOutboxRows   = maxOutboxPending + 1
	maxSnapshotRows = maxOutboxRows * maxRevisionFiles
)

// maxRevisionAuthors is the most editors a revision names (closeGroups).
const maxRevisionAuthors = maxSessions

// The longest fields the writer stores in an outbox row and in the file
// list of meta, in bytes, which the readers hold them to (boundedField). A
// list of authors holds labels of at most maxLabelBytes, which JSON writes
// in at most six bytes for each; a message is a call's of at most 1,000
// code points; the tabs of an entry are the live ones and those of the
// last revision, and its new files and its file list are those of a
// revision.
const (
	maxKindBytes       = len("rollback")
	maxAuthorsBytes    = len(`[]`) + maxRevisionAuthors*(len(`"",`)+6*maxLabelBytes)
	maxMessageBytes    = 4 * 1000
	maxOpIDBytes       = 128
	maxOutboxTabsBytes = len(`{"tabs":[],"new":[]}`) + 2*maxTabs*len(`9007199254740991,`) + maxRevisionFiles*(maxHashBytes+len(`"",`))
	maxFileListBytes   = len(`[]`) + maxRevisionFiles*(len(`{"path":"","hash":"","size":},`)+maxPathBytes+maxHashBytes+len("9999999"))
	// maxSnapshotBytes is the largest revision file.
	maxSnapshotBytes = max(maxPartBytes, maxAxesBytes, maxManifestBytes)
)

// OutboxEntry is a durable revision boundary awaiting host import.
type OutboxEntry struct {
	Seq        int64
	Kind       string // "created", "person", "agent" or "rollback"
	Message    string
	OpID       string
	Authors    []string
	RollbackOf int
	StateSeq   uint64
	CalcDate   string // the calculation date of the state the revision closed with
	Tabs       []uint64
	Files      []File
	NewHashes  []string
	CreatedAt  int64
}

// outboxTabs is the tabs column of an outbox row: the tabs that changed
// since the entry before, and the snapshots the entry added.
type outboxTabs struct {
	Tabs []uint64 `json:"tabs"`
	New  []string `json:"new"`
}

type boundary struct {
	kind, message, opID string
	authors             []string
	force               bool
}

// recordBoundary closes a revision: it encodes the dirty files, stores new
// bytes in snapshots and appends the outbox entry with the full file list
// and the state's calculation date. Without force it records no revision
// when no file changed, and still takes the marks away and saves the chunk
// lists it rebalanced. undo takes the memory part out again.
func (in *Instance) recordBoundary(ctx context.Context, tx *sql.Tx, wb *Workbook, b boundary) (seq int64, undo func(), err error) {
	undo = func() {}
	if !b.force && !wb.files.dirty() {
		return 0, undo, nil
	}
	plan, err := wb.planFiles()
	if err != nil {
		return 0, undo, err
	}
	return in.recordPlan(ctx, tx, wb, b, plan)
}

// recordPlan stages a boundary from the encoded files of a plan.
func (in *Instance) recordPlan(ctx context.Context, tx *sql.Tx, wb *Workbook, b boundary, plan *filePlan) (seq int64, undo func(), err error) {
	changed := len(plan.put) > 0 || len(plan.remove) > 0
	undo = wb.applyPlan(plan)
	fail := func(err error) (int64, func(), error) {
		undo()
		return 0, func() {}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET dirty_seq=0,reserve=0 WHERE dirty_seq<>0`); err != nil {
		return fail(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE hash=''`); err != nil {
		return fail(err)
	}
	// The marks are gone, so the lists rebalance left are saved even when
	// no file changed: the old ones load only with their marks.
	for t, chunks := range plan.chunks {
		if _, err := tx.ExecContext(ctx, `UPDATE tabs SET chunks=? WHERE id=?`, encodeRowIDs(chunks), t.id); err != nil {
			return fail(err)
		}
	}
	if !changed && !b.force {
		return 0, undo, nil
	}
	for _, path := range plan.remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE path=?`, path); err != nil {
			return fail(err)
		}
	}
	for path, f := range plan.put {
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES(?,?,?,0,0)
			ON CONFLICT(path) DO UPDATE SET hash=excluded.hash,size=excluded.size,dirty_seq=0,reserve=0`, path, f.Hash, f.Size); err != nil {
			return fail(err)
		}
	}
	hashes := make([]string, 0, len(plan.data))
	for hash, data := range plan.data {
		if _, err := tx.ExecContext(ctx, `INSERT INTO snapshots(hash,data,refs) VALUES(?,?,1) ON CONFLICT(hash) DO UPDATE SET refs=refs+1`, hash, data); err != nil {
			return fail(err)
		}
		hashes = append(hashes, hash)
	}
	slices.Sort(hashes)
	seq, err = in.insertOutbox(ctx, tx, wb, b, plan.changed, hashes)
	if err != nil {
		return fail(err)
	}
	return seq, undo, nil
}

func (in *Instance) insertOutbox(ctx context.Context, tx *sql.Tx, wb *Workbook, b boundary, tabs []TabID, hashes []string) (int64, error) {
	files, err := json.Marshal(wb.files.sorted())
	if err != nil {
		return 0, err
	}
	if b.authors == nil {
		b.authors = []string{}
	}
	authors, _ := json.Marshal(b.authors)
	ids := make([]uint64, len(tabs))
	for i, id := range tabs {
		ids[i] = uint64(id)
	}
	if hashes == nil {
		hashes = []string{}
	}
	meta, _ := json.Marshal(outboxTabs{Tabs: ids, New: hashes})
	result, err := tx.ExecContext(ctx, `INSERT INTO outbox(kind,authors,message,op_id,state_seq,calc_date,tabs,files,created) VALUES(?,?,?,?,?,?,?,?,?)`,
		b.kind, string(authors), b.message, b.opID, wb.seq, wb.date.String(), string(meta), files, in.now().Unix())
	if err != nil {
		return 0, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE meta SET files=? WHERE singleton=1`, files)
	return seq, err
}

// outboxAdmission refuses new work while the host has not imported the
// outbox.
func outboxAdmission(ctx context.Context, q querier) error {
	var pending int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&pending); err != nil {
		return err
	}
	if pending >= maxOutboxPending {
		return unavailable("outbox_backpressure")
	}
	return nil
}

// checkImportCursor refuses a sheet.db that lacks entries flats.db
// imported: its next entries would reuse sequence numbers the cursor
// passed. sqlite_sequence holds one row, the outbox's, from the first
// entry on. At most two rows are read, each checked whole, so the read
// checks the table.
func checkImportCursor(ctx context.Context, db *sql.DB, store string, opts Options) error {
	rows, err := db.QueryContext(ctx, `SELECT `+storedValues("sqlite_sequence", "", "name", "seq")+`,`+storedFault("sqlite_sequence", "")+
		` FROM sqlite_sequence LIMIT 2`)
	if err != nil {
		return err
	}
	var last int64
	for n := 0; rows.Next(); n++ {
		var name sql.NullString
		var fault int64
		if err := rows.Scan(&name, &last, &fault); err != nil {
			rows.Close()
			return err
		}
		if err := faultError("sqlite_sequence", fault); err != nil {
			rows.Close()
			return err
		}
		if n > 0 || name.String != "outbox" {
			rows.Close()
			return fmt.Errorf("sheet.db sqlite_sequence holds a row for a table without an autoincrement")
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	if opts.ImportStore != store {
		return nil
	}
	if opts.ImportSeq > last {
		return fmt.Errorf("sheet.db is behind flats.db: its outbox reached entry %d and flats.db imported through %d; restore both from one backup", last, opts.ImportSeq)
	}
	return nil
}

// Outbox returns pending entries in import order after the given cursor.
func (in *Instance) Outbox(ctx context.Context, after int64, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if err := in.checkTables(ctx, "outbox"); err != nil {
		return nil, err
	}
	fields := []struct {
		column string
		max    int
	}{
		{"kind", maxKindBytes}, {"authors", maxAuthorsBytes}, {"message", maxMessageBytes}, {"op_id", maxOpIDBytes},
		{"calc_date", maxDateBytes}, {"tabs", maxOutboxTabsBytes}, {"files", maxFileListBytes},
	}
	query := `SELECT seq,rollback_of,state_seq,created`
	for _, f := range fields {
		query += "," + boundedField(f.column, f.max)
	}
	rows, err := in.rdb.QueryContext(ctx, query+` FROM outbox WHERE seq>? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []OutboxEntry{}
	for rows.Next() {
		var e OutboxEntry
		var kind, authors, message, opID, date, tabs sql.NullString
		var files []byte
		var rollback sql.NullInt64
		lengths := make([]sql.NullInt64, len(fields))
		if err := rows.Scan(&e.Seq, &rollback, &e.StateSeq, &e.CreatedAt, &kind, &lengths[0], &authors, &lengths[1], &message, &lengths[2],
			&opID, &lengths[3], &date, &lengths[4], &tabs, &lengths[5], &files, &lengths[6]); err != nil {
			return nil, err
		}
		for i, f := range fields {
			if err := fieldFits(fmt.Sprintf("outbox %d %s", e.Seq, f.column), lengths[i], f.max); err != nil {
				return nil, err
			}
		}
		e.Kind, e.Message, e.OpID, e.CalcDate = kind.String, message.String, opID.String, date.String
		e.RollbackOf = int(rollback.Int64)
		var meta outboxTabs
		for _, field := range []struct {
			raw []byte
			dst any
		}{{[]byte(authors.String), &e.Authors}, {[]byte(tabs.String), &meta}} {
			if err := json.Unmarshal(field.raw, field.dst); err != nil {
				return nil, fmt.Errorf("sheet outbox %d: %w", e.Seq, err)
			}
		}
		if e.Files, err = decodeFileList(files); err != nil {
			return nil, fmt.Errorf("sheet outbox %d: %w", e.Seq, err)
		}
		e.Tabs, e.NewHashes = meta.Tabs, meta.New
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// SnapshotData returns bytes retained until their outbox entries are
// imported.
func (in *Instance) SnapshotData(ctx context.Context, hash string) ([]byte, error) {
	if !validHash(hash) {
		return nil, fmt.Errorf("invalid sheet content hash")
	}
	if err := in.checkTables(ctx, "snapshots"); err != nil {
		return nil, err
	}
	var data []byte
	var size sql.NullInt64
	if err := in.rdb.QueryRowContext(ctx, `SELECT `+boundedField("data", maxSnapshotBytes)+` FROM snapshots WHERE hash=? AND refs>0`, hash).Scan(&data, &size); err != nil {
		return nil, fmt.Errorf("sheet snapshot %s: %w", hash, err)
	}
	if err := fieldFits("snapshot "+hash, size, maxSnapshotBytes); err != nil {
		return nil, err
	}
	return data, nil
}

// Acknowledge releases content after the host has committed the import of
// the entries through the given one. stored is the bytes the import added
// to the host's blob store. While the sheet calculates without an edit the
// call is refused as calculating and has changed nothing. The host sends
// it again with its next import.
func (in *Instance) Acknowledge(ctx context.Context, through, stored int64) error {
	if through < 0 || stored < 0 {
		return fmt.Errorf("invalid sheet acknowledgement")
	}
	return in.execute(ctx, &work{soon: true, run: func() error {
		var pending int
		err := in.transaction(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE seq<=?`, through).Scan(&pending); err != nil {
				return err
			}
			return acknowledgeOutbox(ctx, tx, through, in.now())
		})
		// The bytes count once the entries are gone for good: the host
		// sends an acknowledgement again when its commit failed.
		if err == nil && pending > 0 {
			in.measure.add(stored)
		}
		return err
	}})
}

func acknowledgeOutbox(ctx context.Context, tx *sql.Tx, through int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+boundedField("tabs", maxOutboxTabsBytes)+` FROM outbox WHERE seq<=? ORDER BY seq`, through)
	if err != nil {
		return err
	}
	drop := map[string]int{}
	for rows.Next() {
		var raw sql.NullString
		var size sql.NullInt64
		var meta outboxTabs
		if err := rows.Scan(&raw, &size); err != nil {
			rows.Close()
			return err
		}
		if err := fieldFits("outbox tabs", size, maxOutboxTabsBytes); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
			rows.Close()
			return err
		}
		for _, hash := range meta.New {
			drop[hash]++
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	for hash, count := range drop {
		if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET refs=refs-? WHERE hash=?`, count, hash); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE refs<=0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET imported=1,expires=? WHERE outbox_seq<>0 AND outbox_seq<=? AND imported=0`, now.Add(24*time.Hour).Unix(), through); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE seq<=?`, through)
	return err
}

// References returns the hashes of every revision file the store still
// names: the newest file list and the lists of pending outbox entries.
func (in *Instance) References(ctx context.Context) ([]string, error) {
	if err := in.checkTables(ctx, "meta", "outbox"); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	if err := fileLists(ctx, in.rdb, func(files []File) {
		for _, f := range files {
			seen[f.Hash] = true
		}
	}); err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(seen))
	for hash := range seen {
		hashes = append(hashes, hash)
	}
	slices.Sort(hashes)
	return hashes, nil
}

// fileLists reads the file list of meta and of each outbox row, each only
// when it is of a class and a length the writer stores (storedTables),
// and hands each decoded list to fn.
func fileLists(ctx context.Context, q querier, fn func([]File)) error {
	var newest []byte
	var fault int64
	if err := q.QueryRowContext(ctx, `SELECT `+storedValue("meta", "", "files")+`,CASE WHEN `+columnOf("meta", "files").fits("files")+
		` THEN 0 ELSE 1 END FROM meta WHERE singleton=1`).Scan(&newest, &fault); err != nil {
		return err
	}
	if fault != 0 {
		return fmt.Errorf("sheet.db meta.files holds a value of a class or a length the writer does not store there")
	}
	list, err := decodeFileList(newest)
	if err != nil {
		return fmt.Errorf("sheet.db file list: %w", err)
	}
	fn(list)
	rows, err := q.QueryContext(ctx, `SELECT `+storedValue("outbox", "", "files")+`,CASE WHEN `+columnOf("outbox", "files").fits("files")+
		` THEN 0 ELSE 1 END FROM outbox`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw, &fault); err != nil {
			rows.Close()
			return err
		}
		if fault != 0 {
			rows.Close()
			return fmt.Errorf("sheet.db outbox.files holds a value of a class or a length the writer does not store there")
		}
		list, err := decodeFileList(raw)
		if err != nil {
			rows.Close()
			return fmt.Errorf("sheet.db outbox file list: %w", err)
		}
		fn(list)
	}
	return closeRows(rows)
}

// decodeFileList reads a revision file list as the writer stores it: a
// JSON array of at most maxRevisionFiles files, each with a path of the
// writer's spelling, a content hash and a size. The count is checked
// before each file is added.
func decodeFileList(raw []byte) ([]File, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var files []File
	err := jsonArray(dec, func() error {
		if len(files) == maxRevisionFiles {
			return fmt.Errorf("the list names more than the %d files of a revision", maxRevisionFiles)
		}
		var f File
		if err := dec.Decode(&f); err != nil {
			return err
		}
		if _, ok := parseFilePath(f.Path); !ok || !validHash(f.Hash) || f.Size < 0 {
			return fmt.Errorf("the list names a file %.80q that the writer does not name", f.Path)
		}
		files = append(files, f)
		return nil
	})
	if err == nil {
		err = jsonDone(dec)
	}
	if err != nil {
		return nil, err
	}
	if files == nil {
		files = []File{}
	}
	return files, nil
}

// BlobHashes returns the hashes of the revision files a sheet.db names
// whose bytes it does not hold: the files of the newest revision and of
// every pending outbox entry, less those its snapshots keep for the
// import. It reads each list and hash only when it is of a class and a
// length the writer stores. The backup check reads a copied sheet.db with
// it, as the instance reads its own (References).
func BlobHashes(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	for _, table := range []string{"outbox", "snapshots"} {
		if err := checkRows(ctx, db, table); err != nil {
			return nil, err
		}
	}
	out := map[string]bool{}
	if err := fileLists(ctx, db, func(files []File) {
		for _, f := range files {
			out[f.Hash] = true
		}
	}); err != nil {
		return nil, err
	}
	// A row that does not fit is read to be refused, and a held one to be
	// taken out.
	hash, refs := columnOf("snapshots", "hash"), columnOf("snapshots", "refs")
	fits := hash.fits("hash") + ` AND ` + refs.fits("refs")
	rows, err := db.QueryContext(ctx, `SELECT `+hash.value("hash")+`,CASE WHEN `+fits+` THEN 0 ELSE 1 END FROM snapshots WHERE NOT (`+
		fits+`) OR `+refs.value("refs")+`>0`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h sql.NullString
		var fault int64
		if err := rows.Scan(&h, &fault); err != nil {
			rows.Close()
			return nil, err
		}
		if fault != 0 || !validHash(h.String) {
			rows.Close()
			return nil, fmt.Errorf("sheet.db snapshots names a hash that the writer does not store")
		}
		delete(out, h.String)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return out, nil
}

func decimal(n uint64) string { return strconv.FormatUint(n, 10) }
