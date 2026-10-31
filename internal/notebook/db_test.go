package notebook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStoreRetainedContentReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), dbName)
	s, _, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	text := strings.Repeat("x", 7996) + "e\u0301😀한�"
	if got := utf8.RuneCountInString(text); got != 8001 {
		t.Fatalf("retained fixture has %d code points", got)
	}
	want := map[ID][]byte{}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, raw := range vocabFixtures(t, "items") {
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				return err
			}
			switch fields["kind"] {
			case "page", "database":
				fields["title"] = text
			case "block":
				content := fields["content"].(map[string]any)
				if content["kind"] == "paragraph" {
					content["text"] = []any{map[string]any{"text": text, "marks": []any{}}}
				}
			case "row":
				cells := fields["cells"].(map[string]any)
				cells[string(vocabID(PropertyKind, 1))] = map[string]any{"type": "text", "value": text}
			}
			encoded, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			item, err := DecodeItem(encoded)
			if err != nil {
				return err
			}
			want[item.ID()] = encoded
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for id, expected := range want {
			item, err := s.loadItem(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeItem(item)
			if err != nil || !bytes.Equal(encoded, expected) {
				t.Fatalf("retained item %s changed: %v", id, err)
			}
		}
	}
	check()
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	check()
}

func TestStoreRoundTripReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", dbName)
	s, fresh, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("new database was not fresh")
	}
	state, err := s.state(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	items := []Item{}
	for n, raw := range vocabFixtures(t, "items") {
		item, err := DecodeItem(raw)
		if err != nil {
			t.Fatalf("fixture %d: %v", n, err)
		}
		items = append(items, item)
	}
	parentPage := vocabID(PageKind, 0)
	parentBlock := vocabID(BlockKind, 200)
	items = append(items,
		Item{Ref: Ref{ID: vocabID(PageKind, 200), Inc: 7}, Ver: 7, V: map[GroupKey]Version{},
			Data: Page{Title: "Child", Parent: &parentPage, Position: "a1"}},
		Item{Ref: Ref{ID: parentBlock, Inc: 7}, Ver: 7, V: map[GroupKey]Version{},
			Data: Block{Page: parentPage, Position: "a8", Content: Content{Kind: Bullet, Text: RichText{}}}},
		Item{Ref: Ref{ID: vocabID(BlockKind, 201), Inc: 7}, Ver: 7, V: map[GroupKey]Version{},
			Data: Block{Page: parentPage, Parent: &parentBlock, Position: "a0", Content: Content{Kind: Paragraph, Text: RichText{}}}},
	)
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, item := range items {
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, fresh, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	if fresh {
		t.Fatal("reopened database was fresh")
	}
	reopened, err := s.state(t.Context())
	if err != nil || reopened != state {
		t.Fatalf("state changed on reopen: %#v != %#v: %v", reopened, state, err)
	}
	seen := map[Kind]int{}
	for _, want := range items {
		t.Run(string(want.ID()), func(t *testing.T) {
			got, err := s.loadItem(t.Context(), want.ID())
			if err != nil {
				t.Fatal(err)
			}
			a, err := EncodeItem(want)
			if err != nil {
				t.Fatal(err)
			}
			b, err := EncodeItem(got)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("reopened %s: want %s, got %s: %v", want.Kind(), a, b, err)
			}
			seen[got.Kind()]++
		})
	}
	if len(seen) != 7 {
		t.Fatalf("reopened only %d kinds", len(seen))
	}
	for pragma, want := range map[string]string{"journal_mode": "wal", "synchronous": "2", "foreign_keys": "1", "auto_vacuum": "2", "user_version": "1", "busy_timeout": "5000"} {
		var got string
		if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}
}

func TestStoreEveryCellValueReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), dbName)
	s, _, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	values := []CellValue{}
	for _, raw := range vocabFixtures(t, "cells") {
		value, err := DecodeCellValue(raw)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for i, value := range values {
			row := Item{Ref: Ref{ID: vocabID(RowKind, byte(i)), Inc: 7}, Ver: 11,
				V:    map[GroupKey]Version{PlaceGroup: 7, CellGroup(vocabID(PropertyKind, 0)): 11},
				Data: Row{Database: vocabID(DatabaseKind, 0), Position: "a0", Cells: map[ID]CellValue{vocabID(PropertyKind, 0): value}}}
			if err := putItem(t.Context(), tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, _, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	for i, want := range values {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			item, err := s.loadItem(t.Context(), vocabID(RowKind, byte(i)))
			if err != nil {
				t.Fatal(err)
			}
			got := item.Data.(Row).Cells[vocabID(PropertyKind, 0)]
			a, err := EncodeCellValue(want)
			if err != nil {
				t.Fatal(err)
			}
			b, err := EncodeCellValue(got)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatalf("%s != %s: %v", a, b, err)
			}
		})
	}
}

