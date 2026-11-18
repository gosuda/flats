package sheet

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"maps"
	"math"
	"os"
	"sync"
	"time"
)

const measureTTL = 60 * time.Second

// measureLimit bounds one background measurement of the flat's directory.
// A walk that takes longer ends as a failed measurement, and the next one
// starts on time.
var measureLimit = 2 * time.Minute

func quotaFailure() *WriteError { return &WriteError{Code: "sheet_capacity", Reason: "disk_quota"} }

func quotaAdd(values ...int64) (int64, error) {
	var total int64
	for _, n := range values {
		if n < 0 || n > math.MaxInt64-total {
			return 0, quotaFailure()
		}
		total += n
	}
	return total, nil
}

func quotaMul(a, b int64) (int64, error) {
	if a < 0 || b < 0 || (b != 0 && a > math.MaxInt64/b) {
		return 0, quotaFailure()
	}
	return a * b, nil
}

// quotaPages returns the pages that n bytes of rows need at four fifths
// fill, which is what recording them costs in the worst case.
func quotaPages(n, pageSize int64) int64 {
	usable := 4 * pageSize / 5
	return (n + usable - 1) / usable
}

// contentMeasure is what the quota formula of section 16.5 needs from the
// file state.
type contentMeasure struct {
	dirtySizes []int64 // reserve of each dirty row file, exact size of dirty axes and manifest
	dirtyBytes int64
	listBytes  int64 // bytes of the full file list as an outbox entry stores it
	unimported int64 // snapshot bytes the host has not imported yet
}

func (in *Instance) measureContent(ctx context.Context, tx *sql.Tx, wb *Workbook) (contentMeasure, error) {
	var m contentMeasure
	fs := &wb.files
	add := func(n int64) {
		m.dirtySizes = append(m.dirtySizes, n)
		m.dirtyBytes += n
	}
	for _, reserve := range fs.chunks {
		add(reserve)
	}
	for id := range fs.axes {
		if t := wb.byID[id]; t != nil {
			add(int64(len(encodeAxes(t))))
		}
	}
	if fs.manifest {
		add(int64(len(encodeManifest(wb))))
	}
	// A file entry is its path, a 64-character hash, a size and JSON framing.
	m.listBytes = int64(len(fs.list)+len(m.dirtySizes)) * 160
	err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(length(data)),0) FROM snapshots WHERE refs>0`).Scan(&m.unimported)
	return m, err
}

func (m contentMeasure) futurePages(pageSize int64) int64 {
	pages := 2*quotaPages(m.listBytes+512, pageSize) + int64(len(m.dirtySizes))/50 + 8
	for _, size := range m.dirtySizes {
		pages += quotaPages(size, pageSize-64)
	}
	return pages
}

func usedPages(ctx context.Context, tx *sql.Tx) (int64, error) {
	var count, free int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&count); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	return max(0, count-free), nil
}

// pendingWAL bounds the write-ahead log after this transaction and the
// boundary that will follow it: the log as it is, and a frame for every
// page the two can touch.
func (in *Instance) pendingWAL(ctx context.Context, tx *sql.Tx, future int64) (int64, error) {
	frame := in.pageSize + 24
	actual := 32 + 64*frame
	if info, err := os.Stat(in.path + "-wal"); err == nil {
		actual = max(actual, info.Size())
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var pages int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	pending, err := quotaMul(pages+future, frame)
	if err != nil {
		return 0, err
	}
	return quotaAdd(actual, pending)
}

// checkQuota measures disk usage on the staged transaction and refuses
// when it would pass the flat's quota (section 16.5). other is the usage of
// the rest of the flat's directory.
func (in *Instance) checkQuota(ctx context.Context, tx *sql.Tx, wb *Workbook, other int64) error {
	quota := in.b.Limits().DiskQuota
	if quota <= 0 {
		return nil
	}
	total, future, err := in.quotaUsage(ctx, tx, wb, other, 0)
	if err != nil {
		return err
	}
	if total > quota {
		return quotaFailure()
	}
	if quota-total > future*in.pageSize {
		return nil
	}
	// Within one recording reservation of the quota the formula is too
	// coarse. Record the boundary in a savepoint that is rolled back, take
	// the exact sizes of the encoded files as the reserves, and charge the
	// larger of the formula and the page growth that was observed.
	growth, err := in.probeBoundary(ctx, tx, wb)
	if err != nil {
		return err
	}
	// The fixed part of the formula stays as room for maintenance.
	if total, _, err = in.quotaUsage(ctx, tx, wb, other, growth+8); err != nil {
		return err
	}
	if total > quota {
		return quotaFailure()
	}
	return nil
}

// probeBoundary records the next revision boundary in a savepoint, measures
// the pages that took, and rolls the savepoint back. It leaves each dirty
// row file with the exact size of its next encoding as its reserve.
func (in *Instance) probeBoundary(ctx context.Context, tx *sql.Tx, wb *Workbook) (growth int64, err error) {
	plan, err := wb.planFiles()
	if err != nil {
		return 0, err
	}
	used, err := usedPages(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT quota_probe`); err != nil {
		return 0, err
	}
	_, undo, err := in.recordPlan(ctx, tx, wb, boundary{kind: "person", force: true}, plan)
	after := used
	if err == nil {
		after, err = usedPages(ctx, tx)
	}
	undo()
	// A cancelled request still has to take the probe out before its caller
	// decides about the transaction.
	_, rollback := tx.ExecContext(context.Background(), `ROLLBACK TO quota_probe`)
	_, release := tx.ExecContext(context.Background(), `RELEASE quota_probe`)
	if err = errors.Join(err, rollback, release); err != nil {
		return 0, err
	}
	// The next encoding of a dirty chunk is what the plan encoded for it,
	// also when those bytes equal the recorded file: a later edit adds to
	// the reserve only what it writes, so the reserve has to hold the whole
	// file. A split or a merge moves bytes to chunks that are not dirty
	// yet; one dirty chunk carries them, and the sum stays a bound.
	var spread int64
	for key, size := range plan.encoded {
		if _, dirty := wb.files.chunks[key]; !dirty {
			spread += size
		}
	}
	for key := range wb.files.chunks {
		wb.files.chunks[key] = plan.encoded[key] + spread
		spread = 0
	}
	return max(0, after-used), stageDirty(ctx, tx, wb, wb.seq)
}

