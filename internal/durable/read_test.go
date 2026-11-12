package durable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if _, err := ReadFile(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if err := os.WriteFile(path, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFile(path); err != nil || string(got) != "identity" {
		t.Fatalf("read %q, %v", got, err)
	}
}
