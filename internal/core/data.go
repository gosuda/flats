package core

import (
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
