//go:build !windows

package config

import "path/filepath"

// cleanDataDir returns host.data_dir in clean form. The key is validated as an
// absolute path, so the leading "/" changes nothing; filepath.Clean("/" + p) is
// also the form static analysis accepts as free of ".." traversal for the paths
// Flats builds under the directory.
func cleanDataDir(p string) string { return filepath.Clean("/" + p) }
