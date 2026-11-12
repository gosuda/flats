package config

import "path/filepath"

// cleanDataDir returns host.data_dir in clean form. The key is validated as an
// absolute path, and a path here starts with its volume, so it takes no "/".
func cleanDataDir(p string) string { return filepath.Clean(p) }
