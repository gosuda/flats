package notebook

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// File identifies one canonical content file or live image in a revision.
type File struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// OutboxEntry is a durable revision boundary awaiting host import.
type OutboxEntry struct {
	Seq        int64
	Kind       string
	Message    string
	OpID       string
	Authors    []string
	RollbackOf int
	Files      []File
	NewHashes  []string
	CreatedAt  int64
}

// The initial schema has no timestamp column. Keep it with the private
// snapshot-reference payload, not in the revision's content files.
type outboxHashes struct {
	CreatedAt int64    `json:"created_at"`
	Hashes    []string `json:"hashes"`
}

type contentSource struct {
	path string
	page ID
	ids  []ID
	file *File
}

func contentHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func validateContentHash(hash string) error {
	if len(hash) != sha256.Size*2 || strings.ToLower(hash) != hash {
		return fmt.Errorf("invalid notebook content hash")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("invalid notebook content hash: %w", err)
	}
	return nil
}

func contentBucket(id ID) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:1])
}

// contentSources holds identities, not a second mutable copy of notebook data.
// A caller encodes one bucket at a time from its transaction.
func contentSources(ctx context.Context, tx *sql.Tx) ([]contentSource, error) {
	sources := map[string]*contentSource{"notebook.json": {path: "notebook.json"}}
	add := func(path string, page, id ID) {
		s := sources[path]
		if s == nil {
			s = &contentSource{path: path, page: page}
			sources[path] = s
		}
		s.ids = append(s.ids, id)
	}
	queries := []struct {
		query string
		kind  Kind
	}{
		{`SELECT p.id,p.id,'' FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1 ORDER BY p.id`, PageKind},
		{`SELECT b.id,b.page,'' FROM blocks b JOIN items i ON i.id=b.id JOIN items p ON p.id=b.page WHERE i.alive=1 AND p.alive=1 ORDER BY b.id`, BlockKind},
		{`SELECT d.id,b.page,'' FROM databases d JOIN items i ON i.id=d.id JOIN blocks b ON b.id=d.block JOIN items bi ON bi.id=b.id JOIN items p ON p.id=b.page WHERE i.alive=1 AND bi.alive=1 AND p.alive=1 ORDER BY d.id`, DatabaseKind},
		{`SELECT r.id,b.page,r.database_id FROM rows r JOIN items i ON i.id=r.id JOIN databases d ON d.id=r.database_id JOIN items di ON di.id=d.id JOIN blocks b ON b.id=d.block JOIN items bi ON bi.id=b.id JOIN items p ON p.id=b.page WHERE i.alive=1 AND di.alive=1 AND bi.alive=1 AND p.alive=1 ORDER BY r.id`, RowKind},
	}
	for _, q := range queries {
		rows, err := tx.QueryContext(ctx, q.query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, page, database ID
			if err := rows.Scan(&id, &page, &database); err != nil {
				rows.Close()
				return nil, err
			}
			if err := id.ValidateKind(q.kind); err != nil {
				rows.Close()
				return nil, err
			}
			if err := page.ValidateKind(PageKind); err != nil {
				rows.Close()
				return nil, err
			}
			prefix := "pages/" + string(page) + "/"
			switch q.kind {
			case PageKind:
				add(prefix+"page.json", page, id)
			case BlockKind:
				add(prefix+"blocks/"+contentBucket(id)+".json", page, id)
			case DatabaseKind:
				add(prefix+"databases/"+string(id)+"/schema.json", page, id)
			case RowKind:
				if err := database.ValidateKind(DatabaseKind); err != nil {
					rows.Close()
					return nil, err
				}
				add(prefix+"databases/"+string(database)+"/rows/"+contentBucket(id)+".json", page, id)
			}
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT hash,mime,bytes FROM assets WHERE live_refs>0 ORDER BY hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash, mime string
		var size int64
		if err := rows.Scan(&hash, &mime, &size); err != nil {
			return nil, err
		}
		if err := validateContentHash(hash); err != nil {
			return nil, err
		}
		ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}[mime]
		if ext == "" || size < 0 {
			return nil, fmt.Errorf("invalid notebook asset metadata")
		}
		f := File{Path: "assets/" + hash + "." + ext, Hash: hash, Size: size}
		sources[f.Path] = &contentSource{path: f.Path, file: &f}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]contentSource, 0, len(sources))
	for _, source := range sources {
		out = append(out, *source)
	}
	slices.SortFunc(out, func(a, b contentSource) int { return strings.Compare(a.path, b.path) })
	return out, nil
}

func contentItem(item Item) (map[string]json.RawMessage, error) {
	data, err := CanonicalJSON(item.Data)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	id, err := json.Marshal(item.ID())
	if err != nil {
		return nil, err
	}
	fields["id"] = id
	return fields, nil
}

