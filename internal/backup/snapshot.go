package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/gosuda/flats/internal/sqliteuri"
)

// snapshotDB copies the database rel as a snapshot made with VACUUM INTO from a
// read-only connection (rule 4). It tries again, up to maxTries, when the
// snapshot cannot start within the busy timeout. A database that is gone is
// treated by rule 8. It does no more than the snapshot: the moment of a snapshot
// is when its read transaction starts, and the coherence of rule 16 depends on
// snapshots that start close together, so the sync and the hash of a snapshot
// wait for sealDatabases. gate is nil, or a function that snapshotDB calls when
// it is ready to start and that returns when every snapshot of the group is.
func (c *copier) snapshotDB(rel, kind string, gate func()) error {
	src, dst := c.src(rel), c.dst(rel)
	mp := manifestPath(rel)
	gone, busy := 0, 0
	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			if kind == KindPreview {
				c.skipLocked(rel, "gone while it was copied")
				return nil
			}
			if gone++; gone >= maxTries {
				return &RerunError{fmt.Errorf("%s is gone", rel)}
			}
			time.Sleep(vanishPause)
			continue
		}
		err := vacuumInto(c.ctx, src, dst, kind, c.busy, gate)
		switch {
		case err == nil:
			c.mu.Lock()
			c.dbs[mp] = kind
			delete(c.files, mp) // sealDatabases hashes the snapshot once the last one is made
			if c.afterSnap != nil {
				c.afterSnap(rel)
			}
			c.mu.Unlock()
			return nil
		case isBusy(err) && busy+1 < maxTries:
			busy++
			c.mu.Lock()
			c.retries++
			c.mu.Unlock()
		case kind == KindPreview && !fileExists(src):
			c.skipLocked(rel, "gone while it was copied")
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

func (c *copier) skipLocked(rel, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skip(rel, reason)
}

// vacuumInto writes a snapshot of the database src to the new file dst, which it
// creates with mode 0600 first: VACUUM INTO accepts an empty file. It opens the
// connection first, with the busy timeout, and calls gate, when there is one, just before the statement.
func vacuumInto(ctx context.Context, src, dst, kind string, busy time.Duration, gate func()) error {
	// It does not register the collation of a notebook database: Run does that before it starts a goroutine,
	// because the driver's table of collations is not safe against a registration that overlaps an open.
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
	db, err := sql.Open("sqlite", sqliteuri.File(src)+"?mode=ro&_pragma=busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	if err != nil {
		os.Remove(dst)
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if gate != nil {
		if err := db.PingContext(ctx); err != nil {
			os.Remove(dst)
			return err
		}
		gate()
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// sealDatabases makes every snapshot durable and records its size and hash. It
// runs after the last snapshot, so that its time does not lengthen the window
// between two snapshots.
func (c *copier) sealDatabases() error {
	mps := make([]string, 0, len(c.dbs))
	for mp := range c.dbs {
		mps = append(mps, mp)
	}
	sort.Strings(mps)
	for _, mp := range mps {
		p := filepath.Join(c.root, filepath.FromSlash(mp))
		out, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = out.Sync()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		f, err := hashFile(p)
		if err != nil {
			return err
		}
		f.Path = mp
		c.files[mp] = f
	}
	return nil
}

// barrier lets a group of goroutines start together: each member arrives, and
// all of them go on when the last has arrived.
type barrier struct {
	mu   sync.Mutex
	left int
	open chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{left: n, open: make(chan struct{})} }

// member returns the arrive function of one member, which counts once however
// often it runs, and the gate that the member passes when it is ready.
func (b *barrier) member() (arrive func(), gate func()) {
	var once sync.Once
	arrive = func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.left--; b.left == 0 {
				close(b.open)
			}
		})
	}
	return arrive, func() {
		arrive()
		<-b.open
	}
}

// snapshotTogether snapshots flats.db and the databases in items at the same
// time (rule 15): each one opens its connection first, then all of them start
// their snapshot together. A member that fails before its start still counts, so
// the others do not wait for it. It returns the first error in the order of the
// snapshots. A test can set c.order to take them one by one in a chosen order.
func (c *copier) snapshotTogether(items []dbEntry) error {
	all := append(slices.Clone(items), dbEntry{nameFlatsDB, KindFlats})
	if c.order != nil {
		for _, d := range c.order(all) {
			if err := c.snapshotDB(d.rel, d.kind, nil); err != nil {
				return err
			}
		}
		return nil
	}
	b := newBarrier(len(all))
	errs := make([]error, len(all))
	var wg sync.WaitGroup
	for i, d := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrive, gate := b.member()
			defer arrive()
			errs[i] = c.snapshotDB(d.rel, d.kind, gate)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
