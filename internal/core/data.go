package core

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/store"
)

// checkReserved rejects slugs that collide with system host names, an open
// preview host or a rename redirect. A redirect that points at owner (the
// flat being renamed) does not count: a flat may take its old slug back.
func (s *Service) checkReserved(ctx context.Context, slugName, owner string) error {
	for _, r := range s.cfg.Reserved {
		if slugName == r {
			return invalidf("invalid slug: %q is reserved for the Flats console", slugName)
		}
	}
	s.mu.Lock()
	_, isPreview := s.prevs[slugName]
	r := s.redir[slugName]
	s.mu.Unlock()
	if isPreview {
		return invalidf("invalid slug: %q is currently used by a preview", slugName)
	}
	if r != nil && r.cur != owner {
		return invalidf("invalid slug: %q is currently used by a redirect to %q", slugName, r.cur)
	}
	// Redirects are kept in the database even when none is being served
	// (e.g. after a failed restart), so a new flat can never be shadowed by
	// a redirect that comes back later.
	if red, err := s.st.RedirectFor(ctx, slugName, s.now()); err == nil && red.Flat != owner {
		return invalidf("invalid slug: %q redirects to %q until %s", slugName, red.Flat, red.Until.Format("2006-01-02 15:04 MST"))
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// dbName is the server-flat database inside a data directory.
const dbName = "db.sqlite"

// snapshotData copies a flat's data directory for a preview. The database is
// copied with VACUUM INTO so a live writer (WAL mode) cannot leave the copy
// inconsistent; other files, including user files that merely look like
// databases, are copied as they are.
func snapshotData(src, dst string) error {
	if err := snapshotFiles(src, dst); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(src, dbName)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return vacuumInto(filepath.Join(src, dbName), filepath.Join(dst, dbName))
}

// snapshotFiles copies a data directory without its database.
func snapshotFiles(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	db := filepath.Join(src, dbName)
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case !info.Mode().IsRegular():
			return nil
		case p == db || p == db+"-wal" || p == db+"-shm" || p == db+"-journal":
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func vacuumInto(src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("snapshot %s: %w", filepath.Base(src), err)
	}
	return nil
}

// installDB replaces the flat's database with a copy of src ("" removes it).
// The caller makes sure nothing has the database open.
func (s *Service) installDB(slugName, src string) error {
	dataDir := s.dataDirOf(slugName)
	db := filepath.Join(dataDir, dbName)
	if src != "" {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return err
		}
		if err := copyFile(src, db+".restore"); err != nil {
			os.Remove(db + ".restore")
			return err
		}
	}
	// The old WAL belongs to the old database; it must not be replayed into
	// the new one.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(db + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if src == "" {
		if err := os.Remove(db); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.Rename(db+".restore", db)
}

// keepSnapshots bounds database snapshots per flat.
const keepSnapshots = 10

// Snapshot names are <kind>-<unix ms>.sqlite, with kind before-v<n> (taken
// before version n was deployed, or before a restore made it live),
// before-redeploy-v<n>, before-restore (older releases) or failed-v<n> (the
// data from just before a server deploy that failed its health check).
var snapshotRE = regexp.MustCompile(`^(?:before-(?:v\d+|redeploy-v\d+|restore)|failed-v\d+)-(\d+)\.sqlite$`)

// keepFailedSnapshots bounds failed-v* snapshots separately.
const keepFailedSnapshots = 3

// markFailedSnapshot renames a pre-deploy snapshot after its deploy failed
// and returns the new name.
func (s *Service) markFailedSnapshot(slugName, snap string, n int) string {
	ms := snapshotTime(snap)
	name := fmt.Sprintf("failed-v%d-%d.sqlite", n, ms)
	dir := s.snapshotDir(slugName)
	if err := os.Rename(filepath.Join(dir, snap), filepath.Join(dir, name)); err != nil {
		return snap
	}
	s.pruneSnapshots(slugName, name)
	return name
}

func validSnapshotName(name string) bool { return snapshotRE.MatchString(name) }

func snapshotTime(name string) int64 {
	m := snapshotRE.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

func (s *Service) snapshotDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "snapshots")
}

