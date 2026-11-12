//go:build !windows

// Package privatedir keeps a directory private to the current user where file
// modes do not do it.
package privatedir

// Protect does nothing on Unix, where the mode of the directory decides.
func Protect(string) error { return nil }

// Restrict does nothing on Unix, where the mode of the directory decides.
func Restrict(string) error { return nil }
