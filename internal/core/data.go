package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// checkReserved rejects slugs that collide with system host names or with an
// open preview host.
func (s *Service) checkReserved(slugName string) error {
	for _, r := range s.cfg.Reserved {
		if slugName == r {
			return fmt.Errorf("invalid slug: %q is reserved for the Flats console", slugName)
		}
	}
	s.mu.Lock()
	_, isPreview := s.prevs[slugName]
	_, isRedirect := s.redir[slugName]
	s.mu.Unlock()
	if isPreview || isRedirect {
		return fmt.Errorf("invalid slug: %q is currently used by a preview or redirect", slugName)
	}
	return nil
}

// snapshotData copies a flat's data directory for a preview. SQLite databases
// are copied with VACUUM INTO so a live writer (WAL mode) cannot leave the
// copy inconsistent; other files are copied as-is.
func snapshotData(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
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
		case strings.HasSuffix(p, "-wal") || strings.HasSuffix(p, "-shm") || strings.HasSuffix(p, "-journal"):
			return nil // folded into the VACUUM INTO copy
		case strings.HasSuffix(p, ".sqlite") || strings.HasSuffix(p, ".db"):
			return vacuumInto(p, target)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
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

// keepSnapshots bounds pre-deploy database snapshots per flat.
const keepSnapshots = 10

func (s *Service) snapshotDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "snapshots")
}

// snapshotDB copies the flat's database before version n goes live. It
// returns "" when the flat has no database.
func (s *Service) snapshotDB(slugName string, n int) (string, error) {
	db := filepath.Join(s.dataDirOf(slugName), "db.sqlite")
	if _, err := os.Stat(db); err != nil {
		return "", nil
	}
	dir := s.snapshotDir(slugName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("before-v%d-%d.sqlite", n, s.now().UnixMilli())
	if err := vacuumInto(db, filepath.Join(dir, name)); err != nil {
		return "", err
	}
	s.pruneSnapshots(dir)
	return name, nil
}

func (s *Service) pruneSnapshots(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= keepSnapshots {
		return
	}
	// Names embed the time, but sort by mod time to be safe.
	type ent struct {
		name string
		mod  int64
	}
	var es []ent
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			es = append(es, ent{e.Name(), info.ModTime().UnixNano()})
		}
	}
	for i := 0; i < len(es); i++ {
		for j := i + 1; j < len(es); j++ {
			if es[j].mod < es[i].mod {
				es[i], es[j] = es[j], es[i]
			}
		}
	}
	for _, e := range es[:len(es)-keepSnapshots] {
		os.Remove(filepath.Join(dir, e.name))
	}
}

// Snapshots lists pre-deploy database snapshots, newest first.
func (s *Service) Snapshots(slugName string) ([]string, error) {
	entries, err := os.ReadDir(s.snapshotDir(slugName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i].Name())
	}
	return out, nil
}

// restoreSnapshot puts back the database snapshot taken before version live
// was deployed. The running instance is stopped first so nothing writes to
// the database while it is replaced (the flat answers 503 until the
// rollback deploy finishes).
func (s *Service) restoreSnapshot(ctx context.Context, slugName string, live int) error {
	prefix := fmt.Sprintf("before-v%d-", live)
	snaps, err := s.Snapshots(slugName)
	if err != nil {
		return err
	}
	var pick string
	for _, n := range snaps { // newest first
		if strings.HasPrefix(n, prefix) {
			pick = n
			break
		}
	}
	if pick == "" {
		return fmt.Errorf("%w: no database snapshot was taken before version %d was deployed", ErrConflict, live)
	}
	lf := s.state(slugName)
	if d := lf.cur.Swap(nil); d != nil && d.inst != nil {
		d.inst.Stop()
	}
	dataDir := s.dataDirOf(slugName)
	db := filepath.Join(dataDir, "db.sqlite")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		os.Remove(db + suffix)
	}
	data, err := os.ReadFile(filepath.Join(s.snapshotDir(slugName), pick))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	tmp := db + ".restore"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, db); err != nil {
		return err
	}
	s.Event(ctx, slugName, "warn", "snapshot", "restored database snapshot "+pick, nil)
	return nil
}