// measure keeps the usage of the rest of the flat's directory, at most 60
// seconds old, for people's edits: what the last measurement found, and the
// bytes imports stored since.
//
// A measurement walks the directory without the mutex and can overlap an
// import or another measurement. Two rules keep an older view from
// replacing newer accounting. A measurement drops the imports' bytes only
// when none was added since it began, as it may have read the directory
// before their blobs were there. And a measurement that began before one
// that has already ended changes nothing, as the later one saw at least as
// much.
type measure struct {
	in    *Instance
	mu    sync.Mutex
	other int64
	extra int64
	at    time.Time
	valid bool
	wake  chan struct{}

	begun   uint64 // measurements begun, which numbers them
	settled uint64 // number of the newest measurement that ended
	added   uint64 // additions to extra
}

func (m *measure) get() (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid || m.in.now().Sub(m.at) > measureTTL {
		return 0, false
	}
	total, err := quotaAdd(m.other, m.extra)
	return total, err == nil
}

func (m *measure) add(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added++
	total, err := quotaAdd(m.extra, n)
	if err != nil {
		m.valid = false
		return
	}
	m.extra = total
}

func (m *measure) refresh() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *measure) remeasure(ctx context.Context) error {
	m.mu.Lock()
	m.begun++
	number, added := m.begun, m.added
	m.mu.Unlock()
	n, err := m.in.b.MeasureOther(ctx)
	if err == nil && n < 0 {
		err = unavailable("measurement")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if number < m.settled {
		return err
	}
	m.settled = number
	if err != nil {
		m.valid = false
		return err
	}
	m.other, m.at, m.valid = n, m.in.now(), true
	if added == m.added {
		m.extra = 0
	}
	return nil
}

func (m *measure) run() {
	defer m.in.workers.Done()
	tick := time.NewTicker(measureTTL / 2)
	defer tick.Stop()
	for {
		select {
		case <-m.in.ctx.Done():
			return
		case <-m.wake:
		case <-tick.C:
		}
		ctx, cancel := context.WithTimeout(m.in.ctx, measureLimit)
		err := m.remeasure(ctx)
		cancel()
		if err != nil && m.in.ctx.Err() == nil {
			m.in.log.Warn("sheet directory measurement", "err", err)
		}
	}
}

// truncateWAL checkpoints and truncates the write-ahead log when no reader
// pins it. A busy log is an ordinary outcome and is left for the next call.
func (in *Instance) truncateWAL() (err error) {
	ctx := context.Background()
	conn, err := in.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); restoreErr != nil {
			// Never return a zero-timeout connection to the writer pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, restoreErr)
		}
	}()
	// A stop interrupts the checkpoint where SQLite looks for it, between
	// two pages it copies, so the stop waits for none of the pages left.
	// It waits for what SQLite does not interrupt: the setup before the
	// first page, which sorts the page numbers of the log's frames from
	// their index in memory and syncs the log, the page being copied, and
	// after the last page the sync of sheet.db and the truncation of the
	// log.
	var busy, frames, checkpointed int64
	return conn.QueryRowContext(in.ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed)
}

