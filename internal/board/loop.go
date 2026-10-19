package board

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// item is one unit of apply-loop work: a person batch, which joins the
// group commit, or any other function.
type item struct {
	fn    func() error
	batch *batchReq
	done  chan error
}

// editGroup is a groups row: one editor's open edit group (5.3).
type editGroup struct {
	Label     string
	StartedAt int64
	LastAt    int64
	Dirty     bool
}

// deltaRec is a delta log entry; body is the delta JSON without from.
type deltaRec struct {
	seq     int64
	session string
	n       int64
	body    []byte
	at      int64
}

type snapRef struct{ size, refs int64 }

// opRec is an ops row: an agent call's, import's or rollback's final result.
type opRec struct {
	kind       string
	hash       string
	outboxSeq  int64
	result     json.RawMessage
	at         int64
	importedAt int64
}

// side is the loop's view of the board.db tables the engine does not own.
type side struct {
	groups       map[string]editGroup
	dirty        map[int]int64 // bucket -> seq that dirtied it
	bucketBytes  map[int]int64
	newest       []File
	newestBytes  int64
	snaps        map[string]snapRef
	snapBytes    int64
	outbox       []int64 // seqs of unacknowledged entries
	shareN       int64
	walPending   bool
	walPersisted bool
	lastRecord   int64
	deltas       []deltaRec
	deltaBytes   int64
	ops          map[string]*opRec
}

// txn is one board.db transaction of the apply loop with the matching
// engine savepoint and a working copy of the side tables. Nothing reaches
// the in-memory side or other clients before commit.
type txn struct {
	in   *Instance
	ctx  context.Context
	tx   *sql.Tx
	mark engine.Mark

	groups      map[string]editGroup
	dirty       map[int]int64
	bucketBytes map[int]int64
	newest      []File
	newestBytes int64
	snapAdd     map[string]snapRef // refs added (negative: dropped) and sizes
	snapBytes   int64
	outbox      []int64
	shareN      int64
	lastRecord  int64
	newDeltas   []deltaRec
	trimDeltas  int
	newOps      map[string]*opRec
	dropOps     []string
	assets      map[string]*engine.Asset // nil value: row deleted
	outboxAdded bool
	pending     int64 // blob bytes admitBlobs is about to store (5.4)

	after  []func()
	onFail []func()
}

func (in *Instance) begin() (*txn, error) {
	ctx := context.Background()
	tx, err := in.db.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	sd := in.sd
	t := &txn{
		in: in, ctx: ctx, tx: tx, mark: in.st.Savepoint(),
		groups: maps.Clone(sd.groups), dirty: maps.Clone(sd.dirty), bucketBytes: maps.Clone(sd.bucketBytes),
		newest: sd.newest, newestBytes: sd.newestBytes, snapAdd: map[string]snapRef{}, snapBytes: sd.snapBytes,
		outbox: sd.outbox, shareN: sd.shareN, lastRecord: sd.lastRecord,
		newOps: map[string]*opRec{}, assets: map[string]*engine.Asset{},
	}
	if sd.walPending != sd.walPersisted {
		v := ""
		if sd.walPending {
			v = "1"
		}
		if err := setMeta(ctx, tx, "wal_truncate", v); err != nil {
			t.rollback()
			return nil, err
		}
	}
	return t, nil
}

func (t *txn) rollback() {
	_ = t.tx.Rollback()
	t.in.st.RollbackTo(t.mark)
	t.in.dropStaleSnap()
	for _, f := range t.onFail {
		f()
	}
}

