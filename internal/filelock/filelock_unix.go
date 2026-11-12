//go:build !windows

package filelock

import (
	"errors"
	"os"
	"syscall"
)

// Lock waits for the exclusive lock on f.
func Lock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// TryLock takes the exclusive lock on f, or returns ErrLocked when another
// open file holds it.
func TryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

// Unlock releases the lock on f.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
