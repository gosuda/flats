//go:build !windows

package backup

import (
	"syscall"
	"testing"
)

// makeFIFO creates a named pipe, which a copy must neither open nor follow.
func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
