//go:build !windows

package durable

import "os"

// Rename is os.Rename.
func Rename(from, to string) error { return os.Rename(from, to) }

// RemoveAll is os.RemoveAll.
func RemoveAll(path string) error { return os.RemoveAll(path) }
