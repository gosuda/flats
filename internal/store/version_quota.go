package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Store) SetVersionQuota(ctx context.Context, flat string, quota *int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE flats SET version_quota=? WHERE slug=?`, quota, flat)
	return err
}

func (s *Store) VersionCount(ctx context.Context, flat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE flat=? AND pruned=0`, flat).Scan(&n)
	return n, err
}

func (s *Store) CommitVersionDeletion(ctx context.Context, flat string, number int, approval string, data json.RawMessage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM versions WHERE flat=? AND number=? AND number != (SELECT live_version FROM flats WHERE slug=?)`, flat, number, flat)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("version unavailable or live: %w", ErrNotFound)
	}
	if approval != "" {
		res, err = tx.ExecContext(ctx, `UPDATE approvals SET result_data=? WHERE id=? AND status='applying'`, string(data), approval)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("approval not applying")
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE flats SET updated_at=? WHERE slug=?`, unix(now), flat); err != nil {
		return err
	}
	return tx.Commit()
}

// PreviousAvailableDeployment finds the last deployed version other than live
// whose immutable files have not been pruned or explicitly deleted.
func (s *Store) PreviousAvailableDeployment(ctx context.Context, flat string, live int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT v.number FROM deployments d JOIN versions v ON v.flat=d.flat AND (v.number=d.version OR v.number=d.previous) WHERE d.flat=? AND v.number<>? AND v.pruned=0 ORDER BY d.id DESC,CASE WHEN v.number=d.version THEN 1 ELSE 0 END DESC LIMIT 1`, flat, live).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// SettleVersionDeletion updates only an already-approved deletion's cleanup
// receipt. The original operator decision and decision time are preserved.
func (s *Store) SettleVersionDeletion(ctx context.Context, id string, before, after json.RawMessage, result string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET result=?,result_data=? WHERE id=? AND action='delete_version' AND status='approved' AND result_data=?`, result, string(after), id, string(before))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("deletion cleanup receipt changed concurrently")
	}
	return nil
}

// ListPendingVersionDeletions returns every approved deletion awaiting cleanup.
// Recovery must not use the display list's limit: a receipt can outlive hundreds
// of subsequent approvals before the host restarts.
func (s *Store) ListPendingVersionDeletions(ctx context.Context) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE action='delete_version' AND status='approved' ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		var receipt struct {
			CleanupPending bool `json:"cleanup_pending"`
		}
		if json.Unmarshal(a.ResultData, &receipt) == nil && receipt.CleanupPending {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}