// commit makes the transaction durable, adopts its side changes and runs
// the after functions (deltas, acks). On failure everything is rolled back.
func (t *txn) commit() error {
	in := t.in
	if in.hook != nil {
		in.hook("commit")
	}
	if err := t.tx.Commit(); err != nil {
		_, _ = in.db.db.ExecContext(t.ctx, `ROLLBACK`)
		t.rollback()
		return err
	}
	if in.hook != nil {
		in.hook("committed")
	}
	in.st.Release(t.mark)
	in.dropStaleSnap()
	sd := in.sd
	sd.walPersisted = sd.walPending
	sd.groups, sd.dirty, sd.bucketBytes = t.groups, t.dirty, t.bucketBytes
	sd.newest, sd.newestBytes, sd.outbox, sd.shareN, sd.lastRecord = t.newest, t.newestBytes, t.outbox, t.shareN, t.lastRecord
	for h, a := range t.snapAdd {
		cur := sd.snaps[h]
		cur.refs += a.refs
		if a.size != 0 {
			cur.size = a.size
		}
		if cur.refs <= 0 {
			delete(sd.snaps, h)
		} else {
			sd.snaps[h] = cur
		}
	}
	sd.snapBytes = t.snapBytes
	if t.trimDeltas > 0 {
		for _, d := range sd.deltas[:t.trimDeltas] {
			sd.deltaBytes -= int64(len(d.body))
		}
		sd.deltas = slices.Clone(sd.deltas[t.trimDeltas:])
	}
	for _, d := range t.newDeltas {
		sd.deltas = append(sd.deltas, d)
		sd.deltaBytes += int64(len(d.body))
	}
	for id, op := range t.newOps {
		sd.ops[id] = op
	}
	for _, id := range t.dropOps {
		delete(sd.ops, id)
	}
	in.assets.apply(t.assets)
	in.walCheck()
	for _, f := range t.after {
		f()
	}
	if t.outboxAdded {
		in.b.OutboxReady()
	}
	return nil
}

// persist writes the rows c names and marks the changed buckets dirty at
// seq (5.3 "A person's batch": dirty_seq).
func (t *txn) persist(c engine.Changed, seq int64) error {
	st := t.in.st
	if err := persist(t.ctx, t.tx, st, c); err != nil {
		return err
	}
	for _, h := range c.Assets {
		if a, ok := st.AssetRow(h); ok {
			t.assets[h] = &a
		} else {
			t.assets[h] = nil
		}
	}
	for _, b := range c.Buckets {
		data, _ := st.BucketFile(b)
		t.bucketBytes[b] = int64(len(data))
		if t.dirty[b] == 0 {
			t.dirty[b] = max(seq, 1)
		}
		if _, err := t.tx.ExecContext(t.ctx, `INSERT INTO buckets(bucket, dirty_seq, bytes) VALUES(?, ?, ?)
			ON CONFLICT(bucket) DO UPDATE SET dirty_seq = excluded.dirty_seq, bytes = excluded.bytes`, b, t.dirty[b], len(data)); err != nil {
			return err
		}
	}
	return nil
}

// addDelta appends a committed delta to the delta log and trims the log to
// its limits (4.8 retention).
func (t *txn) addDelta(d *model.Delta, session string, n int64) (deltaRec, error) {
	cp := *d
	cp.From = ""
	body, err := json.Marshal(cp)
	if err != nil {
		return deltaRec{}, err
	}
	by, _ := json.Marshal(d.By)
	now := t.in.now().Unix()
	rec := deltaRec{seq: d.Seq, session: session, n: n, body: body, at: now}
	if _, err := t.tx.ExecContext(t.ctx, `INSERT INTO deltas(seq, from_session, n, by, changes, at) VALUES(?, ?, ?, ?, ?, ?)`,
		d.Seq, session, n, string(by), string(body), now); err != nil {
		return deltaRec{}, err
	}
	t.newDeltas = append(t.newDeltas, rec)
	return rec, t.trimDeltaLog(now)
}

// trimDeltaLog drops the oldest delta log entries beyond 20,000 entries,
// 16 MiB or one hour.
func (t *txn) trimDeltaLog(now int64) error {
	in, sd := t.in, t.in.sd
	count := len(sd.deltas) + len(t.newDeltas)
	bytes := sd.deltaBytes
	for _, d := range t.newDeltas {
		bytes += int64(len(d.body))
	}
	trim := t.trimDeltas
	for trim < len(sd.deltas) {
		d := sd.deltas[trim]
		if count-trim <= in.deltaMaxEntries && bytes <= in.deltaMaxBytes && d.at > now-in.deltaMaxAge {
			break
		}
		bytes -= int64(len(d.body))
		trim++
	}
	if trim == t.trimDeltas {
		return nil
	}
	t.trimDeltas = trim
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM deltas WHERE seq <= ?`, sd.deltas[trim-1].seq)
	return err
}

// touchGroup updates the editor's group for a person batch (5.3).
func (t *txn) touchGroup(key, label string) error {
	now := t.in.now().Unix()
	g, ok := t.groups[key]
	if !ok {
		g.StartedAt = now
	}
	g.Label, g.LastAt, g.Dirty = label, now, true
	t.groups[key] = g
	_, err := t.tx.ExecContext(t.ctx, `INSERT OR REPLACE INTO groups(editor, label, started_at, last_at, dirty) VALUES(?, ?, ?, ?, 1)`,
		key, label, g.StartedAt, g.LastAt)
	return err
}

func (t *txn) anyDirtyGroup() bool {
	for _, g := range t.groups {
		if g.Dirty {
			return true
		}
	}
	return false
}

// dirtyAuthors are the labels of the dirty groups in first-edit order.
func (t *txn) dirtyAuthors() []string {
	keys := slices.Collect(maps.Keys(t.groups))
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(t.groups[a].StartedAt, t.groups[b].StartedAt), strings.Compare(a, b))
	})
	var out []string
	for _, k := range keys {
		if g := t.groups[k]; g.Dirty && !slices.Contains(out, g.Label) {
			out = append(out, g.Label)
		}
	}
	return out
}

// deleteGroups removes every edit group (an agent write's boundary).
func (t *txn) deleteGroups() error {
	t.groups = map[string]editGroup{}
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM groups`)
	return err
}