// quotaUsage is the usage formula of section 16.5 on the staged transaction,
// and the pages reserved for recording the next revision: those of the
// formula, or observed when a probe measured more.
func (in *Instance) quotaUsage(ctx context.Context, tx *sql.Tx, wb *Workbook, other, observed int64) (total, future int64, err error) {
	m, err := in.measureContent(ctx, tx, wb)
	if err != nil {
		return 0, 0, err
	}
	used, err := usedPages(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	future = max(m.futurePages(in.pageSize), observed)
	wal, err := in.pendingWAL(ctx, tx, future)
	if err != nil {
		return 0, 0, err
	}
	db, err := quotaMul(used+future, in.pageSize)
	if err != nil {
		return 0, 0, err
	}
	total, err = quotaAdd(other, db, m.unimported, m.dirtyBytes, wal)
	return total, future, err
}

const (
	// refusedResult bounds one refused operation in an ack: the object with
	// its status and a reason of at most 32 bytes, and its comma.
	refusedResult = 64
	// receiptRecord bounds what a stored receipt takes beside its outcome:
	// the ack's frame, the hash, the key and the record's header.
	receiptRecord = 256
	// receiptSplits is the pages one insert can add by splitting the pages
	// above it in the table's tree.
	receiptSplits = 4
)

// receiptRoom reserves the room of the refusal receipt an action of a
// session with a row would store, before the action applies (technical
// design, section 6): the usage of the sheet as it is, and the pages of
// the receipt and of the session's row with their frames in the
// write-ahead log, are within the quota. Over it the action is refused at
// once and stores nothing. The check of the receipt that is then stored
// measures again.
func (in *Instance) receiptRoom(ctx context.Context, tx *sql.Tx, wb *Workbook, s *session, other int64, ops int) error {
	quota := in.b.Limits().DiskQuota
	if quota <= 0 {
		return nil
	}
	total, _, err := in.quotaUsage(ctx, tx, wb, other, 0)
	if err != nil {
		return err
	}
	bytes := int64(receiptRecord) + int64(ops)*refusedResult + s.weight()
	need, err := quotaMul(quotaPages(bytes, in.pageSize)+receiptSplits, 2*in.pageSize+24)
	if err != nil {
		return err
	}
	if total, err = quotaAdd(total, need); err != nil || total > quota {
		return quotaFailure()
	}
	return nil
}

// receiptFits checks the quota with a receipt staged that changes nothing
// in the sheet. Near the quota the check sets exact reserves, which belong
// to a transaction that commits: when the receipt does not fit they are
// put back.
func (in *Instance) receiptFits(ctx context.Context, tx *sql.Tx, wb *Workbook, other int64) error {
	reserves := maps.Clone(wb.files.chunks)
	err := in.checkQuota(ctx, tx, wb, other)
	if err != nil {
		wb.files.chunks = reserves
	}
	return err
}
