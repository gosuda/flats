package backup

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// holdExclusive locks the rollback-journal database at path exclusively, which
// stops readers, closes locked when it holds the lock, and lets go after hold.
func holdExclusive(t *testing.T, path string, locked chan<- struct{}, hold time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(path))
	if err != nil {
		t.Error(err)
		close(locked)
		return
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Error(err)
		close(locked)
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Error(err)
		close(locked)
		return
	}
	close(locked)
	time.Sleep(hold)
	conn.ExecContext(context.Background(), `ROLLBACK`)
}
