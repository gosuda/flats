package notebook

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"os"
)

func quotaFailure() error {
	return &WriteError{Code: "notebook_capacity", Reason: "disk_quota"}
}

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

func quotaCeil(n, d int64) (int64, error) {
	if n < 0 || d <= 0 {
		return 0, quotaFailure()
	}
	q := n / d
	if n%d != 0 {
		q++
	}
	return q, nil
}

// Split before multiplying so even a representable result near MaxInt64
// does not overflow an intermediate 5*n.
func quotaInflatedPages(n, size int64) (int64, error) {
	d, err := quotaMul(4, size)
	if err != nil || d == 0 || n < 0 {
		return 0, quotaFailure()
	}
	whole, err := quotaMul(n/d, 5)
	if err != nil {
		return 0, err
	}
	remainder, err := quotaMul(n%d, 5)
	if err != nil {
		return 0, err
	}
	part, err := quotaCeil(remainder, d)
	if err != nil {
		return 0, err
	}
	return quotaAdd(whole, part)
}

func quotaFuturePages(m contentMeasure, pageSize int64) (int64, error) {
	if pageSize <= 64 {
		return 0, quotaFailure()
	}
	listBytes, err := quotaAdd(m.FullListBytes, 512)
	if err != nil {
		return 0, err
	}
	listPages, err := quotaInflatedPages(listBytes, pageSize)
	if err != nil {
		return 0, err
	}
	listPages, err = quotaMul(listPages, 2)
	if err != nil {
		return 0, err
	}
	indexPages, err := quotaCeil(int64(len(m.DirtySizes)), 50)
	if err != nil {
		return 0, err
	}
	pages, err := quotaAdd(listPages, indexPages, 8)
	if err != nil {
		return 0, err
	}
	for _, size := range m.DirtySizes {
		n, err := quotaInflatedPages(size, pageSize-64)
		if err != nil {
			return 0, err
		}
		pages, err = quotaAdd(pages, n)
		if err != nil {
			return 0, err
		}
	}
	return pages, nil
}

func quotaUsedPages(ctx context.Context, tx *sql.Tx) (int64, error) {
	var count, free int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&count); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if free < 0 || count < free {
		return 0, fmt.Errorf("invalid notebook page counts")
	}
	return count - free, nil
}

func (in *Instance) quotaWALBytes() (int64, error) {
	frame, err := quotaAdd(in.db.pageSize, 24)
	if err != nil {
		return 0, err
	}
	idle, err := quotaMul(64, frame)
	if err != nil {
		return 0, err
	}
	idle, err = quotaAdd(32, idle)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(in.db.walPath())
	if errors.Is(err, os.ErrNotExist) {
		return idle, nil
	}
	if err != nil {
		return 0, fmt.Errorf("notebook WAL size: %w", err)
	}
	return max(idle, info.Size()), nil
}

func (in *Instance) quotaPendingWAL(ctx context.Context, tx *sql.Tx, future int64) (int64, error) {
	actual, err := in.quotaWALBytes()
	if err != nil {
		return 0, err
	}
	var pages int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	pages, err = quotaAdd(pages, future)
	if err != nil {
		return 0, err
	}
	frame, err := quotaAdd(in.db.pageSize, 24)
	if err != nil {
		return 0, err
	}
	pending, err := quotaMul(pages, frame)
	if err != nil {
		return 0, err
	}
	// SQLite can retain dirty frames until commit, after admission. Its
	// cache does not expose the dirty-page count through database/sql, so
	// reserve a full page-set upper bound, including the future boundary.
	return quotaAdd(actual, pending)
}

func quotaUsage(m contentMeasure, pageSize, used, future, other, pending, wal int64) (int64, error) {
	pages, err := quotaAdd(used, future)
	if err != nil {
		return 0, err
	}
	dbBytes, err := quotaMul(pages, pageSize)
	if err != nil {
		return 0, err
	}
	return quotaAdd(other, dbBytes, m.UnimportedBytes, m.DirtyUniqueBytes, pending, wal)
}

func (in *Instance) quotaRecordedGrowth(ctx context.Context, tx *sql.Tx, used int64) (growth int64, err error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_quota_record`); err != nil {
		return 0, err
	}
	defer func() {
		// A cancelled request still has to undo the probe before its caller
		// can decide whether to keep the enclosing transaction.
		_, rollbackErr := tx.ExecContext(context.Background(), `ROLLBACK TO notebook_quota_record`)
		_, releaseErr := tx.ExecContext(context.Background(), `RELEASE notebook_quota_record`)
		err = errors.Join(err, rollbackErr, releaseErr)
	}()
	if _, err := recordOutbox(ctx, tx, "person", "", "", nil, in.now(), true); err != nil {
		return 0, err
	}
	after, err := quotaUsedPages(ctx, tx)
	if err != nil {
		return 0, err
	}
	return max(0, after-used), nil
}

func (in *Instance) checkQuota(ctx context.Context, tx *sql.Tx, other, pending int64) error {
	if in.measure != nil {
		var err error
		other, err = in.measure.agentOther(other)
		if err != nil {
			return err
		}
	}
	quota := in.b.Limits().DiskQuota
	if quota <= 0 {
		return nil
	}
	m, err := measureContent(ctx, tx)
	if err != nil {
		return err
	}
	used, err := quotaUsedPages(ctx, tx)
	if err != nil {
		return err
	}
	future, err := quotaFuturePages(m, in.db.pageSize)
	if err != nil {
		return err
	}
	wal, err := in.quotaPendingWAL(ctx, tx, future)
	if err != nil {
		return err
	}
	usage, err := quotaUsage(m, in.db.pageSize, used, future, other, pending, wal)
	if err != nil {
		return err
	}
	if usage > quota {
		return quotaFailure()
	}
	// Probe within one more recording reservation of the limit. Outside
	// this window even another full reservation fits without a probe.
	window, err := quotaMul(future, in.db.pageSize)
	if err != nil {
		return err
	}
	if quota-usage <= window {
		growth, err := in.quotaRecordedGrowth(ctx, tx, used)
		if err != nil {
			return err
		}
		// Keep the formula's fixed maintenance/refusal headroom even if
		// recording needs more pages than its variable estimate.
		growth, err = quotaAdd(growth, 8)
		if err != nil {
			return err
		}
		future = max(future, growth)
		// Recording can spill pages into the WAL even though its savepoint
		// is rolled back, so sample again after the probe.
		wal, err = in.quotaPendingWAL(ctx, tx, future)
		if err != nil {
			return err
		}
		usage, err = quotaUsage(m, in.db.pageSize, used, future, other, pending, wal)
		if err != nil {
			return err
		}
		if usage > quota {
			return quotaFailure()
		}
	}
	return nil
}

func (in *Instance) truncateWAL() (err error) {
	ctx := context.Background()
	conn, err := in.db.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`)
		if restoreErr != nil {
			// Never return a zero-timeout connection to the writer pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		err = errors.Join(err, restoreErr)
	}()
	var busy, frames, checkpointed int64
	// Busy is an ordinary pinned-reader outcome, not a reason to wait.
	return conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed)
}
