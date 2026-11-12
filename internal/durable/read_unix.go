//go:build !windows

package durable

import "os"

// ReadFile is os.ReadFile: an open file does not stop a rename or a removal
// here.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
