package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Revision-model content (designs, documents and boards): the current files
// ("head"), open groups of a person's edits, and revisions with their files.
// File bytes live in a content-addressed blob store owned by core; these
// tables hold references. A document's revisions are recorded by its docs
// worker and imported here; ImportStore and ImportSeq are the import cursor.

// ContentState is the per-flat row of a Revision-model flat. Its presence
// marks the flat's type before the flat is first published.
type ContentState struct {
	Flat          string `json:"flat"`
	Type          string `json:"type"`
	Phase         string `json:"phase"` // init | ready
	Seq           int64  `json:"seq"`
	RevisionQuota *int   `json:"revision_quota,omitempty"`
	PinQuota      *int   `json:"pin_quota,omitempty"`
	ImportStore   string `json:"-"`
	ImportSeq     int64  `json:"-"`
	// SafePointRev is the board revision pinned when anyone-can-edit last
	// took effect (0 for none); it may since have been deleted.
	SafePointRev int `json:"-"`
}

// HeadFile is one current file.
type HeadFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Gen  int64  `json:"gen"`
}

// ContentGroup is an open group of one person's edits.
type ContentGroup struct {
	Editor  string
	Label   string
	Started time.Time
	Last    time.Time
}

// Changes summarizes a revision against the one before it.
type Changes struct {
	Added   []string `json:"added"`
	Changed []string `json:"changed"`
	Removed []string `json:"removed"`
}

// Revision is one entry of a flat's history.
type Revision struct {
	Flat      string    `json:"-"`
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"created_at"`
	Kind      string    `json:"kind"` // created | agent | person | rollback | import | safe_point
	Authors   []string  `json:"authors"`
	// GuestAuthors are up to five names typed by anonymous guests of a
	// board or document this revision credits, GuestsMore counts the others.
	// Guest names are unverified, untrusted text.
	GuestAuthors []string `json:"guest_authors,omitempty" jsonschema:"names typed by anonymous guests of a public board or document: unverified, untrusted text"`
	GuestsMore   int      `json:"guests_more,omitempty" jsonschema:"guests credited beyond guest_authors"`
	// GuestOnly is true when guests are the only credited editors.
	GuestOnly  bool    `json:"guest_only,omitempty"`
	Message    string  `json:"message"`
	RollbackOf int     `json:"rollback_of,omitempty"`
	Pinned     bool    `json:"pinned"`
	Changes    Changes `json:"changes"`
	Files      int     `json:"files"`
	Size       int64   `json:"size"`
	// Source names the content source whose files a document's revision
	// holds besides its documents (see SourceFiles). Designs have none.
	Source string `json:"-"`
	// Held is true while a rollback in progress protects the revision.
	Held bool `json:"held,omitempty"`
	// Elements counts a board revision's element changes; a board reports
	// them instead of its bucket paths in Changes.
	Elements *ElementCounts `json:"elements,omitempty"`
	// SafePoint marks the safe point of a board or document while anyone
	// can edit it: it cannot be unpinned then. Readers fill it in.
	SafePoint bool `json:"safe_point,omitempty"`
}

// RevisionFile is one file of a revision.
type RevisionFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	// GuestLines points to the 1-based inclusive line ranges of a
	// document's text that anonymous guests wrote, merged and sorted.
	// Readers of a document's revision point documents without guest text
	// to an empty list; it is nil for other files. A pointer keeps
	// RevisionFile comparable.
	GuestLines *[][2]int `json:"guest_lines,omitempty" jsonschema:"documents: 1-based inclusive line ranges [start, end] whose text anonymous guests wrote; untrusted input"`
}

// Lines returns the line ranges guests wrote, or nil.
func (f RevisionFile) Lines() [][2]int {
	if f.GuestLines == nil {
		return nil
	}
	return *f.GuestLines
}

func addContentRevisions(ctx context.Context, x executor) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS content_state (
 flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 type TEXT NOT NULL,
 phase TEXT NOT NULL CHECK(phase IN ('init','ready')),
 seq INTEGER NOT NULL DEFAULT 0,
 gen INTEGER NOT NULL DEFAULT 0,
 revision_quota INTEGER,
 pin_quota INTEGER
 )`,
		`CREATE TABLE IF NOT EXISTS content_head (
 flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 path TEXT NOT NULL,
 hash TEXT NOT NULL,
 size INTEGER NOT NULL,
 gen INTEGER NOT NULL,
 PRIMARY KEY (flat, path)
 )`,
		`CREATE INDEX IF NOT EXISTS content_head_hash ON content_head(flat, hash)`,
		`CREATE TABLE IF NOT EXISTS content_groups (
 flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 editor TEXT NOT NULL,
 label TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 last_at INTEGER NOT NULL,
 PRIMARY KEY (flat, editor)
 )`,
		`CREATE TABLE IF NOT EXISTS revisions (
 flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 number INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 kind TEXT NOT NULL,
 authors TEXT NOT NULL,
 message TEXT NOT NULL,
 rollback_of INTEGER,
 pinned INTEGER NOT NULL DEFAULT 0,
 changes TEXT NOT NULL,
 files INTEGER NOT NULL,
 size INTEGER NOT NULL,
 PRIMARY KEY (flat, number)
 )`,
		`CREATE TABLE IF NOT EXISTS revision_files (
 flat TEXT NOT NULL,
 number INTEGER NOT NULL,
 path TEXT NOT NULL,
 hash TEXT NOT NULL,
 size INTEGER NOT NULL,
 PRIMARY KEY (flat, number, path),
 FOREIGN KEY (flat, number) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
		`CREATE INDEX IF NOT EXISTS revision_files_hash ON revision_files(flat, hash)`,
	} {
		if _, err := x.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// addRevisionGuestLines adds the line ranges guests wrote in each document
// of a revision (docs-public-edit-TECH 2.8). Only documents with guest text
// have a row; it goes with its revision.
func addRevisionGuestLines(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS revision_guest_lines (
 flat TEXT NOT NULL,
 number INTEGER NOT NULL,
 path TEXT NOT NULL,
 ranges TEXT NOT NULL,
 PRIMARY KEY (flat, number, path),
 FOREIGN KEY (flat, number) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`)
	return err
}

