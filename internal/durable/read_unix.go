//go:build !windows

package durable

import "os"

// Open is os.Open: an open file does not stop a rename or a removal here.
func Open(path string) (*os.File, error) { return os.Open(path) }

// ReadFile is os.ReadFile: an open file does not stop a rename or a removal
// here.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
