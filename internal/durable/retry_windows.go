package durable

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// retryFor bounds the wait for another process. A virus scanner or the search
// indexer opens a new file for a moment, and Windows then refuses to rename or
// remove it, often as access denied. A real denial reads the same, so it costs
// this wait once per call.
const retryFor = 2 * time.Second

func transient(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func retry(op func() error) error {
	deadline := time.Now().Add(retryFor)
	for wait := 5 * time.Millisecond; ; wait *= 2 {
		err := op()
		if err == nil || !transient(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(min(wait, 200*time.Millisecond))
	}
}

// Rename is os.Rename with a short retry.
func Rename(from, to string) error {
	return retry(func() error { return os.Rename(from, to) })
}

// RemoveAll is os.RemoveAll with a short retry.
func RemoveAll(path string) error {
	return retry(func() error { return os.RemoveAll(path) })
}
