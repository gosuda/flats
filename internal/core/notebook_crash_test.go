package core

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

func TestNotebookCrashImages(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newNotebook(t, s, "notes")
	unlock := s.lock("notes")
	defer unlock()
	first := directNotebookWrite(t, in, "image-create", notebookCreatePage)
	titleOp := notebookTitleOp(t, first.Refs["page"].Ref, "Renamed page")
	directNotebookWrite(t, in, "image-title", titleOp)
	entries := notebookOutbox(t, in)
	if len(entries) != 2 {
		t.Fatalf("outbox entries=%d", len(entries))
	}
	images := map[string]string{"written": crashImage(t, dir)}
	var stored int64
	for _, e := range entries {
		for _, hash := range e.NewHashes {
			data, err := in.SnapshotData(ctx, hash)
			if err != nil {
				t.Fatal(err)
			}
			added, err := (notebookBackend{s: s, slug: "notes"}).PutBlob(ctx, hash, data)
			if err != nil {
				t.Fatal(err)
			}
			if added {
				stored += int64(len(data))
			}
		}
	}
	images["materialized"] = crashImage(t, dir)
	imp := notebookImport{bySeq: map[int64]int{}}
	if err := s.commitNotebookEntries(ctx, "notes", in.StoreID(), entries, &imp); err != nil {
		t.Fatal(err)
	}
	images["metadata committed"] = crashImage(t, dir)
	if err := in.Acknowledge(ctx, entries[len(entries)-1].Seq, stored); err != nil {
		t.Fatal(err)
	}
	images["acknowledged"] = crashImage(t, dir)
	if err := s.st.ContentTx(ctx, "notes", func(tx *store.ContentTx) error { return tx.AddHold(ctx, "notebook-rollback:"+in.StoreID()+":3", 2) }); err != nil {
		t.Fatal(err)
	}
	images["rollback held"] = crashImage(t, dir)
	for _, name := range []string{"written", "materialized", "metadata committed", "acknowledged", "rollback held"} {
		t.Run(name, func(t *testing.T) {
			r := restartFrom(t, images[name])
			rin := r.liveNotebook("notes")
			if rin == nil {
				t.Fatal("notebook not restored")
			}
			stray, err := putBlob(r.docsStoreDir("notes"), []byte("unreferenced"))
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * blobGrace)
			if err := filepath.WalkDir(filepath.Join(r.docsStoreDir("notes"), "blobs"), func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				return os.Chtimes(path, old, old)
			}); err != nil {
				t.Fatal(err)
			}
			r.sweepBlobs(ctx, "notes", "notebook")
			if _, err := os.Stat(storedBlobPath(r.docsStoreDir("notes"), stray)); !os.IsNotExist(err) {
				t.Fatalf("sweep did not remove stray: %v", err)
			}
			if got := storedRevisions(t, r, "notes"); !slices.Equal(got, []int{3, 2, 1}) {
				t.Fatalf("history after restart=%v", got)
			}
			if count := notebookCount(t, rin, `SELECT count(*) FROM pages JOIN items USING(id) WHERE alive=1`); count != 2 {
				t.Fatalf("live pages=%d", count)
			}
			for n := 1; n <= 3; n++ {
				src, release, err := (notebookBackend{s: r, slug: "notes"}).RevisionSource(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				titles := []string{}
				for _, f := range src.Files {
					data, err := src.Read(ctx, f.Hash)
					if err != nil {
						release()
						t.Fatal(err)
					}
					if strings.HasSuffix(f.Path, "/page.json") {
						var page struct{ Title string }
						if err := json.Unmarshal(data, &page); err != nil {
							release()
							t.Fatal(err)
						}
						titles = append(titles, page.Title)
					}
				}
				release()
				slices.Sort(titles)
				want := []string{"Notes"}
				if n == 2 {
					want = []string{"Added page", "Notes"}
				}
				if n == 3 {
					want = []string{"Notes", "Renamed page"}
				}
				if !slices.Equal(titles, want) {
					t.Fatalf("revision %d titles=%v, want %v", n, titles, want)
				}
			}
			if entries := notebookOutbox(t, rin); len(entries) != 0 {
				t.Fatalf("unacknowledged entries=%d", len(entries))
			}
			if holds, err := r.st.Holds(ctx, "notes"); err != nil || len(holds) != 0 {
				t.Fatalf("holds after restart=%v, %v", holds, err)
			}
			out, result := writeNotebook(t, r, "notes", "image-create", notebookCreatePage)
			if !result.Replay || out.Revision != 2 || out.Pending {
				t.Fatalf("durable replay=%+v / %+v", out, result)
			}
			if got := storedRevisions(t, r, "notes"); len(got) != 3 {
				t.Fatalf("replay duplicated a revision: %v", got)
			}
		})
	}
}
