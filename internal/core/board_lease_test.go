package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board"
)

// TestBoardHeldRevisionContentAndLease keeps a revision's files leased
// until the caller releases them, and a blob leased through the backend
// survives a sweep that would otherwise remove it (board-TECH 5.5).
func TestBoardHeldRevisionContentAndLease(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	img := pngBytes(t, 3, 2)
	if _, err := s.UpdateBoard(ctx, "plan", BoardUpdate{OpID: "lease-image-1", Assets: []BoardAsset{{Ref: "$pic", Content: img}},
		Ops: boardOps(t, `[{"op":"add","el":{"type":"image","asset":"$pic","alt":"a picture"}}]`)}, ViaMCP); err != nil {
		t.Fatal(err)
	}
	rev := revisionNumbers(t, s, "plan")[0]
	files, err := s.st.RevisionFiles(ctx, "plan", rev)
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	be := boardBackend{s: s, slug: "plan"}
	c, release, err := be.HeldRevisionContent(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Assets[hashBytes(img)]; !ok {
		t.Fatalf("revision %d content has no image: %+v", rev, c.Assets)
	}
	for _, f := range files {
		if !s.leased("plan", f.Hash) {
			t.Fatalf("%s is not leased while held", f.Path)
		}
	}
	release()
	release()
	for _, f := range files {
		if s.leased("plan", f.Hash) {
			t.Fatalf("%s is still leased after release", f.Path)
		}
	}
	if _, _, err := be.HeldRevisionContent(ctx, 999); err == nil {
		t.Fatal("a missing revision read")
	}

	root := s.storeDir("plan", "board")
	stray, err := putBlob(root, []byte("chosen by an export, then unreferenced"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(blobPath(root, stray), old, old); err != nil {
		t.Fatal(err)
	}
	unlease := be.Lease([]string{stray})
	s.sweepBlobs(ctx, "plan", "board")
	if _, err := os.Stat(blobPath(root, stray)); err != nil {
		t.Fatalf("the sweep removed a leased blob: %v", err)
	}
	unlease()
	s.sweepBlobs(ctx, "plan", "board")
	if _, err := os.Stat(blobPath(root, stray)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unleased, unreferenced old blob was kept: %v", err)
	}
}

// TestBoardForegroundImportEndsRetryBackoff ends the ticker's backoff after
// an earlier failed import as soon as any import succeeds, not only the
// ticker's own.
func TestBoardForegroundImportEndsRetryBackoff(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	failed := func() {
		s.boardRetry.Store("plan", boardBackoff{next: time.Now().Add(time.Hour), delay: boardRetryMax})
	}
	cleared := func(path string) {
		t.Helper()
		if _, ok := s.boardRetry.Load("plan"); ok {
			t.Fatalf("a successful %s import left the ticker's backoff in place", path)
		}
	}
	failed()
	writeBoard(t, s, "plan", "after-failure-1", `[{"op":"add","el":{"type":"sticky","text":"one"}}]`)
	cleared("write")
	failed()
	if _, err := s.GetBoard(ctx, "plan", 1, board.Query{}); err != nil {
		t.Fatal(err)
	}
	cleared("revision read")
	failed()
	if _, err := s.RollbackRevision(ctx, "plan", 1, "Agent", ViaMCP); err != nil {
		t.Fatal(err)
	}
	cleared("rollback")
}

// TestBoardPutBlobReuseAgainstSweep: an upload that reuses an old,
// unreferenced blob and the sweep's removal are ordered by the lease, so an
// upload that reports the blob as already stored never loses it
// (board-TECH 5.5).
func TestBoardPutBlobReuseAgainstSweep(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	be := boardBackend{s: s, slug: "plan"}
	data := []byte("an image reused after its rows were cleaned up")
	h := hashBytes(data)
	root := s.docsStoreDir("plan")
	p := blobPath(root, h)
	old := time.Now().Add(-time.Hour)
	age := func() {
		t.Helper()
		if _, err := os.Stat(p); err != nil {
			if _, err := putBlob(root, data); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A lease taken first keeps the blob.
	age()
	unlease := be.Lease([]string{h})
	s.removeUnleasedBlob("plan", h, p, time.Now().Add(-blobGrace))
	unlease()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the sweep removed a leased blob: %v", err)
	}
	for round := 0; round < 500; round++ {
		age()
		var start, wg sync.WaitGroup
		var stored bool
		var putErr error
		start.Add(1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			start.Wait()
			s.removeUnleasedBlob("plan", h, p, time.Now().Add(-blobGrace))
		}()
		go func() {
			defer wg.Done()
			start.Wait()
			stored, putErr = be.PutBlob(ctx, h, data)
		}()
		start.Done()
		wg.Wait()
		if putErr != nil {
			t.Fatal(putErr)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("round %d: stored=%v, but the blob is gone: %v", round, stored, err)
		}
		if info.ModTime().Before(time.Now().Add(-blobGrace)) {
			t.Fatalf("round %d: the reused blob keeps its old time", round)
		}
	}
}

func TestBoardQueryLimitMessage(t *testing.T) {
	if _, err := BoardQuery("", nil, nil, "", "", 0); err != nil {
		t.Fatalf("limit 0 (the default) refused: %v", err)
	}
	_, err := BoardQuery("", nil, nil, "", "", 2001)
	if err == nil || !strings.Contains(err.Error(), "or 0 for the default") {
		t.Fatalf("limit 2001: %v", err)
	}
}
