package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// addAutopilot adds the Autopilot settings, off with no operation allowed
// and an empty guide, and the log of their changes.
func addAutopilot(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS autopilot (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  ops INTEGER NOT NULL DEFAULT 0 CHECK (ops >= 0 AND ops < 512),
  guide TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO autopilot (id) VALUES (1);
CREATE TABLE IF NOT EXISTS autopilot_changes (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  line TEXT NOT NULL,
  logged INTEGER NOT NULL DEFAULT 0 CHECK (logged IN (0, 1))
);`)
	return err
}

// AutopilotRow is the stored Autopilot settings. Ops is a bit set of the
// operations agents may decide; core names the bits.
type AutopilotRow struct {
	Enabled   bool
	Ops       uint16
	Guide     string
	UpdatedAt time.Time // zero while the row was never changed
}

func (r AutopilotRow) sameSettings(o AutopilotRow) bool {
	return r.Enabled == o.Enabled && r.Ops == o.Ops && r.Guide == o.Guide
}

var (
	// ErrAutopilotDenied is returned by an Autopilot claim or rejection that
	// matched no row because Autopilot was off or an operation the approval
	// needs was not allowed when it ran.
	ErrAutopilotDenied = errors.New("autopilot does not allow this decision")
	// ErrApprovalNotPending is returned by an Autopilot claim or rejection of
	// an approval that is no longer pending.
	ErrApprovalNotPending = errors.New("approval no longer pending")
)

// AutopilotChange is the host log line of one committed settings change.
type AutopilotChange struct {
	Seq  int64
	At   time.Time
	Line string
}

// autopilotChangesKept is how many logged change lines stay in the table.
const autopilotChangesKept = 100

// GetAutopilot returns the stored settings.
func (s *Store) GetAutopilot(ctx context.Context) (AutopilotRow, error) {
	return getAutopilot(ctx, s.db)
}

func getAutopilot(ctx context.Context, x executor) (AutopilotRow, error) {
	var r AutopilotRow
	var at int64
	var ops int64
	if err := x.QueryRowContext(ctx, `SELECT enabled, ops, guide, updated_at FROM autopilot WHERE id=1`).Scan(&r.Enabled, &ops, &r.Guide, &at); err != nil {
		return AutopilotRow{}, err
	}
	r.Ops = uint16(ops)
	if at != 0 {
		r.UpdatedAt = fromUnix(at)
	}
	return r, nil
}

// UpdateAutopilot runs one write transaction: it reads the stored row, calls
// change with it, and, when change returns a row different from the stored
// one, writes that row and appends line to autopilot_changes before
// committing. It returns the row as committed (the stored row when nothing
// changed) and whether it wrote. An error from change rolls back and is
// returned as is.
//
// The transaction starts with BEGIN IMMEDIATE, so it holds the write lock
// before it reads and a concurrent writer waits instead of committing over
// the value read here.
func (s *Store) UpdateAutopilot(ctx context.Context, now time.Time,
	change func(stored AutopilotRow) (next AutopilotRow, line string, err error)) (AutopilotRow, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return AutopilotRow{}, false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return AutopilotRow{}, false, err
	}
	done := false
	defer func() {
		if done {
			return
		}
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`); err != nil {
			// A connection whose transaction state is unknown must not go
			// back to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	stored, err := getAutopilot(ctx, conn)
	if err != nil {
		return AutopilotRow{}, false, err
	}
	next, line, err := change(stored)
	if err != nil {
		return stored, false, err
	}
	if next.sameSettings(stored) {
		return stored, false, nil
	}
	next.UpdatedAt = fromUnix(unix(now))
	if _, err := conn.ExecContext(ctx, `UPDATE autopilot SET enabled=?, ops=?, guide=?, updated_at=? WHERE id=1`,
		next.Enabled, int64(next.Ops), next.Guide, unix(now)); err != nil {
		return stored, false, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO autopilot_changes (at, line) VALUES (?, ?)`, unix(now), line); err != nil {
		return stored, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return stored, false, err
	}
	done = true
	return next, true, nil
}

// AutopilotSync reads the row and every change with logged = 0, in seq
// order, in one read transaction.
func (s *Store) AutopilotSync(ctx context.Context) (AutopilotRow, []AutopilotChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AutopilotRow{}, nil, err
	}
	defer tx.Rollback()
	row, err := getAutopilot(ctx, tx)
	if err != nil {
		return AutopilotRow{}, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, at, line FROM autopilot_changes WHERE logged=0 ORDER BY seq`)
	if err != nil {
		return AutopilotRow{}, nil, err
	}
	defer rows.Close()
	var changes []AutopilotChange
	for rows.Next() {
		var c AutopilotChange
		var at int64
		if err := rows.Scan(&c.Seq, &at, &c.Line); err != nil {
			return AutopilotRow{}, nil, err
		}
		c.At = fromUnix(at)
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return AutopilotRow{}, nil, err
	}
	return row, changes, nil
}

// MarkAutopilotLogged sets logged = 1 up to seq and deletes logged changes
// except the newest 100.
func (s *Store) MarkAutopilotLogged(ctx context.Context, seq int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE autopilot_changes SET logged=1 WHERE seq<=? AND logged=0`, seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM autopilot_changes WHERE logged=1 AND seq NOT IN
  (SELECT seq FROM autopilot_changes WHERE logged=1 ORDER BY seq DESC LIMIT ?)`, autopilotChangesKept); err != nil {
		return err
	}
	return tx.Commit()
}

// autopilotAllows is the condition an Autopilot claim or rejection adds: the
// row is on and allows every bit of need. Both parameters are need.
const autopilotAllows = `EXISTS (SELECT 1 FROM autopilot WHERE id=1 AND enabled=1 AND (ops & ?) = ?)`

// ClaimApprovalAutopilot is ClaimApprovalAuthorized for an agent decision:
// it claims the pending approval only while Autopilot is on and allows
// every operation of need, checked in the same statement.
func (s *Store) ClaimApprovalAutopilot(ctx context.Context, id, actor string, need uint16, now time.Time) error {
	if need == 0 {
		return errors.New("autopilot claim without a needed operation")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='applying',decided_by=?,authorized_at=? WHERE id=? AND status='pending' AND `+autopilotAllows,
		actor, unix(now), id, int64(need), int64(need))
	if err != nil {
		return err
	}
	return s.autopilotOutcome(ctx, res, id)
}

// RejectApprovalAutopilot is RejectApprovalAuthorized for an agent decision,
// with the condition of ClaimApprovalAutopilot.
func (s *Store) RejectApprovalAutopilot(ctx context.Context, id, actor, result string, need uint16, now time.Time) error {
	if need == 0 {
		return errors.New("autopilot rejection without a needed operation")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='rejected',result=?,decided_by=?,authorized_at=?,decided_at=? WHERE id=? AND status='pending' AND `+autopilotAllows,
		result, actor, unix(now), unix(now), id, int64(need), int64(need))
	if err != nil {
		return err
	}
	return s.autopilotOutcome(ctx, res, id)
}

// autopilotOutcome names why a conditional update changed nothing. The
// update already decided the outcome; this read only picks the error.
func (s *Store) autopilotOutcome(ctx context.Context, res sql.Result, id string) error {
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	a, err := s.GetApproval(ctx, id)
	if err != nil {
		return err
	}
	if a.Status != "pending" {
		return fmt.Errorf("%w: approval is %s", ErrApprovalNotPending, a.Status)
	}
	return ErrAutopilotDenied
}
