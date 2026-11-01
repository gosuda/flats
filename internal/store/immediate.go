package store

import (
	"context"
	"database/sql/driver"
)

// immediate holds the write lock before fn reads. fn reports whether to
// commit; a no-op or error rolls back. A failed commit also rolls back.
func (s *Store) immediate(ctx context.Context, fn func(executor) (bool, error)) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
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
	wrote, err := fn(conn)
	if err != nil || !wrote {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	done = true
	return true, nil
}