func encodeContentSource(ctx context.Context, tx *sql.Tx, source contentSource) ([]byte, error) {
	if source.path == "notebook.json" {
		return []byte(`{"format":1}`), nil
	}
	if source.file != nil {
		return nil, fmt.Errorf("asset bytes are owned by the blob store")
	}
	if strings.HasSuffix(source.path, "/schema.json") {
		return encodeContentSchema(ctx, tx, source.ids[0])
	}
	items := make([]map[string]json.RawMessage, 0, len(source.ids))
	for _, id := range source.ids {
		record, err := loadRecord(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		fields, err := contentItem(record.Item)
		if err != nil {
			return nil, err
		}
		items = append(items, fields)
	}
	if strings.HasSuffix(source.path, "/page.json") {
		return CanonicalJSON(items[0])
	}
	return CanonicalJSON(items)
}

func encodeContentSchema(ctx context.Context, tx *sql.Tx, id ID) ([]byte, error) {
	record, err := loadRecord(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	fields, err := contentItem(record.Item)
	if err != nil {
		return nil, err
	}
	for _, child := range []struct {
		key   string
		query string
	}{
		{"properties", `SELECT p.id FROM properties p JOIN items i ON i.id=p.id WHERE p.database_id=? AND i.alive=1 ORDER BY p.id`},
		{"options", `SELECT o.id FROM options o JOIN items i ON i.id=o.id JOIN properties p ON p.id=o.property_id JOIN items pi ON pi.id=p.id WHERE p.database_id=? AND i.alive=1 AND pi.alive=1 ORDER BY o.id`},
		{"views", `SELECT v.id FROM views v JOIN items i ON i.id=v.id WHERE v.database_id=? AND i.alive=1 ORDER BY v.id`},
	} {
		rows, err := tx.QueryContext(ctx, child.query, id)
		if err != nil {
			return nil, err
		}
		var ids []ID
		for rows.Next() {
			var childID ID
			if err := rows.Scan(&childID); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, childID)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		values := make([]map[string]json.RawMessage, 0, len(ids))
		for _, childID := range ids {
			record, err := loadRecord(ctx, tx, childID)
			if err != nil {
				return nil, err
			}
			value, err := contentItem(record.Item)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		fields[child.key], err = CanonicalJSON(values)
		if err != nil {
			return nil, err
		}
	}
	return CanonicalJSON(fields)
}

func currentContentFiles(ctx context.Context, tx *sql.Tx) ([]File, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path,hash,bytes FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func newestContentFiles(ctx context.Context, tx *sql.Tx) ([]File, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT files FROM meta WHERE singleton=1`).Scan(&raw); err != nil {
		return nil, err
	}
	var files []File
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return nil, fmt.Errorf("notebook newest file list: %w", err)
	}
	return files, nil
}

func syncContentFiles(ctx context.Context, tx *sql.Tx, seq Sequence) error {
	if seq == 0 {
		return fmt.Errorf("content synchronization requires a positive sequence")
	}
	sources, err := contentSources(ctx, tx)
	if err != nil {
		return err
	}
	newest, err := newestContentFiles(ctx, tx)
	if err != nil {
		return err
	}
	previous := make(map[string]File, len(newest))
	for _, f := range newest {
		previous[f.Path] = f
	}
	current, err := currentContentFiles(ctx, tx)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(sources))
	for _, source := range sources {
		f := File{Path: source.path}
		var dirty Sequence
		if source.file != nil {
			f = *source.file
		} else {
			data, err := encodeContentSource(ctx, tx, source)
			if err != nil {
				return fmt.Errorf("encode %s: %w", source.path, err)
			}
			f.Hash, f.Size = contentHash(data), int64(len(data))
			if previous[f.Path] != f {
				dirty = seq
			}
		}
		var page any
		if source.page != "" {
			page = source.page
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(path,page,bytes,dirty_seq,hash) VALUES(?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET page=excluded.page,bytes=excluded.bytes,dirty_seq=excluded.dirty_seq,hash=excluded.hash WHERE files.page IS NOT excluded.page OR files.bytes<>excluded.bytes OR files.dirty_seq<>excluded.dirty_seq OR files.hash IS NOT excluded.hash`, f.Path, page, f.Size, strconv.FormatUint(uint64(dirty), 10), f.Hash); err != nil {
			return err
		}
		present[f.Path] = true
	}
	for _, f := range current {
		if !present[f.Path] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE path=?`, f.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeContentFileList(w io.Writer, files []File) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	for i, f := range files {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		data, err := CanonicalJSON(f)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]")
	return err
}

func recordOutbox(ctx context.Context, tx *sql.Tx, kind, message, opID string, authors []string, now time.Time, force bool) (int64, error) {
	files, err := currentContentFiles(ctx, tx)
	if err != nil {
		return 0, err
	}
	newest, err := newestContentFiles(ctx, tx)
	if err != nil {
		return 0, err
	}
	clear := func() error {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET dirty_seq='0' WHERE dirty_seq<>'0'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE groups SET dirty=0 WHERE dirty<>0`)
		return err
	}
	if !force && slices.Equal(files, newest) {
		return 0, clear()
	}
	oldHashes := make(map[string]bool, len(newest))
	for _, f := range newest {
		oldHashes[f.Hash] = true
	}
	sources, err := contentSources(ctx, tx)
	if err != nil {
		return 0, err
	}
	byPath := make(map[string]contentSource, len(sources))
	for _, s := range sources {
		byPath[s.path] = s
	}
	hashes := []string{}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "assets/") || oldHashes[f.Hash] {
			continue
		}
		source, ok := byPath[f.Path]
		if !ok {
			return 0, fmt.Errorf("missing content source %s", f.Path)
		}
		data, err := encodeContentSource(ctx, tx, source)
		if err != nil {
			return 0, err
		}
		if contentHash(data) != f.Hash || int64(len(data)) != f.Size {
			return 0, fmt.Errorf("content file %s was not synchronized", f.Path)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO snapshots(hash,data,refs) VALUES(?,?,1) ON CONFLICT(hash) DO UPDATE SET refs=refs+1`, f.Hash, data); err != nil {
			return 0, err
		}
		hashes = append(hashes, f.Hash)
		oldHashes[f.Hash] = true
	}
	slices.Sort(hashes)
	if authors == nil {
		authors = []string{}
	}
	authorData, err := CanonicalJSON(authors)
	if err != nil {
		return 0, err
	}
	hashData, err := CanonicalJSON(outboxHashes{CreatedAt: now.Unix(), Hashes: hashes})
	if err != nil {
		return 0, err
	}
	var op any
	if opID != "" {
		op = opID
	}
	// Build the persistent JSON in SQLite, without another full-list Go
	// string beside the metadata vectors. Paths and hashes are canonical ASCII.
	result, err := tx.ExecContext(ctx, `INSERT INTO outbox(kind,authors,message,op_id,files,hashes)
		VALUES(?,?,?,?,(SELECT json_group_array(json_object('hash',hash,'path',path,'size',bytes)) FROM (SELECT path,hash,bytes FROM files ORDER BY path)),?)`, kind, string(authorData), message, op, string(hashData))
	if err != nil {
		return 0, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET files=(SELECT files FROM outbox WHERE seq=?) WHERE singleton=1`, seq); err != nil {
		return 0, err
	}
	return seq, clear()
}

