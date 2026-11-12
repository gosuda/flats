package filelock

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// The lock covers one byte past any content, so that it never blocks a reader
// of the file: LockFileEx locks are mandatory, unlike flock.
const lockOffset = math.MaxInt64

func lockFile(f *os.File, flags uint32) error {
	overlapped := windows.Overlapped{Offset: lockOffset & math.MaxUint32, OffsetHigh: lockOffset >> 32}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &overlapped)
}

// Lock waits for the exclusive lock on f.
func Lock(f *os.File) error {
	return lockFile(f, windows.LOCKFILE_EXCLUSIVE_LOCK)
}

// TryLock takes the exclusive lock on f, or returns ErrLocked when another
// open file holds it.
func TryLock(f *os.File) error {
	err := lockFile(f, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLocked
	}
	return err
}

// Unlock releases the lock on f.
func Unlock(f *os.File) error {
	overlapped := windows.Overlapped{Offset: lockOffset & math.MaxUint32, OffsetHigh: lockOffset >> 32}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
