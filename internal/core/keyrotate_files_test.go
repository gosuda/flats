package core

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/ostest"
)

// docs/spec/secret-key-rotation.md, rules 11, 12, 13, 16 and 19: the file writes of a rotation.

func TestNewSecretKey(t *testing.T) {
	t.Parallel()
	a, err := NewSecretKey()
	if err != nil || len(a) != 32 {
		t.Fatalf("NewSecretKey = %d bytes, %v", len(a), err)
	}
	b, err := NewSecretKey()
	if err != nil || bytes.Equal(a, b) {
		t.Fatalf("a second key is the same as the first, or failed: %v", err)
	}
}

// The pending key is created with a call that fails when the name exists, and with no temporary file (rule 13).
func TestWritePending(t *testing.T) {
	t.Parallel()
	keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
		dir := filepath.Dir(k.PendingPath())
		if err := k.WritePending([]byte("short")); err == nil {
			t.Error("WritePending took a key that is not 32 bytes")
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Fatalf("the refused write left %v", got)
		}
		if err := k.WritePending(keyOf(8)); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(k.PendingPath())
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != 32 || ostest.WrongMode(fi, 0o600) {
			t.Fatalf("the pending key: %v, %v; want a regular file of 32 bytes with mode 0600", fi, err)
		}
		if got, err := os.ReadFile(k.PendingPath()); err != nil || !bytes.Equal(got, keyOf(8)) {
			t.Errorf("the pending key holds other bytes: %v", err)
		}
		if got := listDir(t, dir); !slices.Equal(got, []string{filepath.Base(k.PendingPath())}) {
			t.Errorf("the folder holds %v; want the pending key alone", got)
		}
		// The name is taken now: the second write fails and changes nothing.
		if err := k.WritePending(keyOf(9)); !os.IsExist(err) {
			t.Errorf("a second WritePending: err = %v, want os.IsExist", err)
		}
		if got, _ := os.ReadFile(k.PendingPath()); !bytes.Equal(got, keyOf(8)) {
			t.Error("the second write changed the pending key")
		}
		if err := k.RemovePending(); err != nil {
			t.Fatalf("RemovePending: %v", err)
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("the folder holds %v after the removal", got)
		}
		if err := k.RemovePending(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RemovePending with nothing there: err = %v, want fs.ErrNotExist", err)
		}
	})
}

// A link at the pending name is not followed: the create fails, and the file that the link names is not made.
func TestWritePendingDoesNotFollowALink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	k := NewKeyFile(dir, "")
	target := filepath.Join(dir, "target")
	if err := ostest.Symlink(t, target, k.PendingPath()); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := k.WritePending(keyOf(8)); !os.IsExist(err) {
		t.Errorf("WritePending over a link: err = %v, want os.IsExist", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the write went through the link: %v", err)
	}
}

// The write of step 3 goes into the file that is there: the same file before and after, with its mode, whatever
// length it had (rule 12).
func TestWriteInPlaceKeepsTheFile(t *testing.T) {
	t.Parallel()
	for name, old := range map[string][]byte{
		"over a key":         goodKey(),
		"over a longer file": bytes.Repeat([]byte{1}, 100),
		"over a short file":  []byte("part"),
		"over an empty file": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
				putFile(t, k.Path(), old)
				if !ostest.Windows {
					if err := os.Chmod(k.Path(), 0o640); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.Stat(k.Path())
				if err != nil {
					t.Fatal(err)
				}
				names := listDir(t, filepath.Dir(k.Path()))
				if err := k.WriteInPlace(keyOf(8), nil); err != nil {
					t.Fatalf("WriteInPlace: %v", err)
				}
				after, err := os.Stat(k.Path())
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) {
					t.Error("the key file is another file after the write: it was replaced")
				}
				if ostest.WrongPerm(after.Mode().Perm(), before.Mode().Perm()) {
					t.Errorf("mode %v after the write, %v before", after.Mode().Perm(), before.Mode().Perm())
				}
				if got, err := os.ReadFile(k.Path()); err != nil || !bytes.Equal(got, keyOf(8)) {
					t.Errorf("the key file holds %d bytes that are not the new key: %v", len(got), err)
				}
				if got := listDir(t, filepath.Dir(k.Path())); !slices.Equal(got, names) {
					t.Errorf("the folder changed: %v -> %v; want no temporary file", names, got)
				}
			})
		})
	}
}

