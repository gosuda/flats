package app

import (
	"os"
	"path/filepath"
	"testing"
)

// markDataDir makes dir a Flats data directory before a test leaves other
// entries in it: on Windows a directory with foreign content is refused.
func markDataDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "flats.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
