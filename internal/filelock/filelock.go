// Package filelock holds an exclusive lock on an open file for as long as the
// file stays open. The operating system releases the lock when the process
// ends, so a crash leaves no stale lock.
package filelock

import "errors"

// ErrLocked reports that another open file holds the lock.
var ErrLocked = errors.New("file is locked")