func TestStoreCellVersionsStayIndependent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), dbName)
	s, _, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := vocabID(PropertyKind, 0), vocabID(PropertyKind, 1)
	item := Item{Ref: Ref{ID: vocabID(RowKind, 0), Inc: 7}, Ver: 13,
		V:    map[GroupKey]Version{PlaceGroup: 7, CellGroup(a): 11, CellGroup(b): 13},
		Data: Row{Database: vocabID(DatabaseKind, 0), Position: "a0", Cells: map[ID]CellValue{a: {Type: TextType, Value: "one"}, b: {Type: TextType, Value: "two"}}}}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return putItem(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
	item.V[CellGroup(a)] = 14
	item.Ver = 14
	data := item.Data.(Row)
	data.Cells[a] = CellValue{Type: TextType, Value: "changed"}
	item.Data = data
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return putItem(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, _, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	got, err := s.loadItem(t.Context(), item.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.V[CellGroup(a)] != 14 || got.V[CellGroup(b)] != 13 || got.V[PlaceGroup] != 7 {
		t.Fatalf("cell groups coupled: %#v", got.V)
	}
	if got.Data.(Row).Cells[b].Value != "two" || got.Data.(Row).Cells[a].Value != "changed" {
		t.Fatal("cell values changed incorrectly")
	}
}

func TestStoreRefusesNewerSchemaWithoutWrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), dbName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA user_version=2; CREATE TABLE future(value TEXT); INSERT INTO future VALUES('keep')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, _, err := openStore(path); err == nil {
		if closeErr := s.close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("opened a newer schema")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("newer schema was modified")
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created WAL: %v", err)
	}
}

