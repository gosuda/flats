package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func writerOutboxStore(t *testing.T) *store {
	t.Helper()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	p, b, db := vocabID(PageKind, 0), vocabID(BlockKind, 1), vocabID(DatabaseKind, 0)
	pr, selectPr, option := vocabID(PropertyKind, 0), vocabID(PropertyKind, 1), vocabID(OptionKind, 0)
	view := vocabID(ViewKind, 0)
	items := map[ID]ItemData{
		p:                     Page{Title: "Page <한글>", Position: "a0"},
		vocabID(PageKind, 1):  Page{Title: "Other", Position: "a1"},
		vocabID(BlockKind, 0): Block{Page: p, Position: "a0", Content: Content{Kind: Paragraph, Text: RichText{{Text: "hello", Marks: []Mark{}}}}},
		b:                     Block{Page: p, Position: "a1", Content: Content{Kind: InlineDatabase, Database: db}},
		db:                    Database{Block: b, Title: "Tasks", PropertyOrder: []ID{pr, selectPr}, ViewOrder: []ID{view}},
		pr:                    Property{Database: db, Definition: PropertyDef{Name: "Title", Type: TitleType}, OptionOrder: []ID{}},
		selectPr:              Property{Database: db, Definition: PropertyDef{Name: "Status", Type: SelectType}, OptionOrder: []ID{option}},
		option:                Option{Property: selectPr, Name: "Ready", Color: "green"},
		vocabID(RowKind, 0):   Row{Database: db, Position: "a0", Cells: map[ID]CellValue{pr: {Type: TitleType, Value: "Task"}, selectPr: {Type: SelectType, Value: option}}},
		view:                  View{Database: db, Definition: StoredView{Name: "Table", Display: StoredDisplay{Layout: "table", Visible: []ID{pr, selectPr}, Widths: []StoredWidth{}}, Sort: []StoredSort{}, Filter: []StoredFilter{}}},
	}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		ids := make([]ID, 0, len(items))
		for id := range items {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if err := putItem(t.Context(), tx, Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: items[id]}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func writerOutboxRecord(t *testing.T, s *store, seq Sequence, force bool) int64 {
	t.Helper()
	var recorded int64
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := syncContentFiles(t.Context(), tx, seq); err != nil {
			return err
		}
		var err error
		recorded, err = recordOutbox(t.Context(), tx, "agent", "message", "op", []string{"Alice"}, time.Unix(1234, 0), force)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return recorded
}

func writerOutboxEntries(t *testing.T, s *store) []OutboxEntry {
	t.Helper()
	in := &Instance{db: s, readers: s.db}
	entries, err := in.Outbox(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func writerOutboxChange(t *testing.T, s *store, id ID, change func(*itemRecord)) {
	t.Helper()
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		record, err := loadRecord(t.Context(), tx, id)
		if err != nil {
			return err
		}
		change(&record)
		return putRecord(t.Context(), tx, record)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWriterOutboxCanonicalFiles(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	if seq := writerOutboxRecord(t, s, 1, false); seq != 1 {
		t.Fatalf("initial outbox sequence = %d", seq)
	}
	entries := writerOutboxEntries(t, s)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.CreatedAt != 1234 || e.OpID != "op" || e.Kind != "agent" || e.Message != "message" || !slices.Equal(e.Authors, []string{"Alice"}) {
		t.Fatalf("outbox metadata = %+v", e)
	}
	in := &Instance{db: s, readers: s.db}
	var schemaSeen, cellsSeen bool
	for i, f := range e.Files {
		if i > 0 && e.Files[i-1].Path >= f.Path {
			t.Fatal("files are not path ordered")
		}
		data, err := in.SnapshotData(t.Context(), f.Hash)
		if err != nil || contentHash(data) != f.Hash || int64(len(data)) != f.Size {
			t.Fatalf("snapshot %s = %s, %v", f.Path, data, err)
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		canonical, err := CanonicalJSON(value)
		if err != nil || !bytes.Equal(data, canonical) {
			t.Fatalf("noncanonical file %s: %s, %v", f.Path, data, err)
		}
		var noRuntimeFields func(any)
		noRuntimeFields = func(value any) {
			switch value := value.(type) {
			case map[string]any:
				for key, child := range value {
					if slices.Contains([]string{"ref", "inc", "ver", "v", "deleted_seq", "notice_seq", "editor"}, key) {
						t.Fatalf("runtime field %s in %s", key, f.Path)
					}
					noRuntimeFields(child)
				}
			case []any:
				for _, child := range value {
					noRuntimeFields(child)
				}
			}
		}
		noRuntimeFields(value)
		if strings.HasSuffix(f.Path, "/schema.json") {
			schema := value.(map[string]any)
			if len(schema["properties"].([]any)) != 2 || len(schema["options"].([]any)) != 1 || len(schema["views"].([]any)) != 1 {
				t.Fatalf("incomplete schema: %s", data)
			}
			schemaSeen = true
		}
		if strings.Contains(f.Path, "/rows/") {
			row := value.([]any)[0].(map[string]any)
			cells := row["cells"].(map[string]any)
			if cells[string(vocabID(PropertyKind, 0))].(map[string]any)["value"] != "Task" {
				t.Fatalf("missing row cells: %s", data)
			}
			cellsSeen = true
		}
	}
	if !schemaSeen || !cellsSeen || len(e.Files) != len(e.NewHashes) {
		t.Fatalf("incomplete content: schema=%v cells=%v files=%d hashes=%d", schemaSeen, cellsSeen, len(e.Files), len(e.NewHashes))
	}
}

func TestWriterOutboxEqualForceAndHashReuse(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	writerOutboxRecord(t, s, 1, false)
	initial := writerOutboxEntries(t, s)[0]
	if got := writerOutboxRecord(t, s, 2, false); got != 0 {
		t.Fatalf("equal state recorded %d", got)
	}
	writerOutboxChange(t, s, vocabID(PageKind, 0), func(r *itemRecord) {
		r.Item.Ref.Inc, r.Item.Ver, r.Aggregate = 9, 9, 9
	})
	if got := writerOutboxRecord(t, s, 9, false); got != 0 {
		t.Fatalf("runtime-only change recorded %d", got)
	}
	writerOutboxRecord(t, s, 10, true)
	entries := writerOutboxEntries(t, s)
	if len(entries) != 2 || len(entries[1].NewHashes) != 0 || !slices.Equal(initial.Files, entries[1].Files) {
		t.Fatalf("forced equal boundary = %+v", entries)
	}
	writerOutboxChange(t, s, vocabID(BlockKind, 0), func(r *itemRecord) {
		b := r.Item.Data.(Block)
		b.Content.Text[0].Text = "updated"
		r.Item.Data = b
	})
	writerOutboxRecord(t, s, 11, false)
	entries = writerOutboxEntries(t, s)
	last := entries[len(entries)-1]
	if len(last.NewHashes) != 1 {
		t.Fatalf("text edit added %d hashes", len(last.NewHashes))
	}
	for i, f := range initial.Files {
		if f != last.Files[i] && !strings.Contains(f.Path, "/blocks/") {
			t.Fatalf("text edit changed unrelated file %s", f.Path)
		}
	}
}

func TestWriterOutboxMovesDeletionsAndAssets(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	live, dead := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for i, hash := range []string{live, dead} {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until) VALUES(?,'image/png',1,1,99,?,1,0)`, hash, 1-i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writerOutboxChange(t, s, vocabID(BlockKind, 0), func(r *itemRecord) {
		b := r.Item.Data.(Block)
		b.Content = Content{Kind: Image, Asset: live, Caption: RichText{}}
		r.Item.Data = b
	})
	writerOutboxRecord(t, s, 1, false)
	first := writerOutboxEntries(t, s)[0]
	var assets int
	for _, f := range first.Files {
		if strings.HasPrefix(f.Path, "assets/") {
			assets++
			if f.Path != "assets/"+live+".png" || slices.Contains(first.NewHashes, live) {
				t.Fatalf("invalid asset revision file %+v", f)
			}
		}
	}
	if assets != 1 {
		t.Fatalf("live assets = %d", assets)
	}
	writerOutboxChange(t, s, vocabID(BlockKind, 1), func(r *itemRecord) {
		b := r.Item.Data.(Block)
		b.Page = vocabID(PageKind, 1)
		r.Item.Data = b
	})
	writerOutboxChange(t, s, vocabID(RowKind, 0), func(r *itemRecord) {
		r.Alive, r.DeletedAt = false, 2
	})
	writerOutboxRecord(t, s, 2, false)
	entries := writerOutboxEntries(t, s)
	last := entries[len(entries)-1]
	for _, f := range last.Files {
		if strings.Contains(f.Path, "/rows/") {
			t.Fatalf("empty row bucket survived: %s", f.Path)
		}
		if strings.Contains(f.Path, "/databases/") && !strings.HasPrefix(f.Path, "pages/"+string(vocabID(PageKind, 1))+"/") {
			t.Fatalf("database file did not follow owning block: %s", f.Path)
		}
	}
}

func TestWriterOutboxAcknowledgeAndReplay(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	firstSeq := writerOutboxRecord(t, s, 1, false)
	first := writerOutboxEntries(t, s)[0]
	writerOutboxChange(t, s, vocabID(PageKind, 0), func(r *itemRecord) {
		p := r.Item.Data.(Page)
		p.Title = "Changed"
		r.Item.Data = p
	})
	writerOutboxRecord(t, s, 2, false)
	writerOutboxChange(t, s, vocabID(PageKind, 0), func(r *itemRecord) {
		p := r.Item.Data.(Page)
		p.Title = "Page <한글>"
		r.Item.Data = p
	})
	thirdSeq := writerOutboxRecord(t, s, 3, false)
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO commands(op_id,request_hash,outcome,mapping_cursor,state,outbox_seq,import_status) VALUES('command','hash','{}','cursor','{}',?,'pending')`, fmt.Sprint(firstSeq))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(5000, 0)
	ack := func(through int64) {
		t.Helper()
		if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return acknowledgeOutbox(t.Context(), tx, through, now) }); err != nil {
			t.Fatal(err)
		}
	}
	ack(firstSeq)
	in := &Instance{db: s, readers: s.db}
	var originalPage File
	for _, f := range first.Files {
		if f.Path == "pages/"+string(vocabID(PageKind, 0))+"/page.json" {
			originalPage = f
		}
	}
	if _, err := in.SnapshotData(t.Context(), originalPage.Hash); err != nil {
		t.Fatalf("snapshot still referenced by third boundary disappeared: %v", err)
	}
	var status string
	var until int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT import_status,retain_until FROM commands WHERE op_id='command'`).Scan(&status, &until); err != nil {
		t.Fatal(err)
	}
	if status != "imported" || until != now.Add(24*time.Hour).Unix() {
		t.Fatalf("command import retention = %s %d", status, until)
	}
	now = now.Add(time.Hour)
	ack(firstSeq)
	var again int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT retain_until FROM commands WHERE op_id='command'`).Scan(&again); err != nil || again != until {
		t.Fatalf("re-ack changed retention: %d, %v", again, err)
	}
	ack(thirdSeq)
	if got := writerOutboxEntries(t, s); len(got) != 0 {
		t.Fatalf("pending after final ack: %+v", got)
	}
	if _, err := in.SnapshotData(t.Context(), originalPage.Hash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("released snapshot lookup = %v", err)
	}
}

func TestWriterOutboxRollbackAndCursor(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	ctx := t.Context()
	errStop := errors.New("rollback")
	if err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := syncContentFiles(ctx, tx, 1); err != nil {
			return err
		}
		if _, err := recordOutbox(ctx, tx, "agent", "", "", nil, time.Unix(1, 0), false); err != nil {
			return err
		}
		return errStop
	}); !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	if entries := writerOutboxEntries(t, s); len(entries) != 0 {
		t.Fatal("rolled-back outbox escaped transaction")
	}
	writerOutboxRecord(t, s, 1, false)
	writerOutboxRecord(t, s, 2, true)
	in := &Instance{db: s, readers: s.db}
	entries, err := in.Outbox(ctx, 1, 1)
	if err != nil || len(entries) != 1 || entries[0].Seq != 2 {
		t.Fatalf("outbox cursor: %+v %v", entries, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := in.Outbox(cancelled, 0, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled outbox read = %v", err)
	}
	before := writerOutboxEntries(t, s)
	if err := s.transaction(ctx, func(tx *sql.Tx) error { return syncContentFiles(ctx, tx, 2) }); err != nil {
		t.Fatal(err)
	}
	after := writerOutboxEntries(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("synchronizing changed recorded boundaries")
	}
}