// usage is the disk quota formula of 5.4, measured inside t after its
// writes. other already includes the bytes stored since its measurement.
func (t *txn) usage(other int64) (int64, error) {
	ps := t.in.db.pageSize
	used, err := pages(t.ctx, t.tx)
	if err != nil {
		return 0, err
	}
	ceil := func(a, b float64) int64 { return int64(math.Ceil(a / b)) }
	var r, dirtyBytes int64
	for b := range t.dirty {
		n := t.bucketBytes[b]
		dirtyBytes += n
		r += ceil(float64(n)*1.25, float64(ps-64))
	}
	files := int64(1)
	for _, n := range t.bucketBytes {
		if n > 0 {
			files++
		}
	}
	filesBytes := max(t.newestBytes, 120*(files+int64(t.in.assets.liveCount())))
	r += 2 * ceil(float64(filesBytes+512)*1.25, float64(ps))
	r += ceil(float64(len(t.dirty)), 50) + 8
	wal := 32 + 64*(ps+24)
	if t.in.sd.walPending {
		if fi, err := os.Stat(t.in.db.walPath()); err == nil {
			wal = max(wal, fi.Size())
		}
	}
	return other + (used+r)*ps + t.snapBytes + dirtyBytes + wal + t.pending, nil
}

// overQuota reports whether t leaves the board over its disk quota, with
// other from the measurement cache. ok is false when no measurement is
// available.
func (t *txn) overQuota(quota int64) (over, ok bool, err error) {
	if quota <= 0 {
		return false, true, nil
	}
	other, ok := t.in.meas.get()
	if !ok {
		return false, false, nil
	}
	u, err := t.usage(other)
	return u > quota, true, err
}

// walCheck truncates the log once it holds more than 64 frames, and keeps
// retrying after a busy or failed truncation (5.4 "WAL").
func (in *Instance) walCheck() {
	db := in.db.db
	var busy, frames, done int64
	err := db.QueryRowContext(context.Background(), `PRAGMA wal_checkpoint(NOOP)`).Scan(&busy, &frames, &done)
	if err == nil && frames <= 64 && !in.sd.walPending {
		return
	}
	err = db.QueryRowContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done)
	in.sd.walPending = err != nil || busy != 0
}

// loop is the board's single apply loop (4.6).
func (in *Instance) loop() {
	defer in.loopWG.Done()
	tick := time.NewTicker(in.tickEvery)
	defer tick.Stop()
	var next *item
	for {
		it := next
		next = nil
		if it == nil {
			select {
			case it = <-in.items:
			case <-tick.C:
				in.guard(in.tick)
				continue
			case <-in.ctx.Done():
				in.drain()
				return
			}
		}
		if it.batch != nil {
			in.guard(func() error { next = in.groupCommit(it); return nil })
			continue
		}
		var err error
		in.guard(func() error { err = it.fn(); return nil })
		it.done <- err
	}
}

// guard runs fn; a panic rolls back the open transaction, reloads the state
// from board.db and is logged, so one bad request cannot leave the loop
// with a half-applied engine state.
func (in *Instance) guard(fn func() error) {
	defer func() {
		if p := recover(); p != nil {
			in.log.Error("board apply loop panic", "panic", p, "stack", string(debug.Stack()))
			if err := in.load(); err != nil {
				in.log.Error("board reload failed", "err", err)
			}
		}
	}()
	if err := fn(); err != nil {
		in.log.Warn("board apply loop", "err", err)
	}
}