// Every name of the key file gets the new key: a hard link is the same file (rule 12).
func TestWriteInPlaceReachesEveryNameOfTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	k := NewKeyFile(dir, "")
	putFile(t, k.Path(), goodKey())
	second := filepath.Join(t.TempDir(), "second-name")
	if err := os.Link(k.Path(), second); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if err := k.WriteInPlace(keyOf(8), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(second); err != nil || !bytes.Equal(got, keyOf(8)) {
		t.Errorf("the second name of the file does not hold the new key: %v", err)
	}
}

// With a key path that is a link, the file that the link names gets the key, and the link stays (rule 12).
func TestWriteInPlaceFollowsALink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	k := NewKeyFile(dir, "")
	target := filepath.Join(t.TempDir(), "mounted-key")
	putFile(t, target, goodKey())
	if err := ostest.Symlink(t, target, k.Path()); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := k.CheckWritable(); err != nil {
		t.Errorf("CheckWritable through a link: %v", err)
	}
	if err := k.WriteInPlace(keyOf(8), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, keyOf(8)) {
		t.Errorf("the file that the link names does not hold the new key: %v", err)
	}
	if fi, err := os.Lstat(k.Path()); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the key path is no link any more: %v", err)
	}

	// A link to nothing is something at the key path that is no file: the write fails and creates nothing.
	gone := filepath.Join(t.TempDir(), "gone")
	other := NewKeyFile(t.TempDir(), "")
	if err := ostest.Symlink(t, gone, other.Path()); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := other.WriteInPlace(keyOf(8), nil); err == nil {
		t.Error("WriteInPlace wrote through a link to nothing")
	}
	if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the write created the file that the link names: %v", err)
	}
}

// With nothing at the key path, the write creates the key file with mode 0600 (rule 19).
func TestWriteInPlaceCreatesAMissingKeyFile(t *testing.T) {
	t.Parallel()
	keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
		if err := k.CheckWritable(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("CheckWritable with nothing at the key path: err = %v, want fs.ErrNotExist", err)
		}
		if err := k.WriteInPlace(keyOf(8), nil); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(k.Path())
		if err != nil || !fi.Mode().IsRegular() || ostest.WrongMode(fi, 0o600) {
			t.Fatalf("the created key file: %v, %v; want a regular file with mode 0600", fi, err)
		}
		if got, err := os.ReadFile(k.Path()); err != nil || !bytes.Equal(got, keyOf(8)) {
			t.Errorf("the created key file holds other bytes: %v", err)
		}
		if got := listDir(t, filepath.Dir(k.Path())); !slices.Equal(got, []string{filepath.Base(k.Path())}) {
			t.Errorf("the folder holds %v; want the key file alone", got)
		}
	})
}

// The read after the write is compared with the key. Bytes that differ, and a stop between the write and the read,
// are errors (step 3 of rule 16). The seam stands for another writer and for a stop of the process.
func TestWriteInPlaceReadsTheFileAgain(t *testing.T) {
	t.Parallel()
	for name, change := range map[string][]byte{
		"another key":  keyOf(9),
		"a short file": []byte("cut"),
		"a long file":  append(keyOf(8), 8),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := NewKeyFile(t.TempDir(), "")
			putFile(t, k.Path(), goodKey())
			err := k.WriteInPlace(keyOf(8), func() error {
				// The write is complete and synced when the seam runs.
				if got, err := os.ReadFile(k.Path()); err != nil || !bytes.Equal(got, keyOf(8)) {
					t.Errorf("the key file at the seam does not hold the new key: %v", err)
				}
				return os.WriteFile(k.Path(), change, 0o600)
			})
			if err == nil || !strings.Contains(err.Error(), "does not hold the key") || !strings.Contains(err.Error(), k.Path()) {
				t.Errorf("err = %v, want the error of the comparison that names the file", err)
			}
		})
	}
	t.Run("a stop at the seam", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.Path(), goodKey())
		stop := errors.New("stopped")
		if err := k.WriteInPlace(keyOf(8), func() error { return stop }); !errors.Is(err, stop) {
			t.Errorf("err = %v, want the error of the seam", err)
		}
	})
	t.Run("a key of the wrong size", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.Path(), goodKey())
		if err := k.WriteInPlace([]byte("short"), nil); err == nil {
			t.Error("WriteInPlace took a key that is not 32 bytes")
		}
		if got, _ := os.ReadFile(k.Path()); !bytes.Equal(got, goodKey()) {
			t.Error("the refused write changed the key file")
		}
	})
	t.Run("a folder at the key path", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		if err := os.Mkdir(k.Path(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := k.CheckWritable(); err == nil {
			t.Error("CheckWritable passed a folder")
		}
		if err := k.WriteInPlace(keyOf(8), nil); err == nil {
			t.Error("WriteInPlace wrote to a folder")
		}
	})
}