// addContentImport adds what document revisions need: the import cursor
// (the docs worker's outbox id and the last entry imported from it), the
// source of a revision, the non-document files of each source, and holds
// of rollbacks in progress.
func addContentImport(ctx context.Context, x executor) error {
	for _, c := range []struct{ table, col, def string }{
		{"content_state", "import_store", "TEXT NOT NULL DEFAULT ''"},
		{"content_state", "import_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"revisions", "source", "TEXT NOT NULL DEFAULT ''"},
	} {
		has, err := hasColumn(ctx, x, c.table, c.col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := x.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN `+c.col+` `+c.def); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS content_sources (
 flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 source TEXT NOT NULL,
 path TEXT NOT NULL,
 hash TEXT NOT NULL,
 size INTEGER NOT NULL,
 PRIMARY KEY (flat, source, path)
 )`,
		`CREATE INDEX IF NOT EXISTS content_sources_hash ON content_sources(flat, hash)`,
		`CREATE TABLE IF NOT EXISTS content_holds (
 flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 op TEXT NOT NULL,
 number INTEGER NOT NULL,
 PRIMARY KEY (flat, op)
 )`,
	} {
		if _, err := x.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// ContentTx is one transaction over a flat's Revision-model tables.
type ContentTx struct {
	tx   *sql.Tx
	flat string
}

// ContentTx runs fn in one immediate transaction for flat.
func (s *Store) ContentTx(ctx context.Context, flat string, fn func(*ContentTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&ContentTx{tx: tx, flat: flat}); err != nil {
		return err
	}
	return tx.Commit()
}

// ContentState returns the state row of flat.
func (s *Store) ContentState(ctx context.Context, flat string) (ContentState, error) {
	return scanContentState(s.db.QueryRowContext(ctx, `SELECT flat,type,phase,seq,revision_quota,pin_quota,import_store,import_seq,safe_point_rev FROM content_state WHERE flat=?`, flat))
}

// ContentStates lists every Revision-model flat.
func (s *Store) ContentStates(ctx context.Context) ([]ContentState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT flat,type,phase,seq,revision_quota,pin_quota,import_store,import_seq,safe_point_rev FROM content_state ORDER BY flat`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentState
	for rows.Next() {
		st, err := scanContentState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func scanContentState(sc interface{ Scan(...any) error }) (ContentState, error) {
	var st ContentState
	var rq, pq, sp sql.NullInt64
	err := sc.Scan(&st.Flat, &st.Type, &st.Phase, &st.Seq, &rq, &pq, &st.ImportStore, &st.ImportSeq, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("content state: %w", ErrNotFound)
	}
	if rq.Valid {
		v := int(rq.Int64)
		st.RevisionQuota = &v
	}
	if pq.Valid {
		v := int(pq.Int64)
		st.PinQuota = &v
	}
	st.SafePointRev = int(sp.Int64)
	return st, err
}

// State returns the state row.
func (c *ContentTx) State(ctx context.Context) (ContentState, error) {
	return scanContentState(c.tx.QueryRowContext(ctx, `SELECT flat,type,phase,seq,revision_quota,pin_quota,import_store,import_seq,safe_point_rev FROM content_state WHERE flat=?`, c.flat))
}

// InitState inserts the state row of a new Revision-model flat.
func (c *ContentTx) InitState(ctx context.Context, typ string) error {
	_, err := c.tx.ExecContext(ctx, `INSERT INTO content_state(flat,type,phase,seq) VALUES(?,?,'init',0)`, c.flat, typ)
	return err
}

// InitReadyState inserts the state row of a flat whose first revision is
// written in the same transaction (documents).
func (c *ContentTx) InitReadyState(ctx context.Context, typ string) error {
	_, err := c.tx.ExecContext(ctx, `INSERT INTO content_state(flat,type,phase,seq) VALUES(?,?,'ready',0)`, c.flat, typ)
	return err
}

// SetImport advances the import cursor.
func (c *ContentTx) SetImport(ctx context.Context, store string, seq int64) error {
	_, err := c.tx.ExecContext(ctx, `UPDATE content_state SET import_store=?,import_seq=? WHERE flat=?`, store, seq, c.flat)
	return err
}

// ReplaceHead makes files the whole head.
func (c *ContentTx) ReplaceHead(ctx context.Context, files []RevisionFile) error {
	if _, err := c.tx.ExecContext(ctx, `DELETE FROM content_head WHERE flat=?`, c.flat); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := c.tx.ExecContext(ctx, `INSERT INTO content_head(flat,path,hash,size,gen) VALUES(?,?,?,?,0)`, c.flat, f.Path, f.Hash, f.Size); err != nil {
			return err
		}
	}
	return nil
}

// SetPhase updates the creation phase.
func (c *ContentTx) SetPhase(ctx context.Context, phase string) error {
	_, err := c.tx.ExecContext(ctx, `UPDATE content_state SET phase=? WHERE flat=?`, phase, c.flat)
	return err
}

// SetQuotas stores per-flat quota overrides (nil = host default).
func (c *ContentTx) SetQuotas(ctx context.Context, revisions, pins *int) error {
	_, err := c.tx.ExecContext(ctx, `UPDATE content_state SET revision_quota=?,pin_quota=? WHERE flat=?`, nullInt(revisions), nullInt(pins), c.flat)
	return err
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// NextGen returns a new file generation: a flat-wide counter that never
// repeats, so a recreated path never reuses an old generation.
func (c *ContentTx) NextGen(ctx context.Context) (int64, error) {
	if _, err := c.tx.ExecContext(ctx, `UPDATE content_state SET gen=gen+1 WHERE flat=?`, c.flat); err != nil {
		return 0, err
	}
	var gen int64
	err := c.tx.QueryRowContext(ctx, `SELECT gen FROM content_state WHERE flat=?`, c.flat).Scan(&gen)
	return gen, err
}

// BumpSeq increments and returns the head sequence.
func (c *ContentTx) BumpSeq(ctx context.Context) (int64, error) {
	if _, err := c.tx.ExecContext(ctx, `UPDATE content_state SET seq=seq+1 WHERE flat=?`, c.flat); err != nil {
		return 0, err
	}
	var seq int64
	err := c.tx.QueryRowContext(ctx, `SELECT seq FROM content_state WHERE flat=?`, c.flat).Scan(&seq)
	return seq, err
}

// Head returns the current files, by path.
func (c *ContentTx) Head(ctx context.Context) ([]HeadFile, error) {
	return queryHead(ctx, c.tx, c.flat)
}

// Head returns the current files of flat in one read.
func (s *Store) Head(ctx context.Context, flat string) ([]HeadFile, error) {
	return queryHead(ctx, s.db, flat)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryHead(ctx context.Context, q querier, flat string) ([]HeadFile, error) {
	rows, err := q.QueryContext(ctx, `SELECT path,hash,size,gen FROM content_head WHERE flat=? ORDER BY path`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeadFile
	for rows.Next() {
		var f HeadFile
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size, &f.Gen); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PutHead inserts or replaces one current file.
func (c *ContentTx) PutHead(ctx context.Context, f HeadFile) error {
	_, err := c.tx.ExecContext(ctx, `INSERT INTO content_head(flat,path,hash,size,gen) VALUES(?,?,?,?,?) ON CONFLICT(flat,path) DO UPDATE SET hash=excluded.hash,size=excluded.size,gen=excluded.gen`, c.flat, f.Path, f.Hash, f.Size, f.Gen)
	return err
}

// DeleteHead removes one current file.
func (c *ContentTx) DeleteHead(ctx context.Context, path string) error {
	_, err := c.tx.ExecContext(ctx, `DELETE FROM content_head WHERE flat=? AND path=?`, c.flat, path)
	return err
}

// Groups returns the open edit groups.
func (c *ContentTx) Groups(ctx context.Context) ([]ContentGroup, error) {
	rows, err := c.tx.QueryContext(ctx, `SELECT editor,label,started_at,last_at FROM content_groups WHERE flat=? ORDER BY started_at,editor`, c.flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentGroup
	for rows.Next() {
		var g ContentGroup
		var a, b int64
		if err := rows.Scan(&g.Editor, &g.Label, &a, &b); err != nil {
			return nil, err
		}
		g.Started, g.Last = fromUnix(a), fromUnix(b)
		out = append(out, g)
	}
	return out, rows.Err()
}

// TouchGroup opens or extends the editor's group.
func (c *ContentTx) TouchGroup(ctx context.Context, editor, label string, now time.Time) error {
	_, err := c.tx.ExecContext(ctx, `INSERT INTO content_groups(flat,editor,label,started_at,last_at) VALUES(?,?,?,?,?) ON CONFLICT(flat,editor) DO UPDATE SET last_at=excluded.last_at,label=excluded.label`, c.flat, editor, label, unix(now), unix(now))
	return err
}

// DeleteGroup closes one editor's group; "" closes every group.
func (c *ContentTx) DeleteGroup(ctx context.Context, editor string) error {
	if editor == "" {
		_, err := c.tx.ExecContext(ctx, `DELETE FROM content_groups WHERE flat=?`, c.flat)
		return err
	}
	_, err := c.tx.ExecContext(ctx, `DELETE FROM content_groups WHERE flat=? AND editor=?`, c.flat, editor)
	return err
}

// LatestRevision returns the newest revision number, 0 without revisions.
func (c *ContentTx) LatestRevision(ctx context.Context) (int, error) {
	var n sql.NullInt64
	err := c.tx.QueryRowContext(ctx, `SELECT MAX(number) FROM revisions WHERE flat=?`, c.flat).Scan(&n)
	return int(n.Int64), err
}

// InsertRevision stores a revision and its files.
func (c *ContentTx) InsertRevision(ctx context.Context, r Revision, files []RevisionFile) error {
	authors, _ := json.Marshal(nonNil(r.Authors))
	guests, _ := json.Marshal(nonNil(r.GuestAuthors))
	changes, _ := json.Marshal(r.Changes)
	var rb any
	if r.RollbackOf > 0 {
		rb = r.RollbackOf
	}
	if _, err := c.tx.ExecContext(ctx, `INSERT INTO revisions(flat,number,created_at,kind,authors,message,rollback_of,pinned,changes,files,size,source,guest_authors,guests_more,guest_only) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.flat, r.Number, unix(r.CreatedAt), r.Kind, string(authors), r.Message, rb, boolInt(r.Pinned), string(changes), r.Files, r.Size, r.Source, string(guests), r.GuestsMore, boolInt(r.GuestOnly)); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := c.tx.ExecContext(ctx, `INSERT INTO revision_files(flat,number,path,hash,size) VALUES(?,?,?,?,?)`, c.flat, r.Number, f.Path, f.Hash, f.Size); err != nil {
			return err
		}
		if len(f.Lines()) == 0 {
			continue
		}
		ranges, _ := json.Marshal(f.Lines())
		if _, err := c.tx.ExecContext(ctx, `INSERT INTO revision_guest_lines(flat,number,path,ranges) VALUES(?,?,?,?)`, c.flat, r.Number, f.Path, string(ranges)); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RevisionFiles returns the files of revision n.
func (c *ContentTx) RevisionFiles(ctx context.Context, n int) ([]RevisionFile, error) {
	return queryRevisionFiles(ctx, c.tx, c.flat, n)
}

// RevisionFiles returns the files of revision n in one read.
func (s *Store) RevisionFiles(ctx context.Context, flat string, n int) ([]RevisionFile, error) {
	return queryRevisionFiles(ctx, s.db, flat, n)
}

func queryRevisionFiles(ctx context.Context, q querier, flat string, n int) ([]RevisionFile, error) {
	rows, err := q.QueryContext(ctx, `SELECT f.path,f.hash,f.size,g.ranges FROM revision_files f LEFT JOIN revision_guest_lines g ON g.flat=f.flat AND g.number=f.number AND g.path=f.path WHERE f.flat=? AND f.number=? ORDER BY f.path`, flat, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RevisionFile
	for rows.Next() {
		var f RevisionFile
		var ranges sql.NullString
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size, &ranges); err != nil {
			return nil, err
		}
		if ranges.Valid {
			var lines [][2]int
			if json.Unmarshal([]byte(ranges.String), &lines) == nil && len(lines) > 0 {
				f.GuestLines = &lines
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

const revisionCols = `number,created_at,kind,authors,message,rollback_of,pinned,changes,files,size,source,guest_authors,guests_more,guest_only,EXISTS(SELECT 1 FROM content_holds h WHERE h.flat=revisions.flat AND h.number=revisions.number)`

func scanRevision(flat string, sc interface{ Scan(...any) error }) (Revision, error) {
	r := Revision{Flat: flat}
	var created int64
	var authors, guests, changes string
	var rb sql.NullInt64
	var pinned, guestOnly int
	err := sc.Scan(&r.Number, &created, &r.Kind, &authors, &r.Message, &rb, &pinned, &changes, &r.Files, &r.Size, &r.Source, &guests, &r.GuestsMore, &guestOnly, &r.Held)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("revision: %w", ErrNotFound)
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt = fromUnix(created)
	r.RollbackOf = int(rb.Int64)
	r.Pinned = pinned == 1
	r.GuestOnly = guestOnly == 1
	_ = json.Unmarshal([]byte(authors), &r.Authors)
	_ = json.Unmarshal([]byte(guests), &r.GuestAuthors)
	_ = json.Unmarshal([]byte(changes), &r.Changes)
	return r, nil
}

// Revision returns revision n.
func (c *ContentTx) Revision(ctx context.Context, n int) (Revision, error) {
	return scanRevision(c.flat, c.tx.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM revisions WHERE flat=? AND number=?`, c.flat, n))
}

// Revision returns revision n of flat.
func (s *Store) Revision(ctx context.Context, flat string, n int) (Revision, error) {
	return scanRevision(flat, s.db.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM revisions WHERE flat=? AND number=?`, flat, n))
}

// ListRevisions returns revisions newest first, older than before when
// before > 0, at most limit.
func (s *Store) ListRevisions(ctx context.Context, flat string, before, limit int) ([]Revision, error) {
	q := `SELECT ` + revisionCols + ` FROM revisions WHERE flat=?`
	args := []any{flat}
	if before > 0 {
		q += ` AND number<?`
		args = append(args, before)
	}
	q += ` ORDER BY number DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(flat, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountPins returns the number of pinned revisions.
func (c *ContentTx) CountPins(ctx context.Context) (int, error) {
	var n int
	err := c.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE flat=? AND pinned=1`, c.flat).Scan(&n)
	return n, err
}

// CountPins returns the number of pinned revisions of flat.
func (s *Store) CountPins(ctx context.Context, flat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE flat=? AND pinned=1`, flat).Scan(&n)
	return n, err
}

// SetPinned marks revision n pinned or not.
func (c *ContentTx) SetPinned(ctx context.Context, n int, pinned bool) error {
	res, err := c.tx.ExecContext(ctx, `UPDATE revisions SET pinned=? WHERE flat=? AND number=?`, boolInt(pinned), c.flat, n)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return fmt.Errorf("revision %d: %w", n, ErrNotFound)
	}
	return nil
}

// DeleteRevision removes revision n and its file rows.
func (c *ContentTx) DeleteRevision(ctx context.Context, n int) error {
	if _, err := c.tx.ExecContext(ctx, `DELETE FROM revision_guest_lines WHERE flat=? AND number=?`, c.flat, n); err != nil {
		return err
	}
	if _, err := c.tx.ExecContext(ctx, `DELETE FROM revision_files WHERE flat=? AND number=?`, c.flat, n); err != nil {
		return err
	}
	_, err := c.tx.ExecContext(ctx, `DELETE FROM revisions WHERE flat=? AND number=?`, c.flat, n)
	return err
}

// ReferencedHashes returns every hash referenced by flat's head or
// revisions.
func (s *Store) ReferencedHashes(ctx context.Context, flat string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM content_head WHERE flat=? UNION SELECT hash FROM revision_files WHERE flat=? UNION SELECT hash FROM content_sources WHERE flat=?`, flat, flat, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// ErrCheckpointBusy reports a checkpoint that could not complete.
var ErrCheckpointBusy = errors.New("checkpoint did not complete")

// Checkpoint runs a WAL checkpoint of the given mode (PASSIVE, FULL). For
// FULL it fails with ErrCheckpointBusy unless every frame of the WAL was
// written to the database file.
func (s *Store) Checkpoint(ctx context.Context, mode string) error {
	switch mode {
	case "PASSIVE", "FULL":
	default:
		return fmt.Errorf("checkpoint mode %q", mode)
	}
	var busy, logFrames, done int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(`+mode+`)`).Scan(&busy, &logFrames, &done); err != nil {
		return err
	}
	if mode == "FULL" && (busy != 0 || (logFrames >= 0 && done < logFrames)) {
		return fmt.Errorf("%w: busy=%d log=%d checkpointed=%d", ErrCheckpointBusy, busy, logFrames, done)
	}
	return nil
}

// --- document sources and holds ---

// PutSource stores the non-document files of a content source; an existing
// source keeps its rows.
func (s *Store) PutSource(ctx context.Context, flat, source string, files []RevisionFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO content_sources(flat,source,path,hash,size) VALUES(?,?,?,?,?)`, flat, source, f.Path, f.Hash, f.Size); err != nil {
			return err
		}
	}
	// A source without files still needs a row to be known.
	if len(files) == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO content_sources(flat,source,path,hash,size) VALUES(?,?,'',?,0)`, flat, source, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SourceFiles returns the files of a content source; ok is false when the
// source is unknown.
func (s *Store) SourceFiles(ctx context.Context, flat, source string) (files []RevisionFile, ok bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path,hash,size FROM content_sources WHERE flat=? AND source=? ORDER BY path`, flat, source)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var f RevisionFile
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size); err != nil {
			return nil, false, err
		}
		ok = true
		if f.Path != "" {
			files = append(files, f)
		}
	}
	return files, ok, rows.Err()
}

// DeleteUnusedSources removes the sources of flat that no revision names
// and keep does not list.
func (s *Store) DeleteUnusedSources(ctx context.Context, flat string, keep map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT source FROM content_sources c WHERE flat=? AND NOT EXISTS(SELECT 1 FROM revisions r WHERE r.flat=c.flat AND r.source=c.source)`, flat)
	if err != nil {
		return err
	}
	var drop []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			rows.Close()
			return err
		}
		if !keep[src] {
			drop = append(drop, src)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, src := range drop {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM content_sources WHERE flat=? AND source=?`, flat, src); err != nil {
			return err
		}
	}
	return nil
}

// Hold is a rollback in progress protecting its target revision.
type Hold struct {
	Op     string
	Number int
}

// AddHold protects revision n for the rollback op.
func (c *ContentTx) AddHold(ctx context.Context, op string, n int) error {
	_, err := c.tx.ExecContext(ctx, `INSERT INTO content_holds(flat,op,number) VALUES(?,?,?)`, c.flat, op, n)
	return err
}

// DeleteHold ends the hold of the rollback op.
func (c *ContentTx) DeleteHold(ctx context.Context, op string) error {
	_, err := c.tx.ExecContext(ctx, `DELETE FROM content_holds WHERE flat=? AND op=?`, c.flat, op)
	return err
}

// Holds lists the holds of flat.
func (s *Store) Holds(ctx context.Context, flat string) ([]Hold, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT op,number FROM content_holds WHERE flat=? ORDER BY op`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hold
	for rows.Next() {
		var h Hold
		if err := rows.Scan(&h.Op, &h.Number); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