// drain answers queued items after the loop stopped.
func (in *Instance) drain() {
	for {
		select {
		case it := <-in.items:
			if it.batch != nil {
				it.batch.c.close(1001, "stopping")
			} else {
				it.done <- ErrStopped
			}
		default:
			return
		}
	}
}

// groupCommit applies the person batches that arrive within 10ms in one
// transaction (4.6) and returns the first non-batch item that arrived.
func (in *Instance) groupCommit(first *item) *item {
	t, err := in.begin()
	if err != nil {
		first.batch.transient("unavailable")
		return nil
	}
	in.stageBatch(t, first.batch)
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	var next *item
collect:
	for {
		select {
		case it := <-in.items:
			if it.batch == nil {
				next = it
				break collect
			}
			in.stageBatch(t, it.batch)
		case <-timer.C:
			break collect
		case <-in.ctx.Done():
			break collect
		}
	}
	if err := t.commit(); err != nil {
		in.log.Error("board commit failed", "err", err)
	}
	return next
}

// tick closes due edit groups and applies the retention rules (4.8, 5.3).
func (in *Instance) tick() error {
	now := in.now().Unix()
	in.snaps.expire(in.now())
	in.shares.expire(now)
	t, err := in.begin()
	if err != nil {
		return err
	}
	if err := in.closeGroups(t, now); err != nil {
		t.rollback()
		return err
	}
	st := in.st
	var open []string
	for _, c := range in.hub.list() {
		if !c.public && c.session != "" {
			open = append(open, c.session)
		}
	}
	changes := []engine.Changed{
		st.TouchSessions(open, now),
		st.ExpireSessions(now),
		st.TrimReceipts(16<<20, now),
		st.TrimDeleted(16 << 20),
		st.SweepAssets(now),
	}
	if in.ticks++; in.ticks%5 == 0 {
		quota := in.b.Limits().DiskQuota
		limit := int64(32 << 20)
		if quota > 0 {
			limit = min(limit, quota/16)
		}
		changes = append(changes, st.TrimUndo(limit))
	}
	if st.Sessions() == 0 && now >= t.lastRecord+engine.SessionTTL {
		changes = append(changes, st.TrimDeleted(0))
	}
	freed := false
	for _, c := range changes {
		if !c.Empty() {
			freed = true
			if err := t.persist(c, st.Seq()); err != nil {
				t.rollback()
				return err
			}
		}
	}
	if err := t.trimDeltaLog(now); err != nil {
		t.rollback()
		return err
	}
	freed = freed || t.trimDeltas > 0
	// A reset is kept for the session lifetime, the longest a live session
	// can hold a watermark before it (4.8), and at most resetsMax of them.
	if c := st.TrimResets(now-engine.SessionTTL, in.resetsMax); !c.Empty() {
		if err := t.persist(c, st.Seq()); err != nil {
			t.rollback()
			return err
		}
	}
	for id, op := range in.sd.ops {
		if op.importedAt > 0 && op.importedAt <= now-24*3600 {
			t.dropOps = append(t.dropOps, id)
			if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM ops WHERE id = ?`, id); err != nil {
				t.rollback()
				return err
			}
		}
	}
	if err := t.commit(); err != nil {
		return err
	}
	if freed {
		_, err = in.db.db.ExecContext(context.Background(), `PRAGMA incremental_vacuum`)
	}
	return err
}

// closeGroups records a person entry when a group is due (30 seconds idle
// or 5 minutes old) and something is dirty, then deletes the due groups.
func (in *Instance) closeGroups(t *txn, now int64) error {
	var due []string
	for k, g := range t.groups {
		if now-g.LastAt >= 30 || now-g.StartedAt >= 300 {
			due = append(due, k)
		}
	}
	if len(due) == 0 {
		return nil
	}
	if t.anyDirtyGroup() {
		if _, err := t.record(recordSpec{kind: "person", authors: t.dirtyAuthors()}); err != nil {
			return err
		}
	}
	for _, k := range due {
		delete(t.groups, k)
		if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM groups WHERE editor = ?`, k); err != nil {
			return err
		}
	}
	return nil
}

// initialize writes a new board: the metadata row, the store id and a
// forced created entry (5.3 "Creation").
func (in *Instance) initialize(title string) error {
	if model.TextLen(title) > model.MaxTitle {
		title = string([]rune(title)[:model.MaxTitle])
	}
	in.st = engine.New(title)
	in.sd = newSide()
	t, err := in.begin()
	if err != nil {
		return err
	}
	all := in.st.Export()
	c := engine.Changed{Meta: true}
	for _, e := range all.Elements {
		c.Elements = append(c.Elements, e.ID)
	}
	if err := t.persist(c, 1); err != nil {
		t.rollback()
		return err
	}
	if err := setMeta(t.ctx, t.tx, "store", randomToken()); err != nil {
		t.rollback()
		return err
	}
	if _, err := t.record(recordSpec{kind: "created", forced: true, authors: []string{}, message: "board created"}); err != nil {
		t.rollback()
		return err
	}
	t.after = nil
	t.outboxAdded = false
	return t.commit()
}