// The check before the first write opens the key file for writing and changes nothing. A file that the user cannot
// write fails it, and the write fails too and leaves the key (rule 12).
func TestCheckWritable(t *testing.T) {
	t.Parallel()
	k := NewKeyFile(t.TempDir(), "")
	putFile(t, k.Path(), goodKey())
	before, err := os.Stat(k.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.CheckWritable(); err != nil {
		t.Fatalf("CheckWritable of a file that can be written: %v", err)
	}
	after, err := os.Stat(k.Path())
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != 32 {
		t.Errorf("the check changed the file: %v -> %v, %v", before.ModTime(), after.ModTime(), err)
	}
	if got, _ := os.ReadFile(k.Path()); !bytes.Equal(got, goodKey()) {
		t.Error("the check changed the key")
	}

	t.Run("a read-only file", func(t *testing.T) {
		if !ostest.Windows && os.Geteuid() == 0 {
			t.Skip("the superuser writes a file of any mode")
		}
		// Mode 0400 is the read-only attribute on Windows.
		if err := os.Chmod(k.Path(), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(k.Path(), 0o600) })
		if err := k.CheckWritable(); err == nil {
			t.Error("CheckWritable passed a file that cannot be written")
		}
		if err := k.WriteInPlace(keyOf(8), nil); err == nil {
			t.Error("WriteInPlace wrote a file that cannot be written")
		}
		if got, err := os.ReadFile(k.Path()); err != nil || !bytes.Equal(got, goodKey()) {
			t.Errorf("the key changed: %v", err)
		}
		if got := listDir(t, filepath.Dir(k.Path())); !slices.Equal(got, []string{SecretKeyFile}) {
			t.Errorf("the folder holds %v", got)
		}
	})
}

// For a configured path, the folder of the pending key must not lie inside the data directory once links are
// resolved (rule 13).
func TestCheckPendingPlace(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	if err := NewKeyFile(data, "").CheckPendingPlace(); err != nil {
		t.Errorf("the default path: %v", err)
	}
	if err := NewKeyFile(data, filepath.Join(keyDir, "host.key")).CheckPendingPlace(); err != nil {
		t.Errorf("a folder outside the data directory: %v", err)
	}
	inner := filepath.Join(data, "keys")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	err := NewKeyFile(data, filepath.Join(inner, "host.key")).CheckPendingPlace()
	if err == nil || !strings.Contains(err.Error(), "inside the data directory") || !strings.Contains(err.Error(), "host.key.next") {
		t.Errorf("a folder in the data directory: err = %v, want a refusal that names the pending key", err)
	}
	if err := NewKeyFile(data, filepath.Join(keyDir, "missing", "host.key")).CheckPendingPlace(); err == nil {
		t.Error("a folder that does not exist passed")
	}
	t.Run("a folder that is a link into the data directory", func(t *testing.T) {
		link := filepath.Join(keyDir, "link")
		if err := ostest.Symlink(t, inner, link); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		err := NewKeyFile(data, filepath.Join(link, "host.key")).CheckPendingPlace()
		if err == nil || !strings.Contains(err.Error(), "inside the data directory") {
			t.Errorf("err = %v, want a refusal", err)
		}
		if got := listDir(t, inner); len(got) != 0 {
			t.Errorf("the check wrote %v", got)
		}
	})
}
