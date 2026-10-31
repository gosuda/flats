package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistoryRecoveryReplay(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	src := historySource(t, f.in, 8)
	first, err := f.in.Recover(t.Context(), src, f.page.ID, Placement{}, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	beforeUndo, beforeOutbox := writerCount(t, f.in, "undo"), writerCount(t, f.in, "outbox")
	retry, err := f.in.Recover(t.Context(), src, f.page.ID, Placement{}, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Refs["root"] != first.Refs["root"] || retry.State != first.State {
		t.Fatalf("recovery changed on retry: first=%+v retry=%+v", first, retry)
	}
	if writerCount(t, f.in, "undo") != beforeUndo || writerCount(t, f.in, "outbox") != beforeOutbox {
		t.Fatal("retry contributed undo or revision")
	}
	var seq Sequence
	if err := f.in.ReadTx(t.Context(), func(_ *sql.Tx, s Sequence) error { seq = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if seq != first.State.Seq {
		t.Fatalf("retry advanced sequence: %v", seq)
	}
}

type historyPruningBackend struct {
	historyHTTPBackend
	pruned bool
	calls  int
}

func (b *historyPruningBackend) RevisionSource(ctx context.Context, n int) (RevisionSource, func(), error) {
	b.calls++
	if b.pruned {
		return RevisionSource{}, nil, &WriteError{Code: "not_found", Reason: "revision"}
	}
	return b.historyHTTPBackend.RevisionSource(ctx, n)
}

func TestHistoryCursorSurvivesPruning(t *testing.T) {
	t.Parallel()
	b := &historyPruningBackend{}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	historyEdit(t, in, CreatePageOp{CreateFields: CreateFields{Ref: "second"}, Title: "Second"})
	b.source = historySource(t, in, 2)
	get := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://example.test/_notebook/api/revisions/2?"+query, nil)
		r.Header.Set("X-Flats-Access", "private")
		w := httptest.NewRecorder()
		in.serveHistory(w, r)
		return w
	}
	w := get("limit=1")
	if w.Code != 200 {
		t.Fatalf("first %d %s", w.Code, w.Body)
	}
	var first struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Next == "" {
		t.Fatal("missing cursor")
	}
	b.pruned = true
	w = get("cursor=" + first.Next)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"complete":true`) {
		t.Fatalf("continuation after pruning %d %s", w.Code, w.Body)
	}
	if b.calls != 1 {
		t.Fatalf("cursor reopened revision %d times", b.calls)
	}
	req, _ := json.Marshal(map[string]string{"cursor": first.Next})
	if _, err := in.Read(t.Context(), req, false); err != nil {
		t.Fatalf("bare cursor: %v", err)
	}
	if _, err := in.Read(t.Context(), req, true); err == nil {
		t.Fatal("Public accepted historical cursor")
	}
}

func TestHistoryRecoveryResponse(t *testing.T) {
	t.Parallel()
	b := &historyPruningBackend{}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	session, err := in.NewSession(t.Context(), "person", "Person", true)
	if err != nil {
		t.Fatal(err)
	}
	b.source = historySource(t, in, 2)
	var page ID
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM pages LIMIT 1`).Scan(&page)
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"revision": 2, "page": page, "session": session.Token})
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://example.test/_notebook/api/recover", strings.NewReader(string(body)))
		r.Header.Set("X-Flats-Access", "private")
		r.Header.Set("X-Flats-Notebook", "1")
		r.Header.Set("Origin", "http://example.test")
		w := httptest.NewRecorder()
		in.serveHistory(w, r)
		return w
	}
	first := call()
	if first.Code != 200 {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 3 || response["root"] == nil || response["revision"] == nil || response["revision_pending"] == nil {
		t.Fatalf("response shape %s", first.Body)
	}
	b.pruned = true
	retry := call()
	if retry.Code != 200 || retry.Body.String() != first.Body.String() {
		t.Fatalf("response changed: %s vs %s", first.Body, retry.Body)
	}
}

