package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFile(t *testing.T, path, data string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return Hash([]byte(data))
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func historyFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, historyDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func set(key, value string) func(*Document) error {
	return func(d *Document) error { return d.Set(key, value) }
}

func TestCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "config")
	path := filepath.Join(dir, "config.json")
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := Create(path, doc)
	if err != nil {
		t.Fatal(err)
	}
	data := readString(t, path)
	if data != string(doc.Encode()) || hash != Hash([]byte(data)) {
		t.Fatalf("Create wrote %q hash %s", data, hash)
	}
	if m := mode(t, path); m != 0o600 {
		t.Errorf("file mode = %v, want 0600", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode = %v, want 0700", m)
	}

	other, _ := New(NewInstanceID(), "/srv/other")
	if _, err := Create(path, other); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second Create = %v, want ErrExist", err)
	}
	if readString(t, path) != data {
		t.Fatal("second Create changed the file")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}

	bad, _ := New(testID, "/x")
	bad.Set("portal.discovery", "false")
	if _, err := Create(filepath.Join(t.TempDir(), "config.json"), bad); errPath(err) != "portal.discovery" {
		t.Errorf("Create of an invalid document = %v", err)
	}
}

func TestSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := fileWith("system", `"keep_versions": 3`)
	hash := writeFile(t, path, orig)
	w := NewWriter(path)

	if _, err := w.Save(Hash([]byte("stale")), set("system.keep_versions", "4")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale hash: err = %v, want ErrConflict", err)
	}
	var ce *ConflictError
	if _, err := w.Save("", nil); !errors.As(err, &ce) || ce.Actual != hash {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := w.Save(hash, set("network.private_backend", "tailscale")); errPath(err) != "network.private_backend" {
		t.Fatalf("invalid result: err = %v", err)
	}
	boom := errors.New("boom")
	if _, err := w.Save(hash, func(*Document) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("mutate error: err = %v", err)
	}
	if readString(t, path) != orig || historyFiles(t, dir) != nil {
		t.Fatal("a rejected save changed the file or history")
	}

	newHash, err := w.Save(hash, set("system.keep_versions", "4"))
	if err != nil {
		t.Fatal(err)
	}
	data := readString(t, path)
	if newHash != Hash([]byte(data)) || !strings.Contains(data, `"keep_versions": 4`) {
		t.Fatalf("saved %q hash %s", data, newHash)
	}
	if m := mode(t, path); m != 0o600 {
		t.Errorf("file mode = %v, want 0600", m)
	}
	hist := historyFiles(t, dir)
	if len(hist) != 1 || !historyName.MatchString(hist[0]) || !strings.HasPrefix(hist[0], "config.v1.") {
		t.Fatalf("history = %v", hist)
	}
	if readString(t, filepath.Join(dir, historyDir, hist[0])) != orig {
		t.Error("history copy is not the previous file")
	}
	if m := mode(t, filepath.Join(dir, historyDir)); m != 0o700 {
		t.Errorf("history dir mode = %v, want 0700", m)
	}

	// Saving canonical bytes again writes nothing.
	again, err := w.Save(newHash, set("system.keep_versions", "4"))
	if err != nil || again != newHash || len(historyFiles(t, dir)) != 1 {
		t.Fatalf("no-op save: hash %s err %v history %v", again, err, historyFiles(t, dir))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestSaveSerialized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	hash := writeFile(t, path, fileWith())
	w := NewWriter(path)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			<-start
			_, errs[i] = w.Save(hash, set("system.keep_versions", []string{"1", "2"}[i]))
		})
	}
	close(start)
	wg.Wait()
	ok, conflict := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("results = %v, want one success and one conflict", errs)
	}
}

func TestHistoryPruning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	hash := writeFile(t, path, fileWith())
	if err := os.MkdirAll(filepath.Join(dir, historyDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, historyDir, "notes.txt"), "keep me")
	w := NewWriter(path)
	var prev string
	for i := range 13 {
		prev = readString(t, path)
		var err error
		if hash, err = w.Save(hash, set("system.keep_versions", string(rune('1'+i%9))+"0")); err != nil {
			t.Fatal(err)
		}
	}
	hist := historyFiles(t, dir)
	if len(hist) != historyKeep+1 {
		t.Fatalf("history = %d files %v, want %d copies and notes.txt", len(hist), hist, historyKeep)
	}
	newest := hist[len(hist)-2] // ReadDir sorts by name; notes.txt is last
	if readString(t, filepath.Join(dir, historyDir, newest)) != prev {
		t.Error("newest history copy is not the previous file")
	}
}

func TestSaveHistoryFailureAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := fileWith()
	hash := writeFile(t, path, orig)
	writeFile(t, filepath.Join(dir, historyDir), "not a directory")
	if _, err := NewWriter(path).Save(hash, set("system.keep_versions", "1")); err == nil {
		t.Fatal("Save succeeded without a history copy")
	}
	if readString(t, path) != orig {
		t.Fatal("file replaced although the history copy failed")
	}
}

func TestSaveMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := NewWriter(path).Save("", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Save of a missing file = %v", err)
	}
}

// fakeRegistry is a schema_version 2 that renamed system.old_keep to
// system.keep_versions.
var fakeRegistry = registry{current: 2, steps: []Migration{{
	From: 1, To: 2, NeedsConfirm: true,
	Apply: func(doc map[string]any) error {
		sys, _ := doc["system"].(map[string]any)
		if v, ok := sys["old_keep"]; ok {
			sys["keep_versions"] = v
			delete(sys, "old_keep")
		}
		return nil
	},
}}}

func TestMigration(t *testing.T) {
	v1 := `{"schema_version": 1, "host": {"instance_id": "` + testID + `", "data_dir": "/x"}, "system": {"old_keep": 5}}`
	l, err := fakeRegistry.parse([]byte(v1))
	if err != nil {
		t.Fatal(err)
	}
	if !l.Migrated || !l.NeedsConfirm || l.FileVersion != 1 || l.Doc.SchemaVersion() != 2 || l.Hash != Hash([]byte(v1)) {
		t.Fatalf("Loaded = %+v", l)
	}
	c, err := l.Doc.Effective(nil)
	if err != nil || c.System.KeepVersions != 5 {
		t.Fatalf("keep_versions = %d, %v", c.System.KeepVersions, err)
	}
	out := l.Doc.Encode()
	var probe map[string]any
	if err := json.Unmarshal(out, &probe); err != nil || probe["schema_version"] != float64(2) {
		t.Fatalf("migrated output %s", out)
	}
	again, err := fakeRegistry.parse(out)
	if err != nil || again.Migrated || again.NeedsConfirm || !bytes.Equal(again.Doc.Encode(), out) {
		t.Fatalf("migration is not idempotent: %v %+v", err, again)
	}
	// Without the step, the old key is unknown.
	if _, err := Parse([]byte(v1)); errPath(err) != "system.old_keep" {
		t.Fatalf("v1 key in the production registry: %v", err)
	}

	broken := registry{current: 3, steps: fakeRegistry.steps}
	if _, err := broken.parse([]byte(v1)); err == nil || !strings.Contains(err.Error(), "no migration from schema_version 2") {
		t.Fatalf("missing step: err = %v", err)
	}
	failing := registry{current: 2, steps: []Migration{{From: 1, To: 2, Apply: func(map[string]any) error { return errors.New("bad value") }}}}
	if _, err := failing.parse([]byte(v1)); err == nil || !strings.Contains(err.Error(), "migrate schema_version 1 to 2") {
		t.Fatalf("failing step: err = %v", err)
	}

	// Save persists the migration and keeps the v1 bytes in history.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	hash := writeFile(t, path, v1)
	w := NewWriter(path)
	w.reg = fakeRegistry
	if _, err := w.Save(hash, nil); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != string(out) {
		t.Fatalf("saved %s, want %s", got, out)
	}
	hist := historyFiles(t, dir)
	if len(hist) != 1 || !strings.HasPrefix(hist[0], "config.v1.") {
		t.Fatalf("history = %v", hist)
	}
}

func TestTooNew(t *testing.T) {
	// A newer file may use keys, nulls or duplicates this binary rejects;
	// the version check comes first.
	newer := `{"schema_version": 3, "host": {"x": null, "x": 1}, "future": {}}`
	for _, r := range []registry{migrations, fakeRegistry} {
		_, err := r.parse([]byte(newer))
		var tn *TooNewError
		if !errors.Is(err, ErrTooNew) || !errors.As(err, &tn) || tn.Version != 3 || tn.Supported != r.current {
			t.Fatalf("registry v%d: err = %v", r.current, err)
		}
	}

	path := filepath.Join(t.TempDir(), "config.json")
	hash := writeFile(t, path, newer)
	if _, err := Load(path); !errors.Is(err, ErrTooNew) {
		t.Fatalf("Load = %v", err)
	}
	w := NewWriter(path)
	w.reg = fakeRegistry
	if _, err := w.Save(hash, nil); !errors.Is(err, ErrTooNew) {
		t.Fatalf("Save = %v", err)
	}
	if readString(t, path) != newer || historyFiles(t, filepath.Dir(path)) != nil {
		t.Fatal("too-new file was changed")
	}
}
