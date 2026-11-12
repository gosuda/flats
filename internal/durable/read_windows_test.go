package durable

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The handle of a read must not stop another program from removing the file.
// A handle from os.Open does.
func TestOpenSharesDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove while the file is open: %v", err)
	}
	if got, err := io.ReadAll(f); err != nil || string(got) != "identity" {
		t.Fatalf("the open handle read %q, %v", got, err)
	}
}

// ReadFile passes the path to the system itself, without the long path
// handling of package os.
func TestReadFileLongPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state")
	if err := os.WriteFile(path, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFile(path); err != nil || string(got) != "identity" {
		t.Fatalf("read %q, %v", got, err)
	}
}
