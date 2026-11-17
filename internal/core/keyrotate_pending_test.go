package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md, rule 17: what the pending key file is.

// keyOf returns a key of 32 bytes b. Tests use goodKey (7) as the key at the key path, keyOf(8) as the key of a
// rotation and keyOf(9) as a key that sealed nothing.
func keyOf(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// labelledStore opens a flats.db in dir whose flat "app" holds one row for each label, in the order given. The
// rows hold bytes that no key opens: the checks of the labels never open a value.
func labelledStore(t *testing.T, dir string, labels ...string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	if err := st.CreateFlat(ctx, store.Flat{Slug: "app", Name: "app", Visibility: store.Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i, label := range labels {
		row := store.SealedSecret{Name: "NAME_" + string(rune('A'+i)), Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: bytes.Repeat([]byte{2}, 40), KeyID: label, UpdatedAt: now}
		if err := st.PutSecret(ctx, "app", row, store.Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// labelsOf reads the labels of st as the checks get them.
func labelsOf(t *testing.T, st *store.Store) store.KeyIDCounts {
	t.Helper()
	ids, err := st.SealedKeyIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestPendingPath(t *testing.T) {
	t.Parallel()
	data, keyDir := t.TempDir(), t.TempDir()
	if got, want := NewKeyFile(data, "").PendingPath(), filepath.Join(data, PendingKeyFile); got != want || PendingKeyFile != "secret.key.next" {
		t.Errorf("the pending path of the default key = %s, want %s", got, want)
	}
	configured := filepath.Join(keyDir, "host.key")
	if got := NewKeyFile(data, configured).PendingPath(); got != configured+".next" {
		t.Errorf("the pending path of a configured key = %s, want %s.next", got, configured)
	}
}

// The three kinds of a regular file at the pending name, and no file (rule 17).
func TestPendingKeyStates(t *testing.T) {
	t.Parallel()
	rotated, unused := keyOf(8), keyOf(9)
	for name, tc := range map[string]struct {
		keyFile []byte // nil: nothing at the key path
		pending []byte // nil: nothing at the pending name
		labels  store.KeyIDCounts
		want    PendingState
		valid   bool // the pending file is a valid key
	}{
		"nothing at the name":                        {goodKey(), nil, store.KeyIDCounts{KeyID(rotated): 2}, PendingNone, false},
		"waits beside the old key":                   {goodKey(), rotated, store.KeyIDCounts{KeyID(rotated): 2}, PendingWaits, true},
		"waits with nothing at the key path":         {nil, rotated, store.KeyIDCounts{KeyID(rotated): 2}, PendingWaits, true},
		"waits beside a damaged key file":            {[]byte("damaged"), rotated, store.KeyIDCounts{KeyID(rotated): 1}, PendingWaits, true},
		"waits when one value of several carries it": {goodKey(), rotated, store.KeyIDCounts{"": 1, KeyID(goodKey()): 1, KeyID(rotated): 1}, PendingWaits, true},
		"a duplicate of the key in place":            {rotated, rotated, store.KeyIDCounts{KeyID(rotated): 2}, PendingDuplicate, true},
		"a duplicate that labels nothing":            {rotated, rotated, nil, PendingDuplicate, true},
		"left over: a key that labels nothing":       {goodKey(), unused, store.KeyIDCounts{KeyID(goodKey()): 2}, PendingLeftOver, true},
		"left over: no sealed value":                 {goodKey(), rotated, nil, PendingLeftOver, true},
		"left over: only values without a label":     {goodKey(), rotated, store.KeyIDCounts{"": 3}, PendingLeftOver, true},
		"left over: an empty file":                   {goodKey(), []byte{}, store.KeyIDCounts{KeyID(rotated): 2}, PendingLeftOver, false},
		"left over: 31 bytes":                        {goodKey(), rotated[:31], store.KeyIDCounts{KeyID(rotated): 2}, PendingLeftOver, false},
		"left over: 33 bytes":                        {goodKey(), append(keyOf(8), 8), store.KeyIDCounts{KeyID(rotated): 2}, PendingLeftOver, false},
		"left over: a long file":                     {goodKey(), bytes.Repeat([]byte{8}, 5000), store.KeyIDCounts{KeyID(rotated): 2}, PendingLeftOver, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			k := NewKeyFile(dir, "")
			if tc.keyFile != nil {
				putFile(t, k.Path(), tc.keyFile)
			}
			if tc.pending != nil {
				putFile(t, k.PendingPath(), tc.pending)
			}
			before := listDir(t, dir)
			got, err := k.Pending(tc.labels)
			if err != nil || got.State != tc.want {
				t.Fatalf("Pending = %+v, %v; want state %d", got.State, err, tc.want)
			}
			if tc.valid {
				if !bytes.Equal(got.Key, tc.pending) || got.ID != KeyID(tc.pending) {
					t.Errorf("Pending gave the key ID %q; want the key of the file and its ID", got.ID)
				}
			} else if got.Key != nil || got.ID != "" {
				t.Errorf("Pending gave a key for a file that is no key: ID %q", got.ID)
			}
			if after := listDir(t, dir); strings.Join(after, " ") != strings.Join(before, " ") {
				t.Errorf("the read changed the folder: %v -> %v", before, after)
			}
		})
	}
}

// Something at the pending name that is no regular file, or that cannot be read, stops the reader. It is never
// treated as left over, because it can be a key that waits (rule 17).
func TestPendingKeyThatCannotBeClassified(t *testing.T) {
	t.Parallel()
	rotated := keyOf(8)
	labels := store.KeyIDCounts{KeyID(rotated): 2}
	check := func(t *testing.T, k KeyFile) {
		t.Helper()
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
			for _, part := range []string{k.PendingPath(), "move it away", "can read"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the message lacks %q: %v", what, part, err)
				}
			}
		}
	}
	t.Run("a folder", func(t *testing.T) {
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.Path(), goodKey())
		if err := os.Mkdir(k.PendingPath(), 0o700); err != nil {
			t.Fatal(err)
		}
		check(t, k)
		if err := k.RemovePending(); !errors.Is(err, ErrPendingKey) {
			t.Errorf("RemovePending of a folder: err = %v, want ErrPendingKey", err)
		}
		if fi, err := os.Lstat(k.PendingPath()); err != nil || !fi.IsDir() {
			t.Errorf("the folder is gone: %v", err)
		}
	})
	t.Run("a link to a key that the values carry", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k := NewKeyFile(dir, "")
		putFile(t, k.Path(), goodKey())
		target := filepath.Join(t.TempDir(), "elsewhere")
		putFile(t, target, rotated)
		if err := ostest.Symlink(t, target, k.PendingPath()); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		check(t, k)
	})
	t.Run("a link to nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k := NewKeyFile(dir, "")
		putFile(t, k.Path(), goodKey())
		if err := ostest.Symlink(t, filepath.Join(dir, "gone"), k.PendingPath()); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		check(t, k)
	})
	t.Run("a file that the user cannot read", func(t *testing.T) {
		needsAnUnprivilegedUnixUser(t)
		t.Parallel()
		k := NewKeyFile(t.TempDir(), "")
		putFile(t, k.Path(), goodKey())
		putFile(t, k.PendingPath(), rotated)
		if err := os.Chmod(k.PendingPath(), 0); err != nil {
			t.Fatal(err)
		}
		check(t, k)
	})
}

func pendingErr(k KeyFile, labels store.KeyIDCounts) error {
	_, err := k.Pending(labels)
	return err
}

// A key path whose folder is missing has no pending key: nothing can be at the name.
func TestPendingKeyOfAMissingFolder(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	k := NewKeyFile(data, filepath.Join(data, "..", filepath.Base(data)+"-gone", "secret.key"))
	if got, err := k.Pending(store.KeyIDCounts{KeyID(keyOf(8)): 1}); err != nil || got.State != PendingNone {
		t.Errorf("Pending = %+v, %v; want none", got.State, err)
	}
}
