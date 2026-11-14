package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
)

// goodCopy makes a copy of a populated host that passes every check.
func goodCopy(t *testing.T) (*fixture, string) {
	t.Helper()
	f := populated(t)
	_, out := f.mustRun("c1")
	got, err := Verify(context.Background(), out)
	if err != nil || len(got.Failures) > 0 {
		t.Fatalf("a fresh copy fails its check: %+v, %v", got, err)
	}
	return f, out
}

// rehash makes the manifest agree with the files again after a test changed
// some, so that only the check under test fails. It drops the entries of files
// that are gone.
func rehash(t *testing.T, out string) {
	t.Helper()
	m, err := readManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	files := m.Files[:0]
	for _, f := range m.Files {
		got, err := hashFile(filepath.Join(out, filepath.FromSlash(f.Path)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got.Path = f.Path
		files = append(files, got)
	}
	m.Files = files
	if err := os.Remove(filepath.Join(out, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if err := m.write(out); err != nil {
		t.Fatal(err)
	}
}

// failuresOf runs Verify and returns what it found.
func failuresOf(t *testing.T, out string) []Failure {
	t.Helper()
	got, err := Verify(context.Background(), out)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return got.Failures
}

func wantFailure(t *testing.T, fails []Failure, path, reason string) {
	t.Helper()
	for _, f := range fails {
		if f.Path == path && strings.Contains(f.Reason, reason) {
			return
		}
	}
	t.Errorf("no failure for %s with %q; got %v", path, reason, fails)
}

func TestVerifyFindsAChangedByte(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	p := filepath.Join(out, "data", "flats", "s", "data", "files", "notes.txt")
	if err := os.WriteFile(p, []byte("guest filE"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantFailure(t, failuresOf(t, out), "data/flats/s/data/files/notes.txt", "another SHA-256")
	if err := os.WriteFile(p, []byte("longer than before"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantFailure(t, failuresOf(t, out), "data/flats/s/data/files/notes.txt", "bytes")
}

func TestVerifyFindsAMissingAndAnExtraFile(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	if err := os.Remove(filepath.Join(out, "data", "tsnet", "node.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "data", "extra.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fails := failuresOf(t, out)
	wantFailure(t, fails, "data/tsnet/node.json", "is missing")
	wantFailure(t, fails, "data/extra.txt", "is not listed")
}

func TestVerifyNeedsAManifest(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	if err := os.Remove(filepath.Join(out, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	wantFailure(t, failuresOf(t, out), ManifestFile, "missing")
}

func TestVerifyRefusesAFormatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	p := filepath.Join(out, ManifestFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["format"] = FormatVersion + 1
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Verify(context.Background(), out)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestVerifyRefusesAnotherSchemaVersion(t *testing.T) {
	t.Parallel()
	for name, version := range map[string]string{"newer": "9999", "older": "1"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, out := goodCopy(t)
			// Set the version in the copy through the helper, which opens the file read-write.
			(&fixture{t: t, data: filepath.Join(out, "data")}).sql(nameFlatsDB, `PRAGMA user_version = `+version)
			rehash(t, out)
			_ = f
			before := snapshotTree(t, out)
			_, err := Verify(context.Background(), out)
			var refused *RefusedError
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "vtest") {
				t.Fatalf("err = %v, want a refusal that names the Flats version of the manifest", err)
			}
			if after := snapshotTree(t, out); !slices.Equal(before, after) {
				t.Errorf("the refusal changed the copy:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestVerifyFindsADamagedDatabase(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	p := filepath.Join(out, "data", "flats", "s", "data", nameDataDB)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 100; i < len(raw); i++ { // keep the header, break every page after it
		raw[i] = 0xff
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rehash(t, out)
	var found bool
	for _, f := range failuresOf(t, out) {
		if f.Path == "data/flats/s/data/db.sqlite" && (strings.Contains(f.Reason, "quick_check") || strings.Contains(f.Reason, "does not open")) {
			found = true
		}
	}
	if !found {
		t.Error("no failure for the damaged database")
	}
}

func TestVerifyFindsAnOutboxBehindItsCursor(t *testing.T) {
	t.Parallel()
	for slug, rel := range map[string]string{
		"b": "data/flats/b/board/board.db", "n": "data/flats/n/notebook/notebook.db", "d": "data/flats/d/data/db.sqlite",
	} {
		t.Run(slug, func(t *testing.T) {
			t.Parallel()
			_, out := goodCopy(t)
			(&fixture{t: t, data: filepath.Join(out, "data")}).sql(nameFlatsDB, `UPDATE content_state SET import_seq = 99 WHERE flat = '`+slug+`'`)
			rehash(t, out)
			wantFailure(t, failuresOf(t, out), rel, "below the import cursor 99")
		})
	}
}

// store.Inspect opens a database that has a -wal file without immutable=1, which creates a -shm file. A check
// of a copy writes nothing (rule 25), so it reports the stray file and leaves the copy as it was.
func TestVerifyWritesNothingWhenTheCopyHoldsAWalFile(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	if err := os.WriteFile(filepath.Join(out, "data", nameFlatsDB+"-wal"), []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, out)
	fails := failuresOf(t, out)
	wantFailure(t, fails, "data/flats.db", "flats.db-wal beside it")
	wantFailure(t, fails, "data/flats.db-wal", "is not listed")
	if after := snapshotTree(t, out); !slices.Equal(before, after) {
		t.Errorf("Verify changed the copy:\nbefore %v\nafter  %v", before, after)
	}
}

// The outbox has acknowledged entries that flats.db has not imported, so their revisions are in no
// database of the copy.
func TestVerifyFindsAnOutboxAheadOfItsCursor(t *testing.T) {
	t.Parallel()
	for slug, rel := range map[string]string{
		"b": "data/flats/b/board/board.db", "n": "data/flats/n/notebook/notebook.db", "d": "data/flats/d/data/db.sqlite",
	} {
		t.Run(slug, func(t *testing.T) {
			t.Parallel()
			_, out := goodCopy(t)
			(&fixture{t: t, data: filepath.Join(out, "data")}).sql(nameFlatsDB, `UPDATE content_state SET import_seq = 1 WHERE flat = '`+slug+`'`)
			rehash(t, out)
			wantFailure(t, failuresOf(t, out), rel, "has acknowledged entries through")
		})
	}
}

func TestVerifyFindsAMissingBlob(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	blobs, err := filepath.Glob(filepath.Join(out, "data", "flats", "b", "revisions", "blobs", "*", "*"))
	if err != nil || len(blobs) != 1 {
		t.Fatalf("blobs = %v, %v", blobs, err)
	}
	if err := os.Remove(blobs[0]); err != nil {
		t.Fatal(err)
	}
	rehash(t, out)
	fails := failuresOf(t, out)
	rel, _ := filepath.Rel(out, blobs[0])
	wantFailure(t, fails, filepath.ToSlash(rel), "needs it")
	if n := len(fails); n != 2 { // flats.db and board.db both name the blob
		t.Errorf("%d failures, want 2: %v", n, fails)
	}
}

func TestVerifyChecksSealedValuesAgainstTheKey(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	(&fixture{t: t, data: filepath.Join(out, "data")}).sql(nameFlatsDB, `INSERT INTO secrets(flat, name, nonce, ciphertext, updated_at) VALUES('s', 'API_KEY', x'00', x'01', 0)`)
	rehash(t, out)
	wantFailure(t, failuresOf(t, out), "data/flats.db", "secret API_KEY of s does not open")
}

// A host whose key does not open a sealed value cannot be backed up, and running
// the backup again does not change that, so the message does not say so.
func TestRunFailsWhenTheKeyDoesNotOpenASealedValue(t *testing.T) {
	t.Parallel()
	f := populated(t)
	f.sql(nameFlatsDB, `INSERT INTO secrets(flat, name, nonce, ciphertext, updated_at) VALUES('s', 'API_KEY', x'00', x'01', 0)`)
	_, err := f.run(f.options("c1"))
	var rerun *RerunError
	if err == nil || errors.As(err, &rerun) || !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("err = %v, want a failure that does not ask to run again", err)
	}
	f.nothingLeft()
}

func TestVerifyChangesNothing(t *testing.T) {
	t.Parallel()
	_, out := goodCopy(t)
	before := snapshotTree(t, out)
	for i := 0; i < 2; i++ {
		if fails := failuresOf(t, out); len(fails) > 0 {
			t.Fatal(fails)
		}
	}
	after := snapshotTree(t, out)
	if !slices.Equal(before, after) {
		t.Errorf("Verify changed the copy:\nbefore %v\nafter  %v", before, after)
	}
}

// snapshotTree lists every path under root with its size, modification time and
// directory flag. On Windows it leaves out the modification time of a directory:
// the system finishes the delete of a file, such as the -wal file that the test
// helper leaves behind, some time after the call returns, and then changes it.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		mod := fi.ModTime().UTC().Format(time.RFC3339Nano)
		if fi.IsDir() && ostest.Windows {
			mod = ""
		}
		out = append(out, rel+"|"+fi.Mode().String()+"|"+mod+"|"+strings.Repeat("d", map[bool]int{true: 1}[fi.IsDir()]))
		if !fi.IsDir() {
			out[len(out)-1] += "|" + itoa(fi.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