// snapshotDB copies the flat's database before version n is deployed while
// live is live. It returns "" when the flat has no database.
func (s *Service) snapshotDB(slugName string, n, live int) (string, error) {
	kind := fmt.Sprintf("before-v%d", n)
	if n == live {
		// Restarting the live version is not "before version n went live":
		// keep restore_data pointed at the snapshot from the real deploy.
		kind = fmt.Sprintf("before-redeploy-v%d", n)
	}
	keep, _ := s.liveSnapshot(slugName, live)
	return s.snapshotAs(slugName, kind, keep)
}

// snapshotAs copies the flat's database to a new snapshot named after kind
// and prunes old snapshots, never removing protect. It returns "" when the
// flat has no database.
func (s *Service) snapshotAs(slugName, kind, protect string) (string, error) {
	if _, err := os.Stat(s.dataDirOf(slugName)); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	db := filepath.Join(s.dataDirOf(slugName), dbName)
	_, dbErr := os.Stat(db)
	if dbErr != nil && !os.IsNotExist(dbErr) {
		return "", dbErr
	}
	dir := s.snapshotDir(slugName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ms := s.now().UnixMilli()
	name := fmt.Sprintf("%s-%d.sqlite", kind, ms)
	for {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			break
		}
		ms++
		name = fmt.Sprintf("%s-%d.sqlite", kind, ms)
	}
	sidecar := filepath.Join(dir, name+".data")
	if err := snapshotData(s.dataDirOf(slugName), sidecar); err != nil {
		os.RemoveAll(sidecar)
		return "", err
	}
	if dbErr == nil {
		if err := copyFile(filepath.Join(sidecar, dbName), filepath.Join(dir, name)); err != nil {
			return "", err
		}
	} else {
		marker, err := sql.Open("sqlite", filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		_, err = marker.Exec("CREATE TABLE IF NOT EXISTS flats_empty_snapshot (id INTEGER)")
		marker.Close()
		if err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(sidecar, ".flats-snapshot-complete"), []byte("1"), 0600); err != nil {
		return "", err
	}
	s.pruneSnapshots(slugName, name, protect)
	return name, nil
}

func (s *Service) pruneSnapshots(slugName string, protect ...string) {
	snaps, err := s.Snapshots(slugName) // newest first
	if err != nil {
		return
	}
	regular, failed := 0, 0
	for _, name := range snaps {
		limit, count := keepSnapshots, &regular
		if strings.HasPrefix(name, "failed-") {
			limit, count = keepFailedSnapshots, &failed
		}
		*count++
		if *count > limit && !slices.Contains(protect, name) {
			os.Remove(filepath.Join(s.snapshotDir(slugName), name))
			os.RemoveAll(filepath.Join(s.snapshotDir(slugName), name+".data"))
		}
	}
}

// Snapshots lists database snapshots, newest first.
func (s *Service) Snapshots(slugName string) ([]string, error) {
	entries, err := os.ReadDir(s.snapshotDir(slugName))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if validSnapshotName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(cmp.Compare(snapshotTime(b), snapshotTime(a)), strings.Compare(b, a))
	})
	return out, nil
}

// liveSnapshot returns the newest snapshot taken before version live was
// deployed: the one rollback with restore_data puts back.
func (s *Service) liveSnapshot(slugName string, live int) (string, error) {
	prefix := fmt.Sprintf("before-v%d-", live)
	snaps, err := s.Snapshots(slugName)
	if err != nil {
		return "", err
	}
	for _, n := range snaps {
		if strings.HasPrefix(n, prefix) {
			return n, nil
		}
	}
	return "", fmt.Errorf("%w: no database snapshot was taken before version %d was deployed", ErrConflict, live)
}

// installSnapshot restores full new snapshots; legacy snapshots only contain DB.
// FILES are preserved when the historical snapshot contains no FILES archive.
func (s *Service) installSnapshot(slugName, path string) error {
	if _, err := os.Stat(filepath.Join(path+".data", ".flats-snapshot-complete")); err == nil {
		staged := s.dataDirOf(slugName) + ".restore"
		os.RemoveAll(staged)
		if err := snapshotData(path+".data", staged); err != nil {
			return err
		}
		os.Remove(filepath.Join(staged, ".flats-snapshot-complete"))
		old := s.dataDirOf(slugName) + ".restore-old"
		os.RemoveAll(old)
		if err := os.Rename(s.dataDirOf(slugName), old); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(staged, s.dataDirOf(slugName)); err != nil {
			_ = os.Rename(old, s.dataDirOf(slugName))
			return err
		}
		return os.RemoveAll(old)
	}
	return s.installDB(slugName, path)
}
