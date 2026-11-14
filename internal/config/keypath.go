package config

import (
	"path/filepath"
	"runtime"
	"strings"
)

// cleanKeyPath returns host.secret_key_path in clean form, or "" for no path.
func cleanKeyPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// insideDir reports whether child is dir or lies below it. It compares the two
// cleaned paths and does not resolve links. On Windows it ignores case.
func insideDir(dir, child string) bool {
	dir, child = filepath.Clean(dir), filepath.Clean(child)
	if runtime.GOOS == "windows" {
		dir, child = strings.ToLower(dir), strings.ToLower(child)
	}
	rel, err := filepath.Rel(dir, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
