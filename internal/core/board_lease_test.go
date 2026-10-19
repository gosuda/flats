package core

import (
	"context"
	"errors"
	"os"
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
