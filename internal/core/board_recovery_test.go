package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/store"
)

// directBoardWrite applies ops to the running board without importing the
// entry it records, as a write whose import has not happened yet. The
// caller holds the flat lock, so the ticker does not import it meanwhile.
func directBoardWrite(t *testing.T, in *board.Instance, opID, ops string, assets ...[]byte) board.UpdateResult {
	t.Helper()
	res, err := in.UpdateBoard(context.Background(), board.UpdateRequest{OpID: opID, Ops: boardOps(t, ops), Label: "Agent", Assets: assets}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutboxSeq == 0 {
		t.Fatalf("direct write recorded no entry: %+v", res)
	}
	return res
}

// boardExecDB runs statements on a SQLite database through its own
// connection, as a fault injection.
func boardExecDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// storedRevisions lists revision numbers, newest first, without the import
// ListRevisions runs first, for callers that hold the flat lock.
func storedRevisions(t *testing.T, s *Service, slugName string) []int {
	t.Helper()
	revs, err := s.st.ListRevisions(context.Background(), slugName, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, r := range revs {
		nums = append(nums, r.Number)
	}
	return nums
}

func outboxLen(t *testing.T, in *board.Instance) int {
	t.Helper()
	entries, err := in.Outbox(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestBoardImportAcknowledgeCrashWindow imports entries whose flats.db
// commit happened but whose acknowledgement in board.db did not (a stop
// between the two): the next import acknowledges them again and records no
// revision twice.
func TestBoardImportAcknowledgeCrashWindow(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	unlock := s.lock("plan")
	defer unlock()
	in := s.liveBoard("plan")
	directBoardWrite(t, in, "crash-window-1", `[{"op":"add","el":{"type":"sticky","text":"one"}}]`)
	directBoardWrite(t, in, "crash-window-2", `[{"op":"add","el":{"type":"sticky","text":"two"}}]`)
	st, err := s.st.ContentState(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := in.Outbox(ctx, st.ImportSeq, boardImportBatch)
	if err != nil || len(entries) != 2 {
		t.Fatalf("outbox = %d entries, %v", len(entries), err)
	}
	// Import steps 1 and 2 (store the snapshots, commit to flats.db), then
	// stop before step 4 (acknowledge).
	root := s.docsStoreDir("plan")
	for _, e := range entries {
		for _, h := range e.Snapshots {
			data, err := in.SnapshotData(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := putBlob(root, data); err != nil {
				t.Fatal(err)
			}
		}
	}
	res := boardImport{bySeq: map[int64]int{}}
	if err := s.commitBoardEntries(ctx, "plan", in.StoreID(), entries, &res); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.revisions, []int{2, 3}) {
		t.Fatalf("committed revisions = %v", res.revisions)
	}
	if n := outboxLen(t, in); n != 2 {
		t.Fatalf("outbox after the commit = %d entries, want 2 unacknowledged", n)
	}
	imp, err := s.importBoardLocked(ctx, "plan", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.revisions) != 0 {
		t.Fatalf("re-import recorded revisions again: %v", imp.revisions)
	}
	if n := outboxLen(t, in); n != 0 {
		t.Fatalf("outbox after the re-import = %d entries, want 0", n)
	}
	if got := storedRevisions(t, s, "plan"); !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("revisions = %v", got)
	}
	for i, e := range entries {
		if n, ok, err := s.st.BoardImportRevision(ctx, "plan", in.StoreID(), e.Seq); err != nil || !ok || n != 2+i {
			t.Fatalf("import mapping of seq %d = %d %v %v", e.Seq, n, ok, err)
		}
	}
}

// TestBoardInterruptedCreation finishes a board whose creation stopped after
// it was marked a board (phase init) and before its shell ran: at startup,
// and on the next write when the board is not running.
func TestBoardInterruptedCreation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	open := func() *Service {
		s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	interrupt := func(s *Service, slugName string) {
		t.Helper()
		if _, err := s.CreateFlat(ctx, slugName, "Plan", ViaMCP); err != nil {
			t.Fatal(err)
		}
		if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.InitState(ctx, "board") }); err != nil {
			t.Fatal(err)
		}
	}
	ready := func(s *Service, slugName string, want []int) {
		t.Helper()
		if st, err := s.st.ContentState(ctx, slugName); err != nil || st.Phase != "ready" {
			t.Fatalf("%s state = %+v %v", slugName, st, err)
		}
		if s.liveBoard(slugName) == nil {
			t.Fatalf("%s is not running", slugName)
		}
		if f, err := s.GetFlat(ctx, slugName); err != nil || f.Type != "board" {
			t.Fatalf("%s = %+v %v", slugName, f, err)
		}
		if got := revisionNumbers(t, s, slugName); !slices.Equal(got, want) {
			t.Fatalf("%s revisions = %v, want %v", slugName, got, want)
		}
		r, _, err := s.GetRevision(ctx, slugName, 1)
		if err != nil || r.Kind != "created" {
			t.Fatalf("%s revision 1 = %+v %v", slugName, r, err)
		}
	}

	s := open()
	interrupt(s, "plan")
	s.Close()
	s = open()
	ready(s, "plan", []int{1})

	interrupt(s, "pad")
	if s.liveBoard("pad") != nil {
		t.Fatal("an interrupted board runs before it is finished")
	}
	res, err := s.UpdateBoard(ctx, "pad", BoardUpdate{OpID: "after-interrupt-1", Ops: boardOps(t, `[{"op":"add","el":{"type":"sticky","text":"hi"}}]`)}, ViaMCP)
	if err != nil || res.Revision == nil || *res.Revision != 2 || res.Created {
		t.Fatalf("write to an interrupted board = %+v %v", res, err)
	}
	ready(s, "pad", []int{2, 1})
	s.Close()
}

// TestBoardFailedMarkFreesSlug: when a board's first write committed in its
// board.db but the flat cannot be marked a board, everything the call made
// is removed, so the slug (or the adopted flat) can be used again.
func TestBoardFailedMarkFreesSlug(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	db := filepath.Join(dir, "flats.db")
	failMark := func(slugName string) {
		boardExecDB(t, db, fmt.Sprintf(`CREATE TRIGGER fail_mark BEFORE INSERT ON content_state WHEN NEW.flat = '%s' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, slugName))
	}
	img := pngBytes(t, 4, 4)
	write := func(slugName, opID string) (BoardWriteResult, error) {
		return s.UpdateBoard(ctx, slugName, BoardUpdate{OpID: opID, Assets: []BoardAsset{{Ref: "$i", Content: img}},
			Ops: boardOps(t, `[{"op":"add","el":{"type":"image","asset":"$i","alt":"a"}}]`)}, ViaMCP)
	}

	failMark("fresh")
	if _, err := write("fresh", "failed-mark-1"); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("write with a failing mark: %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "fresh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("flat after a failed mark: %v", err)
	}
	if _, err := os.Stat(s.flatDir("fresh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory after a failed mark: %v", err)
	}
	boardExecDB(t, db, `DROP TRIGGER fail_mark`)
	if res, err := write("fresh", "failed-mark-2"); err != nil || !res.Created || res.Revision == nil || *res.Revision != 2 {
		t.Fatalf("write after a failed mark = %+v %v", res, err)
	}

	if _, err := s.CreateFlat(ctx, "kept", "Kept", ViaMCP); err != nil {
		t.Fatal(err)
	}
	failMark("kept")
	if _, err := write("kept", "failed-adopt-1"); err == nil {
		t.Fatal("adoption with a failing mark succeeded")
	}
	f, err := s.GetFlat(ctx, "kept")
	if err != nil || f.Name != "Kept" {
		t.Fatalf("adopted flat after a failed mark = %+v %v", f, err)
	}
	if typ, err := s.flatType(ctx, f.Flat); err != nil || typ != "" {
		t.Fatalf("type after a failed mark = %q %v", typ, err)
	}
	for _, p := range []string{s.boardDir("kept"), s.docsStoreDir("kept")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after a failed mark: %v", p, err)
		}
	}
	boardExecDB(t, db, `DROP TRIGGER fail_mark`)
	if res, err := write("kept", "failed-adopt-2"); err != nil || !res.Created {
		t.Fatalf("adoption after a failed mark = %+v %v", res, err)
	}
}

// TestBoardImportFailures: an entry whose asset blob is missing, or whose
// snapshot does not match its hash, cannot be imported. The import changes
// nothing and keeps failing until the cause is fixed, the ticker backs off,
// and a rollback that committed meanwhile reports its pending revision as a
// warning instead of an error.
func TestBoardImportFailures(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	in := s.liveBoard("plan")
	root := s.docsStoreDir("plan")
	img := pngBytes(t, 5, 3)
	hash := hashBytes(img)

	unlock := s.lock("plan")
	directBoardWrite(t, in, "missing-blob-1", `[{"op":"add","el":{"type":"image","asset":"`+hash+`","natural":[5,3],"alt":"a"}}]`, img)
	if err := os.Remove(storedBlobPath(root, hash)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err := s.importBoardLocked(ctx, "plan", in)
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no stored blob") {
			t.Fatalf("import with a missing blob: %v", err)
		}
		if n := outboxLen(t, in); n != 1 {
			t.Fatalf("outbox after a failed import = %d", n)
		}
		if got := storedRevisions(t, s, "plan"); !slices.Equal(got, []int{1}) {
			t.Fatalf("revisions after a failed import = %v", got)
		}
	}
	unlock()

	// The ticker tries a failing board again only after a delay.
	s.boardPending.Store("plan", true)
	s.TickBoards(ctx)
	v, ok := s.boardRetry.Load("plan")
	if !ok {
		t.Fatal("no backoff after a failed background import")
	}
	first := v.(boardBackoff)
	if first.delay < boardRetryMin || !first.next.After(time.Now()) {
		t.Fatalf("backoff = %+v", first)
	}
	s.TickBoards(ctx)
	if v, _ := s.boardRetry.Load("plan"); v.(boardBackoff) != first {
		t.Fatalf("the ticker retried during the backoff: %+v", v)
	}
	if _, ok := s.boardPending.Load("plan"); !ok {
		t.Fatal("a failing board left the pending set")
	}

	// A rollback commits in board.db while the import still fails: it
	// applied, so it succeeds with a warning and no revision.
	r, err := s.RollbackRevisionResult(ctx, "plan", 1, "Agent", ViaMCP)
	if err != nil || r.Revision != 0 || !strings.Contains(r.Warning, "not recorded yet") {
		t.Fatalf("rollback with a pending import = %+v %v", r, err)
	}
	if els := boardElements(t, s, "plan", 0); len(els) != 0 {
		t.Fatalf("board after the rollback = %v", els)
	}

	// Restoring the blob lets everything import; the rollback releases its
	// hold, and a successful tick ends the backoff.
	if _, err := putBlob(root, img); err != nil {
		t.Fatal(err)
	}
	if err := s.catchUpBoard(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
	if got := revisionNumbers(t, s, "plan"); !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("revisions after the fix = %v", got)
	}
	if rev, _, err := s.GetRevision(ctx, "plan", 3); err != nil || rev.Kind != "rollback" || rev.RollbackOf != 1 {
		t.Fatalf("rollback revision = %+v %v", rev, err)
	}
	if holds, _ := s.st.Holds(ctx, "plan"); len(holds) != 0 {
		t.Fatalf("holds after the import = %v", holds)
	}
	s.boardRetry.Store("plan", boardBackoff{next: time.Now().Add(-time.Second), delay: boardRetryMin})
	s.boardPending.Store("plan", true)
	s.TickBoards(ctx)
	if _, ok := s.boardRetry.Load("plan"); ok {
		t.Fatal("backoff kept after a successful import")
	}

	// A snapshot that does not match its hash.
	unlock = s.lock("plan")
	defer unlock()
	directBoardWrite(t, in, "bad-snapshot-1", `[{"op":"add","el":{"type":"sticky","text":"corrupt me"}}]`)
	entries, err := in.Outbox(ctx, 0, 10)
	if err != nil || len(entries) != 1 || len(entries[0].Snapshots) == 0 {
		t.Fatalf("outbox = %+v %v", entries, err)
	}
	snap := entries[0].Snapshots[0]
	if _, err := os.Stat(storedBlobPath(root, snap)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot already a blob: %v", err)
	}
	good, err := in.SnapshotData(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	boardExecDB(t, s.boardDB("plan"), fmt.Sprintf(`UPDATE snapshots SET data = X'00' WHERE hash = '%s'`, snap))
	for range 2 {
		if _, err := s.importBoardLocked(ctx, "plan", in); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "does not match its hash") {
			t.Fatalf("import with a corrupt snapshot: %v", err)
		}
		if _, err := os.Stat(storedBlobPath(root, snap)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a corrupt snapshot was stored: %v", err)
		}
		if got := storedRevisions(t, s, "plan"); !slices.Equal(got, []int{3, 2, 1}) {
			t.Fatalf("revisions after a corrupt snapshot = %v", got)
		}
	}
	boardExecDB(t, s.boardDB("plan"), fmt.Sprintf(`UPDATE snapshots SET data = X'%x' WHERE hash = '%s'`, good, snap))
	if imp, err := s.importBoardLocked(ctx, "plan", in); err != nil || !slices.Equal(imp.revisions, []int{4}) {
		t.Fatalf("import after the fix = %+v %v", imp, err)
	}
}

// TestBoardRevisionDeletedByRetention: a write whose revision retention
// deleted in the same import reports a warning, not the deleted number.
func TestBoardRevisionDeletedByRetention(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	one := 1
	if err := s.SetRevisionQuotas(ctx, "plan", &one, nil); err != nil {
		t.Fatal(err)
	}
	unlock := s.lock("plan")
	defer unlock()
	in := s.liveBoard("plan")
	a := directBoardWrite(t, in, "retained-a-1", `[{"op":"add","el":{"type":"sticky","text":"a"}}]`)
	b := directBoardWrite(t, in, "retained-b-1", `[{"op":"add","el":{"type":"sticky","text":"b"}}]`)
	var outA, outB BoardWriteResult
	s.boardRevisionLocked(ctx, "plan", in, a.OutboxSeq, &outA)
	if outA.Revision != nil || !strings.Contains(outA.Warning, "retention") {
		t.Fatalf("result of the deleted revision = %+v", outA)
	}
	s.boardRevisionLocked(ctx, "plan", in, b.OutboxSeq, &outB)
	if outB.Revision == nil || *outB.Revision != 3 || outB.Warning != "" {
		t.Fatalf("result of the kept revision = %+v", outB)
	}
	if got := storedRevisions(t, s, "plan"); !slices.Equal(got, []int{3}) {
		t.Fatalf("revisions = %v", got)
	}
}
