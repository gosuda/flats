package core

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// crashImage copies the data directory as a killed process would leave it:
// every regular file but the SQLite shared-memory indexes, which a restart
// rebuilds from the logs. Files that change during the copy (a background
// commit or checkpoint) make it start again, so the copy is one instant.
func crashImage(t *testing.T, src string) string {
	t.Helper()
	type stamp struct {
		size int64
		mod  time.Time
	}
	scan := func() map[string]stamp {
		out := map[string]stamp{}
		_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || strings.HasSuffix(p, "-shm") {
				return nil
			}
			if info, err := d.Info(); err == nil {
				out[p] = stamp{info.Size(), info.ModTime()}
			}
			return nil
		})
		return out
	}
	for attempt := 0; attempt < 50; attempt++ {
		dst := filepath.Join(t.TempDir(), "data")
		before := scan()
		ok := true
		for p := range before {
			rel, _ := filepath.Rel(src, p)
			data, err := os.ReadFile(p)
			if err != nil {
				ok = false
				break
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, rel), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if ok && mapsEqualStamp(before, scan()) {
			return dst
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the data directory kept changing during the crash copy")
	return ""
}

func mapsEqualStamp[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// restartFrom opens a service on a crash image.
func restartFrom(t *testing.T, dir string) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	return s
}

// boardTexts maps the text of each element of revision rev (0: now).
func boardTexts(t *testing.T, s *Service, slugName string, rev int) []string {
	t.Helper()
	var out []string
	for _, el := range boardElements(t, s, slugName, rev) {
		out = append(out, el["text"].(string))
	}
	slices.Sort(out)
	return out
}

// checkBoardHistory checks the revision list and the content of each
// revision, that no revision after the first is empty, and that the board
// has nothing left to import.
func checkBoardHistory(t *testing.T, s *Service, slugName string, want map[int][]string, now []string) {
	t.Helper()
	var nums []int
	for n := range want {
		nums = append(nums, n)
	}
	slices.Sort(nums)
	slices.Reverse(nums)
	if got := revisionNumbers(t, s, slugName); !slices.Equal(got, nums) {
		t.Fatalf("revisions = %v, want %v", got, nums)
	}
	for n, texts := range want {
		if got := boardTexts(t, s, slugName, n); !slices.Equal(got, texts) {
			t.Fatalf("revision %d = %v, want %v", n, got, texts)
		}
		r, _, err := s.GetRevision(context.Background(), slugName, n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 1 && (r.Elements == nil || r.Elements.Added+r.Elements.Changed+r.Elements.Removed == 0) {
			t.Fatalf("revision %d (%s) is empty: %+v", n, r.Kind, r.Elements)
		}
	}
	if got := boardTexts(t, s, slugName, 0); !slices.Equal(got, now) {
		t.Fatalf("board now = %v, want %v", got, now)
	}
	if n := outboxLen(t, s.liveBoard(slugName)); n != 0 {
		t.Fatalf("outbox after the restart = %d entries", n)
	}
	if holds, _ := s.st.Holds(context.Background(), slugName); len(holds) != 0 {
		t.Fatalf("holds after the restart = %v", holds)
	}
}

// ageBlobs makes every blob of the flat older than the sweep's grace.
func ageBlobs(t *testing.T, s *Service, slugName string) {
	t.Helper()
	old := time.Now().Add(-2 * blobGrace)
	_ = filepath.WalkDir(filepath.Join(s.storeDir(slugName, "board"), "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
}

// TestBoardKillDuringImportAndRollback kills the process at each boundary of
// an outbox import (after storing the snapshots, after the flats.db commit
// and before the acknowledgement) and of a rollback (after the hold, after
// the board committed it), restarts from the image, and checks the revision
// count and content, board.db, no empty revision, and every write reported
// as saved (board-TECH 14 Crashes). A blob sweep then runs on aged blobs as
// the first thing after the restart, before any other import.
func TestBoardKillDuringImportAndRollback(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	newBoard(t, s, "plan")
	unlock := s.lock("plan")
	in := s.liveBoard("plan")
	directBoardWrite(t, in, "kill-one-1", `[{"op":"add","el":{"type":"sticky","text":"one"}}]`)
	directBoardWrite(t, in, "kill-two-1", `[{"op":"add","el":{"type":"sticky","text":"two"}}]`)
	st, err := s.st.ContentState(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := in.Outbox(ctx, st.ImportSeq, boardImportBatch)
	if err != nil || len(entries) != 2 {
		t.Fatalf("outbox = %d entries, %v", len(entries), err)
	}
	images := map[string]string{"written": crashImage(t, dir)}
	root := s.docsStoreDir("plan")
	var stored int64
	for _, e := range entries {
		for _, h := range e.Snapshots {
			data, err := in.SnapshotData(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := putBlob(root, data); err != nil {
				t.Fatal(err)
			}
			stored += int64(len(data))
		}
	}
	images["blobs stored"] = crashImage(t, dir)
	res := boardImport{bySeq: map[int64]int{}}
	if err := s.commitBoardEntries(ctx, "plan", in.StoreID(), entries, 0, &res); err != nil {
		t.Fatal(err)
	}
	images["flats.db committed"] = crashImage(t, dir)
	if err := in.Acknowledge(ctx, entries[len(entries)-1].Seq, stored); err != nil {
		t.Fatal(err)
	}
	images["acknowledged"] = crashImage(t, dir)

	// A rollback to revision 2: the hold is recorded, then the board
	// commits it, then core would import it.
	// Reading the revision takes the flat lock itself.
	unlock()
	_, c, err := s.boardRevisionContent(ctx, "plan", 2)
	if err != nil {
		t.Fatal(err)
	}
	unlock = s.lock("plan")
	if err := s.st.ContentTx(ctx, "plan", func(tx *store.ContentTx) error { return tx.AddHold(ctx, "kill-rollback", 2) }); err != nil {
		t.Fatal(err)
	}
	images["rollback held"] = crashImage(t, dir)
	if _, err := in.Rollback(ctx, "kill-rollback", 2, c, "Agent"); err != nil {
		t.Fatal(err)
	}
	images["rollback committed"] = crashImage(t, dir)
	unlock()

	written := map[int][]string{1: nil, 2: {"one"}, 3: {"one", "two"}}
	cases := []struct {
		image string
		want  map[int][]string
		now   []string
	}{
		{"written", written, []string{"one", "two"}},
		{"blobs stored", written, []string{"one", "two"}},
		{"flats.db committed", written, []string{"one", "two"}},
		{"acknowledged", written, []string{"one", "two"}},
		{"rollback held", written, []string{"one", "two"}},
		{"rollback committed", map[int][]string{1: nil, 2: {"one"}, 3: {"one", "two"}, 4: {"one"}}, []string{"one"}},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			// The sweep first: it imports before reading references, so
			// snapshots stored before the crash and the entries' files are
			// kept although every blob is past the grace.
			r := restartFrom(t, images[tc.image])
			if r.liveBoard("plan") == nil {
				t.Fatal("the board is not running after the restart")
			}
			stray, err := putBlob(r.docsStoreDir("plan"), []byte("stray "+tc.image))
			if err != nil {
				t.Fatal(err)
			}
			ageBlobs(t, r, "plan")
			r.sweepBlobs(ctx, "plan", "board")
			// The sweep ran (it skips a board it could not import).
			if _, err := os.Stat(storedBlobPath(r.docsStoreDir("plan"), stray)); !os.IsNotExist(err) {
				t.Fatalf("the sweep did not run: stray blob %v", err)
			}
			checkBoardHistory(t, r, "plan", tc.want, tc.now)
			// Each entry is mapped to exactly one revision.
			rin := r.liveBoard("plan")
			for i, e := range entries {
				if n, ok, err := r.st.BoardImportRevision(ctx, "plan", rin.StoreID(), e.Seq); err != nil || !ok || n != 2+i {
					t.Fatalf("import mapping of seq %d = %d %v %v", e.Seq, n, ok, err)
				}
			}
			// Importing again changes nothing.
			runlock := r.lock("plan")
			_, err = r.importBoardLocked(ctx, "plan", rin)
			runlock()
			if err != nil {
				t.Fatal(err)
			}
			checkBoardHistory(t, r, "plan", tc.want, tc.now)
			if tc.image == "rollback committed" {
				rv, _, err := r.GetRevision(ctx, "plan", 4)
				if err != nil || rv.Kind != "rollback" || rv.RollbackOf != 2 {
					t.Fatalf("rollback revision = %+v %v", rv, err)
				}
			}
		})
	}
}
