package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeReferenceHostFilenameLimit(t *testing.T) {
	d := newDataStore(t.TempDir())
	defer d.Close()
	if err := d.filePut("seed", "x"); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", 256)
	if err := checkKey(key); err != nil {
		t.Fatalf("whole-key validation rejected 256 bytes: %v", err)
	}
	// Filesystem acceptance varies. On a host that accepts this name there is no
	// I/O error to assert; the doc deliberately does not impose a universal cap.
	if err := d.filePut(key, "x"); err == nil {
		t.Skip("host accepts a 256-byte segment; per-segment restriction is host-dependent")
	} else {
		t.Logf("validated key rejected by host put: %v", err)
	}
	if _, err := d.fileGet(key); err == nil {
		t.Fatal("host filename error incorrectly returned missing get")
	} else {
		t.Logf("get: %v", err)
	}
	if _, err := d.fileDelete(key); err == nil {
		t.Fatal("host filename error incorrectly returned missing delete")
	} else {
		t.Logf("delete: %v", err)
	}
}

func TestRuntimeReferenceListCutoffBeforeSort(t *testing.T) {
	dir := t.TempDir()
	d := newDataStore(dir)
	defer d.Close()
	parent := filepath.Join(dir, "files", "a")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	// WalkDir descends into a before visiting a.txt, although a.txt sorts first
	// among full relative keys. Exercise the actual 10,000-key cutoff.
	for i := range 10000 {
		if err := os.WriteFile(filepath.Join(parent, fmt.Sprintf("%05d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "a.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	keys, err := d.fileList("")
	if err != nil || len(keys) != 10000 || keys[0] != "a/00000" || keys[len(keys)-1] != "a/09999" {
		t.Fatalf("walk-order cutoff: count=%d, err=%v", len(keys), err)
	}
	for _, key := range keys {
		if key == "a.txt" {
			t.Fatal("list selected sorted global first 10,000 instead of walk prefix")
		}
	}
}

func TestRuntimeReferenceFilesystemIdentity(t *testing.T) {
	for _, pair := range []struct{ name, first, other string }{
		{"case", "a", "A"},
		{"normalization", "caf\u00e9", "cafe\u0301"},
	} {
		t.Run(pair.name, func(t *testing.T) {
			dir := t.TempDir()
			// Determine this volume's identity independently of FILES. Do not enforce
			// APFS semantics on Linux or assume all macOS volumes are insensitive.
			probe := filepath.Join(dir, "probe")
			if err := os.Mkdir(probe, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(probe, pair.first), []byte("first"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(probe, pair.other))
			aliases := err == nil
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			t.Logf("host %s aliases=%v", pair.name, aliases)
			d := newDataStore(dir)
			defer d.Close()
			first, other := "keys/"+pair.first, "keys/"+pair.other
			if err := d.filePut(first, "first"); err != nil {
				t.Fatal(err)
			}
			initial, err := d.fileList("keys/")
			if err != nil || len(initial) != 1 {
				t.Fatalf("initial list: %v, %v", initial, err)
			}
			got, err := d.fileGet(other)
			if err != nil || (aliases && (got == nil || *got != "first")) || (!aliases && got != nil) {
				t.Fatalf("lookup identity: %v, %v", got, err)
			}
			if err := d.filePut(other, "second"); err != nil {
				t.Fatal(err)
			}
			got, err = d.fileGet(first)
			want := "first"
			if aliases {
				want = "second"
			}
			if err != nil || got == nil || *got != want {
				t.Fatalf("overwrite identity: %v, %v; want %s", got, err, want)
			}
			keys, err := d.fileList("keys/")
			if err != nil {
				t.Fatal(err)
			}
			if aliases {
				if len(keys) != 1 || keys[0] != initial[0] {
					t.Fatalf("alias overwrite changed stored spelling: %v -> %v", initial, keys)
				}
				// A lookup alias is not necessarily a literal prefix of the stored name.
				keys, err = d.fileList(other)
				if err != nil || len(keys) != 0 {
					t.Fatalf("alias prefix must stay literal: %v, %v", keys, err)
				}
			} else if len(keys) != 2 {
				t.Fatalf("distinct names collapsed: %v", keys)
			}
			deleted, err := d.fileDelete(other)
			if err != nil || !deleted {
				t.Fatalf("delete alternate name: %v, %v", deleted, err)
			}
			got, err = d.fileGet(first)
			if err != nil || (aliases && got != nil) || (!aliases && (got == nil || *got != "first")) {
				t.Fatalf("delete identity: %v, %v", got, err)
			}
		})
	}
}
