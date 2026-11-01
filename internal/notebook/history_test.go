package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func historySource(t *testing.T, in *Instance, n int) RevisionSource {
	t.Helper()
	blobs := map[string][]byte{}
	var files []File
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		sources, err := contentSources(t.Context(), tx)
		if err != nil {
			return err
		}
		for _, source := range sources {
			if source.file != nil {
				files = append(files, *source.file)
				continue
			}
			data, err := encodeContentSource(t.Context(), tx, source)
			if err != nil {
				return err
			}
			hash := contentHash(data)
			blobs[hash] = data
			files = append(files, File{Path: source.path, Hash: hash, Size: int64(len(data))})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return RevisionSource{Number: n, Files: files, Read: func(ctx context.Context, hash string) ([]byte, error) {
		data, ok := blobs[hash]
		if !ok {
			return nil, fmt.Errorf("missing blob")
		}
		return data, nil
	}}
}

func historyItem(t *testing.T, in *Instance, id ID) itemRecord {
	t.Helper()
	var item itemRecord
	if err := in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		var err error
		item, err = loadRecord(t.Context(), tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func historyEdit(t *testing.T, in *Instance, ops ...Operation) editResult {
	t.Helper()
	var result editResult
	if err := in.write(t.Context(), func(tx *sql.Tx) error {
		seq, err := nextSequence(t.Context(), tx)
		if err != nil {
			return err
		}
		result, err = applyEdit(t.Context(), tx, editOptions{Seq: seq}, ops)
		if err != nil {
			return err
		}
		if err := setSequence(t.Context(), tx, seq); err != nil {
			return err
		}
		return syncContentFiles(t.Context(), tx, seq)
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHistoryRollbackClearsUndoAndPreservesSurvivors(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	target := historySource(t, f.in, 1)
	if _, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(f.page, "Changed")); err != nil {
		t.Fatal(err)
	}
	survivor := historyItem(t, f.in, f.page.ID)
	added := historyEdit(t, f.in, CreatePageOp{CreateFields: CreateFields{Ref: "later"}, Title: "Later"}).Refs["later"]
	f.b.quota.Store(1)
	result, err := f.in.Rollback(t.Context(), target, "Person")
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Store != f.in.StoreID() || result.State.Seq <= Sequence(survivor.Item.Ver) {
		t.Fatalf("state %+v", result)
	}
	restored := historyItem(t, f.in, f.page.ID)
	if restored.Item.Ref != f.page || restored.Item.Data.(Page).Title != "First" || restored.Item.V[TitleGroup] <= survivor.Item.V[TitleGroup] {
		t.Fatalf("restored %+v", restored)
	}
	if historyItem(t, f.in, added.ID).Alive {
		t.Fatal("rollback left added page alive")
	}
	if writerCount(t, f.in, "undo") != 0 || writerCount(t, f.in, "created") != 0 {
		t.Fatal("rollback retained undo or created refs")
	}
	var undo undoSummary
	err = f.in.write(t.Context(), func(tx *sql.Tx) error {
		var err error
		undo, err = undoEdit(t.Context(), tx, editOptions{Seq: result.State.Seq + 1, Session: contentHash([]byte(f.session.Token)), Now: f.in.now()}, false)
		return err
	})
	if err != nil || !undo.Empty {
		t.Fatalf("undo after rollback %+v %v", undo, err)
	}
	entries, err := f.in.Outbox(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if last.Kind != "rollback" || last.RollbackOf != 1 || len(last.NewHashes) != 0 {
		t.Fatalf("rollback outbox %+v", last)
	}
	a, _ := CanonicalJSON(last.Files)
	b, _ := CanonicalJSON(target.Files)
	if !bytes.Equal(a, b) {
		t.Fatalf("target file list changed\n%s\n%s", a, b)
	}
}

func TestHistoryRollbackRevivalRejectsOldIncarnation(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	root := historyEdit(t, f.in, CreatePageOp{CreateFields: CreateFields{Ref: "root"}, Title: "Historical"}).Refs["root"]
	target := historySource(t, f.in, 3)
	historyEdit(t, f.in, DeletePageOp{Page: Target{Ref: &root}})
	result, err := f.in.Rollback(t.Context(), target, "Person")
	if err != nil {
		t.Fatal(err)
	}
	revived := historyItem(t, f.in, root.ID)
	if !revived.Alive || revived.Item.Ref.Inc == root.Inc || Sequence(revived.Item.Ref.Inc) != result.State.Seq {
		t.Fatalf("revival %+v", revived)
	}
	ack, err := f.in.SubmitAction(t.Context(), f.session.Token, 1, writerAction(root, "Stale"))
	if err != nil {
		t.Fatal(err)
	}
	if ack.Summary.Ignored != 1 || historyItem(t, f.in, root.ID).Item.Data.(Page).Title != "Historical" {
		t.Fatalf("stale input %+v", ack)
	}
}

func TestHistoryRecoverCopyRemapsDatabaseAndLinks(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	original := historyEdit(t, f.in,
		CreatePageOp{CreateFields: CreateFields{Ref: "sub"}, Parent: &Target{Ref: &f.page}, Title: "Child"},
		CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "owner", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Data"},
		CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: Target{Tmp: "db"}, Cells: []CellInput{{Property: Target{Tmp: "db.title"}, Value: CellInputValue{Text: dbText("row")}}}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "link"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: PageLink, Target: f.page.ID}},
	)
	target := historySource(t, f.in, 7)
	result, err := f.in.Recover(t.Context(), target, f.page.ID, Placement{}, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	root := result.Refs["root"].Ref
	if root.ID == f.page.ID || historyItem(t, f.in, root.ID).Item.Data.(Page).Title != "First (restored)" {
		t.Fatalf("copy root %+v", result)
	}
	if historyItem(t, f.in, f.page.ID).Item.Data.(Page).Title != "First" {
		t.Fatal("source changed")
	}
	var copiedLink, copiedDB ID
	err = f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
		e := &edit{ctx: t.Context(), tx: tx}
		records, err := e.records(`SELECT id FROM blocks WHERE page=?`, root.ID)
		if err != nil {
			return err
		}
		for _, r := range records {
			b := r.Item.Data.(Block)
			if b.Content.Kind == PageLink {
				copiedLink = b.Content.Target
			}
			if b.Content.Kind == InlineDatabase {
				copiedDB = b.Content.Database
			}
		}
		return nil
	})
	if err != nil || copiedLink != root.ID || copiedDB == original.Refs["db"].ID || copiedDB == "" {
		t.Fatalf("copy refs %s %s %v", copiedLink, copiedDB, err)
	}
	seq := result.State.Seq + 1
	var summary undoSummary
	err = f.in.write(t.Context(), func(tx *sql.Tx) error {
		var err error
		summary, err = undoEdit(t.Context(), tx, editOptions{Seq: seq, Session: contentHash([]byte(f.session.Token)), Now: f.in.now()}, false)
		if err != nil {
			return err
		}
		if err := setSequence(t.Context(), tx, seq); err != nil {
			return err
		}
		if err := refreshHistoryAssets(t.Context(), tx); err != nil {
			return err
		}
		return syncContentFiles(t.Context(), tx, seq)
	})
	if err != nil || summary.Applied != 1 || historyItem(t, f.in, root.ID).Alive || !historyItem(t, f.in, original.Refs["db"].ID).Alive {
		t.Fatalf("recovery undo %+v %v", summary, err)
	}
}

func TestHistoryRecoverAbsentRootWithMovedChildCollision(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	refs := historyEdit(t, f.in, CreatePageOp{CreateFields: CreateFields{Ref: "root"}, Title: "Recover"}, CreatePageOp{CreateFields: CreateFields{Ref: "child"}, Parent: &Target{Tmp: "root"}, Title: "Child"}).Refs
	target := historySource(t, f.in, 3)
	root, child := refs["root"], refs["child"]
	historyEdit(t, f.in, MovePageOp{Page: Target{Ref: &child}}, DeletePageOp{Page: Target{Ref: &root}})
	result, err := f.in.Recover(t.Context(), target, root.ID, Placement{}, f.session.Token)
	if err != nil {
		t.Fatal(err)
	}
	restored := result.Refs["root"].Ref
	if restored.ID != root.ID || restored.Inc == root.Inc {
		t.Fatalf("revival %+v", restored)
	}
	if historyItem(t, f.in, child.ID).Item.Data.(Page).Parent != nil {
		t.Fatal("collision owner moved")
	}
	var count int
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1 AND p.parent=?`, root.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restored child %d %v", count, err)
	}
}

func TestHistoryReadRevisionPagingAndIntegrity(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	historyEdit(t, f.in, CreatePageOp{CreateFields: CreateFields{Ref: "extra"}, Title: "Extra"})
	src := historySource(t, f.in, 4)
	data, err := f.in.ReadRevision(t.Context(), src, []byte(`{"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		Tree     []json.RawMessage `json:"tree"`
		Next     string            `json:"next"`
		Complete bool              `json:"complete"`
	}
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Tree) != 1 || first.Next == "" || first.Complete {
		t.Fatalf("first %s", data)
	}
	// Continuations read the immutable spill, not a current state or leased blob.
	src.Read = func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("must not reread") }
	raw, _ := json.Marshal(map[string]string{"cursor": first.Next})
	second, err := f.in.ReadRevision(t.Context(), src, raw)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.in.ReadRevision(t.Context(), src, raw)
	if err != nil || !bytes.Equal(second, replay) {
		t.Fatalf("cursor replay %v", err)
	}
	var final struct {
		Tree     []json.RawMessage `json:"tree"`
		Complete bool              `json:"complete"`
	}
	_ = json.Unmarshal(second, &final)
	if len(final.Tree) != 1 || !final.Complete {
		t.Fatalf("second %s", second)
	}
	src.Number++
	if _, err := f.in.ReadRevision(t.Context(), src, raw); err == nil {
		t.Fatal("cursor crossed revision")
	}
	src = historySource(t, f.in, 5)
	read := src.Read
	src.Read = func(ctx context.Context, h string) ([]byte, error) {
		data, err := read(ctx, h)
		return append(data, ' '), err
	}
	if _, err := f.in.ReadRevision(t.Context(), src, []byte(`{}`)); err == nil {
		t.Fatal("corrupt source accepted")
	}
}

func TestHistoryLargeDatabasePagesFromDisk(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	refs := historyEdit(t, f.in, CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Rows"}).Refs
	src := historySource(t, f.in, 8)
	baseRead := src.Read
	buckets := map[string][]map[string]json.RawMessage{}
	ids := []ID{}
	for i := 0; i < 501; i++ {
		id, err := NewID(RowKind)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		item := Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Row{Database: refs["db"].ID, Position: "a0", Cells: map[ID]CellValue{refs["db.title"].ID: {Type: TitleType, Value: strings.Repeat("x", 3000)}}}}
		fields, err := contentItem(item)
		if err != nil {
			t.Fatal(err)
		}
		buckets[contentBucket(id)] = append(buckets[contentBucket(id)], fields)
	}
	blobs := map[string][]byte{}
	maxBlob := 0
	for bucket, items := range buckets {
		slices.SortFunc(items, func(a, b map[string]json.RawMessage) int { return bytes.Compare(a["id"], b["id"]) })
		data, err := CanonicalJSON(items)
		if err != nil {
			t.Fatal(err)
		}
		hash := contentHash(data)
		blobs[hash] = data
		src.Files = append(src.Files, File{Path: "pages/" + string(f.page.ID) + "/databases/" + string(refs["db"].ID) + "/rows/" + bucket + ".json", Hash: hash, Size: int64(len(data))})
	}
	src.Read = func(ctx context.Context, h string) ([]byte, error) {
		if b, ok := blobs[h]; ok {
			maxBlob = max(maxBlob, len(b))
			return slices.Clone(b), nil
		}
		return baseRead(ctx, h)
	}
	req, _ := json.Marshal(map[string]any{"database": refs["db"].ID, "limit": 100})
	got := []ID{}
	pages := 0
	for {
		data, err := f.in.ReadRevision(t.Context(), src, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 1<<20 {
			t.Fatalf("read response %d", len(data))
		}
		var result struct {
			Rows     []Item `json:"rows"`
			Next     string `json:"next"`
			Complete bool   `json:"complete"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		for _, row := range result.Rows {
			got = append(got, row.ID())
		}
		pages++
		if result.Complete {
			break
		}
		if pages > 20 {
			t.Fatal("paging stalled")
		}
		// Once capture is complete no later page can depend on leased source I/O.
		src.Read = func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("unexpected source reread") }
		req, _ = json.Marshal(map[string]string{"cursor": result.Next})
	}
	slices.Sort(ids)
	if !slices.Equal(ids, got) || pages < 6 || maxBlob >= 128<<10 {
		t.Fatalf("paging rows=%d pages=%d max source=%d", len(got), pages, maxBlob)
	}
}

func TestHistoryLargeRecordContinuation(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	src := historySource(t, f.in, 11)
	base := src.Read
	id, err := NewID(BlockKind)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("가", (1<<20)/3+16)
	blob, err := CanonicalJSON([]map[string]any{{"id": id, "page": f.page.ID, "parent": nil, "position": "a0", "content": Content{Kind: Code, Code: text}}})
	if err != nil {
		t.Fatal(err)
	}
	hash := contentHash(blob)
	src.Files = append(src.Files, File{Path: "pages/" + string(f.page.ID) + "/blocks/" + contentBucket(id) + ".json", Hash: hash, Size: int64(len(blob))})
	src.Read = func(ctx context.Context, h string) ([]byte, error) {
		if h == hash {
			return blob, nil
		}
		return base(ctx, h)
	}
	req, _ := json.Marshal(map[string]any{"page": f.page.ID})
	var assembled strings.Builder
	parts := 0
	for {
		data, err := f.in.ReadRevision(t.Context(), src, req)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Continuation *struct {
				Record ID     `json:"record"`
				Part   int    `json:"part"`
				Parts  int    `json:"parts"`
				Data   string `json:"data"`
			} `json:"continuation"`
			Next     string `json:"next"`
			Complete bool   `json:"complete"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if c := result.Continuation; c != nil {
			if c.Record != id || c.Part != parts || c.Parts < 2 {
				t.Fatalf("continuation %+v", c)
			}
			assembled.WriteString(c.Data)
			parts++
		}
		if result.Complete {
			break
		}
		req, _ = json.Marshal(map[string]string{"cursor": result.Next})
	}
	var item struct {
		Ref     Ref `json:"ref"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(assembled.String()), &item); err != nil {
		t.Fatal(err)
	}
	if item.Ref.ID != id || item.Content.Text != text || parts < 2 {
		t.Fatalf("continuation identity=%s parts=%d", item.Ref.ID, parts)
	}
}

type historyHTTPBackend struct {
	writerBackend
	source   RevisionSource
	pinned   bool
	rollback int
	released int
}

func (b *historyHTTPBackend) Revisions(context.Context, int, int) (RevisionPage, error) {
	return RevisionPage{Revisions: []RevisionInfo{{Number: b.source.Number, Kind: "created"}}}, nil
}
func (b *historyHTTPBackend) RevisionSource(context.Context, int) (RevisionSource, func(), error) {
	return b.source, func() { b.released++ }, nil
}
func (b *historyHTTPBackend) Pin(_ context.Context, n int, pinned bool) error {
	b.pinned = pinned
	return nil
}
func (b *historyHTTPBackend) Rollback(_ context.Context, n int, _ string) (int, error) {
	b.rollback = n
	return n + 1, nil
}
func (b *historyHTTPBackend) RevisionAsset(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func TestHistoryHTTPRoutesAndOrigin(t *testing.T) {
	t.Parallel()
	b := &historyHTTPBackend{}
	in, _, err := openNotebookFixture(t, t.TempDir(), b, Options{Slug: "history", Title: "Page"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	b.source = historySource(t, in, 2)
	call := func(method, path, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.test/_notebook/api/"+path, strings.NewReader(body))
		r.Header.Set("X-Flats-Access", "private")
		r.Header.Set("X-Flats-Notebook", "1")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		in.serveHistory(w, r)
		return w
	}
	if w := call("GET", "revisions", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"number":2`) {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}
	if w := call("GET", "revisions/2", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"tree"`) {
		t.Fatalf("read %d %s", w.Code, w.Body)
	}
	if b.released != 1 {
		t.Fatal("revision lease not released")
	}
	if w := call("POST", "pin", `{"revision":2,"pinned":true}`, "http://evil.test"); w.Code != 403 || b.pinned {
		t.Fatalf("cross-origin pin %d", w.Code)
	}
	if w := call("POST", "pin", `{"revision":2,"pinned":true}`, "http://example.test"); w.Code != 200 || !b.pinned {
		t.Fatalf("pin %d %s", w.Code, w.Body)
	}
	if w := call("POST", "rollback", `{"revision":2,"page":"bad"}`, "http://example.test"); w.Code != 400 || b.rollback != 0 {
		t.Fatalf("malformed rollback %d", w.Code)
	}
	if w := call("POST", "rollback", `{"revision":2}`, "http://example.test"); w.Code != 200 || b.rollback != 2 {
		t.Fatalf("rollback %d %s", w.Code, w.Body)
	}
}

func TestHistoryMarkdownIncludesDatabaseRows(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	historyEdit(t, f.in, CreateBlockOp{CreateFields: CreateFields{Ref: "text"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Heading1, Text: RichText{{Text: "Heading", Marks: []Mark{}}}}}, CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Rows"}, CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: Target{Tmp: "db"}, Cells: []CellInput{{Property: Target{Tmp: "db.title"}, Value: CellInputValue{Text: dbText("Value")}}}})
	historyEdit(t, f.in, CreateBlockOp{CreateFields: CreateFields{Ref: "link"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: PageLink, Target: f.page.ID}})
	src := historySource(t, f.in, 3)
	req, _ := json.Marshal(map[string]any{"page": f.page.ID, "format": "markdown"})
	data, err := f.in.ReadRevision(t.Context(), src, req)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Markdown   string            `json:"markdown"`
		Complete   bool              `json:"complete"`
		References []json.RawMessage `json:"references"`
		Warnings   []Warning         `json:"warnings"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.References) == 0 || len(result.Warnings) != 1 || result.Warnings[0].Code != "page_link" || !strings.Contains(result.Markdown, "# Heading") || !strings.Contains(result.Markdown, "| Value |") {
		t.Fatalf("markdown %s", data)
	}
}

func TestHistoryRecoveredImagesHaveLiveAssetMetadata(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	hash := contentHash(data)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until) VALUES(?,'image/png',2,3,?,1,0,0)`, hash, len(data))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	historyEdit(t, f.in, CreateBlockOp{CreateFields: CreateFields{Ref: "image"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Image, Asset: hash, Alt: "Photo", Caption: RichText{}}})
	src := historySource(t, f.in, 1)
	if _, err := f.in.Recover(t.Context(), src, f.page.ID, Placement{}, f.session.Token); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT live_refs FROM assets WHERE hash=?`, hash).Scan(&live); err != nil || live != 2 {
		t.Fatalf("live asset %d %v", live, err)
	}
	runtime, err := f.in.historyRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(runtime.dir) == filepath.Dir(f.in.db.path) {
		t.Fatal("history scratch lives inside notebook")
	}
	if _, err := os.Stat(runtime.dir); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryPublicAndOriginRefusal(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	for _, test := range []struct{ name, access, origin, method string }{{"public", "public", "", "GET"}, {"unknown", "private-ish", "", "GET"}, {"missing", "", "", "GET"}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://example.test/_notebook/api/revisions", nil)
			r.Header.Set("X-Flats-Access", test.access)
			w := httptest.NewRecorder()
			f.in.serveHistory(w, r)
			if w.Code != 403 {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
}

func TestHistoryPageChangesIncludesRowBucketsAndRemovedTitle(t *testing.T) {
	t.Parallel()
	f := newWriterFixture(t)
	first := historySource(t, f.in, 1)
	historyEdit(t, f.in, SetPageTitleOp{Page: Target{Ref: &f.page}, Title: "New title"})
	second := historySource(t, f.in, 2)
	changes, err := PageChanges(OutboxEntry{Files: second.Files}, first.Files, second.Read)
	if err != nil || len(changes) != 1 || changes[0].Change != "changed" || changes[0].Title != "New title" {
		t.Fatalf("changes %+v %v", changes, err)
	}
	changes, err = PageChanges(OutboxEntry{Files: []File{}}, first.Files, first.Read)
	if err != nil || len(changes) != 1 || changes[0].Change != "removed" || changes[0].Title != "First" {
		t.Fatalf("removed %+v %v", changes, err)
	}
}

func TestHistoryDeletedPayloadBoundAndUndoRoot(t *testing.T) {
	f := newWriterFixture(t)
	ids := []ID{}
	err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		for i := 0; i < 7; i++ {
			id, err := NewID(BlockKind)
			if err != nil {
				return err
			}
			ids = append(ids, id)
			size := 3 << 20
			if i == 0 {
				size = 1024
			}
			data, err := json.Marshal(Block{Page: f.page.ID, Position: "a0", Content: Content{Kind: Code, Code: strings.Repeat("x", size)}})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO allocated_ids(id,kind) VALUES(?,'block')`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO items(id,kind,inc,alive,deleted_seq,own_version,aggregate_version,data) VALUES(?,'block','2',0,'3','2','2',?)`, id, string(data)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO blocks(id,page,parent,position) VALUES(?,?,NULL,'a0')`, id, f.page.ID); err != nil {
				return err
			}
		}
		r, err := loadRecord(t.Context(), tx, ids[0])
		if err != nil {
			return err
		}
		entry := undoEntry{Components: []undoComponent{{Parts: []undoPart{{Written: r, Desired: &r}}}}}
		return saveUndoEntry(t.Context(), tx, contentHash([]byte(f.session.Token)), "undo", 1, entry)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.in.write(t.Context(), func(tx *sql.Tx) error { return f.in.collectDeleted(t.Context(), tx) })
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT coalesce(sum(length(CAST(data AS BLOB))+128),0) FROM items WHERE alive=0`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total > maxDeletedPayload {
		t.Fatalf("payload %d", total)
	}
	if historyItem(t, f.in, ids[0]).Alive {
		t.Fatal("undo root lost tombstone")
	}
	var allocated int
	if err := f.in.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM allocated_ids`).Scan(&allocated); err != nil || allocated != 8 {
		t.Fatalf("ledger %d %v", allocated, err)
	}
}