func newSide() *side {
	return &side{
		groups: map[string]editGroup{}, dirty: map[int]int64{}, bucketBytes: map[int]int64{},
		snaps: map[string]snapRef{}, ops: map[string]*opRec{},
	}
}

// load reads board.db into the engine and the side tables.
func (in *Instance) load() error {
	ctx := context.Background()
	rows, meta, err := in.db.loadRows(ctx)
	if err != nil {
		return err
	}
	st, err := engine.Load(rows)
	if err != nil {
		return err
	}
	sd := newSide()
	db := in.db.db
	scan := func(query string, fn func(*sql.Rows) error) {
		if err != nil {
			return
		}
		var rs *sql.Rows
		if rs, err = db.QueryContext(ctx, query); err != nil {
			return
		}
		defer rs.Close()
		for rs.Next() && err == nil {
			err = fn(rs)
		}
		if err == nil {
			err = rs.Err()
		}
	}
	scan(`SELECT editor, label, started_at, last_at, dirty FROM groups`, func(r *sql.Rows) error {
		var k string
		var g editGroup
		if err := r.Scan(&k, &g.Label, &g.StartedAt, &g.LastAt, &g.Dirty); err != nil {
			return err
		}
		sd.groups[k] = g
		return nil
	})
	scan(`SELECT bucket, dirty_seq, bytes FROM buckets`, func(r *sql.Rows) error {
		var b int
		var dirty sql.NullInt64
		var n int64
		if err := r.Scan(&b, &dirty, &n); err != nil {
			return err
		}
		sd.bucketBytes[b] = n
		if dirty.Valid {
			sd.dirty[b] = dirty.Int64
		}
		return nil
	})
	scan(`SELECT hash, length(data), refs FROM snapshots`, func(r *sql.Rows) error {
		var h string
		var ref snapRef
		if err := r.Scan(&h, &ref.size, &ref.refs); err != nil {
			return err
		}
		sd.snaps[h] = ref
		sd.snapBytes += ref.size
		return nil
	})
	scan(`SELECT seq FROM outbox ORDER BY seq`, func(r *sql.Rows) error {
		var s int64
		if err := r.Scan(&s); err != nil {
			return err
		}
		sd.outbox = append(sd.outbox, s)
		return nil
	})
	scan(`SELECT seq, from_session, n, changes, at FROM deltas ORDER BY seq`, func(r *sql.Rows) error {
		var d deltaRec
		var body string
		if err := r.Scan(&d.seq, &d.session, &d.n, &body, &d.at); err != nil {
			return err
		}
		d.body = []byte(body)
		sd.deltas = append(sd.deltas, d)
		sd.deltaBytes += int64(len(body))
		return nil
	})
	scan(`SELECT id, kind, request_hash, outbox_seq, result, at, imported_at FROM ops`, func(r *sql.Rows) error {
		var id, res string
		var imported sql.NullInt64
		op := &opRec{}
		if err := r.Scan(&id, &op.kind, &op.hash, &op.outboxSeq, &res, &op.at, &imported); err != nil {
			return err
		}
		op.result, op.importedAt = json.RawMessage(res), imported.Int64
		sd.ops[id] = op
		return nil
	})
	if err != nil {
		return err
	}
	if v := meta["newest"]; v != "" {
		if err := json.Unmarshal([]byte(v), &sd.newest); err != nil {
			return fmt.Errorf("meta.newest: %w", err)
		}
		sd.newestBytes = int64(len(v))
	}
	fmt.Sscan(meta["share_n"], &sd.shareN)
	fmt.Sscan(meta["last_record"], &sd.lastRecord)
	sd.walPending = meta["wal_truncate"] != ""
	sd.walPersisted = sd.walPending
	in.storeID = meta["store"]
	if in.storeID == "" {
		return fmt.Errorf("board.db has no store id")
	}
	in.st, in.sd = st, sd
	in.snapCur = nil
	in.assets.reset(rows.Assets, st)
	in.walCheck()
	return nil
}
