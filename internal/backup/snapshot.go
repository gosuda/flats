package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/sqliteuri"
)

// snapshotDB copies the database rel as a snapshot made with VACUUM INTO from a
// read-only connection (rule 4). It tries again, up to maxTries, when the
// snapshot cannot start within the busy timeout. A database that is gone is
// treated by rule 8.
func (c *copier) snapshotDB(rel, kind string) error {
	src, dst := c.src(rel), c.dst(rel)
	mp := manifestPath(rel)
	gone, busy := 0, 0
	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			if kind == KindPreview {
				c.skip(rel, "gone while it was copied")
				return nil
			}
			if gone++; gone >= maxTries {
				return &RerunError{fmt.Errorf("%s is gone", rel)}
			}
			time.Sleep(vanishPause)
			continue
		}
		err := vacuumInto(c.ctx, src, dst, kind)
		switch {
		case err == nil:
			f, err := hashFile(dst)
			if err != nil {
				return err
			}
			f.Path = mp
			c.files[mp] = f
			c.dbs[mp] = kind
			if c.afterSnap != nil {
				c.afterSnap(rel)
			}
			return nil
		case isBusy(err) && busy+1 < maxTries:
			busy++
			c.retries++
		case kind == KindPreview && !fileExists(src):
			c.skip(rel, "gone while it was copied")
			return nil
		default:
			return fmt.Errorf("snapshot %s: %w", rel, err)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isBusy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "database is locked") || strings.Contains(s, "SQLITE_BUSY")
}

// vacuumInto writes a snapshot of the database src to the new file dst, which it
// creates with mode 0600 first: VACUUM INTO accepts an empty file.
func vacuumInto(ctx context.Context, src, dst, kind string) error {
	if kind == KindNotebook {
		if err := notebook.RegisterCollation(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	db, err := sql.Open("sqlite", sqliteuri.File(src)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		os.Remove(dst)
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		os.Remove(dst)
		return err
	}
	out, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	return out.Sync()
}
