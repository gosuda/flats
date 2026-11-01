package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

func directNotebookWrite(t *testing.T, in *notebook.Instance, opID, ops string) notebook.WriteResult {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(notebookPayload(opID, ops), &fields); err != nil {
		t.Fatal(err)
	}
	fields["slug"] = json.RawMessage(`"notes"`)
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	u, err := notebook.DecodeUpdate(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := in.Update(context.Background(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func notebookOutbox(t *testing.T, in *notebook.Instance) []notebook.OutboxEntry {
	t.Helper()
	entries, err := in.Outbox(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestNotebookImportAcknowledgeCrashReplay(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	unlock := sync.OnceFunc(s.lock("notes"))
	defer unlock()
	directNotebookWrite(t, in, "crash-one", notebookCreatePage)
	directNotebookWrite(t, in, "crash-two", notebookCreatePage)
	entries := notebookOutbox(t, in)
	if len(entries) != 2 {
		t.Fatalf("pending entries = %d", len(entries))
	}
	boardExecDB(t, s.notebookDB("notes"), `CREATE TRIGGER fail_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT, 'injected acknowledgement failure'); END`)
	if _, err := s.importNotebookLocked(ctx, "notes", in); err == nil || !strings.Contains(err.Error(), "injected acknowledgement failure") {
		t.Fatalf("import with failed acknowledgement = %v", err)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("metadata before acknowledgement = %v", got)
	}
	if got := len(notebookOutbox(t, in)); got != 2 {
		t.Fatalf("failed acknowledgement removed %d entries, want 2 remaining", got)
	}
	state, err := s.st.ContentState(ctx, "notes")
	if err != nil || state.ImportSeq != entries[1].Seq || state.ImportStore != in.StoreID() {
		t.Fatalf("committed import cursor = %+v, %v", state, err)
	}
	for i, e := range entries {
		if n, ok, err := s.st.NotebookImportRevision(ctx, "notes", in.StoreID(), e.Seq); err != nil || !ok || n != i+2 {
			t.Fatalf("committed mapping %d = %d, %t, %v", e.Seq, n, ok, err)
		}
	}
	// Reopen only the notebook: metadata and unacknowledged outbox survive
	// independently, just as after a stop between their two commits.
	identity := in.StoreID()
	in.Stop()
	boardExecDB(t, s.notebookDB("notes"), `DROP TRIGGER fail_ack`)
	reopened, fresh, err := notebook.Open(s.notebookDir("notes"), notebookBackend{s: s, slug: "notes"}, notebook.Options{Slug: "notes", Title: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Stop()
	if fresh || reopened.StoreID() != identity {
		t.Fatalf("reopen replaced identity: fresh=%t id=%s", fresh, reopened.StoreID())
	}
	imp, err := s.importNotebookLocked(ctx, "notes", reopened)
	if err != nil || len(imp.revisions) != 0 {
		t.Fatalf("crash replay = %+v, %v", imp, err)
	}
	if got := len(notebookOutbox(t, reopened)); got != 0 {
		t.Fatalf("crash replay left %d outbox entries", got)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("crash replay duplicated revisions = %v", got)
	}
	if got := notebookCount(t, reopened, `SELECT count(*) FROM snapshots WHERE refs>0`); got != 0 {
		t.Fatalf("acknowledgement left snapshot references = %d", got)
	}
	if got := notebookCount(t, reopened, `SELECT count(*) FROM commands WHERE import_status='pending'`); got != 0 {
		t.Fatalf("acknowledgement left pending commands = %d", got)
	}
}

func TestNotebookFailedMarkCleanup(t *testing.T) {
	ctx := context.Background()
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopt=%t", adopt), func(t *testing.T) {
			s, dir := newTestService(t)
			if adopt {
				if _, err := s.CreateFlat(ctx, "notes", "Kept", ViaMCP); err != nil {
					t.Fatal(err)
				}
			}
			boardExecDB(t, filepath.Join(dir, "flats.db"), `CREATE TRIGGER fail_mark BEFORE INSERT ON content_state WHEN NEW.flat='notes' BEGIN SELECT RAISE(ABORT, 'injected mark failure'); END`)
			if _, err := s.UpdateNotebook(ctx, "notes", notebookPayload("failed-mark", notebookCreatePage), ViaMCP); err == nil || !strings.Contains(err.Error(), "injected mark failure") {
				t.Fatalf("failed mark = %v", err)
			}
			if s.liveNotebook("notes") != nil {
				t.Fatal("failed mark left running notebook")
			}
			for _, path := range []string{s.notebookDir("notes"), s.docsStoreDir("notes")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed mark left %s: %v", path, err)
				}
			}
			if adopt {
				f, err := s.GetFlat(ctx, "notes")
				if err != nil || f.Name != "Kept" || f.Type != "flat" {
					t.Fatalf("failed mark changed untyped flat = %+v, %v", f, err)
				}
			} else if _, err := s.st.GetFlat(ctx, "notes"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed mark retained fresh metadata = %v", err)
			}
			boardExecDB(t, filepath.Join(dir, "flats.db"), `DROP TRIGGER fail_mark`)
			out, _ := writeNotebook(t, s, "notes", "failed-mark", notebookCreatePage)
			if !out.Created || out.Pending || out.Revision != 2 {
				t.Fatalf("retry after failed mark = %+v", out)
			}
		})
	}
}

func TestNotebookImportFailureAtomicityAndBackoff(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newNotebook(t, s, "notes")
	unlock := sync.OnceFunc(s.lock("notes"))
	defer unlock()
	directNotebookWrite(t, in, "unimported-one", notebookCreatePage)
	directNotebookWrite(t, in, "unimported-two", notebookCreatePage)
	before, err := s.st.ContentState(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	headBefore, err := s.st.Head(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	entries := notebookOutbox(t, in)
	boardExecDB(t, filepath.Join(dir, "flats.db"), `CREATE TRIGGER fail_import BEFORE INSERT ON notebook_imports WHEN NEW.flat='notes' BEGIN SELECT RAISE(ABORT, 'injected import failure'); END`)
	for range 2 {
		if _, err := s.importNotebookLocked(ctx, "notes", in); err == nil || !strings.Contains(err.Error(), "injected import failure") {
			t.Errorf("failed import = %v", err)
		}
		if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{1}) {
			t.Errorf("failed import committed revisions = %v", got)
		}
		state, err := s.st.ContentState(ctx, "notes")
		if err != nil || state.ImportStore != before.ImportStore || state.ImportSeq != before.ImportSeq {
			t.Errorf("failed import advanced cursor = %+v, %v", state, err)
		}
		head, err := s.st.Head(ctx, "notes")
		if err != nil || !slices.Equal(head, headBefore) {
			t.Errorf("failed import changed head = %+v, %v", head, err)
		}
		for _, e := range entries {
			if _, ok, err := s.st.NotebookImportRevision(ctx, "notes", in.StoreID(), e.Seq); err != nil || ok {
				t.Errorf("failed import retained receipt seq=%d: %t, %v", e.Seq, ok, err)
			}
		}
		if got := len(notebookOutbox(t, in)); got != 2 {
			t.Errorf("failed import acknowledged outbox = %d", got)
		}
	}
	unlock()
	s.notebookPending.Store("notes", true)
	s.TickNotebooks(ctx)
	v, ok := s.notebookRetry.Load("notes")
	if !ok {
		t.Fatal("failed tick did not set backoff")
	}
	first := v.(notebookBackoff)
	if first.delay != 5*time.Second || !first.next.After(time.Now()) {
		t.Fatalf("first backoff = %+v", first)
	}
	s.TickNotebooks(ctx)
	if v, ok := s.notebookRetry.Load("notes"); !ok || v.(notebookBackoff) != first {
		t.Fatalf("tick retried before backoff expired = %+v, %t", v, ok)
	}
	if _, ok := s.notebookPending.Load("notes"); !ok {
		t.Fatal("failed tick lost pending notification")
	}
	boardExecDB(t, filepath.Join(dir, "flats.db"), `DROP TRIGGER fail_import`)
	s.notebookRetry.Store("notes", notebookBackoff{next: time.Now().Add(-time.Second), delay: first.delay})
	s.TickNotebooks(ctx)
	if _, ok := s.notebookRetry.Load("notes"); ok {
		t.Fatal("successful tick retained backoff")
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{3, 2, 1}) || len(notebookOutbox(t, in)) != 0 {
		t.Fatalf("fixed import = %v", got)
	}
	s.notebookRetry.Store("notes", notebookBackoff{next: time.Now().Add(time.Hour), delay: 5 * time.Minute})
	writeNotebook(t, s, "notes", "foreground-success", notebookCreatePage)
	if _, ok := s.notebookRetry.Load("notes"); ok {
		t.Fatal("successful foreground import retained backoff")
	}
}

func TestNotebookCommittedWritePendingImport(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestService(t)
	in := newNotebook(t, s, "notes")
	boardExecDB(t, filepath.Join(dir, "flats.db"), `CREATE TRIGGER fail_import BEFORE INSERT ON notebook_imports WHEN NEW.flat='notes' BEGIN SELECT RAISE(ABORT, 'injected import failure'); END`)
	out, result := writeNotebook(t, s, "notes", "pending-write", notebookCreatePage)
	if !out.Pending || out.Revision != 0 || result.Applied != 1 || len(result.Warnings) != 1 || result.Warnings[0].Code != "revision_pending" || result.Samples.Warnings.Total != 1 {
		t.Fatalf("committed pending write = %+v / %+v", out, result)
	}
	if got := notebookCount(t, in, `SELECT count(*) FROM items WHERE kind='page' AND alive=1`); got != 2 {
		t.Fatalf("pending write did not commit = %d pages", got)
	}
	boardExecDB(t, filepath.Join(dir, "flats.db"), `DROP TRIGGER fail_import`)
	if err := s.catchUpNotebook(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	out, replay := writeNotebook(t, s, "notes", "pending-write", notebookCreatePage)
	if out.Pending || out.Revision != 2 || !replay.Replay || replay.Refs["page"] != result.Refs["page"] || len(replay.Warnings) != 0 {
		t.Fatalf("pending write replay after catchup = %+v / %+v", out, replay)
	}
}

func TestNotebookCorruptSnapshotSkipsImportAndSweep(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	unlock := sync.OnceFunc(s.lock("notes"))
	defer unlock()
	directNotebookWrite(t, in, "corrupt-snapshot", notebookCreatePage)
	entries := notebookOutbox(t, in)
	if len(entries) != 1 || len(entries[0].NewHashes) == 0 {
		unlock()
		t.Fatalf("outbox has no new snapshots = %+v", entries)
	}
	hash := entries[0].NewHashes[0]
	good, err := in.SnapshotData(ctx, hash)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	boardExecDB(t, s.notebookDB("notes"), fmt.Sprintf(`UPDATE snapshots SET data=X'00' WHERE hash='%s'`, hash))
	if _, err := s.importNotebookLocked(ctx, "notes", in); err == nil {
		t.Error("corrupt snapshot imported")
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{1}) {
		t.Errorf("corrupt snapshot committed history = %v", got)
	}
	root := s.docsStoreDir("notes")
	stray, err := putBlob(root, []byte("must survive failed sweep preparation"))
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(storedBlobPath(root, stray), old, old); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	s.sweepBlobs(ctx, "notes", "notebook")
	if _, err := os.Stat(storedBlobPath(root, stray)); err != nil {
		t.Fatalf("sweep proceeded after failed import = %v", err)
	}
	boardExecDB(t, s.notebookDB("notes"), fmt.Sprintf(`UPDATE snapshots SET data=X'%x' WHERE hash='%s'`, good, hash))
	s.sweepBlobs(ctx, "notes", "notebook")
	if _, err := os.Stat(storedBlobPath(root, stray)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixed import did not enable sweep = %v", err)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("fixed snapshot history = %v", got)
	}
}

func TestNotebookHoldSettlementRequiresPositiveAbsence(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"command", "rollback boundary"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newTestService(t)
			in := newNotebook(t, s, "notes")
			unlock := sync.OnceFunc(s.lock("notes"))
			defer unlock()
			directNotebookWrite(t, in, "committed-command", notebookCreatePage)
			committed, absent, table := "committed-command", "absent-command", "commands"
			if kind == "rollback boundary" {
				e := notebookOutbox(t, in)[0]
				// This fixture exercises host settlement, not the unavailable
				// whole-notebook rollback implementation.
				boardExecDB(t, s.notebookDB("notes"), fmt.Sprintf(`UPDATE outbox SET kind='rollback', rollback_target=1 WHERE seq=%d`, e.Seq))
				committed = fmt.Sprintf("notebook-rollback:%s:%d", in.StoreID(), e.Seq-1)
				absent = fmt.Sprintf("notebook-rollback:%s:%d", in.StoreID(), e.Seq)
				table = "outbox"
			}
			for _, op := range []string{committed, absent} {
				if err := s.st.ContentTx(ctx, "notes", func(tx *store.ContentTx) error { return tx.AddHold(ctx, op, 1) }); err != nil {
					t.Fatal(err)
				}
			}
			boardExecDB(t, s.notebookDB("notes"), "ALTER TABLE "+table+" RENAME TO settlement_unavailable")
			s.settleNotebookHoldsLocked(ctx, "notes", in)
			holds, err := s.st.Holds(ctx, "notes")
			if err != nil || len(holds) != 2 {
				t.Errorf("query error released holds = %+v, %v", holds, err)
			}
			boardExecDB(t, s.notebookDB("notes"), "ALTER TABLE settlement_unavailable RENAME TO "+table)
			s.settleNotebookHoldsLocked(ctx, "notes", in)
			holds, err = s.st.Holds(ctx, "notes")
			if err != nil || len(holds) != 1 || holds[0].Op != committed {
				t.Fatalf("positive absence settlement = %+v, %v", holds, err)
			}
			if kind == "rollback boundary" {
				if _, err := s.importNotebookLocked(ctx, "notes", in); err != nil {
					t.Fatal(err)
				}
				if holds, err := s.st.Holds(ctx, "notes"); err != nil || len(holds) != 0 {
					t.Fatalf("import did not release committed rollback hold = %+v, %v", holds, err)
				}
			}
		})
	}
}

func TestNotebookPrunedReceiptNotPending(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	in := newNotebook(t, s, "notes")
	one := 1
	if err := s.SetRevisionQuotas(ctx, "notes", &one, nil); err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(s.lock("notes"))
	defer unlock()
	directNotebookWrite(t, in, "pruned-op", notebookCreatePage)
	directNotebookWrite(t, in, "retained-op", notebookCreatePage)
	if _, err := s.importNotebookLocked(ctx, "notes", in); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	out, result := writeNotebook(t, s, "notes", "pruned-op", notebookCreatePage)
	if out.Pending || out.Revision != 0 || !result.Replay || len(result.Warnings) != 1 || result.Warnings[0].Code != "revision_pruned" {
		t.Fatalf("pruned command replay = %+v / %+v", out, result)
	}
	if got := storedRevisions(t, s, "notes"); !slices.Equal(got, []int{3}) {
		t.Fatalf("pruned replay created a revision = %v", got)
	}
}