func TestHistoryRecoverySurvivesRestart(t *testing.T) {
	t.Parallel()
	b := &writerBackend{}
	dir := t.TempDir()
	in, _, err := openNotebookFixture(t, dir, b, Options{Slug: "history", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := in.NewSession(t.Context(), "person", "Person", true)
	if err != nil {
		in.Stop()
		t.Fatal(err)
	}
	src := historySource(t, in, 5)
	var page ID
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM pages LIMIT 1`).Scan(&page)
	}); err != nil {
		in.Stop()
		t.Fatal(err)
	}
	first, err := in.Recover(t.Context(), src, page, Placement{}, session.Token)
	in.Stop()
	if err != nil {
		t.Fatal(err)
	}
	in, _, err = openNotebookFixture(t, dir, b, Options{Slug: "history"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	src.Read = func(context.Context, string) ([]byte, error) { t.Error("replay reopened content"); return nil, nil }
	replay, err := in.Recover(t.Context(), src, page, Placement{}, session.Token)
	if err != nil || replay.State != first.State || replay.Refs["root"] != first.Refs["root"] {
		t.Fatalf("restart replay %+v %v", replay, err)
	}
}

type historyLeaseBackend struct {
	writerBackend
	mu     sync.Mutex
	leases map[string]int
}

func (b *historyLeaseBackend) Lease(hashes []string) func() {
	b.mu.Lock()
	for _, hash := range hashes {
		b.leases[hash]++
	}
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, hash := range hashes {
			b.leases[hash]--
		}
	}
}
func (b *historyLeaseBackend) count(hash string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.leases[hash]
}

func TestHistoryCaptureAssetLeaseLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cleanup func(*testing.T, *Instance, *atomic.Int64)
	}{
		{"expiry", func(t *testing.T, in *Instance, now *atomic.Int64) {
			now.Add(61)
			runtime, _ := in.historyRuntime()
			runtime.manager.mu.Lock()
			runtime.manager.expireLocked()
			runtime.manager.mu.Unlock()
		}},
		{"eviction", func(t *testing.T, in *Instance, now *atomic.Int64) {
			for i := 0; i < 4; i++ {
				now.Add(1)
				src := historySource(t, in, i+3)
				if _, err := in.ReadRevision(t.Context(), src, []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"stop", func(_ *testing.T, in *Instance, _ *atomic.Int64) { in.Stop() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := new(atomic.Int64)
			now.Store(time.Now().Unix())
			b := &historyLeaseBackend{leases: map[string]int{}}
			in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "First", Now: func() time.Time { return time.Unix(now.Load(), 0) }})
			if err != nil {
				t.Fatal(err)
			}
			defer in.Stop()
			var imageBytes bytes.Buffer
			if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
				t.Fatal(err)
			}
			hash := contentHash(imageBytes.Bytes())
			src := historySource(t, in, 2)
			src.Files = append(src.Files, File{Path: "assets/" + hash + ".png", Hash: hash, Size: int64(imageBytes.Len())})
			if _, err := in.ReadRevision(t.Context(), src, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
			if b.count(hash) != 1 {
				t.Fatalf("capture released asset lease: %d", b.count(hash))
			}
			tc.cleanup(t, in, now)
			if b.count(hash) != 0 {
				t.Fatalf("cleanup retained asset lease: %d", b.count(hash))
			}
		})
	}
}

func TestHistoryRecoveryRequestIdentity(t *testing.T) {
	t.Parallel()
	b := &historyHTTPBackend{}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	session, err := in.NewSession(t.Context(), "person", "Person", true)
	if err != nil {
		t.Fatal(err)
	}
	b.source = historySource(t, in, 2)
	var page ID
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		return tx.QueryRowContext(t.Context(), `SELECT id FROM pages LIMIT 1`).Scan(&page)
	}); err != nil {
		t.Fatal(err)
	}
	call := func(identity string, revision int) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"revision": revision, "page": page, "session": session.Token, "request_id": identity})
		r := httptest.NewRequest("POST", "http://example.test/_notebook/api/recover", bytes.NewReader(body))
		r.Header.Set("X-Flats-Access", "private")
		r.Header.Set("X-Flats-Notebook", "1")
		r.Header.Set("Origin", "http://example.test")
		w := httptest.NewRecorder()
		in.serveHistory(w, r)
		return w
	}
	first := call("first", 2)
	if first.Code != 200 {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	retry := call("first", 2)
	if retry.Code != 200 || retry.Body.String() != first.Body.String() {
		t.Fatalf("retry %d %s", retry.Code, retry.Body)
	}
	second := call("second", 2)
	if second.Code != 200 || second.Body.String() == first.Body.String() {
		t.Fatalf("new identity %d %s", second.Code, second.Body)
	}
	changed := call("first", 3)
	if changed.Code != 409 {
		t.Fatalf("identity conflict %d %s", changed.Code, changed.Body)
	}
	var failure WriteError
	if err := json.Unmarshal(changed.Body.Bytes(), &failure); err != nil || failure.Code != "conflict" || failure.Reason != "op_id_conflict" || failure.Retry {
		t.Fatalf("conflict envelope %s: %v", changed.Body, err)
	}
}

func TestHistoryHistoricalDatabaseHTTP(t *testing.T) {
	t.Parallel()
	b := &historyHTTPBackend{}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	var page Ref
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		var id ID
		if err := tx.QueryRowContext(t.Context(), `SELECT id FROM pages LIMIT 1`).Scan(&id); err != nil {
			return err
		}
		r, err := loadRecord(t.Context(), tx, id)
		page = r.Item.Ref
		return err
	}); err != nil {
		t.Fatal(err)
	}
	refs := historyEdit(t, in, CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "owner", Place: Placement{Page: Target{Ref: &page}}, Title: "Tasks"}, CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: Target{Tmp: "db"}, Cells: []CellInput{{Property: Target{Tmp: "db.title"}, Value: CellInputValue{Text: dbText("Historical value")}}}}).Refs
	b.source = historySource(t, in, 2)
	row := refs["row"]
	historyEdit(t, in, DeleteRowsOp{Rows: []Target{{Ref: &row}}})
	query := "database=" + string(refs["db"].ID) + "&limit=1"
	var schema, rows int
	var assembled strings.Builder
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("GET", "http://example.test/_notebook/api/revisions/2?"+query, nil)
		r.Header.Set("X-Flats-Access", "private")
		w := httptest.NewRecorder()
		in.serveHistory(w, r)
		if w.Code != 200 {
			t.Fatalf("database %d %s", w.Code, w.Body)
		}
		var result struct {
			Schema   []json.RawMessage `json:"schema"`
			Rows     []json.RawMessage `json:"rows"`
			Next     string            `json:"next"`
			Complete bool              `json:"complete"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		schema += len(result.Schema)
		rows += len(result.Rows)
		assembled.Write(w.Body.Bytes())
		if result.Complete {
			break
		}
		if result.Next == "" || i == 9 {
			t.Fatal("database traversal did not complete")
		}
		query = "cursor=" + result.Next
	}
	if schema < 3 || rows != 1 || !strings.Contains(assembled.String(), "Historical value") {
		t.Fatalf("historical projection schema=%d rows=%d %s", schema, rows, assembled.String())
	}
}

type historyPendingBackend struct{ historyHTTPBackend }

func (*historyPendingBackend) Rollback(context.Context, int, string) (int, error) { return 0, nil }

func TestHistoryRollbackPendingResponse(t *testing.T) {
	t.Parallel()
	b := &historyPendingBackend{}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	r := httptest.NewRequest("POST", "http://example.test/_notebook/api/rollback", strings.NewReader(`{"revision":2}`))
	r.Header.Set("X-Flats-Access", "private")
	r.Header.Set("X-Flats-Notebook", "1")
	r.Header.Set("Origin", "http://example.test")
	w := httptest.NewRecorder()
	in.serveHistory(w, r)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"revision":0,"revision_pending":true}` {
		t.Fatalf("pending rollback %d %s", w.Code, w.Body)
	}
}
