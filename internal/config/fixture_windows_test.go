package config

import (
	"path/filepath"
	"strings"
)

// The fixtures of this package use Unix absolute paths such as /var/lib/flats.
func init() {
	isAbs = func(p string) bool { return filepath.IsAbs(p) || strings.HasPrefix(p, "/") }
}
