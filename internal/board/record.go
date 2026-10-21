package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/gosuda/flats/internal/board/model"
)

// recordSpec describes one outbox entry (5.3 record).
type recordSpec struct {
	kind       string // created, person, agent, rollback, import
	forced     bool
	all        bool // serialize every bucket, not only the dirty ones
	authors    []string
	message    string
	rollbackOf int
	op         string
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// assetExt is the file extension of an asset path for a MIME type.
func assetExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return "bin"
}

func extMime(ext string) string {
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	}
	return ""
}

// files is the current file list (5.2): the bucket files, board.json and
// the assets alive elements reference. Buckets that are not dirty keep
// their hash from meta.newest unless all is set. fresh holds the bytes of
// every bucket file whose hash differs from meta.newest.
func (t *txn) files(all bool) (files []File, fresh map[string][]byte) {
	st := t.in.st
	prev := make(map[string]File, len(t.newest))
	for _, f := range t.newest {
		prev[f.Path] = f
	}
	fresh = map[string][]byte{}
	for b := 0; b <= model.BoardBucket; b++ {
		path := model.BucketPath(b)
		p, had := prev[path]
		if !all && t.dirty[b] == 0 && had {
			files = append(files, p)
			continue
		}
		if !all && t.dirty[b] == 0 && b != model.BoardBucket {
			continue
		}
		data, ok := st.BucketFile(b)
		if !ok {
			continue
		}
		f := File{Path: path, Hash: sha256Hex(data), Size: int64(len(data))}
		files = append(files, f)
		if !had || p.Hash != f.Hash {
			fresh[f.Hash] = data
		}
	}
	for _, a := range t.in.assets.live(t.assets) {
		files = append(files, File{Path: "assets/" + a.Hash + "." + assetExt(a.Mime), Hash: a.Hash, Size: a.Bytes})
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return files, fresh
}

// record appends an outbox entry for the current state (5.3), or, when the
// entry is not forced and equals meta.newest, only clears the dirty marks.
// It returns the entry's seq, or 0 when it recorded none.
func (t *txn) record(rs recordSpec) (int64, error) {
	files, fresh := t.files(rs.all)
	guests := t.guestNames()
	clear := func() error {
		t.dirty = map[int]int64{}
		for k, g := range t.groups {
			g.Dirty, g.Names = false, nil
			t.groups[k] = g
		}
		if _, err := t.tx.ExecContext(t.ctx, `UPDATE buckets SET dirty_seq = NULL WHERE dirty_seq IS NOT NULL`); err != nil {
			return err
		}
		_, err := t.tx.ExecContext(t.ctx, `UPDATE groups SET dirty = 0, names = '[]' WHERE dirty <> 0 OR names <> '[]'`)
		return err
	}
	if !rs.forced && slices.Equal(files, t.newest) {
		return 0, clear()
	}
	snaps := make([]string, 0, len(fresh))
	for _, h := range sortedKeys(fresh) {
		data := fresh[h]
		snaps = append(snaps, h)
		cur := t.in.sd.snaps[h]
		add := t.snapAdd[h]
		if cur.refs+add.refs <= 0 {
			if _, err := t.tx.ExecContext(t.ctx, `INSERT OR REPLACE INTO snapshots(hash, data, refs) VALUES(?, ?, 1)`, h, data); err != nil {
				return 0, err
			}
			t.snapBytes += int64(len(data))
		} else if _, err := t.tx.ExecContext(t.ctx, `UPDATE snapshots SET refs = refs + 1 WHERE hash = ?`, h); err != nil {
			return 0, err
		}
		t.snapAdd[h] = snapRef{size: int64(len(data)), refs: add.refs + 1}
	}
	authors := rs.authors
	if len(authors) > 32 {
		authors = authors[:32]
	}
	for i, a := range authors {
		if model.TextLen(a) > 200 {
			authors[i] = string([]rune(a)[:200])
		}
	}
	if authors == nil {
		authors = []string{}
	}
	msg := rs.message
	if model.TextLen(msg) > 500 {
		msg = string([]rune(msg)[:500])
	}
	more := max(0, len(guests)-maxCreditedGuests)
	guestsJSON, _ := json.Marshal(nonNil(guests[:len(guests)-more]))
	authorsJSON, _ := json.Marshal(authors)
	filesJSON, _ := json.Marshal(files)
	snapsJSON, _ := json.Marshal(snaps)
	now := t.in.now().Unix()
	var rollbackOf, op any
	if rs.rollbackOf > 0 {
		rollbackOf = rs.rollbackOf
	}
	if rs.op != "" {
		op = rs.op
	}
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO outbox(created_at, kind, authors, message, rollback_of, op, files, snaps, guests, guests_more)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, now, rs.kind, string(authorsJSON), msg, rollbackOf, op, string(filesJSON), string(snapsJSON), string(guestsJSON), more)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := setMeta(t.ctx, t.tx, "newest", string(filesJSON)); err != nil {
		return 0, err
	}
	if err := setMeta(t.ctx, t.tx, "last_record", now); err != nil {
		return 0, err
	}
	t.newest, t.newestBytes, t.lastRecord = files, int64(len(filesJSON)), now
	t.outbox = append(slices.Clone(t.outbox), seq)
	t.outboxAdded = true
	return seq, clear()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// maxCreditedGuests is how many guest names an entry lists (spec rule 25).
const maxCreditedGuests = 5

// OutboxEntry is one unimported outbox entry (5.3 Import).
type OutboxEntry struct {
	Seq        int64
	CreatedAt  int64 // unix seconds
	Kind       string
	Authors    []string
	Message    string
	RollbackOf int
	Op         string
	Files      []File
	// Snapshots are the bucket files this entry added. Their bytes come
	// from SnapshotData until the entry is acknowledged; every other file
	// of Files is already a blob, except assets, which uploads stored.
	Snapshots []string
	// Guests are up to five guest names credited by this entry, and
	// GuestsMore counts the rest. Guest names never go into Authors.
	Guests     []string
	GuestsMore int
}

// Outbox returns up to limit entries after seq after, oldest first.
func (in *Instance) Outbox(ctx context.Context, after int64, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []OutboxEntry
	err := in.do(ctx, func() error {
		rows, err := in.db.db.QueryContext(context.Background(), `SELECT seq, created_at, kind, authors, message, rollback_of, op, files, snaps, guests, guests_more
			FROM outbox WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e OutboxEntry
			var authors, files, snaps, guests string
			var rollbackOf sql.NullInt64
			var op sql.NullString
			if err := rows.Scan(&e.Seq, &e.CreatedAt, &e.Kind, &authors, &e.Message, &rollbackOf, &op, &files, &snaps, &guests, &e.GuestsMore); err != nil {
				return err
			}
			e.RollbackOf, e.Op = int(rollbackOf.Int64), op.String
			for _, x := range []struct {
				s   string
				dst any
			}{{authors, &e.Authors}, {files, &e.Files}, {snaps, &e.Snapshots}, {guests, &e.Guests}} {
				if err := json.Unmarshal([]byte(x.s), x.dst); err != nil {
					return fmt.Errorf("outbox %d: %w", e.Seq, err)
				}
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// SnapshotData returns the bytes of a bucket file an unacknowledged entry
// added.
func (in *Instance) SnapshotData(ctx context.Context, hash string) ([]byte, error) {
	var data []byte
	err := in.do(ctx, func() error {
		err := in.db.db.QueryRowContext(context.Background(), `SELECT data FROM snapshots WHERE hash = ?`, hash).Scan(&data)
		if err == sql.ErrNoRows {
			return errorf(CategoryNotFound, "no snapshot %s", hash)
		}
		return err
	})
	return data, err
}

// Acknowledge deletes the outbox entries up to seq upTo after core imported
// them (5.3 Import step 4), lowers the snapshot references and adds the
// bytes the import stored as blobs to the quota's "stored since the
// measurement" counter.
func (in *Instance) Acknowledge(ctx context.Context, upTo, stored int64) error {
	return in.do(ctx, func() error {
		t, err := in.begin()
		if err != nil {
			return err
		}
		if err := in.acknowledge(t, upTo); err != nil {
			t.rollback()
			return err
		}
		in.meas.add(stored, true) // core imports under the flat lock
		return t.commit()
	})
}

func (in *Instance) acknowledge(t *txn, upTo int64) error {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT snaps FROM outbox WHERE seq <= ?`, upTo)
	if err != nil {
		return err
	}
	drop := map[string]int64{}
	for rows.Next() {
		var s string
		var hashes []string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(s), &hashes); err != nil {
			rows.Close()
			return err
		}
		for _, h := range hashes {
			drop[h]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range sortedKeys(drop) {
		n := drop[h]
		cur := in.sd.snaps[h]
		add := t.snapAdd[h]
		left := cur.refs + add.refs - n
		size := max(cur.size, add.size)
		if left <= 0 {
			if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM snapshots WHERE hash = ?`, h); err != nil {
				return err
			}
			t.snapBytes -= size
		} else if _, err := t.tx.ExecContext(t.ctx, `UPDATE snapshots SET refs = ? WHERE hash = ?`, left, h); err != nil {
			return err
		}
		t.snapAdd[h] = snapRef{size: size, refs: add.refs - n}
	}
	if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM outbox WHERE seq <= ?`, upTo); err != nil {
		return err
	}
	var keep []int64
	for _, s := range t.outbox {
		if s > upTo {
			keep = append(keep, s)
		}
	}
	t.outbox = keep
	now := in.now().Unix()
	for id, op := range in.sd.ops {
		if op.importedAt == 0 && op.outboxSeq > 0 && op.outboxSeq <= upTo {
			cp := *op
			cp.importedAt = now
			t.newOps[id] = &cp
		}
	}
	_, err = t.tx.ExecContext(t.ctx, `UPDATE ops SET imported_at = ? WHERE imported_at IS NULL AND outbox_seq > 0 AND outbox_seq <= ?`, now, upTo)
	return err
}

// putOp records an operation's final result in ops (5.1). An op without an
// outbox entry counts as imported now.
func (t *txn) putOp(id, kind, hash string, outboxSeq int64, result any) error {
	res, err := json.Marshal(result)
	if err != nil {
		return err
	}
	now := t.in.now().Unix()
	op := &opRec{kind: kind, hash: hash, outboxSeq: outboxSeq, result: res, at: now}
	var imported any
	if outboxSeq == 0 {
		op.importedAt = now
		imported = now
	}
	t.newOps[id] = op
	_, err = t.tx.ExecContext(t.ctx, `INSERT OR REPLACE INTO ops(id, kind, request_hash, outbox_seq, result, at, imported_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		id, kind, hash, outboxSeq, string(res), now, imported)
	return err
}

// OpResult is the stored outcome of an operation id: its kind, the outbox
// entry it recorded (0: none) and its JSON result.
type OpResult struct {
	Kind      string
	OutboxSeq int64
	Result    json.RawMessage
}

// Op returns the stored result of operation id, for repeated requests and
// for settling rollback holds at startup (4.11 "Settling holds").
func (in *Instance) Op(ctx context.Context, id string) (OpResult, bool, error) {
	var out OpResult
	var ok bool
	err := in.do(ctx, func() error {
		if op := in.sd.ops[id]; op != nil {
			out, ok = OpResult{op.kind, op.outboxSeq, op.result}, true
		}
		return nil
	})
	return out, ok, err
}
