package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/ostest"
)

// The two writes of WriteKeyCopy, apart, for a caller that must say which of them failed
// (docs/spec/secret-key-rotation.md, rule 20). The first writes the copy alone and leaves the ID file as it is.
// The second writes the ID of the key alone.
func TestTheTwoWritesOfTheKeyCopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oldKey, newKey := keyOf(1), keyOf(2)
	if _, err := WriteKeyCopy(dir, []byte("copy of the old key"), oldKey); err != nil {
		t.Fatal(err)
	}
	copyPath, idPath := filepath.Join(dir, RecoveryKeyFile), filepath.Join(dir, RecoveryKeyIDFile)
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	replaced, err := WriteKeyCopyFile(dir, []byte("copy of the new key"))
	if err != nil || !replaced {
		t.Fatalf("WriteKeyCopyFile = %v, %v; want a copy that was replaced", replaced, err)
	}
	if got := read(copyPath); got != "copy of the new key" {
		t.Errorf("the copy holds %q", got)
	}
	// Between the two writes the ID does not match, and the start reports it.
	if got := read(idPath); got != KeyID(oldKey)+"\n" {
		t.Errorf("the first write changed the ID file: %q", got)
	}
	if w := RecoveryCopyWarning(dir, newKey); w == "" {
		t.Error("no warning between the two writes")
	}

	if err := WriteKeyCopyID(dir, newKey); err != nil {
		t.Fatal(err)
	}
	if got := read(idPath); got != KeyID(newKey)+"\n" {
		t.Errorf("the ID file holds %q", got)
	}
	if got := read(copyPath); got != "copy of the new key" {
		t.Errorf("the second write changed the copy: %q", got)
	}
	if w := RecoveryCopyWarning(dir, newKey); w != "" {
		t.Errorf("a warning after both writes: %s", w)
	}
	for _, p := range []string{copyPath, idPath} {
		if fi, err := os.Stat(p); err != nil || ostest.WrongMode(fi, 0o600) {
			t.Errorf("the mode of %s: %v, %v", p, fi, err)
		}
	}
	if names, _ := os.ReadDir(dir); len(names) != 2 {
		t.Errorf("the two writes left %d files, want the copy and its ID", len(names))
	}

	// In an empty folder the first write reports no copy that it replaced, and writes no ID file.
	empty := t.TempDir()
	if replaced, err := WriteKeyCopyFile(empty, []byte("first")); err != nil || replaced {
		t.Fatalf("WriteKeyCopyFile in an empty folder = %v, %v", replaced, err)
	}
	if _, err := os.Lstat(filepath.Join(empty, RecoveryKeyIDFile)); !os.IsNotExist(err) {
		t.Errorf("the first write wrote an ID file: %v", err)
	}
}
