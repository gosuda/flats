package design

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MaxFiles is the number of files a design may hold.
const MaxFiles = 512

var segment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// ValidPath reports whether p follows the portable path grammar of design
// files: up to 8 segments of [A-Za-z0-9_][A-Za-z0-9._-]*, 255 bytes in all,
// no dot segments and no reserved names.
func ValidPath(p string) error {
	if p == "" || len(p) > 255 {
		return fmt.Errorf("path %q must be 1 to 255 bytes", p)
	}
	parts := strings.Split(p, "/")
	if len(parts) > 8 {
		return fmt.Errorf("path %q has more than 8 segments", p)
	}
	for _, s := range parts {
		if s == "." || s == ".." || !segment.MatchString(s) {
			return fmt.Errorf("path %q: each segment uses letters, digits, '_', '-' and '.', starting with a letter, digit or '_'", p)
		}
	}
	lower := strings.ToLower(p)
	if lower == "flats.json" || parts[0] == "_design" || strings.EqualFold(parts[0], ".well-known") {
		return fmt.Errorf("path %q is reserved", p)
	}
	return nil
}

// CheckTree reports paths that differ only in letter case, and files that
// are also a directory prefix of another file.
func CheckTree(paths []string) error {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	seen := map[string]string{}
	for _, p := range sorted {
		k := strings.ToLower(p)
		if q, ok := seen[k]; ok && q != p {
			return fmt.Errorf("paths %q and %q differ only in letter case", q, p)
		}
		seen[k] = p
	}
	dirs := map[string]bool{}
	for _, p := range sorted {
		parts := strings.Split(strings.ToLower(p), "/")
		for i := 1; i < len(parts); i++ {
			dirs[strings.Join(parts[:i], "/")] = true
		}
	}
	for _, p := range sorted {
		if dirs[strings.ToLower(p)] {
			return fmt.Errorf("%q is a file and also a folder of other files", p)
		}
	}
	if len(paths) > MaxFiles {
		return fmt.Errorf("a design holds at most %d files; this one would hold %d", MaxFiles, len(paths))
	}
	return nil
}

// IsArtboardPath reports whether p is an HTML file.
func IsArtboardPath(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".html")
}
