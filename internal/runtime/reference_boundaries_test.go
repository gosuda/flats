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
