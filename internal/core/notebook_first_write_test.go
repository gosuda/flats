package core

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

func TestNotebookHostFirstWriteUsesHostScratch(t *testing.T) {
	s, dir := newTestService(t)
	blocked := filepath.Join(dir, "not-a-temp-directory")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", blocked)
	out, result := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
	if !out.Created || out.Pending || result.Applied != 1 {
		t.Fatalf("first write did not complete with host scratch: %+v / %+v", out, result)
	}
	owners, err := filepath.Glob(filepath.Join(dir, "cache", "notebook-spills", "owner-*"))
	if err != nil || len(owners) != 1 {
		t.Fatalf("expected one host scratch owner: %v, %v", owners, err)
	}
}

func TestNotebookHostFirstWriteScratchSurvivesHostInterruption(t *testing.T) {
	ctx := context.Background()
	if dir := os.Getenv("NOTEBOOK_FIRST_WRITE_CRASH_DIR"); dir != "" {
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		boardExecDB(t, filepath.Join(dir, "flats.db"), `CREATE TRIGGER fail_init BEFORE INSERT ON versions WHEN NEW.flat='notes' BEGIN SELECT RAISE(ABORT, 'injected shell failure'); END`)
		out, result := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
		if !out.Created || !out.Pending || result.Applied != 1 {
			t.Fatalf("first write did not reach pending initialization: %+v / %+v", out, result)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "cache", "notebook-spills")
	live := notebook.NewSpillManager(root, 0, nil)
	t.Cleanup(func() {
		if err := live.Close(); err != nil {
			t.Error(err)
		}
	})
	owners, err := filepath.Glob(filepath.Join(root, "owner-*"))
	if err != nil || len(owners) != 1 {
		t.Fatalf("expected one live scratch owner: %v, %v", owners, err)
	}
	source, _ := newTestService(t)
	in := newNotebook(t, source, "live")
	capture, err := live.Capture(ctx, in.ReadTx, notebook.Scope{Tree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, total, err := capture.Part(0)
	if err != nil || total == 0 || len(before) == 0 {
		t.Fatalf("live owner's capture is unavailable: %q, %d, %v", before, total, err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNotebookHostFirstWriteScratchSurvivesHostInterruption$")
	cmd.Env = append(os.Environ(), "NOTEBOOK_FIRST_WRITE_CRASH_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("interrupted host: %v\n%s", err, out)
	}
	all, err := filepath.Glob(filepath.Join(root, "owner-*"))
	if err != nil || len(all) != 2 {
		t.Fatalf("first-write scratch was not host-owned: %v, %v", all, err)
	}
	abandoned := all[0]
	if abandoned == owners[0] {
		abandoned = all[1]
	}

	boardExecDB(t, filepath.Join(dir, "flats.db"), `DROP TRIGGER fail_init`)
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	out, result := writeNotebook(t, s, "notes", "first-write", notebookCreatePage)
	if out.Created || out.Pending || out.Revision != 2 || !result.Replay || result.Applied != 1 {
		t.Fatalf("restart lost the first write: %+v / %+v", out, result)
	}
	if got := notebookCount(t, s.liveNotebook("notes"), `SELECT count(*) FROM items WHERE kind='page' AND alive=1`); got != 2 {
		t.Fatalf("restart duplicated pages: %d", got)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("restart duplicated revisions: %v", got)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted host's scratch remains: %v", err)
	}
	if got, count, err := capture.Part(0); err != nil || count != total || !bytes.Equal(got, before) {
		t.Fatalf("restart changed another live owner's capture: %q, %d, %v", got, count, err)
	}
}