func TestStoreSchemaAndRollback(t *testing.T) {
	t.Parallel()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"meta", "allocated_ids", "items", "pages", "blocks", "databases", "properties", "options", "rows", "views", "cells", "group_versions", "orders", "assets", "sessions", "created", "receipts", "commands", "command_mappings", "deltas", "undo", "groups", "files", "outbox", "snapshots", "view_notices"} {
		var got string
		if err := s.db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	item := Item{Ref: Ref{ID: vocabID(PageKind, 0), Inc: 7}, Ver: 9, V: map[GroupKey]Version{TitleGroup: 9, PlaceGroup: 7}, Data: Page{Title: "First", Position: "a0"}}
	refused := errors.New("refused")
	err = s.transaction(t.Context(), func(tx *sql.Tx) error {
		if err := putItem(t.Context(), tx, item); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("transaction = %v", err)
	}
	if _, err := s.loadItem(t.Context(), item.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rolled-back item exists: %v", err)
	}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return putItem(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO pages(id,parent,position) VALUES(?,NULL,'a0')`, vocabID(PageKind, 2))
		return err
	}); err == nil {
		t.Fatal("foreign key violation accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.transaction(ctx, func(*sql.Tx) error { t.Error("entered cancelled transaction"); return nil }); err == nil {
		t.Fatal("cancelled transaction succeeded")
	}
}

func TestStoreLiveExactNamesAndTombstones(t *testing.T) {
	t.Parallel()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	for _, kind := range []Kind{PropertyKind, OptionKind} {
		t.Run(string(kind), func(t *testing.T) {
			makeItem := func(n byte, name string) Item {
				var data ItemData = Property{Database: vocabID(DatabaseKind, 0), Definition: PropertyDef{Name: name, Type: TextType}, OptionOrder: []ID{}}
				if kind == OptionKind {
					data = Option{Property: vocabID(PropertyKind, 0), Name: name, Color: Gray}
				}
				return Item{Ref: Ref{ID: vocabID(kind, n), Inc: 7}, Ver: 9, V: map[GroupKey]Version{DefinitionGroup: 9}, Data: data}
			}
			first := makeItem(0, "Name")
			write := func(item Item) error {
				return s.transaction(t.Context(), func(tx *sql.Tx) error { return putItem(t.Context(), tx, item) })
			}
			if err := write(first); err != nil {
				t.Fatal(err)
			}
			if err := write(makeItem(1, "Name")); err == nil {
				t.Fatal("duplicate live name accepted")
			}
			if err := write(makeItem(2, "name")); err != nil {
				t.Fatal("case folded name:", err)
			}
			if err := write(makeItem(3, " Name")); err != nil {
				t.Fatal("trimmed name:", err)
			}
			if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
				return putRecord(t.Context(), tx, itemRecord{Item: first, Alive: false, DeletedAt: 10, Aggregate: 9})
			}); err != nil {
				t.Fatal(err)
			}
			if err := write(makeItem(1, "Name")); err != nil {
				t.Fatal("tombstone blocked live name:", err)
			}
			if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
				record, err := loadRecord(t.Context(), tx, first.ID())
				if err != nil {
					return err
				}
				if record.Alive || record.DeletedAt != 10 {
					return fmt.Errorf("lost tombstone %#v", record)
				}
				_, err = tx.ExecContext(t.Context(), `DELETE FROM items WHERE id=?`, first.ID())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var id string
			if err := s.db.QueryRowContext(t.Context(), `SELECT id FROM allocated_ids WHERE id=?`, first.ID()).Scan(&id); err != nil {
				t.Fatal("allocated identity forgotten:", err)
			}
		})
	}
}

func TestStoreProjectionDisagreement(t *testing.T) {
	t.Parallel()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	item := Item{Ref: Ref{ID: vocabID(PageKind, 0), Inc: 7}, Ver: 7, V: map[GroupKey]Version{}, Data: Page{Title: "Page", Position: "a0"}}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return putItem(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE pages SET position='a1' WHERE id=?`, item.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadItem(t.Context(), item.ID()); err == nil {
		t.Fatal("subtype-only rebalance went unnoticed")
	}
}

func TestStoreTombstoneAggregateReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), dbName)
	s, _, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	item := Item{Ref: Ref{ID: vocabID(PageKind, 0), Inc: 7}, Ver: 42, V: map[GroupKey]Version{TitleGroup: 11, PlaceGroup: 7}, Data: Page{Title: "Deleted", Position: "a0"}}
	want := itemRecord{Item: item, Alive: false, DeletedAt: 43, Aggregate: 42}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return putRecord(t.Context(), tx, want) }); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, _, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		got, err := loadRecord(t.Context(), tx, item.ID())
		if err != nil {
			return err
		}
		a, err := EncodeItem(got.Item)
		if err != nil {
			return err
		}
		b, err := EncodeItem(want.Item)
		if err != nil {
			return err
		}
		if got.Alive || got.DeletedAt != 43 || got.Aggregate != 42 || !bytes.Equal(a, b) {
			return fmt.Errorf("tombstone changed: %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreUnsignedSQLOrder(t *testing.T) {
	t.Parallel()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	want := []string{"9", "10", "99", "100", "18446744073709551614", "18446744073709551615"}
	for _, seq := range []string{want[5], want[2], want[1], want[0], want[4], want[3]} {
		if _, err := s.db.ExecContext(t.Context(), `INSERT INTO deltas(seq,effects,source,bytes,at) VALUES(?,'[]','{}',0,0)`, seq); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.QueryContext(t.Context(), `SELECT seq FROM deltas ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for rows.Next() {
		var seq string
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		got = append(got, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SQL order = %v, want %v", got, want)
	}
	for watermark, count := range map[string]int{"9": 5, "10": 4, "99": 3, "100": 2, "18446744073709551614": 1, "18446744073709551615": 0} {
		var n int
		if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM deltas WHERE seq>?`, watermark).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != count {
			t.Errorf("range after %s has %d, want %d", watermark, n, count)
		}
	}
	var maximum string
	if err := s.db.QueryRowContext(t.Context(), `SELECT max(seq) FROM deltas`).Scan(&maximum); err != nil || maximum != want[5] {
		t.Fatalf("SQL max = %s, %v", maximum, err)
	}
	for _, table := range []string{"meta", "items", "group_versions", "sessions", "created", "receipts", "commands", "command_mappings", "deltas", "files", "view_notices"} {
		var ddl string
		if err := s.db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name=?`, table).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		for _, column := range []string{"seq", "reset_seq", "inc", "deleted_seq", "own_version", "aggregate_version", "version", "max_n", "n", "outbox_seq", "dirty_seq"} {
			if regexp.MustCompile(`\b`+column+` TEXT\b`).MatchString(ddl) && !regexp.MustCompile(`\b`+column+` TEXT COLLATE notebook_u64\b`).MatchString(ddl) {
				t.Errorf("%s.%s lacks numeric collation", table, column)
			}
		}
	}
}

func TestStoreNewerWALContentUnchanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), dbName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE future(value TEXT); PRAGMA user_version=2; INSERT INTO future VALUES('retained')`); err != nil {
		t.Fatal(err)
	}
	// Shared-memory reader marks are locks, not notebook content.
	before := map[string][32]byte{}
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		before[suffix] = sha256.Sum256(data)
	}
	if s, _, err := openStore(path); err == nil {
		if err := s.close(); err != nil {
			t.Error(err)
		}
		t.Fatal("opened newer WAL schema")
	}
	for suffix, hash := range before {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(data) != hash {
			t.Fatalf("newer WAL content %q modified", suffix)
		}
	}
}

func TestStoreFullUint64Versions(t *testing.T) {
	t.Parallel()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	item := Item{Ref: Ref{ID: vocabID(PageKind, 0), Inc: 1}, Ver: Version(^uint64(0)), V: map[GroupKey]Version{TitleGroup: Version(^uint64(0))}, Data: Page{Title: strings.Repeat("한", 8), Position: "a0"}}
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error { return putItem(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadItem(t.Context(), item.ID())
	if err != nil || got.Ver != item.Ver {
		t.Fatalf("uint64 stamp changed: %v, %v", got.Ver, err)
	}
}
