//go:build darwin || linux

package app

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Never unlink the lock file: replacing its inode would allow two owners.
// The kernel releases the advisory lock when this descriptor/process closes.
func lockDataDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "flats.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("data directory lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data directory %q is already in use or cannot be locked: %w", dir, err)
	}
	return f, nil
}
