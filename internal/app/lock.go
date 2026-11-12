package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gosuda/flats/internal/filelock"
	"github.com/gosuda/flats/internal/privatedir"
)

// Never unlink the lock file: replacing its inode would allow two owners.
// The kernel releases the lock when this descriptor/process closes.
func lockDataDir(dir string) (*os.File, error) {
	if err := privatedir.Protect(dir); err != nil {
		return nil, fmt.Errorf("data directory %q cannot be made private: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "flats.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("data directory lock: %w", err)
	}
	if err := tryLockDataDir(f); err != nil {
		f.Close()
		if errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("data directory %q is already in use: %w", dir, errDataDirInUse)
		}
		return nil, fmt.Errorf("data directory %q cannot be locked: %w", dir, err)
	}
	recordPID(f)
	return f, nil
}
