package core

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md, rule 17: a key path that is a symbolic link to the pending key file holds the
// bytes of a duplicate, but the removal of the pending key would leave no key. No reader classifies it.
func TestAKeyPathThatLeadsToThePendingKey(t *testing.T) {
	t.Parallel()
	rotated := keyOf(8)
	labels := store.KeyIDCounts{KeyID(rotated): 2}
	t.Run("a link to the pending key file", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.PendingPath(), rotated)
		if err := ostest.Symlink(t, k.PendingPath(), k.Path()); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		for what, err := range map[string]error{
			"Pending":              pendingErr(k, labels),
			"CheckRotation":        CheckRotation(k, labels),
			"CheckStoppedRotation": CheckStoppedRotation(k, labels),
			"with no label":        CheckRotation(k, nil),
		} {
			if !errors.Is(err, ErrPendingKey) || errors.Is(err, ErrRotationStopped) {
				t.Errorf("%s: err = %v, want ErrPendingKey", what, err)
				continue
			}
			for _, part := range []string{k.Path() + " is a link that leads to the pending key file " + k.PendingPath(), "put a real copy of that file at the key path"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the message lacks %q: %v", what, part, err)
				}
			}
		}
		if got, err := os.ReadFile(k.PendingPath()); err != nil || !bytes.Equal(got, rotated) {
			t.Errorf("the pending key changed: %v", err)
		}
	})
	t.Run("a link to another file with the same key", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.PendingPath(), rotated)
		target := filepath.Join(t.TempDir(), "elsewhere.key")
		putFile(t, target, rotated)
		if err := ostest.Symlink(t, target, k.Path()); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		if got, err := k.Pending(labels); err != nil || got.State != PendingDuplicate {
			t.Errorf("Pending = %+v, %v; want a duplicate", got.State, err)
		}
	})
	t.Run("a hard link of the pending key file", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.PendingPath(), rotated)
		if err := os.Link(k.PendingPath(), k.Path()); err != nil {
			t.Skipf("no hard links here: %v", err)
		}
		if got, err := k.Pending(labels); err != nil || got.State != PendingDuplicate {
			t.Fatalf("Pending = %+v, %v; want a duplicate", got.State, err)
		}
		// The key path keeps the key when the pending name goes.
		if err := k.RemovePending(); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(k.Path()); err != nil || !bytes.Equal(got, rotated) {
			t.Errorf("the key path lost the key with the pending name: %v", err)
		}
	})
}

// The write in place leaves exactly the key in a file that was longer, which a stop inside an earlier write can
// leave (step 3 of rule 16).
func TestWriteInPlaceCutsALongerFile(t *testing.T) {
	t.Parallel()
	k := NewKeyFile(t.TempDir(), "")
	putFile(t, k.Path(), bytes.Repeat([]byte{1}, 40))
	if err := k.WriteInPlace(keyOf(8), nil); err != nil {
		t.Fatalf("WriteInPlace over a longer file: %v", err)
	}
	if got, err := os.ReadFile(k.Path()); err != nil || !bytes.Equal(got, keyOf(8)) {
		t.Errorf("the key file holds %d bytes after the write, want the 32 bytes of the key: %v", len(got), err)
	}
}

// A pending key file that WritePending created and could not complete is reported as a file that stays, and a create
// that failed is not.
func TestWritePendingSaysWhetherItsFileStays(t *testing.T) {
	t.Parallel()
	k := NewKeyFile(t.TempDir(), "")
	putFile(t, k.PendingPath(), []byte("taken"))
	if err := k.WritePending(keyOf(8)); err == nil || errors.Is(err, ErrPendingKeyStays) || !os.IsExist(err) {
		t.Errorf("WritePending over a name that is taken: err = %v, want the error of the create alone", err)
	}
}
