//go:build windows

package backup

import "testing"

// makeFIFO does nothing: Windows has no named pipe in the file system that
// os.Lstat reports as one.
func makeFIFO(t *testing.T, path string) { t.Helper() }