// Outbox returns pending entries in import order after the given cursor.
func (in *Instance) Outbox(ctx context.Context, after int64, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := in.readers.QueryContext(ctx, `SELECT seq,kind,authors,message,rollback_target,op_id,files,hashes FROM outbox WHERE seq>? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []OutboxEntry{}
	for rows.Next() {
		var entry OutboxEntry
		var authors, files, hashes string
		var rollback sql.NullInt64
		var op sql.NullString
		if err := rows.Scan(&entry.Seq, &entry.Kind, &authors, &entry.Message, &rollback, &op, &files, &hashes); err != nil {
			return nil, err
		}
		entry.RollbackOf, entry.OpID = int(rollback.Int64), op.String
		var refs outboxHashes
		for _, field := range []struct {
			raw string
			dst any
		}{{authors, &entry.Authors}, {files, &entry.Files}, {hashes, &refs}} {
			if err := json.Unmarshal([]byte(field.raw), field.dst); err != nil {
				return nil, fmt.Errorf("notebook outbox %d: %w", entry.Seq, err)
			}
		}
		entry.NewHashes, entry.CreatedAt = refs.Hashes, refs.CreatedAt
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// SnapshotData returns bytes retained until their outbox entries are imported.
func (in *Instance) SnapshotData(ctx context.Context, hash string) ([]byte, error) {
	if err := validateContentHash(hash); err != nil {
		return nil, err
	}
	var data []byte
	if err := in.readers.QueryRowContext(ctx, `SELECT data FROM snapshots WHERE hash=? AND refs>0`, hash).Scan(&data); err != nil {
		return nil, fmt.Errorf("notebook snapshot %s: %w", hash, err)
	}
	return data, nil
}

// Acknowledge releases content only after the host has committed the import.
func (in *Instance) Acknowledge(ctx context.Context, through, stored int64) error {
	if through < 0 || stored < 0 {
		return fmt.Errorf("invalid notebook acknowledgement")
	}
	return in.write(ctx, func(tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE seq<=?`, through).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 && stored > 0 {
			in.measure.add(stored)
		}
		return acknowledgeOutbox(ctx, tx, through, in.now())
	})
}

func acknowledgeOutbox(ctx context.Context, tx *sql.Tx, through int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT hashes FROM outbox WHERE seq<=? ORDER BY seq`, through)
	if err != nil {
		return err
	}
	drop := map[string]int64{}
	for rows.Next() {
		var raw string
		var refs outboxHashes
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &refs); err != nil {
			rows.Close()
			return err
		}
		for _, hash := range refs.Hashes {
			drop[hash]++
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
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
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET import_status='imported',retain_until=? WHERE outbox_seq IS NOT NULL AND outbox_seq<=? COLLATE notebook_u64 AND import_status<>'imported'`, now.Add(24*time.Hour).Unix(), strconv.FormatInt(through, 10)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE seq<=?`, through)
	return err
}
